// Command pxe-beacon is a single-binary proxyDHCP + TFTP + HTTP server
// that network-boots LAN clients. See PLAN.md for the full design and
// RUN.md for operational notes.
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
	"github.com/venkatamutyala/pxe-beacon/internal/boot"
	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
	"github.com/venkatamutyala/pxe-beacon/internal/httpd"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
	"github.com/venkatamutyala/pxe-beacon/internal/netinfo"
	"github.com/venkatamutyala/pxe-beacon/internal/proxydhcp"
	tftpd "github.com/venkatamutyala/pxe-beacon/internal/tftp"
)

// version is overridden via -ldflags at build time.
var version = "dev"

func main() {
	// Subcommand dispatch. `pxe-beacon` with no args (or only flags)
	// continues to mean "run the server" — preserving v0.3 and
	// earlier behavior. Only known subcommand keywords trigger
	// dispatch.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "fetch":
			runFetch(os.Args[2:])
			return
		case "serve":
			// Allow explicit `pxe-beacon serve [flags]`. Shift args
			// so the rest of main() sees them as if no subcommand
			// was passed.
			os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		}
	}

	var (
		flagIface      = flag.String("interface", "", "network interface to advertise (auto-detect if empty)")
		flagListen     = flag.String("listen", "0.0.0.0", "address to bind UDP sockets on")
		flagHTTPPort   = flag.Int("http-port", 8080, "HTTP port for serving iPXE binary and chain script")
		flagLogLevel   = flag.String("loglevel", "info", "log level: error, warn, info, debug")
		flagChainURL   = flag.String("chain-url", "https://boot.netboot.xyz/menu.ipxe", "URL the iPXE script chainloads")
		flagIPXEScript = flag.String("ipxe-script", "", "path to a custom boot.ipxe template (overrides embedded default)")
		flagAdvIP      = flag.String("advertise-ip", "", "override the advertised IPv4 (auto-detect if empty)")
		flagTFTPListen = flag.String("tftp-listen", "0.0.0.0:69", "TFTP listen address (host:port)")
		flagCrossCert  = flag.Bool("crosscert", false, "emit `set crosscert http://ca.ipxe.org/auto` in boot.ipxe (helps older iPXE builds with HTTPS netboot.xyz)")
		flagHintAfter  = flag.Duration("hint-after", 10*time.Second, "log a 'client never fetched' hint this long after an OFFER if no follow-up arrives (0 disables)")
		flagConfig     = flag.String("config", "", "path to fleet.yaml — enables per-MAC routing, autoinstall, and /status page (unset = v0.1.3 single-machine behavior)")
		flagDataDir    = flag.String("data-dir", defaultDataDir(), "directory holding extracted distro assets (populated by `pxe-beacon fetch`); also where template overrides under templates/ live; served at /assets/<target>/<file>")
		flagLegacyRdir = flag.Bool("legacy-redirector", false, "v0.4.x behavior: serve a TFTP redirector that chains iPXE to HTTP /autoinstall/<mac>/autoexec.ipxe. Default (v0.5.0+) serves a self-contained dispatch script. Use this flag to bisect if v0.5.0 breaks your boot.")
		flagPrintVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"pxe-beacon %s — self-contained proxyDHCP + TFTP + HTTP for PXE boot\n\nUsage of %s:\n",
			version, os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintln(flag.CommandLine.Output(),
			"\nproxyDHCP is broadcast-based — must run on the same L2 segment as the\nPXE client. UDP/67 + UDP/69 + UDP/4011 are privileged, so this needs\nroot (sudo) or CAP_NET_BIND_SERVICE. See RUN.md for full operational notes.")
	}
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

	printBanner(log, version, picked, advIP, *flagListen, *flagHTTPPort, *flagTFTPListen, *flagChainURL, *flagIPXEScript, *flagConfig, lvl)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Load fleet config if -config was passed; otherwise construct
	// an empty fleet so every MAC gets the v0.1.3 menu default.
	var fl *fleet.Fleet
	if *flagConfig != "" {
		var err error
		fl, err = fleet.Load(*flagConfig, log)
		if err != nil {
			log.Errorf("load fleet config: %v", err)
			os.Exit(1)
		}
	} else {
		fl = fleet.Empty(log)
	}
	statusTracker := fleet.NewTracker(fl, 5*time.Minute)

	cfg := proxydhcp.Config{
		AdvertisedIP:   advIP,
		HTTPPort:       *flagHTTPPort,
		IPXEScriptPath: "/boot.ipxe",
		Fleet:          fl,
	}

	lst, err := proxydhcp.New(proxydhcp.ServerOptions{
		Interface:       picked.Iface.Name,
		ListenIP:        *flagListen,
		Config:          cfg,
		Logger:          log,
		FollowUpTimeout: *flagHintAfter,
		StatusTracker:   statusTracker,
	})
	if err != nil {
		log.Errorf("init proxydhcp: %v", err)
		os.Exit(1)
	}

	// v0.5.0: TFTP autoexec.ipxe is a self-contained per-MAC dispatch
	// script generated from the live fleet config. No HTTP chain
	// dependency. Operators can opt back into the v0.4.x redirector
	// via -legacy-redirector if they're bisecting a regression.
	var autoexecFn tftpd.AutoexecRedirector
	if *flagConfig != "" {
		if *flagLegacyRdir {
			log.Warnf("using v0.4.x legacy redirector (per -legacy-redirector flag)")
			autoexecFn = func() []byte {
				return boot.RedirectorScript(advIP.String(), *flagHTTPPort)
			}
		} else {
			dctx := boot.DispatchContext{
				AdvertisedIP: advIP.String(),
				HTTPPort:     *flagHTTPPort,
			}
			autoexecFn = func() []byte {
				return boot.RenderDispatch(fl, dctx)
			}
		}
	}

	// Wire the template-override directory so disk overrides (placed
	// at <data-dir>/templates/<rel>) take precedence over the
	// embedded baseline. Empty data-dir disables.
	assets.SetOverrideDir(*flagDataDir)

	tftpSrv, err := tftpd.New(tftpd.Options{
		Listen:   *flagTFTPListen,
		Logger:   log,
		Tracker:  lst,
		Autoexec: autoexecFn,
	})
	if err != nil {
		log.Errorf("init tftp: %v", err)
		os.Exit(1)
	}

	httpSrv, err := httpd.New(httpd.Options{
		Listen:         fmt.Sprintf("%s:%d", *flagListen, *flagHTTPPort),
		AdvertisedIP:   advIP.String(),
		HTTPPort:       *flagHTTPPort,
		ChainURL:       *flagChainURL,
		IPXEScriptPath: cfg.IPXEScriptPath,
		IPXEScriptFile: *flagIPXEScript,
		SetCrossCert:   *flagCrossCert,
		Logger:         log,
		Tracker:        lst,
		Fleet:          fl,
		FleetStatus:    statusTracker,
		DataDir:        *flagDataDir,
	})
	if err != nil {
		log.Errorf("init http: %v", err)
		os.Exit(1)
	}

	// SIGHUP triggers a fleet config reload (no-op for Empty fleet).
	if *flagConfig != "" {
		go watchSIGHUP(ctx, fl, log)
	}

	errc := make(chan error, 3)
	go func() { errc <- lst.Serve(ctx) }()
	go func() { errc <- tftpSrv.Serve(ctx) }()
	go func() { errc <- httpSrv.Serve(ctx) }()

	log.Infof("ready — press Ctrl-C to exit")
	var firstErr error
	select {
	case <-ctx.Done():
	case firstErr = <-errc:
	}
	// Stop the others gracefully.
	cancel()
	// Drain remaining (best-effort, with a short deadline).
	deadline := time.After(3 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-errc:
		case <-deadline:
			i = 2
		}
	}

	if firstErr != nil && !errors.Is(firstErr, context.Canceled) {
		log.Errorf("server exited: %v", firstErr)
		os.Exit(1)
	}
	log.Infof("pxe-beacon: shutdown complete")
}

