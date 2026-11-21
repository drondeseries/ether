package core

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// LibraryID identifies one library. 1 = Movies, 2 = TV Shows forever;
// ids are never reused.
type LibraryID int64

const (
	LibraryMovies LibraryID = 1
	LibraryShows  LibraryID = 2
)

// LibrarySource says where a library's items come from.
type LibrarySource string

const (
	LibraryBuiltin    LibrarySource = "builtin"    // the TMDB catalogue, split by MediaType
	LibraryPlugin     LibrarySource = "plugin"     // items owned by one plugin MediaKind instance
	LibraryCatalog    LibrarySource = "catalogue"  // owner-selected catalogue sources
	LibraryCollection LibrarySource = "collection" // exactly the titles of one owner collection
)

// Library card shapes.
const (
	CardPoster    = "poster"
	CardLandscape = "landscape"
	CardSquare    = "square"
)

// ValidCardShape reports whether s is a valid stored Library.CardShape value
// ("" for the library's default, or "poster" | "landscape" | "square").
func ValidCardShape(s string) bool {
	switch s {
	case "", CardPoster, CardLandscape, CardSquare:
		return true
	default:
		return false
	}
}

// Library date display formats.
const (
	DateDisplayYear = "year"
	DateDisplayDate = "date"
)

// ValidDateDisplay reports whether s is a valid library dateDisplay value.
func ValidDateDisplay(s string) bool {
	return s == "" || s == DateDisplayYear || s == DateDisplayDate
}

// Library tab view layouts (used on kind: "view" tabs).
const (
	TabLayoutGrid     = "grid"
	TabLayoutList     = "list"
	TabLayoutTimeline = "timeline"
)

// ValidTabLayout reports whether s is a valid stored LibraryTab.Layout value
// ("" for default "grid", or "grid" | "list" | "timeline").
func ValidTabLayout(s string) bool {
	switch s {
	case "", TabLayoutGrid, TabLayoutList, TabLayoutTimeline:
		return true
	default:
		return false
	}
}

// Library tab kinds.
const (
	TabRecommended = "recommended"
	TabLibrary     = "library"
	TabCollections = "collections"
	TabPeople      = "people"
	TabView        = "view"
	TabLive        = "live"
)

// TabFilter narrows a library or view tab using the same rule model as
// collections (match/rules/exclude) and custom libraries (ordered show/hide
// rule groups). Either representation may be sent; ToPolicy compiles both into
// ordered LibraryExclusion rules for libraryfilter.
type TabFilter struct {
	Match             string             `json:"match,omitempty"`   // "all" (default) or "any" for Rules
	Rules             []CollectionRule   `json:"rules,omitempty"`   // positive conditions
	Exclude           []CollectionRule   `json:"exclude,omitempty"` // negative conditions (any match hides)
	Groups            []LibraryExclusion `json:"groups,omitempty"`  // ordered show/hide groups (custom-library style)
	VisibilityDefault string             `json:"visibilityDefault,omitempty"`
}

// UnmarshalJSON accepts either a TabFilter object or a bare []LibraryExclusion
// array so both Collection Studio rule builders and Library Visibility rule
// editors serialize cleanly.
func (f *TabFilter) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*f = TabFilter{}
		return nil
	}
	if trimmed[0] == '[' {
		var groups []LibraryExclusion
		if err := json.Unmarshal(trimmed, &groups); err != nil {
			return err
		}
		*f = TabFilter{Groups: groups}
		return nil
	}
	type plain TabFilter
	var p plain
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*f = TabFilter(p)
	return nil
}

// IsEmpty reports whether no filter conditions are configured.
func (f *TabFilter) IsEmpty() bool {
	return f == nil || (len(f.Rules) == 0 && len(f.Exclude) == 0 && len(f.Groups) == 0)
}

