package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
)

// Per-indexer request identity: how Ether's requests to one indexer look on
// the wire. A profile supplies an ordered header set and a query shape;
// owner overrides are applied on top (headers) and last (parameters).

// HeaderKV is one header in an ordered set.
type HeaderKV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// KVOverride adds, changes or removes one header or query parameter.
type KVOverride struct {
	Name   string `json:"name"`
	Value  string `json:"value,omitempty"`
	Remove bool   `json:"remove,omitempty"`
}

// IndexerRequestIdentity is stored with the indexer config. An empty
// Profile inherits the shared client-identity policy (Ether by default).
type IndexerRequestIdentity struct {
	Profile string       `json:"profile,omitempty"`
	Headers []KVOverride `json:"headers,omitempty"`
	Params  []KVOverride `json:"params,omitempty"`
	// GrabProfile is empty for the profile's paired downloader.
	GrabProfile string       `json:"grabProfile,omitempty"`
	GrabHeaders []KVOverride `json:"grabHeaders,omitempty"`
	GrabParams  []KVOverride `json:"grabParams,omitempty"`
}

// QueryShape describes how a client writes its query string.
type QueryShape struct {
	ParamOrder      []string
	FirstPageOffset bool // send offset=0 when no offset is present
	PadSeasonZero   bool // season 0 is sent as "00"
	YearInQuery     bool // no year param; the year goes in q
}

// ClientProfile is one selectable request identity.
type ClientProfile struct {
	ID         string
	Headers    []HeaderKV
	Shape      QueryShape
	GrabPaired string // default grab profile; empty keeps this profile
}

// clientVersions is the one table of imitated client version strings.
// Bump a client here and every profile using it follows.
var clientVersions = map[string]string{
	"prowlarr":  "Prowlarr/2.7.0.5723 (alpine 3.24.4)",
	"sonarr":    "Sonarr/4.0.20.3014 (alpine 3.24.4)",
	"radarr":    "Radarr/7.0.3.10918 (alpine 3.24.4)",
	"nzbhydra2": "NZBHydra2 9.3.1",
	"sabnzbd":   "SABnzbd/5.1.3",
	"nzbget":    "nzbget/26.3",
	"browser":   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/158.0.0.0 Safari/537.36",
}

// ProfileUserAgent returns a profile's user agent ("" if unknown).
func ProfileUserAgent(id string) string {
	if id == "ether" {
		return DefaultUserAgent()
	}
	return clientVersions[strings.ToLower(strings.TrimSpace(id))]
}

const (
	arrAccept   = "application/rss+xml, text/rss+xml, application/xml, text/xml"
	traceparent = "{traceparent}" // replaced per request
)

var arrShape = QueryShape{
	ParamOrder: []string{"t", "q", "cat", "extended", "apikey", "offset", "limit", "imdbid", "tmdbid", "tvdbid", "season", "ep", "rid", "genre", "artist", "album", "author", "title"},
}

var prowlarrShape = QueryShape{
	ParamOrder:      []string{"t", "cat", "extended", "apikey", "offset", "limit", "q", "imdbid", "tmdbid", "tvdbid", "season", "ep"},
	FirstPageOffset: true,
	PadSeasonZero:   true,
	YearInQuery:     true,
}

// ClientProfiles lists every profile the owner can pick, in picker order.
func ClientProfiles() []ClientProfile {
	arr := func(id string) ClientProfile {
		return ClientProfile{ID: id, GrabPaired: "sabnzbd", Shape: arrShape, Headers: []HeaderKV{
			{"User-Agent", clientVersions[id]},
			{"Accept", arrAccept},
			{"Accept-Encoding", "gzip, br"},
			{"traceparent", traceparent},
		}}
	}
	prowlarr := arr("prowlarr")
	prowlarr.Shape = prowlarrShape
	return []ClientProfile{
		{ID: "ether", Headers: []HeaderKV{{"User-Agent", DefaultUserAgent()}}},
		prowlarr, arr("sonarr"), arr("radarr"),
		{ID: "nzbhydra2", GrabPaired: "sabnzbd", Headers: []HeaderKV{
			{"Accept", "application/xml"},
			{"User-Agent", clientVersions["nzbhydra2"]},
			{"Content-Type", "application/xml"},
			{"Accept-Encoding", "gzip"},
		}},
		{ID: "sabnzbd", Headers: []HeaderKV{
			{"User-Agent", clientVersions["sabnzbd"]},
			{"Accept-Encoding", "gzip"},
			{"Connection", "close"},
		}},
		{ID: "nzbget", Headers: []HeaderKV{
			{"User-Agent", clientVersions["nzbget"]},
			{"Accept", "*/*"},
			{"Accept-Encoding", "gzip"},
			{"Connection", "close"},
		}},
		{ID: "browser", Headers: []HeaderKV{
			{"sec-ch-ua", `"Chromium";v="158", "Not.A/Brand";v="24", "Google Chrome";v="158"`},
			{"sec-ch-ua-mobile", "?0"},
			{"sec-ch-ua-platform", `"macOS"`},
			{"Accept-Language", "en-US,en;q=0.9"},
			{"Upgrade-Insecure-Requests", "1"},
			{"User-Agent", clientVersions["browser"]},
			{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
			{"Sec-Fetch-Site", "none"},
			{"Sec-Fetch-Mode", "navigate"},
			{"Sec-Fetch-User", "?1"},
			{"Sec-Fetch-Dest", "document"},
			{"Accept-Encoding", "gzip, deflate, br, zstd"},
		}},
		{ID: "custom", Headers: []HeaderKV{{"User-Agent", DefaultUserAgent()}}},
	}
}

// LookupProfile returns the named profile.
func LookupProfile(id string) (ClientProfile, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range ClientProfiles() {
		if p.ID == id {
			return p, true
		}
	}
	return ClientProfile{}, false
}

// PairedGrabProfile is the grab profile a search profile defaults to.
func PairedGrabProfile(id string) string {
	if p, ok := LookupProfile(id); ok && p.GrabPaired != "" {
		return p.GrabPaired
	}
	return id
}

// NewTraceparent returns a W3C traceparent value with random ids.
func NewTraceparent() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return "00-" + hex.EncodeToString(b[:16]) + "-" + hex.EncodeToString(b[16:]) + "-01"
}

