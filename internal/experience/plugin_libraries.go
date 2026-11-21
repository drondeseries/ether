package experience

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/reqmemo"
)

// PluginBrowsePage is one keyset-paged window of projected plugin items.
type PluginBrowsePage struct {
	Items      []core.Title
	Total      int
	NextCursor string
}

// PluginItemView is a projected plugin item with its owning library, raw art
// references and playability flag.
type PluginItemView struct {
	Library     core.Library
	Title       core.Title
	Images      []frontend.Image
	Playable    bool
	Unavailable *core.ItemUnavailable
}

// PluginLibraries supplies unified library metadata (builtin, filter, and
// plugin) and store-backed plugin item operations to Service without coupling
// package experience to plugins, pluginitems, or libraries.
type PluginLibraries interface {
	Libraries(ctx context.Context) ([]core.Library, error)
	Library(ctx context.Context, id core.LibraryID) (core.Library, error)
	IsPluginLibraryActive(ctx context.Context, lib core.Library) bool
	RootMediaType(ctx context.Context, lib core.Library) core.MediaType
	Count(ctx context.Context, lib core.Library) (int, error)
	Browse(ctx context.Context, lib core.Library, q frontend.ItemQuery, cursor string, limit int) (PluginBrowsePage, error)
	Item(ctx context.Context, key core.RatingKey) (PluginItemView, error)
	ItemsMany(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]PluginItemView, error)
	Children(ctx context.Context, parent core.RatingKey, recursive bool, cursor string, limit int) (PluginBrowsePage, error)
	Ancestors(ctx context.Context, key core.RatingKey) ([]core.Title, core.Library, error)
	Facets(ctx context.Context, lib core.Library, facet string) ([]core.FacetValue, error)
	Search(ctx context.Context, lib core.Library, query string, q frontend.ItemQuery, limit int) ([]core.Title, error)
	// MemberOf reports whether key is a shared member of a collection
	// library's collection. It is one indexed read, never a walk.
	MemberOf(ctx context.Context, lib core.Library, key core.RatingKey) (bool, error)
}

// PluginShelf is one resolved manifest-declared Recommended shelf for a plugin library.
type PluginShelf struct {
	Key       string
	Label     string
	Sort      core.SortOrder
	Direction string // "desc" (default) or "asc"
	Filters   map[string][]string
}

// PluginPresentation carries manifest-declared Vela library presentation metadata.
type PluginPresentation struct {
	Card        string
	DateDisplay string
	Tabs        []frontend.ViewTab
	Sorts       []string
	Facets      []frontend.ViewFacet
	Shelves     []PluginShelf
}

// PluginFacetPerson is one distinct person entry for a plugin library's people tab.
type PluginFacetPerson struct {
	PersonID    int64
	Name        string
	Count       int
	HasThumb    bool
	SampleTitle *core.Title
}

// PluginFacetPeoplePage is one page of people for a plugin library's people tab.
type PluginFacetPeoplePage struct {
	Items []PluginFacetPerson
	More  bool
}

// PluginSliceExpander is implemented by plugin library adapters that can
// trigger non-blocking background facet/person expansions on viewer browse.
type PluginSliceExpander interface {
	MaybeExpand(ctx context.Context, lib core.Library, q frontend.ItemQuery) bool
}

// PluginPresentationLibraries is implemented by plugin library adapters that
// expose card/tabs/sorts/facets presentation metadata, facet collections, and
// facet people tabs.
type PluginPresentationLibraries interface {
	Presentation(ctx context.Context, lib core.Library) (PluginPresentation, bool)
	FacetBuckets(ctx context.Context, lib core.Library, field string) ([]frontend.FacetBucket, error)
	FacetCollections(ctx context.Context, lib core.Library, field, order, query string, offset, limit int) (frontend.FacetCollectionPage, error)
	FacetPeople(ctx context.Context, lib core.Library, field, order, query string, offset, limit int) (PluginFacetPeoplePage, error)
}

type catalogTabFacetReader interface {
	LibraryFacetCollections(context.Context, core.CatalogFacetCollectionsQuery) (core.CatalogFacetCollectionsPage, error)
	LibraryFacetPeople(context.Context, core.CatalogFacetPeopleQuery) (core.CatalogFacetPeoplePage, error)
}

var (
	_ frontend.LibraryFacetBuckets     = (*Service)(nil)
	_ frontend.LibraryFacetCollections = (*Service)(nil)
	_ frontend.LibraryFacetPeople      = (*Service)(nil)
	_ frontend.PluginLibraryRowsReader = (*Service)(nil)
)

const maxPluginCursorEntries = 512

type pluginCursorCache struct {
	mu    sync.Mutex
	order []string
	items map[string]string
}

func newPluginCursorCache() *pluginCursorCache {
	return &pluginCursorCache{
		items: make(map[string]string),
	}
}

func (c *pluginCursorCache) get(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[key]
	return v, ok
}

func (c *pluginCursorCache) put(key, cursor string) {
	if c == nil || key == "" || cursor == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; !exists {
		if len(c.order) >= maxPluginCursorEntries {
			evict := c.order[0]
			c.order = c.order[1:]
			delete(c.items, evict)
		}
		c.order = append(c.order, key)
	}
	c.items[key] = cursor
}

func filterMediaType(lib core.Library) core.MediaType {
	if lib.MediaType != "" {
		return lib.MediaType
	}
	return core.TypeMovie
}

func isZeroPresentations(p core.Presentations) bool {
	return !p.AdapterOnly && p.Vela == nil && p.Plex == nil && p.MediaBrowser == nil && p.Stremio == nil && p.Xtream == nil && p.Silo == nil
}

func (s *Service) publishedLibrary(ctx context.Context, lib core.Library) (core.MediaType, string, bool) {
	if lib.Hidden {
		return "", "", false
	}
	switch lib.Source {
	case core.LibraryBuiltin:
		mt := lib.MediaType
		if mt == "" {
			if lib.ID == core.LibraryShows {
				mt = core.TypeShow
			} else {
				mt = core.TypeMovie
			}
		}
		if isZeroPresentations(lib.Present) {
			return mt, s.builtinNative(mt), true
		}
		return s.presentLibrary(lib.Present, mt)
	case core.LibraryCatalog:
		return s.presentLibrary(lib.Present, filterMediaType(lib))
	case core.LibraryCollection:
		return s.presentLibrary(lib.Present, filterMediaType(lib))
	case core.LibraryPlugin:
		if s.d.PluginLibraries == nil || !s.d.PluginLibraries.IsPluginLibraryActive(ctx, lib) {
			return "", "", false
		}
		return s.presentLibrary(lib.Present, s.d.PluginLibraries.RootMediaType(ctx, lib))
	default:
		return "", "", false
	}
}

var defaultCatalogueSortIDs = []string{"title", "added", "released", "year", "rating", "duration", "popularity", "lastViewed"}

func libraryTabsToViewTabs(tabs []core.LibraryTab, sorts []string, isCatalogue bool) []frontend.ViewTab {
	out := make([]frontend.ViewTab, 0, len(tabs))
	for _, tab := range tabs {
		if tab.Hidden {
			continue
		}
		sortID := tab.Sort
		if sortID != "" && core.IsLastViewedSort(sortID) {
			sortID = string(core.SortLastViewed)
		}
		vt := frontend.ViewTab{
			ID:                tab.Key,
			Label:             tab.Label,
			Kind:              tab.Kind,
			Facet:             tab.Facet,
			Order:             tab.Order,
			Sort:              sortID,
			Direction:         tab.Direction,
			Card:              tab.EffectiveCard(),
			Layout:            tab.EffectiveLayout(),
			Search:            tab.EffectiveSearch(),
			SearchPlaceholder: tab.SearchPlaceholder,
			Filters:           tab.Filters.Clone(),
		}
		switch tab.Kind {
		case core.TabCollections:
			if isCatalogue && vt.Facet != "" {
				if canon, ok := core.NormalizeCatalogueTabFacet(core.TabCollections, vt.Facet); ok {
					vt.Facet = canon
				}
			}
			if vt.Facet != "" {
				vt.Order = cmp.Or(vt.Order, "count")
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
			}
		case core.TabPeople:
			if isCatalogue && vt.Facet != "" {
				if canon, ok := core.NormalizeCatalogueTabFacet(core.TabPeople, vt.Facet); ok {
					vt.Facet = canon
				}
			}
			if vt.Facet != "" {
				vt.Order = cmp.Or(vt.Order, "name")
			}
		}
		out = append(out, vt)
	}
	return out
}

