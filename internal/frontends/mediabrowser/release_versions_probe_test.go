package mediabrowser

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestReleaseVersionSourcesCarryProbedAudioTracksOnAutoAndVariants(t *testing.T) {
	probed := frontend.MediaInfo{
		Container: "mkv",
		Size:      6 << 30,
		Duration:  2 * time.Hour,
		Streams: []frontend.StreamInfo{
			{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "eac3", Channels: 6, Language: "jpn", Default: true},
			{Index: 2, Type: "audio", Codec: "eac3", Channels: 6, Language: "eng"},
		},
	}
	variants := []frontend.ReleaseVariant{
		{
			ID:      "r-auto",
			Label:   "1080p BluRay",
			Auto:    true,
			Release: frontend.Release{ID: "r-auto", Name: "Anime.2024.1080p.MULTi-GRP", Size: 6 << 30},
			Info:    probed,
		},
		{
			ID:      "r-alt",
			Label:   "1080p WEB-DL",
			Release: frontend.Release{ID: "r-alt", Name: "Anime.2024.1080p.WEB-DL.MULTi-GRP", Size: 4 << 30},
			Info:    probed,
		},
	}

	for _, proto := range []frontend.Protocol{"jellyfin", "emby"} {
		t.Run(string(proto), func(t *testing.T) {
			i := &instance{
				protocol: proto,
				host: frontend.Host{
					VersionHandles: &issuedHandles{},
				},
			}
			req := httptest.NewRequest("GET", "/Users/1/Items/1", nil)
			viewer := &frontend.Viewer{ID: 1, IsOwner: true}
			scope := frontend.VersionScope{Viewer: viewer, Protocol: proto}
			key := frontend.MovieKey(1)
			auto := i.defaultMediaSource(req, viewer, "tok", key)

			got := i.releaseVersionSourcesFor(req, scope, key, variants, auto)
			if len(got) != 2 {
				t.Fatalf("sources = %d, want 2", len(got))
			}
			for idx, raw := range got {
				src := raw.(map[string]any)
				streams, _ := src["MediaStreams"].([]any)
				var audioLangs []string
				for _, s := range streams {
					m, _ := s.(map[string]any)
					if m["Type"] == "Audio" {
						lang, _ := m["Language"].(string)
						audioLangs = append(audioLangs, lang)
					}
				}
				if len(audioLangs) != 2 || audioLangs[0] != "jpn" || audioLangs[1] != "eng" {
					t.Fatalf("source[%d] audio languages = %v, want [jpn eng]", idx, audioLangs)
				}
			}
		})
	}
}
