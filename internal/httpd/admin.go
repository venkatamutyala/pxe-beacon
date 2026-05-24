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

// adminToken is the per-process CSRF token. Generated once at server
// startup and validated on every POST to /admin/*. Same-origin
// browsers only — non-loopback requests are rejected by the
// loopback middleware before they ever reach the CSRF check.
type adminState struct {
	csrf      string
	indexTmpl *htmltmpl.Template
	editTmpl  *htmltmpl.Template
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
	return &adminState{
		csrf:      base64.RawURLEncoding.EncodeToString(b),
		indexTmpl: idx,
		editTmpl:  edit,
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
	if !s.fleetReady(w) {
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
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.admin.indexTmpl.Execute(w, vm); err != nil {
		s.log.Warnf("admin index render: %v", err)
	}
}

// handleAdminFleetSave handles POST /admin/fleet — add or update a
// machine entry. Writes fleet.yaml on success.
func (s *Server) handleAdminFleetSave(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w) {
		return
	}
	mac := strings.TrimSpace(r.FormValue("mac"))
	name := strings.TrimSpace(r.FormValue("name"))
	boot := strings.TrimSpace(r.FormValue("boot"))
	preseed := strings.TrimSpace(r.FormValue("preseed"))
	kickstart := strings.TrimSpace(r.FormValue("kickstart"))
	cloudInit := strings.TrimSpace(r.FormValue("cloud_init"))
	ipxeScript := strings.TrimSpace(r.FormValue("ipxe_script"))

	canon, err := fleet.CanonicalMAC(mac)
	if err != nil {
		redirectFlash(w, r, "err", fmt.Sprintf("invalid MAC: %v", err))
		return
	}
	if !fleet.ValidBootTargets[boot] {
		redirectFlash(w, r, "err", fmt.Sprintf("unknown boot target %q", boot))
		return
	}
	resolvePath := func(p string) string {
		if p == "" {
			return ""
		}
		if filepath.IsAbs(p) {
			return p
		}
		// Resolve relative to fleet.yaml directory.
		return filepath.Clean(filepath.Join(s.opts.Fleet.BaseDir(), p))
	}
	m := fleet.Machine{
		MAC: canon,
		Profile: fleet.Profile{
			Name:       name,
			Boot:       boot,
			Preseed:    resolvePath(preseed),
			Kickstart:  resolvePath(kickstart),
			CloudInit:  resolvePath(cloudInit),
			IPXEScript: resolvePath(ipxeScript),
		},
	}
	if err := s.opts.Fleet.AddOrUpdate(m); err != nil {
		redirectFlash(w, r, "err", fmt.Sprintf("validation failed: %v", err))
		return
	}
	if err := s.opts.Fleet.Save(); err != nil {
		redirectFlash(w, r, "err", fmt.Sprintf("save fleet.yaml: %v", err))
		return
	}
	s.log.Infof("admin: saved machine %s (%s, boot=%s)", name, canon, boot)
	redirectFlash(w, r, "ok", fmt.Sprintf("saved %s (%s)", name, canon))
}

// handleAdminFleetDelete handles POST /admin/fleet/delete.
func (s *Server) handleAdminFleetDelete(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w) {
		return
	}
	mac := strings.TrimSpace(r.FormValue("mac"))
	removed, err := s.opts.Fleet.Remove(mac)
	if err != nil {
		redirectFlash(w, r, "err", fmt.Sprintf("delete: %v", err))
		return
	}
	if !removed {
		redirectFlash(w, r, "err", fmt.Sprintf("not found: %s", mac))
		return
	}
	if err := s.opts.Fleet.Save(); err != nil {
		redirectFlash(w, r, "err", fmt.Sprintf("save fleet.yaml: %v", err))
		return
	}
	s.log.Infof("admin: deleted machine %s", mac)
	redirectFlash(w, r, "ok", fmt.Sprintf("deleted %s", mac))
}

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

// handleAdminReload handles POST /admin/reload — in-process equivalent
// of SIGHUP. Re-reads fleet.yaml from disk.
func (s *Server) handleAdminReload(w http.ResponseWriter, r *http.Request) {
	if !s.fleetReady(w) {
		return
	}
	if err := s.opts.Fleet.Reload(); err != nil {
		redirectFlash(w, r, "err", fmt.Sprintf("reload failed: %v", err))
		return
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