// PublishedViews enumerates published libraries directly, without inventing a viewer.
func (s *Service) PublishedViews(ctx context.Context) ([]frontend.View, error) {
	if s.d.PluginLibraries == nil {
		return nil, nil
	}
	return s.storedViews(ctx, nil)
}

func (s *Service) storedViews(ctx context.Context, v *core.User) ([]frontend.View, error) {
	libs, err := s.d.PluginLibraries.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.View, 0, len(libs))
	for _, lib := range libs {
		if v != nil && !v.AllowsLibrary(lib.ID) {
			continue
		}
		mt, native, ok := s.publishedLibrary(ctx, lib)
		if !ok {
			continue
		}
		revision := ""
		if !lib.UpdatedAt.IsZero() {
			revision = lib.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
		}
		view := frontend.View{
			Revision:  revision,
			ID:        strconv.FormatInt(int64(lib.ID), 10),
			LibraryID: lib.ID,
			Name:      lib.Name,
			Type:      mt,
			Virtual:   true,
			KindKey:   lib.KindKey,
			PluginID:  lib.PluginID,
			Native:    native,
		}
		if lib.Source == core.LibraryPlugin {
			if pres, hasPres := s.d.PluginLibraries.(PluginPresentationLibraries); hasPres {
				if p, ok := pres.Presentation(ctx, lib); ok {
					view.Card = lib.EffectiveCardShape(p.Card)
					view.DateDisplay = p.DateDisplay
					view.Tabs = p.Tabs
					view.Sorts = p.Sorts
					view.Facets = p.Facets
				}
			}
			if view.Card == "" && lib.CardShape != "" {
				view.Card = lib.EffectiveCardShape("")
			}
			if lib.TabsCustomized && len(view.Tabs) == 0 && len(lib.Tabs) > 0 {
				view.Tabs = libraryTabsToViewTabs(lib.Tabs, view.Sorts, false)
			}
		} else {
			view.Card = lib.EffectiveCardShape("")
			srcTabs := lib.Tabs
			if !lib.TabsCustomized {
				srcTabs = core.DefaultCatalogueTabs()
			}
			view.Tabs = libraryTabsToViewTabs(srcTabs, defaultCatalogueSortIDs, true)
		}
		out = append(out, view)
	}
	return out, nil
}

func (s *Service) allowedFilterLibraries(ctx context.Context, v *core.User) []core.Library {
	if s.d.PluginLibraries == nil || v == nil {
		return nil
	}
	libs, err := reqmemo.Do(ctx, "experience:filterlibs:"+strconv.FormatInt(v.ID, 10), func() ([]core.Library, error) {
		all, err := s.d.PluginLibraries.Libraries(ctx)
		if err != nil {
			return nil, err
		}
		var out []core.Library
		for _, lib := range all {
			if !lib.Hidden && (lib.Source == core.LibraryCatalog || lib.Source == core.LibraryCollection && lib.CollectionID > 0) && v.AllowsLibrary(lib.ID) {
				if _, _, ok := s.publishedLibrary(ctx, lib); ok {
					out = append(out, lib)
				}
			}
		}
		return out, nil
	})
	if err != nil {
		return nil
	}
	return libs
}

func withoutSectionCheck(vis Visibility) Visibility {
	vis.SectionFor = nil
	return vis
}

// sectionOrFilterAllowed reports whether v may access a TMDB title via either
// its canonical built-in library or an allowed, published filter library whose
// saved predicate matches the title (or its parent show for seasons/episodes).
func (s *Service) sectionOrFilterAllowed(ctx context.Context, v *core.User, title *core.Title, sectionFor func(core.MediaType) (int, bool)) bool {
	if title == nil || sectionFor == nil {
		return false
	}
	id, ok := sectionFor(title.Type)
	if ok && SectionAllowed(v, id) {
		if s.d.PluginLibraries == nil {
			return true
		}
		// Ensure the canonical built-in library itself has not been hidden on this protocol.
		if lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id)); err == nil {
			_, _, pub := s.publishedLibrary(ctx, lib)
			if pub && lib.Source == core.LibraryBuiltin {
				return true
			}
		}
	}
	filterLibs := s.allowedFilterLibraries(ctx, v)
	if len(filterLibs) == 0 {
		return false
	}
	// A published, unrestricted TMDB source includes remote search summaries
	// as well as stored rows. Requiring a row here both hides those summaries
	// and adds a membership query for every card on Home. Viewer safety is
	// still applied by the caller after this library-admission check.
	kind := title.Type
	if kind == core.TypeEpisode || kind == core.TypeSeason {
		kind = core.TypeShow
	}
	for _, lib := range filterLibs {
		if lib.Source != core.LibraryCatalog || filterMediaType(lib) != kind || lib.RulesBroken || len(lib.Rules) != 0 || (lib.VisibilityDefault != "" && lib.VisibilityDefault != "show") {
			continue
		}
		if len(lib.Sources) == 0 {
			return true
		}
		for _, source := range lib.Sources {
			if kind == core.TypeMovie && source == "tmdb-movies" || kind == core.TypeShow && source == "tmdb-tv" {
				return true
			}
		}
	}
	target := title
	if (title.Type == core.TypeSeason || title.Type == core.TypeEpisode) && title.ShowKey != 0 && s.d.CachedGet != nil {
		if show, err := s.d.CachedGet(ctx, title.ShowKey); err == nil && show != nil {
			target = show
		} else {
			return false
		}
	}
	for _, lib := range filterLibs {
		wantType := filterMediaType(lib)
		if target.Type != wantType {
			continue
		}
		if lib.Source == core.LibraryCollection {
			// A collection's shared membership is a semi-join, one indexed
			// read per collection library for the title's key.
			ok, err := s.d.PluginLibraries.MemberOf(ctx, lib, target.Key)
			if err != nil {
				continue
			}
			if ok {
				return true
			}
			continue
		}
		// Membership includes sources and the visible policy in one indexed read.
		member, err := s.d.PluginLibraries.MemberOf(ctx, lib, target.Key)
		if err == nil && member {
			return true
		}

	}
	return false
}

func (s *Service) pluginItem(ctx context.Context, v *core.User, key core.RatingKey) (PluginItemView, error) {
	if s.d.PluginLibraries == nil || !key.IsPluginKey() {
		return PluginItemView{}, core.ErrNotFound
	}
	piv, err := s.d.PluginLibraries.Item(ctx, key)
	if err != nil {
		return PluginItemView{}, core.ErrNotFound
	}
	if !s.pluginItemAllowed(ctx, v, piv) {
		return PluginItemView{}, core.ErrNotFound
	}
	return piv, nil
}

func (s *Service) pluginItemAllowed(ctx context.Context, v *core.User, piv PluginItemView) bool {
	if v == nil || piv.Title.Key == 0 || !v.AllowsLibrary(piv.Library.ID) {
		return false
	}
	if _, _, ok := s.publishedLibrary(ctx, piv.Library); !ok {
		return false
	}
	if v.MaxContentRating != "" && contentrating.EnforceExceeds(contentrating.EffectiveEnforcement(piv.Title.EnforcementRating, piv.Title.ContentRating, piv.Title.Adult, piv.Title.Keywords), v.MaxContentRating) {
		return false
	}
	return TitleAllowed(s.d.Visibility.Policy, v, &piv.Title)
}

