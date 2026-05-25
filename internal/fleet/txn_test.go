package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/venkatamutyala/pxe-beacon/internal/narrlog"
)

func txnFleet(t *testing.T) *Fleet {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(`
machines:
  - mac: 58:47:ca:70:c7:c9
    name: existing
    boot: debian-12
`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(filepath.Join(dir, "fleet.yaml"), narrlog.New("test", narrlog.LevelError, nil))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCreateAndSave(t *testing.T) {
	f := txnFleet(t)

	etag, err := f.CreateAndSave(Machine{MAC: "aa:bb:cc:dd:ee:01", Profile: Profile{Name: "new", Boot: "debian-12"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if etag == "" {
		t.Error("create should return an etag")
	}
	if f.Lookup("aa:bb:cc:dd:ee:01").Name != "new" {
		t.Error("created machine not in fleet")
	}

	// Re-create same MAC → ErrMACExists.
	_, err = f.CreateAndSave(Machine{MAC: "aa:bb:cc:dd:ee:01", Profile: Profile{Name: "dup", Boot: "debian-12"}})
	if !errors.Is(err, ErrMACExists) {
		t.Fatalf("re-create: want ErrMACExists, got %v", err)
	}

	// Persisted to disk: reload and check.
	if err := f.Reload(); err != nil {
		t.Fatal(err)
	}
	if f.Lookup("aa:bb:cc:dd:ee:01").Name != "new" {
		t.Error("created machine didn't survive reload (not persisted)")
	}
}

func TestUpdateAndSave_IfMatch(t *testing.T) {
	f := txnFleet(t)
	mac := "58:47:ca:70:c7:c9"

	etag, exists := f.ETag(mac)
	if !exists {
		t.Fatal("existing machine should have an etag")
	}

	// No If-Match → 428-equivalent.
	_, err := f.UpdateAndSave(Machine{MAC: mac, Profile: Profile{Name: "x", Boot: "debian-13"}}, "")
	if !errors.Is(err, ErrPreconditionRequired) {
		t.Fatalf("update without If-Match: want ErrPreconditionRequired, got %v", err)
	}

	// Wrong If-Match → 412-equivalent.
	_, err = f.UpdateAndSave(Machine{MAC: mac, Profile: Profile{Name: "x", Boot: "debian-13"}}, `W/"deadbeef"`)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("update with stale If-Match: want ErrPreconditionFailed, got %v", err)
	}

	// Correct If-Match → success, new etag differs.
	newEtag, err := f.UpdateAndSave(Machine{MAC: mac, Profile: Profile{Name: "renamed", Boot: "debian-13"}}, etag)
	if err != nil {
		t.Fatalf("update with correct If-Match: %v", err)
	}
	if newEtag == etag {
		t.Error("etag should change after update")
	}
	if f.Lookup(mac).Name != "renamed" {
		t.Error("update didn't take")
	}

	// Update absent MAC → ErrMACAbsent.
	_, err = f.UpdateAndSave(Machine{MAC: "11:22:33:44:55:66", Profile: Profile{Name: "ghost", Boot: "debian-12"}}, `W/"x"`)
	if !errors.Is(err, ErrMACAbsent) {
		t.Fatalf("update absent: want ErrMACAbsent, got %v", err)
	}
}

func TestDeleteAndSave_Idempotent(t *testing.T) {
	f := txnFleet(t)
	mac := "58:47:ca:70:c7:c9"

	existed, err := f.DeleteAndSave(mac, "")
	if err != nil || !existed {
		t.Fatalf("first delete: existed=%v err=%v", existed, err)
	}
	// Second delete is a no-op success (idempotent).
	existed, err = f.DeleteAndSave(mac, "")
	if err != nil {
		t.Fatalf("second delete should be idempotent no-op, got err=%v", err)
	}
	if existed {
		t.Error("second delete should report existed=false")
	}
}

func TestDeleteAndSave_IfMatch(t *testing.T) {
	f := txnFleet(t)
	mac := "58:47:ca:70:c7:c9"
	etag, _ := f.ETag(mac)

	// Wrong If-Match → 412, entry preserved.
	_, err := f.DeleteAndSave(mac, `W/"stale"`)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("delete stale If-Match: want ErrPreconditionFailed, got %v", err)
	}
	if f.Lookup(mac).Name == "" {
		t.Error("entry should survive a failed-precondition delete")
	}

	// Correct If-Match → deleted.
	existed, err := f.DeleteAndSave(mac, etag)
	if err != nil || !existed {
		t.Fatalf("delete correct If-Match: existed=%v err=%v", existed, err)
	}
}

func TestParams_MergeAndRoundTrip(t *testing.T) {
	// defaults.params merge with machine params (machine wins); the
	// machine's stored params stay own-only so Save round-trips
	// without baking defaults into every entry.
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(path, []byte(`
defaults:
  boot: menu
  params:
    domain: lab.local
    dns: 10.0.0.1
machines:
  - mac: 58:47:ca:70:c7:c9
    name: kube-1
    boot: debian-12
    params:
      hostname: kube-1
      dns: 10.0.0.2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path, narrlog.New("test", narrlog.LevelError, nil))
	if err != nil {
		t.Fatal(err)
	}

	// Lookup returns the MERGED params (machine wins on dns).
	p := f.Lookup("58:47:ca:70:c7:c9")
	want := map[string]string{"domain": "lab.local", "dns": "10.0.0.2", "hostname": "kube-1"}
	for k, v := range want {
		if p.Params[k] != v {
			t.Errorf("merged params[%q] = %q, want %q", k, p.Params[k], v)
		}
	}

	// Save + reload: the merge still holds and the machine's own
	// params didn't absorb the default 'domain' (no denormalization).
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	if err := f.Reload(); err != nil {
		t.Fatal(err)
	}
	p2 := f.Lookup("58:47:ca:70:c7:c9")
	if p2.Params["domain"] != "lab.local" {
		t.Errorf("after round-trip, merged domain = %q, want lab.local", p2.Params["domain"])
	}
	if p2.Params["dns"] != "10.0.0.2" {
		t.Errorf("after round-trip, machine dns override lost: %q", p2.Params["dns"])
	}

	// Confirm the on-disk machine entry holds ONLY its own params
	// (defaults not baked in): re-read raw and assert the machine
	// block doesn't carry 'domain'.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "hostname: kube-1") {
		t.Errorf("expected machine's own param in file:\n%s", raw)
	}
}

func TestETag_PerEntryIndependence(t *testing.T) {
	// Editing machine A must not change machine B's ETag (the whole
	// point of per-entry hashing vs file mtime).
	f := txnFleet(t)
	if _, err := f.CreateAndSave(Machine{MAC: "aa:bb:cc:dd:ee:01", Profile: Profile{Name: "b", Boot: "debian-12"}}); err != nil {
		t.Fatal(err)
	}
	bEtag, _ := f.ETag("aa:bb:cc:dd:ee:01")

	aEtag, _ := f.ETag("58:47:ca:70:c7:c9")
	if _, err := f.UpdateAndSave(Machine{MAC: "58:47:ca:70:c7:c9", Profile: Profile{Name: "changed", Boot: "debian-12"}}, aEtag); err != nil {
		t.Fatal(err)
	}

	bEtag2, _ := f.ETag("aa:bb:cc:dd:ee:01")
	if bEtag != bEtag2 {
		t.Errorf("machine B's etag changed (%s → %s) after editing machine A", bEtag, bEtag2)
	}
}

func TestTxn_ConcurrentWriters(t *testing.T) {
	// Run with -race. Concurrent create/update/delete + reload must
	// not race or corrupt.
	f := txnFleet(t)
	macs := []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02", "aa:bb:cc:dd:ee:03"}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		for _, m := range macs {
			wg.Add(1)
			go func(mac string) {
				defer wg.Done()
				_, _ = f.CreateAndSave(Machine{MAC: mac, Profile: Profile{Name: "c", Boot: "debian-12"}})
				if et, ok := f.ETag(mac); ok {
					_, _ = f.UpdateAndSave(Machine{MAC: mac, Profile: Profile{Name: "u", Boot: "debian-13"}}, et)
				}
				_, _ = f.DeleteAndSave(mac, "")
			}(m)
		}
		wg.Add(1)
		go func() { defer wg.Done(); _ = f.Reload() }()
	}
	wg.Wait()
}
