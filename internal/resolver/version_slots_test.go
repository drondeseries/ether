package resolver

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// A configured menu is a settings contract, not a projection of discovery.
// Verify cold/warm/expired caches, optional modes and admission of an explicit
// logical choice, including its unavailable-source fallback.
func TestConfiguredVersionSlots(t *testing.T) {
	for _, mode := range []string{"default", "higher", "resolution", "source", "both"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testCfg()
			cfg.Resolver.SameQualityAlternatives = 2
			cfg.Resolver.AlternativesPerResolution = mode == "resolution" || mode == "both"
			cfg.Resolver.AlternativesPerSource = mode == "source" || mode == "both"
			cfg.Resolver.OfferHigherVersions = mode != "default"
			idx := &fakeIndexer{releases: variantReleases()}
			svc := New(idx, fakeHealth{}, nil, cfg, Options{})
			defer svc.Close()
			req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
			ctx := context.Background()
			cold, err := svc.VersionSlots(ctx, req, core.MaxVariants, false)
			if err != nil || len(cold) < 2 || len(cold) > core.MaxVariants {
				t.Fatalf("cold slots %d: %v", len(cold), err)
			}
			ids := func(vs []core.ReleaseVariant) []string {
				out := []string{}
				for _, v := range vs {
					out = append(out, v.ID)
				}
				return out
			}
			want := 3
			if mode == "higher" {
				want = 6
			} else if mode != "default" {
				want = core.MaxVariants
			}
			if len(cold) != want {
				t.Fatalf("configured slot count %d, want %d", len(cold), want)
			}
			before := ids(cold)
			if idx.searches.Load() != 0 {
				t.Fatal("browse searched")
			}
			primeSearchCache(t, svc, req.Client)
			searches := idx.searches.Load()
			warm, err := svc.VersionSlots(ctx, req, core.MaxVariants, true)
			if err != nil || !reflect.DeepEqual(ids(warm), before) {
				t.Fatalf("cache changed slots: %v", err)
			}
			if idx.searches.Load() != searches {
				t.Fatal("labels searched")
			}
			found := false
			for _, v := range warm {
				if strings.Contains(v.Label, "GRP") {
					found = true
				}
			}
			if !found {
				t.Fatal("known releases lost their labels")
			}
			// Local responses keep the configured identities immediately and
			// acquire labels only through the bounded background snapshot.
			local := core.WithLocalResponse(ctx)
			immediate, e := svc.VersionSlots(local, req, core.MaxVariants, true)
			if e != nil || !reflect.DeepEqual(ids(immediate), before) {
				t.Fatalf("local slots: %v", e)
			}
			svc.bgWG.Wait()
			labelled, e := svc.VersionSlots(local, req, core.MaxVariants, true)
			if e != nil || !reflect.DeepEqual(labelled, warm) {
				t.Fatalf("local labels differ: %v", e)
			}
			for _, v := range warm[1:] {
				if v.Release.ID == v.ID {
					continue
				} // an unavailable configured reservation
				selected := req
				selected.Prefer = v.ID
				result, e := svc.Resolve(ctx, selected)
				if e != nil || !result.MatchesVersion(v.ID) {
					t.Fatalf("slot admission %s: %v", v.Label, e)
				}
				break
			}
			for _, v := range warm[1:] {
				if v.Release.ID != v.ID {
					continue
				}
				selected := req
				selected.Prefer = v.ID
				result, e := svc.Resolve(ctx, selected)
				if e != nil || result == nil || result.VersionSelector != "" {
					t.Fatalf("unavailable configured slot: %v", e)
				}
				break
			}
			// This configured but absent source remains visible and safely falls
			// back to normal ranking. It must not gain a false selector receipt.
			missing := versionSlot{Kind: "tier", Height: 480, Source: "remux"}.id()
			selected := req
			selected.Prefer = missing
			result, e := svc.Resolve(ctx, selected)
			if e != nil || result == nil || result.VersionSelector != "" {
				t.Fatalf("missing fallback: %v", e)
			}
		})
	}
	t.Run("higher slot and device ceiling", func(t *testing.T) {
		cfg := testCfg()
		cfg.Quality.Rules = nil
		cfg.Resolver.AlternativesPerResolution = true
		cfg.Resolver.OfferHigherVersions = true
		idx := &fakeIndexer{releases: []core.Release{rel("high", "The.Signal.1999.2160p.WEB-DL.HEVC.DDP5.1-GRP", 20000), rel("hd", "The.Signal.1999.1080p.WEB-DL.H.264.DDP5.1-GRP", 8000)}}
		svc := New(idx, fakeHealth{}, nil, cfg, Options{})
		defer svc.Close()
		req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
		slots, err := svc.VersionSlots(context.Background(), req, core.MaxVariants, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range slots {
			slot, _ := parseVersionSlot(v.ID)
			if slot.Height == 2160 {
				req.Prefer = v.ID
				break
			}
		}
		if req.Prefer == "" {
			t.Fatal("owner-enabled higher tier missing")
		}
		res, err := svc.Resolve(context.Background(), req)
		if err != nil || !res.MatchesVersion(req.Prefer) || res.Release.ID != "high" {
			t.Fatalf("higher tier: %+v %v", res, err)
		}
		req.Client.MaxHeight = 1080
		res, err = svc.Resolve(context.Background(), req)
		if err != nil || res.Release.ID != "hd" || res.VersionSelector != "" {
			t.Fatalf("device ceiling: %+v %v", res, err)
		}
	})

}