// Clone returns a deep copy of f.
func (f *TabFilter) Clone() *TabFilter {
	if f == nil {
		return nil
	}
	out := &TabFilter{
		Match:             f.Match,
		VisibilityDefault: f.VisibilityDefault,
	}
	if len(f.Rules) > 0 {
		out.Rules = cloneCollectionRules(f.Rules)
	}
	if len(f.Exclude) > 0 {
		out.Exclude = cloneCollectionRules(f.Exclude)
	}
	if len(f.Groups) > 0 {
		out.Groups = make([]LibraryExclusion, len(f.Groups))
		for i, g := range f.Groups {
			out.Groups[i] = g
			out.Groups[i].Rules = cloneCollectionRules(g.Rules)
		}
	}
	return out
}

func cloneCollectionRules(in []CollectionRule) []CollectionRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]CollectionRule, len(in))
	for i, c := range in {
		if len(c.Value) > 0 {
			v := make(json.RawMessage, len(c.Value))
			copy(v, c.Value)
			c.Value = v
		}
		if len(c.Labels) > 0 {
			c.Labels = append([]string(nil), c.Labels...)
		}
		out[i] = c
	}
	return out
}

// ToPolicy converts f into ordered LibraryExclusion rules and a fallback
// ("show" or "hide") suitable for libraryfilter.CompilePolicy and
// pluginitems.CompilePolicy.
func (f *TabFilter) ToPolicy(mediaType MediaType) ([]LibraryExclusion, string) {
	if f.IsEmpty() {
		return nil, ""
	}
	mt := NormalizeRuleMediaType(string(mediaType))
	if mt == "" {
		mt = "mixed"
	}
	if len(f.Groups) > 0 {
		groups := make([]LibraryExclusion, len(f.Groups))
		hasShow := false
		for i, g := range f.Groups {
			if g.MediaType == "" {
				g.MediaType = mt
			}
			if g.Action == "show" {
				hasShow = true
			}
			groups[i] = g
		}
		fallback := f.VisibilityDefault
		if fallback != "show" && fallback != "hide" {
			if hasShow {
				fallback = "hide"
			} else {
				fallback = "show"
			}
		}
		return NormalizeLibraryRules(groups), fallback
	}
	var out []LibraryExclusion
	if len(f.Exclude) > 0 {
		out = append(out, LibraryExclusion{
			Action:    "hide",
			MediaType: mt,
			Match:     "any",
			Rules:     f.Exclude,
		})
	}
	if len(f.Rules) > 0 {
		match := f.Match
		if match != "any" {
			match = "all"
		}
		out = append(out, LibraryExclusion{
			Action:    "show",
			MediaType: mt,
			Match:     match,
			Rules:     f.Rules,
		})
		return NormalizeLibraryRules(out), "hide"
	}
	return NormalizeLibraryRules(out), "show"
}

// LibraryTab configures one tab inside a library in Vela.
type LibraryTab struct {
	Key               string     `json:"key"`
	Label             string     `json:"label"`
	Kind              string     `json:"kind"`                        // recommended|library|collections|people|view|live
	Facet             string     `json:"facet,omitempty"`             // collections or people facet
	Order             string     `json:"order,omitempty"`             // count|name (collections or people)
	Sort              string     `json:"sort,omitempty"`              // default sort (library, view, facet collections)
	Direction         string     `json:"direction,omitempty"`         // asc|desc
	Card              string     `json:"card,omitempty"`              // landscape (default)|poster|square (live tabs only)
	Layout            string     `json:"layout,omitempty"`            // grid (default)|list|timeline (view tabs only)
	Search            *bool      `json:"search,omitempty"`            // defaults true for library/collections/people/view/live, false for recommended
	SearchPlaceholder string     `json:"searchPlaceholder,omitempty"` // optional custom search placeholder
	Filters           *TabFilter `json:"filters,omitempty"`           // saved filter rules (view or library)
	Hidden            bool       `json:"hidden,omitempty"`            // hidden from Vela while kept in editor
}

// EffectiveLayout returns the tab's layout ("grid" by default for view tabs,
// empty for other tab kinds).
func (t LibraryTab) EffectiveLayout() string {
	if t.Kind != TabView {
		return ""
	}
	switch t.Layout {
	case TabLayoutGrid, TabLayoutList, TabLayoutTimeline:
		return t.Layout
	default:
		return TabLayoutGrid
	}
}

