package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestReleaseTitleIDs(t *testing.T) {
	for _, tc := range []struct {
		name, search, detail string
		kind                 core.MediaType
		want                 core.IDs
		wantErr              bool
	}{
		{"TV alias", `{"total_pages":1,"total_results":1,"results":[{"id":456,"name":"Los Simpson"}]}`, `{"id":456,"external_ids":{"imdb_id":"tt0096697","tvdb_id":71663}}`, core.TypeShow, core.IDs{TMDB: 456, IMDB: "tt0096697", TVDB: 71663}, false},
		{"movie", `{"total_pages":1,"total_results":1,"results":[{"id":348}]}`, `{"id":348,"imdb_id":"tt0078748"}`, core.TypeMovie, core.IDs{TMDB: 348, IMDB: "tt0078748"}, false},
		{"ambiguous", `{"total_pages":1,"total_results":2,"results":[{"id":456},{"id":999}]}`, "", core.TypeShow, core.IDs{}, false},
		{"more pages", `{"total_pages":2,"total_results":21,"results":[{"id":456}]}`, "", core.TypeShow, core.IDs{}, false},
		{"missing count", `{"results":[{"id":456}]}`, "", core.TypeShow, core.IDs{}, false},
		{"empty", `{"total_pages":0,"total_results":0,"results":[]}`, "", core.TypeShow, core.IDs{}, false},
		{"invalid ID", `{"total_pages":1,"total_results":1,"results":[{"id":0}]}`, "", core.TypeShow, core.IDs{}, false},
		{"wrong detail identity", `{"total_pages":1,"total_results":1,"results":[{"id":456}]}`, `{"id":999}`, core.TypeShow, core.IDs{}, true},
		{"search error", "error", "", core.TypeShow, core.IDs{}, true},
		{"detail error", `{"total_pages":1,"total_results":1,"results":[{"id":456}]}`, "error", core.TypeShow, core.IDs{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			searches, details := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := tc.detail
				namespace := "tv"
				if tc.kind == core.TypeMovie {
					namespace = "movie"
				}
				if r.URL.Path == "/search/"+namespace {
					searches++
					if r.URL.Query().Get("query") != "Los Simpsons" {
						t.Errorf("query = %q", r.URL.RawQuery)
					}
					if r.URL.Query().Has("year") || r.URL.Query().Has("first_air_date_year") {
						t.Error("lookup constrained by requested year")
					}
					body = tc.search
				} else {
					details++
				}
				if body == "error" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if body == "" {
					t.Error("unexpected detail fetch")
				}
				fmt.Fprint(w, body)
			}))
			defer srv.Close()
			a := &App{tmdb: tmdb.New("test-key", quietLog(), tmdb.WithBaseURL(srv.URL), tmdb.WithMaxRetries(0))}
			got, err := a.releaseTitleIDs(t.Context(), tc.kind, "Los Simpsons")
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("IDs=%+v err=%v, want %+v error=%v", got, err, tc.want, tc.wantErr)
			}
			if searches != 1 || (tc.detail == "" && details != 0) {
				t.Fatalf("searches=%d details=%d", searches, details)
			}
		})
	}
}
