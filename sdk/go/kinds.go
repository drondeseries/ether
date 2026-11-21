package etherplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Content-kind contract (Ether v0.3.8): a plugin declares media kinds in
// its manifest and answers /ether/v1/kinds/{kind}/* routes. These types
// mirror the host's wire shapes exactly; the shared fixtures under
// sdk/conformance/kinds pin all three SDKs to the same JSON.

// ItemUnavailable explains why an item cannot currently be played.
type ItemUnavailable struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// KindItem is the standardised plugin item envelope. Ether assigns the
// stored identity; the plugin only ever sees its own external ID.
type KindItem struct {
	Unavailable *ItemUnavailable           `json:"unavailable,omitempty"`
	ID          string                     `json:"id"`
	Level       string                     `json:"level"`
	ParentID    string                     `json:"parentId,omitempty"`
	Index       int                        `json:"index,omitempty"`
	Title       string                     `json:"title"`
	SortTitle   string                     `json:"sortTitle,omitempty"`
	Summary     string                     `json:"summary,omitempty"`
	Year        int                        `json:"year,omitempty"`
	Released    int64                      `json:"released,omitempty"` // unix seconds
	Rating      float64                    `json:"rating,omitempty"`
	Playable    bool                       `json:"playable,omitempty"`
	ChildCount  int                        `json:"childCount,omitempty"`
	LeafCount   int                        `json:"leafCount,omitempty"`
	Art         []KindArtRef               `json:"art,omitempty"`
	Fields      map[string]json.RawMessage `json:"fields,omitempty"`
	Hints       json.RawMessage            `json:"hints,omitempty"` // opaque JSON object, echoed to the resolver
	ETag        string                     `json:"etag,omitempty"`
}

// KindItemPart describes one declared file/part in hints.parts on a multi-file
// KindItem (#576). Ether passes the zero-based index back in
// KindResolveRequest.PartIndex when resolving each part.
type KindItemPart struct {
	Index       int     `json:"index"`
	Name        string  `json:"name,omitempty"`
	MIME        string  `json:"mime,omitempty"`
	Size        int64   `json:"size,omitempty"`
	DurationSec float64 `json:"durationSec,omitempty"`
}

