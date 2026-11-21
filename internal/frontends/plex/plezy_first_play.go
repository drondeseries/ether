package plex

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Plezy (a Flutter/mpv Plex client) builds its track selection from the
// /library/metadata row it already holds: getVideoPlaybackData reuses a
// fresh (5 minute) cached row whenever every Part carries Stream, exists
// and accessible, and its subtitle selection trusts only the server's
// selected stream. A cold episode's detail row is estimated — invented
// audio, no subtitles — so first play started with subtitles off, and the
// real tracks the playback probe found seconds later were never re-read
// (#618). Two changes, both scoped to Plezy:
//
//   - A pre-probe (estimated) Part does not claim completeness: file
//     availability leaves exists/accessible unset for it, exactly like
//     PMS without a file check, so Plezy rejects its cached row at Play
//     and re-fetches. Missing flags still read as playable in Plezy
//     (isPlayable is accessible != false && exists != false).
//   - That re-fetch is play intent. The detail-screen fetch and the
//     play-time fetch are identical on the wire (same endpoint, same
//     includeChapters/includeMarkers/checkFiles/includeStreams), so the
//     only reliable signal is state: this client was already served an
//     incomplete row for this title and is asking again. Then the
//     request waits, bounded, on the shared resolve+probe flight — the
//     same warm entry the following file request opens — and answers
//     with the real streams and the viewer's rule-selected subtitle.
//
// The first (browsing) fetch never waits and starts nothing beyond the
// detail warm-up scheduleDetailWarm already runs for every client.

const (
	// plezyPlayProbeWait bounds the play-time re-fetch's wait for the
	// probe, mirroring the Bedrock detail allowance. A slower probe
	// keeps running; the file request joins the same flight.
	plezyPlayProbeWait = 4 * time.Second
	// plezyReaskTTL is how long a served incomplete row arms the
	// play-intent re-fetch. It comfortably covers Plezy's 5-minute
	// fresh-cache window.
	plezyReaskTTL     = 15 * time.Minute
	plezyReaskMaxRows = 512
)

// plezyClient identifies Plezy by its X-Plex-Product. The change is
// scoped to it: other clients re-read metadata around playback, or are
// already served a probed detail page (Bedrock), and keep today's
// wire shape untouched.
func plezyClient(pr *Request) bool {
	return pr != nil && strings.EqualFold(strings.TrimSpace(pr.Product), "Plezy")
}

type plezyReaskKey struct {
	userID   int64
	clientID string
	key      frontend.RatingKey
}

// plezyReaskStore remembers which Plezy clients were served an
// incomplete (pre-probe) detail row, so their next identical fetch can
// be recognised as the play-time re-ask.
type plezyReaskStore struct {
	mu   sync.Mutex
	rows map[plezyReaskKey]time.Time
}

func (st *plezyReaskStore) record(k plezyReaskKey, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.rows == nil {
		st.rows = map[plezyReaskKey]time.Time{}
	}
	if len(st.rows) >= plezyReaskMaxRows {
		for row, at := range st.rows {
			if now.Sub(at) > plezyReaskTTL {
				delete(st.rows, row)
			}
		}
		if len(st.rows) >= plezyReaskMaxRows {
			return // full of fresh rows: skip arming rather than grow
		}
	}
	st.rows[k] = now
}

// armed reports whether k was served an incomplete row recently. The
// row stays armed until it expires or forget is called, so a re-ask
// whose probe misses the wait budget still arms the next retry.
func (st *plezyReaskStore) armed(k plezyReaskKey, now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	at, ok := st.rows[k]
	if !ok {
		return false
	}
	if now.Sub(at) > plezyReaskTTL {
		delete(st.rows, k)
		return false
	}
	return true
}

func (st *plezyReaskStore) forget(k plezyReaskKey) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.rows, k)
}

// plezyFirstPlay runs on a single-item metadata read for a playable
// title. Nothing happens unless the client is Plezy asking with file
// checks (its detail and playback fetches both do) for a title with no
// exact tracks yet and a previously served incomplete row: that
// combination is the play tap, so wait bounded on the shared probe.
func (s *Server) plezyFirstPlay(ctx context.Context, pr *Request, t *frontend.Title, q url.Values) {
	if pr == nil || pr.User == nil || t == nil || !plezyClient(pr) || libraryToolClient(pr) {
		return
	}
	if !metadataOptionOn(q.Get("checkFiles")) && !metadataOptionOn(q.Get("asyncCheckFiles")) && !metadataOptionOn(q.Get("checkFileAvailability")) {
		return
	}
	k := plezyReaskKey{userID: pr.User.ID, clientID: pr.ClientID, key: t.Key}
	if _, _, exact := s.mediaInfoWithSource(ctx, pr, t); exact {
		s.plezyReasks.forget(k)
		return
	}
	if s.plezyReasks.armed(k, s.now()) {
		// Join (or start) the one resolve+probe flight for this
		// title and wait for it. probeDetailTracks bounds the wait
		// by detailWaitUntil and is a no-op once facts are exact;
		// ProbeMetadata schedules the same warm entry the file
		// request's Prepare adopts, so there is no double work.
		pr.detailWaitUntil = s.now().Add(plezyPlayProbeWait)
		_ = s.probeDetailTracks(ctx, pr, t)
		if _, _, exact := s.mediaInfoWithSource(ctx, pr, t); exact {
			s.plezyReasks.forget(k)
			return
		}
	}
	// Still estimated: this response goes out incomplete (see
	// projectFileAvailability), so arm the re-ask Plezy will make at
	// Play instead of reusing this row. Browsing starts nothing and
	// never waits; the first fetch only records.
	s.plezyReasks.record(k, s.now())
}

// plezyPartProbed reports whether a rendered Part's streams come from a
// real probe rather than the pre-probe estimate. Stream bindings carry
// the estimate flag of the info they were rendered from; when no
// binding resolves (a render without issued streams), the title's
// session/resumed/warm probe decides.
func (s *Server) plezyPartProbed(ctx context.Context, pr *Request, part *model.Part) bool {
	resolved := false
	for _, st := range part.Stream {
		if st.ID == 0 {
			continue
		}
		binding, err := s.resolveIssuedStream(ctx, pr.User, pr.ClientID, st.ID)
		if err != nil {
			continue
		}
		if binding.Estimated {
			return false
		}
		resolved = true
	}
	if resolved {
		return true
	}
	key, _, ok := s.partTitle(part.ID)
	if !ok || pr.User == nil || s.playback() == nil {
		return false
	}
	if ps, ok := s.playback().SessionFor(pr.User.ID, pr.ClientID, key); ok && ps != nil && !ps.Info.Estimated && len(ps.Info.Streams) > 0 {
		return true
	}
	caps := s.clientCapsForKey(pr, key)
	if src, ok := s.playback().(resumedTrackSource); ok {
		if info, releaseID, ok := src.ResumedTracks(ctx, pr.User, key, caps); ok && releaseID != "" && !info.Estimated && len(info.Streams) > 0 {
			return true
		}
	}
	if wp, ok := s.playback().(frontend.WarmProber); ok {
		if _, warm := wp.WarmProbeInfo(pr.User, caps, key); warm {
			return true
		}
	}
	return false
}
