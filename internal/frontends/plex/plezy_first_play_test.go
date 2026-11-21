package plex

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// plezyColdPlayback estimates without subtitle tracks until the
// metadata probe runs, then serves the probed facts as a warm probe —
// the cold-episode shape of #618. Nothing probes unless ProbeMetadata
// is called, so any probed track in a response was waited for.
type plezyColdPlayback struct {
	*fakePlayback
	probed core.MediaInfo
	ready  atomic.Bool
}

func (c *plezyColdPlayback) Estimate(context.Context, *core.Title, core.ClientCaps) core.MediaInfo {
	return estimateInfo
}

func (c *plezyColdPlayback) ProbeMetadata(context.Context, *core.User, core.RatingKey, core.ClientCaps) error {
	c.ready.Store(true)
	return nil
}

func (c *plezyColdPlayback) WarmProbeInfo(*core.User, core.ClientCaps, core.RatingKey) (core.MediaInfo, bool) {
	if !c.ready.Load() {
		return core.MediaInfo{}, false
	}
	return c.probed, true
}

// TestPlezyFirstPlayRefetchSelectsRuleSubtitle is #618 on Plezy: the
// detail row for a cold episode is estimated, so it must not claim
// file-checked completeness (Plezy reuses such a row at Play and
// trusts its selected subtitle). The identical re-fetch Plezy then
// makes at Play must answer with the probed tracks and the viewer's
// en/Always subtitle selected — while the first, browsing fetch
// neither probes nor waits.
func TestPlezyFirstPlayRefetchSelectsRuleSubtitle(t *testing.T) {
	info := core.MediaInfo{
		Container: "mkv", Duration: 25 * time.Minute, BitrateKbps: 8000,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, Default: true},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "en", Channels: 6, Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en"},
		},
	}
	f, _, _ := newIssue459Fixture(t, info, "en")
	cold := &plezyColdPlayback{fakePlayback: f.playback, probed: info}
	f.srv.setPlayback(cold)
	f.playback.info = estimateInfo
	key := fightClub.Key.String()
	hdr := []string{
		"X-Plex-Product", "Plezy",
		"X-Plex-Version", "2.24.0",
		"X-Plex-Platform", "Android",
		"X-Plex-Device", "AFTKRT",
		"X-Plex-Client-Identifier", "plezy-618",
		"X-Plex-Session-Identifier", "plezy-618-session",
	}
	// The one query shape both Plezy's detail screen and its
	// play-time getVideoPlaybackData send.
	detail := "/library/metadata/" + key + "?includeChapters=1&includeMarkers=1&checkFiles=1&includeStreams=1"

	first := f.get(t, detail, hdr...)
	if first.Code != 200 {
		t.Fatalf("detail %d: %s", first.Code, first.Body.String())
	}
	if cold.ready.Load() {
		t.Fatal("browsing the detail page probed the release")
	}
	if strings.Contains(partTag(first.Body.String()), `accessible=`) {
		t.Fatalf("pre-probe Part claims file-checked completeness: %s", partTag(first.Body.String()))
	}
	for _, tag := range subtitleTags(first.Body.String()) {
		if strings.Contains(tag, `index="2"`) {
			t.Fatalf("pre-probe row already lists the embedded track: %s", tag)
		}
	}

	second := f.get(t, detail, hdr...)
	if second.Code != 200 {
		t.Fatalf("play re-fetch %d: %s", second.Code, second.Body.String())
	}
	if !cold.ready.Load() {
		t.Fatal("play-time re-fetch did not wait for the probe")
	}
	selected := selectedTag(subtitleTags(second.Body.String()))
	if !strings.Contains(selected, `index="2"`) || !strings.Contains(selected, `languageCode="eng"`) {
		t.Fatalf("play-time re-fetch did not select the English subtitle: %q", selected)
	}
	if !strings.Contains(partTag(second.Body.String()), `accessible="1"`) {
		t.Fatalf("probed Part does not carry file-checked completeness: %s", partTag(second.Body.String()))
	}
}

func partTag(body string) string {
	start := strings.Index(body, "<Part")
	if start < 0 {
		return ""
	}
	rest := body[start:]
	if end := strings.Index(rest, ">"); end >= 0 {
		return rest[:end+1]
	}
	return rest
}
