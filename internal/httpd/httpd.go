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
	"github.com/venkatamutyala/pxe-beacon/internal/callbacktoken"
	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
	"github.com/venkatamutyala/pxe-beacon/internal/installlog"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
	"github.com/venkatamutyala/pxe-beacon/internal/pending"
	"github.com/venkatamutyala/pxe-beacon/pkg/pxebeacon"
	"gopkg.in/yaml.v3"
)

//go:embed status.html
var statusHTMLSrc string

//go:embed openapi.yaml
var openAPISpec []byte

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
	// IPXEDispatch, when non-nil, makes /boot.ipxe (the iPXE-stage
	// OFFER URL) return the per-MAC dispatch script INSTEAD of the
	// static chain template. v0.6.0+: vanilla iPXE (no EMBED) chains
	// to /boot.ipxe at iPXE-stage; this is where the dispatch logic
	// needs to live for that path to work. When nil, /boot.ipxe
	// returns the v0.1.3-era static template (chain to ChainURL).
	IPXEDispatch func() []byte
	// ClientNetmask, when non-empty (e.g. "255.255.0.0"), is the
	// widened netmask the operator chose for cross-/24 routing on
	// flat /16+ networks. It propagates into preseed late_commands
	// so the installed system can install a link-scope route to
	// the wider network — without that, cloud-init phone_home back
	// to pxe-beacon fails after first reboot. v0.6.0+.
	ClientNetmask string
	// Pending owns per-machine pending actions (deploy/rescue/cancel).
	// /api/v1/machines/{mac}/{deploy,rescue,cancel} mutate it;
	// proxyDHCP reads via callback. handleInstallerDone cancels on
	// cloud-init phone_home. May be nil — when nil, the API routes
	// 404 and proxyDHCP runs without a pending filter (every fleet
	// member is effectively pending-deploy, matching <= v0.6.x).
	// v0.7.1+ (was ArmState in v0.7.0).
	Pending *pending.Store
	// CallbackTokens mints + verifies the bearer tokens guarding the
	// public phone-home callbacks (/done, /log). When nil the token
	// feature is off: handlers serve untokenized callback URLs and the
	// verify middleware is a no-op. v0.12.0+.
	CallbackTokens *callbacktoken.Signer
	// RequireCallbackToken, when true, rejects a missing/invalid token
	// on /done + /log with 403. When false (advisory), a MISSING token
	// is accepted + logged but a PRESENT-but-invalid one is still
	// rejected. Ignored when CallbackTokens is nil. v0.12.0+.
	RequireCallbackToken bool
	// InstallLog holds per-MAC diagnostic log tails posted to /log.
	// When nil, /log + /logs 404. v0.12.0+.
	InstallLog *installlog.Store
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
	s.mux.HandleFunc("/snponly.efi", s.serveIPXE(assets.IPXESNPOnly))
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
	s.mux.HandleFunc("GET /autoinstall/{mac}/kickstart.cfg", s.handleKickstart)
	// v0.11.0: SystemRescue rescue config + its autorun setup script,
	// served when a rescue intent is queued (see dispatch.go).
	s.mux.HandleFunc("GET /autoinstall/{mac}/sysrescue.yaml", s.handleSysrescueConfig)
	s.mux.HandleFunc("GET /autoinstall/{mac}/sysrescue-setup.sh", s.handleSysrescueSetup)
	// v0.9.0: /done is the cloud-init phone_home wire endpoint (kept as
	// a permanent alias); /events is the canonical API form. /done is
	// public (the booting box posts to it) so it's token-guarded;
	// /events is loopback-only (operator/API) so it isn't.
	s.mux.Handle("POST /autoinstall/{mac}/done", s.requireCallbackToken(http.HandlerFunc(s.handleInstallEvent)))
	s.mux.Handle("POST /api/v1/machines/{mac}/events", loopbackOnly(http.HandlerFunc(s.handleInstallEvent)))
	// v0.12.0: public diagnostic-log capture (token-guarded) + the
	// loopback-only reader.
	s.mux.Handle("POST /autoinstall/{mac}/log", s.requireCallbackToken(http.HandlerFunc(s.handleInstallLog)))
	s.mux.Handle("GET /api/v1/machines/{mac}/logs", loopbackOnly(http.HandlerFunc(s.handleGetLogs)))
	s.mux.HandleFunc("GET /status", s.handleStatusHTML)
	s.mux.HandleFunc("GET /status.json", s.handleStatusJSON)

	// v0.9.0: health probes. /healthz is "the binary is alive and
	// answering HTTP"; /readyz is "the binary is configured and
	// ready to do its job". Distinguishing them matches kubelet /
	// load-balancer conventions.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	// v0.9.0: the hand-written OpenAPI 3 spec for /api/v1/*. Served
	// read-only for SDK/Terraform-provider codegen.
	s.mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(openAPISpec)))
		_, _ = w.Write(openAPISpec)
	})
	// {file...} (not {file}) so archiso can fetch the nested
	// sysresccd/x86_64/airootfs.sfs that SystemRescue's boot firmware
	// constructs itself. Ubuntu's flat vmlinuz/initrd/squashfs still
	// match.
	s.mux.HandleFunc("GET /assets/{target}/{file...}", s.handleAsset)

	// v0.8.0: K8s-style declarative boot-intent API. Hard-cut from
	// v0.7.1 (no aliases). Loopback-only, no CSRF (curl/scripts/UI,
	// not cookie-auth browsers). Tool-friendly: PUT is idempotent so
	// Terraform / Ansible / React Query map cleanly.
	s.mux.Handle("PUT /api/v1/machines/{mac}/intent", loopbackOnly(http.HandlerFunc(s.handleAPISetIntent)))
	s.mux.Handle("GET /api/v1/machines/{mac}/intent", loopbackOnly(http.HandlerFunc(s.handleAPIGetIntent)))
	// v0.9.0: fleet-config CRUD. The JSON API is now the single
	// mutation control plane; the /admin HTML page is a fetch() client
	// of these. Loopback-only + Content-Type: application/json (the
	// CSRF defense) + audit logging, in lieu of a CSRF token.
	s.mux.Handle("POST /api/v1/machines", loopbackOnly(http.HandlerFunc(s.handleAPICreateMachine)))
	s.mux.Handle("PUT /api/v1/machines/{mac}", loopbackOnly(http.HandlerFunc(s.handleAPIUpdateMachine)))
	s.mux.Handle("DELETE /api/v1/machines/{mac}", loopbackOnly(http.HandlerFunc(s.handleAPIDeleteMachine)))
	s.mux.Handle("GET /api/v1/machines/{mac}", loopbackOnly(http.HandlerFunc(s.handleAPIMachine)))
	s.mux.Handle("GET /api/v1/machines", loopbackOnly(http.HandlerFunc(s.handleAPIList)))

	// v0.5.1: debug route — returns the exact bytes TFTP would serve
	// for autoexec.ipxe. macOS BSD `tftp` has known hangs talking to
	// non-loopback IPs on the same host; curl-friendly diagnostic.
	s.mux.HandleFunc("GET /debug/tftp/autoexec.ipxe", s.handleDebugAutoexec)

	// v0.5.4: iPXE phone-home. The dispatch script hits this URL
	// before the iseq dispatch so we LOG what iPXE's settings actually
	// expand to (mac, net0/mac, net1/mac, ip, gateway, dns) — much
	// more reliable than reading the screen during a fast boot.
	s.mux.HandleFunc("GET /debug/iPXE-state", s.handleDebugIPXEState)
	// v0.5.6: path-based probe route. Some iPXE builds parse `&` in
	// URLs strangely; path segments are always safe. Each chain
	// emits one piece of info: /debug/probe/<key>/<value>.
	s.mux.HandleFunc("GET /debug/probe/{key}/{value...}", s.handleDebugProbe)
	s.mux.HandleFunc("GET /debug/probe/{key}", s.handleDebugProbe)

	// Admin routes — loopback-only, CSRF-guarded on POST. Wildcard
	// {name...} captures slash-containing template paths like
	// "defaults/debian-preseed.cfg".
	s.mux.Handle("GET /admin", loopbackOnly(http.HandlerFunc(s.handleAdminIndex)))
	s.mux.Handle("GET /admin/templates/{name...}", loopbackOnly(http.HandlerFunc(s.handleAdminTemplateView)))
	// v0.9.0: fleet CRUD moved to the JSON API (POST/PUT/DELETE
	// /api/v1/machines). The /admin page is now a fetch() client of
	// those; the old form-encoded /admin/fleet routes are gone.
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
		s.writeError(w, r, http.StatusBadRequest, pxebeacon.ErrCodeMACMissing, "missing mac in URL", nil)
		return ""
	}
	// Accept hyphen-MAC (the canonical URL form) or colon-MAC.
	canon, err := fleet.CanonicalMAC(raw)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, pxebeacon.ErrCodeMACInvalid,
			fmt.Sprintf("invalid mac %q: %v", raw, err), map[string]any{"input": raw})
		return ""
	}
	return canon
}

