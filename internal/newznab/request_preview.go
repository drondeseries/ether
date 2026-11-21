package newznab

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
)

// RequestPreview is what the console shows as "what the indexer sees":
// the exact request line and ordered headers for a sample call. The API
// key value is never included.
type RequestPreview struct {
	Profile     string           `json:"profile"`
	Search      PreviewCall      `json:"search"`
	GrabProfile string           `json:"grabProfile"`
	Grab        PreviewCall      `json:"grab"`
	Profiles    []PreviewProfile `json:"profiles"`
}

// PreviewCall is one sample request.
type PreviewCall struct {
	RequestLine string            `json:"requestLine"`
	Headers     []config.HeaderKV `json:"headers"`
}

// PreviewProfile describes one selectable profile for the picker.
type PreviewProfile struct {
	ID         string `json:"id"`
	UserAgent  string `json:"userAgent"`
	GrabPaired string `json:"grabPaired,omitempty"`
}

// PreviewRequestIdentity builds the preview for a (possibly unsaved)
// indexer draft. It performs no network I/O.
func PreviewRequestIdentity(cfg config.Indexer, policy config.ClientIdentity) (*RequestPreview, error) {
	draft := cfg
	if strings.TrimSpace(draft.URL) == "" {
		draft.URL = "https://indexer.example.com"
	}
	realKey := draft.APIKey
	draft.APIKey = "••••"
	c := New(draft, nil, Options{ClientIdentity: policy, SearchCacheTTL: -1})
	search := c.clientIdentity()
	grab := c.grabIdentity()
	searchURL, err := c.apiURLFor(url.Values{
		"t": {"tvsearch"}, "q": {"Example Show"}, "season": {"0"}, "ep": {"1"},
		"cat": {"5000"}, "extended": {"1"}, "limit": {"100"},
	}, search)
	if err != nil {
		return nil, err
	}
	grabURL, err := c.apiURLFor(url.Values{"t": {"get"}, "id": {"example-guid"}}, grab)
	if err != nil {
		return nil, err
	}
	mask := func(u string) string {
		if realKey != "" {
			u = strings.ReplaceAll(u, url.QueryEscape(realKey), "••••")
			u = strings.ReplaceAll(u, realKey, "••••")
		}
		return u
	}
	call := func(raw string, id config.Identity) (PreviewCall, error) {
		u, err := url.Parse(raw)
		if err != nil {
			return PreviewCall{}, err
		}
		headers := make([]config.HeaderKV, 0, len(id.Ordered))
		for _, h := range id.Ordered {
			v := h.Value
			if strings.EqualFold(h.Name, "traceparent") && v == "{traceparent}" {
				v = "00-…-…-01 (changes each request)"
			}
			headers = append(headers, config.HeaderKV{Name: h.Name, Value: v})
		}
		return PreviewCall{
			RequestLine: fmt.Sprintf("GET %s HTTP/1.1\nHost: %s", mask(u.RequestURI()), u.Host),
			Headers:     headers,
		}, nil
	}
	sc, err := call(searchURL, search)
	if err != nil {
		return nil, err
	}
	gc, err := call(grabURL, grab)
	if err != nil {
		return nil, err
	}
	out := &RequestPreview{Profile: search.Profile, GrabProfile: grab.Profile, Search: sc, Grab: gc}
	for _, p := range config.ClientProfiles() {
		out.Profiles = append(out.Profiles, PreviewProfile{ID: p.ID, UserAgent: config.ProfileUserAgent(p.ID), GrabPaired: p.GrabPaired})
	}
	return out, nil
}