// sanitizePluginLibraryQuery removes sorts and filters a plugin library has
// not declared so undeclared caller parameters are ignored gracefully instead
// of returning empty pages or errors.
func (s *Service) sanitizePluginLibraryQuery(ctx context.Context, lib core.Library, q frontend.ItemQuery) frontend.ItemQuery {
	if q.Filter.Person != 0 && !core.IsPluginPersonID(q.Filter.Person) {
		q.Filter.Person = 0
	}
	pres, ok := s.d.PluginLibraries.(PluginPresentationLibraries)
	if !ok {
		return q
	}
	p, ok := pres.Presentation(ctx, lib)
	if !ok {
		return q
	}
	if q.Sort != "" && !q.Sort.IsLastViewed() && !slices.Contains(p.Sorts, string(q.Sort)) {
		q.Sort, q.Desc = "", false
	}
	declared := make(map[string]bool, len(p.Facets))
	for _, vf := range p.Facets {
		declared[vf.ID] = true
	}
	f := core.Filter{
		Query:       q.Filter.Query,
		Person:      q.Filter.Person,
		TabRules:    q.Filter.TabRules,
		TabFallback: q.Filter.TabFallback,
	}
	if len(q.Filter.Genres) > 0 && (declared["genre"] || declared["genres"] || declared["tags"]) {
		f.Genres = q.Filter.Genres
	}
	if q.Filter.Studio != "" && (declared["studio"] || declared["studios"] || declared["publisher"] || declared["author"] || declared["channel"]) {
		f.Studio = q.Filter.Studio
	}
	for k, vals := range q.Filter.Facets {
		if declared[k] && len(vals) > 0 {
			if f.Facets == nil {
				f.Facets = make(map[string][]string)
			}
			f.Facets[k] = vals
		}
	}
	if declared["year"] && (f.Facets == nil || len(f.Facets["year"]) == 0) {
		if q.Filter.YearFrom > 0 && q.Filter.YearFrom == q.Filter.YearTo {
			if f.Facets == nil {
				f.Facets = make(map[string][]string)
			}
			f.Facets["year"] = []string{strconv.Itoa(q.Filter.YearFrom)}
		} else if len(q.Filter.Years) > 0 {
			if f.Facets == nil {
				f.Facets = make(map[string][]string)
			}
			for _, y := range q.Filter.Years {
				f.Facets["year"] = append(f.Facets["year"], strconv.Itoa(y))
			}
		}
	}
	q.Filter = f
	return q
}

// pluginQuerySQLReady reports whether every predicate in q's shape is one
// the plugin item store applies itself in SQL: sort and keyset order, the
// type-to-level mapping, genres/studio/facets, person, tab and library
// policy, watched state, the content-rating cap and excluded keys. Viewer
// predicates are handled separately by pluginBrowseQuery, which folds them
// into the query that reaches the store.
func pluginQuerySQLReady(q frontend.ItemQuery) bool {
	if q.AdjacentTo != 0 || q.StartAt != 0 || q.Season != nil {
		return false
	}
	if q.Favorite != nil && *q.Favorite {
		// Plugin titles are never favourites; the walk answers empty.
		return false
	}
	if len(q.ExcludedTypes) > 0 || len(q.UnservedTypes) > 0 {
		return false
	}
	f := q.Filter
	return len(f.IncludeKeys) == 0 &&
		f.Type == "" &&
		f.YearFrom == 0 && f.YearTo == 0 && len(f.Years) == 0 && len(f.ExcludeYears) == 0 &&
		len(f.ReleasedFilters) == 0 && f.MinRating == 0 && f.MaxRating == 0 && len(f.ExcludeRatings) == 0 &&
		f.MinRuntime == 0 && f.MaxRuntime == 0 && len(f.ExcludeRuntimes) == 0 &&
		len(f.Languages) == 0 && len(f.Countries) == 0 && len(f.ExcludeCountries) == 0 &&
		len(f.ContentRatings) == 0 && len(f.ExcludeContentRatings) == 0 && f.MinContentRating == "" &&
		len(f.IncludeCollectionNames) == 0 && len(f.ExcludeCollectionNames) == 0 &&
		f.Collection == 0 && len(f.Collections) == 0 && len(f.ExcludeCollections) == 0 &&
		len(f.Credits) == 0 && len(f.StudioFilters) == 0 && len(f.TitleFilters) == 0 &&
		f.FirstCharacter == "" &&
		f.NameStartsWith == "" && f.NameStartsWithOrGreater == "" && f.NameLessThan == "" &&
		f.Query == ""
}

// pluginPolicyPassive reports whether the access policy provably admits
// every plugin title for v: no viewer restriction the policy can state,
// and no catalogue viewer rules that judge titles on their facts. Projected
// plugin titles are never Adult, so the mandatory guard adds nothing here.
// With no ViewerRules source a non-nil policy is treated as judging, so a
// partially wired service keeps the in-process check.
func (s *Service) pluginPolicyPassive(v *core.User) bool {
	if s.d.Visibility.Policy == nil {
		return true
	}
	if ViewerRestricted(s.d.Visibility.Policy, v) {
		return false
	}
	if s.d.ViewerRules == nil {
		return false
	}
	_, judged := s.d.ViewerRules(v)
	return !judged
}

// pluginShareCap folds plex.tv share filters to the one clause the plugin
// store can apply: a content-rating cap, and only when the movie and
// television caps agree (one library page can hold either projected type).
func pluginShareCap(movie, television core.Filter) (string, bool) {
	for _, f := range []core.Filter{movie, television} {
		if len(f.ContentRatings) > 0 || len(f.ExcludeContentRatings) > 0 || f.MinContentRating != "" ||
			len(f.IncludeCollectionNames) > 0 || len(f.ExcludeCollectionNames) > 0 {
			return "", false
		}
	}
	if movie.MaxContentRating != television.MaxContentRating {
		return "", false
	}
	return movie.MaxContentRating, true
}

// pluginBrowseQuery folds the viewer's predicates into q so the store
// applies them in SQL before counting and paging: the content-rating cap
// (the viewer's own plus any share cap) via Filter.MaxContentRating,
// policy-blocked genres via Filter.ExcludeGenres, and hidden keys via
// Filter.ExcludeKeys. ok is false when a predicate has no SQL form
// (fact-judging viewer rules, share rating/label clauses), leaving the
// caller on the in-process walk. Blocked keywords need no push: projected
// plugin titles never carry Keywords, so they can never match.
func (s *Service) pluginBrowseQuery(ctx context.Context, v *core.User, lib core.Library, q frontend.ItemQuery) (frontend.ItemQuery, bool) {
	if v == nil || !pluginQuerySQLReady(q) {
		return q, false
	}
	out := q
	out.Filter.MaxContentRating = tightenRating(q.Filter.MaxContentRating, v.MaxContentRating)
	if s.d.PageExclusions == nil {
		if s.pluginPolicyPassive(v) {
			return out, true
		}
		return q, false
	}
	if s.d.ViewerRules != nil {
		if _, judged := s.d.ViewerRules(v); judged {
			return q, false
		}
	} else if s.d.Visibility.Policy != nil && ViewerRestricted(s.d.Visibility.Policy, v) {
		return q, false
	}
	ex := s.d.PageExclusions(ctx, v, s.d.PluginLibraries.RootMediaType(ctx, lib))
	if ex.Err != nil || len(ex.Keys) > maxIndexedExclusionSet || len(ex.ExcludeGenres) > 64 {
		return q, false
	}
	shareCap, pushable := pluginShareCap(ex.Movie, ex.Television)
	if !pushable {
		return q, false
	}
	out.Filter.MaxContentRating = tightenRating(out.Filter.MaxContentRating, shareCap)
	out.Filter.ExcludeGenres = append(append([]string{}, ex.ExcludeGenres...), q.Filter.ExcludeGenres...)
	out.Filter.ExcludeKeys = append(append([]core.RatingKey{}, q.Filter.ExcludeKeys...), ex.Keys...)
	return out, true
}

