package httpd

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/assets"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

func startTestServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelDebug, logBuf)

	s, err := New(Options{
		Listen:       addr,
		AdvertisedIP: "10.0.0.5",
		ChainURL:     "https://boot.netboot.xyz/menu.ipxe",
		Logger:       log,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx); close(done) }()
	time.Sleep(80 * time.Millisecond)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Logf("log dump:\n%s", logBuf.String())
	})
	return addr
}

func TestHTTP_ServesIPXEBinaryWithContentLength(t *testing.T) {
	addr := startTestServer(t)

	// HEAD first — PLAN gate uses curl -I.
	headReq, _ := http.NewRequest(http.MethodHead, "http://"+addr+"/netboot.xyz.efi", nil)
	resp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", resp.StatusCode)
	}
	want, _ := assets.ReadIPXE(assets.IPXEEFIx64)
	gotCL := resp.Header.Get("Content-Length")
	if gotCL != strconv.Itoa(len(want)) {
		t.Errorf("HEAD Content-Length = %q, want %d", gotCL, len(want))
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("HEAD Content-Type = %q, want application/octet-stream", got)
	}
	_ = resp.Body.Close()

	// GET full body.
	resp2, err := http.Get("http://" + addr + "/netboot.xyz.efi")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp2.StatusCode)
	}
	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("GET body diff: %d vs %d bytes", len(body), len(want))
	}
}

func TestHTTP_RendersBootScript(t *testing.T) {
	addr := startTestServer(t)
	resp, err := http.Get("http://" + addr + "/boot.ipxe")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "#!ipxe") {
		t.Errorf("missing #!ipxe shebang:\n%s", s)
	}
	if !strings.Contains(s, "10.0.0.5") {
		t.Errorf("AdvertisedIP not templated in:\n%s", s)
	}
	if !strings.Contains(s, "https://boot.netboot.xyz/menu.ipxe") {
		t.Errorf("ChainURL not templated in:\n%s", s)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("script Content-Length = %q, want %d", got, len(body))
	}
}

func TestHTTP_404UnknownPath(t *testing.T) {
	addr := startTestServer(t)
	resp, err := http.Get("http://" + addr + "/no-such-thing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHTTP_RootStatusPage(t *testing.T) {
	addr := startTestServer(t)
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "pxe-beacon") {
		t.Errorf("root page missing identifier:\n%s", body)
	}
}

func TestHTTP_CrossCertEmittedWhenEnabled(t *testing.T) {
	logBuf := &bytes.Buffer{}
	log := narrlog.New("test", narrlog.LevelInfo, logBuf)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()

	s, err := New(Options{
		Listen:       addr,
		AdvertisedIP: "10.0.0.5",
		ChainURL:     "https://boot.netboot.xyz/menu.ipxe",
		SetCrossCert: true,
		Logger:       log,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx)
	time.Sleep(80 * time.Millisecond)

	resp, err := http.Get("http://" + addr + "/boot.ipxe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "set crosscert") {
		t.Errorf("SetCrossCert=true did not emit crosscert directive:\n%s", body)
	}
}
