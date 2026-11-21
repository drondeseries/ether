// Package clientnet classifies authenticated frontend peer and proxy addresses.
package clientnet

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

var overlay = netip.MustParsePrefix("100.64.0.0/10")

// LAN reports whether an address belongs to a LAN or overlay network.
func LAN(ip netip.Addr) bool {
	return ip.IsValid() && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || overlay.Contains(ip.Unmap()))
}

func peerAddr(r *http.Request) (netip.Addr, bool) {
	if r == nil {
		return netip.Addr{}, false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return peer.Unmap(), true
}

func isTrustedAddr(ip netip.Addr, trusted []netip.Prefix) bool {
	if !ip.IsValid() {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, p := range trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// TrustedPeer reports whether r's direct connecting peer is loopback or within
// one of the explicitly trusted proxy prefixes.
func TrustedPeer(r *http.Request, trusted ...netip.Prefix) bool {
	peer, ok := peerAddr(r)
	return ok && isTrustedAddr(peer, trusted)
}

// ClientNet classifies the peer and returns its client address. Forwarding
// headers are trusted only from loopback or explicitly configured proxies.
// Malformed chains fail closed. A public hop cannot be hidden by a preceding
// client-supplied private address.
func ClientNet(r *http.Request, trusted ...netip.Prefix) (netip.Addr, bool) {
	peer, ok := peerAddr(r)
	if !ok {
		return netip.Addr{}, false
	}
	if !isTrustedAddr(peer, trusted) {
		return peer, LAN(peer)
	}
	var hops []string
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		hops = strings.Split(strings.Join(values, ","), ",")
	} else if values := r.Header.Values("Forwarded"); len(values) > 0 {
		for _, element := range strings.Split(strings.Join(values, ","), ",") {
			found := false
			for _, param := range strings.Split(element, ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok && strings.EqualFold(k, "for") {
					if found {
						return netip.Addr{}, false
					}
					found = true
					v = strings.Trim(strings.TrimSpace(v), "\"")
					if h, _, e := net.SplitHostPort(v); e == nil {
						v = h
					}
					hops = append(hops, strings.Trim(v, "[]"))
				}
			}
			if !found {
				return netip.Addr{}, false
			}
		}
	} else if v := r.Header.Get("X-Real-IP"); v != "" {
		hops = []string{v}
	}
	if len(hops) == 0 {
		return peer, LAN(peer)
	}
	var nearest, lastTrusted netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		ip, e := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if e != nil {
			return netip.Addr{}, false
		}
		ip = ip.Unmap()
		if isTrustedAddr(ip, trusted) {
			lastTrusted = ip
			continue
		}
		if !LAN(ip) {
			return ip, false
		}
		if !nearest.IsValid() {
			nearest = ip
		}
	}
	if nearest.IsValid() {
		return nearest, LAN(nearest)
	}
	return lastTrusted, LAN(lastTrusted)
}

// ParseProxyEntry parses one trusted proxy IP address (treated as /32 or /128)
// or CIDR prefix. Wildcard ranges (0.0.0.0/0, ::/0) are rejected so any
// internet peer cannot spoof forwarding headers.
func ParseProxyEntry(raw string) (netip.Prefix, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return netip.Prefix{}, errors.New("enter an IP address or CIDR range (for example 192.168.1.5 or 10.0.0.0/8)")
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		p = p.Masked()
		if p.Bits() == 0 {
			return netip.Prefix{}, errors.New("cannot trust every address; enter a specific proxy IP or subnet")
		}
		return p, nil
	}
	if addr, err := netip.ParseAddr(s); err == nil && addr.Zone() == "" {
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	return netip.Prefix{}, errors.New("enter an IP address or CIDR range (for example 192.168.1.5 or 10.0.0.0/8)")
}

// ProxyPrefixes parses configured proxy IPs and networks. Invalid entries grant no trust.
func ProxyPrefixes(cidrs []string) []netip.Prefix {
	var out []netip.Prefix
	for _, raw := range cidrs {
		if p, err := ParseProxyEntry(raw); err == nil {
			out = append(out, p)
		}
	}
	return out
}
