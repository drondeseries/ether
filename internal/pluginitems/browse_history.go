package pluginitems

import (
	"context"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// browseLastViewed returns a keyset-paged slice of plugin items ordered by the
// requesting viewer's watch recency (state_watch.last_viewed_at > 0), followed
// by matching undated/unviewed items in sort_title order. Phase 0 drives from
// state_watch_last_viewed into plugin_items(seq), keeping queries bounded to
// the viewer's watch rows rather than walking the library.
func (s *Service) browseLastViewed(
	ctx context.Context,
	q BrowseQuery,
	viewerID int64,
	baseWhere string,
	baseArgs []any,
	hasFacets bool,
	parentRef string,
	level string,
	desc bool,
	limit int,
) (BrowsePage, error) {
	var total int
	if q.NoTotal {
		// Count-free request: page and cursor continuation suffice.
	} else if !hasFacets && parentRef == "" && level != "" {
		_ = s.db.QueryRowContext(ctx,
			`SELECT COALESCE(n, 0) FROM plugin_item_tally WHERE library_id = ? AND level = ?`,
			int64(q.LibraryID), level).Scan(&total)
	} else if !hasFacets && parentRef == "" && level == "" {
		if k, err := s.GetKindByLibrary(ctx, q.LibraryID); err == nil && len(k.Decl.Levels) > 0 {
			_ = s.db.QueryRowContext(ctx,
				`SELECT COALESCE(n, 0) FROM plugin_item_tally WHERE library_id = ? AND level = ?`,
				int64(q.LibraryID), k.Decl.Levels[0].Key).Scan(&total)
		} else if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM plugin_items i WHERE `+baseWhere,
			baseArgs...).Scan(&total); err != nil {
			return BrowsePage{}, err
		}
	} else {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM plugin_items i WHERE `+baseWhere,
			baseArgs...).Scan(&total); err != nil {
			return BrowsePage{}, err
		}
	}

	var cur browseCursor
	hasCursor := strings.TrimSpace(q.Cursor) != ""
	if hasCursor {
		var err error
		cur, err = decodeBrowseCursor(q.Cursor)
		if err != nil {
			return BrowsePage{}, err
		}
	}

	keyBase := core.PluginKeyOffset
	keyEnd := core.PluginKeyOffset + core.PluginKeyMaxSeq

	var items []core.KindItem
	var viewedAts []int64
	phase0Count := 0

	// Phase 0: dated watch history from state_watch, bounded to this viewer.
	if viewerID > 0 && (!hasCursor || cur.Phase == 0) {
		var p0Where strings.Builder
		p0Args := make([]any, 0, len(baseArgs)+5)
		fmt.Fprintf(&p0Where,
			`w.user_id = ? AND w.last_viewed_at > 0 AND w.rating_key > %d AND w.rating_key < %d AND i.seq = w.rating_key - %d AND (%s)`,
			keyBase, keyEnd, keyBase, baseWhere,
		)
		p0Args = append(p0Args, viewerID)
		p0Args = append(p0Args, baseArgs...)

		if hasCursor && !cur.StartPhase {
			if desc {
				p0Where.WriteString(` AND (w.last_viewed_at <= ? AND (w.last_viewed_at < ? OR i.item_ref > ?))`)
			} else {
				p0Where.WriteString(` AND (w.last_viewed_at >= ? AND (w.last_viewed_at > ? OR i.item_ref > ?))`)
			}
			p0Args = append(p0Args, cur.IntVal, cur.IntVal, cur.ItemRef)
		}

		dir := "ASC"
		if desc {
			dir = "DESC"
		}
		p0Args = append(p0Args, limit+1)

		querySQL := `SELECT ` + prefixedItemCols("i.") + `, w.last_viewed_at
FROM state_watch w CROSS JOIN plugin_items i
WHERE ` + p0Where.String() + `
ORDER BY w.last_viewed_at ` + dir + `, i.item_ref ASC
LIMIT ?`

		rows, err := s.db.QueryContext(ctx, querySQL, p0Args...)
		if err != nil {
			return BrowsePage{}, err
		}
		for rows.Next() {
			var viewedAt int64
			it, _, _, scanErr := scanItemRowExtra(rows, &viewedAt)
			if scanErr != nil {
				rows.Close()
				return BrowsePage{}, scanErr
			}
			items = append(items, it)
			viewedAts = append(viewedAts, viewedAt)
		}
		if err := rows.Close(); err != nil {
			return BrowsePage{}, err
		}
		if err := rows.Err(); err != nil {
			return BrowsePage{}, err
		}

		if len(items) > limit {
			last := items[limit-1]
			lastViewed := viewedAts[limit-1]
			items = items[:limit]
			nextCursor := encodeBrowseCursor(browseCursor{
				Sort:    "last_viewed_at",
				Desc:    desc,
				Phase:   0,
				IntVal:  lastViewed,
				ItemRef: string(last.Ref),
			})
			if len(items) > 0 {
				if err := s.hydrateArtSlice(ctx, items); err != nil {
					return BrowsePage{}, err
				}
			}
			if q.NoTotal {
				total = len(items)
			}
			return BrowsePage{Items: items, Total: total, NextCursor: nextCursor}, nil
		}
		phase0Count = len(items)
	}

	// Phase 1: matching items without a dated watch row, in sort_title order.
	// When Phase 0 filled exactly limit items, need == 1 probes whether Phase 1
	// has any remaining rows without over-fetching.
	need := limit + 1 - len(items)
	if need > 0 {
		p1Where := baseWhere
		p1Args := append([]any(nil), baseArgs...)
		if viewerID > 0 {
			p1Where += fmt.Sprintf(` AND NOT EXISTS (SELECT 1 FROM state_watch w WHERE w.user_id = ? AND w.rating_key = %d + i.seq AND w.last_viewed_at > 0)`, keyBase)
			p1Args = append(p1Args, viewerID)
		}
		if hasCursor && cur.Phase == 1 && !cur.StartPhase {
			p1Where += ` AND (i.sort_title >= ? AND (i.sort_title > ? OR i.item_ref > ?))`
			p1Args = append(p1Args, cur.StrVal, cur.StrVal, cur.ItemRef)
		}
		p1Args = append(p1Args, need)

		querySQL := `SELECT ` + prefixedItemCols("i.") + `
FROM plugin_items i
WHERE ` + p1Where + `
ORDER BY i.sort_title ASC, i.item_ref ASC
LIMIT ?`

		tail, err := s.queryItems(ctx, querySQL, p1Args, false)
		if err != nil {
			return BrowsePage{}, err
		}
		items = append(items, tail...)
	}

	var nextCursor string
	if len(items) > limit {
		items = items[:limit]
		if phase0Count == limit {
			nextCursor = encodeBrowseCursor(browseCursor{
				Sort:       "last_viewed_at",
				Desc:       desc,
				Phase:      1,
				StartPhase: true,
			})
		} else {
			last := items[limit-1]
			nextCursor = encodeBrowseCursor(browseCursor{
				Sort:    "last_viewed_at",
				Desc:    desc,
				Phase:   1,
				StrVal:  last.SortTitle,
				ItemRef: string(last.Ref),
			})
		}
	}

	if len(items) > 0 {
		if err := s.hydrateArtSlice(ctx, items); err != nil {
			return BrowsePage{}, err
		}
	}
	if q.NoTotal {
		total = len(items)
	}
	if items == nil {
		items = []core.KindItem{}
	}
	return BrowsePage{
		Items:      items,
		Total:      total,
		NextCursor: nextCursor,
	}, nil
}
