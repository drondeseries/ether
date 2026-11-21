package pluginitems

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

func (s *Service) searchPostgres(ctx context.Context, q SearchQuery, tokens []string, limit int) (BrowsePage, error) {
	parts := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		parts = append(parts, tok+":*")
	}
	tsQuery := strings.Join(parts, " & ")

	var where strings.Builder
	args := []any{int64(q.LibraryID), tsQuery}
	where.WriteString(`i.library_id = ? AND i.search_tsv @@ to_tsquery('simple', ?)`)
	if len(q.Levels) > 0 {
		placeholders := make([]string, len(q.Levels))
		for idx, lv := range q.Levels {
			placeholders[idx] = "?"
			args = append(args, strings.TrimSpace(lv))
		}
		where.WriteString(` AND i.level IN (` + strings.Join(placeholders, ",") + `)`)
	} else if q.Level != "" {
		where.WriteString(` AND i.level = ?`)
		args = append(args, q.Level)
	}
	if q.PersonID > 0 {
		ok, err := s.appendPersonFilter(ctx, q.LibraryID, &where, &args, q.PersonID)
		if err != nil {
			return BrowsePage{}, err
		}
		if !ok {
			return BrowsePage{Total: 0}, nil
		}
	}
	if err := s.appendFacets(ctx, q.LibraryID, &where, &args, q.Facets, q.AllFacets, q.ExcludeFacets); err != nil {
		return BrowsePage{}, err
	}
	viewerID := q.ViewerID
	if viewerID <= 0 {
		viewerID = core.CatalogViewerID(ctx)
	}
	if err := s.appendVisibilityForViewer(ctx, &where, &args, q.LibraryID, q.Policy, q.Hidden, viewerID); err != nil {
		return BrowsePage{}, err
	}
	if err := s.appendVisibilityForViewer(ctx, &where, &args, q.LibraryID, q.TabPolicy, false, viewerID); err != nil {
		return BrowsePage{}, err
	}

	var total int
	countSQL := `SELECT COUNT(*) FROM plugin_items i WHERE ` + where.String()
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return BrowsePage{}, err
	}
	if total == 0 {
		return BrowsePage{Total: 0}, nil
	}

	exactTitle, exactSort, prefixTitle, prefixSort := searchTitleTierArgs(q.Query)
	querySQL := `SELECT ` + prefixedItemCols("i.") + `
FROM plugin_items i
WHERE ` + where.String() + `
ORDER BY
	CASE
		WHEN LOWER(i.title) = ? OR i.sort_title = ? THEN 0
		WHEN LOWER(i.title) LIKE ? ESCAPE '\' OR i.sort_title LIKE ? ESCAPE '\' THEN 1
		ELSE 2
	END ASC,
	ts_rank(i.search_tsv, to_tsquery('simple', ?)) DESC,
	i.sort_title ASC,
	i.item_ref ASC
LIMIT ?`
	qArgs := append(append([]any(nil), args...), exactTitle, exactSort, prefixTitle, prefixSort, tsQuery, limit)
	items, err := s.queryItems(ctx, querySQL, qArgs, true)
	if err != nil {
		return BrowsePage{}, err
	}
	return BrowsePage{Items: items, Total: total}, nil
}
