package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// The console returns a bounded device window with exact latest-play details,
// even when friendly names collide and history contains older plays.
func TestSeenPlaybackDevicesBounded(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Clients.Overrides = []core.ClientCapabilityOverride{{Platform: "Android", Device: "Box", Force: true, DolbyVisionProfiles: []int{5, 7, 8}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 102; i++ {
		client := fmt.Sprintf("client-%d", i)
		play := &core.PlaySession{ID: fmt.Sprintf("play-%d", i), UserID: 1, Key: 99, BytesServed: 100 << 20, Username: client, StartedAt: time.Now(), Client: core.ClientCaps{ClientID: client, DeviceName: "Living room", Device: "Box", Platform: "Android", Product: "Plex for Android (TV)"}}
		if err := a.stats.Record(ctx, play, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := a.seenPlaybackDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 100 {
		t.Fatalf("got %d devices", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Device != "Box" || row.Name != "Living room" || row.App != "Plex for Android (TV)" || row.Viewer != row.ClientIdentifier || row.OverrideIndex == nil || *row.OverrideIndex != 0 {
			t.Fatalf("latest play identity lost: %+v", row)
		}
		if seen[row.ClientIdentifier] {
			t.Fatal("duplicate device")
		}
		seen[row.ClientIdentifier] = true
	}
}
