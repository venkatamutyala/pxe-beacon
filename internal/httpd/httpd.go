// Package httpd serves the iPXE binary (for UEFI HTTP-boot) and the
// templated boot.ipxe chain script. PLAN section 0 warns specifically
// that UEFI HTTP boot is picky about Content-Length, so we always set
// it explicitly and use a fixed-content ReadSeeker, never chunked.
package httpd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/venkatamutyala/pxe-beacon/internal/assets"
	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

// Tracker is the same interface the TFTP server uses to notify the
// proxyDHCP listener that a client has progressed.
type Tracker interface {
	NoteServed(mac string)
}

// Options carries deployment config.
type Options struct {
	Listen          string // ":8080" or "127.0.0.1:8080"
	AdvertisedIP    string // for templating into boot.ipxe
	ChainURL        string // netboot.xyz menu URL by default
	IPXEScriptPath  string // URL path the script is served at
	IPXEScriptFile  string // optional path to override-on-disk template
	SetCrossCert    bool   // true to add `set crosscert` (PLAN gotcha)
	Logger          *narrlog.Logger
	Tracker         Tracker
}

// Server is the pxe-beacon HTTP server.
type Server struct {
	opts   Options
	log    *narrlog.Logger
	mux    *http.ServeMux
	tmpl   *template.Template
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

	s := &Server{
		opts: o,
		log:  o.Logger.With("http"),
		mux:  http.NewServeMux(),
		tmpl: tmpl,
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

// routes registers handlers. Two kinds:
//   - GET /<ipxe-binary> : the iPXE binaries, for UEFI HTTP boot.
//   - GET /boot.ipxe     : the chain script.
// A root handler also returns a tiny status page so curl localhost
// works as a healthcheck and the operator gets a friendly hint.
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
