package pluginitems

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/store"
)

// KindRecord is one installed (plugin, kind) row in plugin_kinds (spec §2.3).
type KindRecord struct {
	PluginID      string
	KindKey       string
	LibraryID     core.LibraryID
	Decl          plugins.MediaKindMeta
	DeclHash      string
	SchemaVersion int64
}

// SyncState is the persisted sync status for one (plugin, kind) (spec §2.3).
type SyncState struct {
	PluginID    string    `json:"pluginId"`
	KindKey     string    `json:"kindKey"`
	Cursor      string    `json:"cursor,omitempty"`
	Generation  uint64    `json:"generation"`
	LastStarted time.Time `json:"lastStarted,omitzero"`
	LastOK      time.Time `json:"lastOk,omitzero"`
	LastError   string    `json:"lastError,omitempty"`
	Items       int64     `json:"items"`
}

const itemCols = `item_ref, seq, plugin_id, kind_key, library_id, external_id, parent_ref, level, idx, title, sort_title, summary, year, released, rating, playable, child_count, leaf_count, hints, fields, etag, unavailable, added_at`

func prefixedItemCols(prefix string) string {
	parts := strings.Split(itemCols, ", ")
	for i, p := range parts {
		parts[i] = prefix + p
	}
	return strings.Join(parts, ", ")
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanItemRow(sc rowScanner) (core.KindItem, core.LibraryID, int64, error) {
	return scanItemRowExtra(sc)
}

func scanItemRowExtra(sc rowScanner, extra ...any) (core.KindItem, core.LibraryID, int64, error) {
	var (
		it             core.KindItem
		refStr         string
		seq            int64
		pluginID       string
		kindKey        string
		libID          int64
		parentRef      string
		idx            int64
		year           int64
		playable       int
		childCount     int64
		leafCount      int64
		hintsRaw       string
		fieldsRaw      string
		unavailableRaw string
	)
	dest := make([]any, 0, 23+len(extra))
	dest = append(dest,
		&refStr, &seq, &pluginID, &kindKey, &libID, &it.ID, &parentRef, &it.Level, &idx,
		&it.Title, &it.SortTitle, &it.Summary, &year, &it.Released, &it.Rating,
		&playable, &childCount, &leafCount, &hintsRaw, &fieldsRaw, &it.ETag, &unavailableRaw, &it.AddedAt,
	)
	dest = append(dest, extra...)
	if err := sc.Scan(dest...); err != nil {
		return core.KindItem{}, 0, 0, err
	}
	it.Ref = core.ItemRef(refStr)
	it.Key = core.PluginKey(seq)
	if parentRef != "" {
		it.ParentID = core.ItemRef(parentRef).External()
	}
	it.Index = int(idx)
	it.Year = int(year)
	if unavailableRaw != "" {
		if err := json.Unmarshal([]byte(unavailableRaw), &it.Unavailable); err != nil {
			return core.KindItem{}, 0, 0, err
		}
	}
	it.Playable = playable != 0 && it.Unavailable == nil
	it.ChildCount = int(childCount)
	it.LeafCount = int(leafCount)
	if hintsRaw != "" && hintsRaw != "{}" {
		it.Hints = json.RawMessage(hintsRaw)
	}
	if fieldsRaw != "" && fieldsRaw != "{}" {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(fieldsRaw), &m); err == nil && len(m) > 0 {
			it.Fields = m
		}
	}
	return it, core.LibraryID(libID), seq, nil
}

