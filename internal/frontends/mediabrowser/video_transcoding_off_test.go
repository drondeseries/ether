package mediabrowser

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestPlaybackInfoErrorVideoTranscodingOff(t *testing.T) {
	i := &instance{protocol: "jellyfin"}
	rec := httptest.NewRecorder()
	i.playbackInfoError(rec, "plan", frontend.ErrVideoTranscodingDisabled)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if body["ErrorCode"] != "NoCompatibleStream" {
		t.Fatalf("ErrorCode = %v, want NoCompatibleStream", body["ErrorCode"])
	}
	msg, _ := body["Message"].(string)
	if !strings.Contains(msg, "video transcode") || !strings.Contains(msg, "turned off") {
		t.Fatalf("Message = %q", msg)
	}

	// Emby streamError returns HTTP 422 with the clear viewer message.
	embyRec := httptest.NewRecorder()
	streamError("emby", embyRec, frontend.ErrVideoTranscodingDisabled)
	if embyRec.Code != http.StatusUnprocessableEntity || !strings.Contains(embyRec.Body.String(), "turned off") {
		t.Fatalf("emby streamError = %d %q", embyRec.Code, embyRec.Body.String())
	}
}
