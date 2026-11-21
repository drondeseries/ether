package pluginitems

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
)

// FacetPeopleQuery requests a single page of distinct people from one declared
// type:"people" field at a library's root level, ordered by name (default) or
// item count, with sample items resolved in a single batched query for the page.
type FacetPeopleQuery struct {
	LibraryID core.LibraryID
	Level     string
	Policy    *libraryfilter.Filter
	FieldKey  string
	Order     string // "name" (default) | "count"
	Query     string
	Offset    int
	Limit     int
}

// FacetPerson is one distinct person entry for a people tab, with item count
// and a sample credited item for headshot authorization.
type FacetPerson struct {
	PersonID   int64
	Name       string
	Count      int
	Thumb      string
	SampleItem *core.KindItem
}

// FacetPeoplePage is one page of people for a library people tab.
type FacetPeoplePage struct {
	Items []FacetPerson
	More  bool
}

// buildRootItemPredicates builds two equivalent root-level + visibility WHERE
// predicates on plugin_items i:
//   - indexedWhere uses `i.library_id = ?` for library-driven scans/aggregations;
//   - fieldDrivenWhere uses `i.library_id + 0 = ?` so SQLite drives joins from
//     `plugin_item_fields_lookup` / `plugin_item_fields_lookup_int` /
//     `plugin_item_fields_fold` instead of scanning all rows in `plugin_items`
//     when matching bounded field values.
func (s *Service) buildRootItemPredicates(ctx context.Context, rec KindRecord, level string, policy *libraryfilter.Filter) (indexedWhere, fieldDrivenWhere string, args []any, err error) {
	var idxB, fldB strings.Builder
	args = []any{int64(rec.LibraryID)}
	idxB.WriteString(`i.library_id = ?`)
	fldB.WriteString(`i.library_id + 0 = ?`)

	if lv := strings.TrimSpace(level); lv != "" {
		idxB.WriteString(` AND i.level = ?`)
		fldB.WriteString(` AND i.level = ?`)
		args = append(args, lv)
	} else if len(rec.Decl.Levels) > 0 {
		idxB.WriteString(` AND i.level = ?`)
		fldB.WriteString(` AND i.level = ?`)
		args = append(args, rec.Decl.Levels[0].Key)
	} else {
		idxB.WriteString(` AND i.parent_ref = ''`)
		fldB.WriteString(` AND i.parent_ref = ''`)
	}

	var visB strings.Builder
	var visArgs []any
	if err := s.appendVisibility(ctx, &visB, &visArgs, rec.LibraryID, policy, false); err != nil {
		return "", "", nil, err
	}
	if visB.Len() > 0 {
		idxB.WriteString(visB.String())
		fldB.WriteString(visB.String())
		args = append(args, visArgs...)
	}
	return idxB.String(), fldB.String(), args, nil
}

