package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
)

// KindUpsertResult is what one kinds.upsert batch did (#646): Accepted
// items were taken, of which Added were new, Updated rewrote a stored
// item, and Unchanged matched the stored item and were skipped.
type KindUpsertResult struct {
	Accepted  int
	Added     int
	Updated   int
	Unchanged int
}

// KindBackends are the Ether services the kind host calls end in: the
// plugin-item store, the source resolver and the blob cache. Any field
// may be nil, in which case the matching host call returns
// ErrUnavailable. App wiring fills them (pluginitems, the indexer and
// debrid services, blobcache); the plugin id is always the caller's,
// so a plugin can only ever reach its own kinds and cache.
type KindBackends struct {
	KindUpsert func(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (accepted int, err error)
	// KindUpsertStats, when set, is preferred over KindUpsert and its
	// counts ride the host reply.
	KindUpsertStats func(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (KindUpsertResult, error)
	KindDelete      func(ctx context.Context, pluginID, kindKey string, externalIDs []string) error
	KindItems       func(ctx context.Context, pluginID, kindKey, cursor string, limit int) (core.KindPage, error)
	KindItemsByID   func(ctx context.Context, pluginID, kindKey string, externalIDs []string) ([]core.KindItem, error)
	KindItemCount   func(ctx context.Context, pluginID string) (int, error)
	// KindVisible and KindBrowse evaluate a plugin library's Rules and viewer
	// access policy for the requesting frontend-adapter viewer (#615).
	KindVisible    func(ctx context.Context, pluginID string, viewerID int64, kindKey string, ids []string) ([]string, error)
	KindBrowse     func(ctx context.Context, pluginID string, viewerID int64, kindKey string, q core.KindBrowseRequest) (core.KindPage, error)
	ResolveSources func(ctx context.Context, pluginID string, q core.SourceQuery) ([]core.SourceCandidate, error)
	CachePut       func(ctx context.Context, pluginID, libraryKey, name string, r io.Reader) (blobRef string, err error)
	CacheOpen      func(ctx context.Context, pluginID, blobRef string) (io.ReadSeekCloser, error)
	CacheDelete    func(ctx context.Context, pluginID, blobRef string) error
	// StreamURL mints a scoped stream ticket for one of the plugin's own
	// kind items for one viewer and returns the Ether stream URL (spec
	// §6.5). The backend re-checks item ownership and library visibility.
	StreamURL     func(ctx context.Context, pluginID string, viewerID int64, itemRef string) (string, error)
	StreamPartURL func(ctx context.Context, pluginID string, viewerID int64, itemRef string, partIndex *int) (string, error)
}

// SetKindBackends wires the kind host services into the Manager's
// Backends. Called once at startup by app wiring or in tests.
func (m *Manager) SetKindBackends(b KindBackends) {
	m.mu.Lock()
	m.deps.Backends.KindUpsert = b.KindUpsert
	m.deps.Backends.KindUpsertStats = b.KindUpsertStats
	m.deps.Backends.KindDelete = b.KindDelete
	m.deps.Backends.KindItems = b.KindItems
	m.deps.Backends.KindItemsByID = b.KindItemsByID
	m.deps.Backends.KindItemCount = b.KindItemCount
	m.deps.Backends.KindVisible = b.KindVisible
	m.deps.Backends.KindBrowse = b.KindBrowse
	m.deps.Backends.ResolveSources = b.ResolveSources
	m.deps.Backends.CachePut = b.CachePut
	m.deps.Backends.CacheOpen = b.CacheOpen
	m.deps.Backends.CacheDelete = b.CacheDelete
	m.deps.Backends.StreamURL = b.StreamURL
	m.deps.Backends.StreamPartURL = b.StreamPartURL
	m.mu.Unlock()
}

func (m *Manager) kindBackends() KindBackends {
	m.mu.Lock()
	defer m.mu.Unlock()
	return KindBackends{
		KindUpsert:      m.deps.Backends.KindUpsert,
		KindUpsertStats: m.deps.Backends.KindUpsertStats,
		KindDelete:      m.deps.Backends.KindDelete,
		KindItems:       m.deps.Backends.KindItems,
		KindItemsByID:   m.deps.Backends.KindItemsByID,
		KindItemCount:   m.deps.Backends.KindItemCount,
		KindVisible:     m.deps.Backends.KindVisible,
		KindBrowse:      m.deps.Backends.KindBrowse,
		ResolveSources:  m.deps.Backends.ResolveSources,
		CachePut:        m.deps.Backends.CachePut,
		CacheOpen:       m.deps.Backends.CacheOpen,
		CacheDelete:     m.deps.Backends.CacheDelete,
		StreamURL:       m.deps.Backends.StreamURL,
		StreamPartURL:   m.deps.Backends.StreamPartURL,
	}
}

// Per-call caps for kind host services (spec §3.3 sizes, host side).
const (
	MaxKindUpsertItems = 1000
	MaxKindVisibleIDs  = 500
	MaxKindHintsBytes  = 16 << 10
	MaxKindArtRoles    = 16
	// HeaderAdapterViewerToken carries a host-minted, request-scoped token on
	// frontend adapter calls so remote plugins can prove the authenticated
	// viewer on viewer-scoped host callbacks without supplying viewer IDs.
	HeaderAdapterViewerToken = "X-Ether-Viewer-Token"
)

var externalIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

// ValidExternalID reports whether id is a legal plugin external id
// (spec §2.2: no ':' or '/').
func ValidExternalID(id string) bool { return externalIDRe.MatchString(id) }

// CheckKindItem validates one item envelope against its kind
// declaration: identity, level, the string caps and the hints shape.
// Field-value typing against KindField is the item store's deeper
// check (spec §3.3); this one is shared by the host and the kit so a
// plugin is told the same thing everywhere. It returns a plain
// reason, or "" when the item passes.
func CheckKindItem(decl MediaKindMeta, it core.KindItem) string {
	switch {
	case !ValidExternalID(it.ID):
		return "id must start with a letter or digit and use letters, digits, dot, dash, underscore or tilde, at most 128 characters"
	case it.ParentID != "" && !ValidExternalID(it.ParentID):
		return "parentId must be a valid external id"
	case strings.TrimSpace(it.Title) == "":
		return "title is required"
	case utf8.RuneCountInString(it.Title) > 500:
		return "title is longer than 500 characters"
	case utf8.RuneCountInString(it.Summary) > 20000:
		return "summary is longer than 20000 characters"
	case len(it.Art) > MaxKindArtRoles:
		return "art lists more than 16 roles"
	case len(it.Hints) > MaxKindHintsBytes:
		return "hints are larger than 16 KiB"
	}
	lv, at, ok := decl.Level(it.Level)
	if !ok {
		return "level " + it.Level + " is not declared for this kind"
	}
	if at == 0 && it.ParentID != "" {
		return "a root-level item cannot name a parent"
	}
	if at > 0 && it.ParentID == "" {
		return "a " + it.Level + " item needs a parentId"
	}
	if it.Playable && !lv.Playable {
		return "level " + it.Level + " is not declared playable"
	}
	if len(it.Hints) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(it.Hints, &obj); err != nil || obj == nil {
			return "hints must be a JSON object"
		}
	}
	seenArt := map[string]bool{}
	for _, a := range it.Art {
		if a.Role == "" || a.URL == "" || seenArt[a.Role] {
			return "each art entry needs a distinct role and a url"
		}
		seenArt[a.Role] = true
	}
	for key := range it.Fields {
		if !kindFieldRe.MatchString(key) {
			return "field names must match the declared field pattern"
		}
	}
	return ""
}

// kindCheck resolves the caller's record and one declared kind,
// requiring one of caps. Roles the owner switched off are absent from
// the effective manifest, so they deny like a missing capability.
func (h *host) kindCheck(ctx context.Context, kindKey string, caps ...Capability) (Record, MediaKindMeta, error) {
	r, _, err := h.record(ctx)
	if err != nil {
		return Record{}, MediaKindMeta{}, err
	}
	man := r.Manifest
	man.Ether = man.EffectiveEther()
	allowed := false
	for _, c := range caps {
		if man.HasCapability(c) {
			allowed = true
		}
	}
	if !allowed {
		h.noteDenied(string(caps[0]))
		return Record{}, MediaKindMeta{}, fmt.Errorf("%w: %s", ErrPermissionDenied, caps[0])
	}
	decl, ok := man.MediaKindByKey(kindKey)
	if !ok {
		return Record{}, MediaKindMeta{}, fmt.Errorf("%w: kind %q is not declared", ErrUnavailable, kindKey)
	}
	return r, decl, nil
}

type kindInverse struct {
	Op       string          `json:"op"`
	Kind     string          `json:"kind"`
	Items    []string        `json:"items,omitempty"`    // external ids touched (or created on upsert)
	Previous []core.KindItem `json:"previous,omitempty"` // previous items before upsert or delete
}

// snapshotKindItems looks up any existing items for kindKey whose IDs
// are in ids so a live KindUpsert or KindDelete can record a real
// inverse in the journal.
func (h *host) snapshotKindItems(ctx context.Context, kindKey string, ids []string) (existing []core.KindItem, missing []string) {
	if len(ids) == 0 {
		return nil, nil
	}
	unique := make([]string, 0, len(ids))
	seenID := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !seenID[id] {
			seenID[id] = true
			unique = append(unique, id)
		}
	}
	readFn := h.m.kindBackends().KindItemsByID
	if readFn == nil {
		return nil, unique
	}
	items, err := readFn(ctx, h.plugin, kindKey, unique)
	if err != nil {
		return nil, unique
	}
	found := make(map[string]core.KindItem, len(items))
	for _, it := range items {
		if _, dup := found[it.ID]; !dup {
			found[it.ID] = it
		}
	}
	for _, id := range unique {
		if prev, ok := found[id]; ok {
			existing = append(existing, prev)
		} else {
			missing = append(missing, id)
		}
	}
	return existing, missing
}

