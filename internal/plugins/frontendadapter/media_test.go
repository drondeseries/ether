package frontendadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestMediaHandlerSurfacesArchiveUnstreamablePlainly(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	issuer := NewTicketIssuer()
	handler := NewMediaHandler(MediaDeps{
		Tickets: issuer,
		Log:     logger,
		Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
			return viewerWithLibraries(id, "Reader", core.LibraryMovies), nil
		},
		Open: func(_ context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
			return nil, fmt.Errorf("open container: %w", core.ErrArchiveUnstreamable)
		},
	})

	token, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:item-1", Adapter: "feed"})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+token, nil))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if got := rec.Header().Get("X-Ether-Media-Error"); got != "archive_unstreamable" {
		t.Fatalf("X-Ether-Media-Error = %q, want archive_unstreamable", got)
	}
	if !strings.Contains(rec.Body.String(), "This archive format or compression method cannot be streamed.") {
		t.Fatalf("unexpected body %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "temporarily unavailable") {
		t.Fatalf("body still says temporarily unavailable: %q", rec.Body.String())
	}
	if !strings.Contains(logBuf.String(), "cannot be streamed") {
		t.Fatalf("expected plain log entry, got %q", logBuf.String())
	}
}

func TestMediaHandlerDefaultsEmptyMIMEToOctetStreamNeverVideo(t *testing.T) {
	issuer := NewTicketIssuer()
	payload := append([]byte{0x00, 0x00, 0x00, 0x18}, []byte("ftypisom-opaque-binary")...)
	handler := NewMediaHandler(MediaDeps{
		Tickets: issuer,
		Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
			return viewerWithLibraries(id, "Reader", core.LibraryMovies), nil
		},
		Open: func(_ context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
			return &frontend.KindMedia{
				ReaderAt: bytes.NewReader(payload),
				Closer:   io.NopCloser(nil),
				Size:     int64(len(payload)),
				MIME:     "",
				Name:     "payload.bin",
			}, nil
		},
	})

	token, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:bin-1", Adapter: "feed"})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+token, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", got)
	}
}

func TestMediaHandlerPartScopedTicketsAndQuery(t *testing.T) {
	issuer := NewTicketIssuer()
	var wholeOpens int
	var partOpens []int

	handler := NewMediaHandler(MediaDeps{
		Tickets: issuer,
		Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
			return viewerWithLibraries(id, "Reader", core.LibraryMovies), nil
		},
		Open: func(_ context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
			wholeOpens++
			data := []byte("whole-item")
			return &frontend.KindMedia{
				ReaderAt: bytes.NewReader(data),
				Closer:   io.NopCloser(nil),
				Size:     int64(len(data)),
				MIME:     "application/octet-stream",
				Name:     "whole.bin",
			}, nil
		},
		OpenPart: func(_ context.Context, _ *core.Viewer, _ string, partIndex int, _ []string) (*frontend.KindMedia, error) {
			partOpens = append(partOpens, partIndex)
			data := []byte(fmt.Sprintf("part-%d", partIndex))
			return &frontend.KindMedia{
				ReaderAt: bytes.NewReader(data),
				Closer:   io.NopCloser(nil),
				Size:     int64(len(data)),
				MIME:     "application/octet-stream",
				Name:     fmt.Sprintf("part-%d.bin", partIndex),
			}, nil
		},
	})

	unpinned, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:multi", Adapter: "feed"})
	if err != nil {
		t.Fatal(err)
	}
	// Whole-item open when no part is specified.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+unpinned, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "whole-item" || wholeOpens != 1 {
		t.Fatalf("whole open: code=%d body=%q opens=%d", rec.Code, rec.Body.String(), wholeOpens)
	}
	// ?part=2 on unpinned ticket opens part 2.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+unpinned+"?part=2", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "part-2" {
		t.Fatalf("query part=2: code=%d body=%q", rec.Code, rec.Body.String())
	}
	// Malformed or negative ?part returns 400 invalid_part.
	for _, bad := range []string{"-1", "abc"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+unpinned+"?part="+bad, nil))
		if rec.Code != http.StatusBadRequest || rec.Header().Get("X-Ether-Media-Error") != "invalid_part" {
			t.Fatalf("query part=%s: code=%d hdr=%q body=%s", bad, rec.Code, rec.Header().Get("X-Ether-Media-Error"), rec.Body.String())
		}
	}

	// Part-scoped ticket serves only its bound part.
	part1 := 1
	pinned, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:multi", Adapter: "feed", PartIndex: &part1})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+pinned, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "part-1" {
		t.Fatalf("pinned ticket: code=%d body=%q", rec.Code, rec.Body.String())
	}
	// Conflicting ?part=0 on a ticket pinned to part 1 is rejected with 404 ticket_invalid.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+pinned+"?part=0", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("X-Ether-Media-Error") != "ticket_invalid" {
		t.Fatalf("conflicting part query: code=%d hdr=%q body=%s", rec.Code, rec.Header().Get("X-Ether-Media-Error"), rec.Body.String())
	}
}

func TestMediaHandlerPreparingReturns503RetryAfterWhileNoSourceStays409(t *testing.T) {
	for _, tc := range []struct {
		name       string
		openErr    error
		wantStatus int
		wantCode   string
		wantRetry  string
	}{
		{
			name:       "preparing with explicit retryAfter",
			openErr:    &core.PreparingError{RetryAfter: 7 * time.Second},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "preparing",
			wantRetry:  "7",
		},
		{
			name:       "preparing sentinel defaults to 5s",
			openErr:    core.ErrPreparing,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "preparing",
			wantRetry:  "5",
		},
		{
			name:       "real no_source stays 409",
			openErr:    core.ErrNotAvailable,
			wantStatus: http.StatusConflict,
			wantCode:   "no_source",
			wantRetry:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer := NewTicketIssuer()
			handler := NewMediaHandler(MediaDeps{
				Tickets: issuer,
				Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
					return viewerWithLibraries(id, "Reader", core.LibraryMovies), nil
				},
				Open: func(_ context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
					return nil, tc.openErr
				},
			})
			token, err := issuer.Issue(StreamGrant{ViewerID: 7, ItemID: "plug:kind:item", Adapter: "feed"})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KindMediaPath+token, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Retry-After"); got != tc.wantRetry {
				t.Fatalf("Retry-After = %q, want %q", got, tc.wantRetry)
			}
			if got := rec.Header().Get("X-Ether-Media-Error"); got != tc.wantCode {
				t.Fatalf("X-Ether-Media-Error = %q, want %q", got, tc.wantCode)
			}
			var wire struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil || wire.Error.Code != tc.wantCode {
				t.Fatalf("json body = %s, err = %v, want code %q", rec.Body.String(), err, tc.wantCode)
			}
		})
	}
}
