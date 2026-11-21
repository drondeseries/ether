package state

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

const continueSeriesIndex = `CREATE INDEX state_history_continue_series ON state_history(user_id,completed,rating_key,viewed_at DESC,id DESC);`

func init() {
	store.Register("state", 11, continueSeriesIndex)
}

// Rank only this viewer's completed episodes using the covering viewer-first
// index. Limit before loading snapshots, then fetch each selected row by its
// primary key rather than scanning history a second time. Both databases use
// integer division for series identity and support these window/CTE clauses.
const continueSeriesQuery = `WITH ranked AS (
 SELECT id, viewed_at, ROW_NUMBER() OVER (
 PARTITION BY rating_key/10000000 ORDER BY viewed_at DESC,id DESC) AS position
 FROM state_history WHERE user_id=? AND completed=1 AND rating_key>=? AND rating_key<?
 ), selected AS (
 SELECT id FROM ranked WHERE position=1 ORDER BY viewed_at DESC,id DESC LIMIT ?
 ) SELECT h.id,h.user_id,h.rating_key,h.viewed_at,h.completed,h.view_type,h.device_id,h.snapshot
 FROM selected JOIN state_history h ON h.id=selected.id
 ORDER BY h.viewed_at DESC,h.id DESC`

// ContinueSeriesHistory returns the latest completed episode per series,
// newest first, without an age cutoff. Reduce before limiting so repeated
// episodes and movies cannot evict another series from the candidate list.
func (s *State) ContinueSeriesHistory(ctx context.Context, uid int64, limit int) ([]core.HistoryEntry, error) {
	if uid <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, continueSeriesQuery, uid,
		int64(core.EpisodeKey(0, 0, 0)), int64(core.EpisodeKey(0, 0, 0))+1000000000000000, min(max(limit, 1), 200))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.HistoryEntry
	for rows.Next() {
		h, err := scanHistory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
