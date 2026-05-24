package proxydhcp

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"

	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

// ServerOptions wires the listener to the rest of the program.
type ServerOptions struct {
	Interface string // "" means "any"
	ListenIP  string // e.g. "0.0.0.0"
	// Port67 / Port4011 default to 67 and 4011. Tests override to
	// avoid needing root; in production these stay at the
	// well-known values clients send to.
	Port67   int
	Port4011 int
	Config   Config // BuildOffer config
	Logger   *narrlog.Logger
	// FollowUpTimeout sets how long after sending an OFFER we wait
	// before logging the "client never fetched" hint if no
	// corresponding TFTP/HTTP fetch was observed. PLAN section 4
	// requires this failure-path hint. Zero disables.
	FollowUpTimeout time.Duration
	// BroadcastReply controls whether OFFERs to port-67 senders are
	// sent to 255.255.255.255:68. Default true; tests with synthetic
	// clients on loopback set false so we reply unicast to the peer
	// and don't need to capture broadcast traffic.
	BroadcastReply *bool
	// StatusTracker, if non-nil, receives per-MAC fleet status events
	// (firmware-dhcp, ipxe-dhcp) as the listener processes packets.
	StatusTracker *fleet.Tracker
}

// OfferTracker lets the TFTP and HTTP servers notify the proxyDHCP
// listener that a particular client has progressed past the OFFER —
// which is what suppresses the "stuck" hint. The interface keeps the
// circular dependency between servers off the type level.
type OfferTracker interface {
	NoteServed(mac string)
}

// Listener owns the UDP/67 and UDP/4011 sockets. It does no DHCP
// logic — all decisions go through BuildOffer. The split exists for
// the same reason the BuildOffer purity rule does: it makes the live
// path a thin shim and the unit tests faithful to production.
type Listener struct {
	opts    ServerOptions
	log     *narrlog.Logger
	srv67   *server4.Server
	srv4011 *server4.Server

	// wg joins the two Serve goroutines on shutdown. v0.8.1 fix for
	// the pre-existing TestListener_EndToEnd_* races: without joining,
	// Serve returned while the upstream dhcp library was still inside
	// its read loop, racing with the test's cancel().
	wg sync.WaitGroup

	pendingMu sync.Mutex
	pending   map[string]time.Time // MAC -> OFFER sent at
}

// New creates the Listener but does not start it.
func New(o ServerOptions) (*Listener, error) {
	if o.Logger == nil {
		return nil, fmt.Errorf("ServerOptions.Logger required")
	}
	return &Listener{
		opts:    o,
		log:     o.Logger.With("proxydhcp"),
		pending: make(map[string]time.Time),
	}, nil
}

func (l *Listener) broadcastReply() bool {
	if l.opts.BroadcastReply == nil {
		return true
	}
	return *l.opts.BroadcastReply
}

// NoteServed implements OfferTracker — called by the TFTP/HTTP
// servers when a client successfully fetches an asset.
//
// The TFTP and HTTP servers don't natively know the client's MAC
// (TFTP RRQ doesn't carry it, HTTP requests come from an IP not a
// MAC), so they pass opaque tags like "tftp-anon" or "http-anon".
// Treat *any* such signal as "the server is actively serving
// something" and clear all pending hints — the failure-path hint
// exists to flag a stuck server, not to track per-client progress.
// (When a real per-MAC name is passed in the future, we honor it
// as a hint-clear for that specific MAC.)
func (l *Listener) NoteServed(mac string) {
	if mac == "" {
		return
	}
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if _, exact := l.pending[mac]; exact {
		delete(l.pending, mac)
		return
	}
	// Opaque tag — clear everything; we know the server is active.
	for k := range l.pending {
		delete(l.pending, k)
	}
}

// Serve binds 67 + 4011 and runs until ctx is cancelled.
func (l *Listener) Serve(ctx context.Context) error {
	p67 := l.opts.Port67
	if p67 == 0 {
		p67 = 67
	}
	p4011 := l.opts.Port4011
	if p4011 == 0 {
		p4011 = 4011
	}
	addr67 := &net.UDPAddr{IP: net.ParseIP(l.opts.ListenIP), Port: p67}
	addr4011 := &net.UDPAddr{IP: net.ParseIP(l.opts.ListenIP), Port: p4011}

	s67, err := server4.NewServer(l.opts.Interface, addr67, l.handler("udp/67"))
	if err != nil {
		return fmt.Errorf("bind udp/%d: %w (hint: ports <1024 need root — try sudo, or a setcap on Linux)", p67, err)
	}
	l.srv67 = s67

	s4011, err := server4.NewServer(l.opts.Interface, addr4011, l.handler("udp/4011"))
	if err != nil {
		_ = s67.Close()
		return fmt.Errorf("bind udp/%d: %w (hint: ports <1024 need root)", p4011, err)
	}
	l.srv4011 = s4011

	l.log.Infof("listening on udp/%d (DHCP) and udp/%d (PXE BINL), interface=%q advertise=%s",
		p67, p4011, l.opts.Interface, l.opts.Config.AdvertisedIP)

	errc := make(chan error, 2)
	l.wg.Add(2)
	go func() {
		defer l.wg.Done()
		errc <- l.srv67.Serve()
	}()
	go func() {
		defer l.wg.Done()
		errc <- l.srv4011.Serve()
	}()

	select {
	case <-ctx.Done():
		l.log.Infof("proxyDHCP: shutdown requested")
	case err := <-errc:
		if err != nil {
			l.log.Errorf("proxyDHCP listener exited: %v", err)
		}
	}
	// v0.8.1: Close THEN Wait. Close unblocks the Serve goroutines
	// out of their read loops; Wait ensures they've fully exited
	// before Serve returns (and therefore before the test reads any
	// shared state under -race).
	_ = l.srv67.Close()
	_ = l.srv4011.Close()
	l.wg.Wait()
	return nil
}

