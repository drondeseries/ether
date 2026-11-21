package resolver

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	maxSearchRefills   = 2
	searchRefillBudget = 45 * time.Second
	refillRetryDelay   = 15 * time.Second // after a budget refusal only
)

var (
	_ core.SearchCacheRefiller       = (*Service)(nil)
	_ core.WarmupSearchCacheRefiller = (*Service)(nil)
)

type refillSearchContextKey struct{}

// refillMark records a finished detail discovery: no other discovery for
// the title until until; missed reports that it left no search answer.
type refillMark struct {
	until  time.Time
	missed bool
}

// RefillSearchCache recovers candidates for an open detail page. It never
// waits for capacity or performs resolution, descriptor fetching, health
// checks or provider work.
//
// Nobody pressed Play: discovery is speculative indexer work. It draws on
// the viewer detail share, asks each indexer its primary query only (no
// fallback burst, no rescue), skips a title remembered unavailable, and
// runs at most once per title per unavailable cooloff, whatever it found:
// a version picker reopened all evening must not search all evening.
// Errors are best-effort: no retries or queue.
func (s *Service) RefillSearchCache(title *core.Title) <-chan struct{} {
	return s.refillSearchCache(title, false)
}

// RefillWarmupSearchCache recovers picker candidates for a warm-up reusing
// a remembered source. No viewer is waiting on this work, so it uses the
// unattended share, even when the warm-up originated from a detail page.
func (s *Service) RefillWarmupSearchCache(title *core.Title) <-chan struct{} {
	return s.refillSearchCache(title, true)
}

func (s *Service) refillSearchCache(title *core.Title, unattended bool) <-chan struct{} {
	if title == nil {
		return nil
	}
	req := searchRequestFor(title)
	if !unattended && req.Anime {
		if e, ok := s.getAvailability(s.bgCtx, int64(title.Key)); !ok || e.available {
			s.warmSeaDexForOpen(title, req)
		}
	}
	key := searchCacheKey(req)
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed {
		return nil
	}
	if done := s.refills[key]; done != nil {
		return done
	}
	if mark := s.refillRetryAfter[key]; s.now().Before(mark.until) {
		return nil
	}
	if _, ok := s.peekSearchCache(key); ok || len(s.refills) >= maxSearchRefills {
		return nil
	}
	if e, ok := s.getAvailability(s.bgCtx, int64(title.Key)); ok && !e.available {
		// Remembered unavailable: the cooloff stands for discovery too.
		return nil
	}
	if s.refills == nil {
		s.refills = make(map[string]chan struct{})
	}
	done := make(chan struct{})
	s.refills[key] = done
	cooloff := s.cooloffFor(title)
	// Use the service lifetime, not the metadata request or source warm-up.
	searchCtx := core.WithIndexerPurpose(context.WithValue(s.bgCtx, refillSearchContextKey{}, true), core.IndexerPurposeDetail)
	if unattended {
		searchCtx = core.WithUnattendedSearch(core.WithIndexerPurpose(searchCtx, core.IndexerPurposeWarmup))
	}
	ctx, cancel := context.WithTimeout(searchCtx, searchRefillBudget)
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		var err error
		defer func() {
			cancel()
			s.bgMu.Lock()
			if s.refillRetryAfter == nil {
				s.refillRetryAfter = make(map[string]refillMark)
			}
			boundMap(s.refillRetryAfter, maxSearchCache, func(m refillMark) bool { return !s.now().Before(m.until) })
			next := cooloff
			if errors.Is(err, core.ErrBudgetExhausted) {
				// Refused before any request: the share may allow it later.
				next = refillRetryDelay
			}
			found, _ := s.peekSearchCache(key)
			s.refillRetryAfter[key] = refillMark{until: s.now().Add(next), missed: len(found) == 0}
			delete(s.refills, key)
			close(done)
			s.bgMu.Unlock()
		}()
		// Recheck the cache: a foreground search may have filled it since
		// admission. No admin-console discovery/observation side effects.
		var results []core.Release
		results, _, err = s.searchCached(ctx, req, false)
		if !unattended && err == nil && len(results) > 0 {
			s.warmReleaseTitlesForSearch(title, req, results)
		}
	}()
	return done
}