func (s *Service) queryItems(ctx context.Context, query string, args []any, hydrateArt bool) ([]core.KindItem, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []core.KindItem
	for rows.Next() {
		it, _, _, err := scanItemRow(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if hydrateArt && len(items) > 0 {
		if err := s.hydrateArtSlice(ctx, items); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func (s *Service) hydrateArtSlice(ctx context.Context, items []core.KindItem) error {
	if len(items) == 0 {
		return nil
	}
	byRef := make(map[core.ItemRef][]int, len(items))
	placeholders := make([]string, 0, len(items))
	args := make([]any, 0, len(items))
	for i := range items {
		ref := items[i].Ref
		if _, seen := byRef[ref]; !seen {
			placeholders = append(placeholders, "?")
			args = append(args, string(ref))
		}
		byRef[ref] = append(byRef[ref], i)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT item_ref, role, url, width, height FROM plugin_item_art WHERE item_ref IN (`+strings.Join(placeholders, ",")+`) ORDER BY item_ref, role`,
		args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var refStr, role, url string
		var w, h int64
		if err := rows.Scan(&refStr, &role, &url, &w, &h); err != nil {
			return err
		}
		art := core.KindArtRef{Role: role, URL: url, Width: int(w), Height: int(h)}
		for _, idx := range byRef[core.ItemRef(refStr)] {
			items[idx].Art = append(items[idx].Art, art)
		}
	}
	return rows.Err()
}

// UpsertKind installs or upgrades one (plugin, kind) declaration in
// plugin_kinds (spec §2.3, §2.5). On upgrade, decl_hash is compared against
// the stored declaration: additive changes bump schema_version; non-additive
// changes are refused with a ValidationError unless replaceContents is true,
// in which case existing items for that kind are purged and sync state reset.
func (s *Service) UpsertKind(ctx context.Context, pluginID string, libraryID core.LibraryID, decl plugins.MediaKindMeta, replaceContents bool) (KindRecord, error) {
	if strings.TrimSpace(pluginID) == "" || strings.TrimSpace(decl.Key) == "" || libraryID <= 0 {
		return KindRecord{}, errors.New("pluginitems: pluginID, kindKey and libraryID are required")
	}
	mu := s.kindLock(pluginID, decl.Key)
	mu.Lock()
	defer mu.Unlock()

	declJSON, err := json.Marshal(decl)
	if err != nil {
		return KindRecord{}, err
	}
	declHash := decl.DeclHash()
	isPG := store.DialectOf(s.db) == store.Postgres

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KindRecord{}, err
	}
	defer tx.Rollback()

	var (
		oldDeclJSON string
		oldHash     string
		schemaVer   int64
		purged      bool
	)
	err = tx.QueryRowContext(ctx,
		`SELECT decl, decl_hash, schema_version FROM plugin_kinds WHERE plugin_id = ? AND kind_key = ?`,
		pluginID, decl.Key).Scan(&oldDeclJSON, &oldHash, &schemaVer)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		schemaVer = 1
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plugin_kinds(plugin_id, kind_key, library_id, decl, decl_hash, schema_version, fts_version) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			pluginID, decl.Key, int64(libraryID), string(declJSON), declHash, schemaVer, currentFTSVersion); err != nil {
			return KindRecord{}, err
		}
		var preExisting int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM plugin_items WHERE plugin_id = ? AND kind_key = ?`,
			pluginID, decl.Key).Scan(&preExisting); err != nil {
			return KindRecord{}, err
		}
		if preExisting > 0 {
			if err := reindexKindTx(ctx, tx, isPG, true, pluginID, decl.Key, libraryID, decl); err != nil {
				return KindRecord{}, err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plugin_sync_state(plugin_id, kind_key) VALUES(?, ?) ON CONFLICT(plugin_id, kind_key) DO NOTHING`,
			pluginID, decl.Key); err != nil {
			return KindRecord{}, err
		}
	case err != nil:
		return KindRecord{}, err
	default:
		needReindex := false
		if oldHash != declHash {
			var oldDecl plugins.MediaKindMeta
			if err := json.Unmarshal([]byte(oldDeclJSON), &oldDecl); err != nil {
				return KindRecord{}, err
			}
			if upgErr := plugins.ValidateMediaKindsUpgrade([]plugins.MediaKindMeta{oldDecl}, []plugins.MediaKindMeta{decl}); upgErr != nil {
				if !replaceContents {
					return KindRecord{}, upgErr
				}
				if err := s.purgeKindTx(ctx, tx, pluginID, decl.Key, libraryID, false); err != nil {
					return KindRecord{}, err
				}
				purged = true
			} else {
				// Only the declared fields and search fields shape the
				// per-item index. A change to tabs, shelves, card shape or
				// labels must not rewrite every item under the writer lock.
				needReindex = indexShapeChanged(oldDecl, decl)
			}
			schemaVer++
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugin_kinds SET library_id = ?, decl = ?, decl_hash = ?, schema_version = ?, fts_version = ? WHERE plugin_id = ? AND kind_key = ?`,
			int64(libraryID), string(declJSON), declHash, schemaVer, currentFTSVersion, pluginID, decl.Key); err != nil {
			return KindRecord{}, err
		}
		if needReindex {
			if err := reindexKindTx(ctx, tx, isPG, true, pluginID, decl.Key, libraryID, decl); err != nil {
				return KindRecord{}, err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plugin_sync_state(plugin_id, kind_key) VALUES(?, ?) ON CONFLICT(plugin_id, kind_key) DO NOTHING`,
			pluginID, decl.Key); err != nil {
			return KindRecord{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return KindRecord{}, err
	}
	if purged {
		s.forgetLibraryStats(libraryID)
	}
	return KindRecord{
		PluginID:      pluginID,
		KindKey:       decl.Key,
		LibraryID:     libraryID,
		Decl:          decl,
		DeclHash:      declHash,
		SchemaVersion: schemaVer,
	}, nil
}

// indexShapeChanged reports whether the parts of a kind declaration that
// extractIndexData reads (declared fields and search fields) differ.
func indexShapeChanged(oldDecl, newDecl plugins.MediaKindMeta) bool {
	o, err1 := json.Marshal(struct {
		F any
		S any
	}{oldDecl.Fields, oldDecl.Browse.SearchFields})
	n, err2 := json.Marshal(struct {
		F any
		S any
	}{newDecl.Fields, newDecl.Browse.SearchFields})
	return err1 != nil || err2 != nil || string(o) != string(n)
}

// GetKind returns the installed KindRecord for (pluginID, kindKey).
func (s *Service) GetKind(ctx context.Context, pluginID, kindKey string) (KindRecord, error) {
	return s.scanKind(s.db.QueryRowContext(ctx,
		`SELECT plugin_id, kind_key, library_id, decl, decl_hash, schema_version FROM plugin_kinds WHERE plugin_id = ? AND kind_key = ?`,
		pluginID, kindKey))
}

// GetKindByLibrary returns the installed KindRecord for libraryID.
func (s *Service) GetKindByLibrary(ctx context.Context, libraryID core.LibraryID) (KindRecord, error) {
	return s.scanKind(s.db.QueryRowContext(ctx,
		`SELECT plugin_id, kind_key, library_id, decl, decl_hash, schema_version FROM plugin_kinds WHERE library_id = ?`,
		int64(libraryID)))
}

// ListKinds returns all installed kinds for pluginID (or all plugins when pluginID == "").
func (s *Service) ListKinds(ctx context.Context, pluginID string) ([]KindRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if pluginID == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT plugin_id, kind_key, library_id, decl, decl_hash, schema_version FROM plugin_kinds ORDER BY plugin_id, kind_key`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT plugin_id, kind_key, library_id, decl, decl_hash, schema_version FROM plugin_kinds WHERE plugin_id = ? ORDER BY kind_key`,
			pluginID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KindRecord
	for rows.Next() {
		rec, err := s.scanKind(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Service) scanKind(sc rowScanner) (KindRecord, error) {
	var (
		rec      KindRecord
		libID    int64
		declJSON string
	)
	if err := sc.Scan(&rec.PluginID, &rec.KindKey, &libID, &declJSON, &rec.DeclHash, &rec.SchemaVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return KindRecord{}, core.ErrNotFound
		}
		return KindRecord{}, err
	}
	rec.LibraryID = core.LibraryID(libID)
	if err := json.Unmarshal([]byte(declJSON), &rec.Decl); err != nil {
		return KindRecord{}, err
	}
	return rec, nil
}

// KindUpsert implements the store side of plugins.KindBackends.KindUpsert
// (spec §3.6): validates and upserts items into the kind's library, updates
// tallies and FTS, and bumps the sync generation.
func (s *Service) KindUpsert(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, error) {
	return s.UpsertItems(ctx, pluginID, kindKey, items)
}

// KindUpsertWithStats is KindUpsert reporting what the batch did, for the
// host wire reply (#646).
func (s *Service) KindUpsertWithStats(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (plugins.KindUpsertResult, error) {
	stats, err := s.UpsertItemsWithStats(ctx, pluginID, kindKey, items)
	return plugins.KindUpsertResult{
		Accepted: stats.Accepted, Added: stats.Added,
		Updated: stats.Updated, Unchanged: stats.Unchanged,
	}, err
}

// UpsertItems validates and writes a batch of KindItems for (pluginID,
// kindKey), maintaining tallies, typed EAV fields, art and FTS. The batch
// commits in bounded chunks under the per-kind lock (#646).
func (s *Service) UpsertItems(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, error) {
	accepted, _, err := s.UpsertItemsWithAdded(ctx, pluginID, kindKey, items)
	return accepted, err
}

// UpsertItemsWithAdded validates and writes a batch of KindItems for
// (pluginID, kindKey), returning both accepted items and newly inserted items.
func (s *Service) UpsertItemsWithAdded(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, int, error) {
	stats, err := s.UpsertItemsWithStats(ctx, pluginID, kindKey, items)
	return stats.Accepted, stats.Added, err
}

// UpsertItemsWithStats is UpsertItemsWithAdded plus the updated/unchanged
// split: unchanged items are detected by content hash and not rewritten.
func (s *Service) UpsertItemsWithStats(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (UpsertStats, error) {
	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()
	return s.upsertItemsWithStatsLocked(ctx, pluginID, kindKey, items, s.now().Unix(), true, true)
}

// upsertItemsForSync writes a batch as part of a pull SyncKind pass; SyncKind
// notifies the planner-statistics refresher once the full sync completes.
func (s *Service) upsertItemsForSync(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, error) {
	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()
	stats, err := s.upsertItemsWithStatsLocked(ctx, pluginID, kindKey, items, s.now().Unix(), true, false)
	return stats.Accepted, err
}

func (s *Service) upsertItemsLocked(ctx context.Context, pluginID, kindKey string, items []core.KindItem, seenAt int64, bumpGen bool) (int, error) {
	stats, err := s.upsertItemsWithStatsLocked(ctx, pluginID, kindKey, items, seenAt, bumpGen, true)
	return stats.Accepted, err
}

func (s *Service) upsertItemsWithStatsLocked(ctx context.Context, pluginID, kindKey string, items []core.KindItem, seenAt int64, bumpGen, notifyStats bool) (UpsertStats, error) {
	kind, err := s.GetKind(ctx, pluginID, kindKey)
	if err != nil {
		return UpsertStats{}, err
	}
	if len(items) == 0 {
		return UpsertStats{}, nil
	}

	// Track levels in the incoming batch so parent levels within the same
	// batch are verified even before DB insertion.
	batchLevels := make(map[string]string, len(items))
	for _, it := range items {
		if it.ID != "" && it.Level != "" {
			batchLevels[it.ID] = it.Level
		}
	}

	// Look up any referenced parents already in the database.
	parentIDs := make([]string, 0)
	seenParents := map[string]bool{}
	for _, it := range items {
		if it.ParentID != "" && !seenParents[it.ParentID] {
			if _, inBatch := batchLevels[it.ParentID]; !inBatch {
				seenParents[it.ParentID] = true
				parentIDs = append(parentIDs, it.ParentID)
			}
		}
	}
	dbParentLevels, err := s.lookupLevelsByExternalIDs(ctx, pluginID, kindKey, parentIDs)
	if err != nil {
		return UpsertStats{}, err
	}

	// Validate every item and deduplicate by ID (last occurrence in batch wins).
	validated := make([]ValidatedItem, 0, len(items))
	posByID := make(map[string]int, len(items))
	totalDroppedFields := 0
	for _, it := range items {
		parentLevel := ""
		if it.ParentID != "" {
			if lv, ok := batchLevels[it.ParentID]; ok {
				parentLevel = lv
			} else if lv, ok := dbParentLevels[it.ParentID]; ok {
				parentLevel = lv
			}
		}
		v, err := ValidateItem(kind.Decl, it, parentLevel)
		if err != nil {
			return UpsertStats{}, err
		}
		v.Item.Ref = core.NewItemRef(pluginID, kindKey, v.Item.ID)
		totalDroppedFields += v.DroppedFields
		if idx, dup := posByID[v.Item.ID]; dup {
			validated[idx] = v
		} else {
			posByID[v.Item.ID] = len(validated)
			validated = append(validated, v)
		}
	}
	if totalDroppedFields > 0 {
		s.noteAttention(ctx, pluginID, fmt.Sprintf("dropped %d undeclared field(s) on kind %s", totalDroppedFields, kindKey))
	}

	// Prepare every stored representation and content hash before the
	// first write transaction opens, then commit in bounded chunks so
	// the write gate is released between them (#646). The per-kind lock
	// is held throughout, so the batch stays atomic to this kind's
	// writers; a failure partway leaves earlier chunks committed.
	prepared := make([]preparedItem, len(validated))
	for i, v := range validated {
		p, err := prepareUpsertItem(pluginID, kindKey, kind.DeclHash, v)
		if err != nil {
			return UpsertStats{}, err
		}
		prepared[i] = p
	}

	nowUnix := s.now().Unix()
	if seenAt <= 0 {
		seenAt = nowUnix
	}
	stats := UpsertStats{Accepted: len(items)}
	var totalItems int64
	for start := 0; start < len(prepared); start += upsertChunkSize {
		end := min(start+upsertChunkSize, len(prepared))
		res, err := s.writeUpsertChunk(ctx, kind, pluginID, kindKey, prepared[start:end], seenAt, nowUnix, bumpGen && end == len(prepared))
		if err != nil {
			// The failed chunk rolled back; only earlier committed
			// chunks are counted.
			return stats, err
		}
		stats.Added += res.added
		stats.Updated += res.updated
		stats.Unchanged += res.unchanged
		if res.totalItems > 0 {
			totalItems = res.totalItems
		}
	}
	if notifyStats && totalItems > 0 {
		s.noteLibraryItems(kind.LibraryID, totalItems, false)
	}
	return stats, nil
}

func applyMigration3SQLite(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`ALTER TABLE plugin_kinds ADD COLUMN fts_version INTEGER NOT NULL DEFAULT 0`,
		`DROP TABLE IF EXISTS plugin_item_fts`,
		`CREATE VIRTUAL TABLE plugin_item_fts USING fts5(item_ref UNINDEXED, library_id UNINDEXED, title, fields, people)`,
		`INSERT OR REPLACE INTO plugin_item_fts_config(k, v) VALUES ('rank', 'bm25(10.0, 2.0, 1.0)')`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return reindexAllTx(ctx, tx, false)
}

func applyMigration3Postgres(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE plugin_kinds ADD COLUMN fts_version BIGINT NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return reindexAllTx(ctx, tx, true)
}

var migration4FoldBatchSize = 500

// contentRatingLevel is the persisted rating_level for one item's fields:
// the contentrating enforcement level of its contentRating / certification /
// mpaa field, or 0 (unrated) when it carries none. Projected titles read the
// same fields, so the stored level and the in-process check agree.
func contentRatingLevel(fields map[string]json.RawMessage) int {
	for _, key := range []string{"contentRating", "certification", "mpaa"} {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s = strings.TrimSpace(s); s != "" {
				return int(contentrating.EnforceLevelOf(s))
			}
		}
	}
	return 0
}

func applyMigration9(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE plugin_items ADD COLUMN rating_level INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	const batchSize = 500
	cursor := ""
	for {
		rows, err := tx.QueryContext(ctx, `
SELECT item_ref, fields FROM plugin_items
WHERE item_ref > ?
ORDER BY item_ref ASC
LIMIT ?`, cursor, batchSize)
		if err != nil {
			return err
		}
		type itemFields struct {
			ref    string
			fields string
		}
		var batch []itemFields
		for rows.Next() {
			var it itemFields
			if err := rows.Scan(&it.ref, &it.fields); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, it)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		for _, it := range batch {
			var fields map[string]json.RawMessage
			if json.Unmarshal([]byte(it.fields), &fields) != nil {
				continue
			}
			if lvl := contentRatingLevel(fields); lvl != 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE plugin_items SET rating_level = ? WHERE item_ref = ?`, lvl, it.ref); err != nil {
					return err
				}
			}
		}
		cursor = batch[len(batch)-1].ref
		if len(batch) < batchSize {
			return nil
		}
	}
}

func applyMigration4(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE plugin_item_fields ADD COLUMN v_fold TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := backfillFieldFoldsTx(ctx, tx, migration4FoldBatchSize); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `CREATE INDEX plugin_item_fields_fold ON plugin_item_fields(field, v_fold, v_text, v_int, item_ref)`)
	return err
}

// applyMigration6 adds plugin_item_fields_lookup_real and flags only existing
// plugin_kinds that declared previously-unindexed fields for lazy reindex on
// startup so all declared fields become index-backed for grouping without
// reindexing 1M-item kinds whose fields were already indexed.
func applyMigration6(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS plugin_item_fields_lookup_real ON plugin_item_fields(field, v_real, item_ref)`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT plugin_id, kind_key, decl FROM plugin_kinds`)
	if err != nil {
		return err
	}
	type kindPair struct {
		pluginID string
		kindKey  string
	}
	var toReindex []kindPair
	for rows.Next() {
		var pid, kkey, declJSON string
		if err := rows.Scan(&pid, &kkey, &declJSON); err != nil {
			rows.Close()
			return err
		}
		var decl plugins.MediaKindMeta
		if err := json.Unmarshal([]byte(declJSON), &decl); err != nil {
			continue
		}
		for _, f := range decl.Fields {
			if !f.Indexed && f.Type != "people" {
				toReindex = append(toReindex, kindPair{pluginID: pid, kindKey: kkey})
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, k := range toReindex {
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugin_kinds SET fts_version = 0 WHERE plugin_id = ? AND kind_key = ?`,
			k.pluginID, k.kindKey); err != nil {
			return err
		}
	}
	return nil
}

func backfillFieldFoldsTx(ctx context.Context, tx *sql.Tx, batchSize int) error {
	if batchSize <= 0 {
		batchSize = 500
	}
	curField, curText := "", ""
	for {
		rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT field, v_text
FROM plugin_item_fields
WHERE v_text <> '' AND (field > ? OR (field = ? AND v_text > ?))
ORDER BY field ASC, v_text ASC
LIMIT ?`, curField, curField, curText, batchSize)
		if err != nil {
			return err
		}
		type fieldTextPair struct {
			field string
			text  string
		}
		var batch []fieldTextPair
		for rows.Next() {
			var p fieldTextPair
			if err := rows.Scan(&p.field, &p.text); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		for _, p := range batch {
			if folded := FoldFieldText(p.text); folded != "" {
				if _, err := tx.ExecContext(ctx,
					`UPDATE plugin_item_fields SET v_fold = ? WHERE field = ? AND v_text = ?`,
					folded, p.field, p.text); err != nil {
					return err
				}
			}
		}
		last := batch[len(batch)-1]
		curField, curText = last.field, last.text
		if len(batch) < batchSize {
			break
		}
	}
	return nil
}

type pendingKindRow struct {
	pluginID  string
	kindKey   string
	libraryID core.LibraryID
	declJSON  string
}

func reindexAllTx(ctx context.Context, tx *sql.Tx, isPG bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT plugin_id, kind_key, library_id, decl FROM plugin_kinds ORDER BY plugin_id, kind_key`)
	if err != nil {
		return err
	}
	var kinds []pendingKindRow
	for rows.Next() {
		var r pendingKindRow
		var libID int64
		if err := rows.Scan(&r.pluginID, &r.kindKey, &libID, &r.declJSON); err != nil {
			rows.Close()
			return err
		}
		r.libraryID = core.LibraryID(libID)
		kinds = append(kinds, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, k := range kinds {
		var decl plugins.MediaKindMeta
		if err := json.Unmarshal([]byte(k.declJSON), &decl); err != nil {
			continue
		}
		if err := reindexKindTx(ctx, tx, isPG, false, k.pluginID, k.kindKey, k.libraryID, decl); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugin_kinds SET fts_version = ? WHERE plugin_id = ? AND kind_key = ?`,
			currentFTSVersion, k.pluginID, k.kindKey); err != nil {
			return err
		}
	}

	if isPG {
		_, err = tx.ExecContext(ctx, `
UPDATE plugin_items
SET search_tsv = setweight(to_tsvector('simple', title), 'A')
WHERE NOT EXISTS (
	SELECT 1 FROM plugin_kinds k WHERE k.plugin_id = plugin_items.plugin_id AND k.kind_key = plugin_items.kind_key
)`)
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO plugin_item_fts(rowid, item_ref, library_id, title, fields, people)
SELECT p.seq, p.item_ref, p.library_id, p.title, '', ''
FROM plugin_items p
WHERE NOT EXISTS (
	SELECT 1 FROM plugin_kinds k WHERE k.plugin_id = p.plugin_id AND k.kind_key = p.kind_key
) AND p.title <> ''`)
	return err
}

func (s *Service) reindexPendingKinds(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT plugin_id, kind_key, library_id, decl FROM plugin_kinds WHERE fts_version < ? ORDER BY plugin_id, kind_key`,
		currentFTSVersion)
	if err != nil {
		return err
	}
	var pending []pendingKindRow
	for rows.Next() {
		var r pendingKindRow
		var libID int64
		if err := rows.Scan(&r.pluginID, &r.kindKey, &libID, &r.declJSON); err != nil {
			rows.Close()
			return err
		}
		r.libraryID = core.LibraryID(libID)
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(pending) == 0 {
		return nil
	}

	isPG := store.DialectOf(s.db) == store.Postgres
	for _, k := range pending {
		var decl plugins.MediaKindMeta
		if err := json.Unmarshal([]byte(k.declJSON), &decl); err != nil {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := reindexKindTx(ctx, tx, isPG, true, k.pluginID, k.kindKey, k.libraryID, decl); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugin_kinds SET fts_version = ? WHERE plugin_id = ? AND kind_key = ?`,
			currentFTSVersion, k.pluginID, k.kindKey); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

type reindexItemRow struct {
	itemRef    string
	seq        int64
	libraryID  core.LibraryID
	title      string
	fieldsJSON string
}

func reindexKindTx(ctx context.Context, tx *sql.Tx, isPG, hasFold bool, pluginID, kindKey string, fallbackLibID core.LibraryID, decl plugins.MediaKindMeta) error {
	const batchSize = 500
	cursor := ""
	for {
		rows, err := tx.QueryContext(ctx, `
SELECT item_ref, seq, library_id, title, fields
FROM plugin_items
WHERE plugin_id = ? AND kind_key = ? AND item_ref > ?
ORDER BY item_ref ASC
LIMIT ?`, pluginID, kindKey, cursor, batchSize)
		if err != nil {
			return err
		}
		var batch []reindexItemRow
		for rows.Next() {
			var r reindexItemRow
			var libID int64
			if err := rows.Scan(&r.itemRef, &r.seq, &libID, &r.title, &r.fieldsJSON); err != nil {
				rows.Close()
				return err
			}
			if libID > 0 {
				r.libraryID = core.LibraryID(libID)
			} else {
				r.libraryID = fallbackLibID
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		for _, r := range batch {
			indexed, searchFields, searchPeople := extractIndexData(decl, r.fieldsJSON)
			if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_fields WHERE item_ref = ?`, r.itemRef); err != nil {
				return err
			}
			for _, fv := range indexed {
				if hasFold {
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO plugin_item_fields(item_ref, field, v_text, v_int, v_real, v_fold) VALUES(?, ?, ?, ?, ?, ?)
						 ON CONFLICT(item_ref, field, v_text, v_int) DO UPDATE SET v_real = excluded.v_real, v_fold = excluded.v_fold`,
						r.itemRef, fv.Field, fv.Text, fv.Int, fv.Real, fv.Fold); err != nil {
						return err
					}
				} else {
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO plugin_item_fields(item_ref, field, v_text, v_int, v_real) VALUES(?, ?, ?, ?, ?)
						 ON CONFLICT(item_ref, field, v_text, v_int) DO UPDATE SET v_real = excluded.v_real`,
						r.itemRef, fv.Field, fv.Text, fv.Int, fv.Real); err != nil {
						return err
					}
				}
			}
			if isPG {
				if _, err := tx.ExecContext(ctx, `
UPDATE plugin_items
SET search_tsv = setweight(to_tsvector('simple', ?), 'A') || setweight(to_tsvector('simple', ?), 'B') || setweight(to_tsvector('simple', ?), 'C')
WHERE item_ref = ?`,
					r.title, searchFields, searchPeople, r.itemRef); err != nil {
					return err
				}
			} else {
				if err := sqliteIndexFTS(ctx, tx, r.seq, core.ItemRef(r.itemRef), r.libraryID, r.title, searchFields, searchPeople); err != nil {
					return err
				}
			}
		}
		cursor = batch[len(batch)-1].itemRef
		if len(batch) < batchSize {
			break
		}
	}
	dialect := store.SQLite
	if isPG {
		dialect = store.Postgres
	}
	if hasTableTx(ctx, tx, dialect, "plugin_item_field_tally") {
		if err := rebuildKindFieldTallyTx(ctx, tx, pluginID, kindKey, fallbackLibID); err != nil {
			return err
		}
	}
	return nil
}

func rebuildKindFieldTallyTx(ctx context.Context, tx *sql.Tx, pluginID, kindKey string, fallbackLibID core.LibraryID) error {
	libSet := map[int64]bool{}
	if fallbackLibID > 0 {
		libSet[int64(fallbackLibID)] = true
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT library_id FROM plugin_items WHERE plugin_id = ? AND kind_key = ? AND library_id > 0`,
		pluginID, kindKey)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		libSet[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	libs := make([]int64, 0, len(libSet))
	for id := range libSet {
		libs = append(libs, id)
	}
	slices.Sort(libs)
	for _, libID := range libs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_field_tally WHERE library_id = ?`, libID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_item_field_tally(library_id, level, field, v_text, cnt)
SELECT i.library_id, i.level, f.field, f.v_text, COUNT(*)
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE i.library_id = ? AND i.level <> '' AND f.v_text <> ''
GROUP BY i.library_id, i.level, f.field, f.v_text`, libID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_item_field_tally(library_id, level, field, v_text, cnt)
SELECT library_id, '', field, v_text, CAST(SUM(cnt) AS BIGINT)
FROM plugin_item_field_tally
WHERE library_id = ? AND level <> ''
GROUP BY library_id, field, v_text`, libID); err != nil {
			return err
		}
	}
	return nil
}