// EffectiveCard returns the tab's card shape ("landscape" by default for live
// tabs, empty for other tab kinds).
func (t LibraryTab) EffectiveCard() string {
	if t.Kind != TabLive {
		return ""
	}
	switch t.Card {
	case CardPoster, CardLandscape, CardSquare:
		return t.Card
	default:
		return CardLandscape
	}
}

// EffectiveSearch reports whether in-tab search is enabled on t.
func (t LibraryTab) EffectiveSearch() bool {
	if t.Kind == TabRecommended {
		return false
	}
	if t.Search != nil {
		return *t.Search
	}
	return true
}

// Clone returns a deep copy of t.
func (t LibraryTab) Clone() LibraryTab {
	out := t
	if t.Search != nil {
		b := *t.Search
		out.Search = &b
	}
	if t.Filters != nil {
		out.Filters = t.Filters.Clone()
	}
	return out
}

// DefaultCatalogueTabs returns the default tabs when a catalogue or collection
// library has no owner-configured tabs.
func DefaultCatalogueTabs() []LibraryTab {
	searchOff := false
	searchOn := true
	return []LibraryTab{
		{Key: "recommended", Label: "Recommended", Kind: TabRecommended, Search: &searchOff},
		{Key: "library", Label: "Library", Kind: TabLibrary, Search: &searchOn},
		{Key: "collections", Label: "Collections", Kind: TabCollections, Search: &searchOn},
	}
}

// NormalizeCatalogueTabFacet normalizes a catalogue library facet name to its
// canonical singular key, returning ("", false) when unrecognized. For
// collections, an empty facet means owner collections.
func NormalizeCatalogueTabFacet(kind, facet string) (string, bool) {
	f := strings.ToLower(strings.TrimSpace(facet))
	switch kind {
	case TabCollections:
		switch f {
		case "":
			return "", true
		case "genre", "genres":
			return "genre", true
		case "studio", "studios":
			return "studio", true
		case "network", "networks":
			return "network", true
		case "country", "countries":
			return "country", true
		case "language", "languages":
			return "language", true
		case "contentrating", "content_rating", "content-rating", "contentratings", "content_ratings", "content-ratings", "certification", "certifications":
			return "contentRating", true
		case "year", "years":
			return "year", true
		case "decade", "decades":
			return "decade", true
		case "franchise", "franchises":
			return "franchise", true
		case "tag", "tags":
			return "tag", true
		case "keyword", "keywords":
			return "keyword", true
		case "director", "directors":
			return "director", true
		case "writer", "writers":
			return "writer", true
		case "resolution", "resolutions":
			return "resolution", true
		case "hdr":
			return "hdr", true
		case "audiolanguage", "audio_language", "audio-language", "audiolanguages", "audio_languages", "audio-languages":
			return "audioLanguage", true
		case "rating", "ratings", "ratingband", "rating_band", "rating-band", "ratingbands", "rating_bands", "rating-bands":
			return "rating", true
		case "runtime", "runtimes", "duration", "runtimeband", "runtime_band", "runtime-band", "runtimebands", "runtime_bands", "runtime-bands":
			return "runtime", true
		case "addedmonth", "added_month", "added-month", "addedmonths", "added_months", "added-months":
			return "addedMonth", true
		default:
			return "", false
		}
	case TabPeople:
		switch f {
		case "cast":
			return "cast", true
		case "director", "directors":
			return "director", true
		case "writer", "writers":
			return "writer", true
		default:
			return "", false
		}
	default:
		return "", f == ""
	}
}

// ParseCatalogueRatingBand maps a canonical rating band ID onto (min, max, maxExclusive).
func ParseCatalogueRatingBand(id string) (minR, maxR float64, maxExclusive, ok bool) {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "9-10":
		return 9.0, 10.0, false, true
	case "8-8.9":
		return 8.0, 9.0, true, true
	case "7-7.9":
		return 7.0, 8.0, true, true
	case "6-6.9":
		return 6.0, 7.0, true, true
	case "under-6":
		return 0, 6.0, true, true
	default:
		return 0, 0, false, false
	}
}

