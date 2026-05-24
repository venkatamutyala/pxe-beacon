package cache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kdomanski/iso9660"
)

// FetchOpts tunes how Fetch runs.
type FetchOpts struct {
	// Force redownloads + re-extracts even when the target already
	// looks populated.
	Force bool
	// Progress, if non-nil, is called periodically during the
	// download with (bytesDone, bytesTotal). bytesTotal is -1 if
	// the server doesn't send Content-Length.
	Progress func(done, total int64)
	// HTTPClient lets tests inject a stub. Defaults to http.DefaultClient
	// with a generous timeout.
	HTTPClient *http.Client
}

// Fetch downloads an ISO for `target` and extracts the AssetFiles
// into the cache's target subdir. Returns the Manifest on success.
//
// The implementation is two-pass:
//  1. Stream the ISO to a temp file on disk (ISO9660 needs random
//     access; we can't extract directly from the HTTP response).
//  2. Open the ISO via kdomanski/iso9660, walk to each AssetFile,
//     stream to the final on-disk location with atomic rename.
//  3. Delete the temp ISO (saves ~1.5 GB).
//
// Idempotent: if the target dir is already populated and Force is
// false, returns the existing Manifest without doing any work.
func (c *Cache) Fetch(ctx context.Context, targetName string, opts FetchOpts) (*Manifest, error) {
	spec, ok := Targets[targetName]
	if !ok {
		return nil, fmt.Errorf("unknown fetch target %q (known: %s)",
			targetName, strings.Join(knownTargets(), ", "))
	}

	if !opts.Force {
		if ok, m := c.IsPopulated(targetName); ok {
			return m, nil
		}
	}

	tdir, err := c.TargetDir(targetName)
	if err != nil {
		return nil, err
	}

	isoDownloadPath := filepath.Join(tdir, "_download.iso")
	if err := downloadFile(ctx, opts, spec.ISOURL, isoDownloadPath); err != nil {
		_ = os.Remove(isoDownloadPath)
		return nil, fmt.Errorf("download %s: %w", spec.ISOURL, err)
	}

	// Open the ISO and extract.
	isoFile, err := os.Open(isoDownloadPath)
	if err != nil {
		return nil, fmt.Errorf("open downloaded iso: %w", err)
	}
	img, err := iso9660.OpenImage(isoFile)
	if err != nil {
		_ = isoFile.Close()
		return nil, fmt.Errorf("parse iso9660: %w", err)
	}

	manifest := &Manifest{
		Target:    targetName,
		Source:    spec.ISOURL,
		FetchedAt: time.Now().UTC(),
		Files:     map[string]Asset{},
	}

	for _, name := range AssetFiles {
		isoInternalPath, ok := spec.ISOPath[name]
		if !ok {
			_ = isoFile.Close()
			return nil, fmt.Errorf("target %q: no ISO path for asset %q", targetName, name)
		}
		dest := filepath.Join(tdir, name)
		size, sum, err := extractFile(img, isoInternalPath, dest)
		if err != nil {
			_ = isoFile.Close()
			return nil, fmt.Errorf("extract %s from %s: %w", isoInternalPath, spec.ISOURL, err)
		}
		manifest.Files[name] = Asset{Size: size, SHA256: sum}
	}
	_ = isoFile.Close()

	// Remove the downloaded ISO to save disk (1.5 GB).
	if err := os.Remove(isoDownloadPath); err != nil {
		// Non-fatal — leave it; manifest still gets written.
		fmt.Fprintf(os.Stderr, "warning: could not remove %s: %v\n", isoDownloadPath, err)
	}

	if err := c.WriteManifest(targetName, manifest); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	return manifest, nil
}

func downloadFile(ctx context.Context, opts FetchOpts, url, dest string) error {
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 1<<20) // 1 MB
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				_ = out.Close()
				_ = os.Remove(tmp)
				return werr
			}
			done += int64(n)
			if opts.Progress != nil {
				opts.Progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return rerr
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// extractFile streams `isoPath` (an absolute path inside the image)
// out to `dest` on the host filesystem. Returns (size, sha256, err).
// Uses an atomic rename so a crash mid-extract doesn't leave a
// half-written file in place.
func extractFile(img *iso9660.Image, isoPath, dest string) (int64, string, error) {
	f, err := findISO(img, isoPath)
	if err != nil {
		return 0, "", err
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = out.Close() }()
	n, err := io.Copy(out, f.Reader())
	if err != nil {
		_ = os.Remove(tmp)
		return 0, "", err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return 0, "", err
	}
	sum, err := SHA256File(dest)
	if err != nil {
		return n, "", err
	}
	return n, sum, nil
}

// findISO walks an ISO image to the file at the given absolute path.
// Returns the *iso9660.File for reading.
func findISO(img *iso9660.Image, p string) (*iso9660.File, error) {
	root, err := img.RootDir()
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur := root
	for i, part := range parts {
		if part == "" {
			continue
		}
		kids, err := cur.GetChildren()
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", strings.Join(parts[:i], "/"), err)
		}
		var found *iso9660.File
		for _, k := range kids {
			// ISO9660 uppercases + may append ;1 — normalize.
			if normalizeISOName(k.Name()) == strings.ToLower(part) {
				found = k
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("not found in iso: %s", p)
		}
		cur = found
	}
	return cur, nil
}

func normalizeISOName(s string) string {
	if i := strings.Index(s, ";"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

func knownTargets() []string {
	out := make([]string, 0, len(Targets))
	for k := range Targets {
		out = append(out, k)
	}
	return out
}
