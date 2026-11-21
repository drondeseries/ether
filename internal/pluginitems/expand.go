package pluginitems

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	ExpansionStateRunning = "running"
	ExpansionStateDone    = "done"
	ExpansionStateError   = "error"
)

// FacetExpansion is one durable row in plugin_facet_expansions recording the
// latest on-demand expansion of a (plugin, kind, facet, value) slice.
type FacetExpansion struct {
	PluginID   string
	KindKey    string
	LibraryID  core.LibraryID
	Facet      string
	ValueKey   string
	State      string
	StartedAt  time.Time
	FinishedAt time.Time
	ItemsAdded int64
	Error      string
}

// IsFresh reports whether e completed successfully within ttl of now.
func (e FacetExpansion) IsFresh(now time.Time, ttl time.Duration) bool {
	if e.State != ExpansionStateDone || e.FinishedAt.IsZero() || ttl <= 0 {
		return false
	}
	return now.Sub(e.FinishedAt) < ttl
}

// PersonFacetMatch identifies one (field, display name) credit for a synthetic
// plugin person ID within a library.
type PersonFacetMatch struct {
	Field string
	Name  string
}

// GetFacetExpansion looks up the durable expansion record for
// (pluginID, kindKey, facet, valueKey).
func (s *Service) GetFacetExpansion(ctx context.Context, pluginID, kindKey, facet, valueKey string) (FacetExpansion, bool, error) {
	if s == nil || s.db == nil {
		return FacetExpansion{}, false, nil
	}
	var (
		out        FacetExpansion
		libID      int64
		startedAt  int64
		finishedAt int64
	)
	err := s.db.QueryRowContext(ctx, `
SELECT plugin_id, kind_key, library_id, facet, value_key, state, started_at, finished_at, items_added, error
FROM plugin_facet_expansions
WHERE plugin_id = ? AND kind_key = ? AND facet = ? AND value_key = ?`,
		pluginID, kindKey, facet, valueKey,
	).Scan(&out.PluginID, &out.KindKey, &libID, &out.Facet, &out.ValueKey, &out.State, &startedAt, &finishedAt, &out.ItemsAdded, &out.Error)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FacetExpansion{}, false, nil
		}
		return FacetExpansion{}, false, err
	}
	out.LibraryID = core.LibraryID(libID)
	if startedAt > 0 {
		out.StartedAt = time.Unix(startedAt, 0).UTC()
	}
	if finishedAt > 0 {
		out.FinishedAt = time.Unix(finishedAt, 0).UTC()
	}
	return out, true, nil
}

// MarkFacetExpansionRunning upserts a running expansion record for
// (pluginID, kindKey, facet, valueKey).
func (s *Service) MarkFacetExpansionRunning(ctx context.Context, pluginID, kindKey string, libraryID core.LibraryID, facet, valueKey string) error {
	if s == nil || s.db == nil {
		return nil
	}
	nowUnix := s.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO plugin_facet_expansions(plugin_id, kind_key, library_id, facet, value_key, state, started_at, finished_at, items_added, error)
VALUES(?, ?, ?, ?, ?, ?, ?, 0, 0, '')
ON CONFLICT(plugin_id, kind_key, facet, value_key) DO UPDATE SET
	library_id  = excluded.library_id,
	state       = excluded.state,
	started_at  = excluded.started_at,
	finished_at = 0,
	items_added = 0,
	error       = ''`,
		pluginID, kindKey, int64(libraryID), facet, valueKey, ExpansionStateRunning, nowUnix,
	)
	return err
}

// FinishFacetExpansion records completion or failure of a background facet
// expansion in plugin_facet_expansions.
func (s *Service) FinishFacetExpansion(ctx context.Context, pluginID, kindKey string, libraryID core.LibraryID, facet, valueKey, state string, itemsAdded int, errMsg string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if len(errMsg) > 1024 {
		errMsg = errMsg[:1024]
	}
	nowUnix := s.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO plugin_facet_expansions(plugin_id, kind_key, library_id, facet, value_key, state, started_at, finished_at, items_added, error)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(plugin_id, kind_key, facet, value_key) DO UPDATE SET
	library_id  = excluded.library_id,
	state       = excluded.state,
	finished_at = excluded.finished_at,
	items_added = excluded.items_added,
	error       = excluded.error`,
		pluginID, kindKey, int64(libraryID), facet, valueKey, state, nowUnix, nowUnix, int64(itemsAdded), errMsg,
	)
	return err
}

// CountKindItems returns the stored item count for (pluginID, kindKey).
func (s *Service) CountKindItems(ctx context.Context, pluginID, kindKey string) (int, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	return s.countKindItems(ctx, pluginID, kindKey)
}

// LookupPersonFacetByName resolves a people-field bucket label within libraryID
// to its synthetic plugin person ID and canonical display name when present.
func (s *Service) LookupPersonFacetByName(ctx context.Context, libraryID core.LibraryID, field, name string) (personID int64, canonicalName string, ok bool, err error) {
	if s == nil || s.db == nil || libraryID <= 0 || field == "" || strings.TrimSpace(name) == "" {
		return 0, "", false, nil
	}
	trimmed := strings.TrimSpace(name)
	err = s.db.QueryRowContext(ctx, `
SELECT f.v_int, f.v_text
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE i.library_id = ? AND f.field = ? AND f.v_text = ? AND f.v_int > 0
LIMIT 1`, int64(libraryID), field, trimmed).Scan(&personID, &canonicalName)
	if err == nil && core.IsPluginPersonID(personID) {
		return personID, canonicalName, true, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, err
	}
	folded := FoldFieldText(trimmed)
	if folded == "" {
		return 0, "", false, nil
	}
	err = s.db.QueryRowContext(ctx, `
SELECT f.v_int, f.v_text
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE i.library_id = ? AND f.field = ? AND f.v_fold = ? AND f.v_int > 0
LIMIT 1`, int64(libraryID), field, folded).Scan(&personID, &canonicalName)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	if !core.IsPluginPersonID(personID) {
		return 0, "", false, nil
	}
	return personID, canonicalName, true, nil
}

// LookupPersonFacetsByID returns the distinct (field, name) pairs among fields
// crediting personID in libraryID.
func (s *Service) LookupPersonFacetsByID(ctx context.Context, libraryID core.LibraryID, fields []string, personID int64) ([]PersonFacetMatch, error) {
	if s == nil || s.db == nil || libraryID <= 0 || len(fields) == 0 || !core.IsPluginPersonID(personID) {
		return nil, nil
	}
	placeholders := make([]string, len(fields))
	args := make([]any, 0, 2+len(fields))
	args = append(args, int64(libraryID), personID)
	for i, fk := range fields {
		placeholders[i] = "?"
		args = append(args, fk)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT f.field, MIN(f.v_text)
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE i.library_id = ? AND f.v_int = ? AND f.v_int <> 0 AND f.field IN (`+strings.Join(placeholders, ",")+`) AND f.v_text <> ''
GROUP BY f.field
ORDER BY f.field ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonFacetMatch
	for rows.Next() {
		var m PersonFacetMatch
		if err := rows.Scan(&m.Field, &m.Name); err != nil {
			return nil, err
		}
		m.Name = strings.TrimSpace(m.Name)
		if m.Field != "" && m.Name != "" {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}