func (l *Listener) handler(source string) server4.Handler {
	return func(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
		l.handleOne(source, conn, peer, req)
	}
}

func (l *Listener) handleOne(source string, conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
	l.log.Debugf("%s: received from %s: %s", source, peer, req.Summary())

	reply, dec, err := BuildOffer(req, l.opts.Config)
	logDecision(l.log, source, dec, err)

	if err != nil {
		// Includes the benign skip case — the logger already
		// rendered it; just return.
		return
	}

	// proxyDHCP REPLY destinations:
	//   - DHCPDISCOVER from 0.0.0.0:68 → reply broadcast to 255.255.255.255:68
	//   - REQUEST on 4011 → unicast to client IP, port 4011 typically
	dst, _ := peer.(*net.UDPAddr)
	if dst == nil {
		l.log.Warnf("%s: peer is not UDP, dropping reply", source)
		return
	}
	if source == "udp/67" && l.broadcastReply() {
		// Force broadcast for OFFER (proxyDHCP is broadcast-based).
		dst = &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
	}

	buf := reply.ToBytes()
	if _, err := conn.WriteTo(buf, dst); err != nil {
		l.log.Errorf("%s: send OFFER to %s: %v", source, dst, err)
		return
	}
	l.log.Debugf("%s: sent OFFER (%d bytes) to %s", source, len(buf), dst)
	l.log.HexDump(fmt.Sprintf("%s OFFER bytes", source), buf)

	// Track the OFFER so the failure-path hint can fire if the
	// client never follows up with TFTP/HTTP.
	l.notePending(dec.ClientMAC)

	// Record a status event for the fleet tracker (fleet mode only).
	if l.opts.StatusTracker != nil {
		var ev fleet.Event
		switch dec.Stage {
		case StageFirmwareTFTP, StageFirmwareHTTP:
			ev = fleet.EventFirmwareDHCP
		case StageIPXEScript:
			ev = fleet.EventIPXEDHCP
		}
		if ev != "" {
			l.opts.StatusTracker.Note(dec.ClientMAC, ev)
		}
	}
}

func (l *Listener) notePending(mac string) {
	if l.opts.FollowUpTimeout <= 0 || mac == "" {
		return
	}
	now := time.Now()
	l.pendingMu.Lock()
	l.pending[mac] = now
	l.pendingMu.Unlock()

	timeout := l.opts.FollowUpTimeout
	go func() {
		time.Sleep(timeout)
		l.pendingMu.Lock()
		t, still := l.pending[mac]
		// Only fire if THIS goroutine's OFFER is the latest one
		// pending for this MAC. If another OFFER landed in the
		// meantime, the timestamp will be newer and this older
		// goroutine should silently exit — that newer OFFER has
		// its own hint goroutine which will fire (or not) based on
		// its own progress. Without this check we'd hint-spam on
		// every OFFER retry within the timeout window.
		shouldFire := still && t.Equal(now)
		if shouldFire {
			delete(l.pending, mac)
		}
		l.pendingMu.Unlock()
		if shouldFire {
			l.log.Hint("client %s got the OFFER but never fetched within %s — "+
				"check same-segment, firewall, and that advertised IP %s is reachable from the client",
				mac, timeout, l.opts.Config.AdvertisedIP)
		}
	}()
}

// logDecision turns a Decision/err into the narrated log lines PLAN
// section 4 requires. Centralized here so listener.go stays a thin
// shim and the test of BuildOffer's behavior is what carries the
// invariant.
func logDecision(log *narrlog.Logger, source string, d Decision, err error) {
	archStr := "<absent>"
	if len(d.Archs) > 0 {
		archStr = fmt.Sprintf("0x%02x(%s)", uint16(d.Archs[0]), d.Archs[0])
	}
	vc := d.VendorClass
	if vc == "" {
		vc = "<absent>"
	}

	clientLabel := d.ClientMAC
	if d.MachineName != "" {
		clientLabel = fmt.Sprintf("%s (%s)", d.MachineName, d.ClientMAC)
	}

	switch {
	case err == ErrSkip && d.IsBenignSkip():
		log.Benign(fmt.Sprintf("%s from %s: %s", source, clientLabel, d.SkipReason))
		return
	case err == ErrSkip:
		log.Infof("%s skip: client=%s reason=%s", source, clientLabel, d.SkipReason)
		return
	case err != nil:
		log.Errorf("%s build offer failed for %s: %v", source, clientLabel, err)
		return
	}

	if d.UnknownArch {
		log.Warnf("unrecognized option-93 arch from %s (%s); falling back to %s",
			clientLabel, archStr, d.Transport)
	}

	stageNote := ""
	if d.Stage == StageFirmwareHTTP {
		stageNote = " (UEFI HTTP boot)"
	}
	if d.BootTarget != "" && d.BootTarget != "menu" {
		stageNote += fmt.Sprintf(" [target=%s]", d.BootTarget)
	}

	log.Decision(clientLabel, archStr, defaultStr(d.UserClass, "<none>"), d.Stage,
		fmt.Sprintf("serve %s via %s from %s%s", d.BootFile, d.Transport, d.NextServer, stageNote))
	log.Debugf("parsed: vendor-class=%q user-class=%q archs=%v selected=%v",
		vc, d.UserClass, d.Archs, d.SelectedArch)
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
