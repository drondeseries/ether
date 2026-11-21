package pluginitems

import (
	"context"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// PersonMatch describes a credited person and their visible plugin items within
// a single plugin library.
type PersonMatch struct {
	ID          int64
	Name        string
	CreditCount int
	Thumb       string
	SampleItem  core.KindItem
	Roles       map[string]int
	Items       []core.KindItem
}

func declaredPeopleFields(decl plugins.MediaKindMeta) []string {
	var out []string
	for _, fd := range decl.Fields {
		if fd.Type == "people" {
			out = append(out, fd.Key)
		}
	}
	return out
}

func personWordPrefixMatch(name string, tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	words := tokenizeSearchQuery(name)
	if len(words) == 0 {
		return false
	}
	for _, tok := range tokens {
		matched := false
		for _, w := range words {
			if strings.HasPrefix(w, tok) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// SearchPeople finds people credited in any declared type:"people" field of
// libraryID whose name word-prefixes match query, returning up to personLimit
// people and up to itemLimit visible items per matched person.
func (s *Service) SearchPeople(ctx context.Context, libraryID core.LibraryID, policy *libraryfilter.Filter, query string, personLimit, itemLimit int) ([]PersonMatch, error) {
	if s == nil || s.db == nil || libraryID <= 0 {
		return nil, nil
	}
	rec, err := s.GetKindByLibrary(ctx, libraryID)
	if err != nil {
		return nil, nil
	}
	pFields := declaredPeopleFields(rec.Decl)
	if len(pFields) == 0 {
		return nil, nil
	}
	tokens := tokenizeSearchQuery(query)
	if strings.TrimSpace(query) != "" && len(tokens) == 0 {
		return nil, nil
	}
	if personLimit <= 0 {
		personLimit = 20
	}
	if personLimit > 100 {
		personLimit = 100
	}

	placeholders := make([]string, len(pFields))
	args := make([]any, 0, 2+len(pFields)+len(tokens))
	args = append(args, int64(libraryID))
	for i, fk := range pFields {
		placeholders[i] = "?"
		args = append(args, fk)
	}

	var where strings.Builder
	where.WriteString(`i.library_id = ? AND f.field IN (` + strings.Join(placeholders, ",") + `) AND f.v_int <> 0 AND f.v_int > 0 AND f.v_text <> ''`)
	for _, tok := range tokens {
		where.WriteString(` AND LOWER(f.v_text) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(tok)+"%")
	}
	if err := s.appendVisibility(ctx, &where, &args, libraryID, policy, false); err != nil {
		return nil, err
	}

	exactNorm := strings.Join(tokens, " ")
	fetchCap := max(personLimit*4, 100)
	qArgs := append(append([]any(nil), args...), exactNorm, fetchCap)
	rows, err := s.db.QueryContext(ctx, `
SELECT f.v_int, MIN(f.v_text) AS display_name, COUNT(DISTINCT f.item_ref) AS cnt
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE `+where.String()+`
GROUP BY f.v_int
ORDER BY
	CASE WHEN LOWER(MIN(f.v_text)) = ? THEN 0 ELSE 1 END ASC,
	cnt DESC,
	display_name ASC
LIMIT ?`, qArgs...)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id    int64
		name  string
		count int
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.name, &c.count); err != nil {
			rows.Close()
			return nil, err
		}
		if personWordPrefixMatch(c.name, tokens) {
			cands = append(cands, c)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(cands) == 0 {
		return nil, nil
	}

	sort.SliceStable(cands, func(i, j int) bool {
		li := strings.Join(tokenizeSearchQuery(cands[i].name), " ")
		lj := strings.Join(tokenizeSearchQuery(cands[j].name), " ")
		if (li == exactNorm) != (lj == exactNorm) {
			return li == exactNorm
		}
		if len(tokens) == 0 && cands[i].count != cands[j].count {
			return cands[i].count > cands[j].count
		}
		if li != lj {
			return li < lj
		}
		return cands[i].id < cands[j].id
	})
	if len(cands) > personLimit {
		cands = cands[:personLimit]
	}

	loadItems := itemLimit
	if loadItems <= 0 {
		loadItems = 5
	}
	out := make([]PersonMatch, 0, len(cands))
	for _, c := range cands {
		pm, ok, err := s.loadPersonMatch(ctx, rec, policy, pFields, c.id, c.name, c.count, loadItems, itemLimit > 0)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, pm)
		}
	}
	return out, nil
}

// PersonByID resolves a single plugin person by synthetic personID within libraryID.
func (s *Service) PersonByID(ctx context.Context, libraryID core.LibraryID, policy *libraryfilter.Filter, personID int64, itemLimit int) (PersonMatch, bool, error) {
	if s == nil || s.db == nil || libraryID <= 0 || !core.IsPluginPersonID(personID) {
		return PersonMatch{}, false, nil
	}
	rec, err := s.GetKindByLibrary(ctx, libraryID)
	if err != nil {
		return PersonMatch{}, false, nil
	}
	pFields := declaredPeopleFields(rec.Decl)
	if len(pFields) == 0 {
		return PersonMatch{}, false, nil
	}

	placeholders := make([]string, len(pFields))
	args := make([]any, 0, 2+len(pFields))
	args = append(args, int64(libraryID), personID)
	for i, fk := range pFields {
		placeholders[i] = "?"
		args = append(args, fk)
	}
	var where strings.Builder
	where.WriteString(`i.library_id + 0 = ? AND f.v_int = ? AND f.v_int <> 0 AND f.field IN (` + strings.Join(placeholders, ",") + `)`)
	if err := s.appendVisibility(ctx, &where, &args, libraryID, policy, false); err != nil {
		return PersonMatch{}, false, err
	}

	var name string
	var count int
	err = s.db.QueryRowContext(ctx, `
SELECT COALESCE(MIN(f.v_text), ''), COUNT(DISTINCT f.item_ref)
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE `+where.String(), args...).Scan(&name, &count)
	if err != nil || count == 0 || name == "" {
		return PersonMatch{}, false, err
	}

	loadItems := itemLimit
	if loadItems <= 0 {
		loadItems = 50
	}
	return s.loadPersonMatch(ctx, rec, policy, pFields, personID, name, count, loadItems, itemLimit > 0)
}

func (s *Service) loadPersonMatch(ctx context.Context, rec KindRecord, policy *libraryfilter.Filter, pFields []string, personID int64, fallbackName string, totalCount, loadLimit int, keepItems bool) (PersonMatch, bool, error) {
	if loadLimit <= 0 {
		loadLimit = 20
	}
	if loadLimit > MaxBrowseLimit {
		loadLimit = MaxBrowseLimit
	}
	placeholders := make([]string, len(pFields))
	args := make([]any, 0, 3+len(pFields))
	args = append(args, int64(rec.LibraryID))
	for i, fk := range pFields {
		placeholders[i] = "?"
		args = append(args, fk)
	}
	args = append(args, personID)

	var where strings.Builder
	where.WriteString(`i.library_id + 0 = ? AND i.item_ref IN (SELECT pf.item_ref FROM plugin_item_fields pf WHERE pf.field IN (` + strings.Join(placeholders, ",") + `) AND pf.v_int <> 0 AND pf.v_int = ?)`)
	if err := s.appendVisibility(ctx, &where, &args, rec.LibraryID, policy, false); err != nil {
		return PersonMatch{}, false, err
	}

	querySQL := `SELECT ` + prefixedItemCols("i.") + `
FROM plugin_items i
WHERE ` + where.String() + `
ORDER BY i.released DESC, i.year DESC, i.sort_title ASC, i.item_ref ASC
LIMIT ?`
	qArgs := append(append([]any(nil), args...), loadLimit)
	items, err := s.queryItems(ctx, querySQL, qArgs, true)
	if err != nil {
		return PersonMatch{}, false, err
	}
	if len(items) == 0 {
		return PersonMatch{}, false, nil
	}

	roles := map[string]int{}
	var roleWhere strings.Builder
	roleArgs := make([]any, 0, 2+len(pFields))
	roleArgs = append(roleArgs, int64(rec.LibraryID), personID)
	for _, fk := range pFields {
		roleArgs = append(roleArgs, fk)
	}
	roleWhere.WriteString(`i.library_id + 0 = ? AND f.v_int = ? AND f.v_int <> 0 AND f.field IN (` + strings.Join(placeholders, ",") + `)`)
	if s.appendVisibility(ctx, &roleWhere, &roleArgs, rec.LibraryID, policy, false) == nil {
		roleRows, err := s.db.QueryContext(ctx, `
SELECT f.field, COUNT(DISTINCT f.item_ref)
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE `+roleWhere.String()+`
GROUP BY f.field`, roleArgs...)
		if err == nil {
			for roleRows.Next() {
				var fk string
				var cnt int
				if roleRows.Scan(&fk, &cnt) == nil && cnt > 0 {
					roles[peopleFieldBucket(fk)] += cnt
				}
			}
			roleRows.Close()
		}
	}

	pm := PersonMatch{
		ID:          personID,
		Name:        fallbackName,
		CreditCount: totalCount,
		SampleItem:  items[0],
		Roles:       roles,
	}
	for _, it := range items {
		cast, dirs, wris := extractProjectedPeople(it.Fields, rec.Decl)
		for _, group := range [][]core.Person{cast, dirs, wris} {
			for _, p := range group {
				if p.TMDB == personID {
					if p.Name != "" && pm.Name == fallbackName {
						pm.Name = p.Name
					}
					if p.ThumbPath != "" && pm.Thumb == "" {
						pm.Thumb = p.ThumbPath
						pm.SampleItem = it
					}
				}
			}
		}
		if pm.Thumb != "" {
			break
		}
	}
	if keepItems {
		pm.Items = items
	}
	return pm, true, nil
}

// FindPersonID scans distinct indexed person IDs in libraryID and returns the
// first ID satisfying match. Used when resolving opaque person facet hashes on
// a cold server before the person has been browsed.
func (s *Service) FindPersonID(ctx context.Context, libraryID core.LibraryID, policy *libraryfilter.Filter, match func(personID int64, name string) bool) (int64, bool, error) {
	if s == nil || s.db == nil || libraryID <= 0 || match == nil {
		return 0, false, nil
	}
	rec, err := s.GetKindByLibrary(ctx, libraryID)
	if err != nil {
		return 0, false, nil
	}
	pFields := declaredPeopleFields(rec.Decl)
	if len(pFields) == 0 {
		return 0, false, nil
	}
	placeholders := make([]string, len(pFields))
	args := make([]any, 0, 1+len(pFields))
	args = append(args, int64(libraryID))
	for i, fk := range pFields {
		placeholders[i] = "?"
		args = append(args, fk)
	}
	var where strings.Builder
	where.WriteString(`i.library_id = ? AND f.field IN (` + strings.Join(placeholders, ",") + `) AND f.v_int <> 0 AND f.v_int > 0`)
	if err := s.appendVisibility(ctx, &where, &args, libraryID, policy, false); err != nil {
		return 0, false, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT f.v_int, MIN(f.v_text)
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE `+where.String()+`
GROUP BY f.v_int`, args...)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		var name string
		if err := rows.Scan(&pid, &name); err != nil {
			return 0, false, err
		}
		if match(pid, name) {
			return pid, true, nil
		}
	}
	return 0, false, rows.Err()
}
