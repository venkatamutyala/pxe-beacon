package proxydhcp

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"

	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

// TestListener_EndToEnd_SyntheticDISCOVER fires a real DISCOVER at the
// listener over UDP loopback and asserts the OFFER bytes contain the
// fields a UEFI x86_64 client expects. This exercises the listener +
// BuildOffer + ToBytes path that the unit tests skip — it's the
// PLAN's "optional synthetic DHCP client" check, added because it
// catches socket-level bugs the pure tests can't.
func TestListener_EndToEnd_SyntheticDISCOVER(t *testing.T) {
	false_ := false

	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelDebug, logBuf)

	l, err := New(ServerOptions{
		ListenIP:       "127.0.0.1",
		Port67:         16700,
		Port4011:       17011,
		BroadcastReply: &false_, // unicast back to client port
		Config: Config{
			AdvertisedIP:   net.ParseIP("127.0.0.1"),
			HTTPPort:       8080,
			IPXEScriptPath: "/boot.ipxe",
		},
		Logger:          log,
		FollowUpTimeout: 0,
	})
	if err != nil {
		t.Fatalf("new listener: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = l.Serve(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)

	// Synthetic client: bind a high port so the server can reply
	// unicast to us.
	clientAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenUDP("udp", clientAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	hw, _ := net.ParseMAC("58:47:ca:70:c7:c9")
	disc, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover),
		dhcpv4.WithHwAddr(hw),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00007:UNDI:003016")),
		dhcpv4.WithOption(dhcpv4.OptGeneric(
			dhcpv4.OptionClientSystemArchitectureType,
			iana.Archs{iana.EFI_X86_64}.ToBytes(),
		)),
	)
	if err != nil {
		t.Fatal(err)
	}
	srvAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:16700")
	if _, err := pc.WriteTo(disc.ToBytes(), srvAddr); err != nil {
		t.Fatalf("send DISCOVER: %v", err)
	}

	// Read the reply.
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read OFFER: %v\nlog:\n%s", err, logBuf.String())
	}
	reply, err := dhcpv4.FromBytes(buf[:n])
	if err != nil {
		t.Fatalf("parse OFFER: %v", err)
	}

	if got := reply.MessageType(); got != dhcpv4.MessageTypeOffer {
		t.Errorf("msg type = %s, want OFFER", got)
	}
	if got := reply.YourIPAddr.String(); got != "0.0.0.0" {
		t.Errorf("yiaddr = %s, want 0.0.0.0 (proxyDHCP MUST NOT assign IPs)", got)
	}
	if got := reply.ServerIdentifier().String(); got != "127.0.0.1" {
		t.Errorf("server-identifier = %s, want 127.0.0.1", got)
	}
	if reply.BootFileName != "netboot.xyz.efi" {
		t.Errorf("bootfile = %q, want netboot.xyz.efi", reply.BootFileName)
	}

	logStr := logBuf.String()
	if !strings.Contains(logStr, "stage=firmware-TFTP") {
		t.Errorf("expected stage=firmware-TFTP in narrated log:\n%s", logStr)
	}
	if !strings.Contains(logStr, "decision: serve netboot.xyz.efi via TFTP") {
		t.Errorf("expected decision line:\n%s", logStr)
	}
}

// TestListener_EndToEnd_iPXEUserClassServesScript verifies the second
// canonical case: the iPXE-stage client gets the script URL, not the
// binary. This is what prevents the chainload loop PLAN section 0
// warns about.
func TestListener_EndToEnd_iPXEUserClassServesScript(t *testing.T) {
	false_ := false
	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelInfo, logBuf)

	l, _ := New(ServerOptions{
		ListenIP:       "127.0.0.1",
		Port67:         16701,
		Port4011:       17012,
		BroadcastReply: &false_,
		Config: Config{
			AdvertisedIP:   net.ParseIP("127.0.0.1"),
			HTTPPort:       8080,
			IPXEScriptPath: "/boot.ipxe",
		},
		Logger: log,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Serve(ctx)
	time.Sleep(100 * time.Millisecond)

	pc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer pc.Close()

	hw, _ := net.ParseMAC("58:47:ca:70:c7:c9")
	disc, _ := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover),
		dhcpv4.WithHwAddr(hw),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
		dhcpv4.WithOption(dhcpv4.OptGeneric(
			dhcpv4.OptionClientSystemArchitectureType,
			iana.Archs{iana.EFI_X86_64}.ToBytes(),
		)),
		dhcpv4.WithUserClass("iPXE", false),
	)
	srvAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:16701")
	_, _ = pc.WriteTo(disc.ToBytes(), srvAddr)

	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read: %v\nlog:\n%s", err, logBuf.String())
	}
	reply, _ := dhcpv4.FromBytes(buf[:n])

	wantURL := "http://127.0.0.1:8080/boot.ipxe"
	if reply.BootFileName != wantURL {
		t.Errorf("bootfile = %q, want %q\nlog:\n%s", reply.BootFileName, wantURL, logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "stage=iPXE-script") {
		t.Errorf("missing iPXE-script stage in log:\n%s", logBuf.String())
	}
}