// Menu labels, reservation and admission must share the same Auto identity as
// discovery changes, whether Auto is fresh, remembered, or owner-pinned.
func TestVersionSlotSourceAgreement(t *testing.T) {
	for _, mode := range []string{"fresh", "remembered", "pinned"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testCfg()
			cfg.Quality.Rules = nil
			cfg.Quality.Default, cfg.Quality.Remote = "4K", "4K"
			cfg.Resolver.AlternativesPerResolution = true
			cfg.Resolver.OfferHigherVersions = true
			releases := []core.Release{
				rel("high", "The.Signal.1999.2160p.WEB-DL.HEVC.DDP5.1-GRP", 20000),
				rel("hd", "The.Signal.1999.1080p.WEB-DL.H.264.DDP5.1-GRP", 8000),
			}
			svc := New(&fakeIndexer{releases: releases}, fakeHealth{}, nil, cfg, Options{})
			defer svc.Close()
			ctx := context.Background()
			req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
			primeSearchCache(t, svc, req.Client)
			if mode == "remembered" {
				req.PlayedRelease = "hd"
			}
			if mode == "pinned" {
				pinned, _ := svc.LookupRelease("hd")
				if _, err := svc.Pin(ctx, req.Title.Key, "", pinned, ""); err != nil {
					t.Fatal(err)
				}
				req.PlayedRelease = "high" // a pin wins over previous playback
			}
			versions, err := svc.VersionSlots(ctx, req, core.MaxVariants, true)
			if err != nil {
				t.Fatal(err)
			}
			wantAuto := "high"
			if mode != "fresh" {
				wantAuto = "hd"
			}
			if versions[0].Release.ID != wantAuto {
				t.Fatalf("Auto = %s, want %s", versions[0].Release.ID, wantAuto)
			}
			for _, version := range versions {
				if version.Release.ID == version.ID {
					continue
				}
				pick := req
				pick.Prefer, pick.PreferredOnly = version.ID, true
				got, err := svc.Resolve(ctx, pick)
				if err != nil || got == nil || got.Release.ID != version.Release.ID || !got.MatchesVersion(version.ID) {
					t.Fatalf("menu %s (%s) admitted %+v: %v", version.Label, version.Release.ID, got, err)
				}
			}
			if mode != "fresh" && versions[1].Release.ID != "high" {
				t.Fatalf("4K source consumed by Auto: %+v", versions[1])
			}
		})
	}
}

func TestColdVersionSlotsPlaceholderHasNoPresumedAudioLanguage(t *testing.T) {
	cfg := testCfg()
	svc := New(&fakeIndexer{}, fakeHealth{}, nil, cfg, Options{})
	defer svc.Close()
	title := matrixTitle()
	title.Language = "en"
	slots, err := svc.VersionSlots(context.Background(), core.ResolveRequest{Title: title, Client: appleTVCaps()}, core.MaxVariants, false)
	if err != nil || len(slots) == 0 {
		t.Fatalf("cold VersionSlots = %d, %v", len(slots), err)
	}
	for i, slot := range slots {
		if !slot.Info.Estimated {
			t.Fatalf("slot %d not estimated: %+v", i, slot.Info)
		}
		var audio *core.StreamInfo
		for j := range slot.Info.Streams {
			if slot.Info.Streams[j].Type == "audio" {
				audio = &slot.Info.Streams[j]
				break
			}
		}
		if audio == nil {
			t.Fatalf("slot %d missing placeholder audio stream: %+v", i, slot.Info.Streams)
		}
		if audio.Language != "" {
			t.Fatalf("slot %d cold placeholder claimed audio language %q, want empty", i, audio.Language)
		}
	}
}
