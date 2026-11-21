package plex

import (
	"fmt"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestPlaybackMessageVideoTranscodingOff(t *testing.T) {
	msg := playbackMessage(fmt.Errorf("plan: %w", frontend.ErrVideoTranscodingDisabled))
	if msg != msgVideoTranscodingOff || !strings.Contains(msg, "video transcode") || !strings.Contains(msg, "turned off") {
		t.Fatalf("playbackMessage(ErrVideoTranscodingDisabled) = %q", msg)
	}
}