func (h *host) KindUpsert(ctx context.Context, kindKey string, items []core.KindItem) (int, error) {
	result, err := h.KindUpsertWithResult(ctx, kindKey, items)
	return result.Accepted, err
}

// KindUpsertWithResult is KindUpsert reporting the added/updated/
// unchanged split when the backend provides it (#646).
func (h *host) KindUpsertWithResult(ctx context.Context, kindKey string, items []core.KindItem) (KindUpsertResult, error) {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return KindUpsertResult{}, err
	}
	r, decl, err := h.kindCheck(ctx, kindKey, CapCatalogProvider)
	if err != nil {
		return KindUpsertResult{}, err
	}
	if decl.Sync.Mode != KindSyncPush {
		return KindUpsertResult{}, fmt.Errorf("%w: kind %s is pull-synced by Ether and takes no pushes", ErrPermissionDenied, kindKey)
	}
	if len(items) > MaxKindUpsertItems {
		return KindUpsertResult{}, fmt.Errorf("plugins: kind upsert batch larger than %d items", MaxKindUpsertItems)
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if msg := CheckKindItem(decl, it); msg != "" {
			return KindUpsertResult{}, fmt.Errorf("plugins: kind item %q: %s", it.ID, msg)
		}
		ids = append(ids, it.ID)
	}
	if r.Shadow {
		return KindUpsertResult{Accepted: len(items)}, h.journal(ctx, r, "kinds.upsert", kindKey, kindInverse{Op: "kinds.upsert", Kind: kindKey, Items: ids})
	}
	backends := h.m.kindBackends()
	if backends.KindUpsert == nil && backends.KindUpsertStats == nil {
		return KindUpsertResult{}, ErrUnavailable
	}
	prev, created := h.snapshotKindItems(ctx, kindKey, ids)
	var result KindUpsertResult
	if backends.KindUpsertStats != nil {
		result, err = backends.KindUpsertStats(ctx, h.plugin, kindKey, items)
	} else {
		var accepted int
		accepted, err = backends.KindUpsert(ctx, h.plugin, kindKey, items)
		result = KindUpsertResult{Accepted: accepted}
	}
	if err != nil {
		return KindUpsertResult{}, err
	}
	if err := h.journal(ctx, r, "kinds.upsert", kindKey, kindInverse{
		Op:       "kinds.upsert",
		Kind:     kindKey,
		Items:    created,
		Previous: prev,
	}); err != nil {
		return KindUpsertResult{}, err
	}
	return result, nil
}

