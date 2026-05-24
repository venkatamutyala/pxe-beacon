package tftp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	pin "github.com/pin/tftp/v3"

	"github.com/venkatamutyala/pxe-beacon/internal/assets"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

// startTestServer launches a TFTP server on an OS-assigned UDP port
// (so we don't need root and tests can run in parallel) and returns
// the bound address and a cancellation func.
func startTestServer(t *testing.T) (string, func()) {
	t.Helper()
	// Bind to :0 to get an ephemeral port.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral: %v", err)
	}
	addr := pc.LocalAddr().String()
	// We need pin/tftp to take over the conn; close ours and reuse the port.
	_ = pc.Close()

	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelDebug, logBuf)
	s, err := New(Options{Listen: addr, Logger: log})
	if err != nil {
		t.Fatalf("new tftp: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = s.Serve(ctx)
		close(done)
	}()

	// Give the server a moment to bind.
	time.Sleep(50 * time.Millisecond)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Log("tftp shutdown timed out")
		}
		t.Logf("log dump:\n%s", logBuf.String())
	})
	return addr, cancel
}

func TestTFTP_ServesEmbeddedEFI(t *testing.T) {
	addr, _ := startTestServer(t)

	c, err := pin.NewClient(addr)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	c.SetTimeout(2 * time.Second)
	wt, err := c.Receive("netboot.xyz.efi", "octet")
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	var got bytes.Buffer
	n, err := wt.WriteTo(&got)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	want, err := assets.ReadIPXE(assets.IPXEEFIx64)
	if err != nil {
		t.Fatalf("read embedded: %v", err)
	}
	if int(n) != len(want) {
		t.Fatalf("size = %d, want %d", n, len(want))
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("served bytes differ from embedded")
	}
}

func TestTFTP_AcceptsAliasFilename(t *testing.T) {
	// PLAN doesn't pin a single canonical name; firmware may request
	// "ipxe.efi" or "netboot.xyz.efi". Both must work.
	addr, _ := startTestServer(t)
	c, _ := pin.NewClient(addr)
	c.SetTimeout(2 * time.Second)
	wt, err := c.Receive("ipxe.efi", "octet")
	if err != nil {
		t.Fatalf("receive ipxe.efi: %v", err)
	}
	want, _ := assets.ReadIPXE(assets.IPXEEFIx64)
	got := &bytes.Buffer{}
	if _, err := wt.WriteTo(got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatal("ipxe.efi alias did not match embedded netboot.xyz.efi")
	}
}

func TestTFTP_AcceptsMACPrefixedPath(t *testing.T) {
	// Per-host overlay paths are common; we ignore the prefix and
	// serve the leaf.
	addr, _ := startTestServer(t)
	c, _ := pin.NewClient(addr)
	c.SetTimeout(2 * time.Second)
	wt, err := c.Receive("58:47:ca:70:c7:c9/netboot.xyz.efi", "octet")
	if err != nil {
		t.Fatalf("receive MAC-prefixed: %v", err)
	}
	want, _ := assets.ReadIPXE(assets.IPXEEFIx64)
	got := &bytes.Buffer{}
	if _, err := wt.WriteTo(got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatal("MAC-prefixed fetch did not match embedded")
	}
}

func TestTFTP_AutoexecRedirector(t *testing.T) {
	// When Options.Autoexec is set, TFTP serves the redirector for
	// autoexec.ipxe RRQs. Used by fleet mode in v0.2.
	redirectorBytes := []byte("#!ipxe\nchain http://1.2.3.4:8080/autoinstall/aa-bb/autoexec.ipxe\nexit\n")

	// Pick a port the same way startTestServer does.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()

	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelDebug, logBuf)
	s, err := New(Options{
		Listen:   addr,
		Logger:   log,
		Autoexec: func() []byte { return redirectorBytes },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)

	c, _ := pin.NewClient(addr)
	c.SetTimeout(2 * time.Second)
	wt, err := c.Receive("autoexec.ipxe", "octet")
	if err != nil {
		t.Fatalf("receive autoexec.ipxe: %v\nlog:\n%s", err, logBuf.String())
	}
	var got bytes.Buffer
	if _, err := wt.WriteTo(&got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), redirectorBytes) {
		t.Errorf("autoexec.ipxe content mismatch:\n got: %q\nwant: %q", got.Bytes(), redirectorBytes)
	}
}

func TestTFTP_AutoexecWithoutRedirector_404s(t *testing.T) {
	// When Options.Autoexec is nil, autoexec.ipxe still 404s — this
	// preserves the v0.1.3 behavior so users who don't pass -config
	// see no change.
	addr, _ := startTestServer(t) // startTestServer sets no Autoexec
	c, _ := pin.NewClient(addr)
	c.SetTimeout(1 * time.Second)
	if _, err := c.Receive("autoexec.ipxe", "octet"); err == nil {
		t.Error("expected 404 for autoexec.ipxe with no redirector configured")
	}
}

func TestTFTP_404ForUnknown(t *testing.T) {
	addr, _ := startTestServer(t)
	c, _ := pin.NewClient(addr)
	c.SetTimeout(1 * time.Second)
	_, err := c.Receive("notarealfile.bin", "octet")
	if err == nil {
		t.Fatal("expected error for unknown file")
	}
	// pin/tftp surfaces the error message we returned.
	if !strings.Contains(err.Error(), "not found") &&
		!errors.Is(err, io.EOF) {
		t.Logf("err = %v (acceptable as long as transfer failed)", err)
	}
}
