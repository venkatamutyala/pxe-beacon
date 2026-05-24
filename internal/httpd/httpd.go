// Package httpd serves the iPXE binary (for UEFI HTTP-boot) and the
// templated boot.ipxe chain script. PLAN section 0 warns specifically
// that UEFI HTTP boot is picky about Content-Length, so we always set
// it explicitly and use a fixed-content ReadSeeker, never chunked.
package httpd

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	htmltmpl "html/template"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/assets"
	"github.com/venkatamutyala/pxe-beacon/internal/boot"
	"github.com/venkatamutyala/pxe-beacon/internal/cache"
	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

//go:embed status.html
var statusHTMLSrc string

// Tracker is the same interface the TFTP server uses to notify the
// proxyDHCP listener that a client has progressed.
type Tracker interface {
	NoteServed(mac string)
}

// Options carries deployment config.
type Options struct {
	Listen         string // ":8080" or "127.0.0.1:8080"
	AdvertisedIP   string // for templating into boot.ipxe
	HTTPPort       int    // for templating into per-MAC autoexec/cloud-init
	ChainURL       string // netboot.xyz menu URL by default
	IPXEScriptPath string // URL path the script is served at
	IPXEScriptFile string // optional path to override-on-disk template
	SetCrossCert   bool   // true to add `set crosscert` (PLAN gotcha)
	Logger         *narrlog.Logger
	Tracker        Tracker
	// Fleet enables the v0.2 /autoinstall and /status routes. When
	// nil, those routes 404 — preserving v0.1.3 behavior.
	Fleet *fleet.Fleet
	// FleetStatus is the in-memory tracker the autoinstall and
	// status handlers update + read. Required when Fleet is non-nil.
	FleetStatus *fleet.Tracker
	// DataDir is the on-disk directory populated by
	// `pxe-beacon fetch <target>`. When set, /assets/<target>/<file>
	// serves files from this directory. v0.4+ for Ubuntu Subiquity
	// autoinstall (the kernel + initrd + filesystem.squashfs that
	// don't exist as flat HTTP files anywhere upstream).
	DataDir string
	// TFTPAutoexec returns the exact bytes TFTP serves for
	// autoexec.ipxe. When non-nil, /debug/tftp/autoexec.ipxe returns
	// the same content over HTTP (curl-friendly diagnostic for ops
	// who can't easily run a TFTP client). v0.5.1+.
	TFTPAutoexec func() []byte
}

// Server is the pxe-beacon HTTP server.
type Server struct {
	opts       Options
	log        *narrlog.Logger
	mux        *http.ServeMux
	tmpl       *template.Template
	statusTmpl *htmltmpl.Template
	admin      *adminState
	startedAt  time.Time
}

// New builds the server but does not start it.
func New(o Options) (*Server, error) {
	if o.Logger == nil {
		return nil, errors.New("Options.Logger required")
	}
	if o.Listen == "" {
		o.Listen = ":8080"
	}
	if o.IPXEScriptPath == "" {
		o.IPXEScriptPath = "/boot.ipxe"
	}
	if o.ChainURL == "" {
		o.ChainURL = "https://boot.netboot.xyz/menu.ipxe"
	}

	tmpl, err := loadTemplate(o.IPXEScriptFile)
	if err != nil {
		return nil, fmt.Errorf("load iPXE script template: %w", err)
	}

	statusTmpl, err := htmltmpl.New("status").Funcs(htmltmpl.FuncMap{
		"humanDuration": humanDuration,
		"statusDot":     statusDot,
	}).Parse(statusHTMLSrc)
	if err != nil {
		return nil, fmt.Errorf("parse status.html: %w", err)
	}

	admin, err := newAdminState()
	if err != nil {
		return nil, fmt.Errorf("init admin state: %w", err)
	}
	s := &Server{
		opts:       o,
		log:        o.Logger.With("http"),
		mux:        http.NewServeMux(),
		tmpl:       tmpl,
		statusTmpl: statusTmpl,
		admin:      admin,
		startedAt:  time.Now(),
	}
	s.routes()
	return s, nil
}

