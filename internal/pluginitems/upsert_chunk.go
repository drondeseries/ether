package pluginitems

import (
	"context"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// upsertChunkSize bounds one write transaction inside an upsert batch
// (#646). The per-kind lock is held across chunks, so a batch still looks
// atomic to the kind's own writers, but the database write gate is
// released between chunks and unrelated writers interleave instead of
// timing out behind a minutes-long republish.
const upsertChunkSize = 200

type upsertChunkResult struct {
	added      int
	updated    int
	unchanged  int
	totalItems int64
}

// writeUpsertChunk writes one chunk of prepared items in a single
// transaction. Existing items whose content hash matches are not
// rewritten — no plugin_items index churn, no FTS replace, no field or
// art delete/reinsert — only their seen bookkeeping is refreshed, in one
// statement, and only when it would change.
func (s *Service) writeUpsertChunk(ctx context.Context, kind KindRecord, pluginID, kindKey string, chunk []preparedItem, seenAt, nowUnix int64, bumpGen bool) (upsertChunkResult, error) {
	var res upsertChunkResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	existing, err := lookupExistingItemsTx(ctx, tx, chunk)
	if err != nil {
		return res, err
	}

	unchangedRefs := make([]core.ItemRef, 0, len(chunk))
	changed := make([]bool, len(chunk))
	for i := range chunk {
		ref := chunk[i].validated.Item.Ref
		ex, ok := existing[ref]
		switch {
		case !ok:
			changed[i] = true
			res.added++
		case ex.hash != "" && ex.hash == chunk[i].hash:
			unchangedRefs = append(unchangedRefs, ref)
			res.unchanged++
		default:
			changed[i] = true
			res.updated++
		}
	}

	if res.added > 0 {
		maxItems := s.MaxItemsFor(pluginID)
		// The tally is maintained in the same transactions as the items,
		// so summing its per-level rows (bounded by kinds x levels)
		// replaces a COUNT(*) over the plugin's whole item table.
		var pluginTotal int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(n), 0) FROM plugin_item_tally WHERE library_id IN (SELECT library_id FROM plugin_kinds WHERE plugin_id = ?)`,
			pluginID).Scan(&pluginTotal); err != nil {
			return res, err
		}
		if pluginTotal+int64(res.added) > int64(maxItems) {
			s.noteAttention(ctx, pluginID, fmt.Sprintf("plugin exceeded max item cap (%d)", maxItems))
			return res, ErrMaxItemsExceeded
		}
	}

	var nextSeq int64
	if res.added > 0 {
		nextSeq, err = allocateSeqRangeTx(ctx, tx, int64(res.added))
		if err != nil {
			return res, err
		}
	}

	if len(unchangedRefs) > 0 {
		placeholders := make([]string, len(unchangedRefs))
		args := make([]any, 0, len(unchangedRefs)+2)
		args = append(args, seenAt)
		for i, r := range unchangedRefs {
			placeholders[i] = "?"
			args = append(args, string(r))
		}
		args = append(args, seenAt)
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugin_items SET seen_at = ?, missed_walks = 0 WHERE item_ref IN (`+strings.Join(placeholders, ",")+`) AND (missed_walks <> 0 OR seen_at <> ?)`,
			args...); err != nil {
			return res, err
		}
	}

	fieldTallyDelta := map[fieldTallyKey]int64{}
	changedExisting := make([]core.ItemRef, 0, len(chunk))
	for i := range chunk {
		if !changed[i] {
			continue
		}
		if _, ok := existing[chunk[i].validated.Item.Ref]; ok {
			changedExisting = append(changedExisting, chunk[i].validated.Item.Ref)
		}
	}
	if len(changedExisting) > 0 {
		oldFields, err := lookupExistingTextFieldsTx(ctx, tx, changedExisting)
		if err != nil {
			return res, err
		}
		for _, of := range oldFields {
			if ex, ok := existing[of.ref]; ok {
				addFieldTallyDelta(fieldTallyDelta, ex.level, of.field, of.text, -1)
			}
		}
	}

	isPG := store.DialectOf(s.db) == store.Postgres
	tallyDelta := map[string]int64{}

	for i := range chunk {
		if !changed[i] {
			continue
		}
		p := &chunk[i]
		v := &p.validated
		it := &v.Item
		ex, isExisting := existing[it.Ref]
		var seq int64
		var addedAt int64
		if isExisting {
			seq = ex.seq
			addedAt = ex.addedAt
			if ex.level != it.Level {
				tallyDelta[ex.level]--
				tallyDelta[it.Level]++
			}
		} else {
			seq = nextSeq
			nextSeq++
			addedAt = nowUnix
			tallyDelta[it.Level]++
		}
		it.Key = core.PluginKey(seq)

		if isPG {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_items(
	item_ref, seq, plugin_id, kind_key, library_id, external_id, parent_ref, level, idx,
	title, sort_title, summary, year, released, rating, rating_level, playable, child_count, leaf_count,
	unavailable, hints, fields, etag, added_at, updated_at, seen_at, missed_walks, content_hash, search_tsv
) VALUES(
	?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, 0, ?,
	setweight(to_tsvector('simple', ?), 'A') || setweight(to_tsvector('simple', ?), 'B') || setweight(to_tsvector('simple', ?), 'C')
) ON CONFLICT(item_ref) DO UPDATE SET
	parent_ref   = excluded.parent_ref,
	level        = excluded.level,
	idx          = excluded.idx,
	title        = excluded.title,
	sort_title   = excluded.sort_title,
	summary      = excluded.summary,
	year         = excluded.year,
	released     = excluded.released,
	rating       = excluded.rating,
	rating_level = excluded.rating_level,
	playable     = excluded.playable,
	child_count  = excluded.child_count,
	leaf_count   = excluded.leaf_count,
	unavailable  = excluded.unavailable,
	hints        = excluded.hints,
	fields       = excluded.fields,
	etag         = excluded.etag,
	updated_at   = excluded.updated_at,
	seen_at      = excluded.seen_at,
	missed_walks = 0,
	content_hash = excluded.content_hash,
	search_tsv   = excluded.search_tsv`,
				string(it.Ref), seq, pluginID, kindKey, int64(kind.LibraryID), it.ID, p.parentRef, it.Level, it.Index,
				it.Title, it.SortTitle, it.Summary, it.Year, it.Released, it.Rating, contentRatingLevel(it.Fields), p.playableInt, it.ChildCount, it.LeafCount,
				p.unavailableStr, p.hintsStr, p.fieldsStr, it.ETag, addedAt, nowUnix, seenAt, p.hash,
				it.Title, v.SearchFields, v.SearchPeople,
			); err != nil {
				return res, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_items(
	item_ref, seq, plugin_id, kind_key, library_id, external_id, parent_ref, level, idx,
	title, sort_title, summary, year, released, rating, rating_level, playable, child_count, leaf_count,
	unavailable, hints, fields, etag, added_at, updated_at, seen_at, missed_walks, content_hash
) VALUES(
	?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, 0, ?
) ON CONFLICT(item_ref) DO UPDATE SET
	parent_ref   = excluded.parent_ref,
	level        = excluded.level,
	idx          = excluded.idx,
	title        = excluded.title,
	sort_title   = excluded.sort_title,
	summary      = excluded.summary,
	year         = excluded.year,
	released     = excluded.released,
	rating       = excluded.rating,
	rating_level = excluded.rating_level,
	playable     = excluded.playable,
	child_count  = excluded.child_count,
	leaf_count   = excluded.leaf_count,
	unavailable  = excluded.unavailable,
	hints        = excluded.hints,
	fields       = excluded.fields,
	etag         = excluded.etag,
	updated_at   = excluded.updated_at,
	seen_at      = excluded.seen_at,
	missed_walks = 0,
	content_hash = excluded.content_hash`,
				string(it.Ref), seq, pluginID, kindKey, int64(kind.LibraryID), it.ID, p.parentRef, it.Level, it.Index,
				it.Title, it.SortTitle, it.Summary, it.Year, it.Released, it.Rating, contentRatingLevel(it.Fields), p.playableInt, it.ChildCount, it.LeafCount,
				p.unavailableStr, p.hintsStr, p.fieldsStr, it.ETag, addedAt, nowUnix, seenAt, p.hash,
			); err != nil {
				return res, err
			}
			if err := sqliteIndexFTS(ctx, tx, seq, it.Ref, kind.LibraryID, it.Title, v.SearchFields, v.SearchPeople); err != nil {
				return res, err
			}
		}

		// Replace indexed fields for this item.
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_fields WHERE item_ref = ?`, string(it.Ref)); err != nil {
			return res, err
		}
		for _, fv := range v.IndexedFields {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO plugin_item_fields(item_ref, field, v_text, v_int, v_real, v_fold) VALUES(?, ?, ?, ?, ?, ?)
				 ON CONFLICT(item_ref, field, v_text, v_int) DO UPDATE SET v_real = excluded.v_real, v_fold = excluded.v_fold`,
				string(it.Ref), fv.Field, fv.Text, fv.Int, fv.Real, fv.Fold); err != nil {
				return res, err
			}
			if fv.Text != "" {
				addFieldTallyDelta(fieldTallyDelta, it.Level, fv.Field, fv.Text, 1)
			}
		}

		// Replace artwork rows for this item.
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_item_art WHERE item_ref = ?`, string(it.Ref)); err != nil {
			return res, err
		}
		for _, a := range it.Art {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO plugin_item_art(item_ref, role, url, width, height) VALUES(?, ?, ?, ?, ?)`,
				string(it.Ref), a.Role, a.URL, a.Width, a.Height); err != nil {
				return res, err
			}
		}
	}

	if err := applyTallyDeltaTx(ctx, tx, kind.LibraryID, tallyDelta); err != nil {
		return res, err
	}
	if err := applyFieldTallyDeltaTx(ctx, tx, kind.LibraryID, fieldTallyDelta); err != nil {
		return res, err
	}
	if bumpGen {
		total, err := s.bumpKindSyncStateTx(ctx, tx, pluginID, kindKey, kind.LibraryID, nowUnix)
		if err != nil {
			return res, err
		}
		res.totalItems = total
	}
	if err := tx.Commit(); err != nil {
		return upsertChunkResult{}, err
	}
	return res, nil
}
