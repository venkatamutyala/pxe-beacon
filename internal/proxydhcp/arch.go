// Package proxydhcp implements the proxyDHCP responder for pxe-beacon.
//
// PLAN says: BuildOffer is a pure function — parsed request in, reply
// out, no sockets. Sockets live only in listener.go. This file owns the
// option-93 → boot-asset mapping, which is the second-most important
// invariant after the purity rule: get the arch table wrong and a
// machine boots the wrong file (or nothing).
package proxydhcp

import (
	"github.com/insomniacslk/dhcp/iana"
	"github.com/venkatamutyala/pxe-beacon/internal/assets"
)

// Transport identifies how the client expects to fetch its bootloader.
type Transport int

const (
	TransportUnknown Transport = iota
	TransportTFTP
	TransportHTTP
)

func (t Transport) String() string {
	switch t {
	case TransportTFTP:
		return "TFTP"
	case TransportHTTP:
		return "HTTP"
	}
	return "unknown"
}

// ArchProfile is what we choose to serve for a given DHCP option-93
// architecture identifier.
type ArchProfile struct {
	// Arch is the IANA arch code (RFC 4578).
	Arch iana.Arch
	// Transport is how the *firmware* fetches the bootloader.
	Transport Transport
	// IPXEKind tells the asset package which embedded binary to serve.
	IPXEKind assets.IPXEKind
	// BootFile is the path/URL the OFFER advertises in option 67.
	// For TFTP we send a filename; for HTTP we send a fully-qualified URL.
	// The leaf filename is what TFTP RRQs request.
	BootFile string
}

// archTable maps every architecture pxe-beacon recognizes to its boot
// profile. Adding an arch is a matter of inserting a row here.
//
// Source for the option-93 codes: RFC 4578 §2.1 and
// https://www.iana.org/assignments/dhcpv6-parameters/dhcpv6-parameters.xhtml#processor-architecture
var archTable = map[iana.Arch]ArchProfile{
	// Legacy BIOS PCs — TFTP only, undionly.kpxe.
	iana.INTEL_X86PC: {
		Arch:      iana.INTEL_X86PC,
		Transport: TransportTFTP,
		IPXEKind:  assets.IPXELegacyBIOS,
		BootFile:  "netboot.xyz.kpxe",
	},
	// UEFI x86_64 — TFTP, netboot.xyz EFI build. The most common
	// modern PXE arch (0x07).
	iana.EFI_X86_64: {
		Arch:      iana.EFI_X86_64,
		Transport: TransportTFTP,
		IPXEKind:  assets.IPXEEFIx64,
		BootFile:  "netboot.xyz.efi",
	},
	// UEFI ARM64 — TFTP, arm64 EFI build.
	iana.EFI_ARM64: {
		Arch:      iana.EFI_ARM64,
		Transport: TransportTFTP,
		IPXEKind:  assets.IPXEARM64,
		BootFile:  "netboot.xyz-arm64.efi",
	},
	// UEFI HTTP-boot variants — firmware fetches over HTTP directly.
	// option-93 0x10 (16) is x86_64 HTTP boot; this is what the PLAN
	// calls out as the second canonical case.
	iana.EFI_X86_64_HTTP: {
		Arch:      iana.EFI_X86_64_HTTP,
		Transport: TransportHTTP,
		IPXEKind:  assets.IPXEEFIx64,
		BootFile:  "netboot.xyz.efi",
	},
	iana.EFI_ARM64_HTTP: {
		Arch:      iana.EFI_ARM64_HTTP,
		Transport: TransportHTTP,
		IPXEKind:  assets.IPXEARM64,
		BootFile:  "netboot.xyz-arm64.efi",
	},
	// EFI IA32 (some thin clients): TFTP, but we don't have an ia32
	// build embedded; serve the EFI x86_64 SNP-only build as a best-
	// effort fallback for the rare case. Documented as best-effort.
	iana.EFI_IA32: {
		Arch:      iana.EFI_IA32,
		Transport: TransportTFTP,
		IPXEKind:  assets.IPXESNPOnly,
		BootFile:  "netboot.xyz-snponly.efi",
	},
}

// LookupArch returns the profile for arch, falling back to the most
// common modern case (EFI x86_64 over TFTP) if the firmware sent an
// option-93 we don't recognize. Returning a fallback is better than
// silently dropping the OFFER — the firmware will at least try
// something, and we log the unknown arch loudly.
func LookupArch(arch iana.Arch) (ArchProfile, bool) {
	if p, ok := archTable[arch]; ok {
		return p, true
	}
	return archTable[iana.EFI_X86_64], false
}

// KnownArchs returns the recognized arch codes, useful for diagnostics.
func KnownArchs() []iana.Arch {
	out := make([]iana.Arch, 0, len(archTable))
	for a := range archTable {
		out = append(out, a)
	}
	return out
}
