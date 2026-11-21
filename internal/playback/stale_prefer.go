package playback

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// A Plex client keeps the variant Part id of the version it was shown and
// plays it on every open, long after the release behind it failed its
// health check (the resolver then rejects it for a cooldown, so it is "not
// among candidates"). Each open used to ask the resolver for that release
// again, fall back to a fresh normal ranking that skips the warm entry, and
// so play a different release from the one the opening prefetch warmed.
//
// When a non-exact pick falls back, the Manager forgets it for
// stalePreferTTL: later opens of that version are the automatic choice
// (which the warm-up and resume already hold). After the TTL one open asks
// for the picked release again. A refreshed search with a matching candidate
// clears the cooldown on the next open, without waiting for that TTL.
const (
	stalePreferTTL  = 10 * time.Minute
	maxStalePrefers = 1024
)

type stalePreference struct {
	at     time.Time
	title  *core.Title
	played string
	reason string
}

type stalePreferKey struct {
	key     core.RatingKey
	release string
}

// preferenceStale reports a picked release that recently fell back. The
// caller holds m.mu.
func (m *Manager) preferenceStaleLocked(key core.RatingKey, release string, now time.Time) bool {
	entry, ok := m.stalePrefers[stalePreferKey{key, release}]
	if !ok {
		return false
	}
	if now.Sub(entry.at) >= stalePreferTTL {
		delete(m.stalePrefers, stalePreferKey{key, release})
		return false
	}
	if observer, ok := m.deps.Resolver.(core.PreferenceFallbackObserver); ok && observer.PreferredVersionRecovered(entry.title, release, entry.played, entry.at) {
		delete(m.stalePrefers, stalePreferKey{key, release})
		m.log.Info("picked version found in refreshed search; retrying the pick", "key", key, "title", entry.title.Title, "prefer", release)
		return false
	}
	return true
}

func (m *Manager) notePreferenceStale(ctx context.Context, s *session, played, reason string) {
	key, release := s.ident.key, s.prefer
	now := m.now()
	m.mu.Lock()
	if m.stalePrefers == nil {
		m.stalePrefers = map[stalePreferKey]stalePreference{}
	}
	if len(m.stalePrefers) >= maxStalePrefers {
		var oldest stalePreferKey
		var oldestAt time.Time
		for k, entry := range m.stalePrefers {
			at := entry.at
			if now.Sub(at) >= stalePreferTTL {
				delete(m.stalePrefers, k)
			} else if oldestAt.IsZero() || at.Before(oldestAt) {
				oldest, oldestAt = k, at
			}
		}
		if len(m.stalePrefers) >= maxStalePrefers {
			delete(m.stalePrefers, oldest)
		}
	}
	entry := stalePreference{at: now, title: s.title, played: played, reason: reason}
	m.stalePrefers[stalePreferKey{key, release}] = entry
	m.mu.Unlock()
	m.recordPreferenceFallback(ctx, key, release, entry, stalePreferTTL, false)
}

// forgetStalePrefersLocked drops a title's stale picks (InvalidateTitle:
// the owner pinned, blocked or re-searched it). The caller holds m.mu.
func (m *Manager) forgetStalePrefersLocked(key core.RatingKey) {
	for k := range m.stalePrefers {
		if titleIncludes(key, k.key) {
			delete(m.stalePrefers, k)
		}
	}
}

func (m *Manager) recordPreferenceFallback(ctx context.Context, key core.RatingKey, prefer string, entry stalePreference, retry time.Duration, latched bool) {
	title := ""
	if entry.title != nil {
		title = entry.title.Title
	}
	m.log.Info("picked version unavailable; playing the automatic choice until it recovers", "key", key, "title", title, "prefer", prefer, "release", entry.played, "reason", entry.reason, "retry_after", retry, "cooldown_active", latched)
	if observer, ok := m.deps.Resolver.(core.PreferenceFallbackObserver); ok {
		observer.RecordPreferenceFallback(ctx, core.ResolveRequest{Title: entry.title, Prefer: prefer}, entry.played, entry.reason, retry)
	}
}
