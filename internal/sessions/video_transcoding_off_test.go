package sessions

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestDecideProfileVideoTranscodingOff(t *testing.T) {
	hevc := core.MediaInfo{
		Container: "mkv",
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2},
		},
	}
	h264TrueHD := core.MediaInfo{
		Container: "mkv",
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "truehd", Channels: 6},
		},
	}
	h264OnlyCaps := core.ClientCaps{
		Product:       "Plex Web",
		Platform:      "Chrome",
		MaxHeight:     1080,
		Containers:    []string{"mp4"},
		HLSContainers: []string{"fmp4", "mpegts"},
		VideoCodecs:   []string{"h264"},
		AudioCodecs:   []string{"aac"},
	}

	b := &Broker{deps: BrokerDeps{
		SurfacePolicy:       testSessionPolicy,
		Transcoder:          profileTranscoder{},
		VideoTranscodingOff: func() bool { return true },
	}}

	// Without declared profiles: HEVC on H.264 client fails with ErrVideoTranscodingDisabled.
	req := frontend.PlanRequest{
		Caps:   h264OnlyCaps,
		Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: 1, SubtitleStream: -1}},
	}
	_, err := b.decideProfile(&core.PlaySession{Info: hevc}, req)
	if !errors.Is(err, core.ErrVideoTranscodingDisabled) {
		t.Fatalf("HEVC without profiles: err = %v, want ErrVideoTranscodingDisabled", err)
	}
	var noPath *frontend.NoCompatiblePathError
	if !errors.As(err, &noPath) || !strings.Contains(err.Error(), reasonVideoTranscodingOff) {
		t.Fatalf("expected NoCompatiblePathError mentioning %q, got %v", reasonVideoTranscodingOff, err)
	}

	// With declared profiles: HEVC still rejected with ErrVideoTranscodingDisabled.
	profCaps := h264OnlyCaps
	profCaps.Profiles = &core.ClientCapabilityProfiles{
		DirectPlay:  []core.ClientDirectPlayProfile{{MediaType: "video", Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}},
		Transcoding: []core.ClientTranscodeProfile{{MediaType: "video", Delivery: "hls", Containers: []string{"mpegts", "fmp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}},
	}
	reqWithProfiles := req
	reqWithProfiles.Caps = profCaps
	_, err = b.decideProfile(&core.PlaySession{Info: hevc}, reqWithProfiles)
	if !errors.Is(err, core.ErrVideoTranscodingDisabled) {
		t.Fatalf("HEVC with profiles: err = %v, want ErrVideoTranscodingDisabled", err)
	}

	// Video copy + audio conversion (H.264 + TrueHD -> AAC) succeeds!
	dec, err := b.decideProfile(&core.PlaySession{Info: h264TrueHD}, reqWithProfiles)
	if err != nil {
		t.Fatalf("H.264 + TrueHD audio conversion failed: %v", err)
	}
	if !dec.VideoCopy || dec.AudioCopy {
		t.Fatalf("expected video copy + audio conversion, got %+v", dec)
	}

	// planAlternatives still reports encode=true for Jellyfin audio-only conversion,
	// without enabling video transcode.
	_, _, encode := b.planAlternatives(&core.PlaySession{Info: h264TrueHD}, reqWithProfiles)
	if !encode {
		t.Fatal("expected audio-only conversion to report Transcoding=true in planAlternatives")
	}

	// Proactive fallback and Vela quality plan are disabled when video transcoding is off.
	if b.ProactiveFallbackAvailable() {
		t.Fatal("ProactiveFallbackAvailable() must be false when video transcoding is off")
	}
	variants, limits, err := b.qualityPlan(context.Background(), &brokerSession{public: frontend.Session{Protocol: "vela"}}, &core.Viewer{ID: 1})
	if err != nil || len(variants) != 0 || len(limits) == 0 || limits[0].Reason != "transcoding_disabled" {
		t.Fatalf("qualityPlan = (%v, %v, %v), want transcoding_disabled", variants, limits, err)
	}
}