func (h *host) KindDelete(ctx context.Context, kindKey string, externalIDs []string) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	r, decl, err := h.kindCheck(ctx, kindKey, CapCatalogProvider)
	if err != nil {
		return err
	}
	if decl.Sync.Mode != KindSyncPush {
		return fmt.Errorf("%w: kind %s is pull-synced by Ether and takes no pushes", ErrPermissionDenied, kindKey)
	}
	if len(externalIDs) > MaxKindUpsertItems {
		return fmt.Errorf("plugins: kind delete batch larger than %d items", MaxKindUpsertItems)
	}
	for _, id := range externalIDs {
		if !ValidExternalID(id) {
			return fmt.Errorf("plugins: kind item id %q is not a valid external id", id)
		}
	}
	if r.Shadow {
		return h.journal(ctx, r, "kinds.delete", kindKey, kindInverse{Op: "kinds.delete", Kind: kindKey, Items: externalIDs})
	}
	backend := h.m.kindBackends().KindDelete
	if backend == nil {
		return ErrUnavailable
	}
	prev, _ := h.snapshotKindItems(ctx, kindKey, externalIDs)
	if err := backend(ctx, h.plugin, kindKey, externalIDs); err != nil {
		return err
	}
	return h.journal(ctx, r, "kinds.delete", kindKey, kindInverse{
		Op:       "kinds.delete",
		Kind:     kindKey,
		Items:    externalIDs,
		Previous: prev,
	})
}

