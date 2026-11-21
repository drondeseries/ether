package pluginitems

import (
	"context"
	"strings"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/store"
)

// SearchQuery scopes a full-text search to a single library (spec §2.3, §3.4).
type SearchQuery struct {
	Policy    *libraryfilter.Filter
	TabPolicy *libraryfilter.Filter
	ViewerID  int64
	Hidden    bool
	LibraryID core.LibraryID
	Query     string
	Level     string
	Levels    []string
	PersonID  int64
	Facets    map[string][]string
	// AllFacets requires every listed value per field; ExcludeFacets
	// drops items having any listed value (see BrowseQuery).
	AllFacets     map[string][]string
	ExcludeFacets map[string][]string
	Limit         int
}

// Search executes a library-scoped full-text search over plugin items
// (SQLite FTS5 or PostgreSQL tsvector, spec §2.3, §3.4, §10).
func (s *Service) Search(ctx context.Context, q SearchQuery) (BrowsePage, error) {
	if q.LibraryID <= 0 {
		return BrowsePage{}, nil
	}
	tokens := tokenizeSearchQuery(q.Query)
	if len(tokens) == 0 {
		return BrowsePage{}, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultBrowseLimit
	}
	if limit > MaxBrowseLimit {
		limit = MaxBrowseLimit
	}
	if store.DialectOf(s.db) == store.Postgres {
		return s.searchPostgres(ctx, q, tokens, limit)
	}
	return s.searchSQLite(ctx, q, tokens, limit)
}

func tokenizeSearchQuery(raw string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(raw) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	if len(tokens) > 16 {
		tokens = tokens[:16]
	}
	return tokens
}

func escapeLike(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func searchTitleTierArgs(rawQuery string) (exactTitle, exactSort, prefixTitle, prefixSort string) {
	exactTitle = strings.ToLower(strings.TrimSpace(rawQuery))
	exactSort = SortTitle(rawQuery)
	if exactSort == "" {
		exactSort = exactTitle
	}
	prefixTitle = escapeLike(exactTitle) + "%"
	prefixSort = escapeLike(exactSort) + "%"
	return exactTitle, exactSort, prefixTitle, prefixSort
}