// loadTemplate reads either the override file or the embedded
// default.
func loadTemplate(override string) (*template.Template, error) {
	var raw []byte
	var err error
	if override != "" {
		raw, err = os.ReadFile(override)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", override, err)
		}
	} else {
		raw, err = assets.ReadScript()
		if err != nil {
			return nil, err
		}
	}
	return template.New("boot.ipxe").Parse(string(raw))
}

// routes registers handlers.
//
//   - GET /                                    status / hint page
//   - GET /<ipxe-binary>                       iPXE binaries (for UEFI HTTP boot)
//   - GET /boot.ipxe                           chain script (legacy)
//   - GET /autoinstall/{mac}/autoexec.ipxe     per-MAC autoexec from TFTP redirector
//   - GET /autoinstall/{mac}/user-data         cloud-init user-data
//   - GET /autoinstall/{mac}/meta-data         cloud-init meta-data
//   - POST /autoinstall/{mac}/done             cloud-init phone_home callback
//   - GET /status                              HTML fleet status page
//   - GET /status.json                         JSON fleet status
//
// The /autoinstall and /status routes 404 when -config wasn't passed
// (Fleet is nil) — preserving v0.1.3 behavior for single-machine
// users.
func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleRoot)
	s.mux.HandleFunc("/netboot.xyz.efi", s.serveIPXE(assets.IPXEEFIx64))
	s.mux.HandleFunc("/netboot.xyz-snponly.efi", s.serveIPXE(assets.IPXESNPOnly))
	s.mux.HandleFunc("/netboot.xyz-arm64.efi", s.serveIPXE(assets.IPXEARM64))
	s.mux.HandleFunc("/netboot.xyz.kpxe", s.serveIPXE(assets.IPXELegacyBIOS))
	// Aliases for the same binaries — firmware sometimes uses these
	// names, and the proxyDHCP OFFER also uses the canonical name,
	// but we want curl to "just work" regardless.
	s.mux.HandleFunc("/ipxe.efi", s.serveIPXE(assets.IPXEEFIx64))
	s.mux.HandleFunc("/ipxe-arm64.efi", s.serveIPXE(assets.IPXEARM64))
	s.mux.HandleFunc("/undionly.kpxe", s.serveIPXE(assets.IPXELegacyBIOS))

	s.mux.HandleFunc(s.opts.IPXEScriptPath, s.handleScript)

	// v0.2 routes — only meaningful when Fleet is wired up. We
	// register them unconditionally; the handlers themselves return
	// 404 with a helpful message when Fleet is nil.
	s.mux.HandleFunc("GET /autoinstall/{mac}/autoexec.ipxe", s.handleAutoexec)
	s.mux.HandleFunc("GET /autoinstall/{mac}/user-data", s.handleUserData)
	s.mux.HandleFunc("GET /autoinstall/{mac}/meta-data", s.handleMetaData)
	s.mux.HandleFunc("GET /autoinstall/{mac}/preseed.cfg", s.handlePreseed)
	s.mux.HandleFunc("POST /autoinstall/{mac}/done", s.handleInstallerDone)
	s.mux.HandleFunc("GET /status", s.handleStatusHTML)
	s.mux.HandleFunc("GET /status.json", s.handleStatusJSON)
	s.mux.HandleFunc("GET /assets/{target}/{file}", s.handleAsset)

	// v0.5.1: debug route — returns the exact bytes TFTP would serve
	// for autoexec.ipxe. macOS BSD `tftp` has known hangs talking to
	// non-loopback IPs on the same host; curl-friendly diagnostic.
	s.mux.HandleFunc("GET /debug/tftp/autoexec.ipxe", s.handleDebugAutoexec)

	// Admin routes — loopback-only, CSRF-guarded on POST. Wildcard
	// {name...} captures slash-containing template paths like
	// "defaults/debian-preseed.cfg".
	s.mux.Handle("GET /admin", loopbackOnly(http.HandlerFunc(s.handleAdminIndex)))
	s.mux.Handle("GET /admin/templates/{name...}", loopbackOnly(http.HandlerFunc(s.handleAdminTemplateView)))
	s.mux.Handle("POST /admin/fleet", loopbackOnly(http.HandlerFunc(s.csrfGuard(s.handleAdminFleetSave))))
	s.mux.Handle("POST /admin/fleet/delete", loopbackOnly(http.HandlerFunc(s.csrfGuard(s.handleAdminFleetDelete))))
	s.mux.Handle("POST /admin/templates-reset/{name...}", loopbackOnly(http.HandlerFunc(s.csrfGuard(s.handleAdminTemplateReset))))
	s.mux.Handle("POST /admin/templates/{name...}", loopbackOnly(http.HandlerFunc(s.csrfGuard(s.handleAdminTemplateSave))))
	s.mux.Handle("POST /admin/reload", loopbackOnly(http.HandlerFunc(s.csrfGuard(s.handleAdminReload))))
}

