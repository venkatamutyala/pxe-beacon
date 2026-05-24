// Package tftp serves the embedded netboot.xyz iPXE binaries over
// UDP/69. pin/tftp handles the high-port transfer that PLAN section 0
// flags as a Docker-on-macOS-killer; we just plug a read handler in.
package tftp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"strings"

	pin "github.com/pin/tftp/v3"
	"github.com/venkatamutyala/pxe-beacon/internal/assets"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

// Tracker is the same hook the proxyDHCP listener uses to suppress
// its "client never fetched" hint. The TFTP server has no way to know
// the client's MAC (TFTP doesn't carry it), but we can at least
// signal that *some* client fetched something.
type Tracker interface {
	NoteServed(mac string)
}

// Server wraps pin/tftp with our embedded asset handler.
type Server struct {
	log     *narrlog.Logger
	tracker Tracker
	srv     *pin.Server
	addr    string
}

// Options carries the listener config.
type Options struct {
	Listen  string // "0.0.0.0:69" or "127.0.0.1:6969" for tests
	Logger  *narrlog.Logger
	Tracker Tracker
}

// New constructs a TFTP server but does not start it.
func New(o Options) (*Server, error) {
	if o.Logger == nil {
		return nil, errors.New("Options.Logger required")
	}
	if o.Listen == "" {
		o.Listen = "0.0.0.0:69"
	}
	s := &Server{
		log:     o.Logger.With("tftp"),
		tracker: o.Tracker,
		addr:    o.Listen,
	}
	s.srv = pin.NewServer(s.readHandler, nil) // write disabled
	return s, nil
}

// Serve listens until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", s.addr)
	if err != nil {
		return fmt.Errorf("resolve tftp addr %q: %w", s.addr, err)
	}
	pc, err := net.ListenPacket("udp", addr.String())
	if err != nil {
		return fmt.Errorf("bind tftp %s: %w (hint: udp/69 needs root)", s.addr, err)
	}
	s.log.Infof("listening on %s", pc.LocalAddr())

	done := make(chan error, 1)
	go func() {
		done <- s.srv.Serve(pc)
	}()

	select {
	case <-ctx.Done():
		s.log.Infof("tftp: shutdown requested")
		s.srv.Shutdown()
		<-done
		return nil
	case err := <-done:
		return err
	}
}

// readHandler resolves a TFTP path to an embedded asset and streams it.
//
// Path scheme decision (recorded for PROGRESS.md): we accept both the
// flat form ("netboot.xyz.efi") AND a MAC-prefixed form
// ("00:11:22:33:44:55/netboot.xyz.efi"). The MAC-prefix scheme is a
// common convention so per-host overlays can be layered later; for v1
// we ignore the prefix and serve the same binary. The leaf filename
// is what determines which asset we serve.
func (s *Server) readHandler(filename string, rf io.ReaderFrom) error {
	leaf := path.Base(strings.ReplaceAll(filename, "\\", "/"))
	logPath := filename
	if filename != leaf {
		logPath = fmt.Sprintf("%s (leaf=%s)", filename, leaf)
	}

	kind, ok := kindForLeaf(leaf)
	if !ok {
		s.log.Warnf(`RRQ %q -> 404 (unknown filename; known: netboot.xyz.efi, netboot.xyz-snponly.efi, netboot.xyz-arm64.efi, netboot.xyz.kpxe)`, filename)
		return fmt.Errorf("file not found: %s", filename)
	}

	data, err := assets.ReadIPXE(kind)
	if err != nil {
		s.log.Errorf("RRQ %q -> 500 reading embedded asset: %v", filename, err)
		return err
	}

	s.log.Served("TFTP", kind.String(), logPath, len(data))

	// pin/tftp supports tsize negotiation when the ReaderFrom is an
	// io.ReaderFrom that exposes Size — but our reader is a
	// bytes.Reader which doesn't have it. Set the option via the
	// outgoing transfer if the type supports it.
	if outSizer, ok := rf.(interface{ SetSize(int64) }); ok {
		outSizer.SetSize(int64(len(data)))
	}

	// pin/tftp's returned `n` includes blocksize/OACK overhead and
	// can exceed len(data); trust the error return for success.
	if _, err := rf.ReadFrom(bytes.NewReader(data)); err != nil {
		s.log.Errorf("RRQ %q -> transfer error: %v", filename, err)
		return err
	}
	s.log.Infof(`TFTP RRQ %q -> served %s (%d bytes) ok`, filename, kind, len(data))
	if s.tracker != nil {
		// We don't know the MAC at this layer; use an opaque tag so
		// the hint timer at least clears something. Per-MAC tracking
		// is a Phase-2 nicety.
		s.tracker.NoteServed("tftp-anon")
	}
	return nil
}

// kindForLeaf maps a requested filename to an embedded asset.
func kindForLeaf(leaf string) (assets.IPXEKind, bool) {
	switch leaf {
	case "netboot.xyz.efi", "ipxe.efi":
		return assets.IPXEEFIx64, true
	case "netboot.xyz-snponly.efi", "ipxe-snponly.efi":
		return assets.IPXESNPOnly, true
	case "netboot.xyz-arm64.efi", "ipxe-arm64.efi":
		return assets.IPXEARM64, true
	case "netboot.xyz.kpxe", "undionly.kpxe", "ipxe.kpxe":
		return assets.IPXELegacyBIOS, true
	}
	return 0, false
}
