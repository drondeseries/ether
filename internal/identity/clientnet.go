package identity

import (
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"net/http"
	"net/netip"
)

// LAN reports whether an address belongs to a LAN or overlay network.
func LAN(ip netip.Addr) bool { return clientnet.LAN(ip) }

// ClientNet applies the shared frontend peer/proxy trust grammar.
func ClientNet(r *http.Request, trusted ...netip.Prefix) (netip.Addr, bool) {
	return clientnet.ClientNet(r, trusted...)
}

// TrustedPeer reports whether r's connecting peer is loopback or a trusted proxy.
func TrustedPeer(r *http.Request, trusted ...netip.Prefix) bool {
	return clientnet.TrustedPeer(r, trusted...)
}

// ParseProxyEntry parses one trusted proxy IP address or CIDR prefix.
func ParseProxyEntry(raw string) (netip.Prefix, error) { return clientnet.ParseProxyEntry(raw) }

// ProxyPrefixes parses configured peer IPs and networks; invalid entries grant no trust.
func ProxyPrefixes(cidrs []string) []netip.Prefix { return clientnet.ProxyPrefixes(cidrs) }