// macHyphen is what iPXE's ${net0/mac:hexhyp} produces and what we
// canonicalize URL path segments to (colons in URLs are fine but ugly).
var macHyphen = regexp.MustCompile(`^[0-9a-fA-F]{2}(-[0-9a-fA-F]{2}){5}$`)

// extractMAC pulls and normalizes the {mac} path value. Returns the
// canonical colon-MAC, or "" + writes a 400 response.
func (s *Server) extractMAC(w http.ResponseWriter, r *http.Request) string {
	raw := r.PathValue("mac")
	if raw == "" {
		http.Error(w, "missing mac in URL", http.StatusBadRequest)
		return ""
	}
	// Accept hyphen-MAC (the canonical URL form) or colon-MAC.
	canon, err := fleet.CanonicalMAC(raw)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid mac %q: %v", raw, err), http.StatusBadRequest)
		return ""
	}
	return canon
}

// fleetReady returns true when Fleet + Tracker are both wired up. When
// not, the handler should 404 and tell the user to pass -config.
func (s *Server) fleetReady(w http.ResponseWriter) bool {
	if s.opts.Fleet == nil || s.opts.FleetStatus == nil {
		http.Error(w, "fleet mode not enabled (start pxe-beacon with -config <fleet.yaml>)", http.StatusNotFound)
		return false
	}
	return true
}