func (h *host) KindItems(ctx context.Context, kindKey, cursor string, limit int) (core.KindPage, error) {
	if _, _, err := h.kindCheck(ctx, kindKey, CapCatalogProvider); err != nil {
		return core.KindPage{}, err
	}
	backend := h.m.kindBackends().KindItems
	if backend == nil {
		return core.KindPage{}, ErrUnavailable
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > MaxKindUpsertItems {
		limit = MaxKindUpsertItems
	}
	return backend(ctx, h.plugin, kindKey, cursor, limit)
}

// KindVisible evaluates the library's Rules and viewer access policy for a
// bounded batch (max MaxKindVisibleIDs) of external IDs in kindKey (#615).
// Only frontend adapter requests carrying an authenticated viewer may call it;
// the viewer comes solely from the host context, never from guest input.
func (h *host) KindVisible(ctx context.Context, kindKey string, ids []string) ([]string, error) {
	if _, _, err := h.kindCheck(ctx, kindKey, CapFrontendAdapter); err != nil {
		return nil, err
	}
	viewerID, ok := ViewerFromContext(ctx)
	if !ok || viewerID <= 0 {
		return nil, fmt.Errorf("%w: kinds.visible requires an authenticated frontend adapter viewer", ErrPermissionDenied)
	}
	if len(ids) > MaxKindVisibleIDs {
		return nil, &ValidationError{Fields: []FieldError{{
			Field:   "ids",
			Message: fmt.Sprintf("kinds.visible accepts at most %d ids per call", MaxKindVisibleIDs),
		}}}
	}
	if len(ids) == 0 {
		return []string{}, nil
	}
	backend := h.m.kindBackends().KindVisible
	if backend == nil {
		return nil, ErrUnavailable
	}
	visible, err := backend(ctx, h.plugin, viewerID, kindKey, ids)
	if err != nil {
		return nil, err
	}
	if visible == nil {
		visible = []string{}
	}
	return visible, nil
}

// KindBrowse browses or searches kindKey using the same LibraryPolicy path
// as Ether's console and protocol projectors for the requesting
// frontend-adapter viewer (#615). Only frontend adapter requests carrying an
// authenticated viewer may call it; the viewer comes solely from the host
// context, never from guest input.
func (h *host) KindBrowse(ctx context.Context, kindKey string, q core.KindBrowseRequest) (core.KindPage, error) {
	if _, _, err := h.kindCheck(ctx, kindKey, CapFrontendAdapter); err != nil {
		return core.KindPage{}, err
	}
	viewerID, ok := ViewerFromContext(ctx)
	if !ok || viewerID <= 0 {
		return core.KindPage{}, fmt.Errorf("%w: kinds.browse requires an authenticated frontend adapter viewer", ErrPermissionDenied)
	}
	if q.ParentID != "" && !ValidExternalID(q.ParentID) {
		return core.KindPage{}, &ValidationError{Fields: []FieldError{{
			Field:   "parentId",
			Message: "parentId must be a valid external id",
		}}}
	}
	if q.Limit <= 0 {
		q.Limit = 200
	}
	if q.Limit > MaxKindUpsertItems {
		q.Limit = MaxKindUpsertItems
	}
	backend := h.m.kindBackends().KindBrowse
	if backend == nil {
		return core.KindPage{}, ErrUnavailable
	}
	page, err := backend(ctx, h.plugin, viewerID, kindKey, q)
	if err != nil {
		return core.KindPage{}, err
	}
	if page.Items == nil {
		page.Items = []core.KindItem{}
	}
	return page, nil
}

func (h *host) ResolveSources(ctx context.Context, q core.SourceQuery) ([]core.SourceCandidate, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	man := r.Manifest
	man.Ether = man.EffectiveEther()
	if !man.HasCapability(CapKindResolver) {
		h.noteDenied(string(CapKindResolver))
		return nil, fmt.Errorf("%w: %s", ErrPermissionDenied, CapKindResolver)
	}
	need := "search"
	switch q.Kind {
	case "indexer_search":
		if strings.TrimSpace(q.Query) == "" && strings.TrimSpace(q.Category) == "" {
			return nil, &ValidationError{Fields: []FieldError{{
				Field:   "query",
				Message: "indexer_search needs a query or category",
			}}}
		}
	case "debrid_check":
		if len(q.InfoHashes) == 0 && q.InfoHash == "" {
			return nil, &ValidationError{Fields: []FieldError{{
				Field:   "infoHashes",
				Message: "debrid_check needs an info hash",
			}}}
		}
	case "release_files":
		if q.InfoHash == "" && q.Magnet == "" && q.Candidate == nil {
			return nil, &ValidationError{Fields: []FieldError{{
				Field:   "infoHash",
				Message: "release_files needs an info hash, magnet or candidate",
			}}}
		}
	case "debrid_acquire":
		need = "acquire"
		if q.InfoHash == "" && q.Magnet == "" {
			return nil, &ValidationError{Fields: []FieldError{{
				Field:   "infoHash",
				Message: "debrid_acquire needs an info hash or magnet",
			}}}
		}
	default:
		return nil, &ValidationError{Fields: []FieldError{{
			Field:   "kind",
			Message: fmt.Sprintf("unknown source query kind %q", q.Kind),
		}}}
	}
	level := normLevel(r.Manifest.Ether.Permissions.Sources)
	if err := h.deny(r, g, "sources", level, func(l string) bool {
		return l == "acquire" || (l == "search" && need == "search")
	}); err != nil {
		return nil, err
	}
	backend := h.m.kindBackends().ResolveSources
	if backend == nil {
		return nil, ErrUnavailable
	}
	return backend(ctx, h.plugin, q)
}

func (h *host) CachePut(ctx context.Context, libraryKey, name string, r io.Reader) (string, error) {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return "", err
	}
	decl, err := h.cacheWriteCheck(ctx, libraryKey, name)
	if err != nil {
		return "", err
	}
	backend := h.m.kindBackends().CachePut
	if backend == nil {
		return "", ErrUnavailable
	}
	// A single blob can never exceed the whole per-library quota.
	limited := io.LimitReader(r, decl.Cache.EffectiveMaxBytes()+1)
	return backend(ctx, h.plugin, libraryKey, name, limited)
}