func applyHeaderOverrides(base []HeaderKV, ovs []KVOverride) []HeaderKV {
	out := append([]HeaderKV(nil), base...)
	for _, ov := range ovs {
		name := strings.TrimSpace(ov.Name)
		if name == "" {
			continue
		}
		idx := -1
		for i, h := range out {
			if strings.EqualFold(h.Name, name) {
				idx = i
				break
			}
		}
		switch {
		case ov.Remove:
			if idx >= 0 {
				out = append(out[:idx], out[idx+1:]...)
			}
		case idx >= 0:
			out[idx] = HeaderKV{textproto.CanonicalMIMEHeaderKey(name), ov.Value}
		default:
			out = append(out, HeaderKV{textproto.CanonicalMIMEHeaderKey(name), ov.Value})
		}
	}
	return out
}

func identityFromOrdered(ordered []HeaderKV, params []KVOverride, shape QueryShape, profile string) Identity {
	id := Identity{Ordered: ordered, Params: params, Shape: shape, Profile: profile, Headers: map[string]string{}}
	for _, h := range ordered {
		if strings.EqualFold(h.Name, "User-Agent") {
			id.UserAgent = h.Value
		} else {
			id.Headers[h.Name] = h.Value
		}
	}
	if id.UserAgent == "" {
		id.UserAgent = DefaultUserAgent()
	}
	return id
}

// ResolveIndexer returns the wire identity for one indexer and request
// kind. Without a per-indexer identity it is exactly the shared policy
// (honoring the legacy per-indexer user agent).
func (p ClientIdentity) ResolveIndexer(ix Indexer, grab bool) Identity {
	ri := ix.RequestIdentity
	if ri == nil || strings.TrimSpace(ri.Profile) == "" {
		base := p.Resolve(ix.UserAgent)
		ordered := []HeaderKV{{"User-Agent", base.UserAgent}}
		for k, v := range base.Headers {
			ordered = append(ordered, HeaderKV{k, v})
		}
		sort.SliceStable(ordered[1:], func(i, j int) bool { return ordered[1+i].Name < ordered[1+j].Name })
		var ovs, povs []KVOverride
		if ri != nil {
			if grab {
				ovs, povs = ri.GrabHeaders, ri.GrabParams
			} else {
				ovs, povs = ri.Headers, ri.Params
			}
			// A grab-only profile still applies to grabs.
			if grab && strings.TrimSpace(ri.GrabProfile) != "" {
				if prof, ok := LookupProfile(ri.GrabProfile); ok {
					return identityFromOrdered(applyHeaderOverrides(prof.Headers, ovs), povs, prof.Shape, prof.ID)
				}
			}
		}
		return identityFromOrdered(applyHeaderOverrides(ordered, ovs), povs, QueryShape{}, "")
	}
	profileID := strings.ToLower(strings.TrimSpace(ri.Profile))
	ovs, povs := ri.Headers, ri.Params
	if grab {
		ovs, povs = ri.GrabHeaders, ri.GrabParams
		if g := strings.TrimSpace(ri.GrabProfile); g != "" {
			profileID = strings.ToLower(g)
		} else {
			profileID = PairedGrabProfile(profileID)
		}
	}
	prof, ok := LookupProfile(profileID)
	if !ok {
		prof = ClientProfile{ID: profileID, Headers: []HeaderKV{{"User-Agent", DefaultUserAgent()}}}
	}
	return identityFromOrdered(applyHeaderOverrides(prof.Headers, ovs), povs, prof.Shape, prof.ID)
}