// handleAutoexec serves the per-MAC iPXE script. The TFTP redirector
// chains iPXE here; we look up the MAC's boot target and render the
// matching template (or the operator's custom script).
func (s *Server) handleAutoexec(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w) {
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	p := s.opts.Fleet.Lookup(mac)
	ctx := boot.RenderContext{
		Name:         p.Name,
		MAC:          mac,
		AdvertisedIP: s.opts.AdvertisedIP,
		HTTPPort:     s.opts.HTTPPort,
	}
	var body []byte
	var err error
	switch {
	case p.Boot == "custom":
		body, err = boot.RenderCustom(p.IPXEScript, ctx)
	case boot.IsBuiltIn(p.Boot):
		body, err = boot.RenderAutoexec(p.Boot, ctx)
	default:
		http.Error(w, fmt.Sprintf("unsupported boot target %q", p.Boot), http.StatusInternalServerError)
		return
	}
	if err != nil {
		s.log.Errorf("GET %s -> 500 render: %v", r.URL.Path, err)
		http.Error(w, fmt.Sprintf("render: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if _, err := w.Write(body); err != nil {
		s.log.Warnf("GET %s -> write failed: %v", r.URL.Path, err)
		return
	}
	label := mac
	if p.Name != "" {
		label = fmt.Sprintf("%s (%s)", p.Name, mac)
	}
	s.log.Infof("GET %s -> 200, %d bytes [target=%s, client=%s]",
		r.URL.Path, len(body), p.Boot, label)
}

// handleUserData serves the cloud-init user-data file referenced from
// fleet.yaml for this MAC. The file is run through text/template with
// {Name, MAC, AdvertisedIP, HTTPPort} so operators can keep a
// phone_home URL working without hardcoding their server IP.
func (s *Server) handleUserData(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w) {
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	p := s.opts.Fleet.Lookup(mac)
	var raw []byte
	var err error
	if p.CloudInit != "" {
		raw, err = os.ReadFile(p.CloudInit)
		if err != nil {
			s.log.Errorf("GET %s -> 500 read cloud_init: %v", r.URL.Path, err)
			http.Error(w, fmt.Sprintf("read cloud_init: %v", err), http.StatusInternalServerError)
			return
		}
	} else {
		// v0.5.0: no cloud_init in fleet.yaml → embedded default
		// (or operator override at <data-dir>/templates/defaults/
		// cloud-init.yaml).
		raw, err = assets.ReadDefault("cloud-init.yaml")
		if err != nil {
			s.log.Errorf("GET %s -> 500 read default cloud-init: %v", r.URL.Path, err)
			http.Error(w, fmt.Sprintf("default cloud-init: %v", err), http.StatusInternalServerError)
			return
		}
	}
	tmpl, err := template.New("user-data").Parse(string(raw))
	if err != nil {
		http.Error(w, fmt.Sprintf("parse cloud_init template: %v", err), http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"Name":         p.Name,
		"MAC":          mac,
		"MACHyp":       strings.ReplaceAll(mac, ":", "-"),
		"AdvertisedIP": s.opts.AdvertisedIP,
		"HTTPPort":     s.opts.HTTPPort,
	}); err != nil {
		http.Error(w, fmt.Sprintf("render cloud_init: %v", err), http.StatusInternalServerError)
		return
	}
	body := buf.Bytes()
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
	s.opts.FleetStatus.Note(mac, fleet.EventUserDataFetched)
	if s.opts.Tracker != nil {
		s.opts.Tracker.NoteServed("user-data-anon")
	}
	s.log.Infof("GET %s -> 200, %d bytes [client=%s]", r.URL.Path, len(body), labelOf(p.Name, mac))
}

// handlePreseed serves a Debian preseed.cfg for the requesting MAC.
//
// Three cases:
//
//  1. fleet entry has `preseed:` set → template + serve that file. If
//     `cloud_init:` is ALSO set, append a `late_command` that
//     installs cloud-init on the target and drops user-data /
//     meta-data into /var/lib/cloud/seed/nocloud/ so cloud-init runs
//     on first boot of the installed system.
//  2. fleet entry has `cloud_init:` only (no preseed) → serve a
//     "go interactive" stub so d-i prompts on the console; we don't
//     auto-generate a full preseed (disk layouts / passwords / etc.
//     are too opinionated to default).
//  3. neither → same "interactive stub" — d-i goes interactive.
//
// The interactive stub is technically a valid (empty) preseed; d-i
// fetches it, finds no answers, and prompts normally.
func (s *Server) handlePreseed(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w) {
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	p := s.opts.Fleet.Lookup(mac)

	tvars := map[string]any{
		"Name":         p.Name,
		"MAC":          mac,
		"MACHyp":       strings.ReplaceAll(mac, ":", "-"),
		"AdvertisedIP": s.opts.AdvertisedIP,
		"HTTPPort":     s.opts.HTTPPort,
	}

	var body []byte

	// v0.5.0: if operator didn't supply preseed:, fall back to the
	// embedded default (which can be overridden via the admin UI by
	// editing <data-dir>/templates/defaults/debian-preseed.cfg).
	var raw []byte
	var rerr error
	if p.Preseed != "" {
		raw, rerr = os.ReadFile(p.Preseed)
		if rerr != nil {
			s.log.Errorf("GET %s -> 500 read preseed: %v", r.URL.Path, rerr)
			http.Error(w, fmt.Sprintf("read preseed: %v", rerr), http.StatusInternalServerError)
			return
		}
	} else if p.Boot == "debian-12" || p.Boot == "debian-13" {
		raw, rerr = assets.ReadDefault("debian-preseed.cfg")
		if rerr != nil {
			s.log.Errorf("GET %s -> 500 read default preseed: %v", r.URL.Path, rerr)
			http.Error(w, fmt.Sprintf("default preseed: %v", rerr), http.StatusInternalServerError)
			return
		}
	}

	if raw != nil {
		tmpl, err := template.New("preseed").Parse(string(raw))
		if err != nil {
			http.Error(w, fmt.Sprintf("parse preseed template: %v", err), http.StatusInternalServerError)
			return
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, tvars); err != nil {
			http.Error(w, fmt.Sprintf("render preseed: %v", err), http.StatusInternalServerError)
			return
		}
		body = buf.Bytes()

		// Always append the cloud-init bridge when CloudInit is set
		// OR we used the embedded default preseed (which assumes
		// cloud-init.yaml will be served as user-data too — possibly
		// the embedded default). Bridge installs cloud-init on the
		// target + seeds /var/lib/cloud/seed/nocloud/ on first boot.
		// PXE expert fix #4: bridge also pins datasource_list so
		// cloud-init doesn't waste 120s probing Ec2/Azure/GCP.
		if p.CloudInit != "" || p.Preseed == "" {
			bridge := renderCloudInitBridge(tvars)
			if !bytes.HasSuffix(body, []byte("\n")) {
				body = append(body, '\n')
			}
			body = append(body, bridge...)
		}
	} else {
		body = []byte(`# pxe-beacon: no preseed configured for ` + mac + `
# (boot=` + p.Boot + `). For unattended d-i, set ` + "`boot: debian-12`" + ` or
# ` + "`boot: debian-13`" + ` in fleet.yaml — pxe-beacon then serves the
# embedded default preseed, with user pxe / password pxe.
`)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
	if s.opts.FleetStatus != nil {
		// Mark the machine as having entered the install stage. Use
		// the existing user-data-fetched event — preseed.cfg is the
		// Debian-side analog of cloud-init's user-data.
		s.opts.FleetStatus.Note(mac, fleet.EventUserDataFetched)
	}
	s.log.Infof("GET %s -> 200, %d bytes [client=%s, preseed=%t, cloud_init_bridge=%t]",
		r.URL.Path, len(body), labelOf(p.Name, mac), p.Preseed != "", p.Preseed != "" && p.CloudInit != "")
}

// renderCloudInitBridge produces the late_command directive that
// installs cloud-init on the target and seeds it with the operator's
// user-data + meta-data. Runs on FIRST BOOT of the installed system,
// not during d-i. Reuses pxe-beacon's existing /user-data and
// /meta-data endpoints — they already exist for Ubuntu, so we just
// fetch them again post-install.
func renderCloudInitBridge(vars map[string]any) []byte {
	// PXE expert fix #4: pin datasource_list so cloud-init's first
	// boot doesn't time out 120s probing Ec2/Azure/GCP before
	// finding the NoCloud seed dir.
	body := fmt.Sprintf(`
### cloud-init bridge — appended automatically by pxe-beacon (v0.5.0+).
### Runs on first boot of the installed system. Idempotent.
d-i preseed/late_command string \
  in-target apt-get update ; \
  in-target apt-get install -y --no-install-recommends cloud-init wget ca-certificates ; \
  in-target mkdir -p /var/lib/cloud/seed/nocloud ; \
  in-target sh -c 'echo "datasource_list: [NoCloud, None]" > /etc/cloud/cloud.cfg.d/90-pxe-beacon-nocloud.cfg' ; \
  in-target wget -q -O /var/lib/cloud/seed/nocloud/user-data http://%s:%d/autoinstall/%s/user-data ; \
  in-target wget -q -O /var/lib/cloud/seed/nocloud/meta-data http://%s:%d/autoinstall/%s/meta-data ; \
  in-target systemctl enable cloud-init.service cloud-config.service cloud-final.service cloud-init-local.service ;
`,
		vars["AdvertisedIP"], vars["HTTPPort"], vars["MACHyp"],
		vars["AdvertisedIP"], vars["HTTPPort"], vars["MACHyp"],
	)
	return []byte(body)
}

// handleMetaData serves a minimal cloud-init meta-data document
// computed from the fleet entry. NoCloud datasource requires
// instance-id at minimum; we also set local-hostname so the installed
// system gets the operator's chosen name.
func (s *Server) handleMetaData(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w) {
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	p := s.opts.Fleet.Lookup(mac)
	name := p.Name
	if name == "" {
		name = strings.ReplaceAll(mac, ":", "-")
	}
	body := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", name, name)
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = io.WriteString(w, body)
	s.log.Infof("GET %s -> 200, %d bytes [client=%s]", r.URL.Path, len(body), labelOf(p.Name, mac))
}

// handleInstallerDone is the cloud-init phone_home callback. Once
// hit, the machine transitions to "installer-done" in the status
// tracker.
func (s *Server) handleInstallerDone(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w) {
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	p := s.opts.Fleet.Lookup(mac)
	s.opts.FleetStatus.Note(mac, fleet.EventInstallerDone)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	body := "ok\n"
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = io.WriteString(w, body)
	s.log.Infof("POST %s -> 200, phone_home received [client=%s]", r.URL.Path, labelOf(p.Name, mac))
}

// handleAsset serves a file from DataDir/<target>/<file>. The target
// + file names are validated to reject path traversal (cache.AssetPath
// does the check). Used by the Ubuntu autoexec templates to fetch
// handleDebugAutoexec returns the same bytes TFTP serves for
// autoexec.ipxe. Lets operators curl the dispatch script when the
// macOS BSD tftp client hangs (a known issue talking to non-loopback
// IPs on the same machine).
func (s *Server) handleDebugAutoexec(w http.ResponseWriter, r *http.Request) {
	if s.opts.TFTPAutoexec == nil {
		http.Error(w, "TFTP autoexec not configured (start pxe-beacon with -config)", http.StatusNotFound)
		return
	}
	body := s.opts.TFTPAutoexec()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Pxe-Beacon-Note", "this is the EXACT byte stream TFTP serves for autoexec.ipxe")
	_, _ = w.Write(body)
}

// vmlinuz / initrd / filesystem.squashfs that `pxe-beacon fetch`
// previously extracted from the live-server ISO.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
	file := r.PathValue("file")
	if s.opts.DataDir == "" {
		http.Error(w, "asset serving disabled — start pxe-beacon with -data-dir or run `pxe-beacon fetch "+target+"` first", http.StatusNotFound)
		return
	}
	c, err := cache.New(s.opts.DataDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("data dir: %v", err), http.StatusInternalServerError)
		return
	}
	path, err := c.AssetPath(target, file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, fmt.Sprintf("asset %s/%s not found — run `pxe-beacon fetch %s` to populate it", target, file, target), http.StatusNotFound)
			s.log.Warnf("GET %s -> 404 (file not in data dir; pxe-beacon fetch needed)", r.URL.Path)
			return
		}
		http.Error(w, fmt.Sprintf("open: %v", err), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, fmt.Sprintf("stat: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		s.log.Infof("HEAD %s -> 200, %d bytes (asset)", r.URL.Path, fi.Size())
		return
	}
	http.ServeContent(w, r, file, fi.ModTime(), f)
	s.log.Infof("GET %s -> 200, %d bytes (asset, %s)", r.URL.Path, fi.Size(), r.RemoteAddr)
}

// handleStatusJSON renders the in-memory tracker snapshot as JSON.
// Stable shape; safe to consume from monitoring / scripts.
func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w) {
		return
	}
	snap := s.opts.FleetStatus.Snapshot()
	payload := map[string]any{
		"server": map[string]any{
			"advertised_ip": s.opts.AdvertisedIP,
			"http_port":     s.opts.HTTPPort,
			"uptime_s":      int(time.Since(s.startedAt).Seconds()),
			"started_at":    s.startedAt.UTC().Format(time.RFC3339),
		},
		"machines": snap,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		s.log.Warnf("GET /status.json: encode error: %v", err)
	}
}