// watchSIGHUP reloads the fleet config when SIGHUP arrives. Cancelled
// by ctx so it dies with the rest of the program.
func watchSIGHUP(ctx context.Context, fl *fleet.Fleet, log *narrlog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			log.Infof("SIGHUP received, reloading fleet config")
			if err := fl.Reload(); err != nil {
				log.Errorf("fleet reload failed: %v (keeping previous config)", err)
			}
		}
	}
}

func printBanner(log *narrlog.Logger, ver string, picked *netinfo.Picked, advIP net.IP,
	listen string, httpPort int, tftpListen, chainURL, scriptOverride, configPath string, lvl narrlog.Level) {

	log.Infof("pxe-beacon %s (%s/%s)", ver, runtime.GOOS, runtime.GOARCH)
	log.Infof("  interface     : %s", picked.Iface.Name)
	log.Infof("  advertised-ip : %s", advIP)
	log.Infof("  proxyDHCP     : udp/67 + udp/4011 on %s", listen)
	log.Infof("  TFTP          : %s", tftpListen)
	log.Infof("  HTTP          : %s:%d (chain script /boot.ipxe)", listen, httpPort)
	log.Infof("  chain-url     : %s", chainURL)
	if configPath != "" {
		log.Infof("  fleet config  : %s (SIGHUP reloads)", configPath)
		log.Infof("  status page   : http://%s:%d/status", advIP, httpPort)
	} else {
		log.Infof("  fleet config  : (none — single-machine mode; pass -config for fleet mode)")
	}
	log.Infof("  loglevel      : %s", lvl)

	if picked.IsWireless {
		log.Warnf("interface %s looks wireless — PXE TFTP frequently times out on WiFi; prefer wired", picked.Iface.Name)
	}
	if scriptOverride != "" {
		log.Infof("using custom iPXE script: %s", scriptOverride)
	}

	// Sanity-check the embedded asset wiring at startup so a broken
	// embed is reported once, not from a TFTP RRQ minutes later.
	if b, err := assets.ReadIPXE(assets.IPXEEFIx64); err != nil {
		log.Warnf("embedded asset check failed: %v", err)
	} else {
		log.Infof("embedded netboot.xyz.efi ready (%d bytes)", len(b))
	}
}