// viewerContextKey carries the authenticated viewer of a proxied request
// into a plugin call, so a host service can act as that viewer without the
// plugin passing (or being trusted with) the identity.
type viewerContextKey struct{}

// WithViewer attaches a viewer id to a plugin call context.
func WithViewer(ctx context.Context, viewerID int64) context.Context {
	return context.WithValue(ctx, viewerContextKey{}, viewerID)
}

// ViewerFromContext returns the viewer attached by WithViewer, if any.
func ViewerFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(viewerContextKey{}).(int64)
	return id, ok
}

type adapterViewerGrant struct {
	pluginID string
	viewerID int64
	expires  time.Time
}

func isFrontendAdapterPath(path string) bool {
	return strings.HasPrefix(path, "/ether/v1/frontend/") && len(path) > len("/ether/v1/frontend/")
}

// bindAdapterViewer attaches the authenticated viewer from X-Ether-Viewer to
// ctx and mints a request-scoped X-Ether-Viewer-Token for remote callbacks,
// strictly on /ether/v1/frontend/* adapter calls. Non-adapter calls never
// receive a viewer context or token, and any pre-existing token header is
// stripped before binding.
func (m *Manager) bindAdapterViewer(ctx context.Context, pluginID string, req *Request, deadline time.Duration) (context.Context, func()) {
	if req != nil && req.Headers != nil {
		for k := range req.Headers {
			if strings.EqualFold(k, HeaderAdapterViewerToken) {
				delete(req.Headers, k)
			}
		}
	}
	if req == nil || !isFrontendAdapterPath(req.Path) || req.Headers == nil {
		return ctx, func() {}
	}
	raw := req.Headers["X-Ether-Viewer"]
	if raw == "" {
		return ctx, func() {}
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return ctx, func() {}
	}
	ctx = WithViewer(ctx, id)
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ctx, func() {}
	}
	token := hex.EncodeToString(b[:])
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	ttl := deadline
	if ttl < time.Minute {
		ttl = time.Minute
	}
	m.adapterViewerMu.Lock()
	if m.adapterViewerTokens == nil {
		m.adapterViewerTokens = make(map[string]adapterViewerGrant)
	}
	for k, v := range m.adapterViewerTokens {
		if !now.Before(v.expires) {
			delete(m.adapterViewerTokens, k)
		}
	}
	m.adapterViewerTokens[token] = adapterViewerGrant{
		pluginID: pluginID,
		viewerID: id,
		expires:  now.Add(ttl),
	}
	m.adapterViewerMu.Unlock()
	req.Headers[HeaderAdapterViewerToken] = token
	return ctx, func() { m.revokeAdapterViewerToken(token) }
}

