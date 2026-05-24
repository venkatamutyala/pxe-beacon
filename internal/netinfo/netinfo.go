// Package netinfo picks an interface and derives the IPv4 address
// pxe-beacon advertises to clients. PLAN section 0 warns explicitly
// about WiFi causing TFTP timeouts, so this package surfaces a
// warning when the chosen interface looks wireless.
package netinfo

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// Picked is the result of selecting an interface.
type Picked struct {
	Iface       *net.Interface
	AdvertiseIP net.IP
	IsWireless  bool
}

// Pick chooses an interface. If name is non-empty, it must exist and
// have an IPv4 address. If empty, the first non-loopback, up interface
// with an IPv4 address wins. The PLAN doesn't require any sophisticated
// scoring beyond that — operators can always pass -interface.
func Pick(name string) (*Picked, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}

	if name != "" {
		for i := range ifaces {
			if ifaces[i].Name == name {
				ip, ok := firstIPv4(&ifaces[i])
				if !ok {
					return nil, fmt.Errorf("interface %q has no IPv4 address", name)
				}
				return &Picked{
					Iface:       &ifaces[i],
					AdvertiseIP: ip,
					IsWireless:  looksWireless(ifaces[i].Name),
				}, nil
			}
		}
		return nil, fmt.Errorf("interface %q not found", name)
	}

	for i := range ifaces {
		if ifaces[i].Flags&net.FlagLoopback != 0 {
			continue
		}
		if ifaces[i].Flags&net.FlagUp == 0 {
			continue
		}
		ip, ok := firstIPv4(&ifaces[i])
		if !ok {
			continue
		}
		return &Picked{
			Iface:       &ifaces[i],
			AdvertiseIP: ip,
			IsWireless:  looksWireless(ifaces[i].Name),
		}, nil
	}
	return nil, errors.New("no suitable interface found (pass -interface explicitly)")
}

func firstIPv4(iface *net.Interface) (net.IP, bool) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, false
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil && !v4.IsLoopback() {
			return v4, true
		}
	}
	return nil, false
}

// looksWireless is a heuristic — naming alone isn't authoritative but
// PLAN section 0 says we should err on the side of warning rather
// than letting a WiFi-induced TFTP timeout go unexplained.
func looksWireless(name string) bool {
	lname := strings.ToLower(name)
	// Linux: wl*, wlan*, wlp*, wlx*. BSD: ath*, ral*. macOS WiFi is
	// typically named en0 on MacBooks but en0 is wired on other Macs;
	// don't false-positive — the operator can read the banner and
	// decide.
	prefixes := []string{"wl", "wlan", "wlp", "wlx", "ath", "ra"}
	for _, p := range prefixes {
		if strings.HasPrefix(lname, p) {
			return true
		}
	}
	return false
}