// ShapeQuery applies the identity's query shape and parameter overrides
// and encodes in profile order (remaining keys sorted). apikey and t can
// be changed by an override but never removed.
func (id Identity) ShapeQuery(params url.Values) string {
	v := url.Values{}
	for k, vs := range params {
		v[k] = append([]string(nil), vs...)
	}
	if id.Shape.YearInQuery {
		if y := v.Get("year"); y != "" {
			v.Del("year")
			if q := v.Get("q"); q != "" && !strings.HasSuffix(q, y) {
				v.Set("q", q+" "+y)
			}
		}
	}
	if id.Shape.PadSeasonZero && v.Get("season") == "0" {
		v.Set("season", "00")
	}
	if id.Shape.FirstPageOffset && v.Get("offset") == "" {
		v.Set("offset", "0")
	}
	for _, ov := range id.Params {
		name := strings.TrimSpace(ov.Name)
		if name == "" {
			continue
		}
		if ov.Remove {
			if name == "apikey" || name == "t" {
				continue
			}
			v.Del(name)
		} else {
			v.Set(name, ov.Value)
		}
	}
	seen := map[string]bool{}
	var keys []string
	for _, k := range id.Shape.ParamOrder {
		if _, ok := v[k]; ok && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	var rest []string
	for k := range v {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)
	var b strings.Builder
	for _, k := range keys {
		for _, val := range v[k] {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(val))
		}
	}
	return b.String()
}

// Validate checks a per-indexer request identity. Field is "requestIdentity".
func (ri *IndexerRequestIdentity) Validate() []IdentityFieldError {
	if ri == nil {
		return nil
	}
	var errs []IdentityFieldError
	add := func(format string, args ...any) {
		errs = append(errs, IdentityFieldError{"requestIdentity", fmt.Sprintf(format, args...)})
	}
	for _, id := range []string{ri.Profile, ri.GrabProfile} {
		if strings.TrimSpace(id) != "" {
			if _, ok := LookupProfile(id); !ok {
				add("unknown client profile %q", id)
			}
		}
	}
	checkHeaders := func(ovs []KVOverride) {
		if len(ovs) > 32 {
			add("at most 32 header overrides")
			return
		}
		seen := map[string]bool{}
		for _, ov := range ovs {
			name := strings.TrimSpace(ov.Name)
			if !headerNameRE.MatchString(name) || len(name) > 64 {
				add("%q is not a valid header name", ov.Name)
				continue
			}
			lower := strings.ToLower(name)
			if seen[lower] {
				add("%q is listed twice", name)
				continue
			}
			seen[lower] = true
			switch lower {
			case "host", "content-length", "transfer-encoding":
				add("%q is sent by the transport and cannot be overridden", name)
				continue
			}
			if !ov.Remove {
				if msg := invalidHeaderValue(ov.Value, 1024); msg != "" {
					add("%q %s", name, msg)
				}
			}
		}
	}
	checkParams := func(ovs []KVOverride) {
		if len(ovs) > 32 {
			add("at most 32 parameter overrides")
			return
		}
		seen := map[string]bool{}
		for _, ov := range ovs {
			name := strings.TrimSpace(ov.Name)
			if name == "" || len(name) > 64 || strings.ContainsAny(name, " \t\r\n\x00&=") {
				add("%q is not a valid parameter name", ov.Name)
				continue
			}
			if seen[name] {
				add("%q is listed twice", name)
				continue
			}
			seen[name] = true
			if ov.Remove && (name == "apikey" || name == "t") {
				add("%q cannot be removed", name)
			}
			if len(ov.Value) > 1024 || strings.ContainsAny(ov.Value, "\r\n\x00") {
				add("%q has an invalid value", name)
			}
		}
	}
	checkHeaders(ri.Headers)
	checkHeaders(ri.GrabHeaders)
	checkParams(ri.Params)
	checkParams(ri.GrabParams)
	return errs
}

// SensitiveHeader reports whether a header's value is a credential that
// must be redacted in API responses.
func SensitiveHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "cookie", "x-api-key", "proxy-authorization":
		return true
	}
	return false
}

// ProfileForUserAgent maps a saved user agent to the profile that sends
// it, for the one-time migration of the user-agent setting.
func ProfileForUserAgent(ua string) (string, bool) {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "", false
	}
	for id, v := range clientVersions {
		if ua == v {
			return id, true
		}
	}
	for id, v := range legacyPresetUserAgents {
		if ua == v {
			return id, true
		}
	}
	return "", false
}