func pluginCursorPrefix(scope string, q frontend.ItemQuery) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%v|%v|%v|%+v", scope, q.Sort, q.Desc, q.Recursive, q.Types, q.Filter)))
	return hex.EncodeToString(sum[:16]) + "|"
}

func (s *Service) pluginLibraryItems(ctx context.Context, v *core.User, lib core.Library, q frontend.ItemQuery) (core.Page, error) {
	// A studio or performer opened for the first time fills its catalogue
	// before the page is read (MaybeExpand waits, bounded, on the first page),
	// so the viewer sees every title the first time.
	expanding := false
	if !q.SuppressExpand && strings.TrimSpace(q.Filter.Query) == "" {
		if exp, ok := s.d.PluginLibraries.(PluginSliceExpander); ok {
			expanding = exp.MaybeExpand(ctx, lib, q)
		}
	}
	page, err := s.readPluginLibraryItems(ctx, v, lib, q)
	if err != nil {
		return core.Page{}, err
	}
	page.Expanding = expanding
	return page, nil
}

func (s *Service) sortTitlesByLastViewed(ctx context.Context, v *core.User, items []core.Title, desc bool) {
	if len(items) < 2 || v == nil || s.d.State == nil {
		return
	}
	keys := make([]core.RatingKey, len(items))
	for i := range items {
		keys[i] = items[i].Key
	}
	states, err := s.d.State.GetMany(ctx, v.ID, keys)
	if err != nil {
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		ti := states[items[i].Key].LastViewedAt
		tj := states[items[j].Key].LastViewedAt
		if ti.IsZero() != tj.IsZero() {
			return !ti.IsZero()
		}
		if !ti.IsZero() && !ti.Equal(tj) {
			if desc {
				return ti.After(tj)
			}
			return ti.Before(tj)
		}
		return false
	})
}

func (s *Service) readPluginLibraryItems(ctx context.Context, v *core.User, lib core.Library, q frontend.ItemQuery) (core.Page, error) {
	if v != nil && core.CatalogViewerID(ctx) == 0 {
		ctx = core.WithCatalogViewer(ctx, v)
	}
	if query := strings.TrimSpace(q.Filter.Query); query != "" {
		limit := q.Limit
		if limit <= 0 {
			limit = 200
		}
		want := max(q.Offset, 0) + limit
		window := min(max(want*2, 50), 500)
		subQ := q
		subQ.Filter.Query = ""
		hits, err := s.d.PluginLibraries.Search(ctx, lib, query, subQ, window)
		if err != nil {
			return core.Page{}, err
		}
		filtered, err := s.filterPluginTitles(ctx, v, hits, subQ)
		if err != nil {
			return core.Page{}, err
		}
		if ppl, ok := s.d.PluginLibraries.(PluginPeopleLibraries); ok && len(subQ.Filter.Facets) == 0 && len(subQ.Filter.TabRules) == 0 && subQ.Filter.TabFallback == "" {
			_, peopleTitles, pErr := s.searchPluginPeopleInLibraries(ctx, v, []core.Library{lib}, ppl, query, subQ, 20)
			if pErr != nil && errors.Is(pErr, context.Canceled) {
				return core.Page{}, pErr
			}
			if len(peopleTitles) > 0 {
				seen := make(map[core.RatingKey]bool, len(filtered)+len(peopleTitles))
				for _, item := range filtered {
					seen[item.Key] = true
				}
				for _, item := range peopleTitles {
					if !seen[item.Key] {
						seen[item.Key] = true
						filtered = append(filtered, item)
					}
				}
			}
		}
		if q.Sort.IsLastViewed() {
			s.sortTitlesByLastViewed(ctx, v, filtered, q.Desc)
		} else if q.Sort != "" {
			if err := s.sortTitles(ctx, filtered, q); err != nil {
				return core.Page{}, err
			}
		} else {
			sort.SliceStable(filtered, func(i, j int) bool {
				return filtered[i].SearchScore > filtered[j].SearchScore
			})
		}
		return slicePluginTitles(filtered, subQ)
	}

	offset := max(q.Offset, 0)
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}

	// Fast O(limit) keyset path when the store applies every predicate in
	// SQL, viewer predicates included (pluginBrowseQuery folded them in).
	if sqlQ, ok := s.pluginBrowseQuery(ctx, v, lib, q); ok && q.Limit > 0 {
		q = sqlQ
		viewerID := int64(0)
		if v != nil {
			viewerID = v.ID
		}
		prefix := pluginCursorPrefix(fmt.Sprintf("lib:%d:u:%d", lib.ID, viewerID), q)
		cursor := ""
		curOffset := 0
		if offset > 0 && s.pluginCursors != nil {
			if cached, ok := s.pluginCursors.get(prefix + strconv.Itoa(offset)); ok {
				cursor = cached
				curOffset = offset
			}
		}
		// Advance keyset cursor up to offset without retaining skipped pages.
		for curOffset < offset {
			step := min(offset-curOffset, 500)
			page, err := s.d.PluginLibraries.Browse(ctx, lib, q, cursor, step)
			if err != nil {
				return core.Page{}, err
			}
			if len(page.Items) == 0 || page.NextCursor == "" {
				return core.Page{Items: []core.Title{}, Total: page.Total}, nil
			}
			curOffset += len(page.Items)
			cursor = page.NextCursor
			if s.pluginCursors != nil {
				s.pluginCursors.put(prefix+strconv.Itoa(curOffset), cursor)
			}
		}
		page, err := s.d.PluginLibraries.Browse(ctx, lib, q, cursor, limit)
		if err != nil {
			return core.Page{}, err
		}
		if page.NextCursor != "" && s.pluginCursors != nil {
			s.pluginCursors.put(prefix+strconv.Itoa(offset+len(page.Items)), page.NextCursor)
		}
		if page.Items == nil {
			page.Items = []core.Title{}
		}
		return core.Page{Items: page.Items, Total: page.Total}, nil
	}

	// In-process keyset scan for the predicates with no SQL form (anchors,
	// fact-judging viewer rules, share rating/label clauses). It runs to
	// exhaustion so the total is the true filtered count, never a capped
	// count presented as the total; NoTotal callers still stop once the
	// page is filled.
	var permitted []core.Title
	cursor := ""
	needAll := q.AdjacentTo != 0 || q.StartAt != 0 || q.Limit <= 0
	wantEnd := offset + limit
	totalMatched := 0
	for {
		page, err := s.d.PluginLibraries.Browse(ctx, lib, q, cursor, 200)
		if err != nil {
			return core.Page{}, err
		}
		batch, err := s.filterPluginTitles(ctx, v, page.Items, q)
		if err != nil {
			return core.Page{}, err
		}
		for _, item := range batch {
			if needAll || (totalMatched >= offset && len(permitted) < limit) {
				permitted = append(permitted, item)
			}
			totalMatched++
		}
		if page.NextCursor == "" || len(page.Items) == 0 || page.NextCursor == cursor {
			break
		}
		if !needAll && totalMatched >= wantEnd && q.NoTotal {
			break
		}
		cursor = page.NextCursor
	}
	if !needAll {
		if permitted == nil {
			permitted = []core.Title{}
		}
		return core.Page{Items: permitted, Total: totalMatched}, nil
	}
	return slicePluginTitles(permitted, q)
}

