package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/braedonsaunders/ether/internal/httpclient"
)

// Identity is the resolved client identity Ether writes on the wire.
type Identity struct {
	UserAgent string
	Headers   map[string]string
	// Ordered is the full header set in profile order (User-Agent included).
	Ordered []HeaderKV
	// Params are query-parameter overrides, applied last by ShapeQuery.
	Params  []KVOverride
	Shape   QueryShape
	Profile string
}

// ClientIdentity is the owner's policy for the client Ether presents to
// Newznab/Torznab indexers. An empty UserAgent identifies as Ether.
type ClientIdentity struct {
	UserAgent string            `json:"userAgent,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"` // sealed at rest, redacted in API responses
}

// IdentityFieldError is one rejected client-identity setting, keyed like the
// config field it belongs to ("userAgent", "headers").
type IdentityFieldError struct {
	Field   string
	Message string
}

// DefaultUserAgent returns Ether's honest outbound User-Agent string.
func DefaultUserAgent() string {
	return httpclient.UserAgent()
}

// Resolve returns the wire identity for an indexer. override is the indexer's
// own UserAgent ("": inherit the global policy; an empty policy uses Ether's
// default User-Agent). Custom headers always come from the global policy.
func (p ClientIdentity) Resolve(override string) Identity {
	agent := strings.TrimSpace(override)
	if agent == "" {
		agent = strings.TrimSpace(p.UserAgent)
	}
	if agent == "" {
		agent = DefaultUserAgent()
	}
	return Identity{UserAgent: agent, Headers: p.Headers}
}

// ValidateUserAgent checks a free-text User-Agent value (global or per
// indexer), returning "" when valid or a human-readable reason when rejected.
func ValidateUserAgent(value string) string {
	return invalidHeaderValue(value, 512)
}

// Validate rejects settings the transport or an indexer would choke on.
func (p ClientIdentity) Validate() []IdentityFieldError {
	var errs []IdentityFieldError
	if msg := ValidateUserAgent(p.UserAgent); msg != "" {
		errs = append(errs, IdentityFieldError{"userAgent", msg})
	}
	if len(p.Headers) > 16 {
		return append(errs, IdentityFieldError{"headers", "at most 16 headers"})
	}
	seen := make(map[string]bool, len(p.Headers))
	for name, value := range p.Headers {
		if !headerNameRE.MatchString(name) || len(name) > 64 {
			errs = append(errs, IdentityFieldError{"headers", fmt.Sprintf("%q is not a valid header name", name)})
			continue
		}
		lower := strings.ToLower(name)
		if seen[lower] {
			errs = append(errs, IdentityFieldError{"headers", fmt.Sprintf("%q is listed twice", name)})
			continue
		}
		seen[lower] = true
		if forbiddenHeader(lower) {
			errs = append(errs, IdentityFieldError{"headers", fmt.Sprintf("%q is sent by the transport and cannot be overridden", name)})
			continue
		}
		if msg := invalidHeaderValue(value, 1024); msg != "" {
			errs = append(errs, IdentityFieldError{"headers", fmt.Sprintf("%q %s", name, msg)})
		}
	}
	return errs
}

var headerNameRE = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

// forbiddenHeader lists names the Go transport owns. accept-encoding is
// included because the transport only decompresses gzip when it added that
// header itself; a custom value would hand gzip bytes to the feed parser.
func forbiddenHeader(lower string) bool {
	switch lower {
	case "host", "content-length", "connection", "transfer-encoding", "upgrade",
		"user-agent", "accept-encoding", "proxy-authorization", "proxy-connection":
		return true
	}
	return false
}

// invalidHeaderValue returns a message when value cannot go on the wire.
func invalidHeaderValue(value string, max int) string {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "must not contain line breaks"
	}
	for i := 0; i < len(value); i++ {
		if (value[i] < 0x20 && value[i] != '\t') || value[i] == 0x7f {
			return "must contain only printable characters"
		}
	}
	if len(value) > max {
		return "is too long"
	}
	return ""
}

// legacyPresetUserAgents exists only to convert saved preset selections from
// older configs into explicit free-text userAgent values on load (no legacy
// compatibility beyond that one-time conversion).
var legacyPresetUserAgents = map[string]string{
	"sonarr":    "Sonarr/4.0.20.3014 (ubuntu 26.04)",
	"radarr":    "Radarr/6.4.4.10685 (ubuntu 26.04)",
	"prowlarr":  "Prowlarr/2.7.0.5723 (ubuntu 26.04)",
	"nzbhydra2": "NZBHydra2 9.2.0",
	"sabnzbd":   "SABnzbd/5.1.3",
	"nzbget":    "nzbget/26.3",
}

// upgradeClientIdentity converts saved pre-removal preset fields on global
// clientIdentity and per-indexer entries into free-text userAgent values and
// drops the retired fields, reporting whether doc changed.
func upgradeClientIdentity(doc map[string]any) bool {
	changed := false
	if ci, ok := doc["clientIdentity"].(map[string]any); ok {
		ua, _ := ci["userAgent"].(string)
		if strings.TrimSpace(ua) == "" {
			for _, slot := range []string{"tvSearch", "movieSearch", "tvGrab", "movieGrab"} {
				raw, _ := ci[slot].(string)
				key := strings.ToLower(strings.TrimSpace(raw))
				if key == "" || key == "ether" {
					continue
				}
				if key == "custom" {
					break
				}
				if mapped, ok := legacyPresetUserAgents[key]; ok {
					ci["userAgent"] = mapped
					changed = true
					break
				}
			}
		}
		for _, slot := range []string{"tvSearch", "movieSearch", "tvGrab", "movieGrab"} {
			if _, exists := ci[slot]; exists {
				delete(ci, slot)
				changed = true
			}
		}
	}
	if ixs, ok := doc["indexers"].([]any); ok {
		for _, item := range ixs {
			ix, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// One-time migration: a saved per-indexer user agent becomes a
			// request identity (the matching profile, or custom with that
			// exact user agent). userAgent stays as a mirror.
			if _, has := ix["requestIdentity"]; !has {
				if ua, _ := ix["userAgent"].(string); strings.TrimSpace(ua) != "" {
					if profile, ok := ProfileForUserAgent(ua); ok {
						ix["requestIdentity"] = map[string]any{"profile": profile}
					} else {
						custom := map[string]any{
							"profile": "custom",
							"headers": []any{map[string]any{"name": "User-Agent", "value": strings.TrimSpace(ua)}},
						}
						ix["requestIdentity"] = custom
					}
					changed = true
				}
			}
			raw, exists := ix["clientIdentity"]
			if !exists {
				continue
			}
			existingUA, _ := ix["userAgent"].(string)
			if strings.TrimSpace(existingUA) == "" {
				preset, _ := raw.(string)
				key := strings.ToLower(strings.TrimSpace(preset))
				if mapped, ok := legacyPresetUserAgents[key]; ok {
					ix["userAgent"] = mapped
				}
			}
			delete(ix, "clientIdentity")
			changed = true
		}
	}
	return changed
}