// fleetReady returns true when Fleet + Tracker are both wired up. When
// not, it writes a content-negotiated error and returns false.
//
// v0.9.0: 503 (not 404) — "service up, feature not configured" — and
// content-negotiated so an API client gets the structured envelope.
func (s *Server) fleetReady(w http.ResponseWriter, r *http.Request) bool {
	if s.opts.Fleet == nil || s.opts.FleetStatus == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, pxebeacon.ErrCodeFleetNotLoaded,
			"fleet mode not enabled (start pxe-beacon with -config <fleet.yaml>)", nil)
		return false
	}
	return true
}

// handleAutoexec serves the per-MAC iPXE script. The TFTP redirector
// chains iPXE here; we look up the MAC's boot target and render the
// matching template (or the operator's custom script).
func (s *Server) handleAutoexec(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
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
	if !s.fleetReady(w, r) {
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
	token := s.mintCallbackToken(mac)
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"Name":          p.Name,
		"MAC":           mac,
		"MACHyp":        strings.ReplaceAll(mac, ":", "-"),
		"AdvertisedIP":  s.opts.AdvertisedIP,
		"HTTPPort":      s.opts.HTTPPort,
		"Params":        p.Params,
		"CallbackToken": token,
	}); err != nil {
		http.Error(w, fmt.Sprintf("render cloud_init: %v", err), http.StatusInternalServerError)
		return
	}
	body := buf.Bytes()
	// v0.12.0: pxe-beacon OWNS phone_home. Append the tokenized callback
	// so the operator never writes one (fleet load rejects it if they
	// do). YAML only — a non-cloud-config payload (shell/jinja/multipart)
	// can't take an appended block, so warn + serve it untouched.
	if block := s.cloudInitPhoneHome(mac, token); block != nil {
		if isYAMLCloudConfig(body) {
			if !bytes.HasSuffix(body, []byte("\n")) {
				body = append(body, '\n')
			}
			body = append(body, block...)
		} else {
			s.log.Warnf("GET %s: cloud-init payload for %s is not #cloud-config/autoinstall YAML; skipping phone_home injection (machine won't auto-report installer-done)",
				r.URL.Path, mac)
		}
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
	s.opts.FleetStatus.Note(mac, fleet.EventUserDataFetched)
	if s.opts.Tracker != nil {
		s.opts.Tracker.NoteServed("user-data-anon")
	}
	s.log.Infof("GET %s -> 200, %d bytes [client=%s]", r.URL.Path, len(body), labelOf(p.Name, mac))
}

