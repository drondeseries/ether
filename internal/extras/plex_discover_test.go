package extras

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestPlexDiscoverExtrasSource(t *testing.T) {
	const (
		discoverHex = "5d776827999c64001ec2c7f8"
		plexToken   = "secret-plex-token"
	)
	var (
		failDiscover atomic.Bool
		sigSeq       atomic.Int64
		cdnSawToken  atomic.Bool
		mp4Body      = append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, make([]byte, 64)...)
	)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/" + discoverHex + "/extras":
			if failDiscover.Load() {
				http.Error(w, "upstream down", http.StatusBadGateway)
				return
			}
			sig := sigSeq.Add(1)
			payload := map[string]any{
				"MediaContainer": map[string]any{
					"Metadata": []map[string]any{
						{
							"ratingKey":             "disc-trl-1",
							"title":                 "Goldfinger Official Trailer",
							"subtype":               "trailer",
							"extraType":             1,
							"duration":              191000,
							"originallyAvailableAt": "1964-09-17",
							"Media": []map[string]any{
								{
									"container":       "mp4",
									"videoCodec":      "h264",
									"audioCodec":      "aac",
									"videoResolution": "720",
									"bitrate":         2500,
									"duration":        191000,
									"url":             fmt.Sprintf("%s/cdn/trailer.mp4?sig=%d", srv.URL, sig),
									"Part": []map[string]any{
										{"key": "/library/metadata/" + discoverHex + "/extras/disc-trl-1/parts/mp4-2500"},
									},
								},
							},
						},
						{
							"ratingKey": "disc-scene-1",
							"title":     "Laser Scene",
							"subtype":   "sceneOrSample",
							"extraType": 6,
							"duration":  95000,
							"Media": []map[string]any{
								{
									"container":       "mp4",
									"videoCodec":      "h264",
									"audioCodec":      "aac",
									"videoResolution": "720",
									"bitrate":         2200,
									"Part": []map[string]any{
										{"key": "/library/metadata/" + discoverHex + "/extras/disc-scene-1/parts/mp4-2200"},
									},
								},
							},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
		case "/library/metadata/" + discoverHex + "/extras/disc-scene-1/parts/mp4-2200":
			if r.Header.Get("X-Plex-Token") != plexToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Location", srv.URL+"/cdn/scene.mp4")
			w.WriteHeader(http.StatusFound)
		case "/cdn/trailer.mp4", "/cdn/scene.mp4":
			if r.Header.Get("X-Plex-Token") != "" || strings.Contains(r.URL.RawQuery, plexToken) {
				cdnSawToken.Store(true)
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(mp4Body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tokenVal := plexToken
	discover := NewPlexDiscover(PlexDiscoverOptions{
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
		Token:   func() string { return tokenVal },
		GUID:    func(context.Context, core.RatingKey) string { return "plex://movie/" + discoverHex },
	})
	movie := &core.Title{Key: core.MovieKey(658), Type: core.TypeMovie, Title: "Goldfinger", IDs: core.IDs{TMDB: 658}}
	ytVideos := fakeVideos{vs: []tmdb.Video{
		{Site: "YouTube", Key: "dQw4w9WgXcQ", Name: "YouTube Trailer", Type: tmdb.VideoTrailer, Official: true, ISO639: "en", Size: 1080},
	}}

	// 1. Discover enables extras without yt-dlp or any resolver command.
	noCmd := New(Options{
		Discover:  discover,
		Videos:    ytVideos,
		CacheDir:  t.TempDir(),
		CacheHTTP: srv.Client(),
		Config:    func() config.Extras { return config.Extras{Enabled: true} },
		LookPath:  func(string) (string, error) { return "", exec.ErrNotFound },
	})
	t.Cleanup(noCmd.Close)
	if !noCmd.Enabled() || noCmd.CommandAvailable() || !noCmd.DiscoverAvailable() || noCmd.DisabledReason() != "" {
		t.Fatalf("Discover alone should enable extras without yt-dlp: enabled=%v cmd=%v discover=%v reason=%q",
			noCmd.Enabled(), noCmd.CommandAvailable(), noCmd.DiscoverAvailable(), noCmd.DisabledReason())
	}

	// 2. With both Discover and command available, Discover trailer orders first,
	// resolves fresh signed URLs per play, resolves Part.key 302 server-side
	// without exposing the Plex token, and caches MP4 without invoking yt-dlp.
	var runnerCalls atomic.Int32
	svc := New(Options{
		Discover:  discover,
		Videos:    ytVideos,
		CacheDir:  t.TempDir(),
		CacheHTTP: srv.Client(),
		Config:    func() config.Extras { return config.Extras{Enabled: true, ResolverCommand: "yt-dlp"} },
		LookPath:  func(s string) (string, error) { return s, nil },
		Runner: func(context.Context, string, []string) ([]byte, error) {
			runnerCalls.Add(1)
			return nil, errors.New("yt-dlp should not run for Plex Discover extras")
		},
	})
	t.Cleanup(svc.Close)

	list, err := svc.List(context.Background(), movie, "en")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) == 0 || list[0].Source != SourcePlexDiscover || !list[0].IsTrailer() {
		t.Fatalf("want Plex Discover trailer first, got %+v", list)
	}
	if list[0].Site != "" || list[0].SiteKey != "" {
		t.Fatalf("Discover extra must not set YouTube/Vimeo Site/SiteKey: %+v", list[0])
	}

	// Signed URL is resolved fresh per play and never carries X-Plex-Token.
	r1, err := svc.Resolve(context.Background(), list[0])
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	r2, err := svc.Resolve(context.Background(), list[0])
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if r1.URL == r2.URL || r1.Container != "mp4" || len(r1.Headers) != 0 || strings.Contains(r1.URL, plexToken) {
		t.Fatalf("expected fresh token-free signed MP4 URL per Resolve: r1=%+v r2=%+v", r1, r2)
	}

	// Part.key-only extra resolves 302 server-side with token and returns clean media URL.
	var scene Extra
	for _, item := range list {
		if item.Source == SourcePlexDiscover && item.Subtype == "sceneOrSample" {
			scene = item
			break
		}
	}
	if scene.Key == 0 {
		t.Fatalf("missing Discover sceneOrSample extra in %+v", list)
	}
	rs, err := svc.Resolve(context.Background(), scene)
	if err != nil {
		t.Fatalf("Resolve Part.key fallback: %v", err)
	}
	if rs.URL != srv.URL+"/cdn/scene.mp4" || len(rs.Headers) != 0 {
		t.Fatalf("Part.key redirect = %+v, want clean /cdn/scene.mp4 without headers", rs)
	}

	// Caching Discover trailer downloads MP4 directly without invoking yt-dlp or sending Plex token.
	cached, err := svc.CacheExtra(context.Background(), list[0])
	if err != nil {
		t.Fatalf("CacheExtra: %v", err)
	}
	if _, err := os.Stat(cached.Path); err != nil {
		t.Fatalf("cached MP4 missing: %v", err)
	}
	if runnerCalls.Load() != 0 {
		t.Fatalf("yt-dlp runner called %d times for Discover MP4", runnerCalls.Load())
	}
	if cdnSawToken.Load() {
		t.Fatal("Plex token leaked to media CDN")
	}

	// 3. Fallback: when Discover fails, TMDB + command extras still list cleanly.
	failDiscover.Store(true)
	fallbackSvc := New(Options{
		Discover:  discover,
		Videos:    ytVideos,
		CacheDir:  t.TempDir(),
		CacheHTTP: srv.Client(),
		Now:       func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		Config:    func() config.Extras { return config.Extras{Enabled: true, ResolverCommand: "yt-dlp"} },
		LookPath:  func(s string) (string, error) { return s, nil },
	})
	t.Cleanup(fallbackSvc.Close)
	fb, err := fallbackSvc.List(context.Background(), movie, "en")
	if err != nil {
		t.Fatalf("fallback List: %v", err)
	}
	if len(fb) == 0 || fb[0].Source != SourceCommand || fb[0].SiteKey != "dQw4w9WgXcQ" {
		t.Fatalf("expected clean fallback to TMDB+command, got %+v", fb)
	}
}
