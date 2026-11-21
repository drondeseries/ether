package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestScoreSourceCandidatesVideoAndNonVideo(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults("")
	cfg.Quality = config.Quality{
		Default: "Ultra",
		Profiles: []core.QualityProfile{
			{
				Name:             "Ultra",
				MaxHeight:        2160,
				MinHeight:        720,
				AllowHDR:         true,
				AllowDolbyVision: true,
				QualityOrder:     config.ResolutionGroups(2160, 720),
				CustomFormatScores: map[string]int{
					"grp-bonus": 250,
				},
			},
			{
				Name:         "Compact 1080p",
				MaxHeight:    1080,
				MinHeight:    720,
				QualityOrder: config.ResolutionGroups(1080, 720),
			},
		},
	}
	cfg.Resolver.BlockedTerms = []string{"badgroup"}
	cfg.Resolver.CustomFormats = []core.CustomFormat{
		{
			ID:   "grp-bonus",
			Name: "Bonus Group",
			Specifications: []core.FormatSpecification{
				{
					Name:           "grp",
					Implementation: "ReleaseTitleSpecification",
					Fields: map[string]json.RawMessage{
						"value": json.RawMessage(`"\\bBONUS\\b"`),
					},
				},
			},
		},
	}

	svc := New(nil, nil, nil, cfg, Options{})

	rels := []core.Release{
		{
			ID:        "r-remux",
			Name:      "Movie.2024.2160p.UHD.BluRay.REMUX.HEVC.Atmos-BONUS",
			Protocol:  core.ProtocolUsenet,
			NZBURL:    "https://idx.example/nzb/1",
			Indexer:   "NZBGeek",
			IndexerID: "geek",
			Size:      55 << 30,
		},
		{
			ID:        "r-1080",
			Name:      "Movie.2024.1080p.WEB-DL.DDP5.1.H.264-NTb",
			Protocol:  core.ProtocolTorrent,
			InfoHash:  "0123456789abcdef0123456789abcdef01234567",
			Indexer:   "Tracker",
			IndexerID: "trk",
			Size:      8 << 30,
			Seeders:   42,
		},
		{
			ID:       "r-blocked",
			Name:     "Movie.2024.2160p.WEB-DL.HEVC-badgroup",
			Protocol: core.ProtocolUsenet,
			NZBURL:   "https://idx.example/nzb/2",
			Size:     15 << 30,
		},
		{
			ID:       "r-ebook",
			Name:     "Movie Official Tie-In Novel (2024) [EPUB]",
			Protocol: core.ProtocolUsenet,
			NZBURL:   "https://idx.example/nzb/3",
			Size:     4 << 20,
		},
		{
			ID:       "r-flac",
			Name:     "Movie Original Soundtrack (2024) [FLAC 24bit]",
			Protocol: core.ProtocolTorrent,
			InfoHash: "fedcba9876543210fedcba9876543210fedcba98",
			Size:     600 << 20,
		},
	}

	got, err := ScoreSourceCandidates(ctx, svc, "", rels)
	if err != nil {
		t.Fatalf("ScoreSourceCandidates default profile: %v", err)
	}
	if len(got) != len(rels) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(rels))
	}

	remux, web1080, blocked, ebook, flac := got[0], got[1], got[2], got[3], got[4]
	if remux.Score == nil || web1080.Score == nil || blocked.Score == nil {
		t.Fatalf("expected video candidates to carry non-nil Score: %+v %+v %+v", remux, web1080, blocked)
	}
	if remux.Resolution != 2160 || remux.Source != "remux" || remux.Codec != "hevc" {
		t.Fatalf("remux parsed fields = (%d, %q, %q), want (2160, remux, hevc)", remux.Resolution, remux.Source, remux.Codec)
	}
	if web1080.Resolution != 1080 || web1080.Source != "webdl" || web1080.Codec != "h264" {
		t.Fatalf("web1080 parsed fields = (%d, %q, %q), want (1080, webdl, h264)", web1080.Resolution, web1080.Source, web1080.Codec)
	}
	if !(*remux.Score > *web1080.Score && *web1080.Score > *blocked.Score && *blocked.Score < 0) {
		t.Fatalf("score order = remux:%d web1080:%d blocked:%d, want remux > web1080 > blocked (<0)",
			*remux.Score, *web1080.Score, *blocked.Score)
	}

	for _, nv := range []core.SourceCandidate{ebook, flac} {
		if nv.Score != nil || nv.Resolution != 0 || nv.Source != "" || nv.Codec != "" {
			t.Fatalf("non-video candidate %q should omit video fields, got %+v", nv.Title, nv)
		}
		raw, err := json.Marshal(nv)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"score", "resolution", "source", "codec"} {
			if _, exists := m[k]; exists {
				t.Fatalf("non-video JSON unexpectedly contained %q: %s", k, raw)
			}
		}
	}

	// Named profile flips preference to 1080p WEB over 2160p Remux (which is disallowed in Compact 1080p).
	compact, err := ScoreSourceCandidates(ctx, svc, "Compact 1080p", rels[:2])
	if err != nil {
		t.Fatalf("ScoreSourceCandidates(Compact 1080p): %v", err)
	}
	if compact[1].Score == nil || compact[0].Score == nil || *compact[1].Score <= *compact[0].Score {
		t.Fatalf("Compact 1080p scores: remux=%v 1080p=%v, want 1080p > remux", compact[0].Score, compact[1].Score)
	}

	// Unknown profile returns ErrUnknownProfile.
	if _, err := ScoreSourceCandidates(ctx, svc, "NoSuchProfile", rels[:1]); !errors.Is(err, ErrUnknownProfile) {
		t.Fatalf("unknown profile err = %v, want ErrUnknownProfile", err)
	}
}
