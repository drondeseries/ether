package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/reqmemo"
)

const (
	defaultKindRefresh       = time.Hour
	defaultLiveSearchTimeout = 2 * time.Second
	maxLiveSearchPerPlugin   = 2
	liveSearchBackgroundWait = 10 * time.Second
)

// KindCatalogDeps configures a KindCatalog adapter.
type KindCatalogDeps struct {
	TitleInLibrary func(context.Context, core.Library, core.RatingKey) (bool, error)

	Manager           *plugins.Manager
	Libraries         *libraries.Service
	Items             *pluginitems.Service
	Blobs             *blobcache.Cache
	BlobDir           string
	Logger            *slog.Logger
	LiveSearchTimeout time.Duration
}

type liveSearchCall struct {
	done  chan struct{}
	items []core.KindItem
}

// KindCatalog bridges plugin media-kind declarations and lifecycle events to
// the libraries, pluginitems and blobcache stores, and implements
// experience.PluginLibraries so client surfaces read exclusively from the
// local store on browse, detail, children and facet paths.
type KindCatalog struct {
	titleInLibrary func(context.Context, core.Library, core.RatingKey) (bool, error)

	syncCtx           context.Context
	mgr               *plugins.Manager
	libs              *libraries.Service
	items             *pluginitems.Service
	blobs             *blobcache.Cache
	blobDir           string
	log               *slog.Logger
	liveSearchTimeout time.Duration

	quotaMu sync.RWMutex
	quotas  map[string]blobcache.Quota

	liveMu    sync.Mutex
	liveCalls map[string]*liveSearchCall
	liveSems  map[string]chan struct{}
}

var (
	_ experience.PluginLibraries             = (*KindCatalog)(nil)
	_ experience.PluginPeopleLibraries       = (*KindCatalog)(nil)
	_ experience.PluginPresentationLibraries = (*KindCatalog)(nil)
)

// AttachKindCatalog constructs a KindCatalog and registers its OnSync and
// OnRemove lifecycle hooks on deps.Manager. Call before Manager.Start so
// already-installed kind plugins are reconciled on startup.
func AttachKindCatalog(syncCtx context.Context, deps KindCatalogDeps) *KindCatalog {
	if syncCtx == nil {
		syncCtx = context.Background()
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	timeout := deps.LiveSearchTimeout
	if timeout <= 0 {
		timeout = defaultLiveSearchTimeout
	}
	c := &KindCatalog{
		syncCtx:           syncCtx,
		titleInLibrary:    deps.TitleInLibrary,
		mgr:               deps.Manager,
		libs:              deps.Libraries,
		items:             deps.Items,
		blobs:             deps.Blobs,
		blobDir:           deps.BlobDir,
		log:               log.With("component", "kindcatalog"),
		liveSearchTimeout: timeout,
		quotas:            make(map[string]blobcache.Quota),
		liveCalls:         make(map[string]*liveSearchCall),
		liveSems:          make(map[string]chan struct{}),
	}
	if deps.Manager != nil {
		deps.Manager.OnSync(c.syncPlugin)
		deps.Manager.OnRemove(c.removePlugin)
	}
	return c
}

// QuotaFor returns the declared blobcache quota for (pluginID, libraryKey),
// used by blobcache.Cache.HostBackends and KindResolver.
func (c *KindCatalog) QuotaFor(pluginID, libraryKey string) blobcache.Quota {
	if c == nil {
		return blobcache.Quota{}
	}
	c.quotaMu.RLock()
	q, ok := c.quotas[pluginID+"\x00"+libraryKey]
	c.quotaMu.RUnlock()
	if ok {
		return q
	}
	return blobcache.Quota{}
}

func (c *KindCatalog) setQuota(pluginID, libraryKey string, decl plugins.KindCacheDecl) blobcache.Quota {
	q := blobcache.Quota{
		MaxBytes: decl.MaxBytes,
		TTL:      parseKindDuration(decl.TTL, 0),
	}
	c.quotaMu.Lock()
	c.quotas[pluginID+"\x00"+libraryKey] = q
	c.quotaMu.Unlock()
	return q
}

func (c *KindCatalog) deleteQuota(pluginID, libraryKey string) {
	c.quotaMu.Lock()
	delete(c.quotas, pluginID+"\x00"+libraryKey)
	c.quotaMu.Unlock()
}

// RefreshPlugin reconciles an explicitly restored library with current setup
// and enabled state, including restarting its background synchronization.
func (c *KindCatalog) RefreshPlugin(ctx context.Context, r plugins.Record) { c.syncPlugin(ctx, r) }

func (c *KindCatalog) syncPlugin(ctx context.Context, r plugins.Record) {
	if c == nil || c.items == nil || c.libs == nil {
		return
	}
	var declared []plugins.MediaKindMeta
	if e := r.Manifest.EffectiveEther(); e != nil {
		for _, k := range e.Contributes.MediaKinds {
			if k.HasLibrary() {
				declared = append(declared, k)
			}
		}
	}
	existing, err := c.items.ListKinds(ctx, r.ID)
	if err != nil {
		c.log.Warn("list plugin kinds failed", "plugin", r.ID, "err", err)
		return
	}
	// Binding owner rule: plugins without any library-declaring mediaKinds
	// never touch the libraries store or create blobcache namespaces.
	if len(declared) == 0 && len(existing) == 0 {
		return
	}

	active := c.recordAllowsCatalogSync(ctx, r)
	oldByKey := make(map[string]pluginitems.KindRecord, len(existing))
	for _, old := range existing {
		oldByKey[old.KindKey] = old
	}
	keep := make(map[string]bool, len(declared))
	for _, k := range declared {
		keep[k.Key] = true
		libName := c.resolvePluginText(ctx, r.ID, k.Library.Name)
		if strings.TrimSpace(libName) == "" {
			libName = k.Key
		}
		lib, err := c.libs.UpsertPluginLibrary(ctx, r.ID, k.Key, libraries.PluginLibraryDecl{
			Name:    libName,
			Present: k.Present,
		})
		if errors.Is(err, libraries.ErrDeleted) {
			c.items.StopKindSync(r.ID, k.Key)
			continue
		}
		if err != nil {
			c.log.Warn("upsert plugin library failed", "plugin", r.ID, "kind", k.Key, "err", err)
			continue
		}
		// A plugin update that changes the declared presentation re-applies
		// it to the kind's existing library; an unchanged declaration leaves
		// the stored presentation (and any owner edit of it) alone. Levels
		// and fields already flow through UpsertKind below, which stores the
		// new declaration and reindexes only when the index shape changed.
		if old, ok := oldByKey[k.Key]; ok && !presentationsEqual(old.Decl.Present, k.Present) {
			next := k.Present
			if next.Vela == nil {
				// Keep the Vela publication chosen (or inferred) at
				// install when the manifest stays silent about it.
				next.Vela = lib.Present.Vela
			}
			if err := c.libs.SetPresent(ctx, lib.ID, next); err != nil {
				c.log.Warn("re-apply plugin library presentation failed", "plugin", r.ID, "kind", k.Key, "err", err)
			} else {
				lib.Present = next
			}
		}
		q := c.setQuota(r.ID, lib.Key, k.Cache)
		if c.blobs != nil {
			c.blobs.BindPluginLibrary(r.ID, lib.Key)
			if c.blobNamespaceExistsOnDisk(lib.Key) {
				c.blobs.Namespace(lib.Key, q)
			}
		}
		if _, err := c.items.UpsertKind(ctx, r.ID, lib.ID, k, false); err != nil {
			c.log.Warn("upsert plugin kind failed", "plugin", r.ID, "kind", k.Key, "err", err)
			if c.mgr != nil {
				c.mgr.RecordFailure(ctx, r.ID, err.Error())
			}
			c.items.StopKindSync(r.ID, k.Key)
			continue
		}
		if active && k.Sync.Mode == plugins.KindSyncPull {
			interval := parseKindDuration(k.Sync.Refresh, defaultKindRefresh)
			c.items.StartKindSync(c.syncCtx, r.ID, k.Key, interval)
		} else {
			c.items.StopKindSync(r.ID, k.Key)
		}
	}

	// Purge any previously installed kind whose declaration or library block
	// was removed on update (pluginitems -> libraries -> blobcache).
	for _, old := range existing {
		if keep[old.KindKey] {
			continue
		}
		c.items.StopKindSync(r.ID, old.KindKey)
		var oldKey string
		if oldLib, err := c.libs.Retained(ctx, old.LibraryID); err == nil {
			oldKey = oldLib.Key
		}
		if err := c.items.PurgeKind(ctx, r.ID, old.KindKey); err != nil {
			c.log.Warn("purge removed kind failed", "plugin", r.ID, "kind", old.KindKey, "err", err)
		}
		if err := c.libs.Purge(ctx, old.LibraryID); err != nil && !errors.Is(err, core.ErrNotFound) {
			c.log.Warn("delete removed kind library failed", "plugin", r.ID, "kind", old.KindKey, "err", err)
		}
		if oldKey != "" {
			c.deleteQuota(r.ID, oldKey)
			c.purgeBlobNamespace(oldKey)
		}
	}
}

// presentationsEqual compares two declared presentations by their JSON
// shape, the form both the manifest and the library store use.
func presentationsEqual(a, b core.Presentations) bool {
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(aj) == string(bj)
}

func (c *KindCatalog) removePlugin(ctx context.Context, pluginID string) {
	if c == nil || c.items == nil {
		return
	}
	c.items.StopPluginSync(pluginID)
	existing, err := c.items.ListKinds(ctx, pluginID)
	if err != nil {
		c.log.Warn("list kinds on remove failed", "plugin", pluginID, "err", err)
	}
	// Non-kind plugins (or kinds that never declared a library) leave
	// libraries and blobcache completely untouched.
	if len(existing) == 0 {
		return
	}

	var libIDs []core.LibraryID
	var libKeys []string
	seenID := make(map[core.LibraryID]bool, len(existing))
	if c.libs != nil {
		for _, rec := range existing {
			if rec.LibraryID > 0 && !seenID[rec.LibraryID] {
				seenID[rec.LibraryID] = true
				libIDs = append(libIDs, rec.LibraryID)
				if lib, err := c.libs.Retained(ctx, rec.LibraryID); err == nil && lib.Key != "" {
					libKeys = append(libKeys, lib.Key)
				}
			}
		}
	}

	// Spec §2.5 removal order: pluginitems -> libraries -> blobcache.
	if err := c.items.PurgePlugin(ctx, pluginID); err != nil {
		c.log.Warn("purge plugin items failed", "plugin", pluginID, "err", err)
	}
	if c.libs != nil {
		for _, id := range libIDs {
			if err := c.libs.Purge(ctx, id); err != nil && !errors.Is(err, core.ErrNotFound) {
				c.log.Warn("delete plugin library failed", "plugin", pluginID, "library", id, "err", err)
			}
		}
	}
	for _, key := range libKeys {
		c.deleteQuota(pluginID, key)
		c.purgeBlobNamespace(key)
	}
}

func (c *KindCatalog) blobNamespaceExistsOnDisk(libKey string) bool {
	if c.blobDir == "" || libKey == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(c.blobDir, libKey))
	return err == nil && info.IsDir()
}