func (m *Manager) revokeAdapterViewerToken(token string) {
	if token == "" {
		return
	}
	m.adapterViewerMu.Lock()
	delete(m.adapterViewerTokens, token)
	m.adapterViewerMu.Unlock()
}

// ResolveAdapterViewerToken verifies that token belongs to an active
// /ether/v1/frontend/* call for pluginID and returns its authenticated viewer.
func (m *Manager) ResolveAdapterViewerToken(pluginID, token string) (int64, bool) {
	token = strings.TrimSpace(token)
	if pluginID == "" || token == "" {
		return 0, false
	}
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	m.adapterViewerMu.Lock()
	defer m.adapterViewerMu.Unlock()
	grant, ok := m.adapterViewerTokens[token]
	if !ok {
		return 0, false
	}
	if !now.Before(grant.expires) {
		delete(m.adapterViewerTokens, token)
		return 0, false
	}
	if grant.pluginID != pluginID || grant.viewerID <= 0 {
		return 0, false
	}
	return grant.viewerID, true
}

// StreamURL mints a scoped stream ticket for one of the plugin's own kind
// items and returns the Ether stream URL (spec §6.5). The viewer is the one
// the proxied request authenticated, carried on the call context as
// X-Ether-Viewer. The backend re-checks that the item belongs to this plugin
// and that the viewer may see its library.
func (h *host) StreamURL(ctx context.Context, itemRef string) (string, error) {
	return h.StreamPartURL(ctx, itemRef, nil)
}