func (s *Service) pluginChildren(ctx context.Context, v *core.User, parent core.RatingKey, q frontend.ItemQuery) (core.Page, error) {
	if _, err := s.pluginItem(ctx, v, parent); err != nil {
		return core.Page{}, err
	}
	offset := max(q.Offset, 0)
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}

	// Children carries no ItemQuery to the store, so its SQL page can only
	// be trusted when no viewer predicate would filter it in process.
	if v != nil && pluginQuerySQLReady(q) && q.Watched == nil && v.MaxContentRating == "" && q.Filter.MaxContentRating == "" &&
		len(q.Filter.ExcludeKeys) == 0 && len(q.Filter.ExcludeGenres) == 0 && s.pluginPolicyPassive(v) && q.Sort == "" && q.Limit > 0 {
		prefix := pluginCursorPrefix("child:"+parent.String(), q)
		cursor := ""
		curOffset := 0
		if offset > 0 && s.pluginCursors != nil {
			if cached, ok := s.pluginCursors.get(prefix + strconv.Itoa(offset)); ok {
				cursor = cached
				curOffset = offset
			}
		}
		for curOffset < offset {
			step := min(offset-curOffset, 500)
			page, err := s.d.PluginLibraries.Children(ctx, parent, q.Recursive, cursor, step)
			if err != nil {
				return core.Page{}, err
			}
			if len(page.Items) == 0 || page.NextCursor == "" {
				return core.Page{Items: []core.Title{}, Total: page.Total}, nil
			}
			curOffset += len(page.Items)
			cursor = page.NextCursor
			if s.pluginCursors != nil {
				s.pluginCursors.put(prefix+strconv.Itoa(curOffset), cursor)
			}
		}
		page, err := s.d.PluginLibraries.Children(ctx, parent, q.Recursive, cursor, limit)
		if err != nil {
			return core.Page{}, err
		}
		if page.NextCursor != "" && s.pluginCursors != nil {
			s.pluginCursors.put(prefix+strconv.Itoa(offset+len(page.Items)), page.NextCursor)
		}
		if page.Items == nil {
			page.Items = []core.Title{}
		}
		return core.Page{Items: page.Items, Total: page.Total}, nil
	}

	var all []core.Title
	cursor := ""
	for {
		page, err := s.d.PluginLibraries.Children(ctx, parent, q.Recursive, cursor, 200)
		if err != nil {
			return core.Page{}, err
		}
		batch, err := s.filterPluginTitles(ctx, v, page.Items, q)
		if err != nil {
			return core.Page{}, err
		}
		all = append(all, batch...)
		if page.NextCursor == "" || len(page.Items) == 0 || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	if q.Sort != "" {
		if err := s.sortTitles(ctx, all, q); err != nil {
			return core.Page{}, err
		}
	}
	return slicePluginTitles(all, q)
}

func (s *Service) filterPluginTitles(ctx context.Context, v *core.User, items []core.Title, q frontend.ItemQuery) ([]core.Title, error) {
	if len(items) == 0 {
		return nil, nil
	}
	var watched map[core.RatingKey]core.WatchStatus
	if q.Watched != nil {
		keys := make([]core.RatingKey, len(items))
		for i := range items {
			keys[i] = items[i].Key
		}
		var err error
		watched, err = s.d.State.GetMany(ctx, v.ID, keys)
		if err != nil {
			return nil, err
		}
	}
	// Plugin library titles are not favourites in this build: they are not in
	// the catalogue the favourites policy reads, so favouriting one is refused
	// and none is ever stored. Honour the filter directly rather than asking
	// the favourites store, whose policy check answers ErrForbidden for keys it
	// cannot resolve and would fail the whole page (Jellyfin answered 503 to
	// `Filters=IsFavorite` on a plugin library).
	if q.Favorite != nil && *q.Favorite {
		return []core.Title{}, nil
	}
	sub := q
	sub.Watched = nil
	sub.Favorite = nil
	out := make([]core.Title, 0, len(items))
	for _, t := range items {
		if v.MaxContentRating != "" && contentrating.EnforceExceeds(contentrating.EffectiveEnforcement(t.EnforcementRating, t.ContentRating, t.Adult, t.Keywords), v.MaxContentRating) {
			continue
		}
		if !TitleAllowed(s.d.Visibility.Policy, v, &t) {
			continue
		}
		if q.Watched != nil && (watched[t.Key].ViewCount > 0) != *q.Watched {
			continue
		}
		ok, err := s.matchesUserQuery(ctx, v, t, sub)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func slicePluginTitles(items []core.Title, q frontend.ItemQuery) (core.Page, error) {
	if q.AdjacentTo != 0 {
		return adjacentTitles(items, q)
	}
	if q.StartAt != 0 {
		return startingTitles(items, q)
	}
	total := len(items)
	start := min(max(q.Offset, 0), total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	slice := items[start:end]
	if slice == nil {
		slice = []core.Title{}
	}
	return core.Page{Items: slice, Total: total}, nil
}

func (s *Service) pluginAncestors(ctx context.Context, v *core.User, key core.RatingKey) ([]core.Title, error) {
	if _, err := s.pluginItem(ctx, v, key); err != nil {
		return nil, err
	}
	ancestors, lib, err := s.d.PluginLibraries.Ancestors(ctx, key)
	if err != nil {
		return nil, err
	}
	out := make([]core.Title, 0, len(ancestors))
	for _, a := range ancestors {
		piv := PluginItemView{Library: lib, Title: a}
		if !s.pluginItemAllowed(ctx, v, piv) {
			return nil, core.ErrNotFound
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Service) pluginLibraryFacets(ctx context.Context, viewer *core.Viewer, lib core.Library, name string) ([]core.FacetValue, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if !v.AllowsLibrary(lib.ID) {
		return nil, core.ErrNotFound
	}
	if _, _, ok := s.publishedLibrary(ctx, lib); !ok {
		return nil, core.ErrNotFound
	}
	return s.d.PluginLibraries.Facets(ctx, lib, name)
}

func (s *Service) filterLibraryFacets(ctx context.Context, viewer *core.Viewer, lib core.Library, name string) ([]core.FacetValue, bool, error) {
	if s.d.FacetFilter == nil || s.d.PageExclusions == nil {
		return nil, false, nil
	}
	sqlFacet := sqlFacets[name]
	if sqlFacet == "" {
		return nil, false, nil
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, false, err
	}
	if !v.AllowsLibrary(lib.ID) {
		return nil, false, core.ErrNotFound
	}
	mt, _, ok := s.publishedLibrary(ctx, lib)
	if !ok {
		return nil, false, core.ErrNotFound
	}
	ex := s.d.PageExclusions(ctx, v, mt)
	if ex.Err != nil {
		return nil, false, ex.Err
	}
	base := s.libraryBrowseFilter(ctx, v, lib, core.Filter{Type: mt})
	filter, fits, none := viewerBrowseFilter(base, v, ex, mt)
	if !fits {
		return nil, false, nil
	}
	if none {
		return []core.FacetValue{}, true, nil
	}
	vals, err := s.d.FacetFilter(ctx, filter, sqlFacet)
	if err != nil {
		return nil, false, err
	}
	for i := range vals {
		vals[i].ID = facetID(name, vals[i].Key)
	}
	return vals, true, nil
}

func (s *Service) pluginSearchItems(ctx context.Context, v *core.User, query string, q frontend.ItemQuery, window int) ([]core.Title, error) {
	if s.d.PluginLibraries == nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	libs, err := s.d.PluginLibraries.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	subQ := q
	subQ.Filter.Query = ""
	var out []core.Title
	seen := make(map[core.RatingKey]bool)
	for _, lib := range libs {
		if lib.Source != core.LibraryPlugin || !v.AllowsLibrary(lib.ID) {
			continue
		}
		if _, _, ok := s.publishedLibrary(ctx, lib); !ok {
			continue
		}
		hits, err := s.d.PluginLibraries.Search(ctx, lib, query, subQ, window)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			continue
		}
		filtered, err := s.filterPluginTitles(ctx, v, hits, subQ)
		if err != nil {
			return nil, err
		}
		for _, item := range filtered {
			if !seen[item.Key] {
				seen[item.Key] = true
				out = append(out, item)
			}
		}
	}
	_, peopleTitles, err := s.searchPluginPeopleForUser(ctx, v, query, subQ, 20)
	if err != nil && errors.Is(err, context.Canceled) {
		return nil, err
	}
	for _, item := range peopleTitles {
		if !seen[item.Key] {
			seen[item.Key] = true
			out = append(out, item)
		}
	}
	if len(out) > 1 {
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].SearchScore > out[j].SearchScore
		})
	}
	return out, nil
}

func (s *Service) builtinNative(mt core.MediaType) string {
	if s.d.BuiltinNative != nil {
		return s.d.BuiltinNative(mt)
	}
	return string(mt)
}

func (s *Service) presentLibrary(p core.Presentations, mt core.MediaType) (core.MediaType, string, bool) {
	if s.d.LibraryPresentation == nil {
		return "", "", false
	}
	return s.d.LibraryPresentation(p, mt)
}

func (s *Service) publishedLibraryByViewID(ctx context.Context, v *core.User, viewID string) (core.Library, core.MediaType, error) {
	if s.d.PluginLibraries == nil || v == nil {
		return core.Library{}, "", core.ErrNotFound
	}
	idNum, err := strconv.ParseInt(strings.TrimSpace(viewID), 10, 64)
	if err != nil || idNum <= 0 {
		return core.Library{}, "", core.ErrNotFound
	}
	lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(idNum))
	if err != nil || !v.AllowsLibrary(lib.ID) {
		return core.Library{}, "", core.ErrNotFound
	}
	mt, _, ok := s.publishedLibrary(ctx, lib)
	if !ok {
		return core.Library{}, "", core.ErrNotFound
	}
	return lib, mt, nil
}