func (c *KindCatalog) purgeBlobNamespace(libKey string) {
	if c.blobs == nil || libKey == "" {
		return
	}
	if c.blobDir != "" && !c.blobNamespaceExistsOnDisk(libKey) {
		return
	}
	c.blobs.Namespace(libKey, blobcache.Quota{}).Purge()
}

func (c *KindCatalog) resolvePluginText(ctx context.Context, pluginID, val string) string {
	val = strings.TrimSpace(val)
	key, ok := strings.CutPrefix(val, "$t:")
	if !ok || c.mgr == nil {
		return val
	}
	if resolved, ok := c.mgr.ResolveText(ctx, pluginID, key, "en"); ok && strings.TrimSpace(resolved) != "" {
		return resolved
	}
	return key
}

func (c *KindCatalog) recordAllowsCatalogSync(ctx context.Context, r plugins.Record) bool {
	if !r.Enabled || !r.Manifest.HasCapability(plugins.CapCatalogProvider) {
		return false
	}
	if c.mgr != nil {
		blockers, err := c.mgr.SetupBlockers(ctx, r.ID)
		if err != nil || len(blockers) > 0 {
			return false
		}
	}
	return true
}

// Libraries implements experience.PluginLibraries.
func (c *KindCatalog) Libraries(ctx context.Context) ([]core.Library, error) {
	if c == nil || c.libs == nil {
		return nil, nil
	}
	return c.libs.List(ctx)
}

// Library implements experience.PluginLibraries.
func (c *KindCatalog) Library(ctx context.Context, id core.LibraryID) (core.Library, error) {
	if c == nil || c.libs == nil {
		return core.Library{}, core.ErrNotFound
	}
	return c.libs.Get(ctx, id)
}

