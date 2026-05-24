package assets

import (
	"bytes"
	"testing"
)

// TestEmbeddedIPXE_IsVanilla guards against accidentally re-embedding
// netboot.xyz's iPXE build. v0.6.0 swapped to vanilla upstream iPXE
// because netboot.xyz's `EMBED=embed.ipxe` preempts standard
// autoboot and silently breaks our per-MAC dispatch.
//
// Positive check: every embedded EFI binary contains the iPXE banner
// string "Open Source Network Boot Firmware" — fail-stops empty or
// non-iPXE binaries.
//
// Negative check: no embedded binary contains "netboot.xyz" — fails
// if someone copies netboot.xyz binaries back in.
func TestEmbeddedIPXE_IsVanilla(t *testing.T) {
	kinds := []IPXEKind{IPXEEFIx64, IPXESNPOnly, IPXEARM64}
	for _, k := range kinds {
		data, err := ReadIPXE(k)
		if err != nil {
			t.Fatalf("%s: ReadIPXE: %v", k, err)
		}
		if len(data) < 1000 {
			t.Errorf("%s: embedded binary is suspiciously small (%d bytes)", k, len(data))
		}
		if !bytes.Contains(data, []byte("Open Source Network Boot Firmware")) {
			t.Errorf("%s: missing iPXE banner string — embedded binary may not be iPXE", k)
		}
		if bytes.Contains(data, []byte("netboot.xyz")) {
			t.Errorf("%s: embedded binary contains 'netboot.xyz' string — v0.6.0+ uses vanilla upstream iPXE only", k)
		}
	}
}

// TestEmbeddedKPXE_NotNetbootXyz — legacy BIOS .kpxe is LZMA-
// compressed so readable strings don't survive. We rely on the
// negative check: a netboot.xyz build embeds metadata that survives
// compression because their embed.ipxe lives in an uncompressed
// section.
func TestEmbeddedKPXE_NotNetbootXyz(t *testing.T) {
	data, err := ReadIPXE(IPXELegacyBIOS)
	if err != nil {
		t.Fatalf("ReadIPXE: %v", err)
	}
	if len(data) < 1000 {
		t.Errorf("undionly.kpxe is suspiciously small (%d bytes)", len(data))
	}
	if bytes.Contains(data, []byte("netboot.xyz")) {
		t.Errorf("undionly.kpxe contains 'netboot.xyz' string — v0.6.0+ uses vanilla upstream iPXE only")
	}
}