func extractIndexData(decl plugins.MediaKindMeta, fieldsJSON string) ([]IndexedFieldValue, string, string) {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return nil, "", ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil || len(fields) == 0 {
		return nil, "", ""
	}
	declaredFields := make(map[string]plugins.KindField, len(decl.Fields))
	for _, f := range decl.Fields {
		declaredFields[f.Key] = f
	}
	searchFieldSet := make(map[string]bool, len(decl.Browse.SearchFields))
	for _, sf := range decl.Browse.SearchFields {
		searchFieldSet[sf] = true
	}
	var (
		indexed     []IndexedFieldValue
		searchParts []string
		peopleParts []string
	)
	for k, raw := range fields {
		fdecl, exists := declaredFields[k]
		if !exists {
			continue
		}
		vals, texts, err := validateFieldValue(fdecl, raw)
		if err != nil {
			continue
		}
		indexed = append(indexed, vals...)
		if fdecl.Type == "people" {
			if len(texts) > 0 {
				peopleParts = append(peopleParts, strings.Join(texts, " "))
			}
		} else if searchFieldSet[k] && len(texts) > 0 {
			searchParts = append(searchParts, strings.Join(texts, " "))
		}
	}
	searchText := strings.Join(searchParts, " ")
	if len(searchText) > MaxFTSTextBytes {
		searchText = clipUTF8(searchText, MaxFTSTextBytes)
	}
	peopleText := strings.Join(peopleParts, " ")
	if len(peopleText) > MaxFTSTextBytes {
		peopleText = clipUTF8(peopleText, MaxFTSTextBytes)
	}
	return indexed, searchText, peopleText
}

