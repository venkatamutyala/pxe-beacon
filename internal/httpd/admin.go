package httpd

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	htmltmpl "html/template"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/venkatamutyala/pxe-beacon/internal/assets"
	"github.com/venkatamutyala/pxe-beacon/internal/fleet"
)

//go:embed admin.html
var adminHTMLSrc string

//go:embed admin_template.html
var adminTemplateHTMLSrc string

//go:embed admin_cloudinit.html
var adminCloudInitHTMLSrc string

// adminToken is the per-process CSRF token. Generated once at server
// startup and validated on every POST to /admin/*. Same-origin
// browsers only — non-loopback requests are rejected by the
// loopback middleware before they ever reach the CSRF check.
type adminState struct {
	csrf          string
	indexTmpl     *htmltmpl.Template
	editTmpl      *htmltmpl.Template
	cloudInitTmpl *htmltmpl.Template
}

func newAdminState() (*adminState, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	idx, err := htmltmpl.New("admin").Parse(adminHTMLSrc)
	if err != nil {
		return nil, fmt.Errorf("parse admin.html: %w", err)
	}
	edit, err := htmltmpl.New("admin-template").Parse(adminTemplateHTMLSrc)
	if err != nil {
		return nil, fmt.Errorf("parse admin_template.html: %w", err)
	}
	ci, err := htmltmpl.New("admin-cloudinit").Parse(adminCloudInitHTMLSrc)
	if err != nil {
		return nil, fmt.Errorf("parse admin_cloudinit.html: %w", err)
	}
	return &adminState{
		csrf:          base64.RawURLEncoding.EncodeToString(b),
		indexTmpl:     idx,
		editTmpl:      edit,
		cloudInitTmpl: ci,
	}, nil
}

// loopbackOnly is the middleware that gates every /admin/* route.
// Refuses non-loopback origins with 403. Per the PM review we do
// NOT ship a flag to disable this — operators who want remote access
// should put a reverse proxy with auth in front.
func loopbackOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			http.Error(w, "admin is loopback-only; use SSH tunnel for remote access", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// csrfGuard wraps a handler so POSTs without the matching token are
// rejected. Token comes from the page (form field `csrf`). Idempotent
// for GET requests.
func (s *Server) csrfGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
				return
			}
			if r.FormValue("csrf") != s.admin.csrf {
				http.Error(w, "csrf token mismatch", http.StatusForbidden)
				return
			}
		}
		h(w, r)
	}
}

// handleAdminIndex renders /admin.
func (s *Server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}
	type viewModel struct {
		AdvertisedIP string
		HTTPPort     int
		DataDir      string
		Machines     []fleet.Machine
		Templates    []string
		CSRF         string
		Flash        string
		FlashKind    string
		PendingTTL   string
	}
	pendingTTL := "no expiry"
	if s.opts.Pending != nil && s.opts.Pending.TTL() > 0 {
		pendingTTL = s.opts.Pending.TTL().String()
	}
	vm := viewModel{
		AdvertisedIP: s.opts.AdvertisedIP,
		HTTPPort:     s.opts.HTTPPort,
		DataDir:      s.opts.DataDir,
		Machines:     s.opts.Fleet.ListMachines(),
		Templates:    assets.ListEditableTemplates(),
		CSRF:         s.admin.csrf,
		Flash:        r.URL.Query().Get("flash"),
		FlashKind:    r.URL.Query().Get("kind"),
		PendingTTL:   pendingTTL,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.admin.indexTmpl.Execute(w, vm); err != nil {
		s.log.Warnf("admin index render: %v", err)
	}
}

// v0.9.0: fleet CRUD (add/update/delete) moved to the JSON API —
// POST/PUT/DELETE /api/v1/machines in api.go. The /admin page is now a
// fetch() client of those endpoints. The old form-encoded
// handleAdminFleetSave / handleAdminFleetDelete handlers were removed;
// the transactional Fleet.CreateAndSave / UpdateAndSave / DeleteAndSave
// methods (txn.go) are the single mutation path, with rollback on Save
// failure and If-Match optimistic concurrency.

