// Command pxe-beacon is a single-binary proxyDHCP + TFTP + HTTP server
// that network-boots LAN clients. See PLAN.md for the full design.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/assets"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
	"github.com/venkatamutyala/pxe-beacon/internal/netinfo"
	"github.com/venkatamutyala/pxe-beacon/internal/proxydhcp"
	tftpd "github.com/venkatamutyala/pxe-beacon/internal/tftp"
)

// version is overridden via -ldflags at build time.
var version = "dev"

func main() {
	var (
		flagIface      = flag.String("interface", "", "network interface to advertise (auto-detect if empty)")
		flagListen     = flag.String("listen", "0.0.0.0", "address to listen on for UDP services")
		flagHTTPPort   = flag.Int("http-port", 8080, "HTTP port for serving iPXE binary and chain script")
		flagLogLevel   = flag.String("loglevel", "info", "log level: error, warn, info, debug")
		flagChainURL   = flag.String("chain-url", "https://boot.netboot.xyz/menu.ipxe", "URL the iPXE script chainloads")
		flagIPXEScript = flag.String("ipxe-script", "", "path to a custom boot.ipxe template (overrides embedded default)")
		flagAdvIP      = flag.String("advertise-ip", "", "override the advertised IPv4 (auto-detect if empty)")
		flagTFTPListen = flag.String("tftp-listen", "0.0.0.0:69", "TFTP listen address (host:port)")
		flagPrintVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *flagPrintVer {
		fmt.Printf("pxe-beacon %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	lvl, err := narrlog.ParseLevel(*flagLogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pxe-beacon:", err)
		os.Exit(2)
	}

	log := narrlog.New("main", lvl, os.Stderr)
	log.Infof("pxe-beacon %s starting (%s/%s)", version, runtime.GOOS, runtime.GOARCH)

	picked, err := netinfo.Pick(*flagIface)
	if err != nil {
		log.Errorf("interface selection: %v", err)
		os.Exit(1)
	}
	advIP := picked.AdvertiseIP
	if *flagAdvIP != "" {
		ip := net.ParseIP(*flagAdvIP)
		if ip == nil || ip.To4() == nil {
			log.Errorf("invalid -advertise-ip %q (need IPv4)", *flagAdvIP)
			os.Exit(2)
		}
		advIP = ip.To4()
	}

	log.Infof("interface=%s ipv4=%s http-port=%d listen=%s",
		picked.Iface.Name, advIP, *flagHTTPPort, *flagListen)
	if picked.IsWireless {
		log.Warnf("interface %s looks wireless — TFTP may time out; prefer wired or document this", picked.Iface.Name)
	}
	log.Infof("chain-url=%s", *flagChainURL)
	if *flagIPXEScript != "" {
		log.Infof("using custom iPXE script: %s", *flagIPXEScript)
	}

	// Sanity-check the embedded asset wiring at startup so the user
	// hears about it once, not from a TFTP RRQ minutes later.
	if b, err := assets.ReadIPXE(assets.IPXEEFIx64); err != nil {
		log.Warnf("embedded asset check failed: %v", err)
	} else {
		log.Infof("embedded netboot.xyz.efi ready (%d bytes)", len(b))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := proxydhcp.Config{
		AdvertisedIP:   advIP,
		HTTPPort:       *flagHTTPPort,
		IPXEScriptPath: "/boot.ipxe",
	}

	lst, err := proxydhcp.New(proxydhcp.ServerOptions{
		Interface:       picked.Iface.Name,
		ListenIP:        *flagListen,
		Config:          cfg,
		Logger:          log,
		FollowUpTimeout: 10 * time.Second,
	})
	if err != nil {
		log.Errorf("init proxydhcp: %v", err)
		os.Exit(1)
	}

	tftpSrv, err := tftpd.New(tftpd.Options{
		Listen:  *flagTFTPListen,
		Logger:  log,
		Tracker: lst,
	})
	if err != nil {
		log.Errorf("init tftp: %v", err)
		os.Exit(1)
	}

	errc := make(chan error, 2)
	go func() { errc <- lst.Serve(ctx) }()
	go func() { errc <- tftpSrv.Serve(ctx) }()

	log.Infof("ready — press Ctrl-C to exit")
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Errorf("listener exited: %v", err)
			os.Exit(1)
		}
	}
	log.Infof("pxe-beacon: shutdown complete")
}

