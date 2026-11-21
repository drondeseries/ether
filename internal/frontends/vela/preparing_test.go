package vela

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestVelaPreparingErrorMapsTo503RetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantDelay string
		wantSec   int
	}{
		{"explicit retryAfter", &frontend.PreparingError{RetryAfter: 7 * time.Second}, "7", 7},
		{"bare sentinel defaults to 5s", frontend.ErrPreparing, "5", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			fail(w, tc.err)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != tc.wantDelay {
				t.Fatalf("Retry-After = %q, want %q", got, tc.wantDelay)
			}
			var wire struct {
				Error struct {
					Class, Code, Message string
					Retry                struct {
						Allowed                   bool
						AfterSeconds, MaxAttempts int
					}
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Error.Class != "source" || wire.Error.Code != "preparing" || !wire.Error.Retry.Allowed || wire.Error.Retry.AfterSeconds != tc.wantSec || wire.Error.Retry.MaxAttempts != 3 {
				t.Fatalf("unexpected wire payload: %+v", wire)
			}
		})
	}
}