// FacetPeople lists distinct people in a declared people-type field at the
// library's root level, grouped by synthetic plugin person ID (v_int), with
// label from v_text and item count, ordered by name (case-insensitive, default)
// or count, and resolves sample items for headshots in a single batched query.
func (s *Service) FacetPeople(ctx context.Context, q FacetPeopleQuery) (FacetPeoplePage, error) {
	if s == nil || s.db == nil {
		return FacetPeoplePage{}, fmt.Errorf("pluginitems: nil service")
	}
	fieldKey := strings.TrimSpace(q.FieldKey)
	if q.LibraryID <= 0 || fieldKey == "" {
		return FacetPeoplePage{}, fmt.Errorf("pluginitems: library_id and field_key are required")
	}
	trimmedQuery := strings.TrimSpace(q.Query)
	terms := foldQueryTerms(trimmedQuery)
	if trimmedQuery != "" && len(terms) == 0 {
		return FacetPeoplePage{Items: []FacetPerson{}}, nil
	}
	rec, err := s.GetKindByLibrary(ctx, q.LibraryID)
	if err != nil {
		return FacetPeoplePage{}, err
	}
	isPeople := false
	for _, fd := range rec.Decl.Fields {
		if fd.Key == fieldKey && fd.Type == "people" {
			isPeople = true
			break
		}
	}
	if !isPeople {
		return FacetPeoplePage{Items: []FacetPerson{}}, nil
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := max(q.Offset, 0)

	itemWhere, fieldDrivenWhere, itemArgs, err := s.buildRootItemPredicates(ctx, rec, q.Level, q.Policy)
	if err != nil {
		return FacetPeoplePage{}, err
	}

	orderBy := `LOWER(MIN(f.v_text)) ASC, MIN(f.v_text) ASC, f.v_int ASC`
	if strings.EqualFold(strings.TrimSpace(q.Order), "count") {
		orderBy = `cnt DESC, LOWER(MIN(f.v_text)) ASC, MIN(f.v_text) ASC, f.v_int ASC`
	}

	minPersonID := core.PluginPersonIDBase
	maxPersonID := core.PluginPersonIDBase + core.PluginPersonIDSpan
	var where strings.Builder
	qArgs := append([]any(nil), itemArgs...)
	if len(terms) > 0 {
		where.WriteString(fieldDrivenWhere)
		where.WriteString(` AND f.field = ? AND f.v_int + 0 > ? AND f.v_int + 0 < ? AND f.v_text <> ''`)
		qArgs = append(qArgs, fieldKey, minPersonID, maxPersonID)
		appendFoldedWordPrefix(&where, &qArgs, terms)
	} else {
		where.WriteString(itemWhere)
		where.WriteString(` AND f.field = ? AND f.v_int <> 0 AND f.v_int > ? AND f.v_int < ? AND f.v_text <> ''`)
		qArgs = append(qArgs, fieldKey, minPersonID, maxPersonID)
	}
	qArgs = append(qArgs, limit+1, offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.v_int, MIN(f.v_text) AS label, COUNT(*) AS cnt
		 FROM plugin_item_fields f
		 JOIN plugin_items i ON i.item_ref = f.item_ref
		 WHERE `+where.String()+`
		 GROUP BY f.v_int
		 ORDER BY `+orderBy+`
		 LIMIT ? OFFSET ?`, qArgs...)
	if err != nil {
		return FacetPeoplePage{}, err
	}
	var out []FacetPerson
	for rows.Next() {
		var pid int64
		var label string
		var cnt int
		if err := rows.Scan(&pid, &label, &cnt); err != nil {
			rows.Close()
			return FacetPeoplePage{}, err
		}
		label = strings.TrimSpace(label)
		if core.IsPluginPersonID(pid) && label != "" && cnt > 0 {
			out = append(out, FacetPerson{
				PersonID: pid,
				Name:     label,
				Count:    cnt,
			})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return FacetPeoplePage{}, err
	}
	rows.Close()

	more := false
	if len(out) > limit {
		out = out[:limit]
		more = true
	}
	if len(out) == 0 {
		return FacetPeoplePage{Items: []FacetPerson{}}, nil
	}

	// Fetch up to 4 candidate items per person on the page in ONE query driven
	// from plugin_item_fields_lookup_int so we can pick a headshot-bearing
	// credit (or fall back to the newest visible credit) with zero per-person loops.
	idxByPID := make(map[int64]int, len(out))
	placeholders := make([]string, len(out))
	sampleArgs := make([]any, 0, 1+len(out)+len(itemArgs))
	sampleArgs = append(sampleArgs, fieldKey)
	for i, p := range out {
		idxByPID[p.PersonID] = i
		placeholders[i] = "?"
		sampleArgs = append(sampleArgs, p.PersonID)
	}
	sampleArgs = append(sampleArgs, itemArgs...)

	sampleSQL := `WITH ranked AS (
	SELECT f.v_int AS bucket_pid,
		i.item_ref,
		ROW_NUMBER() OVER (PARTITION BY f.v_int ORDER BY i.released DESC, i.added_at DESC, i.year DESC, i.item_ref ASC) AS rn
	FROM plugin_item_fields f
	JOIN plugin_items i ON i.item_ref = f.item_ref
	WHERE f.field = ? AND f.v_int <> 0 AND f.v_int IN (` + strings.Join(placeholders, ",") + `) AND ` + fieldDrivenWhere + `
)
SELECT r.bucket_pid, ` + prefixedItemCols("i.") + `
FROM ranked r
JOIN plugin_items i ON i.item_ref = r.item_ref
WHERE r.rn <= 4
ORDER BY r.bucket_pid ASC, r.rn ASC`

	sRows, err := s.db.QueryContext(ctx, sampleSQL, sampleArgs...)
	if err != nil {
		return FacetPeoplePage{}, err
	}
	for sRows.Next() {
		var pid int64
		it, _, _, scanErr := scanItemRow(prefixScanner{sc: sRows, prefix: []any{&pid}})
		if scanErr != nil {
			sRows.Close()
			return FacetPeoplePage{}, scanErr
		}
		idx, ok := idxByPID[pid]
		if !ok {
			continue
		}
		if out[idx].SampleItem == nil {
			cp := it
			out[idx].SampleItem = &cp
		}
		if out[idx].Thumb != "" {
			continue
		}
		cast, dirs, wris := extractProjectedPeople(it.Fields, rec.Decl)
		for _, group := range [][]core.Person{cast, dirs, wris} {
			for _, cr := range group {
				if cr.TMDB == pid {
					if cr.Name != "" && out[idx].SampleItem != nil && out[idx].SampleItem.Ref == it.Ref {
						out[idx].Name = cr.Name
					}
					if cr.ThumbPath != "" {
						out[idx].Thumb = cr.ThumbPath
						cp := it
						out[idx].SampleItem = &cp
						if cr.Name != "" {
							out[idx].Name = cr.Name
						}
						break
					}
				}
			}
			if out[idx].Thumb != "" {
				break
			}
		}
	}
	if err := sRows.Err(); err != nil {
		sRows.Close()
		return FacetPeoplePage{}, err
	}
	sRows.Close()
	return FacetPeoplePage{Items: out, More: more}, nil
}

// resolveNewestByTextBuckets resolves the newest visible item (plus artwork)
// for all text-indexed buckets on a FacetCollections page in a single SQL query
// driven from plugin_item_fields_lookup + 1 batched art hydration query.
func (s *Service) resolveNewestByTextBuckets(ctx context.Context, fieldKey string, buckets []FacetBucket, fieldDrivenWhere string, itemArgs []any) ([]*core.KindItem, error) {
	if len(buckets) == 0 {
		return nil, nil
	}
	idxByID := make(map[string][]int, len(buckets))
	placeholders := make([]string, 0, len(buckets))
	args := make([]any, 0, 1+len(buckets)+len(itemArgs))
	args = append(args, fieldKey)
	for i, b := range buckets {
		if _, seen := idxByID[b.ID]; !seen {
			placeholders = append(placeholders, "?")
			args = append(args, b.ID)
		}
		idxByID[b.ID] = append(idxByID[b.ID], i)
	}
	args = append(args, itemArgs...)

	querySQL := `WITH ranked AS (
	SELECT f.v_text AS bucket_id,
		i.item_ref,
		ROW_NUMBER() OVER (PARTITION BY f.v_text ORDER BY i.released DESC, i.added_at DESC, i.year DESC, i.item_ref ASC) AS rn
	FROM plugin_item_fields f
	JOIN plugin_items i ON i.item_ref = f.item_ref
	WHERE f.field = ? AND f.v_text IN (` + strings.Join(placeholders, ",") + `) AND ` + fieldDrivenWhere + `
)
SELECT r.bucket_id, ` + prefixedItemCols("i.") + `
FROM ranked r
JOIN plugin_items i ON i.item_ref = r.item_ref
WHERE r.rn = 1`

	rows, err := s.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, err
	}
	var items []core.KindItem
	var bucketIDs []string
	for rows.Next() {
		var bID string
		it, _, _, scanErr := scanItemRow(prefixScanner{sc: rows, prefix: []any{&bID}})
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		items = append(items, it)
		bucketIDs = append(bucketIDs, bID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]*core.KindItem, len(buckets))
	if len(items) == 0 {
		return out, nil
	}
	if err := s.hydrateArtSlice(ctx, items); err != nil {
		return nil, err
	}
	for i, bID := range bucketIDs {
		for _, targetIdx := range idxByID[bID] {
			cp := items[i]
			out[targetIdx] = &cp
		}
	}
	return out, nil
}

// resolveNewestByIntBuckets resolves the newest visible item (plus artwork)
// for exact integer buckets on a FacetCollections page in a single SQL query
// driven from plugin_item_fields_lookup_int + 1 batched art hydration query.
func (s *Service) resolveNewestByIntBuckets(ctx context.Context, fieldKey string, buckets []FacetBucket, fieldDrivenWhere string, itemArgs []any) ([]*core.KindItem, error) {
	if len(buckets) == 0 {
		return nil, nil
	}
	idxByVal := make(map[int64][]int, len(buckets))
	nonZeroPh := make([]string, 0, len(buckets))
	nonZeroVals := make([]any, 0, len(buckets))
	hasZero := false
	for i, b := range buckets {
		v, err := strconv.ParseInt(b.ID, 10, 64)
		if err != nil {
			continue
		}
		if _, seen := idxByVal[v]; !seen {
			if v == 0 {
				hasZero = true
			} else {
				nonZeroPh = append(nonZeroPh, "?")
				nonZeroVals = append(nonZeroVals, v)
			}
		}
		idxByVal[v] = append(idxByVal[v], i)
	}
	if !hasZero && len(nonZeroPh) == 0 {
		return make([]*core.KindItem, len(buckets)), nil
	}

	var querySQL string
	var args []any
	switch {
	case !hasZero:
		args = make([]any, 0, 1+len(nonZeroVals)+len(itemArgs))
		args = append(args, fieldKey)
		args = append(args, nonZeroVals...)
		args = append(args, itemArgs...)
		querySQL = `WITH ranked AS (
	SELECT f.v_int AS bucket_val,
		i.item_ref,
		ROW_NUMBER() OVER (PARTITION BY f.v_int ORDER BY i.released DESC, i.added_at DESC, i.year DESC, i.item_ref ASC) AS rn
	FROM plugin_item_fields f
	JOIN plugin_items i ON i.item_ref = f.item_ref
	WHERE f.field = ? AND f.v_int <> 0 AND f.v_int IN (` + strings.Join(nonZeroPh, ",") + `) AND ` + fieldDrivenWhere + `
)
SELECT r.bucket_val, ` + prefixedItemCols("i.") + `
FROM ranked r
JOIN plugin_items i ON i.item_ref = r.item_ref
WHERE r.rn = 1`
	case len(nonZeroPh) == 0:
		args = make([]any, 0, 2+len(itemArgs))
		args = append(args, fieldKey, int64(0))
		args = append(args, itemArgs...)
		querySQL = `WITH ranked AS (
	SELECT f.v_int AS bucket_val,
		i.item_ref,
		ROW_NUMBER() OVER (PARTITION BY f.v_int ORDER BY i.released DESC, i.added_at DESC, i.year DESC, i.item_ref ASC) AS rn
	FROM plugin_item_fields f
	JOIN plugin_items i ON i.item_ref = f.item_ref
	WHERE f.field = ? AND (f.v_text = '' OR f.v_int <> 0) AND f.v_int = ? AND ` + fieldDrivenWhere + `
)
SELECT r.bucket_val, ` + prefixedItemCols("i.") + `
FROM ranked r
JOIN plugin_items i ON i.item_ref = r.item_ref
WHERE r.rn = 1`
	default:
		// Split non-zero values (f.v_int <> 0 -> covering lookup_int) from 0
		// ((f.v_text = '' OR f.v_int <> 0) AND f.v_int = ? -> lookup_int seek)
		// so SQLite does not abandon lookup_int for plugin_item_fields_fold
		// when a page includes 0 alongside multiple non-zero buckets.
		args = make([]any, 0, 3+len(nonZeroVals)+2*len(itemArgs))
		args = append(args, fieldKey)
		args = append(args, nonZeroVals...)
		args = append(args, itemArgs...)
		args = append(args, fieldKey, int64(0))
		args = append(args, itemArgs...)
		querySQL = `WITH matched AS (
	SELECT f.v_int AS bucket_val, i.item_ref, i.released, i.added_at, i.year
	FROM plugin_item_fields f
	JOIN plugin_items i ON i.item_ref = f.item_ref
	WHERE f.field = ? AND f.v_int <> 0 AND f.v_int IN (` + strings.Join(nonZeroPh, ",") + `) AND ` + fieldDrivenWhere + `
	UNION ALL
	SELECT f.v_int AS bucket_val, i.item_ref, i.released, i.added_at, i.year
	FROM plugin_item_fields f
	JOIN plugin_items i ON i.item_ref = f.item_ref
	WHERE f.field = ? AND (f.v_text = '' OR f.v_int <> 0) AND f.v_int = ? AND ` + fieldDrivenWhere + `
),
ranked AS (
	SELECT bucket_val,
		item_ref,
		ROW_NUMBER() OVER (PARTITION BY bucket_val ORDER BY released DESC, added_at DESC, year DESC, item_ref ASC) AS rn
	FROM matched
)
SELECT r.bucket_val, ` + prefixedItemCols("i.") + `
FROM ranked r
JOIN plugin_items i ON i.item_ref = r.item_ref
WHERE r.rn = 1`
	}

	rows, err := s.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, err
	}
	var items []core.KindItem
	var vals []int64
	for rows.Next() {
		var v int64
		it, _, _, scanErr := scanItemRow(prefixScanner{sc: rows, prefix: []any{&v}})
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		items = append(items, it)
		vals = append(vals, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]*core.KindItem, len(buckets))
	if len(items) == 0 {
		return out, nil
	}
	if err := s.hydrateArtSlice(ctx, items); err != nil {
		return nil, err
	}
	for i, v := range vals {
		for _, targetIdx := range idxByVal[v] {
			cp := items[i]
			out[targetIdx] = &cp
		}
	}
	return out, nil
}

// resolveNewestByFloatBuckets resolves the newest visible item (plus artwork)
// for exact float buckets on a FacetCollections page in a single SQL query
// driven from plugin_item_fields_lookup_real + 1 batched art hydration query.
func (s *Service) resolveNewestByFloatBuckets(ctx context.Context, fieldKey string, buckets []FacetBucket, fieldDrivenWhere string, itemArgs []any) ([]*core.KindItem, error) {
	if len(buckets) == 0 {
		return nil, nil
	}
	idxByKey := make(map[string][]int, len(buckets))
	placeholders := make([]string, 0, len(buckets))
	args := make([]any, 0, 1+len(buckets)+len(itemArgs))
	args = append(args, fieldKey)
	for i, b := range buckets {
		v, err := strconv.ParseFloat(b.ID, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		key := strconv.FormatFloat(v, 'f', -1, 64)
		if _, seen := idxByKey[key]; !seen {
			placeholders = append(placeholders, "?")
			args = append(args, v)
		}
		idxByKey[key] = append(idxByKey[key], i)
	}
	if len(placeholders) == 0 {
		return make([]*core.KindItem, len(buckets)), nil
	}
	args = append(args, itemArgs...)

	querySQL := `WITH ranked AS (
	SELECT f.v_real AS bucket_val,
		i.item_ref,
		ROW_NUMBER() OVER (PARTITION BY f.v_real ORDER BY i.released DESC, i.added_at DESC, i.year DESC, i.item_ref ASC) AS rn
	FROM plugin_item_fields f
	JOIN plugin_items i ON i.item_ref = f.item_ref
	WHERE f.field = ? AND f.v_text = '' AND f.v_real IN (` + strings.Join(placeholders, ",") + `) AND ` + fieldDrivenWhere + `
)
SELECT r.bucket_val, ` + prefixedItemCols("i.") + `
FROM ranked r
JOIN plugin_items i ON i.item_ref = r.item_ref
WHERE r.rn = 1`

	rows, err := s.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, err
	}
	var items []core.KindItem
	var keys []string
	for rows.Next() {
		var v float64
		it, _, _, scanErr := scanItemRow(prefixScanner{sc: rows, prefix: []any{&v}})
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		items = append(items, it)
		keys = append(keys, strconv.FormatFloat(v, 'f', -1, 64))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]*core.KindItem, len(buckets))
	if len(items) == 0 {
		return out, nil
	}
	if err := s.hydrateArtSlice(ctx, items); err != nil {
		return nil, err
	}
	for i, key := range keys {
		for _, targetIdx := range idxByKey[key] {
			cp := items[i]
			out[targetIdx] = &cp
		}
	}
	return out, nil
}

// resolveNewestByPersonIDs resolves the newest visible item (plus artwork) for
// all person-ID buckets on a FacetCollections page in a single SQL query driven
// from plugin_item_fields_lookup_int + 1 batched art hydration query.
func (s *Service) resolveNewestByPersonIDs(ctx context.Context, fieldKey string, personIDs []int64, fieldDrivenWhere string, itemArgs []any) ([]*core.KindItem, error) {
	if len(personIDs) == 0 {
		return nil, nil
	}
	idxByPID := make(map[int64][]int, len(personIDs))
	placeholders := make([]string, 0, len(personIDs))
	args := make([]any, 0, 1+len(personIDs)+len(itemArgs))
	args = append(args, fieldKey)
	for i, pid := range personIDs {
		if _, seen := idxByPID[pid]; !seen {
			placeholders = append(placeholders, "?")
			args = append(args, pid)
		}
		idxByPID[pid] = append(idxByPID[pid], i)
	}
	args = append(args, itemArgs...)

	querySQL := `WITH ranked AS (
	SELECT f.v_int AS bucket_pid,
		i.item_ref,
		ROW_NUMBER() OVER (PARTITION BY f.v_int ORDER BY i.released DESC, i.added_at DESC, i.year DESC, i.item_ref ASC) AS rn
	FROM plugin_item_fields f
	JOIN plugin_items i ON i.item_ref = f.item_ref
	WHERE f.field = ? AND f.v_int <> 0 AND f.v_int IN (` + strings.Join(placeholders, ",") + `) AND ` + fieldDrivenWhere + `
)
SELECT r.bucket_pid, ` + prefixedItemCols("i.") + `
FROM ranked r
JOIN plugin_items i ON i.item_ref = r.item_ref
WHERE r.rn = 1`

	rows, err := s.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, err
	}
	var items []core.KindItem
	var pids []int64
	for rows.Next() {
		var pid int64
		it, _, _, scanErr := scanItemRow(prefixScanner{sc: rows, prefix: []any{&pid}})
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		items = append(items, it)
		pids = append(pids, pid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]*core.KindItem, len(personIDs))
	if len(items) == 0 {
		return out, nil
	}
	if err := s.hydrateArtSlice(ctx, items); err != nil {
		return nil, err
	}
	for i, pid := range pids {
		for _, targetIdx := range idxByPID[pid] {
			cp := items[i]
			out[targetIdx] = &cp
		}
	}
	return out, nil
}

// resolveNewestByPredicateBuckets resolves the newest visible item for numeric,
// date, duration, or envelope buckets in a single UNION ALL query + 1 batched
// art hydration query.
func (s *Service) resolveNewestByPredicateBuckets(ctx context.Context, fieldKey string, fieldTypes map[string]string, buckets []FacetBucket, itemWhere string, itemArgs []any) ([]*core.KindItem, error) {
	if len(buckets) == 0 {
		return nil, nil
	}
	arms := make([]string, 0, len(buckets))
	var args []any
	for idx, b := range buckets {
		var w strings.Builder
		w.WriteString(itemWhere)
		wArgs := make([]any, 0, 1+len(itemArgs)+4)
		wArgs = append(wArgs, idx)
		wArgs = append(wArgs, itemArgs...)
		libraryfilter.AppendPluginFacetClauses(&w, &wArgs, map[string][]string{fieldKey: {b.ID}}, fieldTypes)
		arms = append(arms, `SELECT * FROM (SELECT ? AS bucket_idx, i.item_ref FROM plugin_items i WHERE `+w.String()+` ORDER BY i.released DESC, i.added_at DESC, i.year DESC, i.item_ref ASC LIMIT 1) sub`+fmt.Sprintf("%d", idx))
		args = append(args, wArgs...)
	}
	querySQL := `WITH winners AS (
	` + strings.Join(arms, "\n\tUNION ALL\n\t") + `
)
SELECT w.bucket_idx, ` + prefixedItemCols("i.") + `
FROM winners w
JOIN plugin_items i ON i.item_ref = w.item_ref`

	rows, err := s.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, err
	}
	var items []core.KindItem
	var bucketIdxs []int
	for rows.Next() {
		var bIdx int
		it, _, _, scanErr := scanItemRow(prefixScanner{sc: rows, prefix: []any{&bIdx}})
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		items = append(items, it)
		bucketIdxs = append(bucketIdxs, bIdx)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]*core.KindItem, len(buckets))
	if len(items) == 0 {
		return out, nil
	}
	if err := s.hydrateArtSlice(ctx, items); err != nil {
		return nil, err
	}
	for i, bIdx := range bucketIdxs {
		if bIdx >= 0 && bIdx < len(out) {
			cp := items[i]
			out[bIdx] = &cp
		}
	}
	return out, nil
}