// KindArtRef is one artwork reference on a KindItem.
type KindArtRef struct {
	Role   string `json:"role"`
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// KindPage is one page of kind items; an empty NextCursor ends the walk.
type KindPage struct {
	Items      []KindItem `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Total      *int       `json:"total,omitempty"`
}

// KindBrowseRequest is Ether's browse/search request for one kind level.
type KindBrowseRequest struct {
	Kind     string `json:"-"` // filled from the route, never on the wire
	ParentID string `json:"parentId,omitempty"`
	Level    string `json:"level,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Sort     string `json:"sort,omitempty"`
	Desc     bool   `json:"desc,omitempty"`
	// Facets requires any of the listed values per field; AllFacets
	// requires every listed value and ExcludeFacets drops items having
	// any listed value. All three combine with AND.
	Facets        map[string][]string `json:"facets,omitempty"`
	AllFacets     map[string][]string `json:"allFacets,omitempty"`
	ExcludeFacets map[string][]string `json:"excludeFacets,omitempty"`
	Query         string              `json:"query,omitempty"`
}

// KindBrowseQuery is the query passed to kinds.browse from a frontend adapter (#615).
type KindBrowseQuery = KindBrowseRequest

// KindExpandRequest asks a plugin to expand one declared facet bucket or
// plugin person slice in the background via POST /ether/v1/kinds/{kind}/expand.
type KindExpandRequest struct {
	Kind    string `json:"-"`
	Facet   string `json:"facet"`
	Value   string `json:"value"`
	ValueID int64  `json:"valueId,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
}

// KindExpandResponse is returned by POST /ether/v1/kinds/{kind}/expand.
// Either Items (with optional Cursor/NextCursor for further pages) or Job
// ("/ether/job/...") is populated.
type KindExpandResponse struct {
	Items      []KindItem `json:"items,omitempty"`
	Cursor     string     `json:"cursor,omitempty"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Job        string     `json:"job,omitempty"`
}

// KindChangePage is one delta page for pull sync.
type KindChangePage struct {
	Upserts    []KindItem `json:"upserts,omitempty"`
	Deletes    []string   `json:"deletes,omitempty"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Complete   bool       `json:"complete"`
}

// KindChangesRequest is Ether's delta-feed request.
type KindChangesRequest struct {
	Kind   string `json:"-"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// SourceQuery asks Ether to find sources (Host.ResolveSources).
type SourceQuery struct {
	Kind  string `json:"kind"` // indexer_search|debrid_check|debrid_acquire|release_files
	Query string `json:"query,omitempty"`
	// Category accepts comma-separated Newznab IDs or standard family names;
	// when set with an empty Query, indexer_search requests recent releases.
	Category string `json:"category,omitempty"`
	// Profile names the quality profile to score video release names against
	// on indexer_search; empty uses the owner's default profile.
	Profile    string   `json:"profile,omitempty"`
	InfoHashes []string `json:"infoHashes,omitempty"`
	InfoHash   string   `json:"infoHash,omitempty"`
	Magnet     string   `json:"magnet,omitempty"`
	// Candidate carries an indexer_search candidate back for release_files.
	Candidate *SourceCandidate `json:"candidate,omitempty"`
}

// ReleaseFile is one file inside a candidate release (release_files).
type ReleaseFile struct {
	Index int    `json:"index"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
}

// SourceCandidate is one Ether-found release for a resolver to rank.
type SourceCandidate struct {
	InfoHash string `json:"infoHash,omitempty"`
	Magnet   string `json:"magnet,omitempty"`
	NZBURL   string `json:"nzbUrl,omitempty"`
	// FileIndex selects a zero-based entry in the torrent provider file list; nil uses video selection.
	FileIndex *int   `json:"fileIndex,omitempty"`
	Title     string `json:"title,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Seeders   int    `json:"seeders,omitempty"`
	Cached    bool   `json:"cached,omitempty"`
	Indexer   string `json:"indexer,omitempty"`
	IndexerID string `json:"indexerId,omitempty"`
	// Score, Resolution, Source and Codec are populated on indexer_search
	// candidates whose release name identifies video; non-video candidates
	// leave them empty.
	Score      *int   `json:"score,omitempty"`
	Resolution int    `json:"resolution,omitempty"`
	Source     string `json:"source,omitempty"`
	Codec      string `json:"codec,omitempty"`
	// Files is populated only by a release_files query.
	Files []ReleaseFile `json:"files,omitempty"`

	// Ref names this candidate in match scores. Ether derives it from the
	// infohash/NZB when empty; matchers echo it verbatim.
	Ref string `json:"-"`
}

// CandidateRef returns the stable ref matchers report for a candidate.
func (c SourceCandidate) CandidateRef() string {
	switch {
	case c.Ref != "":
		return c.Ref
	case c.InfoHash != "":
		return "infohash:" + strings.ToLower(c.InfoHash)
	case c.Magnet != "":
		return "magnet:" + c.Magnet
	case c.NZBURL != "":
		return "nzb:" + c.NZBURL
	default:
		return c.Title
	}
}

// KindMatchRequest asks a matcher to score candidates for one item.
type KindMatchRequest struct {
	Kind       string            `json:"-"`
	Item       KindItem          `json:"item"`
	Candidates []SourceCandidate `json:"candidates"`
	Settings   map[string]any    `json:"settings,omitempty"`
}

// KindMatchScore is one candidate score (0..1) with plain-language reasons.
type KindMatchScore struct {
	CandidateRef string   `json:"candidateRef"`
	Score        float64  `json:"score"`
	Reasons      []string `json:"reasons,omitempty"`
}

// KindMatchResponse is a matcher's answer, best first.
type KindMatchResponse struct {
	Pending   bool             `json:"pending,omitempty"`
	RetryInMS int64            `json:"retryInMs,omitempty"`
	Job       string           `json:"job,omitempty"`
	Matches   []KindMatchScore `json:"matches"`
}

// PlayableSource is one candidate way to acquire an item's bytes. URL may
// carry {itemIndex}, {pageNumber}, {byteStart}, {byteEnd} and {byteRange}
// placeholders that Ether substitutes.
type PlayableSource struct {
	Path        string            `json:"path,omitempty"`
	ID          string            `json:"id"`
	Label       string            `json:"label,omitempty"`
	Pattern     string            `json:"pattern"` // range|download|direct|container|release|file
	MIME        string            `json:"mime"`
	Size        int64             `json:"size,omitempty"`
	DurationSec float64           `json:"durationSec,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	BlobRef     string            `json:"blobRef,omitempty"`
	SHA256      string            `json:"sha256,omitempty"`
	// SHA1 and CRC32 are hex digests checked against the served bytes,
	// including a release's selected file or container member; Size too.
	SHA1      string        `json:"sha1,omitempty"`
	CRC32     string        `json:"crc32,omitempty"`
	MaxBytes  int64         `json:"maxBytes,omitempty"`
	ExpiresAt int64         `json:"expiresAt,omitempty"`
	Container *ContainerRef `json:"container,omitempty"`
	Release   *ReleaseHint  `json:"release,omitempty"`
}

// ContainerRef describes an archive member inside ordered volumes.
type ContainerRef struct {
	Format         string      `json:"format,omitempty"` // "" (sniff)|rar|7z|zip
	Volumes        []VolumeRef `json:"volumes"`
	Member         string      `json:"member,omitempty"`
	MemberIndex    *int        `json:"memberIndex,omitempty"`
	PasswordSecret string      `json:"passwordSecret,omitempty"`
}

// VolumeRef locates one archive volume by URL or blob cache reference.
type VolumeRef struct {
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	BlobRef string            `json:"blobRef,omitempty"`
	Size    int64             `json:"size"`
}

// ReleaseHint hands a release to Ether's Usenet/debrid/torrent pipeline.
type ReleaseHint struct {
	// FileIndex overrides candidate.fileIndex; out-of-range indices fail without fallback.
	FileIndex *int             `json:"fileIndex,omitempty"`
	NZBURL    string           `json:"nzbUrl,omitempty"`
	Magnet    string           `json:"magnet,omitempty"`
	InfoHash  string           `json:"infoHash,omitempty"`
	Candidate *SourceCandidate `json:"candidate,omitempty"`
	// File selects an exact torrent path or unique basename, including non-video files.
	File           string `json:"file,omitempty"`
	FileHint       string `json:"fileHint,omitempty"`
	PasswordSecret string `json:"passwordSecret,omitempty"`
}

// KindResolveRequest asks a resolver for playable sources for one item.
type KindResolveRequest struct {
	Kind       string         `json:"-"`
	LibraryKey string         `json:"libraryKey"`
	Item       KindItem       `json:"item"`
	PartIndex  *int           `json:"partIndex,omitempty"`
	Client     map[string]any `json:"client,omitempty"`
}

// KindResolveResponse is a resolver's ordered sources.
type KindResolveResponse struct {
	Pending   bool             `json:"pending,omitempty"`
	RetryInMS int64            `json:"retryInMs,omitempty"`
	Job       string           `json:"job,omitempty"`
	Sources   []PlayableSource `json:"sources"`
	TTLSec    int              `json:"ttlSec,omitempty"`
}

// KindLiveRequest asks a kind live provider for currently live items on a
// declared "live" library tab (POST /ether/v1/kinds/{kind}/live).
type KindLiveRequest struct {
	Kind   string `json:"-"`
	Tab    string `json:"tab,omitempty"`
	Q      string `json:"q,omitempty"`
	Filter string `json:"filter,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// KindLiveItem is one live stream item returned by POST /ether/v1/kinds/{kind}/live.
type KindLiveItem struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Subtitle string   `json:"subtitle,omitempty"`
	Badge    string   `json:"badge,omitempty"`
	Viewers  *int     `json:"viewers,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Thumb    string   `json:"thumb,omitempty"`
	Live     *bool    `json:"live,omitempty"`
}

// KindLiveFilter is one optional filter option returned by a live tab.
type KindLiveFilter struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// KindLivePage is returned by POST /ether/v1/kinds/{kind}/live.
type KindLivePage struct {
	Items      []KindLiveItem   `json:"items"`
	Filters    []KindLiveFilter `json:"filters,omitempty"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// KindLiveResolveRequest asks a kind live provider to resolve one live item ID
// into a playable stream URL (POST /ether/v1/kinds/{kind}/live/resolve).
type KindLiveResolveRequest struct {
	Kind string `json:"-"`
	ID   string `json:"id"`
}

// KindLiveResolveResponse is returned by POST /ether/v1/kinds/{kind}/live/resolve.
type KindLiveResolveResponse struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	MIME    string            `json:"mime,omitempty"`
	Title   string            `json:"title,omitempty"`
}

// SetupTestRequest is Ether's setup-step test call: the step key plus the
// saved (non-secret) values of the step's fields.
type SetupTestRequest struct {
	Step   string         `json:"step"`
	Fields map[string]any `json:"fields,omitempty"`
}

// SetupTestResult is a setup test's answer.
type SetupTestResult struct {
	OK     bool              `json:"ok"`
	Detail string            `json:"detail,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Reserved plugin error codes Ether understands on kind routes.
var (
	ErrKindNotFound     = &RouteError{Status: http.StatusNotFound, Code: "not_found", Message: "Item not found."}
	ErrKindNotAvailable = &RouteError{Status: http.StatusOK, Code: "not_available", Message: "No source is available right now."}
)

// CatalogProvider serves browse and detail for one kind. Implement
// ChangesProvider too for delta pull sync; without it Ether re-walks.
type CatalogProvider interface {
	Browse(ctx *Context, req KindBrowseRequest) (KindPage, error)
	Detail(ctx *Context, kind, id string) (KindItem, error)
}

// ChangesProvider is the optional delta feed for pull sync.
type ChangesProvider interface {
	Changes(ctx *Context, req KindChangesRequest) (KindChangePage, error)
}

// SearchProvider is the optional provider-side search route.
type SearchProvider interface {
	Search(ctx *Context, req KindBrowseRequest) (KindPage, error)
}

// ExpandProvider is the optional on-demand facet/person slice expander for
// kinds that declare browse.expand.
type ExpandProvider interface {
	Expand(ctx *Context, req KindExpandRequest) (KindExpandResponse, error)
}

// KindResolver turns a playable item into ordered playable sources.
type KindResolver interface {
	Resolve(ctx *Context, req KindResolveRequest) (KindResolveResponse, error)
}

// KindMatcher scores source candidates for one item.
type KindMatcher interface {
	Match(ctx *Context, req KindMatchRequest) (KindMatchResponse, error)
}

// KindLiveProvider serves a kind's declared "live" library tab: listing
// currently live streams and resolving an item ID into a playable stream URL.
type KindLiveProvider interface {
	Live(ctx *Context, req KindLiveRequest) (KindLivePage, error)
	ResolveLive(ctx *Context, req KindLiveResolveRequest) (KindLiveResolveResponse, error)
}

// FrontendAdapter is a plugin-provided protocol surface (OPDS, Subsonic,
// custom). Ether authenticates the viewer first and forwards the request
// under /ether/v1/frontend/{key}; the handler sees X-Ether-Viewer headers
// and never a credential.
type FrontendAdapter interface {
	Handler() http.Handler
}

// SetupTester answers setup-step test calls.
type SetupTester interface {
	TestSetup(ctx *Context, req SetupTestRequest) (SetupTestResult, error)
}

type kindHandlers struct {
	catalog  map[string]CatalogProvider
	resolve  map[string]KindResolver
	match    map[string]KindMatcher
	live     map[string]KindLiveProvider
	frontend map[string]FrontendAdapter
	setup    SetupTester
}

func (p *Plugin) kinds() *kindHandlers {
	if p.kindHandlers == nil {
		p.kindHandlers = &kindHandlers{
			catalog: map[string]CatalogProvider{}, resolve: map[string]KindResolver{},
			match: map[string]KindMatcher{}, live: map[string]KindLiveProvider{},
			frontend: map[string]FrontendAdapter{},
		}
	}
	return p.kindHandlers
}

// OnCatalog registers the catalogue provider for one declared kind.
func (p *Plugin) OnCatalog(kind string, c CatalogProvider) { p.kinds().catalog[kind] = c }

// OnResolve registers the resolver for one declared kind.
func (p *Plugin) OnResolve(kind string, r KindResolver) { p.kinds().resolve[kind] = r }

// OnMatch registers the matcher for one declared kind.
func (p *Plugin) OnMatch(kind string, m KindMatcher) { p.kinds().match[kind] = m }

// OnLive registers the live-tab provider for one declared kind.
func (p *Plugin) OnLive(kind string, l KindLiveProvider) { p.kinds().live[kind] = l }

// OnFrontend registers one declared frontend adapter.
func (p *Plugin) OnFrontend(key string, a FrontendAdapter) { p.kinds().frontend[key] = a }

// OnSetupTest registers the handler for /ether/v1/setup/{step}/test.
func (p *Plugin) OnSetupTest(t SetupTester) { p.kinds().setup = t }

// StreamURL mints a short-lived, scoped ticket for one of this plugin's own
// kind items and returns the Ether stream URL to embed in a frontend adapter
// response (spec §6.5). The viewer is taken from the request's X-Ether-Viewer
// header, which Ether injects after authenticating; the plugin never sees a
// credential. Bytes are served by Ether from the ticket, never by the plugin.
func (c *Context) StreamURL(itemRef string) (string, error) {
	if c.Ether == nil {
		return "", errors.New("stream url: no host service available")
	}
	viewerID, err := strconv.ParseInt(strings.TrimSpace(c.Headers["X-Ether-Viewer"]), 10, 64)
	if err != nil || viewerID <= 0 {
		return "", errors.New("stream url: no authenticated viewer on this request")
	}
	return c.Ether.StreamURL(c.Context, viewerID, itemRef)
}

// StreamPartURL mints a short-lived, scoped ticket for one zero-based part of
// a multi-file kind item (#576) and returns the Ether stream URL to embed in a
// frontend adapter response.
func (c *Context) StreamPartURL(itemRef string, partIndex int) (string, error) {
	if c.Ether == nil {
		return "", errors.New("stream url: no host service available")
	}
	if partIndex < 0 {
		return "", errors.New("stream url: partIndex must be non-negative")
	}
	viewerID, err := strconv.ParseInt(strings.TrimSpace(c.Headers["X-Ether-Viewer"]), 10, 64)
	if err != nil || viewerID <= 0 {
		return "", errors.New("stream url: no authenticated viewer on this request")
	}
	if ph, ok := c.Ether.(PartStreamHost); ok {
		return ph.StreamPartURL(c.Context, viewerID, itemRef, partIndex)
	}
	u, err := c.Ether.StreamURL(c.Context, viewerID, itemRef)
	if err != nil {
		return "", err
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "part=" + strconv.Itoa(partIndex), nil
}

func (c *Context) viewerToken() string {
	if tok := strings.TrimSpace(headerValue(c.Headers, HeaderViewerToken)); tok != "" {
		return tok
	}
	if tok, ok := ViewerTokenFromContext(c.Context); ok {
		return tok
	}
	return ""
}

// KindVisible evaluates the library's Rules and viewer access policy for a
// bounded batch (at most MaxKindVisibleIDs) of external IDs in kindKey (#615).
// Only frontend adapter requests carrying Ether's authenticated viewer token
// may call it.
func (c *Context) KindVisible(kindKey string, ids []string) ([]string, error) {
	if c.Ether == nil {
		return nil, errors.New("kinds.visible: no host service available")
	}
	kh, ok := c.Ether.(KindPolicyHost)
	if !ok {
		return nil, errors.New("kinds.visible: host client does not implement KindPolicyHost")
	}
	tok := c.viewerToken()
	if tok == "" {
		return nil, errors.New("kinds.visible: no authenticated frontend adapter viewer on this request")
	}
	return kh.KindVisible(WithViewerToken(c.Context, tok), kindKey, ids)
}

// KindBrowse browses or searches kindKey with the library's Rules and viewer
// access policy applied for the requesting frontend-adapter viewer (#615).
// Only frontend adapter requests carrying Ether's authenticated viewer token
// may call it.
func (c *Context) KindBrowse(kindKey string, query KindBrowseQuery) (KindPage, error) {
	if c.Ether == nil {
		return KindPage{}, errors.New("kinds.browse: no host service available")
	}
	kh, ok := c.Ether.(KindPolicyHost)
	if !ok {
		return KindPage{}, errors.New("kinds.browse: host client does not implement KindPolicyHost")
	}
	tok := c.viewerToken()
	if tok == "" {
		return KindPage{}, errors.New("kinds.browse: no authenticated frontend adapter viewer on this request")
	}
	return kh.KindBrowse(WithViewerToken(c.Context, tok), kindKey, query)
}

// dispatchKinds handles the /ether/v1/kinds, /ether/v1/setup and
// /ether/v1/frontend route families. It reports false when the path is
// none of them so Dispatch can fall through to its 404.
func (p *Plugin) dispatchKinds(pctx *Context, method, path string, req RawRequest) (RawResponse, bool) {
	if !strings.HasPrefix(path, "/ether/v1/") {
		return RawResponse{}, false
	}
	rest := strings.TrimPrefix(path, "/ether/v1/")
	parts := strings.Split(rest, "/")
	// Frontend adapters are protocol surfaces (WebDAV, Subsonic, OPDS, …) and
	// see every method token. The kind and setup routes stay JSON GET/POST.
	if parts[0] == "frontend" {
		if len(parts) >= 2 {
			key, _ := url.PathUnescape(parts[1])
			return p.dispatchFrontend(pctx, method, key, "/"+strings.Join(parts[2:], "/"), req), true
		}
		return RawResponse{}, false
	}
	if method != http.MethodPost && method != http.MethodGet {
		return errorJSON(http.StatusMethodNotAllowed, "method_not_allowed", "Kind routes use GET or POST."), true
	}
	switch {
	case len(parts) >= 2 && parts[0] == "kinds":
		kind, _ := url.PathUnescape(parts[1])
		return p.dispatchKind(pctx, kind, parts[2:], req), true
	case len(parts) == 3 && parts[0] == "setup" && parts[2] == "test":
		if p.kindHandlers == nil || p.kindHandlers.setup == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Setup test handler is not registered."), true
		}
		var in SetupTestRequest
		if len(req.Body) > 0 {
			if err := json.Unmarshal(req.Body, &in); err != nil {
				return errorJSON(http.StatusBadRequest, "invalid_json", "Request body must be valid JSON."), true
			}
		}
		if in.Step == "" {
			in.Step, _ = url.PathUnescape(parts[1])
		}
		res, err := p.kindHandlers.setup.TestSetup(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err), true
		}
		return okJSON(res, http.StatusOK), true
	}
	return RawResponse{}, false
}

func (p *Plugin) dispatchKind(pctx *Context, kind string, tail []string, req RawRequest) RawResponse {
	h := p.kindHandlers
	decode := func(v any) (RawResponse, bool) {
		if len(req.Body) == 0 {
			return RawResponse{}, true
		}
		if err := json.Unmarshal(req.Body, v); err != nil {
			return errorJSON(http.StatusBadRequest, "invalid_json", "Request body must be valid JSON."), false
		}
		return RawResponse{}, true
	}
	switch {
	case len(tail) == 1 && (tail[0] == "browse" || tail[0] == "search"):
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		if prov == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No catalogue provider is registered for kind %q.", kind))
		}
		var in KindBrowseRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		if in.Limit <= 0 {
			in.Limit = 200
		}
		if tail[0] == "search" {
			sp, isSearch := prov.(SearchProvider)
			if !isSearch {
				return errorJSON(http.StatusNotImplemented, "not_implemented", "This provider does not search.")
			}
			page, err := sp.Search(pctx, in)
			return kindPageResponse(page, err, in.Limit)
		}
		page, err := prov.Browse(pctx, in)
		return kindPageResponse(page, err, in.Limit)
	case len(tail) == 1 && tail[0] == "expand":
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		ep, isExpand := prov.(ExpandProvider)
		if prov == nil || !isExpand {
			return errorJSON(http.StatusNotImplemented, "not_implemented", "This provider does not expand facets.")
		}
		var in KindExpandRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := ep.Expand(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if res.Job == "" && res.Items == nil {
			res.Items = []KindItem{}
		}
		return okJSON(res, http.StatusOK)
	case len(tail) == 2 && tail[0] == "items":
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		if prov == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No catalogue provider is registered for kind %q.", kind))
		}
		id, _ := url.PathUnescape(tail[1])
		item, err := prov.Detail(pctx, kind, id)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(item, http.StatusOK)
	case len(tail) == 1 && tail[0] == "changes":
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		cp, isChanges := prov.(ChangesProvider)
		if prov == nil || !isChanges {
			// 404/501 tells Ether to fall back to a re-walk diff.
			return errorJSON(http.StatusNotImplemented, "not_implemented", "This provider has no changes feed.")
		}
		var in KindChangesRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		page, err := cp.Changes(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if page.Upserts == nil {
			page.Upserts = []KindItem{}
		}
		return okJSON(page, http.StatusOK)
	case len(tail) == 1 && tail[0] == "resolve":
		var r KindResolver
		if h != nil {
			r = h.resolve[kind]
		}
		if r == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No resolver is registered for kind %q.", kind))
		}
		var in KindResolveRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := r.Resolve(pctx, in)
		if err != nil {
			if errors.Is(err, ErrKindNotAvailable) {
				return okJSON(KindResolveResponse{Sources: []PlayableSource{}}, http.StatusOK)
			}
			return toRouteErrorResponse(err)
		}
		if res.Sources == nil {
			res.Sources = []PlayableSource{}
		}
		return okJSON(res, http.StatusOK)
	case len(tail) == 1 && tail[0] == "live":
		var lp KindLiveProvider
		if h != nil {
			lp = h.live[kind]
		}
		if lp == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No live provider is registered for kind %q.", kind))
		}
		var in KindLiveRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		if in.Limit <= 0 {
			in.Limit = 50
		}
		page, err := lp.Live(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if page.Items == nil {
			page.Items = []KindLiveItem{}
		}
		if in.Limit > 0 && len(page.Items) > in.Limit {
			page.Items = page.Items[:in.Limit]
		}
		return okJSON(page, http.StatusOK)
	case len(tail) == 2 && tail[0] == "live" && tail[1] == "resolve":
		var lp KindLiveProvider
		if h != nil {
			lp = h.live[kind]
		}
		if lp == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No live provider is registered for kind %q.", kind))
		}
		var in KindLiveResolveRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := lp.ResolveLive(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	case len(tail) == 1 && tail[0] == "match":
		var m KindMatcher
		if h != nil {
			m = h.match[kind]
		}
		if m == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No matcher is registered for kind %q.", kind))
		}
		var in KindMatchRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := m.Match(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if res.Matches == nil {
			res.Matches = []KindMatchScore{}
		}
		return okJSON(res, http.StatusOK)
	}
	return errorJSON(http.StatusNotFound, "not_found", "No kind route matched.")
}

func lookup(h *kindHandlers, get func(*kindHandlers) CatalogProvider) CatalogProvider {
	if h == nil {
		return nil
	}
	return get(h)
}

// kindPageResponse enforces the page contract: never more than limit
// items (Ether treats an oversized page as a contract error).
func kindPageResponse(page KindPage, err error, limit int) RawResponse {
	if err != nil {
		return toRouteErrorResponse(err)
	}
	if limit > 0 && len(page.Items) > limit {
		page.Items = page.Items[:limit]
	}
	if page.Items == nil {
		page.Items = []KindItem{}
	}
	return okJSON(page, http.StatusOK)
}

type frontendContextKey struct{}

// FrontendContext returns the Ether plugin Context attached to a
// FrontendAdapter http.Request during dispatch, so ServeHTTP handlers can
// call pctx.KindVisible, pctx.KindBrowse, pctx.StreamURL, etc. directly (#615).
func FrontendContext(ctx context.Context) (*Context, bool) {
	if ctx == nil {
		return nil, false
	}
	pctx, ok := ctx.Value(frontendContextKey{}).(*Context)
	return pctx, ok && pctx != nil
}

const maxFrontendBody = 10 << 20

func (p *Plugin) dispatchFrontend(pctx *Context, method, key, subPath string, req RawRequest) RawResponse {
	var a FrontendAdapter
	if p.kindHandlers != nil {
		a = p.kindHandlers.frontend[key]
	}
	if a == nil || a.Handler() == nil {
		return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No frontend adapter is registered for %q.", key))
	}
	if subPath == "/" || subPath == "" {
		subPath = "/"
	}
	// Protocol adapters (Subsonic, OPDS, WebDAV) carry their arguments in the
	// query string; the server relays them, so rebuild the request URL with
	// the original query. The server flattens repeated keys to the first
	// value, so a query map holds one value per key.
	_, query := parsePathAndQuery(req.Path, req.Query)
	target := "http://ether.invalid" + subPath
	if len(query) > 0 {
		target += "?" + encodeQuery(query)
	}
	reqCtx := pctx.Context
	if tok := headerValue(req.Headers, HeaderViewerToken); tok != "" {
		reqCtx = WithViewerToken(reqCtx, tok)
		pctx.Context = reqCtx
	}
	reqCtx = context.WithValue(reqCtx, frontendContextKey{}, pctx)
	httpReq := httptest.NewRequestWithContext(reqCtx, method, target, bytes.NewReader(req.Body))
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httpReq)
	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxFrontendBody+1))
	if err != nil {
		return errorJSON(http.StatusBadGateway, "adapter_error", err.Error())
	}
	if len(body) > maxFrontendBody {
		return errorJSON(http.StatusBadGateway, "adapter_error", "Frontend adapter response is larger than 10 MiB; serve media through Ether stream URLs.")
	}
	hdrs := map[string]string{}
	for k := range res.Header {
		hdrs[k] = res.Header.Get(k)
	}
	// A HEAD reply carries headers only, exactly as a real server would.
	if method == http.MethodHead {
		body = nil
	}
	return RawResponse{Status: res.StatusCode, Headers: hdrs, BinaryBody: body}
}

// encodeQuery renders a query map back into a raw query string, preserving
// key order is unnecessary; values are escaped so adapters see what the
// viewer sent.
func encodeQuery(query map[string]string) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(query[k]))
	}
	return strings.Join(parts, "&")
}