func (s *Service) pluginLibraryByViewID(ctx context.Context, v *core.User, viewID string) (core.Library, PluginPresentationLibraries, PluginPresentation, error) {
	lib, _, err := s.publishedLibraryByViewID(ctx, v, viewID)
	if err != nil || lib.Source != core.LibraryPlugin {
		return core.Library{}, nil, PluginPresentation{}, core.ErrNotFound
	}
	pres, ok := s.d.PluginLibraries.(PluginPresentationLibraries)
	if !ok {
		return lib, nil, PluginPresentation{}, nil
	}
	p, _ := pres.Presentation(ctx, lib)
	return lib, pres, p, nil
}

func (s *Service) catalogueFacetQueryFilter(ctx context.Context, v *core.User, lib core.Library, mt core.MediaType) (core.Filter, bool, error) {
	base := s.libraryBrowseFilter(ctx, v, lib, core.Filter{Type: mt})
	if s.d.PageExclusions != nil {
		ex := s.d.PageExclusions(ctx, v, mt)
		if ex.Err != nil {
			return core.Filter{}, false, ex.Err
		}
		filter, fits, none := viewerBrowseFilter(base, v, ex, mt)
		if none {
			return filter, true, nil
		}
		if fits {
			return filter, false, nil
		}
	}
	if v != nil {
		base.CatalogViewerID = v.ID
		base.MaxContentRating = tightenRating(base.MaxContentRating, v.MaxContentRating)
	}
	return base, false, nil
}

// LibraryFacetBuckets implements frontend.LibraryFacetBuckets.
func (s *Service) LibraryFacetBuckets(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID, field string) ([]frontend.FacetBucket, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	lib, pres, p, err := s.pluginLibraryByViewID(ctx, v, viewID)
	if err != nil || pres == nil {
		return nil, core.ErrNotFound
	}
	declared := false
	for _, f := range p.Facets {
		if f.ID == field {
			declared = true
			break
		}
	}
	if !declared {
		return nil, core.ErrNotFound
	}
	return pres.FacetBuckets(ctx, lib, field)
}

// LibraryFacetCollections implements frontend.LibraryFacetCollections.
func (s *Service) LibraryFacetCollections(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID, field, order string, page frontend.PageQuery) (frontend.FacetCollectionPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	lib, mt, err := s.publishedLibraryByViewID(ctx, v, viewID)
	if err != nil {
		return frontend.FacetCollectionPage{}, core.ErrNotFound
	}
	if lib.Source == core.LibraryPlugin {
		pres, ok := s.d.PluginLibraries.(PluginPresentationLibraries)
		if !ok || pres == nil {
			return frontend.FacetCollectionPage{}, core.ErrNotFound
		}
		p, _ := pres.Presentation(ctx, lib)
		declared := false
		for _, f := range p.Facets {
			if f.ID == field {
				declared = true
				break
			}
		}
		if !declared {
			return frontend.FacetCollectionPage{}, core.ErrNotFound
		}
		out, err := pres.FacetCollections(ctx, lib, field, order, page.Query, page.Offset, page.Limit)
		if err != nil {
			return frontend.FacetCollectionPage{}, err
		}
		for i := range out.Items {
			if out.Items[i].NewestItem != nil {
				piv := PluginItemView{Library: lib, Title: *out.Items[i].NewestItem}
				if !s.pluginItemAllowed(ctx, v, piv) {
					out.Items[i].NewestItem = nil
				}
			}
		}
		return out, nil
	}

	canonFacet, ok := core.NormalizeCatalogueTabFacet(core.TabCollections, field)
	if !ok || canonFacet == "" {
		return frontend.FacetCollectionPage{}, core.ErrNotFound
	}
	catReader, ok := s.d.Catalog.(catalogTabFacetReader)
	if !ok || catReader == nil {
		return frontend.FacetCollectionPage{}, core.ErrNotFound
	}
	filter, none, err := s.catalogueFacetQueryFilter(ctx, v, lib, mt)
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	if none {
		return frontend.FacetCollectionPage{Items: []frontend.FacetCollection{}}, nil
	}
	raw, err := catReader.LibraryFacetCollections(ctx, core.CatalogFacetCollectionsQuery{
		Filter: filter,
		Facet:  canonFacet,
		Order:  order,
		Query:  page.Query,
		Offset: page.Offset,
		Limit:  page.Limit,
	})
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	items := make([]frontend.FacetCollection, 0, len(raw.Items))
	for _, entry := range raw.Items {
		var newest *core.Title
		if entry.NewestItem != nil {
			t := *entry.NewestItem
			if s.visible(ctx, v, &t) {
				newest = &t
			}
		}
		items = append(items, frontend.FacetCollection{
			ID:         entry.ID,
			Name:       entry.Name,
			ItemCount:  entry.ItemCount,
			NewestItem: newest,
		})
	}
	return frontend.FacetCollectionPage{Items: items, More: raw.More}, nil
}