// ParseCatalogueRuntimeBand maps a canonical runtime band ID onto inclusive seconds (minSec, maxSec).
func ParseCatalogueRuntimeBand(id string) (minSec, maxSec int64, ok bool) {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "under-30m":
		return 1, 1799, true
	case "30-60m":
		return 1800, 3599, true
	case "60-90m":
		return 3600, 5399, true
	case "90-120m":
		return 5400, 7199, true
	case "120-180m":
		return 7200, 10799, true
	case "180m-plus":
		return 10800, 0, true
	default:
		return 0, 0, false
	}
}

// ParseCatalogueAddedMonth parses a canonical "YYYY-MM" bucket ID into inclusive UTC Unix bounds.
func ParseCatalogueAddedMonth(id string) (fromTS, toTS int64, ok bool) {
	id = strings.TrimSpace(id)
	if len(id) != 7 || id[4] != '-' {
		return 0, 0, false
	}
	t, err := time.Parse("2006-01", id)
	if err != nil {
		return 0, 0, false
	}
	y, m, _ := t.UTC().Date()
	if y < 1970 || y > 9999 {
		return 0, 0, false
	}
	start := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0).Add(-time.Second)
	return start.Unix(), end.Unix(), true
}

// Library is the one library model for builtins, plugin kinds and
// owner catalogue libraries alike (spec §1.1).
type Library struct {
	ID      LibraryID     `json:"id"`
	Key     string        `json:"key"`  // slug, unique, immutable: "movies", "shows", "pk-<plugin>-<kind>", owner slug
	Name    string        `json:"name"` // owner-editable display name
	Source  LibrarySource `json:"source"`
	Sources []string      `json:"sources"`
	// Builtin only: TypeMovie or TypeShow.
	MediaType MediaType `json:"mediaType,omitempty"`
	// Plugin only.
	PluginID string `json:"pluginId,omitempty"`
	KindKey  string `json:"kindKey,omitempty"` // MediaKind.Key within that plugin
	// Collection only: the owner collection whose titles this library
	// shows (an MDBList list, a Trakt list, a smart rule, a hand-made
	// list…). MediaType pins movie|show. A deleted
	// collection leaves the library empty, never an error.
	CollectionID int64 `json:"collectionId,omitempty"`
	// Visibility rules for this library. Built-in libraries 1 and 2
	// govern canonical movie/show visibility across every protocol; catalogue
	// libraries may carry additional rules on top of their canonical library.
	RulesBroken       bool               `json:"rulesBroken,omitempty"`
	Rules             []LibraryExclusion `json:"rules,omitempty"`
	VisibilityDefault string             `json:"visibilityDefault,omitempty"`
	// Owner-defined Vela tabs. Empty when TabsCustomized is false, in which
	// case effective tabs come from the plugin declaration or DefaultCatalogueTabs.
	Tabs           []LibraryTab `json:"tabs,omitempty"`
	TabsCustomized bool         `json:"tabsCustomized,omitempty"`
	// CardShape overrides how item thumbnails in this library are shaped
	// ("poster", "landscape", or "square"). Empty means default: the plugin's
	// declared library.card for plugin libraries, or "poster" for catalogue
	// libraries.
	CardShape string `json:"cardShape,omitempty"`
	// Presentation: how protocols expose this library (§6.2). For builtins
	// this is synthesised, not stored.
	Present Presentations `json:"present,omitempty"`
	// QualityProfile is the owner-set quality profile name titles in this
	// library rank with. Empty means "Default": today's Movies/Shows
	// home/away defaults apply. Set on any library, including catalogue
	// libraries (an "Anime" library can rank with an anime profile).
	QualityProfile string    `json:"qualityProfile,omitempty"`
	Sort           int       `json:"sort"`   // owner order in every client
	Hidden         bool      `json:"hidden"` // owner hid it from all clients (still in console)
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// EffectiveCardShape resolves the library's card shape: the owner's CardShape
// choice first, then pluginDefault (for plugin libraries), falling back to
// "poster".
func (l Library) EffectiveCardShape(pluginDefault string) string {
	switch l.CardShape {
	case CardPoster, CardLandscape, CardSquare:
		return l.CardShape
	}
	if l.Source == LibraryPlugin {
		switch pluginDefault {
		case CardPoster, CardLandscape, CardSquare:
			return pluginDefault
		}
	}
	return CardPoster
}

// Presentations declares how a library is published per protocol (§6.2).
// Declared by the plugin for plugin libraries, editable by the owner per
// library afterwards; a zero value publishes nowhere (console-only).
type Presentations struct {
	AdapterOnly bool `json:"adapterOnly,omitempty"` // only via FrontendAdapter (§6.5)
	Vela        *struct {
		Type string `json:"type"` // movies|shows
	} `json:"vela,omitempty"`
	Plex *struct {
		Base string `json:"base"` // movie|show|artist|photo  (Plex section type / agent family)
		// level mapping for Base=show: root->show, mid->season, leaf->episode; movie: root|leaf->movie
	} `json:"plex,omitempty"`
	MediaBrowser *struct { // Jellyfin + Emby share internal/frontends/mediabrowser
		CollectionType string            `json:"collectionType"`      // movies|tvshows|music|audiobooks|books|photos|homevideos
		ItemTypes      map[string]string `json:"itemTypes,omitempty"` // level -> Movie|Series|Season|Episode|MusicArtist|MusicAlbum|Audio|AudioBook|Book|PhotoAlbum|Photo
	} `json:"mediaBrowser,omitempty"`
	Stremio *struct {
		Type string `json:"type"` // movie|series|channel|tv — closed Stremio set; other kinds use their own add-on surface instead
	} `json:"stremio,omitempty"`
	Xtream *struct {
		Category string `json:"category"`
	} `json:"xtream,omitempty"` // movie|series|live
	Silo *struct {
		Type string `json:"type"`
	} `json:"silo,omitempty"` // mirrors MediaBrowser item family
}

// CanonicalLibraryForTitle returns the one canonical library of a TMDB
// title (§1.2): TypeMovie → 1; TypeShow/Season/Episode → 2. Any other
// type has no canonical library and returns 0.
func CanonicalLibraryForTitle(t MediaType) LibraryID {
	switch t {
	case TypeMovie:
		return LibraryMovies
	case TypeShow, TypeSeason, TypeEpisode:
		return LibraryShows
	default:
		return 0
	}
}

// AllowsLibrary reports whether u may open library id (§1.4): the owner
// and a viewer with no restriction see every library; otherwise the
// library must be listed. A nil user sees nothing.
func (u *User) AllowsLibrary(id LibraryID) bool {
	if u == nil {
		return false
	}
	if u.IsOwner || len(u.AllowedLibraries) == 0 {
		return true
	}
	for _, allowed := range u.AllowedLibraries {
		if allowed == id {
			return true
		}
	}
	return false
}

// CatalogFacetCollectionsQuery requests a paged facet listing over catalogue
// titles matching Filter.
type CatalogFacetCollectionsQuery struct {
	Filter Filter
	Facet  string // genre|studio|network|country|language|contentRating|year|decade|franchise|tag|keyword|director|writer|resolution|hdr|audioLanguage|rating|runtime|addedMonth
	Order  string // count (default) | name
	Query  string // optional folded search string
	Offset int
	Limit  int
}

// CatalogFacetCollection is one facet bucket in a catalogue library's
// collections tab, along with its newest visible title for artwork.
type CatalogFacetCollection struct {
	ID         string
	Name       string
	ItemCount  int
	NewestItem *Title
}

// CatalogFacetCollectionsPage is one page of catalogue facet collections.
type CatalogFacetCollectionsPage struct {
	Items []CatalogFacetCollection
	More  bool
}

// CatalogFacetPeopleQuery requests a paged cast, director, or writer listing
// over catalogue titles matching Filter.
type CatalogFacetPeopleQuery struct {
	Filter Filter
	Facet  string // cast | director | writer
	Order  string // name (default) | count
	Query  string // optional folded search string
	Offset int
	Limit  int
}

// CatalogFacetPerson is one person entry in a catalogue library's people tab.
type CatalogFacetPerson struct {
	PersonTMDB int64
	Name       string
	ItemCount  int
	SampleKey  RatingKey
}

// CatalogFacetPeoplePage is one page of catalogue facet people.
type CatalogFacetPeoplePage struct {
	Items []CatalogFacetPerson
	More  bool
}