type existingItemMeta struct {
	seq     int64
	level   string
	addedAt int64
	hash    string
}

func lookupExistingItemsTx(ctx context.Context, tx *sql.Tx, items []preparedItem) (map[core.ItemRef]existingItemMeta, error) {
	out := make(map[core.ItemRef]existingItemMeta, len(items))
	const chunkSize = 250
	for start := 0; start < len(items); start += chunkSize {
		end := min(start+chunkSize, len(items))
		chunk := items[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, v := range chunk {
			placeholders[i] = "?"
			args[i] = string(v.validated.Item.Ref)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT item_ref, seq, level, added_at, content_hash FROM plugin_items WHERE item_ref IN (`+strings.Join(placeholders, ",")+`)`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var refStr, lv, hash string
			var seq, added int64
			if err := rows.Scan(&refStr, &seq, &lv, &added, &hash); err != nil {
				rows.Close()
				return nil, err
			}
			out[core.ItemRef(refStr)] = existingItemMeta{seq: seq, level: lv, addedAt: added, hash: hash}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Service) lookupLevelsByExternalIDs(ctx context.Context, pluginID, kindKey string, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(ids))
	const chunkSize = 250
	for start := 0; start < len(ids); start += chunkSize {
		end := min(start+chunkSize, len(ids))
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 2+len(chunk))
		args = append(args, pluginID, kindKey)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT external_id, level FROM plugin_items WHERE plugin_id = ? AND kind_key = ? AND external_id IN (`+strings.Join(placeholders, ",")+`)`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var extID, lv string
			if err := rows.Scan(&extID, &lv); err != nil {
				rows.Close()
				return nil, err
			}
			out[extID] = lv
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// allocateSeqRangeTx reserves count sequence numbers in plugin_items_seq
// and returns the first reserved sequence number. Sequence numbers are
// never decremented or reused across deletes or plugin removals (spec §2.2).
func allocateSeqRangeTx(ctx context.Context, tx *sql.Tx, count int64) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_items_seq SET next = next + ? WHERE id = 1`, count); err != nil {
		return 0, err
	}
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT next FROM plugin_items_seq WHERE id = 1`).Scan(&next); err != nil {
		return 0, err
	}
	first := next - count
	if first+count >= core.PluginKeyMaxSeq {
		return 0, errors.New("pluginitems: PluginKey sequence space exhausted")
	}
	return first, nil
}

func applyTallyDeltaTx(ctx context.Context, tx *sql.Tx, libraryID core.LibraryID, delta map[string]int64) error {
	for level, d := range delta {
		if d == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_item_tally(library_id, level, n) VALUES(?, ?, ?)
ON CONFLICT(library_id, level) DO UPDATE SET n = CASE
	WHEN plugin_item_tally.n + excluded.n < 0 THEN 0
	ELSE plugin_item_tally.n + excluded.n
END`, int64(libraryID), level, d); err != nil {
			return err
		}
	}
	return nil
}

type fieldTallyKey struct {
	level string
	field string
	text  string
}

type existingTextField struct {
	ref   core.ItemRef
	field string
	text  string
}

func addFieldTallyDelta(delta map[fieldTallyKey]int64, level, field, text string, d int64) {
	if level == "" || field == "" || text == "" || d == 0 {
		return
	}
	delta[fieldTallyKey{level: level, field: field, text: text}] += d
	delta[fieldTallyKey{level: "", field: field, text: text}] += d
}

func lookupExistingTextFieldsTx(ctx context.Context, tx *sql.Tx, refs []core.ItemRef) ([]existingTextField, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	var out []existingTextField
	const chunkSize = 250
	for start := 0; start < len(refs); start += chunkSize {
		end := min(start+chunkSize, len(refs))
		chunk := refs[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, r := range chunk {
			placeholders[i] = "?"
			args[i] = string(r)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT item_ref, field, v_text FROM plugin_item_fields WHERE item_ref IN (`+strings.Join(placeholders, ",")+`) AND v_text <> ''`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var refStr, field, text string
			if err := rows.Scan(&refStr, &field, &text); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, existingTextField{ref: core.ItemRef(refStr), field: field, text: text})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func applyFieldTallyDeltaTx(ctx context.Context, tx *sql.Tx, libraryID core.LibraryID, delta map[fieldTallyKey]int64) error {
	if len(delta) == 0 || libraryID <= 0 {
		return nil
	}
	keys := make([]fieldTallyKey, 0, len(delta))
	for k, d := range delta {
		if d != 0 {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	slices.SortFunc(keys, func(a, b fieldTallyKey) int {
		if c := strings.Compare(a.level, b.level); c != 0 {
			return c
		}
		if c := strings.Compare(a.field, b.field); c != 0 {
			return c
		}
		return strings.Compare(a.text, b.text)
	})
	libID := int64(libraryID)
	for _, k := range keys {
		d := delta[k]
		if d > 0 {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_item_field_tally(library_id, level, field, v_text, cnt)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(library_id, level, field, v_text) DO UPDATE SET
	cnt = plugin_item_field_tally.cnt + excluded.cnt`,
				libID, k.level, k.field, k.text, d); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
UPDATE plugin_item_field_tally
SET cnt = cnt + ?
WHERE library_id = ? AND level = ? AND field = ? AND v_text = ?`,
				d, libID, k.level, k.field, k.text); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
DELETE FROM plugin_item_field_tally
WHERE library_id = ? AND level = ? AND field = ? AND v_text = ? AND cnt <= 0`,
				libID, k.level, k.field, k.text); err != nil {
				return err
			}
		}
	}
	return nil
}

// bumpKindSyncStateTx records a new generation and the kind's item
// count from the per-level tally (maintained in the same transactions as
// the items), never a COUNT(*) over the kind's items (#646).
func (s *Service) bumpKindSyncStateTx(ctx context.Context, tx *sql.Tx, pluginID, kindKey string, libraryID core.LibraryID, nowUnix int64) (int64, error) {
	var totalItems int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(n), 0) FROM plugin_item_tally WHERE library_id = ?`,
		int64(libraryID)).Scan(&totalItems); err != nil {
		return 0, err
	}
	gen := int64(s.syncGen.Add(1))
	_, err := tx.ExecContext(ctx, `
INSERT INTO plugin_sync_state(plugin_id, kind_key, generation, last_started, last_ok, last_error, items)
VALUES(?, ?, ?, 0, ?, '', ?)
ON CONFLICT(plugin_id, kind_key) DO UPDATE SET
	generation = excluded.generation,
	last_ok    = excluded.last_ok,
	last_error = '',
	items      = excluded.items`,
		pluginID, kindKey, gen, nowUnix, totalItems)
	return totalItems, err
}

// KindDelete implements the store side of plugins.KindBackends.KindDelete
// (spec §3.6).
func (s *Service) KindDelete(ctx context.Context, pluginID, kindKey string, externalIDs []string) error {
	return s.DeleteItems(ctx, pluginID, kindKey, externalIDs)
}

// DeleteItems deletes the named externalIDs (and any descendant items) from
// (pluginID, kindKey), maintaining tallies, FTS, matches and resolutions.
func (s *Service) DeleteItems(ctx context.Context, pluginID, kindKey string, externalIDs []string) error {
	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()
	return s.deleteItemsLocked(ctx, pluginID, kindKey, externalIDs, true)
}

func (s *Service) deleteItemsLocked(ctx context.Context, pluginID, kindKey string, externalIDs []string, bumpGen bool) error {
	kind, err := s.GetKind(ctx, pluginID, kindKey)
	if err != nil {
		return err
	}
	if len(externalIDs) == 0 {
		return nil
	}
	refs := make([]core.ItemRef, 0, len(externalIDs))
	seen := make(map[core.ItemRef]bool, len(externalIDs))
	for _, id := range externalIDs {
		ref := core.NewItemRef(pluginID, kindKey, id)
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	leafLevel := ""
	if n := len(kind.Decl.Levels); n > 0 {
		leafLevel = kind.Decl.Levels[n-1].Key
	}
	if err := s.deleteRefsAndDescendantsTx(ctx, tx, kind.LibraryID, refs, len(kind.Decl.Levels), leafLevel); err != nil {
		return err
	}
	if bumpGen {
		if _, err := s.bumpKindSyncStateTx(ctx, tx, pluginID, kindKey, kind.LibraryID, s.now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) deleteRefsAndDescendantsTx(ctx context.Context, tx *sql.Tx, libraryID core.LibraryID, rootRefs []core.ItemRef, levelCount int, leafLevel string) error {
	if len(rootRefs) == 0 {
		return nil
	}
	type target struct {
		ref   core.ItemRef
		seq   int64
		level string
	}
	allTargets := make(map[core.ItemRef]target, len(rootRefs))
	frontier := append([]core.ItemRef(nil), rootRefs...)

	// Walk up to 5 hierarchy levels to collect all targets and their descendants.
	for depth := 0; depth < 6 && len(frontier) > 0; depth++ {
		var nextFrontier []core.ItemRef
		const chunkSize = 250
		for start := 0; start < len(frontier); start += chunkSize {
			end := min(start+chunkSize, len(frontier))
			chunk := frontier[start:end]
			placeholders := make([]string, len(chunk))
			args := make([]any, len(chunk))
			for i, r := range chunk {
				placeholders[i] = "?"
				args[i] = string(r)
			}
			inClause := strings.Join(placeholders, ",")
			if depth == 0 {
				rows, err := tx.QueryContext(ctx,
					`SELECT item_ref, seq, level FROM plugin_items WHERE item_ref IN (`+inClause+`)`, args...)
				if err != nil {
					return err
				}
				for rows.Next() {
					var refStr, lv string
					var seq int64
					if err := rows.Scan(&refStr, &seq, &lv); err != nil {
						rows.Close()
						return err
					}
					r := core.ItemRef(refStr)
					allTargets[r] = target{ref: r, seq: seq, level: lv}
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					return err
				}
				rows.Close()
			}
			// Find direct children of this chunk. A single-level kind
			// has none, and an item at the deepest level cannot be a
			// parent, so neither reaches the database. The lookup
			// itself must stay an index seek on a flat kind, where
			// every row's parent_ref is '' and planner statistics
			// otherwise talk SQLite into scanning the whole table
			// (#647): parent_ref <> '' excludes that bucket on both
			// dialects, and SQLite is pinned to the parent index.
			if levelCount == 1 {
				continue
			}
			parents := chunk
			parentArgs := args
			if leafLevel != "" {
				parents = parents[:0]
				parentArgs = parentArgs[:0]
				for _, r := range chunk {
					if t, known := allTargets[r]; known && t.level == leafLevel {
						continue
					}
					parents = append(parents, r)
					parentArgs = append(parentArgs, string(r))
				}
				if len(parents) == 0 {
					continue
				}
				placeholders = make([]string, len(parents))
				for i := range parents {
					placeholders[i] = "?"
				}
				inClause = strings.Join(placeholders, ",")
			}
			childrenQuery := `SELECT item_ref, seq, level FROM plugin_items WHERE parent_ref IN (` + inClause + `) AND parent_ref <> ''`
			if store.DialectOf(s.db) == store.SQLite {
				childrenQuery = `SELECT item_ref, seq, level FROM plugin_items INDEXED BY plugin_items_parent WHERE parent_ref IN (` + inClause + `) AND parent_ref <> ''`
			}
			crows, err := tx.QueryContext(ctx, childrenQuery, parentArgs...)
			if err != nil {
				return err
			}
			for crows.Next() {
				var refStr, lv string
				var seq int64
				if err := crows.Scan(&refStr, &seq, &lv); err != nil {
					crows.Close()
					return err
				}
				r := core.ItemRef(refStr)
				if _, already := allTargets[r]; !already {
					allTargets[r] = target{ref: r, seq: seq, level: lv}
					nextFrontier = append(nextFrontier, r)
				}
			}
			if err := crows.Err(); err != nil {
				crows.Close()
				return err
			}
			crows.Close()
		}
		frontier = nextFrontier
	}

	if len(allTargets) == 0 {
		return nil
	}

	targetRefs := make([]core.ItemRef, 0, len(allTargets))
	for r := range allTargets {
		targetRefs = append(targetRefs, r)
	}
	slices.Sort(targetRefs)

	oldFields, err := lookupExistingTextFieldsTx(ctx, tx, targetRefs)
	if err != nil {
		return err
	}
	fieldTallyDelta := map[fieldTallyKey]int64{}
	for _, of := range oldFields {
		if t, ok := allTargets[of.ref]; ok {
			addFieldTallyDelta(fieldTallyDelta, t.level, of.field, of.text, -1)
		}
	}

	isSQLite := store.DialectOf(s.db) == store.SQLite
	tallyDelta := map[string]int64{}
	for _, r := range targetRefs {
		t := allTargets[r]
		tallyDelta[t.level]--
		if isSQLite {
			if err := sqliteDeleteFTS(ctx, tx, t.seq); err != nil {
				return err
			}
		}
		refStr := string(t.ref)
		partPrefix := refStr + "#part:"
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_resolutions WHERE item_ref = ? OR SUBSTR(item_ref, 1, ?) = ?`,
			refStr, len(partPrefix), partPrefix); err != nil {
			return err
		}
		for _, tbl := range []string{
			"plugin_item_fields",
			"plugin_item_art",
			"plugin_matches",
			"plugin_items",
		} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+tbl+` WHERE item_ref = ?`, refStr); err != nil {
				return err
			}
		}
	}
	if err := applyTallyDeltaTx(ctx, tx, libraryID, tallyDelta); err != nil {
		return err
	}
	return applyFieldTallyDeltaTx(ctx, tx, libraryID, fieldTallyDelta)
}

// KindItems implements the store side of plugins.KindBackends.KindItems
// (spec §3.6): reads the plugin's own library back, keyset-paged across all
// levels by external_id.
func (s *Service) KindItems(ctx context.Context, pluginID, kindKey, cursor string, limit int) (core.KindPage, error) {
	if _, err := s.GetKind(ctx, pluginID, kindKey); err != nil {
		return core.KindPage{}, err
	}
	if limit <= 0 {
		limit = DefaultBrowseLimit
	}
	if limit > MaxBrowseLimit {
		limit = MaxBrowseLimit
	}
	items, err := s.queryItems(ctx, `
SELECT `+itemCols+`
FROM plugin_items
WHERE plugin_id = ? AND kind_key = ? AND external_id > ?
ORDER BY external_id ASC
LIMIT ?`,
		[]any{pluginID, kindKey, cursor, limit + 1}, true)
	if err != nil {
		return core.KindPage{}, err
	}
	var nextCursor string
	if len(items) > limit {
		items = items[:limit]
		nextCursor = items[len(items)-1].ID
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM plugin_items WHERE plugin_id = ? AND kind_key = ?`,
		pluginID, kindKey).Scan(&total); err != nil {
		return core.KindPage{}, err
	}
	return core.KindPage{
		Items:      items,
		NextCursor: nextCursor,
		Total:      &total,
	}, nil
}

// KindItemsByID implements the store side of plugins.KindBackends.KindItemsByID:
// fetches only the matching stored items for (pluginID, kindKey) by external_id
// via primary-key lookups on item_ref, hydrating art only for matching rows.
func (s *Service) KindItemsByID(ctx context.Context, pluginID, kindKey string, externalIDs []string) ([]core.KindItem, error) {
	if _, err := s.GetKind(ctx, pluginID, kindKey); err != nil {
		return nil, err
	}
	if len(externalIDs) == 0 {
		return nil, nil
	}
	refs := make([]string, 0, len(externalIDs))
	seen := make(map[string]bool, len(externalIDs))
	for _, id := range externalIDs {
		if !plugins.ValidExternalID(id) {
			continue
		}
		ref := string(core.NewItemRef(pluginID, kindKey, id))
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	var out []core.KindItem
	const chunkSize = 500
	for start := 0; start < len(refs); start += chunkSize {
		end := min(start+chunkSize, len(refs))
		chunk := refs[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, r := range chunk {
			placeholders[i] = "?"
			args[i] = r
		}
		items, err := s.queryItems(ctx,
			`SELECT `+itemCols+` FROM plugin_items WHERE item_ref IN (`+strings.Join(placeholders, ",")+`)`,
			args, true)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	slices.SortFunc(out, func(a, b core.KindItem) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// GetByRef returns one stored KindItem and its LibraryID by ItemRef.
func (s *Service) GetByRef(ctx context.Context, ref core.ItemRef) (core.KindItem, core.LibraryID, error) {
	return s.getOne(ctx, `SELECT `+itemCols+` FROM plugin_items WHERE item_ref = ?`, string(ref))
}

// GetByKey returns one stored KindItem and its LibraryID by its PluginKey RatingKey.
func (s *Service) GetByKey(ctx context.Context, key core.RatingKey) (core.KindItem, core.LibraryID, error) {
	seq := key.PluginSeq()
	if seq <= 0 {
		return core.KindItem{}, 0, core.ErrNotFound
	}
	return s.getOne(ctx, `SELECT `+itemCols+` FROM plugin_items WHERE seq = ?`, seq)
}

// GetByExternal returns one stored KindItem and its LibraryID by (pluginID, kindKey, externalID).
func (s *Service) GetByExternal(ctx context.Context, pluginID, kindKey, externalID string) (core.KindItem, core.LibraryID, error) {
	return s.GetByRef(ctx, core.NewItemRef(pluginID, kindKey, externalID))
}

func (s *Service) getOne(ctx context.Context, query string, arg any) (core.KindItem, core.LibraryID, error) {
	it, libID, _, err := scanItemRow(s.db.QueryRowContext(ctx, query, arg))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.KindItem{}, 0, core.ErrNotFound
		}
		return core.KindItem{}, 0, err
	}
	slice := []core.KindItem{it}
	if err := s.hydrateArtSlice(ctx, slice); err != nil {
		return core.KindItem{}, 0, err
	}
	return slice[0], libID, nil
}

// Children returns the direct children of parentRef ordered by (idx ASC, sort_title ASC, item_ref ASC).
func (s *Service) Children(ctx context.Context, parentRef core.ItemRef, cursor string, limit int) (BrowsePage, error) {
	_, libID, err := s.GetByRef(ctx, parentRef)
	if err != nil {
		return BrowsePage{}, err
	}
	return s.Browse(ctx, BrowseQuery{
		LibraryID: libID,
		ParentRef: parentRef,
		Sort:      "index",
		Cursor:    cursor,
		Limit:     limit,
	})
}

// LibraryTally returns the maintained per-level item counts for libraryID
// from plugin_item_tally (spec §2.3, §3.4).
// PluginItemCount returns the total stored item count across all libraries
// owned by pluginID, read in O(1) from plugin_item_tally.
func (s *Service) PluginItemCount(ctx context.Context, pluginID string) (int, error) {
	if s == nil || strings.TrimSpace(pluginID) == "" {
		return 0, nil
	}
	var total int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(t.n), 0)
		 FROM plugin_item_tally t
		 JOIN plugin_kinds pk ON pk.library_id = t.library_id
		 WHERE pk.plugin_id = ?`, pluginID).Scan(&total)
	if err != nil {
		return 0, err
	}
	return int(total), nil
}

func (s *Service) LibraryTally(ctx context.Context, libraryID core.LibraryID) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT level, n FROM plugin_item_tally WHERE library_id = ?`, int64(libraryID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var lv string
		var n int64
		if err := rows.Scan(&lv, &n); err != nil {
			return nil, err
		}
		out[lv] = int(n)
	}
	return out, rows.Err()
}

// SyncStateOf returns the current SyncState row for (pluginID, kindKey).
func (s *Service) SyncStateOf(ctx context.Context, pluginID, kindKey string) (SyncState, error) {
	var (
		st          SyncState
		gen         int64
		lastStarted int64
		lastOK      int64
	)
	err := s.db.QueryRowContext(ctx, `
SELECT plugin_id, kind_key, cursor, generation, last_started, last_ok, last_error, items
FROM plugin_sync_state
WHERE plugin_id = ? AND kind_key = ?`, pluginID, kindKey).Scan(
		&st.PluginID, &st.KindKey, &st.Cursor, &gen, &lastStarted, &lastOK, &st.LastError, &st.Items)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SyncState{}, core.ErrNotFound
		}
		return SyncState{}, err
	}
	st.Generation = uint64(gen)
	if lastStarted > 0 {
		st.LastStarted = time.Unix(lastStarted, 0).UTC()
	}
	if lastOK > 0 {
		st.LastOK = time.Unix(lastOK, 0).UTC()
	}
	return st, nil
}

// PurgeKind removes all items, fields, art, tallies, matches, resolutions,
// sync state and kind registration for one (pluginID, kindKey) (spec §2.5).
func (s *Service) PurgeKind(ctx context.Context, pluginID, kindKey string) error {
	s.StopKindSync(pluginID, kindKey)
	mu := s.kindLock(pluginID, kindKey)
	mu.Lock()
	defer mu.Unlock()

	var libID int64
	_ = s.db.QueryRowContext(ctx,
		`SELECT library_id FROM plugin_kinds WHERE plugin_id = ? AND kind_key = ?`,
		pluginID, kindKey).Scan(&libID)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.purgeKindTx(ctx, tx, pluginID, kindKey, core.LibraryID(libID), true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if libID > 0 {
		s.forgetLibraryStats(core.LibraryID(libID))
	}
	s.syncGen.Add(1)
	return nil
}

// PurgePlugin is the idempotent removal sequence for all kinds owned by
// pluginID (spec §2.5): stops sync goroutines, deletes watch-state rows for
// the plugin's RatingKeys, and purges items, fields, art, FTS, tallies,
// matches, resolutions, sync state and plugin_kinds rows.
func (s *Service) PurgePlugin(ctx context.Context, pluginID string) error {
	s.StopPluginSync(pluginID)

	kinds, err := s.ListKinds(ctx, pluginID)
	if err != nil {
		return err
	}
	for _, k := range kinds {
		mu := s.kindLock(pluginID, k.KindKey)
		mu.Lock()
		defer mu.Unlock()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, k := range kinds {
		if err := s.purgeKindTx(ctx, tx, pluginID, k.KindKey, k.LibraryID, true); err != nil {
			return err
		}
	}
	// Catch any orphan rows for pluginID even if plugin_kinds was already empty.
	if err := s.purgeKindTx(ctx, tx, pluginID, "", 0, true); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	for _, k := range kinds {
		if k.LibraryID > 0 {
			s.forgetLibraryStats(k.LibraryID)
		}
	}
	s.syncGen.Add(1)
	return nil
}

func (s *Service) purgeKindTx(ctx context.Context, tx *sql.Tx, pluginID, kindKey string, libraryID core.LibraryID, dropKindRow bool) error {
	whereItems := `plugin_id = ? AND kind_key = ?`
	args := []any{pluginID, kindKey}
	if kindKey == "" {
		whereItems = `plugin_id = ?`
		args = []any{pluginID}
	}

	// Delete watch-state rows for the plugin's RatingKeys by seq range when
	// state_watch is present in the schema (spec §2.5).
	var minSeq, maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(seq), MAX(seq) FROM plugin_items WHERE `+whereItems, args...).Scan(&minSeq, &maxSeq); err != nil {
		return err
	}
	if minSeq.Valid && maxSeq.Valid && minSeq.Int64 > 0 {
		minKey := int64(core.PluginKey(minSeq.Int64))
		maxKey := int64(core.PluginKey(maxSeq.Int64))
		offset := int64(core.PluginKey(1)) - 1
		if hasTableTx(ctx, tx, store.DialectOf(s.db), "state_watch") {
			delArgs := append([]any{minKey, maxKey, offset}, args...)
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM state_watch WHERE rating_key BETWEEN ? AND ? AND rating_key IN (SELECT ? + seq FROM plugin_items WHERE `+whereItems+`)`,
				delArgs...); err != nil {
				return err
			}
		}
	}

	if store.DialectOf(s.db) == store.SQLite {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_item_fts WHERE rowid IN (SELECT seq FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
	}
	for _, tbl := range []string{
		"plugin_item_fields",
		"plugin_item_art",
		"plugin_matches",
	} {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+tbl+` WHERE item_ref IN (SELECT item_ref FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM plugin_resolutions WHERE EXISTS (SELECT 1 FROM plugin_items WHERE (`+whereItems+`) AND (plugin_resolutions.item_ref = plugin_items.item_ref OR SUBSTR(plugin_resolutions.item_ref, 1, LENGTH(plugin_items.item_ref) + 6) = plugin_items.item_ref || '#part:'))`,
		args...); err != nil {
		return err
	}
	if libraryID > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_tally WHERE library_id = ?`, int64(libraryID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_field_tally WHERE library_id = ?`, int64(libraryID)); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_item_tally WHERE library_id IN (SELECT DISTINCT library_id FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_item_field_tally WHERE library_id IN (SELECT DISTINCT library_id FROM plugin_items WHERE `+whereItems+`)`,
			args...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_items WHERE `+whereItems, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_facet_expansions WHERE `+whereItems, args...); err != nil {
		return err
	}
	if dropKindRow {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_sync_state WHERE `+whereItems, args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_kinds WHERE `+whereItems, args...); err != nil {
			return err
		}
	} else if kindKey != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugin_sync_state SET cursor = '', last_started = 0, last_ok = 0, last_error = '', items = 0 WHERE plugin_id = ? AND kind_key = ?`,
			pluginID, kindKey); err != nil {
			return err
		}
	}
	return nil
}

func hasTableTx(ctx context.Context, tx *sql.Tx, d store.Dialect, table string) bool {
	var n int
	if d == store.Postgres {
		_ = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?`,
			table).Scan(&n)
		return n > 0
	}
	_ = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`,
		table).Scan(&n)
	return n > 0
}

// ResolutionCacheMaxTTL is the maximum positive cache window for a resolved
// PlayableSource (spec §4.4: min(expiresAt, now+15m)).
const (
	ResolutionCacheMaxTTL      = 15 * time.Minute
	ResolutionNegativeCacheTTL = 60 * time.Second
)

func resolutionStorageKey(ref core.ItemRef, partIdx *int) string {
	if partIdx == nil {
		return string(ref)
	}
	return fmt.Sprintf("%s#part:%d", ref, *partIdx)
}

// SaveResolution caches a successful PlayableSource for ref until
// min(src.ExpiresAt, now+15m) (spec §4.4).
func (s *Service) SaveResolution(ctx context.Context, ref core.ItemRef, src core.PlayableSource) error {
	return s.SavePartResolution(ctx, ref, nil, src)
}

// SavePartResolution caches a successful PlayableSource for ref and optional
// zero-based partIdx until min(src.ExpiresAt, now+15m) (spec §4.4, #576).
func (s *Service) SavePartResolution(ctx context.Context, ref core.ItemRef, partIdx *int, src core.PlayableSource) error {
	now := s.now()
	exp := now.Add(ResolutionCacheMaxTTL).Unix()
	if src.ExpiresAt > 0 && src.ExpiresAt < exp {
		exp = src.ExpiresAt
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO plugin_resolutions(item_ref, pattern, source, negative, expires_at)
VALUES(?, ?, ?, 0, ?)
ON CONFLICT(item_ref) DO UPDATE SET
	pattern    = excluded.pattern,
	source     = excluded.source,
	negative   = 0,
	expires_at = excluded.expires_at`,
		resolutionStorageKey(ref, partIdx), src.Pattern, string(raw), exp)
	return err
}

// SaveNegativeResolution caches a not_available resolution result for 60s so
// a dead kind cannot stampede the plugin (spec §4.4).
func (s *Service) SaveNegativeResolution(ctx context.Context, ref core.ItemRef) error {
	return s.SaveNegativePartResolution(ctx, ref, nil)
}

// SaveNegativePartResolution caches a not_available resolution result for ref
// and optional partIdx for 60s (spec §4.4, #576).
func (s *Service) SaveNegativePartResolution(ctx context.Context, ref core.ItemRef, partIdx *int) error {
	exp := s.now().Add(ResolutionNegativeCacheTTL).Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO plugin_resolutions(item_ref, pattern, source, negative, expires_at)
VALUES(?, '', '{}', 1, ?)
ON CONFLICT(item_ref) DO UPDATE SET
	pattern    = '',
	source     = '{}',
	negative   = 1,
	expires_at = excluded.expires_at`,
		resolutionStorageKey(ref, partIdx), exp)
	return err
}

// GetResolution looks up a non-expired cached resolution for ref (spec §4.4).
// If a negative cache entry is active, it returns (zero, true, core.ErrNotAvailable).
func (s *Service) GetResolution(ctx context.Context, ref core.ItemRef) (core.PlayableSource, bool, error) {
	return s.GetPartResolution(ctx, ref, nil)
}

// GetPartResolution looks up a non-expired cached resolution for ref and
// optional partIdx (spec §4.4, #576).
func (s *Service) GetPartResolution(ctx context.Context, ref core.ItemRef, partIdx *int) (core.PlayableSource, bool, error) {
	nowUnix := s.now().Unix()
	var (
		sourceJSON string
		negative   int
		expiresAt  int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT source, negative, expires_at FROM plugin_resolutions WHERE item_ref = ?`,
		resolutionStorageKey(ref, partIdx)).Scan(&sourceJSON, &negative, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.PlayableSource{}, false, nil
		}
		return core.PlayableSource{}, false, err
	}
	if expiresAt <= nowUnix {
		return core.PlayableSource{}, false, nil
	}
	if negative != 0 {
		return core.PlayableSource{}, true, core.ErrNotAvailable
	}
	var src core.PlayableSource
	if err := json.Unmarshal([]byte(sourceJSON), &src); err != nil {
		return core.PlayableSource{}, false, err
	}
	return src, true, nil
}

// InvalidateResolution deletes cached whole-item and per-part resolutions for
// ref, e.g. on a mid-stream upstream failure (spec §4.4, #576).
func (s *Service) InvalidateResolution(ctx context.Context, ref core.ItemRef) error {
	refStr := string(ref)
	partPrefix := refStr + "#part:"
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM plugin_resolutions WHERE item_ref = ? OR SUBSTR(item_ref, 1, ?) = ?`,
		refStr, len(partPrefix), partPrefix)
	return err
}

// InvalidatePartResolution deletes a single part's cached resolution when
// partIdx is non-nil, or invalidates the whole item (and its parts) when nil.
func (s *Service) InvalidatePartResolution(ctx context.Context, ref core.ItemRef, partIdx *int) error {
	if partIdx == nil {
		return s.InvalidateResolution(ctx, ref)
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM plugin_resolutions WHERE item_ref = ?`,
		resolutionStorageKey(ref, partIdx))
	return err
}
