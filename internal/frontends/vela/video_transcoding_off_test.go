package vela

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestFailVideoTranscodingOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"bare", frontend.ErrVideoTranscodingDisabled},
		{"wrapped_no_compatible_path", &frontend.NoCompatiblePathError{
			RejectedPaths: []frontend.RejectedPlaybackPath{{Path: "video", Reasons: []string{frontend.ErrVideoTranscodingDisabled.Error()}}},
			Cause:         frontend.ErrVideoTranscodingDisabled,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			fail(rec, tc.err)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", rec.Code)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "no_compatible_path" || !strings.Contains(body.Error.Message, "turned off") {
				t.Fatalf("error body = %+v", body.Error)
			}
		})
	}
}