// handleAdminTemplateView handles GET /admin/templates/{name}.
// Name in the URL is the relative path under scripts/ — e.g.
// "defaults/debian-preseed.cfg" or "autoexec/menu.ipxe". To keep
// URLs flat we accept names with `-` separating dirs from files.
func (s *Server) handleAdminTemplateView(w http.ResponseWriter, r *http.Request) {
	rel := normalizeTemplateName(r.PathValue("name"))
	if rel == "" {
		http.Error(w, "missing template name", http.StatusBadRequest)
		return
	}
	if !isEditableTemplate(rel) {
		http.Error(w, fmt.Sprintf("not an editable template: %s", rel), http.StatusBadRequest)
		return
	}

	embeddedBytes, err := assets.ReadEmbedded(rel)
	if err != nil {
		http.Error(w, fmt.Sprintf("read embedded: %v", err), http.StatusInternalServerError)
		return
	}

	overridePath := ""
	hasOverride := false
	var content []byte
	if d := assets.OverrideDir(); d != "" {
		overridePath = filepath.Join(d, "templates", rel)
		if b, err := os.ReadFile(overridePath); err == nil {
			content = b
			hasOverride = true
		}
	}
	if !hasOverride {
		content = embeddedBytes
	}

	type viewModel struct {
		Name            string
		HasOverride     bool
		OverridePath    string
		Content         string
		EmbeddedContent string
		CSRF            string
		Flash           string
		FlashKind       string
	}
	vm := viewModel{
		Name:            rel,
		HasOverride:     hasOverride,
		OverridePath:    overridePath,
		Content:         string(content),
		EmbeddedContent: string(embeddedBytes),
		CSRF:            s.admin.csrf,
		Flash:           r.URL.Query().Get("flash"),
		FlashKind:       r.URL.Query().Get("kind"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.admin.editTmpl.Execute(w, vm); err != nil {
		s.log.Warnf("admin template render: %v", err)
	}
}

// handleAdminTemplateSave handles POST /admin/templates/{name}.
func (s *Server) handleAdminTemplateSave(w http.ResponseWriter, r *http.Request) {
	rel := normalizeTemplateName(r.PathValue("name"))
	if rel == "" || !isEditableTemplate(rel) {
		http.Error(w, "invalid template name", http.StatusBadRequest)
		return
	}
	d := assets.OverrideDir()
	if d == "" {
		http.Error(w, "no data-dir configured (start pxe-beacon with -data-dir to enable overrides)", http.StatusBadRequest)
		return
	}
	content := r.FormValue("content")
	overridePath := filepath.Join(d, "templates", rel)
	if err := os.MkdirAll(filepath.Dir(overridePath), 0o755); err != nil {
		http.Error(w, fmt.Sprintf("mkdir: %v", err), http.StatusInternalServerError)
		return
	}
	tmp := overridePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		http.Error(w, fmt.Sprintf("write: %v", err), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, overridePath); err != nil {
		http.Error(w, fmt.Sprintf("rename: %v", err), http.StatusInternalServerError)
		return
	}
	s.log.Infof("admin: saved override for %s (%d bytes) at %s", rel, len(content), overridePath)
	http.Redirect(w, r, "/admin/templates/"+r.PathValue("name")+"?flash=saved&kind=ok", http.StatusSeeOther)
}

// handleAdminTemplateReset handles POST /admin/templates/{name}/reset.
func (s *Server) handleAdminTemplateReset(w http.ResponseWriter, r *http.Request) {
	rel := normalizeTemplateName(r.PathValue("name"))
	if rel == "" || !isEditableTemplate(rel) {
		http.Error(w, "invalid template name", http.StatusBadRequest)
		return
	}
	d := assets.OverrideDir()
	if d == "" {
		http.Error(w, "no data-dir configured", http.StatusBadRequest)
		return
	}
	overridePath := filepath.Join(d, "templates", rel)
	if err := os.Remove(overridePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, fmt.Sprintf("remove: %v", err), http.StatusInternalServerError)
		return
	}
	s.log.Infof("admin: reset override for %s (removed %s)", rel, overridePath)
	http.Redirect(w, r, "/admin/templates/"+rel+"?flash=reset&kind=ok", http.StatusSeeOther)
}

// handleAdminCloudInitView handles GET /admin/machines/{mac}/cloud-init —
// the per-machine cloud-init content editor (v0.14.0). Loads the on-disk
// override if present, else seeds the textarea from the machine's
// fleet.yaml cloud_init: path (if set + readable), else the embedded
// default — so "edit" always starts from what would actually be served.
func (s *Server) handleAdminCloudInitView(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}
	mac, err := fleet.CanonicalMAC(r.PathValue("mac"))
	if err != nil {
		http.Error(w, "invalid MAC", http.StatusBadRequest)
		return
	}
	if s.opts.DataDir == "" {
		http.Error(w, "no data-dir configured (start pxe-beacon with -data-dir to enable per-machine overrides)", http.StatusBadRequest)
		return
	}
	p := s.opts.Fleet.Lookup(mac)
	ovPath := s.machineCloudInitOverridePath(mac)

	var content, source string
	hasOverride := false
	if b, ok := s.machineCloudInitOverride(mac); ok {
		content, hasOverride = string(b), true
	} else if p.CloudInit != "" {
		if b, rerr := os.ReadFile(p.CloudInit); rerr == nil {
			content = string(b)
			source = "starting from fleet cloud_init: " + p.CloudInit
		}
	}
	if content == "" && !hasOverride {
		if b, derr := assets.ReadDefault("cloud-init.yaml"); derr == nil {
			content = string(b)
			source = "starting from embedded default"
		}
	}

	name := p.Name
	if name == "" {
		name = mac
	}
	vm := struct {
		Name, MAC, MACHyp              string
		HasOverride                    bool
		OverridePath, Content          string
		Source, CSRF, Flash, FlashKind string
	}{
		Name: name, MAC: mac, MACHyp: strings.ReplaceAll(mac, ":", "-"),
		HasOverride: hasOverride, OverridePath: ovPath, Content: content,
		Source: source, CSRF: s.admin.csrf,
		Flash: r.URL.Query().Get("flash"), FlashKind: r.URL.Query().Get("kind"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.admin.cloudInitTmpl.Execute(w, vm); err != nil {
		s.log.Warnf("admin cloud-init render: %v", err)
	}
}

// handleAdminCloudInitSave handles POST /admin/machines/{mac}/cloud-init.
// Validates that the content doesn't define its own phone_home (pxe-beacon
// owns it), then writes the override file atomically.
func (s *Server) handleAdminCloudInitSave(w http.ResponseWriter, r *http.Request) {
	mac, err := fleet.CanonicalMAC(r.PathValue("mac"))
	if err != nil {
		http.Error(w, "invalid MAC", http.StatusBadRequest)
		return
	}
	machyp := strings.ReplaceAll(mac, ":", "-")
	ovPath := s.machineCloudInitOverridePath(mac)
	if ovPath == "" {
		http.Error(w, "no data-dir configured", http.StatusBadRequest)
		return
	}
	content := r.FormValue("content")
	if fleet.DefinesPhoneHome([]byte(content)) {
		s.cloudInitFlash(w, r, machyp, "err", "remove phone_home — pxe-beacon appends its own tokenized phone_home (it owns the callback)")
		return
	}
	if err := os.MkdirAll(filepath.Dir(ovPath), 0o755); err != nil {
		http.Error(w, fmt.Sprintf("mkdir: %v", err), http.StatusInternalServerError)
		return
	}
	tmp := ovPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		http.Error(w, fmt.Sprintf("write: %v", err), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, ovPath); err != nil {
		http.Error(w, fmt.Sprintf("rename: %v", err), http.StatusInternalServerError)
		return
	}
	s.log.Infof("admin: saved cloud-init override for %s (%d bytes) at %s", mac, len(content), ovPath)
	s.cloudInitFlash(w, r, machyp, "ok", "saved")
}

// handleAdminCloudInitReset handles POST /admin/machines/{mac}/cloud-init-reset
// — deletes the override so the machine reverts to its fleet.yaml path (or
// the embedded default).
func (s *Server) handleAdminCloudInitReset(w http.ResponseWriter, r *http.Request) {
	mac, err := fleet.CanonicalMAC(r.PathValue("mac"))
	if err != nil {
		http.Error(w, "invalid MAC", http.StatusBadRequest)
		return
	}
	machyp := strings.ReplaceAll(mac, ":", "-")
	ovPath := s.machineCloudInitOverridePath(mac)
	if ovPath == "" {
		http.Error(w, "no data-dir configured", http.StatusBadRequest)
		return
	}
	if err := os.Remove(ovPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, fmt.Sprintf("remove: %v", err), http.StatusInternalServerError)
		return
	}
	s.log.Infof("admin: deleted cloud-init override for %s", mac)
	s.cloudInitFlash(w, r, machyp, "ok", "override deleted")
}

func (s *Server) cloudInitFlash(w http.ResponseWriter, r *http.Request, machyp, kind, msg string) {
	http.Redirect(w, r, "/admin/machines/"+machyp+"/cloud-init?flash="+escape(msg)+"&kind="+kind, http.StatusSeeOther)
}

// handleAdminReload handles POST /admin/reload — in-process equivalent
// of SIGHUP. Re-reads fleet.yaml from disk.
func (s *Server) handleAdminReload(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w, r) {
		return
	}
	if err := s.opts.Fleet.Reload(); err != nil {
		redirectFlash(w, r, "err", fmt.Sprintf("reload failed: %v", err))
		return
	}
	// v0.8.1: drop pending entries for MACs removed from fleet.yaml.
	if s.opts.Pending != nil {
		machines := s.opts.Fleet.Machines()
		removed, dropped := s.opts.Pending.RetainOnly(func(mac string) bool {
			_, ok := machines[mac]
			return ok
		})
		if removed > 0 {
			s.log.Infof("admin reload: dropped %d pending intent(s) for removed MAC(s): %v", removed, dropped)
		}
	}
	s.log.Infof("admin: fleet reloaded")
	redirectFlash(w, r, "ok", "fleet reloaded from disk")
}

// ----- helpers -----

// normalizeTemplateName cleans the URL wildcard match. Path-traversal
// protection via the isEditableTemplate allowlist.
func normalizeTemplateName(s string) string {
	return strings.TrimSpace(s)
}

func isEditableTemplate(rel string) bool {
	for _, ok := range assets.ListEditableTemplates() {
		if ok == rel {
			return true
		}
	}
	return false
}

func redirectFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	http.Redirect(w, r, "/admin?flash="+escape(msg)+"&kind="+kind, http.StatusSeeOther)
}

func escape(s string) string {
	// URL-friendly escape for the flash querystring. Limited subset
	// — anything we send is from our own server-side strings, no
	// user-controlled content needing strict escaping.
	return hex.EncodeToString([]byte(s))[:min2(len(s)*2, 200)]
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