// LibraryFacetPeople implements frontend.LibraryFacetPeople.
func (s *Service) LibraryFacetPeople(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID, field, order string, page frontend.PageQuery) (frontend.FacetPersonPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.FacetPersonPage{}, err
	}
	lib, mt, err := s.publishedLibraryByViewID(ctx, v, viewID)
	if err != nil {
		return frontend.FacetPersonPage{}, core.ErrNotFound
	}
	if lib.Source == core.LibraryPlugin {
		pres, ok := s.d.PluginLibraries.(PluginPresentationLibraries)
		if !ok || pres == nil {
			return frontend.FacetPersonPage{}, core.ErrNotFound
		}
		p, _ := pres.Presentation(ctx, lib)
		declaredPeople := false
		for _, f := range p.Facets {
			if f.ID == field && f.Type == "people" {
				declaredPeople = true
				break
			}
		}
		if !declaredPeople {
			return frontend.FacetPersonPage{}, core.ErrNotFound
		}
		raw, err := pres.FacetPeople(ctx, lib, field, order, page.Query, page.Offset, page.Limit)
		if err != nil {
			return frontend.FacetPersonPage{}, err
		}
		items := make([]frontend.FacetPerson, 0, len(raw.Items))
		for _, entry := range raw.Items {
			if !core.IsPluginPersonID(entry.PersonID) || strings.TrimSpace(entry.Name) == "" || entry.Count <= 0 {
				continue
			}
			var artKey core.RatingKey
			if entry.SampleTitle != nil {
				piv := PluginItemView{Library: lib, Title: *entry.SampleTitle}
				if s.pluginItemAllowed(ctx, v, piv) {
					artKey = entry.SampleTitle.Key
				}
			}
			items = append(items, frontend.FacetPerson{
				Person: frontend.PersonDetail{
					ID:          personFacetID(entry.PersonID),
					Name:        entry.Name,
					TMDB:        entry.PersonID,
					ArtworkItem: artKey,
				},
				ItemCount: entry.Count,
			})
		}
		return frontend.FacetPersonPage{Items: items, More: raw.More}, nil
	}

	canonFacet, ok := core.NormalizeCatalogueTabFacet(core.TabPeople, field)
	if !ok || canonFacet == "" {
		return frontend.FacetPersonPage{}, core.ErrNotFound
	}
	catReader, ok := s.d.Catalog.(catalogTabFacetReader)
	if !ok || catReader == nil {
		return frontend.FacetPersonPage{}, core.ErrNotFound
	}
	filter, none, err := s.catalogueFacetQueryFilter(ctx, v, lib, mt)
	if err != nil {
		return frontend.FacetPersonPage{}, err
	}
	if none {
		return frontend.FacetPersonPage{Items: []frontend.FacetPerson{}}, nil
	}
	raw, err := catReader.LibraryFacetPeople(ctx, core.CatalogFacetPeopleQuery{
		Filter: filter,
		Facet:  canonFacet,
		Order:  order,
		Query:  page.Query,
		Offset: page.Offset,
		Limit:  page.Limit,
	})
	if err != nil {
		return frontend.FacetPersonPage{}, err
	}
	items := make([]frontend.FacetPerson, 0, len(raw.Items))
	for _, entry := range raw.Items {
		if entry.PersonTMDB <= 0 || strings.TrimSpace(entry.Name) == "" || entry.ItemCount <= 0 {
			continue
		}
		items = append(items, frontend.FacetPerson{
			Person: frontend.PersonDetail{
				ID:          personFacetID(entry.PersonTMDB),
				Name:        entry.Name,
				TMDB:        entry.PersonTMDB,
				ArtworkItem: entry.SampleKey,
			},
			ItemCount: entry.ItemCount,
		})
	}
	return frontend.FacetPersonPage{Items: items, More: raw.More}, nil
}

// PluginLibraryCandidateRows builds the candidate shelves (20 items each) for a
// plugin library's Recommended tab before per-library layout policies run.
func (s *Service) PluginLibraryCandidateRows(ctx context.Context, viewer *core.Viewer, viewID string) ([]frontend.Row, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	rows, _, err := s.pluginLibraryCandidateRows(ctx, v, viewID)
	return rows, err
}

func (s *Service) pluginLibraryCandidateRows(ctx context.Context, v *core.User, viewID string) ([]frontend.Row, bool, error) {
	lib, pres, p, err := s.pluginLibraryByViewID(ctx, v, viewID)
	if err != nil {
		return nil, false, err
	}
	const (
		maxCandidates = 16
		rowLimit      = 20
	)
	declaredShelves := len(p.Shelves) > 0
	var rows []frontend.Row
	appendRow := func(id, title string, purpose frontend.RowPurpose, kind string, items []core.Title) {
		if len(items) == 0 || (!declaredShelves && len(rows) >= maxCandidates) {
			return
		}
		if len(items) > rowLimit {
			items = items[:rowLimit]
		}
		copied := make([]core.Title, len(items))
		copy(copied, items)
		entries := make([]frontend.RowEntry, len(copied))
		for i := range copied {
			entries[i] = frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &copied[i]}
		}
		rows = append(rows, frontend.Row{
			ID:      id,
			Title:   title,
			Purpose: purpose,
			Kind:    kind,
			Style:   "shelf",
			Entries: entries,
			Items:   copied,
		})
	}

	// 1. Continue watching.
	if s.d.State != nil {
		if sts, sErr := s.d.State.InProgress(ctx, v.ID, 200); sErr == nil && len(sts) > 0 {
			var keys []core.RatingKey
			for _, st := range sts {
				if st.ViewOffset > 0 && st.Key.IsPluginKey() {
					keys = append(keys, st.Key)
				}
			}
			if len(keys) > 0 {
				if pivs, pErr := s.d.PluginLibraries.ItemsMany(ctx, keys); pErr == nil {
					var cont []core.Title
					for _, k := range keys {
						piv, ok := pivs[k]
						if !ok || piv.Library.ID != lib.ID || !s.pluginItemAllowed(ctx, v, piv) {
							continue
						}
						cont = append(cont, piv.Title)
						if len(cont) >= rowLimit {
							break
						}
					}
					appendRow("plugin:"+viewID+":continue", "Continue Watching", frontend.RowContinue, "continue", cont)
				}
			}
		}
	}

	// Plugin-declared default shelves replace the auto-generated shelves and
	// are not clamped to the auto candidate cap.
	if declaredShelves {
		for _, sh := range p.Shelves {
			sortOrder := sh.Sort
			if sortOrder == "" {
				sortOrder = core.SortReleased
			}
			desc := sh.Direction != "asc"
			q := frontend.ItemQuery{
				Sort:           sortOrder,
				Desc:           desc,
				Limit:          rowLimit,
				NoTotal:        true,
				SuppressExpand: true,
			}
			if len(sh.Filters) > 0 {
				facets := make(map[string][]string, len(sh.Filters))
				for k, vals := range sh.Filters {
					facets[k] = append([]string(nil), vals...)
				}
				q.Filter = core.Filter{Facets: facets}
			}
			page, pErr := s.pluginLibraryItems(ctx, v, lib, q)
			if pErr != nil || len(page.Items) == 0 {
				continue
			}
			title := sh.Label
			if strings.TrimSpace(title) == "" {
				title = sh.Key
			}
			appendRow("plugin:"+viewID+":shelf:"+sh.Key, title, frontend.RowRecommendation, "shelf", page.Items)
		}
		if rows == nil {
			return []frontend.Row{}, true, nil
		}
		return rows, true, nil
	}

	// 2. Newest.
	if page, pErr := s.pluginLibraryItems(ctx, v, lib, frontend.ItemQuery{
		Sort:    core.SortReleased,
		Desc:    true,
		Limit:   rowLimit,
		NoTotal: true,
	}); pErr == nil {
		appendRow("plugin:"+viewID+":released", "Newest", frontend.RowRecentlyReleased, "recentlyReleased", page.Items)
	}

	// 3. Recently added.
	if page, pErr := s.pluginLibraryItems(ctx, v, lib, frontend.ItemQuery{
		Sort:    core.SortAdded,
		Desc:    true,
		Limit:   rowLimit,
		NoTotal: true,
	}); pErr == nil {
		appendRow("plugin:"+viewID+":added", "Recently Added", frontend.RowRecentlyAdded, "recentlyAdded", page.Items)
	}

	// 4. Top rated (when rating is a declared sort).
	if slices.Contains(p.Sorts, "rating") && len(rows) < maxCandidates {
		if page, pErr := s.pluginLibraryItems(ctx, v, lib, frontend.ItemQuery{
			Sort:    core.SortRating,
			Desc:    true,
			Limit:   rowLimit,
			NoTotal: true,
		}); pErr == nil {
			appendRow("plugin:"+viewID+":rating", "Top Rated", frontend.RowRecommendation, "topRated", page.Items)
		}
	}

	// 5. Facet shelves: a few from the first declared facet, then a few from
	// the next non-people string facet (such as genres or tags). Buckets come
	// from the precomputed facet tallies behind FacetBuckets; each facet's
	// largest buckets form a small pool and the day's shelves are picked from
	// that pool by a per-library daily seed (see dailyFacetPool), so the
	// shelves rotate daily and equal-size buckets never fall back to A-Z.
	// Probing stays bounded per facet so viewer restrictions never walk
	// hundreds of buckets.
	if pres != nil && len(p.Facets) > 0 && len(rows) < maxCandidates {
		now := time.Now()
		if s.d.Now != nil {
			now = s.d.Now()
		}
		day := now.Local().Format("2006-01-02")
		shelfFacets := []frontend.ViewFacet{p.Facets[0]}
		if facet, ok := nextNonPeopleStringFacet(p.Facets); ok {
			shelfFacets = append(shelfFacets, facet)
		}
		for _, facet := range shelfFacets {
			if len(rows) >= maxCandidates {
				break
			}
			buckets, bErr := pres.FacetBuckets(ctx, lib, facet.ID)
			if bErr != nil || len(buckets) == 0 {
				continue
			}
			pool := dailyFacetPool(buckets, lib.ID, facet.ID, day, pluginFacetPoolSize)
			added := 0
			for tried, b := range pool {
				if len(rows) >= maxCandidates || added >= pluginFacetShelves || tried >= pluginFacetProbes {
					break
				}
				page, pErr := s.pluginLibraryItems(ctx, v, lib, frontend.ItemQuery{
					Sort:           core.SortReleased,
					Desc:           true,
					Limit:          rowLimit,
					NoTotal:        true,
					SuppressExpand: true,
					Filter: core.Filter{
						Facets: map[string][]string{facet.ID: {b.ID}},
					},
				})
				if pErr != nil || len(page.Items) == 0 {
					continue
				}
				title := b.Label
				if isOriginFacetKey(facet.ID) {
					title = "From " + b.Label
				}
				before := len(rows)
				appendRow("plugin:"+viewID+":facet:"+facet.ID+":"+b.ID, title, frontend.RowRecommendation, "facet", page.Items)
				if len(rows) > before {
					added++
				}
			}
		}
	}
	if rows == nil {
		return []frontend.Row{}, false, nil
	}
	return rows, false, nil
}

