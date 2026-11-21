package playback

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// namesakeRuntimeTolerance is the allowed difference between a probed file
// and the catalogue runtime: the larger of these two bounds.
const (
	namesakeRuntimeSlack      = 8 * time.Minute
	namesakeRuntimeSlackRatio = 0.12
)

// NamesakeCatalog, implemented by the catalogue, reports whether another
// catalogue title namesakes key. The answer is an on-demand indexed lookup;
// playback caches it per title for the life of the manager so a title's
// reselection does not repeat it.
type NamesakeCatalog interface {
	Namesakes(ctx context.Context, key core.RatingKey) (bool, error)
}

// namesakeRequest reports whether t is one of two namesake catalogue titles.
// Plugin kind projections (KindLevel set) are not catalogue titles and are
// never gated.
func (m *Manager) namesakeRequest(ctx context.Context, t *core.Title) bool {
	if t == nil || t.KindLevel != "" || t.Key == 0 {
		return false
	}
	cat, ok := m.deps.Catalog.(NamesakeCatalog)
	if !ok {
		return false
	}
	if ans, ok := m.namesakeLookup(t.Key); ok {
		return ans
	}
	ans, err := cat.Namesakes(ctx, t.Key)
	if err != nil {
		m.log.Debug("namesake lookup failed", "key", t.Key, "err", err)
		return false
	}
	m.rememberNamesake(t.Key, ans)
	return ans
}

// namesakeLookup returns a cached answer for key, if the manager holds one.
func (m *Manager) namesakeLookup(key core.RatingKey) (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ans, ok := m.namesake[key]
	return ans, ok
}

// rememberNamesake caches a namesake answer for key.
func (m *Manager) rememberNamesake(key core.RatingKey, ans bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.namesake == nil {
		m.namesake = map[core.RatingKey]bool{}
	}
	m.namesake[key] = ans
}

// namesakeTitleConflict reports whether r's reported identifiers name a
// different catalogue title than t. Only explicit identifier conflicts count;
// missing identifiers carry no evidence.
func namesakeTitleConflict(t *core.Title, r core.Release) bool {
	if t == nil || t.IDs == (core.IDs{}) || r.TitleIDs == (core.IDs{}) {
		return false
	}
	_, conflict := releaseparse.CompareTitleIDs(r.TitleIDs, t.IDs)
	return conflict
}

// namesakeDurationMismatch reports whether a probed duration contradicts the
// catalogue runtime by more than the tolerance.
func namesakeDurationMismatch(runtime, probed time.Duration) bool {
	if runtime <= 0 || probed <= 0 {
		return false
	}
	allow := math.Max(namesakeRuntimeSlack.Seconds(), namesakeRuntimeSlackRatio*runtime.Seconds())
	return math.Abs(probed.Seconds()-runtime.Seconds()) > allow
}

// releaseNamesakeRejected gates a candidate for an ambiguous (namesake)
// request: a release whose reported identifiers name the other title is
// rejected before opening. Unambiguous titles are never gated.
func (m *Manager) releaseNamesakeRejected(ctx context.Context, s *session, r core.Release) bool {
	if s.title == nil || !m.namesakeRequest(ctx, s.title) {
		return false
	}
	return namesakeTitleConflict(s.title, r)
}

// releaseRuntimeMismatch checks actual container facts independently of catalogue
// namesake coverage. Only explicit multi-episode filenames scale the runtime;
// a season pack's selected file is normally a single episode.
func releaseRuntimeMismatch(t *core.Title, r core.Release, probed time.Duration) bool {
	if t == nil || t.KindLevel != "" || t.Runtime <= 0 || probed <= 0 {
		return false
	}
	expected := t.Runtime
	if t.Type == core.TypeEpisode {
		info := releaseparse.Parse(r.Name)
		if len(info.Episodes) > 1 {
			expected *= time.Duration(len(info.Episodes))
		}
	}
	slack := math.Max((8 * time.Minute).Seconds(), 0.35*expected.Seconds())
	return math.Abs(probed.Seconds()-expected.Seconds()) > slack
}

// segmentStart is where the requested episode begins inside a combined
// multi-episode file: for the second episode of an "S01E01-E02" file it is
// one segment in, so a server-started stream starts at that episode rather
// than replaying the first. The segment length is the probed file duration
// divided by the episodes the file names (broadcast segments run equal),
// falling back to the catalogue runtime before a probe. It is zero when
// the file holds only the requested episode, when the requested episode is
// the first it names, or when it does not name the requested episode at
// all — acceptance of such a release is the resolver's decision, never
// widened here. A season pack's selected file is normally a single
// episode, so packs never offset.
func segmentStart(t *core.Title, r core.Release, probed time.Duration) time.Duration {
	if t == nil || t.KindLevel != "" || t.Type != core.TypeEpisode || r.Info.FullSeason {
		return 0
	}
	episodes := r.Info.Episodes
	if len(episodes) < 2 {
		episodes = releaseparse.Parse(r.Name).Episodes
	}
	if len(episodes) < 2 {
		return 0
	}
	want := t.Episode
	if r.Target != nil && r.Target.Episode > 0 {
		want = r.Target.Episode
	}
	idx := slices.Index(episodes, want)
	if idx <= 0 {
		return 0
	}
	segment := t.Runtime
	if probed > 0 {
		segment = probed / time.Duration(len(episodes))
	}
	if segment <= 0 {
		return 0
	}
	start := time.Duration(idx) * segment
	if probed > 0 && start >= probed {
		return 0
	}
	return start
}