func (h *host) StreamPartURL(ctx context.Context, itemRef string, partIndex *int) (string, error) {
	rec, _, err := h.record(ctx)
	if err != nil {
		return "", err
	}
	man := rec.Manifest
	man.Ether = man.EffectiveEther()
	if man.Ether == nil || !man.HasCapability(CapFrontendAdapter) {
		h.noteDenied(string(CapFrontendAdapter))
		return "", fmt.Errorf("%w: %s", ErrPermissionDenied, CapFrontendAdapter)
	}
	viewerID, ok := ViewerFromContext(ctx)
	if !ok || viewerID <= 0 {
		return "", fmt.Errorf("%w: stream url needs an authenticated viewer", ErrPermissionDenied)
	}
	if strings.TrimSpace(itemRef) == "" {
		return "", fmt.Errorf("plugins: stream url needs an item ref")
	}
	if partIndex != nil && *partIndex < 0 {
		return "", fmt.Errorf("plugins: stream url partIndex must be non-negative")
	}
	backends := h.m.kindBackends()
	if backends.StreamPartURL != nil {
		return backends.StreamPartURL(ctx, h.plugin, viewerID, itemRef, partIndex)
	}
	if backends.StreamURL == nil {
		return "", ErrUnavailable
	}
	u, err := backends.StreamURL(ctx, h.plugin, viewerID, itemRef)
	if err != nil || partIndex == nil {
		return u, err
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "part=" + strconv.Itoa(*partIndex), nil
}

func validateBlobRef(blobRef string) error {
	hexPart, ok := strings.CutPrefix(blobRef, "blob:")
	if !ok || len(hexPart) != 64 {
		return &ValidationError{Fields: []FieldError{{Field: "blobRef", Message: "cache reference must be blob:<sha256>"}}}
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return &ValidationError{Fields: []FieldError{{Field: "blobRef", Message: "cache reference must be blob:<sha256>"}}}
	}
	return nil
}

func (h *host) CacheOpen(ctx context.Context, blobRef string) (io.ReadSeekCloser, error) {
	r, err := h.callRecord(ctx)
	if err != nil {
		return nil, err
	}
	if !r.Enabled {
		return nil, ErrDisabled
	}
	if err := validateBlobRef(blobRef); err != nil {
		return nil, err
	}
	backend := h.m.kindBackends().CacheOpen
	if backend == nil {
		return nil, ErrUnavailable
	}
	return backend(ctx, h.plugin, blobRef)
}

// CacheDelete removes one cached blob from this plugin's own library cache
// namespaces, freeing its quota immediately. Deleting a ref that is absent or
// already evicted succeeds.
func (h *host) CacheDelete(ctx context.Context, blobRef string) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	rec, _, err := h.record(ctx)
	if err != nil {
		return err
	}
	man := rec.Manifest
	man.Ether = man.EffectiveEther()
	if man.Ether == nil || len(man.Ether.Contributes.MediaKinds) == 0 || !(man.HasCapability(CapCatalogProvider) || man.HasCapability(CapKindResolver)) {
		h.noteDenied("cache")
		return fmt.Errorf("%w: cache", ErrPermissionDenied)
	}
	if err := validateBlobRef(blobRef); err != nil {
		return err
	}
	backend := h.m.kindBackends().CacheDelete
	if backend == nil {
		return ErrUnavailable
	}
	return backend(ctx, h.plugin, blobRef)
}