// handleStatusHTML renders the same data as a plain auto-refreshing
// HTML page.
func (s *Server) handleStatusHTML(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w) {
		return
	}
	snap := s.opts.FleetStatus.Snapshot()
	type viewModel struct {
		AdvertisedIP string
		HTTPPort     int
		UptimeSec    int
		StartedAt    string
		Machines     []fleet.Status
		Now          time.Time
	}
	vm := viewModel{
		AdvertisedIP: s.opts.AdvertisedIP,
		HTTPPort:     s.opts.HTTPPort,
		UptimeSec:    int(time.Since(s.startedAt).Seconds()),
		StartedAt:    s.startedAt.UTC().Format(time.RFC3339),
		Machines:     snap,
		Now:          time.Now(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.statusTmpl.Execute(w, vm); err != nil {
		s.log.Warnf("GET /status: render error: %v", err)
	}
}

func labelOf(name, mac string) string {
	if name == "" {
		return mac
	}
	return fmt.Sprintf("%s (%s)", name, mac)
}

func humanDuration(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func statusDot(state string) string {
	switch state {
	case "":
		return "○" // pending
	case string(fleet.EventInstallerDone):
		return "●" // done
	default:
		return "◐" // in progress
	}
}

// Serve binds and runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.Listen)
	if err != nil {
		hint := ""
		if strings.Contains(err.Error(), "address already in use") {
			hint = " (hint: another process is already on this port — see `lsof -i :<port>`)"
		} else if strings.Contains(err.Error(), "permission denied") {
			hint = " (hint: ports <1024 need root)"
		}
		return fmt.Errorf("bind http %s: %w%s", s.opts.Listen, err, hint)
	}
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.log.Infof("listening on %s (script path %s)", ln.Addr(), s.opts.IPXEScriptPath)

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		s.log.Infof("http: shutdown requested")
		shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		<-done
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.log.Warnf(`GET %q -> 404 (unknown path; try / or %s)`, r.URL.Path, s.opts.IPXEScriptPath)
		http.NotFound(w, r)
		return
	}
	body := fmt.Sprintf(`pxe-beacon HTTP server
endpoints:
  /boot.ipxe                - iPXE chain script (templated)
  /netboot.xyz.efi          - UEFI x86_64 iPXE binary (HTTP boot)
  /netboot.xyz-arm64.efi    - UEFI ARM64 iPXE binary
  /netboot.xyz.kpxe         - legacy BIOS iPXE binary
advertised-ip: %s
chain-url:     %s
`, s.opts.AdvertisedIP, s.opts.ChainURL)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = io.WriteString(w, body)
	s.log.Infof(`GET / -> 200, %d bytes (%s)`, len(body), r.RemoteAddr)
}

