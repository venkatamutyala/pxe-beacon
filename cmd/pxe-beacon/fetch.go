package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/cache"
)

// runFetch is the entry point for `pxe-beacon fetch <target> [flags]`.
//
// Downloads the live-server ISO for `target` (e.g. ubuntu-22.04),
// extracts the Subiquity kernel + initrd + filesystem.squashfs into
// the data dir, and writes a manifest. Idempotent: re-running for an
// already-populated target is a no-op unless -force is passed.
//
// Designed to be a one-time-per-distro op the operator runs manually
// before starting `pxe-beacon serve`. Once the data dir is populated,
// the HTTP server's /assets/<target>/<file> routes serve from it.
func runFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "directory to store extracted assets")
	force := fs.Bool("force", false, "re-download even if target is already populated")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: pxe-beacon fetch <target> [flags]")
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Downloads + extracts an Ubuntu live-server ISO so its")
		fmt.Fprintln(fs.Output(), "Subiquity kernel + initrd + filesystem.squashfs can be")
		fmt.Fprintln(fs.Output(), "served by pxe-beacon for unattended autoinstalls.")
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Supported targets:")
		for _, t := range sortedTargets() {
			spec := cache.Targets[t]
			fmt.Fprintf(fs.Output(), "  %-14s -> %s\n", t, spec.ISOURL)
		}
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}

	if len(args) < 1 {
		fs.Usage()
		os.Exit(2)
	}
	target := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(2)
	}
	if _, ok := cache.Targets[target]; !ok {
		fmt.Fprintf(os.Stderr, "fetch: unknown target %q\n\n", target)
		fs.Usage()
		os.Exit(2)
	}

	c, err := cache.New(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch: %v\n", err)
		os.Exit(1)
	}

	if ok, m := c.IsPopulated(target); ok && !*force {
		fmt.Printf("fetch: %s already populated at %s (fetched %s)\n",
			target, filepath.Join(c.Root, target), m.FetchedAt.Format(time.RFC3339))
		fmt.Println("fetch: pass -force to redownload")
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	spec := cache.Targets[target]
	fmt.Printf("fetch: target = %s\n", target)
	fmt.Printf("fetch: source = %s\n", spec.ISOURL)
	fmt.Printf("fetch: dest   = %s\n", filepath.Join(c.Root, target))
	fmt.Println("fetch: downloading ISO (this is ~1.5GB, several minutes on most links)…")

	progressBar := newProgressPrinter()
	m, err := c.Fetch(ctx, target, cache.FetchOpts{
		Force:    *force,
		Progress: progressBar.update,
	})
	progressBar.done()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nfetch: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("fetch: extracted:")
	for _, name := range spec.Dests() {
		a := m.Files[name]
		fmt.Printf("  %s/%s   %8s   sha256=%s…\n",
			target, name, humanSize(a.Size), a.SHA256[:12])
	}
	fmt.Println("fetch: done.")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  pxe-beacon -config /etc/pxe-beacon/fleet.yaml -data-dir %s\n", c.Root)
	if target == "systemrescue" {
		// SystemRescue is a rescue intent, not a fleet boot target.
		fmt.Println("  (then PUT /api/v1/machines/{mac}/intent {\"action\":\"rescue\"} to boot it)")
	} else {
		fmt.Println("  (fleet.yaml entry with boot: " + target + " + cloud_init: ./your.yaml)")
	}
}

func sortedTargets() []string {
	out := []string{}
	for k := range cache.Targets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// defaultDataDir returns the default for -data-dir: a `.pxe-beacon`
// subfolder of the current working directory, so fetched distro assets
// and per-machine overrides land next to where you run the binary
// (discoverable + easy to clean up). $PXE_BEACON_DATA overrides it; the
// container image passes -data-dir /var/lib/pxe-beacon explicitly.
func defaultDataDir() string {
	if d := os.Getenv("PXE_BEACON_DATA"); d != "" {
		return d
	}
	if cwd, err := os.Getwd(); err == nil {
		return filepath.Join(cwd, ".pxe-beacon")
	}
	return "./.pxe-beacon"
}

// progressPrinter shows a `XX.X MB / YY.Y MB (zz%)` line that
// overwrites in place using \r. Throttled to ~5/sec.
type progressPrinter struct {
	last time.Time
}

func newProgressPrinter() *progressPrinter { return &progressPrinter{} }

func (p *progressPrinter) update(done, total int64) {
	now := time.Now()
	if now.Sub(p.last) < 200*time.Millisecond {
		return
	}
	p.last = now
	if total > 0 {
		pct := float64(done) * 100 / float64(total)
		fmt.Printf("\rfetch: %s / %s (%.1f%%)        ",
			humanSize(done), humanSize(total), pct)
	} else {
		fmt.Printf("\rfetch: %s                       ", humanSize(done))
	}
}

func (p *progressPrinter) done() { fmt.Println() }

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %sB", float64(b)/float64(div),
		strings.TrimSpace(" kMGTPE"[exp:exp+1]))
}