func (h *host) cacheWriteCheck(ctx context.Context, libraryKey, name string) (MediaKindMeta, error) {
	rec, _, err := h.record(ctx)
	if err != nil {
		return MediaKindMeta{}, err
	}
	man := rec.Manifest
	man.Ether = man.EffectiveEther()
	// The library key must name one of this plugin's own kinds; the
	// blob cache namespace follows from it, so a plugin can never
	// write into another library's cache.
	var decl MediaKindMeta
	found := false
	if man.Ether != nil {
		for _, k := range man.Ether.Contributes.MediaKinds {
			if core.PluginLibraryKey(h.plugin, k.Key) == libraryKey {
				decl, found = k, true
			}
		}
	}
	if !found || !(man.HasCapability(CapCatalogProvider) || man.HasCapability(CapKindResolver)) {
		h.noteDenied("cache")
		return MediaKindMeta{}, fmt.Errorf("%w: cache", ErrPermissionDenied)
	}
	if strings.TrimSpace(name) == "" {
		return MediaKindMeta{}, fmt.Errorf("plugins: cache put needs a name")
	}
	return decl, nil
}

func (h *host) archiveExtractCheck(ctx context.Context, explicitLibraryKey string) (string, error) {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return "", err
	}
	if explicit := strings.TrimSpace(explicitLibraryKey); explicit != "" {
		if _, err := h.cacheWriteCheck(ctx, explicit, "_"); err != nil {
			return "", err
		}
		return explicit, nil
	}
	rec, _, err := h.record(ctx)
	if err != nil {
		return "", err
	}
	man := rec.Manifest
	man.Ether = man.EffectiveEther()
	if man.Ether == nil || len(man.Ether.Contributes.MediaKinds) == 0 || !(man.HasCapability(CapCatalogProvider) || man.HasCapability(CapKindResolver)) {
		h.noteDenied("cache")
		return "", fmt.Errorf("%w: cache", ErrPermissionDenied)
	}
	return core.PluginLibraryKey(h.plugin, man.Ether.Contributes.MediaKinds[0].Key), nil
}

func (h *host) resolveExtractTarget(ctx context.Context, explicitLibraryKey, sourceLibraryKey, name string) (string, int64, error) {
	if explicit := strings.TrimSpace(explicitLibraryKey); explicit != "" {
		decl, err := h.cacheWriteCheck(ctx, explicit, name)
		if err != nil {
			return "", 0, err
		}
		return explicit, decl.Cache.EffectiveMaxBytes(), nil
	}
	if src := strings.TrimSpace(sourceLibraryKey); src != "" {
		if decl, err := h.cacheWriteCheck(ctx, src, name); err == nil {
			return src, decl.Cache.EffectiveMaxBytes(), nil
		}
	}
	libKey, err := h.archiveExtractCheck(ctx, "")
	if err != nil {
		return "", 0, err
	}
	decl, err := h.cacheWriteCheck(ctx, libKey, name)
	if err != nil {
		return "", 0, err
	}
	return libKey, decl.Cache.EffectiveMaxBytes(), nil
}