// cloudInitPhoneHome builds the pxe-beacon-owned phone_home block appended
// to every served cloud-init. The token (when the feature is on) rides the
// URL query — cloud-init's phone_home can't set headers.
func (s *Server) cloudInitPhoneHome(mac, token string) []byte {
	url := fmt.Sprintf("http://%s:%d/autoinstall/%s/done",
		s.opts.AdvertisedIP, s.opts.HTTPPort, strings.ReplaceAll(mac, ":", "-"))
	if token != "" {
		url += "?t=" + token
	}
	return []byte(fmt.Sprintf(`
# phone_home appended by pxe-beacon — it owns this callback (carries the
# auth token). Do NOT define your own phone_home; fleet load rejects it.
phone_home:
  url: %s
  post: all
  tries: 10
`, url))
}

// isYAMLCloudConfig reports whether b is a cloud-config / Subiquity
// autoinstall YAML document we can safely append a top-level phone_home to.
// Shell-script (#!), jinja, and MIME-multipart user-data are not.
func isYAMLCloudConfig(b []byte) bool {
	t := strings.TrimSpace(string(b))
	switch {
	case strings.HasPrefix(t, "#!"),
		strings.HasPrefix(t, "## template: jinja"),
		strings.HasPrefix(t, "Content-Type: multipart"):
		return false
	}
	var m map[string]any
	return yaml.Unmarshal(b, &m) == nil && m != nil
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
	if !s.fleetReady(w, r) {
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	p := s.opts.Fleet.Lookup(mac)

	tvars := map[string]any{
		"Name":             p.Name,
		"MAC":              mac,
		"MACHyp":           strings.ReplaceAll(mac, ":", "-"),
		"AdvertisedIP":     s.opts.AdvertisedIP,
		"HTTPPort":         s.opts.HTTPPort,
		"ClientNetmask":    s.opts.ClientNetmask,
		"WiderNetworkCIDR": widerNetworkCIDR(s.opts.AdvertisedIP, s.opts.ClientNetmask),
		"Params":           p.Params,
		"CallbackToken":    s.mintCallbackToken(mac),
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

// handleKickstart serves a RHEL-family kickstart.cfg for the requesting
// MAC. Used by rocky-9 / alma-9 boot targets — Anaconda's equivalent
// of Debian's preseed.cfg.
//
// Three cases mirror handlePreseed:
//  1. fleet entry has `kickstart:` set → template + serve that file.
//  2. fleet entry has boot rocky-9 / alma-9 (no kickstart) → embedded
//     default with user pxe / password pxe, cloud-init seeded in %post.
//  3. neither → "no kickstart configured" stub (Anaconda goes
//     interactive).
//
// Template vars are the same as handlePreseed PLUS RepoBaseURL — the
// distro-specific BaseOS URL (Rocky vs Alma) is selected per fleet
// entry's boot target.
func (s *Server) handleKickstart(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	p := s.opts.Fleet.Lookup(mac)

	repoBase := ""
	switch p.Boot {
	case "rocky-9":
		repoBase = "https://download.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os"
	case "alma-9":
		repoBase = "https://repo.almalinux.org/almalinux/9/BaseOS/x86_64/os"
	}

	tvars := map[string]any{
		"Name":             p.Name,
		"MAC":              mac,
		"MACHyp":           strings.ReplaceAll(mac, ":", "-"),
		"AdvertisedIP":     s.opts.AdvertisedIP,
		"HTTPPort":         s.opts.HTTPPort,
		"ClientNetmask":    s.opts.ClientNetmask,
		"WiderNetworkCIDR": widerNetworkCIDR(s.opts.AdvertisedIP, s.opts.ClientNetmask),
		"RepoBaseURL":      repoBase,
		"CallbackToken":    s.mintCallbackToken(mac),
	}

	var body []byte
	var raw []byte
	var rerr error
	switch {
	case p.Kickstart != "":
		raw, rerr = os.ReadFile(p.Kickstart)
		if rerr != nil {
			s.log.Errorf("GET %s -> 500 read kickstart: %v", r.URL.Path, rerr)
			http.Error(w, fmt.Sprintf("read kickstart: %v", rerr), http.StatusInternalServerError)
			return
		}
	case p.Boot == "rocky-9" || p.Boot == "alma-9":
		raw, rerr = assets.ReadDefault("rhel-kickstart.cfg")
		if rerr != nil {
			s.log.Errorf("GET %s -> 500 read default kickstart: %v", r.URL.Path, rerr)
			http.Error(w, fmt.Sprintf("default kickstart: %v", rerr), http.StatusInternalServerError)
			return
		}
	}

	if raw != nil {
		// Template funcs — `replace` is used in the kickstart to derive
		// AppStream URL from BaseOS URL.
		funcs := template.FuncMap{
			"replace": func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
		}
		tmpl, err := template.New("kickstart").Funcs(funcs).Parse(string(raw))
		if err != nil {
			http.Error(w, fmt.Sprintf("parse kickstart template: %v", err), http.StatusInternalServerError)
			return
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, tvars); err != nil {
			http.Error(w, fmt.Sprintf("render kickstart: %v", err), http.StatusInternalServerError)
			return
		}
		body = buf.Bytes()
	} else {
		body = []byte(`# pxe-beacon: no kickstart configured for ` + mac + `
# (boot=` + p.Boot + `). For unattended Rocky/Alma install, set
# ` + "`boot: rocky-9`" + ` or ` + "`boot: alma-9`" + ` in fleet.yaml.
`)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
	if s.opts.FleetStatus != nil {
		s.opts.FleetStatus.Note(mac, fleet.EventUserDataFetched)
	}
	s.log.Infof("GET %s -> 200, %d bytes [client=%s, kickstart=%t]",
		r.URL.Path, len(body), labelOf(p.Name, mac), p.Kickstart != "")
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
	if !s.fleetReady(w, r) {
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

// rescueTemplateVars builds the standard template namespace shared by
// the SystemRescue config + autorun script, mirroring handleUserData.
func (s *Server) rescueTemplateVars(mac string, p fleet.Profile) map[string]any {
	return map[string]any{
		"Name":         p.Name,
		"MAC":          mac,
		"MACHyp":       strings.ReplaceAll(mac, ":", "-"),
		"AdvertisedIP": s.opts.AdvertisedIP,
		"HTTPPort":     s.opts.HTTPPort,
		"Params":       p.Params,
	}
}

// handleSysrescueConfig serves the per-MAC SystemRescue sysrescuecfg
// YAML (loaded via the kernel's sysrescuecfg= param when a rescue
// intent is armed). Uses the operator's `rescue:` file if set, else
// the embedded default — both Go-templated with the standard vars so
// params.rescue_root_password / params.ssh_authorized_key flow through.
func (s *Server) handleSysrescueConfig(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	p := s.opts.Fleet.Lookup(mac)
	var raw []byte
	var err error
	if p.Rescue != "" {
		raw, err = os.ReadFile(p.Rescue)
		if err != nil {
			s.log.Errorf("GET %s -> 500 read rescue: %v", r.URL.Path, err)
			http.Error(w, fmt.Sprintf("read rescue: %v", err), http.StatusInternalServerError)
			return
		}
	} else {
		raw, err = assets.ReadDefault("sysrescue.yaml")
		if err != nil {
			s.log.Errorf("GET %s -> 500 read default sysrescue: %v", r.URL.Path, err)
			http.Error(w, fmt.Sprintf("default sysrescue: %v", err), http.StatusInternalServerError)
			return
		}
	}
	s.serveTemplated(w, r, "sysrescue.yaml", raw, p, mac, "text/yaml; charset=utf-8")
}

// handleSysrescueSetup serves the autorun shell script that injects the
// operator SSH key + ensures sshd, referenced from sysrescue.yaml's
// autorun.exec block. Always the embedded default (overridable on disk).
func (s *Server) handleSysrescueSetup(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	p := s.opts.Fleet.Lookup(mac)
	raw, err := assets.ReadDefault("sysrescue-setup.sh")
	if err != nil {
		s.log.Errorf("GET %s -> 500 read default sysrescue-setup: %v", r.URL.Path, err)
		http.Error(w, fmt.Sprintf("default sysrescue-setup: %v", err), http.StatusInternalServerError)
		return
	}
	s.serveTemplated(w, r, "sysrescue-setup.sh", raw, p, mac, "text/x-shellscript; charset=utf-8")
}

// serveTemplated parses + executes raw against the rescue template vars
// and writes the result with the given content type. Shared by the two
// SystemRescue handlers.
func (s *Server) serveTemplated(w http.ResponseWriter, r *http.Request, name string, raw []byte, p fleet.Profile, mac, contentType string) {
	tmpl, err := template.New(name).Parse(string(raw))
	if err != nil {
		http.Error(w, fmt.Sprintf("parse %s template: %v", name, err), http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, s.rescueTemplateVars(mac, p)); err != nil {
		http.Error(w, fmt.Sprintf("render %s: %v", name, err), http.StatusInternalServerError)
		return
	}
	body := buf.Bytes()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
	s.log.Infof("GET %s -> 200, %d bytes [client=%s]", r.URL.Path, len(body), labelOf(p.Name, mac))
}

// notePhase records an install-lifecycle phase and adjusts pending
// intent. Shared by the two routes that report observed state:
//
//	POST /autoinstall/{mac}/done            (cloud-init phone_home; legacy alias)
//	POST /api/v1/machines/{mac}/events       (v0.9.0 canonical)
//
//	installer-done   → cancel a pending INSTALL (keep rescue — a stale
//	                   phone_home from a prior install must not clear a
//	                   freshly-queued rescue).
//	installer-failed → KEEP pending intact: the operator wants a retry
//	                   to re-PXE; cancelling would strand a half-installed
//	                   box on local disk.
func (s *Server) notePhase(mac string, phase fleet.Event) {
	s.opts.FleetStatus.Note(mac, phase)
	if phase == fleet.EventInstallerDone && s.opts.Pending != nil {
		if action, _, _, ok := s.opts.Pending.Status(mac); ok && action == pending.ActionInstall {
			s.opts.Pending.Cancel(mac)
		}
	}
}

// handleInstallEvent serves BOTH POST /autoinstall/{mac}/done (the
// cloud-init phone_home wire endpoint — form-encoded, no `phase`,
// defaults to installer-done) AND POST /api/v1/machines/{mac}/events
// (JSON or form, explicit `phase`). The success response is
// content-negotiated: plain "ok" for cloud-init, the machine view for
// API clients. v0.9.0+.
func (s *Server) handleInstallEvent(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	p := s.opts.Fleet.Lookup(mac)

	phase, reason := parseEventPhase(r)
	var ev fleet.Event
	switch phase {
	case "", "installer-done", "done":
		ev = fleet.EventInstallerDone
	case "installer-failed", "failed":
		ev = fleet.EventInstallerFailed
	default:
		s.writeError(w, r, http.StatusBadRequest, pxebeacon.ErrCodeActionInvalid,
			`phase must be "installer-done" or "installer-failed"`,
			map[string]any{"got": phase})
		return
	}
	s.notePhase(mac, ev)
	s.log.Infof("audit event=install-event phase=%s reason=%q target_mac=%s target_name=%q from=%s",
		ev, reason, mac, p.Name, r.RemoteAddr)

	if wantsJSON(r) {
		writeJSON(w, s.buildMachineView(mac, p))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	body := "ok\n"
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = io.WriteString(w, body)
}

// mintCallbackToken returns a bearer token for canonMAC, or "" when the
// token feature is disabled (CallbackTokens nil). Templated into served
// callback URLs as {{.CallbackToken}}.
func (s *Server) mintCallbackToken(canonMAC string) string {
	if s.opts.CallbackTokens == nil {
		return ""
	}
	return s.opts.CallbackTokens.Mint(canonMAC)
}

// requireCallbackToken guards the public callbacks (/done, /log). It reads
// the token from the `t` query param and verifies it against the path MAC.
//
//   - token feature off (CallbackTokens nil) → pass through.
//   - present-but-invalid token → 403 ALWAYS (forged/expired is never ok).
//   - missing token → 403 if RequireCallbackToken, else accept + warn
//     (advisory mode, so operators can roll out before enforcing).
func (s *Server) requireCallbackToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.CallbackTokens == nil {
			next.ServeHTTP(w, r)
			return
		}
		mac, err := fleet.CanonicalMAC(r.PathValue("mac"))
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, pxebeacon.ErrCodeMACInvalid,
				"invalid MAC in path", map[string]any{"mac": r.PathValue("mac")})
			return
		}
		tok := r.URL.Query().Get("t")
		if tok == "" {
			if s.opts.RequireCallbackToken {
				s.log.Warnf("audit event=callback-rejected reason=missing-token target_mac=%s from=%s path=%s",
					mac, r.RemoteAddr, r.URL.Path)
				s.writeError(w, r, http.StatusForbidden, pxebeacon.ErrCodeCallbackToken,
					"missing callback token", map[string]any{"mac": mac})
				return
			}
			s.log.Warnf("audit event=callback-unauthenticated reason=missing-token target_mac=%s from=%s path=%s (advisory mode; set -require-callback-token to enforce)",
				mac, r.RemoteAddr, r.URL.Path)
			next.ServeHTTP(w, r)
			return
		}
		if err := s.opts.CallbackTokens.Verify(mac, tok); err != nil {
			s.log.Warnf("audit event=callback-rejected reason=%v target_mac=%s from=%s path=%s",
				err, mac, r.RemoteAddr, r.URL.Path)
			s.writeError(w, r, http.StatusForbidden, pxebeacon.ErrCodeCallbackToken,
				"invalid callback token", map[string]any{"mac": mac})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleInstallLog captures diagnostic output an installer posts to
// /autoinstall/{mac}/log (token-guarded by the middleware). Body is raw
// text (dmesg + installer/cloud-init logs); we keep the tail per MAC.
func (s *Server) handleInstallLog(w http.ResponseWriter, r *http.Request) {
	if s.opts.InstallLog == nil {
		http.Error(w, "log capture disabled", http.StatusNotFound)
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, installlog.MaxPerMAC))
	s.opts.InstallLog.Append(mac, body)
	s.log.Infof("audit event=install-log target_mac=%s bytes=%d from=%s", mac, len(body), r.RemoteAddr)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

// handleGetLogs returns a MAC's retained log tail as text/plain. Loopback-only.
func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	if s.opts.InstallLog == nil {
		http.Error(w, "log capture disabled", http.StatusNotFound)
		return
	}
	mac := s.extractMAC(w, r)
	if mac == "" {
		return
	}
	body := s.opts.InstallLog.Get(mac)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// parseEventPhase pulls the optional phase + reason from either a JSON
// body or a form-encoded body (cloud-init phone_home posts form data
// with no phase field — that's the empty-phase = installer-done case).
func parseEventPhase(r *http.Request) (phase, reason string) {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if strings.TrimSpace(ct) == "application/json" {
		var body struct {
			Phase  string `json:"phase"`
			Reason string `json:"reason"`
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		_ = json.Unmarshal(raw, &body)
		return body.Phase, body.Reason
	}
	// Form-encoded (cloud-init) or anything else: best-effort form parse.
	_ = r.ParseForm()
	return r.FormValue("phase"), r.FormValue("reason")
}

// handleAsset serves a file from DataDir/<target>/<file>. The target
// + file names are validated to reject path traversal (cache.AssetPath
// does the check). Used by the Ubuntu autoexec templates to fetch
// widerNetworkCIDR computes the CIDR of the network shared by
// AdvertisedIP and ClientNetmask. e.g.
//
//	("10.69.69.218", "255.255.0.0") -> "10.69.0.0/16"
//
// Used by the preseed late_command to install a link-scope route
// on the installed system so cloud-init phone_home can reach
// pxe-beacon on cross-/24 networks. Returns "" if inputs are
// unparseable or netmask is empty.
func widerNetworkCIDR(advertisedIP, netmask string) string {
	if netmask == "" {
		return ""
	}
	ip := net.ParseIP(advertisedIP)
	if ip == nil {
		return ""
	}
	ip = ip.To4()
	if ip == nil {
		return ""
	}
	mask := net.IPMask(net.ParseIP(netmask).To4())
	if len(mask) != 4 {
		return ""
	}
	network := ip.Mask(mask)
	ones, _ := mask.Size()
	return fmt.Sprintf("%s/%d", network.String(), ones)
}

// handleDebugProbe is the simpler path-based variant of the iPXE
// phone-home. Lets the dispatch script call multiple chains with
// per-key URLs that contain no `&` characters at all — robust to
// any URL-parsing quirks in iPXE's chain command.
func (s *Server) handleDebugProbe(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	value := r.PathValue("value")
	if value == "" {
		s.log.Infof("iPXE-state via HTTP from %s: %s", r.RemoteAddr, key)
	} else {
		s.log.Infof("iPXE-state via HTTP from %s: %s=%q", r.RemoteAddr, key, value)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("#!ipxe\n"))
}

// handleDebugIPXEState logs what iPXE's settings actually expand to.
// The dispatch script chains here before the iseq dispatch, passing
// mac variants + dhcp results as query params. Critical for
// diagnosing 'iseq does not match' problems on multi-NIC or
// quirky-firmware boxes. Returns an empty iPXE script so chain is
// a no-op.
func (s *Server) handleDebugIPXEState(w http.ResponseWriter, r *http.Request) {
	// Log every query param the iPXE script sent. The most useful
	// fields are mac/net0/net1 (the MAC iPXE actually evaluates) and
	// ip/gateway/dns (the network state after the script's dhcp).
	q := r.URL.Query()
	pairs := []string{}
	for _, k := range []string{"stage", "mac", "net0", "net1", "net2", "net3", "ip", "gateway", "dns", "platform", "buildarch", "version"} {
		if v := q.Get(k); v != "" {
			pairs = append(pairs, fmt.Sprintf("%s=%q", k, v))
		}
	}
	// Anything else passed gets dumped too (so future probes don't
	// need a code change).
	for k, vs := range q {
		known := false
		for _, kk := range []string{"stage", "mac", "net0", "net1", "net2", "net3", "ip", "gateway", "dns", "platform", "buildarch", "version"} {
			if k == kk {
				known = true
				break
			}
		}
		if !known && len(vs) > 0 {
			pairs = append(pairs, fmt.Sprintf("%s=%q", k, vs[0]))
		}
	}
	s.log.Infof("iPXE-state from %s: %s", r.RemoteAddr, strings.Join(pairs, " "))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("#!ipxe\n"))
}

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
//
// v0.9.0: shape unified with GET /api/v1/machines — nested
// {desired, observed} per machine — so UI consumers don't write two
// parsers. Deprecation header points at /api/v1/machines; this URL
// will be removed in v0.10.
//
// v0.9.0 also fixes the silent-corrupt-on-encode-error bug: we now
// buffer-then-flush so an encode failure becomes a 500, not a
// truncated 200.
func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}
	machines := s.opts.Fleet.ListMachines()
	out := make([]pxebeacon.Machine, 0, len(machines))
	for _, m := range machines {
		out = append(out, s.buildMachineView(m.MAC, m.Profile))
	}
	payload := pxebeacon.StatusResponse{
		Server: pxebeacon.ServerInfo{
			AdvertisedIP: s.opts.AdvertisedIP,
			HTTPPort:     s.opts.HTTPPort,
			UptimeS:      int(time.Since(s.startedAt).Seconds()),
			StartedAt:    s.startedAt.UTC().Format(time.RFC3339),
			PendingTTLs:  pendingTTLSeconds(s.opts.Pending),
		},
		Machines: out,
	}

	// Buffer first, then flush — so an encode error becomes a 500
	// (not a partial 200 with truncated JSON).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		s.log.Warnf("GET /status.json: encode error: %v", err)
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	// v0.9.0: deprecation marker (RFC 8594). Points clients at the
	// canonical /api/v1/machines resource. /status.json is scheduled
	// for removal in v0.10.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Link", `</api/v1/machines>; rel="successor-version"`)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}

// handleStatusHTML renders the same data as a plain auto-refreshing
// HTML page.
func (s *Server) handleStatusHTML(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
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
	// v0.5.11: bind IPv4-only ("tcp4") for the same reason TFTP does
	// — macOS 26.4.1's IPv6 v6only=1 default means external IPv4
	// clients (e.g. PXE clients on a different subnet) can't reach
	// an IPv6 dual-stack socket. iPXE shell test on venkat@'s box
	// confirmed: chain http://10.69.69.218:8080/... returned
	// connection-reset, meaning the SYN arrived but the kernel had
	// no IPv4 listener and sent RST.
	ln, err := net.Listen("tcp4", s.opts.Listen)
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
	var body []byte
	// v0.6.0: when a fleet config is loaded, /boot.ipxe serves the
	// per-MAC dispatch script. vanilla iPXE (no EMBED) chains to this
	// URL at iPXE-stage per our proxyDHCP OFFER, so the dispatch logic
	// must live here for that boot path to work. The legacy template
	// path (chain to ChainURL = netboot.xyz menu) is preserved for
	// the no-config single-machine case.
	if s.opts.IPXEDispatch != nil {
		body = s.opts.IPXEDispatch()
	} else {
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
		body = buf.Bytes()
	}
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