// IsPluginLibraryActive reports whether lib's owning plugin is installed,
// enabled, has catalog_provider active (not disabled via RolesOff), declares a
// library for KindKey, and has no required setup blockers. Runtime crash state
// intentionally does NOT hide the library so synced items remain browsable
// offline when a plugin fails at runtime.
func (c *KindCatalog) IsPluginLibraryActive(ctx context.Context, lib core.Library) bool {
	if c == nil || c.mgr == nil || lib.Source != core.LibraryPlugin || lib.PluginID == "" || lib.KindKey == "" {
		return false
	}
	// A page may check many items from the same plugin library. Remember
	// missing/disabled providers too: a missing plugin must not cost one SQL
	// lookup per candidate. The read-request memo expires after two seconds;
	// background and write contexts still recheck the current record each time.
	active, _ := reqmemo.Do(ctx, fmt.Sprintf("plugin-library-active:%p:%v:%s:%s", c, lib.ID, lib.PluginID, lib.KindKey), func() (bool, error) {
		r, err := c.mgr.Get(ctx, lib.PluginID)
		if errors.Is(err, plugins.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !c.recordAllowsCatalogSync(ctx, r) {
			return false, nil
		}
		e := r.Manifest.EffectiveEther()
		if e == nil {
			return false, nil
		}
		for _, k := range e.Contributes.MediaKinds {
			if k.Key == lib.KindKey {
				return k.HasLibrary(), nil
			}
		}
		return false, nil
	})
	return active
}

// RootMediaType implements experience.PluginLibraries.
func (c *KindCatalog) RootMediaType(ctx context.Context, lib core.Library) core.MediaType {
	if c == nil || c.items == nil {
		return core.TypeMovie
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil || len(rec.Decl.Levels) == 0 {
		return core.TypeMovie
	}
	proj := pluginitems.Project(core.KindItem{Level: rec.Decl.Levels[0].Key}, rec.Decl, lib.Present, lib.CardShape)
	if proj.Type == "" {
		return core.TypeMovie
	}
	return proj.Type
}

// Count implements experience.PluginLibraries.
func (c *KindCatalog) Count(ctx context.Context, lib core.Library) (int, error) {
	if c == nil || c.items == nil {
		return 0, nil
	}
	policy, err := c.items.LibraryPolicy(ctx, lib)
	if err != nil {
		return 0, err
	}
	if !policy.Passive() {
		page, err := c.items.Browse(ctx, pluginitems.BrowseQuery{LibraryID: lib.ID, Policy: policy, Limit: 1})
		return page.Total, err
	}
	tally, err := c.items.LibraryTally(ctx, lib.ID)
	if err != nil {
		return 0, err
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err == nil && len(rec.Decl.Levels) > 0 {
		return tally[rec.Decl.Levels[0].Key], nil
	}
	total := 0
	for _, v := range tally {
		total += v
	}
	return total, nil
}

// Browse implements experience.PluginLibraries. It reads strictly from the
// pluginitems store and never invokes the plugin runtime.
func (c *KindCatalog) Browse(ctx context.Context, lib core.Library, q frontend.ItemQuery, cursor string, limit int) (experience.PluginBrowsePage, error) {
	if c == nil || c.items == nil {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	if q.Filter.Person != 0 && !core.IsPluginPersonID(q.Filter.Person) {
		return experience.PluginBrowsePage{Items: []core.Title{}, Total: 0}, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return experience.PluginBrowsePage{}, err
	}
	level, levels, anyLevel, matched := browseLevelsForQuery(rec.Decl, lib.Present, q)
	if !matched {
		return experience.PluginBrowsePage{Items: []core.Title{}, Total: 0}, nil
	}
	policy, err := c.items.LibraryPolicy(ctx, lib)
	if err != nil {
		return experience.PluginBrowsePage{}, err
	}
	bq := pluginitems.BrowseQuery{
		NoTotal:          q.NoTotal,
		Policy:           policy,
		LibraryID:        lib.ID,
		ViewerID:         core.CatalogViewerID(ctx),
		Level:            level,
		Levels:           levels,
		AnyLevel:         anyLevel,
		PersonID:         q.Filter.Person,
		Sort:             mapBrowseSort(q.Sort, q.Desc),
		Facets:           mapFilterToFacets(rec.Decl, q.Filter),
		ExcludeFacets:    mapExcludeGenresToFacets(rec.Decl, q.Filter.ExcludeGenres),
		Watched:          q.Watched,
		MaxContentRating: q.Filter.MaxContentRating,
		ExcludeKeys:      q.Filter.ExcludeKeys,
		Cursor:           cursor,
		Limit:            limit,
	}
	if len(q.Filter.TabRules) > 0 || q.Filter.TabFallback != "" {
		tp, tpErr := c.items.CompilePolicy(ctx, lib.ID, q.Filter.TabRules, q.Filter.TabFallback)
		if tpErr != nil {
			return experience.PluginBrowsePage{}, tpErr
		}
		bq.TabPolicy = tp
	}
	page, err := c.items.Browse(ctx, bq)
	if err != nil {
		return experience.PluginBrowsePage{}, err
	}
	titles := c.projectItems(ctx, rec, lib, page.Items)
	if flatAudioQuery(rec.Decl, lib.Present, q) {
		for i, item := range page.Items {
			if item.Playable && item.Unavailable == nil {
				titles[i] = flatAlbumIncarnation(titles[i])
			}
		}
	}
	return experience.PluginBrowsePage{
		Items:      titles,
		Total:      page.Total,
		NextCursor: page.NextCursor,
	}, nil
}

// Item implements experience.PluginLibraries.
func (c *KindCatalog) Item(ctx context.Context, key core.RatingKey) (experience.PluginItemView, error) {
	if c == nil || c.items == nil || c.libs == nil || !key.IsPluginKey() {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	item, libID, err := c.items.GetByKey(ctx, key)
	if err != nil {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	lib, err := c.libs.Get(ctx, libID)
	if err != nil {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	rec, err := c.items.GetKindByLibrary(ctx, libID)
	if err != nil {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	hidden, e := c.items.ItemHidden(core.WithExplicitSearch(ctx), lib, item.Ref)
	if e != nil || hidden {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	titles := c.projectItems(ctx, rec, lib, []core.KindItem{item})
	return experience.PluginItemView{
		Library:     lib,
		Title:       titles[0],
		Images:      projectImages(item.Art, lib.EffectiveCardShape(rec.Decl.Library.EffectiveCard()) == core.CardLandscape),
		Unavailable: item.Unavailable,
		Playable:    item.Unavailable == nil && item.Playable && levelIsPlayable(rec.Decl, item.Level),
	}, nil
}

// ItemsMany implements experience.PluginLibraries using batched item, artwork,
// and visibility queries per library rather than one query per key.
func (c *KindCatalog) ItemsMany(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]experience.PluginItemView, error) {
	out := make(map[core.RatingKey]experience.PluginItemView, len(keys))
	if c == nil || c.items == nil || c.libs == nil || len(keys) == 0 {
		return out, nil
	}
	keyed, err := c.items.GetByKeys(ctx, keys)
	if err != nil || len(keyed) == 0 {
		return out, err
	}
	byLib := make(map[core.LibraryID][]core.KindItem)
	var libOrder []core.LibraryID
	seenKey := make(map[core.RatingKey]bool, len(keyed))
	for _, key := range keys {
		if !key.IsPluginKey() || seenKey[key] {
			continue
		}
		ki, ok := keyed[key]
		if !ok {
			continue
		}
		seenKey[key] = true
		if _, exists := byLib[ki.LibraryID]; !exists {
			libOrder = append(libOrder, ki.LibraryID)
		}
		byLib[ki.LibraryID] = append(byLib[ki.LibraryID], ki.Item)
	}

	visCtx := core.WithExplicitSearch(ctx)
	for _, libID := range libOrder {
		lib, err := c.libs.Get(ctx, libID)
		if err != nil {
			continue
		}
		rec, err := c.items.GetKindByLibrary(ctx, libID)
		if err != nil {
			continue
		}
		candidates := byLib[libID]
		refs := make([]core.ItemRef, len(candidates))
		for i, it := range candidates {
			refs[i] = it.Ref
		}
		visibleRefs, err := c.items.FilterVisibleRefs(visCtx, lib, refs)
		if err != nil {
			continue
		}
		visibleItems := make([]core.KindItem, 0, len(candidates))
		for _, it := range candidates {
			if visibleRefs[it.Ref] {
				visibleItems = append(visibleItems, it)
			}
		}
		if len(visibleItems) == 0 {
			continue
		}
		titles := c.projectItems(ctx, rec, lib, visibleItems)
		landscape := lib.EffectiveCardShape(rec.Decl.Library.EffectiveCard()) == core.CardLandscape
		for i, it := range visibleItems {
			out[it.Key] = experience.PluginItemView{
				Library:     lib,
				Title:       titles[i],
				Images:      projectImages(it.Art, landscape),
				Unavailable: it.Unavailable,
				Playable:    it.Unavailable == nil && it.Playable && levelIsPlayable(rec.Decl, it.Level),
			}
		}
	}
	return out, nil
}

// flatPlayableAlbum reports whether a kind is the single-level playable
// album shape a flat music plugin declares: one playable level typed as a
// container (MediaBrowser MusicAlbum, or Plex base artist). Clients of
// those protocols only ever play leaf items, so such an album is also
// presented once as its own Audio/track child (see flatAlbumIncarnation);
// Jellyfin's album play button and Songs tab, and Plex's album track list,
// all reach the playable item that way.
func flatPlayableAlbum(decl plugins.MediaKindMeta, present core.Presentations) bool {
	if len(decl.Levels) != 1 || !decl.Levels[0].Playable {
		return false
	}
	if present.MediaBrowser != nil && present.MediaBrowser.ItemTypes[decl.Levels[0].Key] == "MusicAlbum" {
		return true
	}
	return present.Plex != nil && present.Plex.Base == "artist"
}

// flatAlbumIncarnation is a flat playable album presented as its own single
// playable child: same item and Key, typed as the leaf (Audio), naming
// itself as its album.
func flatAlbumIncarnation(t core.Title) core.Title {
	t.SelfChild = true
	t.MBItemType = "Audio"
	t.ParentKey = t.Key
	t.ParentTitle = t.Title
	t.ChildCount = 0
	t.LeafCount = 0
	return t
}

// wantsMBType reports whether the query's IncludeItemTypes named the given
// MediaBrowser type (carried lowercased in UnservedTypes).
func wantsMBType(q frontend.ItemQuery, name string) bool {
	for _, kind := range q.UnservedTypes {
		if strings.EqualFold(kind, name) {
			return true
		}
	}
	return false
}

// excludesMBType reports whether the query excluded the given MediaBrowser
// type by name.
func excludesMBType(q frontend.ItemQuery, name string) bool {
	for _, excluded := range q.ExcludedTypes {
		if strings.EqualFold(string(excluded), name) {
			return true
		}
	}
	return false
}

// flatAudioQuery reports whether q asks for Audio items a flat playable
// album kind answers with its albums' Audio incarnations (Jellyfin's Songs
// tab), without also asking for the albums themselves.
func flatAudioQuery(decl plugins.MediaKindMeta, present core.Presentations, q frontend.ItemQuery) bool {
	return flatPlayableAlbum(decl, present) && wantsMBType(q, "Audio") &&
		!wantsMBType(q, "MusicAlbum") && !excludesMBType(q, "Audio")
}

// Children implements experience.PluginLibraries.
func (c *KindCatalog) Children(ctx context.Context, parent core.RatingKey, recursive bool, cursor string, limit int) (experience.PluginBrowsePage, error) {
	if c == nil || c.items == nil || c.libs == nil || !parent.IsPluginKey() {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	parentItem, libID, err := c.items.GetByKey(ctx, parent)
	if err != nil {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	lib, err := c.libs.Get(ctx, libID)
	if err != nil {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	rec, err := c.items.GetKindByLibrary(ctx, libID)
	if err != nil {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	_, levelIdx, _ := rec.Decl.Level(parentItem.Level)
	if !recursive || levelIdx < 0 || levelIdx >= len(rec.Decl.Levels)-2 {
		policy, e := c.items.LibraryPolicy(ctx, lib)
		if e != nil {
			return experience.PluginBrowsePage{}, e
		}
		page, err := c.items.Browse(ctx, pluginitems.BrowseQuery{LibraryID: lib.ID, ParentRef: parentItem.Ref, Cursor: cursor, Limit: limit, Policy: policy, Sort: "index"})
		if err != nil {
			return experience.PluginBrowsePage{}, err
		}
		if len(page.Items) == 0 && page.Total == 0 && cursor == "" && limit != 0 &&
			parentItem.Playable && parentItem.Unavailable == nil && flatPlayableAlbum(rec.Decl, lib.Present) {
			// A flat playable album has no track items to list; present
			// the album itself once as its playable child.
			self := c.projectItems(ctx, rec, lib, []core.KindItem{parentItem})
			return experience.PluginBrowsePage{Items: []core.Title{flatAlbumIncarnation(self[0])}, Total: 1}, nil
		}
		return experience.PluginBrowsePage{
			Items:      c.projectItems(ctx, rec, lib, page.Items),
			Total:      page.Total,
			NextCursor: page.NextCursor,
		}, nil
	}

	// Recursive multi-level expansion (e.g. root -> intermediate -> leaf):
	// walk descendants breadth-first from the store with bounded pages.
	var leaves []core.KindItem
	queue := []core.ItemRef{parentItem.Ref}
	const maxExpanded = 2000
	for len(queue) > 0 && len(leaves) < maxExpanded {
		cur := queue[0]
		queue = queue[1:]
		childCursor := ""
		for {
			policy, e := c.items.LibraryPolicy(ctx, lib)
			if e != nil {
				return experience.PluginBrowsePage{}, e
			}
			page, err := c.items.Browse(ctx, pluginitems.BrowseQuery{LibraryID: lib.ID, ParentRef: cur, Cursor: childCursor, Limit: 200, Policy: policy, Sort: "index"})
			if err != nil {
				return experience.PluginBrowsePage{}, err
			}
			for _, it := range page.Items {
				_, idx, _ := rec.Decl.Level(it.Level)
				if idx >= 0 && idx < len(rec.Decl.Levels)-1 && !it.Playable {
					queue = append(queue, it.Ref)
				} else {
					leaves = append(leaves, it)
					if len(leaves) >= maxExpanded {
						break
					}
				}
			}
			if page.NextCursor == "" || len(page.Items) == 0 || len(leaves) >= maxExpanded {
				break
			}
			childCursor = page.NextCursor
		}
	}
	offset := 0
	if cursor != "" {
		if n, err := strconv.Atoi(cursor); err == nil && n >= 0 {
			offset = n
		}
	}
	if limit <= 0 {
		limit = 200
	}
	start := min(offset, len(leaves))
	end := min(start+limit, len(leaves))
	next := ""
	if end < len(leaves) {
		next = strconv.Itoa(end)
	}
	return experience.PluginBrowsePage{
		Items:      c.projectItems(ctx, rec, lib, leaves[start:end]),
		Total:      len(leaves),
		NextCursor: next,
	}, nil
}

// Ancestors implements experience.PluginLibraries, returning ancestors ordered
// from root to immediate parent.
func (c *KindCatalog) Ancestors(ctx context.Context, key core.RatingKey) ([]core.Title, core.Library, error) {
	if c == nil || c.items == nil || c.libs == nil || !key.IsPluginKey() {
		return nil, core.Library{}, core.ErrNotFound
	}
	cur, libID, err := c.items.GetByKey(ctx, key)
	if err != nil {
		return nil, core.Library{}, core.ErrNotFound
	}
	lib, err := c.libs.Get(ctx, libID)
	if err != nil {
		return nil, core.Library{}, core.ErrNotFound
	}
	rec, err := c.items.GetKindByLibrary(ctx, libID)
	if err != nil {
		return nil, core.Library{}, core.ErrNotFound
	}
	var chain []core.KindItem
	for depth := 0; depth < 5 && cur.ParentID != ""; depth++ {
		p, pLibID, err := c.items.GetByExternal(ctx, rec.PluginID, rec.KindKey, cur.ParentID)
		if err != nil || pLibID != libID {
			return nil, core.Library{}, core.ErrNotFound
		}
		chain = append(chain, p)
		cur = p
	}
	slices.Reverse(chain)
	return c.projectItems(ctx, rec, lib, chain), lib, nil
}

// Facets implements experience.PluginLibraries.
func (c *KindCatalog) Facets(ctx context.Context, lib core.Library, facet string) ([]core.FacetValue, error) {
	if c == nil || c.items == nil {
		return nil, nil
	}
	if facet == "person" {
		policy, err := c.items.LibraryPolicy(ctx, lib)
		if err != nil {
			return nil, err
		}
		type personEntry struct {
			id   int64
			name string
		}
		var entries []personEntry
		_, _, err = c.items.FindPersonID(ctx, lib.ID, policy, func(pid int64, name string) bool {
			if core.IsPluginPersonID(pid) && strings.TrimSpace(name) != "" {
				entries = append(entries, personEntry{id: pid, name: strings.TrimSpace(name)})
			}
			return false
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(entries, func(i, j int) bool {
			li, lj := strings.ToLower(entries[i].name), strings.ToLower(entries[j].name)
			if li != lj {
				return li < lj
			}
			return entries[i].id < entries[j].id
		})
		out := make([]core.FacetValue, 0, len(entries))
		for _, e := range entries {
			keyStr := strconv.FormatInt(e.id, 10)
			out = append(out, core.FacetValue{
				ID:    core.FacetID("person", keyStr),
				Key:   keyStr,
				Title: e.name,
				Count: 1,
			})
		}
		return out, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	rootLevel := ""
	if len(rec.Decl.Levels) > 0 {
		rootLevel = rec.Decl.Levels[0].Key
	}
	fields := resolveFacetFields(rec.Decl, facet)
	if len(fields) == 0 {
		return []core.FacetValue{}, nil
	}
	merged := make(map[string]int)
	for _, fieldKey := range fields {
		page, err := c.items.Facets(ctx, lib.ID, rootLevel, fieldKey, 500)
		if err != nil {
			return nil, err
		}
		for _, fc := range page.Values {
			if fc.Value != "" {
				merged[fc.Value] += fc.Count
			}
		}
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]core.FacetValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, core.FacetValue{
			ID:    core.FacetID(facet, k),
			Key:   k,
			Title: k,
			Count: merged[k],
		})
	}
	return out, nil
}

// Search implements experience.PluginLibraries: local FTS5/tsvector search plus
// optional 2-second bounded live plugin search with background upsert (§15.4).
func (c *KindCatalog) Search(ctx context.Context, lib core.Library, query string, q frontend.ItemQuery, limit int) ([]core.Title, error) {
	if c == nil || c.items == nil {
		return nil, nil
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if q.Filter.Person != 0 && !core.IsPluginPersonID(q.Filter.Person) {
		return nil, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	searchLevels, ok := searchLevelsForQuery(rec.Decl, lib.Present, q)
	if !ok {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	visCtx := core.WithExplicitSearch(ctx)
	policy, err := c.items.LibraryPolicy(visCtx, lib)
	if err != nil {
		return nil, err
	}
	facets := mapFilterToFacets(rec.Decl, q.Filter)
	sq := pluginitems.SearchQuery{
		Policy:    policy,
		LibraryID: lib.ID,
		ViewerID:  core.CatalogViewerID(ctx),
		Query:     query,
		Levels:    searchLevels,
		PersonID:  q.Filter.Person,
		Facets:    facets,
		Limit:     limit,
	}
	if len(q.Filter.TabRules) > 0 || q.Filter.TabFallback != "" {
		tp, tpErr := c.items.CompilePolicy(visCtx, lib.ID, q.Filter.TabRules, q.Filter.TabFallback)
		if tpErr != nil {
			return nil, tpErr
		}
		sq.TabPolicy = tp.ForSearch()
	}
	localPage, err := c.items.Search(ctx, sq)
	if err != nil {
		return nil, err
	}
	combined := localPage.Items
	if rec.Decl.Browse.LiveSearch && c.mgr != nil && c.IsPluginLibraryActive(ctx, lib) {
		liveItems := c.runLiveSearch(ctx, rec, query, limit)
		if len(liveItems) > 0 {
			if len(facets) > 0 || sq.TabPolicy != nil {
				if refreshed, rErr := c.items.Search(ctx, sq); rErr == nil {
					combined = refreshed.Items
				}
			} else {
				seen := make(map[core.RatingKey]bool, len(combined)+len(liveItems))
				merged := make([]core.KindItem, 0, len(combined)+len(liveItems))
				for _, it := range combined {
					if it.Key != 0 && !seen[it.Key] {
						seen[it.Key] = true
						merged = append(merged, it)
					}
				}
				for _, it := range liveItems {
					if it.Key != 0 && !seen[it.Key] {
						seen[it.Key] = true
						merged = append(merged, it)
					}
				}
				combined = merged
			}
		}
	}
	refs := make([]core.ItemRef, len(combined))
	for i, it := range combined {
		refs[i] = it.Ref
	}
	visibleRefs, err := c.items.FilterVisibleRefs(visCtx, lib, refs)
	if err != nil {
		return nil, err
	}
	allowed := combined[:0]
	for _, it := range combined {
		if visibleRefs[it.Ref] {
			allowed = append(allowed, it)
		}
	}
	projected := c.projectItems(ctx, rec, lib, allowed)
	if flatAudioQuery(rec.Decl, lib.Present, q) {
		for i, item := range allowed {
			if item.Playable && item.Unavailable == nil {
				projected[i] = flatAlbumIncarnation(projected[i])
			}
		}
	}
	for i := range projected {
		projected[i].SearchScore = scorePluginSearchHit(projected[i], query)
	}
	return projected, nil
}

// SearchPeople implements experience.PluginPeopleLibraries.
func (c *KindCatalog) SearchPeople(ctx context.Context, lib core.Library, query string, q frontend.ItemQuery, personLimit, itemLimit int) ([]experience.PluginPersonMatch, error) {
	if c == nil || c.items == nil {
		return nil, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	if _, ok := searchLevelsForQuery(rec.Decl, lib.Present, q); !ok {
		return nil, nil
	}
	policy, err := c.items.LibraryPolicy(core.WithExplicitSearch(ctx), lib)
	if err != nil {
		return nil, err
	}
	if itemLimit <= 0 {
		itemLimit = 25
	}
	matches, err := c.items.SearchPeople(ctx, lib.ID, policy, query, personLimit, itemLimit)
	if err != nil || len(matches) == 0 {
		return nil, nil
	}
	out := make([]experience.PluginPersonMatch, 0, len(matches))
	for _, m := range matches {
		titles := c.projectItems(ctx, rec, lib, m.Items)
		if len(q.Types) > 0 || len(q.UnservedTypes) > 0 {
			filtered := titles[:0]
			for _, t := range titles {
				if levelMatchesQueryTypes(t, q) {
					filtered = append(filtered, t)
				}
			}
			titles = filtered
		}
		if len(titles) == 0 {
			continue
		}
		out = append(out, experience.PluginPersonMatch{
			PersonID:    m.ID,
			Name:        m.Name,
			CreditCount: m.CreditCount,
			ArtworkItem: m.SampleItem.Key,
			HasThumb:    m.Thumb != "",
			Roles:       m.Roles,
			Titles:      titles,
		})
	}
	return out, nil
}

// PersonByID implements experience.PluginPeopleLibraries.
func (c *KindCatalog) PersonByID(ctx context.Context, lib core.Library, personID int64, itemLimit int) (experience.PluginPersonMatch, bool, error) {
	if c == nil || c.items == nil || !core.IsPluginPersonID(personID) {
		return experience.PluginPersonMatch{}, false, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return experience.PluginPersonMatch{}, false, nil
	}
	policy, err := c.items.LibraryPolicy(core.WithExplicitSearch(ctx), lib)
	if err != nil {
		return experience.PluginPersonMatch{}, false, err
	}
	if itemLimit <= 0 {
		itemLimit = 50
	}
	m, ok, err := c.items.PersonByID(ctx, lib.ID, policy, personID, itemLimit)
	if err != nil || !ok {
		return experience.PluginPersonMatch{}, false, err
	}
	titles := c.projectItems(ctx, rec, lib, m.Items)
	return experience.PluginPersonMatch{
		PersonID:    m.ID,
		Name:        m.Name,
		CreditCount: m.CreditCount,
		ArtworkItem: m.SampleItem.Key,
		HasThumb:    m.Thumb != "",
		Roles:       m.Roles,
		Titles:      titles,
	}, true, nil
}

// FindPersonID implements experience.PluginPeopleLibraries.
func (c *KindCatalog) FindPersonID(ctx context.Context, lib core.Library, match func(personID int64, name string) bool) (int64, bool, error) {
	if c == nil || c.items == nil || match == nil {
		return 0, false, nil
	}
	policy, err := c.items.LibraryPolicy(core.WithExplicitSearch(ctx), lib)
	if err != nil {
		return 0, false, err
	}
	return c.items.FindPersonID(ctx, lib.ID, policy, match)
}

// MemberOf implements experience.PluginLibraries for a collection
// library: it reports whether key is a shared member of the collection,
// one indexed read on collections_members, never a walk.
func (c *KindCatalog) MemberOf(ctx context.Context, lib core.Library, key core.RatingKey) (bool, error) {
	if c == nil || c.libs == nil || key == 0 {
		return false, nil
	}
	if c.titleInLibrary != nil {
		return c.titleInLibrary(ctx, lib, key)
	}
	ok, err := c.libs.SourceMember(ctx, lib, key)
	if err != nil || !ok {
		return ok, err
	}
	if lib.Source == core.LibraryCollection {
		return c.libs.CollectionMember(ctx, lib.CollectionID, key)
	}
	return len(lib.Rules) == 0 && lib.VisibilityDefault != "hide", nil
}

func (c *KindCatalog) runLiveSearch(ctx context.Context, rec pluginitems.KindRecord, query string, limit int) []core.KindItem {
	sfKey := rec.PluginID + "\x00" + rec.KindKey + "\x00" + strings.ToLower(strings.TrimSpace(query))

	c.liveMu.Lock()
	call, inflight := c.liveCalls[sfKey]
	if !inflight {
		call = &liveSearchCall{done: make(chan struct{})}
		c.liveCalls[sfKey] = call
		sem := c.liveSems[rec.PluginID]
		if sem == nil {
			sem = make(chan struct{}, maxLiveSearchPerPlugin)
			c.liveSems[rec.PluginID] = sem
		}
		go c.execLiveSearch(sfKey, sem, rec, query, limit, call)
	}
	c.liveMu.Unlock()

	budget := c.liveSearchTimeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && rem < budget {
			budget = rem
		}
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()

	select {
	case <-call.done:
		return call.items
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return nil
	}
}

func (c *KindCatalog) execLiveSearch(sfKey string, sem chan struct{}, rec pluginitems.KindRecord, query string, limit int, call *liveSearchCall) {
	defer func() {
		close(call.done)
		c.liveMu.Lock()
		delete(c.liveCalls, sfKey)
		c.liveMu.Unlock()
	}()

	bgCtx, cancel := context.WithTimeout(c.syncCtx, liveSearchBackgroundWait)
	defer cancel()

	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-bgCtx.Done():
		return
	}

	body, err := json.Marshal(core.KindBrowseRequest{Query: query, Limit: limit})
	if err != nil {
		return
	}
	resp, err := c.mgr.Call(bgCtx, rec.PluginID, plugins.Request{
		Method: "POST",
		Path:   "/ether/v1/kinds/" + rec.KindKey + "/search",
		Body:   body,
	})
	if err != nil || resp.Status != 200 || len(resp.Body) == 0 {
		return
	}
	var page core.KindPage
	if err := json.Unmarshal(resp.Body, &page); err != nil || len(page.Items) == 0 {
		return
	}
	if _, err := c.items.UpsertItems(bgCtx, rec.PluginID, rec.KindKey, page.Items); err != nil {
		c.log.Debug("live search upsert failed", "plugin", rec.PluginID, "kind", rec.KindKey, "err", err)
		return
	}
	stored := make([]core.KindItem, 0, len(page.Items))
	for _, raw := range page.Items {
		if it, _, err := c.items.GetByExternal(bgCtx, rec.PluginID, rec.KindKey, raw.ID); err == nil {
			stored = append(stored, it)
		}
	}
	call.items = stored
}

func (c *KindCatalog) projectItems(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, items []core.KindItem) []core.Title {
	if len(items) == 0 {
		return []core.Title{}
	}
	rootCache := make(map[string]core.Title)
	resolveRoot := func(parentID string) (core.Title, bool) {
		if parentID == "" {
			return core.Title{}, false
		}
		if t, ok := rootCache[parentID]; ok {
			return t, t.Key != 0
		}
		curID := parentID
		var root core.KindItem
		found := false
		for depth := 0; depth < 4 && curID != ""; depth++ {
			p, pLib, err := c.items.GetByExternal(ctx, rec.PluginID, rec.KindKey, curID)
			if err != nil || pLib != lib.ID {
				break
			}
			root = p
			found = true
			curID = p.ParentID
		}
		if !found {
			rootCache[parentID] = core.Title{}
			return core.Title{}, false
		}
		rt := pluginitems.Project(root, rec.Decl, lib.Present, lib.CardShape)
		rt.LibraryID = lib.ID
		if rt.ContentRating == "" {
			rt.ContentRating = extractContentRating(root.Fields)
		}
		rootCache[parentID] = rt
		return rt, true
	}

	parentCache := make(map[string]core.Title)
	resolveParent := func(parentID string) (core.Title, bool) {
		if t, ok := parentCache[parentID]; ok {
			return t, t.Key != 0
		}
		p, pLib, err := c.items.GetByExternal(ctx, rec.PluginID, rec.KindKey, parentID)
		if err != nil || pLib != lib.ID {
			parentCache[parentID] = core.Title{}
			return core.Title{}, false
		}
		pt := pluginitems.Project(p, rec.Decl, lib.Present, lib.CardShape)
		pt.LibraryID = lib.ID
		parentCache[parentID] = pt
		return pt, true
	}

	flatAlbum := flatPlayableAlbum(rec.Decl, lib.Present)
	out := make([]core.Title, len(items))
	for i, it := range items {
		t := pluginitems.Project(it, rec.Decl, lib.Present, lib.CardShape)
		t.LibraryID = lib.ID
		if flatAlbum && it.Playable && it.Unavailable == nil {
			// The album lists its one synthetic playable child.
			if t.ChildCount == 0 {
				t.ChildCount = 1
			}
			if t.LeafCount == 0 {
				t.LeafCount = 1
			}
		}
		if t.ContentRating == "" {
			t.ContentRating = extractContentRating(it.Fields)
		}
		if it.ParentID != "" {
			// Immediate parent for every hierarchy (track -> album,
			// album -> artist); the root naming below is the video
			// projection's show identity.
			if parent, ok := resolveParent(it.ParentID); ok {
				t.ParentKey = parent.Key
				t.ParentTitle = parent.Title
			}
		}
		if (t.Type == core.TypeSeason || t.Type == core.TypeEpisode) && it.ParentID != "" {
			if root, ok := resolveRoot(it.ParentID); ok {
				t.ShowKey = root.Key
				t.ShowTitle = root.Title
				t.ShowYear = root.Year
			}
		}
		out[i] = t
	}
	return out
}

func extractContentRating(fields map[string]json.RawMessage) string {
	for _, key := range []string{"contentRating", "certification", "mpaa"} {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

func projectImages(art []core.KindArtRef, landscape bool) []frontend.Image {
	if len(art) == 0 {
		return nil
	}
	out := make([]frontend.Image, 0, len(art)+1)
	hasThumb := false
	var backdrop *frontend.Image
	for _, a := range art {
		u := strings.TrimSpace(a.URL)
		if u == "" {
			continue
		}
		kind := a.Role
		if kind == "" {
			kind = "poster"
		}
		im := frontend.Image{
			Kind:   kind,
			Path:   u,
			Width:  a.Width,
			Height: a.Height,
		}
		switch kind {
		case frontend.ImageThumb:
			hasThumb = true
		case frontend.ImageBackdrop:
			if backdrop == nil {
				cp := im
				backdrop = &cp
			}
		}
		out = append(out, im)
	}
	if landscape && !hasThumb && backdrop != nil {
		cp := *backdrop
		cp.Kind = frontend.ImageThumb
		out = append(out, cp)
	}
	return out
}

func levelIsPlayable(decl plugins.MediaKindMeta, levelKey string) bool {
	lv, _, ok := decl.Level(levelKey)
	return ok && lv.Playable
}

// levelMatchesQueryTypes reports whether a level's projected title answers
// the query's type filter: by its core media type (movie/show/season/episode)
// or by the MediaBrowser item type the presentation declares for it
// (MusicAlbum, Audio, Book, …), which clients name in IncludeItemTypes and
// the query carries in UnservedTypes. A level the client excluded by either
// name never matches.
func levelMatchesQueryTypes(projected core.Title, q frontend.ItemQuery) bool {
	for _, excluded := range q.ExcludedTypes {
		if excluded == projected.Type || strings.EqualFold(string(excluded), projected.MBItemType) {
			return false
		}
	}
	if slices.Contains(q.Types, projected.Type) {
		return true
	}
	for _, kind := range q.UnservedTypes {
		if strings.EqualFold(kind, projected.MBItemType) {
			return true
		}
	}
	return false
}

func browseLevelsForQuery(decl plugins.MediaKindMeta, present core.Presentations, q frontend.ItemQuery) (level string, levels []string, anyLevel bool, ok bool) {
	if len(decl.Levels) == 0 {
		return "", nil, true, true
	}
	if len(q.Types) == 0 && len(q.UnservedTypes) == 0 {
		if q.Filter.Person > 0 {
			return "", nil, true, true
		}
		// An exclusion alone still narrows the root listing.
		if len(q.ExcludedTypes) == 0 {
			return decl.Levels[0].Key, nil, false, true
		}
		root := pluginitems.Project(core.KindItem{Level: decl.Levels[0].Key}, decl, present)
		for _, excluded := range q.ExcludedTypes {
			if excluded == root.Type || strings.EqualFold(string(excluded), root.MBItemType) {
				return "", nil, false, false
			}
		}
		return decl.Levels[0].Key, nil, false, true
	}
	flatAudio := flatAudioQuery(decl, present, q)
	var matched []string
	for _, lv := range decl.Levels {
		if !q.Recursive && lv.Key != decl.Levels[0].Key {
			continue
		}
		projected := pluginitems.Project(core.KindItem{Level: lv.Key}, decl, present)
		if levelMatchesQueryTypes(projected, q) || flatAudio {
			matched = append(matched, lv.Key)
		}
	}
	if len(matched) == 0 {
		return "", nil, false, false
	}
	if len(matched) == 1 {
		return matched[0], nil, false, true
	}
	return "", matched, false, true
}

func searchLevelsForQuery(decl plugins.MediaKindMeta, present core.Presentations, q frontend.ItemQuery) ([]string, bool) {
	if (len(q.Types) == 0 && len(q.UnservedTypes) == 0) || len(decl.Levels) == 0 {
		return nil, true
	}
	flatAudio := flatAudioQuery(decl, present, q)
	var matched []string
	for _, lv := range decl.Levels {
		projected := pluginitems.Project(core.KindItem{Level: lv.Key}, decl, present)
		if levelMatchesQueryTypes(projected, q) || flatAudio {
			matched = append(matched, lv.Key)
		}
	}
	if len(matched) == 0 {
		return nil, false
	}
	if len(matched) == len(decl.Levels) {
		return nil, true
	}
	return matched, true
}

func searchWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func normalizeArticleSort(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, prefix := range []string{"the ", "an ", "a "} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			if rest = strings.TrimSpace(rest); rest != "" {
				return rest
			}
		}
	}
	return s
}

func scorePluginSearchHit(t core.Title, query string) float64 {
	qNorm := strings.ToLower(strings.TrimSpace(query))
	qSort := normalizeArticleSort(query)
	tNorm := strings.ToLower(strings.TrimSpace(t.Title))
	tSort := strings.ToLower(strings.TrimSpace(t.SortTitle))
	if tSort == "" {
		tSort = normalizeArticleSort(t.Title)
	}
	if qNorm != "" && (tNorm == qNorm || (qSort != "" && tSort == qSort)) {
		return 1.0
	}
	if qNorm != "" && (strings.HasPrefix(tNorm, qNorm) || (qSort != "" && strings.HasPrefix(tSort, qSort))) {
		return 0.97
	}
	qTokens := searchWords(query)
	if len(qTokens) > 0 {
		tWords := searchWords(t.Title)
		allInTitle := len(tWords) > 0
		for _, qt := range qTokens {
			found := false
			for _, tw := range tWords {
				if strings.HasPrefix(tw, qt) {
					found = true
					break
				}
			}
			if !found {
				allInTitle = false
				break
			}
		}
		if allInTitle {
			return 0.93
		}
	}
	return 0.60
}

func mapBrowseSort(sortBy core.SortOrder, desc bool) string {
	var col string
	switch sortBy {
	case "", core.SortTitle:
		col = "title"
	case core.SortAdded:
		col = "added"
	case core.SortReleased:
		col = "released"
	case core.SortYear:
		col = "year"
	case core.SortRating:
		col = "rating"
	case core.SortPopularity:
		col = "rating"
	case core.SortIndex:
		col = "index"
	case core.SortLastViewed, core.SortPlayed:
		col = "last_viewed"
	default:
		col = "title"
	}
	if sortBy == "" && !desc {
		return "title:asc"
	}
	if desc {
		return col + ":desc"
	}
	return col + ":asc"
}

func resolveFacetFields(decl plugins.MediaKindMeta, facet string) []string {
	for _, f := range decl.Browse.Facets {
		if f == facet {
			return []string{f}
		}
	}
	for _, fd := range decl.Fields {
		if fd.Key == facet {
			return []string{fd.Key}
		}
	}
	if facet == "genre" {
		for _, fd := range decl.Fields {
			if fd.Key == "genre" || fd.Key == "genres" || fd.Key == "tags" {
				return []string{fd.Key}
			}
		}
		if len(decl.Browse.Facets) > 0 {
			return append([]string(nil), decl.Browse.Facets...)
		}
	}
	return nil
}

// mapExcludeGenresToFacets turns viewer-blocked genres into ExcludeFacets
// over the declared genre-bearing fields (the fields Project reads into
// Title.Genres), so the store drops them in SQL before counting or paging.
func mapExcludeGenresToFacets(decl plugins.MediaKindMeta, genres []string) map[string][]string {
	if len(genres) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(decl.Fields))
	for _, fd := range decl.Fields {
		declared[fd.Key] = true
	}
	out := make(map[string][]string)
	for _, key := range []string{"genre", "genres", "tags", "tag"} {
		if declared[key] {
			out[key] = append([]string(nil), genres...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapFilterToFacets(decl plugins.MediaKindMeta, f core.Filter) map[string][]string {
	if len(f.Genres) == 0 && f.Studio == "" && len(f.Facets) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(decl.Fields))
	filterable := make(map[string]bool, len(decl.Fields)*2+4)
	filterable["year"] = true
	filterable["decade"] = true
	filterable["released"] = true
	filterable["rating"] = true
	for _, fd := range decl.Fields {
		declared[fd.Key] = true
		filterable[fd.Key] = true
		if fd.Type == "date" {
			filterable[fd.Key+"-decade"] = true
		}
	}
	out := make(map[string][]string)
	for k, vals := range f.Facets {
		if !filterable[k] || len(vals) == 0 {
			continue
		}
		out[k] = append([]string(nil), vals...)
	}
	if len(f.Genres) > 0 {
		fields := resolveFacetFields(decl, "genre")
		if len(fields) > 0 && declared[fields[0]] {
			out[fields[0]] = append(out[fields[0]], f.Genres...)
		}
	}
	if f.Studio != "" {
		for _, cand := range []string{"studio", "studios", "publisher", "author", "channel"} {
			if declared[cand] {
				out[cand] = append(out[cand], f.Studio)
				break
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	return nil
}

func parseKindDuration(raw string, fallback time.Duration) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fallback
	}
	if strings.HasSuffix(s, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return fallback
}

func fillPluginTabDefaults(vt *frontend.ViewTab, sorts []string) {
	if vt.Sort != "" && core.IsLastViewedSort(vt.Sort) {
		vt.Sort = string(core.SortLastViewed)
	}
	switch {
	case vt.Kind == "collections" && vt.Facet != "":
		if vt.Order == "" {
			vt.Order = "count"
		}
		effSort := vt.Sort
		if effSort == "" {
			if slices.Contains(sorts, "released") {
				effSort = "released"
			} else if len(sorts) > 0 {
				effSort = sorts[0]
			} else {
				effSort = "server"
			}
		}
		effDir := vt.Direction
		if effDir == "" {
			switch effSort {
			case "server", "title", "index":
				effDir = "asc"
			default:
				effDir = "desc"
			}
		}
		vt.Sort = effSort
		vt.Direction = effDir
	case vt.Kind == "people" && vt.Facet != "":
		if vt.Order == "" {
			vt.Order = "name"
		}
	case vt.Kind == "view":
		if vt.Layout == "" {
			vt.Layout = core.TabLayoutGrid
		}
		if vt.Sort != "" && vt.Direction == "" {
			switch vt.Sort {
			case "server", "title", "index":
				vt.Direction = "asc"
			default:
				vt.Direction = "desc"
			}
		}
	}
}

// Presentation implements experience.PluginPresentationLibraries.
func (c *KindCatalog) Presentation(ctx context.Context, lib core.Library) (experience.PluginPresentation, bool) {
	if c == nil || c.items == nil || lib.Source != core.LibraryPlugin {
		return experience.PluginPresentation{}, false
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return experience.PluginPresentation{}, false
	}
	decl := rec.Decl
	sorts := append([]string(nil), decl.Browse.Sorts...)

	var tabs []frontend.ViewTab
	if lib.TabsCustomized {
		tabs = make([]frontend.ViewTab, 0, len(lib.Tabs))
		for _, tab := range lib.Tabs {
			if tab.Hidden {
				continue
			}
			effSearch := tab.EffectiveSearch()
			var placeholder string
			if effSearch && strings.TrimSpace(tab.SearchPlaceholder) != "" {
				placeholder = c.resolvePluginText(ctx, rec.PluginID, tab.SearchPlaceholder)
			}
			vt := frontend.ViewTab{
				ID:                tab.Key,
				Label:             c.resolvePluginText(ctx, rec.PluginID, tab.Label),
				Kind:              tab.Kind,
				Facet:             tab.Facet,
				Order:             tab.Order,
				Sort:              tab.Sort,
				Direction:         tab.Direction,
				Card:              tab.EffectiveCard(),
				Layout:            tab.EffectiveLayout(),
				Search:            effSearch,
				SearchPlaceholder: placeholder,
				Filters:           tab.Filters.Clone(),
			}
			fillPluginTabDefaults(&vt, sorts)
			tabs = append(tabs, vt)
		}
	} else {
		rawTabs := decl.Library.EffectiveTabs()
		tabs = make([]frontend.ViewTab, 0, len(rawTabs))
		for _, tab := range rawTabs {
			vt := frontend.ViewTab{
				ID:        tab.Key,
				Label:     c.resolvePluginText(ctx, rec.PluginID, tab.Label),
				Kind:      tab.Kind,
				Facet:     tab.Facet,
				Order:     tab.Order,
				Sort:      tab.Sort,
				Direction: tab.Direction,
				Card:      tab.EffectiveCard(),
				Layout:    tab.EffectiveLayout(),
				Filters:   tab.Filters.Clone(),
			}
			if tab.Kind == "collections" || tab.Kind == "people" || tab.Kind == "view" || tab.Kind == "live" {
				vt.Search = tab.EffectiveSearch()
				if vt.Search && strings.TrimSpace(tab.SearchPlaceholder) != "" {
					vt.SearchPlaceholder = c.resolvePluginText(ctx, rec.PluginID, tab.SearchPlaceholder)
				}
			}
			fillPluginTabDefaults(&vt, sorts)
			tabs = append(tabs, vt)
		}
	}

	fieldsByKey := make(map[string]plugins.KindField, len(decl.Fields))
	for _, fd := range decl.Fields {
		fieldsByKey[fd.Key] = fd
	}
	resolveViewFacet := func(fk string) (frontend.ViewFacet, bool) {
		if fd, ok := fieldsByKey[fk]; ok {
			lbl := c.resolvePluginText(ctx, rec.PluginID, fd.Label)
			if strings.TrimSpace(lbl) == "" {
				lbl = fk
			}
			return frontend.ViewFacet{
				ID:    fk,
				Label: lbl,
				Type:  mapVelaFacetType(fd.Type),
				Multi: true,
			}, true
		}
		if base, ok := libraryfilter.SplitDateDecadeFacet(fk); ok {
			if fd, exists := fieldsByKey[base]; exists && fd.Type == "date" {
				lbl := c.resolvePluginText(ctx, rec.PluginID, fd.Label)
				if strings.TrimSpace(lbl) == "" {
					lbl = base
				}
				return frontend.ViewFacet{
					ID:    fk,
					Label: lbl + " (decade)",
					Type:  "date",
					Multi: true,
				}, true
			}
		}
		switch fk {
		case "year":
			return frontend.ViewFacet{ID: "year", Label: "Year", Type: "int", Multi: true}, true
		case "decade":
			return frontend.ViewFacet{ID: "decade", Label: "Decade", Type: "int", Multi: true}, true
		case "released":
			return frontend.ViewFacet{ID: "released", Label: "Released", Type: "date", Multi: true}, true
		case "rating":
			return frontend.ViewFacet{ID: "rating", Label: "Rating", Type: "int", Multi: true}, true
		default:
			return frontend.ViewFacet{}, false
		}
	}

	facets := make([]frontend.ViewFacet, 0, len(decl.Browse.Facets)+len(tabs))
	seenFacet := make(map[string]bool, len(decl.Browse.Facets)+len(tabs))
	for _, fk := range decl.Browse.Facets {
		if seenFacet[fk] {
			continue
		}
		if vf, ok := resolveViewFacet(fk); ok {
			seenFacet[fk] = true
			facets = append(facets, vf)
		}
	}
	for _, vt := range tabs {
		if vt.Facet == "" || seenFacet[vt.Facet] {
			continue
		}
		if vf, ok := resolveViewFacet(vt.Facet); ok {
			seenFacet[vt.Facet] = true
			facets = append(facets, vf)
		}
	}

	var shelves []experience.PluginShelf
	if len(decl.Library.Shelves) > 0 {
		shelves = make([]experience.PluginShelf, 0, len(decl.Library.Shelves))
		for _, sh := range decl.Library.Shelves {
			lbl := c.resolvePluginText(ctx, rec.PluginID, sh.Label)
			if strings.TrimSpace(lbl) == "" {
				lbl = sh.Key
			}
			shelves = append(shelves, experience.PluginShelf{
				Key:       sh.Key,
				Label:     lbl,
				Sort:      mapPluginShelfSort(sh.EffectiveSort()),
				Direction: sh.EffectiveDirection(),
				Filters:   sh.EffectiveFilters(),
			})
		}
	}

	return experience.PluginPresentation{
		Card:        lib.EffectiveCardShape(decl.Library.EffectiveCard()),
		DateDisplay: decl.Library.EffectiveDateDisplay(),
		Tabs:        tabs,
		Sorts:       sorts,
		Facets:      facets,
		Shelves:     shelves,
	}, true
}

func mapPluginShelfSort(raw string) core.SortOrder {
	switch raw {
	case "title":
		return core.SortTitle
	case "added":
		return core.SortAdded
	case "released", "":
		return core.SortReleased
	case "year":
		return core.SortYear
	case "rating":
		return core.SortRating
	case "index":
		return core.SortIndex
	default:
		return core.SortReleased
	}
}

func mapVelaFacetType(raw string) string {
	switch raw {
	case "enum", "bool":
		return "enum"
	case "people":
		return "people"
	case "int", "float":
		return "int"
	case "date":
		return "date"
	case "duration":
		return "duration"
	default:
		return "strings"
	}
}

// FacetBuckets implements experience.PluginPresentationLibraries.
func (c *KindCatalog) FacetBuckets(ctx context.Context, lib core.Library, field string) ([]frontend.FacetBucket, error) {
	if c == nil || c.items == nil {
		return []frontend.FacetBucket{}, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	rootLevel := ""
	if len(rec.Decl.Levels) > 0 {
		rootLevel = rec.Decl.Levels[0].Key
	}
	policy, err := c.items.LibraryPolicy(ctx, lib)
	if err != nil {
		return nil, err
	}
	buckets, err := c.items.FacetBuckets(ctx, lib.ID, rootLevel, policy, field)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.FacetBucket, len(buckets))
	for i, b := range buckets {
		out[i] = frontend.FacetBucket{
			ID:    b.ID,
			Label: b.Label,
			Count: b.Count,
		}
	}
	return out, nil
}

// FacetCollections implements experience.PluginPresentationLibraries.
func (c *KindCatalog) FacetCollections(ctx context.Context, lib core.Library, field, order, query string, offset, limit int) (frontend.FacetCollectionPage, error) {
	if c == nil || c.items == nil {
		return frontend.FacetCollectionPage{Items: []frontend.FacetCollection{}}, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	rootLevel := ""
	if len(rec.Decl.Levels) > 0 {
		rootLevel = rec.Decl.Levels[0].Key
	}
	policy, err := c.items.LibraryPolicy(ctx, lib)
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	page, err := c.items.FacetCollections(ctx, pluginitems.FacetCollectionsQuery{
		LibraryID: lib.ID,
		Level:     rootLevel,
		Policy:    policy,
		FieldKey:  field,
		Order:     order,
		Query:     query,
		Offset:    offset,
		Limit:     limit,
	})
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	var newestRaw []core.KindItem
	newestIdx := make([]int, 0, len(page.Items))
	for i, col := range page.Items {
		if col.NewestItem != nil {
			newestRaw = append(newestRaw, *col.NewestItem)
			newestIdx = append(newestIdx, i)
		}
	}
	var projected []core.Title
	if len(newestRaw) > 0 {
		projected = c.projectItems(ctx, rec, lib, newestRaw)
	}
	out := make([]frontend.FacetCollection, len(page.Items))
	for i, col := range page.Items {
		out[i] = frontend.FacetCollection{
			ID:        col.ID,
			Name:      col.Label,
			ItemCount: col.Count,
		}
	}
	for j, bucketIdx := range newestIdx {
		if j < len(projected) {
			t := projected[j]
			out[bucketIdx].NewestItem = &t
		}
	}
	return frontend.FacetCollectionPage{Items: out, More: page.More}, nil
}

// FacetPeople implements experience.PluginPresentationLibraries.
func (c *KindCatalog) FacetPeople(ctx context.Context, lib core.Library, field, order, query string, offset, limit int) (experience.PluginFacetPeoplePage, error) {
	if c == nil || c.items == nil {
		return experience.PluginFacetPeoplePage{Items: []experience.PluginFacetPerson{}}, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return experience.PluginFacetPeoplePage{}, err
	}
	rootLevel := ""
	if len(rec.Decl.Levels) > 0 {
		rootLevel = rec.Decl.Levels[0].Key
	}
	policy, err := c.items.LibraryPolicy(ctx, lib)
	if err != nil {
		return experience.PluginFacetPeoplePage{}, err
	}
	page, err := c.items.FacetPeople(ctx, pluginitems.FacetPeopleQuery{
		LibraryID: lib.ID,
		Level:     rootLevel,
		Policy:    policy,
		FieldKey:  field,
		Order:     order,
		Query:     query,
		Offset:    offset,
		Limit:     limit,
	})
	if err != nil {
		return experience.PluginFacetPeoplePage{}, err
	}
	var sampleRaw []core.KindItem
	sampleIdx := make([]int, 0, len(page.Items))
	for i, p := range page.Items {
		if p.SampleItem != nil {
			sampleRaw = append(sampleRaw, *p.SampleItem)
			sampleIdx = append(sampleIdx, i)
		}
	}
	var projected []core.Title
	if len(sampleRaw) > 0 {
		projected = c.projectItems(ctx, rec, lib, sampleRaw)
	}
	out := make([]experience.PluginFacetPerson, len(page.Items))
	for i, p := range page.Items {
		out[i] = experience.PluginFacetPerson{
			PersonID: p.PersonID,
			Name:     p.Name,
			Count:    p.Count,
			HasThumb: p.Thumb != "",
		}
	}
	for j, personIdx := range sampleIdx {
		if j < len(projected) {
			t := projected[j]
			out[personIdx].SampleTitle = &t
		}
	}
	return experience.PluginFacetPeoplePage{Items: out, More: page.More}, nil
}
