package playback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type realDecideTranscoder struct {
	*fakeTranscoder
	videoOff bool
}

func (r realDecideTranscoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	return transcode.Decide(info, caps, opts, transcode.Policy{})
}

func (r realDecideTranscoder) VideoTranscodingOff() bool { return r.videoOff }

func TestVideoTranscodingOffSkipsProbedEncodeCandidateAndFailsClearly(t *testing.T) {
	hevcRelease := mkv("HEVC", 100_000, 100*time.Second)
	hevcRelease.Codec = "hevc"
	hevcRelease.Height = 1080
	hevcRelease.BitrateKbps = 6000
	// H.264 1080p SDR: video copies (remux + audio conversion allowed when off).
	h264Release := mkv("H264", 100_000, 100*time.Second)
	h264Release.Codec = "h264"
	h264Release.Height = 1080
	h264Release.BitrateKbps = 6000

	h264OnlyCaps := core.ClientCaps{
		Product:     "Plex Web",
		Platform:    "Chrome",
		MaxHeight:   1080,
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
		Containers:  []string{"mp4"},
	}

	t.Run("skips probed HEVC and selects probed H264", func(t *testing.T) {
		h := newHarness(t, []*fakeRelease{hevcRelease, h264Release}, []string{"HEVC", "H264"}, func(h *harness, _ *Options) {
			h.cfg.Transcode.VideoTranscoding = "off"
			h.testTranscode = realDecideTranscoder{fakeTranscoder: h.tr, videoOff: true}
		})
		ps, err := h.m.Prepare(context.Background(), owner, movieKey, h264OnlyCaps)
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if ps.Resolution == nil || ps.Resolution.Release.ID != "H264" {
			t.Fatalf("selected release = %+v, want H264", ps.Resolution)
		}
	})

	t.Run("fails with ErrVideoTranscodingDisabled when every probed candidate needs video encode", func(t *testing.T) {
		h := newHarness(t, []*fakeRelease{hevcRelease}, []string{"HEVC"}, func(h *harness, _ *Options) {
			h.cfg.Transcode.VideoTranscoding = "off"
			h.testTranscode = realDecideTranscoder{fakeTranscoder: h.tr, videoOff: true}
		})
		_, err := h.m.Prepare(context.Background(), owner, movieKey, h264OnlyCaps)
		if !errors.Is(err, core.ErrVideoTranscodingDisabled) {
			t.Fatalf("Prepare() err = %v, want ErrVideoTranscodingDisabled", err)
		}
	})
}
