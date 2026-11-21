package adapters

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/resolver"
)

type stubSearchIndexer struct {
	lastReq core.SearchRequest
	rels    []core.Release
}

func (s *stubSearchIndexer) Name() string { return "stub" }

func (s *stubSearchIndexer) Search(_ context.Context, req core.SearchRequest) ([]core.Release, error) {
	s.lastReq = req
	return append([]core.Release(nil), s.rels...), nil
}

func (s *stubSearchIndexer) FetchNZB(_ context.Context, _ core.Release) ([]byte, error) {
	return nil, core.ErrNotFound
}

func TestKindResolverIndexerSearchRecentCapAndScoring(t *testing.T) {
	ctx := context.Background()

	defCfg := config.Defaults("")
	defCfg.Quality = config.Quality{
		Default: "Main",
		Profiles: []core.QualityProfile{
			{
				Name:             "Main",
				MaxHeight:        2160,
				MinHeight:        720,
				AllowHDR:         true,
				AllowDolbyVision: true,
				QualityOrder:     config.ResolutionGroups(2160, 720),
			},
			{
				Name:         "HDOnly",
				MaxHeight:    1080,
				MinHeight:    720,
				QualityOrder: config.ResolutionGroups(1080, 720),
			},
		},
	}

	base := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	rels := make([]core.Release, 0, 135)
	for i := 0; i < 135; i++ {
		title := fmt.Sprintf("Show.S01E%03d.1080p.WEB-DL.H264-GRP", i+1)
		if i == 134 {
			// Newest item is non-video to verify mixed feed handling.
			title = "Magazine Issue 135 (2026) [EPUB]"
		} else if i == 133 {
			title = "Show.S01E134.2160p.WEB-DL.HEVC-GRP"
		}
		rels = append(rels, core.Release{
			ID:        fmt.Sprintf("rel-%d", i),
			Name:      title,
			Protocol:  core.ProtocolUsenet,
			NZBURL:    fmt.Sprintf("https://idx.example/nzb/%d", i),
			Indexer:   "NZBIndex",
			IndexerID: "nzbidx",
			PubDate:   base.Add(time.Duration(i) * time.Minute),
		})
	}

	idx := &stubSearchIndexer{rels: rels}
	resSvc := resolver.New(idx, nil, nil, defCfg, resolver.Options{})
	kr := NewKindResolver(KindResolverDeps{
		Indexer:  idx,
		Resolver: resSvc,
	})

	// 1. Empty query without category is refused with ValidationError.
	var ve *plugins.ValidationError
	if _, err := kr.ResolveSources(ctx, "plugin.example", core.SourceQuery{
		Kind: "indexer_search",
	}); !errors.As(err, &ve) {
		t.Fatalf("empty query + empty category err = %v, want *plugins.ValidationError", err)
	}

	// 2. Empty query with category succeeds, sorts newest-first, and caps at 100.
	cands, err := kr.ResolveSources(ctx, "plugin.example", core.SourceQuery{
		Kind:     "indexer_search",
		Category: "7000,5000",
	})
	if err != nil {
		t.Fatalf("empty query + category err: %v", err)
	}
	if idx.lastReq.Title != "" || idx.lastReq.Category != "7000,5000" {
		t.Fatalf("forwarded SearchRequest = %+v, want empty Title and Category=7000,5000", idx.lastReq)
	}
	if len(cands) != 100 {
		t.Fatalf("len(cands) = %d, want 100", len(cands))
	}
	// Item 134 (newest) is first and non-video; item 133 (2160p) is second and scored; item 35 is 100th.
	if cands[0].Title != "Magazine Issue 135 (2026) [EPUB]" || cands[0].Score != nil || cands[0].Resolution != 0 {
		t.Fatalf("newest non-video candidate = %+v, want unscored EPUB", cands[0])
	}
	if cands[1].Resolution != 2160 || cands[1].Source != "webdl" || cands[1].Codec != "hevc" || cands[1].Score == nil {
		t.Fatalf("second candidate = %+v, want scored 2160p webdl hevc", cands[1])
	}
	if cands[2].Resolution != 1080 || cands[2].Score == nil || *cands[1].Score <= *cands[2].Score {
		t.Fatalf("Main profile should score 2160p (%v) > 1080p (%v)", cands[1].Score, cands[2].Score)
	}
	if cands[99].Title != "Show.S01E036.1080p.WEB-DL.H264-GRP" {
		t.Fatalf("100th newest candidate = %q, want Show.S01E036...", cands[99].Title)
	}

	// 3. Named profile ("HDOnly") flips ranking so 1080p outranks 2160p.
	hdCands, err := kr.ResolveSources(ctx, "plugin.example", core.SourceQuery{
		Kind:     "indexer_search",
		Category: "5000",
		Profile:  "HDOnly",
	})
	if err != nil {
		t.Fatalf("HDOnly search: %v", err)
	}
	if *hdCands[2].Score <= *hdCands[1].Score {
		t.Fatalf("HDOnly profile should score 1080p (%d) > 2160p (%d)", *hdCands[2].Score, *hdCands[1].Score)
	}

	// 4. Unknown profile is refused with ValidationError on field "profile".
	ve = nil
	if _, err := kr.ResolveSources(ctx, "plugin.example", core.SourceQuery{
		Kind:     "indexer_search",
		Category: "5000",
		Profile:  "DoesNotExist",
	}); !errors.As(err, &ve) || len(ve.Fields) == 0 || ve.Fields[0].Field != "profile" {
		t.Fatalf("unknown profile err = %v (fields=%+v), want ValidationError on profile", err, ve)
	}
}
