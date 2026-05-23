// Command pxe-beacon is a single-binary proxyDHCP + TFTP + HTTP server
// that network-boots LAN clients. See PLAN.md for the full design.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/venkatamutyala/pxe-beacon/internal/assets"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
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

	// Startup banner. (M5 fleshes out interface/IP/WiFi detection; the M0
	// banner just proves we wired flags + logging + embed.)
	printBanner(log, *flagIface, *flagListen, *flagHTTPPort, *flagChainURL, *flagIPXEScript)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Errorf("fatal: %v", err)
		os.Exit(1)
	}
	log.Infof("pxe-beacon: shutdown complete")
}

func printBanner(log *narrlog.Logger, iface, listen string, httpPort int, chainURL, scriptOverride string) {
	log.Infof("pxe-beacon %s starting (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
	log.Infof("flags: interface=%q listen=%s http-port=%d chain-url=%s",
		iface, listen, httpPort, chainURL)
	if scriptOverride != "" {
		log.Infof("using custom iPXE script: %s", scriptOverride)
	}
	// Confirm embed wiring as part of the startup check.
	if b, err := assets.ReadIPXE(assets.IPXEEFIx64); err != nil {
		log.Warnf("embedded asset check failed: %v", err)
	} else {
		log.Infof("embedded %s ready (%d bytes)", assets.IPXEEFIx64, len(b))
	}
}

// run is the M0 placeholder. Later milestones wire up proxyDHCP, TFTP,
// and HTTP goroutines here and block on ctx.Done().
func run(ctx context.Context, log *narrlog.Logger) error {
	log.Infof("ready — press Ctrl-C to exit (M0 scaffold: no services started yet)")
	<-ctx.Done()
	log.Infof("signal received, shutting down")
	return nil
}