func (s *Server) serveIPXE(kind assets.IPXEKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := assets.ReadIPXE(kind)
		if err != nil {
			s.log.Errorf("GET %q -> 500 reading embedded asset: %v", r.URL.Path, err)
			http.Error(w, "asset error", http.StatusInternalServerError)
			return
		}
		// PLAN section 0 explicitly: UEFI HTTP boot is picky about
		// Content-Length — always set it.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == http.MethodHead {
			// Headers only — net/http will skip the body for HEAD,
			// but make it explicit so the log line is honest.
			w.WriteHeader(http.StatusOK)
			s.log.Infof(`HEAD %s -> 200, %d bytes`, r.URL.Path, len(data))
			return
		}
		http.ServeContent(w, r, kind.String(), time.Time{}, bytes.NewReader(data))
		s.log.Infof(`GET %s -> 200, %d bytes (%s)`, r.URL.Path, len(data), r.RemoteAddr)
		if s.opts.Tracker != nil {
			s.opts.Tracker.NoteServed("http-anon")
		}
	}
}

type scriptVars struct {
	AdvertisedIP string
	ChainURL     string
	SetCrossCert bool
}

func (s *Server) handleScript(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	err := s.tmpl.Execute(&buf, scriptVars{
		AdvertisedIP: s.opts.AdvertisedIP,
		ChainURL:     s.opts.ChainURL,
		SetCrossCert: s.opts.SetCrossCert,
	})
	if err != nil {
		s.log.Errorf("template render: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	body := buf.Bytes()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		s.log.Infof(`HEAD %s -> 200, %d bytes`, r.URL.Path, len(body))
		return
	}
	_, _ = w.Write(body)
	s.log.Infof(`GET %s -> 200, %d bytes (%s)`, r.URL.Path, len(body), r.RemoteAddr)
	if s.opts.Tracker != nil {
		s.opts.Tracker.NoteServed("ipxe-anon")
	}
}
