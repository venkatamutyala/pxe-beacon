package proxydhcp

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

// ServerOptions wires the listener to the rest of the program.
type ServerOptions struct {
	Interface string  // "" means "any"
	ListenIP  string  // e.g. "0.0.0.0"
	Config    Config  // BuildOffer config
	Logger    *narrlog.Logger
	// FollowUpTimeout sets how long after sending an OFFER we wait
	// before logging the "client never fetched" hint if no
	// corresponding TFTP/HTTP fetch was observed. PLAN section 4
	// requires this failure-path hint. Zero disables.
	FollowUpTimeout time.Duration
	// NotifyServed lets external servers (TFTP, HTTP) tell us a
	// client has fetched something so we cancel the hint timer.
	// Listener constructs this; callers retrieve it via OfferTracker.
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
	opts   ServerOptions
	log    *narrlog.Logger
	srv67  *server4.Server
	srv4011 *server4.Server

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

// NoteServed implements OfferTracker — called by the TFTP/HTTP
// servers when a client successfully fetches an asset.
func (l *Listener) NoteServed(mac string) {
	if mac == "" {
		return
	}
	l.pendingMu.Lock()
	delete(l.pending, mac)
	l.pendingMu.Unlock()
}

// Serve binds 67 + 4011 and runs until ctx is cancelled.
func (l *Listener) Serve(ctx context.Context) error {
	addr67 := &net.UDPAddr{IP: net.ParseIP(l.opts.ListenIP), Port: 67}
	addr4011 := &net.UDPAddr{IP: net.ParseIP(l.opts.ListenIP), Port: 4011}

	s67, err := server4.NewServer(l.opts.Interface, addr67, l.handler("udp/67"))
	if err != nil {
		return fmt.Errorf("bind udp/67: %w (hint: ports <1024 need root — try sudo, or a setcap on Linux)", err)
	}
	l.srv67 = s67

	s4011, err := server4.NewServer(l.opts.Interface, addr4011, l.handler("udp/4011"))
	if err != nil {
		_ = s67.Close()
		return fmt.Errorf("bind udp/4011: %w (hint: ports <1024 need root)", err)
	}
	l.srv4011 = s4011

	l.log.Infof("listening on udp/67 (DHCP) and udp/4011 (PXE BINL), interface=%q advertise=%s",
		l.opts.Interface, l.opts.Config.AdvertisedIP)

	errc := make(chan error, 2)
	go func() { errc <- l.srv67.Serve() }()
	go func() { errc <- l.srv4011.Serve() }()

	select {
	case <-ctx.Done():
		l.log.Infof("proxyDHCP: shutdown requested")
	case err := <-errc:
		if err != nil {
			l.log.Errorf("proxyDHCP listener exited: %v", err)
		}
	}
	_ = l.srv67.Close()
	_ = l.srv4011.Close()
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
	if source == "udp/67" {
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
}

func (l *Listener) notePending(mac string) {
	if l.opts.FollowUpTimeout <= 0 || mac == "" {
		return
	}
	l.pendingMu.Lock()
	l.pending[mac] = time.Now()
	l.pendingMu.Unlock()

	timeout := l.opts.FollowUpTimeout
	go func() {
		time.Sleep(timeout)
		l.pendingMu.Lock()
		_, still := l.pending[mac]
		if still {
			delete(l.pending, mac)
		}
		l.pendingMu.Unlock()
		if still {
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

	switch {
	case err == ErrSkip && d.IsBenignSkip():
		log.Benign(fmt.Sprintf("%s from %s: %s", source, d.ClientMAC, d.SkipReason))
		return
	case err == ErrSkip:
		log.Infof("%s skip: client=%s reason=%s", source, d.ClientMAC, d.SkipReason)
		return
	case err != nil:
		log.Errorf("%s build offer failed for %s: %v", source, d.ClientMAC, err)
		return
	}

	if d.UnknownArch {
		log.Warnf("unrecognized option-93 arch from %s (%s); falling back to %s",
			d.ClientMAC, archStr, d.Transport)
	}

	stageNote := ""
	if d.Stage == StageFirmwareHTTP {
		stageNote = " (UEFI HTTP boot)"
	}

	log.Decision(d.ClientMAC, archStr, defaultStr(d.UserClass, "<none>"), d.Stage,
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
