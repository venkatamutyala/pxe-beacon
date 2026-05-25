package sightings

import "strings"

// ouiVendors maps a MAC OUI (first three octets, lowercase colon form) to a
// human vendor name. This is a deliberately small, hand-curated subset of
// common homelab / small-DC hardware makers — NOT the full IEEE registry.
// Unknown prefixes resolve to "" (the feed just shows MAC + arch then).
// A given vendor owns many OUIs; only a few representative ones are listed.
var ouiVendors = map[string]string{
	// Raspberry Pi Foundation / Trading
	"dc:a6:32": "Raspberry Pi",
	"e4:5f:01": "Raspberry Pi",
	"b8:27:eb": "Raspberry Pi",
	"28:cd:c1": "Raspberry Pi",
	// Dell
	"00:14:22": "Dell",
	"18:66:da": "Dell",
	"f8:bc:12": "Dell",
	"d0:67:e5": "Dell",
	"b0:83:fe": "Dell",
	// HP / HPE
	"00:25:b3": "HP",
	"3c:d9:2b": "HPE",
	"98:f2:b3": "HPE",
	"a0:b3:cc": "HPE",
	// Supermicro
	"00:25:90": "Supermicro",
	"0c:c4:7a": "Supermicro",
	"3c:ec:ef": "Supermicro",
	"ac:1f:6b": "Supermicro",
	// Lenovo
	"00:21:cc": "Lenovo",
	"e8:6a:64": "Lenovo",
	"70:5a:0f": "Lenovo",
	// Intel (NUC / onboard NICs)
	"00:1b:21": "Intel",
	"94:c6:91": "Intel",
	"a4:bf:01": "Intel",
	// Cisco / UCS
	"00:25:b5": "Cisco UCS",
	// VMware / virtual (handy when test-booting VMs)
	"00:0c:29": "VMware",
	"00:50:56": "VMware",
	"00:05:69": "VMware",
	// QEMU/KVM + VirtualBox (locally-administered defaults vary, but the
	// well-known assigned prefixes are useful for the dev loop)
	"52:54:00": "QEMU/KVM",
	"08:00:27": "VirtualBox",
}

// VendorForMAC resolves the vendor name for a MAC from its OUI, or "" if the
// prefix isn't in the curated table. Accepts colon- or hyphen-form MACs.
func VendorForMAC(mac string) string {
	m := strings.ToLower(strings.ReplaceAll(mac, "-", ":"))
	if len(m) < 8 {
		return ""
	}
	return ouiVendors[m[:8]] // "xx:xx:xx"
}
