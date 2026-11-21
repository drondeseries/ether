package silo

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestPlaybackErrorVideoTranscodingOff(t *testing.T) {
	rec := httptest.NewRecorder()
	playbackError(rec, frontend.ErrVideoTranscodingDisabled)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "turned off") {
		t.Fatalf("playbackError = %d %q", rec.Code, rec.Body.String())
	}
}