func pluginLibraryHasCustomMaxRows(source core.NeutralHomeLayoutSource, ctx context.Context, userID int64, section, device string) bool {
	reader, ok := source.(interface {
		HomeLayout(context.Context) (core.HomeLayout, error)
	})
	if !ok {
		return false
	}
	d, err := reader.HomeLayout(ctx)
	if err != nil {
		return false
	}
	check := func(pages []core.LayoutPage) bool {
		for _, p := range pages {
			if p.Section != section {
				continue
			}
			if p.MaxRows > 0 || (device != "" && p.DeviceMaxRows[device] > 0) {
				return true
			}
		}
		return false
	}
	if check(d.Global) {
		return true
	}
	for _, vl := range d.Viewers {
		if vl.UserID == userID && check(vl.Pages) {
			return true
		}
	}
	return false
}

// PluginLibraryRows implements frontend.PluginLibraryRowsReader: shelves built
// from the plugin library's stored items, arranged by that library's own
// "library:<id>" layout page (defaulting to 8 shelves for auto-generated rows
// when uncustomised; declared shelves are not clamped to 8).
func (s *Service) PluginLibraryRows(ctx context.Context, viewer *core.Viewer, dev core.Device, viewID string) ([]frontend.Row, error) {
	const defaultMaxRows = 8
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	rows, declaredShelves, err := s.pluginLibraryCandidateRows(ctx, v, viewID)
	if err != nil {
		return nil, err
	}
	if s.d.Layout == nil {
		if !declaredShelves && len(rows) > defaultMaxRows {
			rows = rows[:defaultMaxRows]
		}
		return rows, nil
	}
	id, _ := strconv.Atoi(viewID)
	now := time.Now()
	if s.d.Now != nil {
		now = s.d.Now()
	}
	hc := core.NewHubContext(now.Local(), dev.Product, dev.Platform, dev.DeviceName)
	req := frontend.RowRequest{
		Viewer:    v,
		Device:    dev,
		Placement: frontend.PlacementLibrary,
		ViewID:    viewID,
	}
	sit := core.HomeSituation{
		LibraryID: id,
		Device:    hc.Device,
		Daypart:   hc.Daypart,
		Weekend:   hc.Weekend,
		Now:       hc.Now,
		Product:   dev.Product,
		Platform:  dev.Platform,
		ClientID:  dev.ID,
	}
	if declaredShelves && len(rows) > 0 {
		sit.DefaultMaxRows = len(rows)
	}
	arranged, err := LayoutRows(ctx, s.d.Layout, nil, req, sit, rows)
	if err != nil {
		return nil, err
	}
	section := "library:" + strconv.Itoa(id)
	if !declaredShelves && !pluginLibraryHasCustomMaxRows(s.d.Layout, ctx, v.ID, section, hc.Device) && len(arranged) > defaultMaxRows {
		kept := make([]frontend.Row, 0, defaultMaxRows)
		for _, r := range arranged {
			if len(kept) < defaultMaxRows || r.Layout.Always {
				r.Rank = len(kept)
				r.Layout.Order = len(kept)
				kept = append(kept, r)
			}
		}
		arranged = kept
	}
	return arranged, nil
}

const (
	// pluginFacetPoolSize caps how many of a facet's largest buckets are
	// eligible for a day's Recommended shelves.
	pluginFacetPoolSize = 24
	// pluginFacetShelves caps the shelves taken from one facet.
	pluginFacetShelves = 4
	// pluginFacetProbes caps bucket probes per facet, so buckets emptied by
	// viewer restrictions are skipped without walking the whole pool.
	pluginFacetProbes = 12
)

// dailyFacetPool returns up to poolSize of the largest buckets in the order
// they should be probed for shelves on day (a "2006-01-02" date). The pool is
// formed by bucket count, with a hash of (library, day, facet, bucket)
// breaking count ties, and the pool itself is ordered by that same hash. The
// result is stable within a day, differs between libraries, rotates daily,
// and never depends on bucket labels, so many equal-size buckets (such as
// developers with similar catalogue sizes) are not always won by the labels
// that sort first.
func dailyFacetPool(buckets []frontend.FacetBucket, libID core.LibraryID, facetID, day string, poolSize int) []frontend.FacetBucket {
	if len(buckets) == 0 || poolSize <= 0 {
		return nil
	}
	type keyed struct {
		bucket frontend.FacetBucket
		key    string
	}
	keyedBuckets := make([]keyed, len(buckets))
	for i, b := range buckets {
		id := b.ID
		if id == "" {
			id = b.Label
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", int64(libID), day, facetID, id)))
		keyedBuckets[i] = keyed{bucket: b, key: hex.EncodeToString(sum[:])}
	}
	sort.SliceStable(keyedBuckets, func(i, j int) bool {
		if keyedBuckets[i].bucket.Count != keyedBuckets[j].bucket.Count {
			return keyedBuckets[i].bucket.Count > keyedBuckets[j].bucket.Count
		}
		return keyedBuckets[i].key < keyedBuckets[j].key
	})
	if len(keyedBuckets) > poolSize {
		keyedBuckets = keyedBuckets[:poolSize]
	}
	sort.SliceStable(keyedBuckets, func(i, j int) bool {
		return keyedBuckets[i].key < keyedBuckets[j].key
	})
	out := make([]frontend.FacetBucket, len(keyedBuckets))
	for i, k := range keyedBuckets {
		out[i] = k.bucket
	}
	return out
}

// nextNonPeopleStringFacet returns the first facet after facets[0] whose type
// is a string list (genres, tags and similar), skipping people and
// numeric/date facets whose buckets would not read as shelves.
func nextNonPeopleStringFacet(facets []frontend.ViewFacet) (frontend.ViewFacet, bool) {
	if len(facets) < 2 {
		return frontend.ViewFacet{}, false
	}
	for _, facet := range facets[1:] {
		if facet.Type == "strings" || facet.Type == "string" {
			return facet, true
		}
	}
	return frontend.ViewFacet{}, false
}

func isOriginFacetKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "developer", "developers", "publisher", "publishers", "channel", "channels", "network", "networks", "author", "authors":
		return true
	default:
		return false
	}
}
