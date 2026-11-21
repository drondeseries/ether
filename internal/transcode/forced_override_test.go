package transcode

import (
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Forced edits are sparse: neither untouched runtime facts nor declarations
// belonging to other identities may be replaced by the registry baseline.
func TestForcedOverridePrecedence(t *testing.T) {
	on := true
	c := core.ClientCaps{Product: "Plex for Android (TV)", Platform: "Android", Device: "Xiaomi TV Box", VideoCodecs: []string{"hevc"}, AudioCodecs: []string{"truehd"}, MaxHeight: 1080, DVProfiles: []int{5, 8}}
	o := core.ClientCapabilityOverride{Platform: "Android", Device: "Xiaomi TV Box", DolbyVision: &on, DolbyVisionProfiles: []int{5, 7, 8}}
	normal := ApplyForcedOverrides(c, "", []core.ClientCapabilityOverride{o})
	if !reflect.DeepEqual(normal, c) {
		t.Fatalf("non-forced edit changed declarations: %+v", normal)
	}
	o.Force = true
	got := ApplyForcedOverrides(c, "", []core.ClientCapabilityOverride{o})
	want := c
	want.DolbyVision = true
	want.DVProfiles = []int{5, 7, 8}
	want.DVProfilesForced = true
	want.ForcedOverrideApplied = true
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sparse force: got %+v, want %+v", got, want)
	}
	if !SupportsDVProfile(got, 7) {
		t.Fatal("forced profile 7 unavailable")
	}
	o.Platform = "tvOS"
	if got := ApplyForcedOverrides(c, "", []core.ClientCapabilityOverride{o}); !reflect.DeepEqual(got, c) {
		t.Fatal("edit crossed identity")
	}
	o.Platform = "Android"
	o.DolbyVisionProfiles = []int{}
	got = ApplyForcedOverrides(c, "", []core.ClientCapabilityOverride{o})
	if got.DVProfiles == nil || len(got.DVProfiles) != 0 || len(ClientPolicyFor(got).DolbyVisionProfiles) != 0 {
		t.Fatal("explicit empty profiles lost")
	}
	if !reflect.DeepEqual(c.DVProfiles, []int{5, 8}) {
		t.Fatal("runtime input mutated")
	}
}

func TestShieldForcedDVProfile7DirectPlayAndRemuxStrip(t *testing.T) {
	bl := true
	info := core.MediaInfo{
		Container: "mkv",
		Streams: []core.StreamInfo{
			{
				Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10",
				Width: 3840, Height: 2160, BitDepth: 10, ColorTransfer: "smpte2084",
				DolbyVision: true, DolbyVisionProfile: 7, DolbyVisionCompatibilityID: 6,
				DolbyVisionBLPresent: &bl,
			},
			{Index: 1, Type: "audio", Codec: "eac3", Channels: 6},
		},
	}
	baseCaps := core.ClientCaps{
		Product: "Plex for Android (TV)", Platform: "Android", Device: "SHIELD Android TV",
		Containers: []string{"mkv"}, VideoCodecs: []string{"hevc"}, AudioCodecs: []string{"eac3", "truehd"},
		MaxHeight: 2160, HDR10: true, DolbyVision: true, DVProfiles: []int{5, 8},
	}
	// Audio-only Force must not bypass DV variant checks.
	audioOnly := ApplyForcedOverrides(baseCaps, "", []core.ClientCapabilityOverride{{
		Force: true, Platform: "Android", Device: "SHIELD Android TV", AudioCodecs: []string{"eac3"},
	}})
	if d := Decide(info, audioOnly, core.DecideOptions{DirectPlay: true, DirectStream: true, Protocol: core.ProtocolHLS}, Policy{}); d.Kind != core.DecisionRemux || !d.StripDolbyVision {
		t.Fatalf("audio-only force bypassed P7 check: %+v", d)
	}
	// Explicit Force dolbyVisionProfiles [5,7,8] allows DirectPlay with P7 intact.
	forcedDV := ApplyForcedOverrides(baseCaps, "", []core.ClientCapabilityOverride{{
		Force: true, Platform: "Android", Device: "SHIELD Android TV", DolbyVisionProfiles: []int{5, 7, 8},
	}})
	if d := Decide(info, forcedDV, core.DecideOptions{DirectPlay: true, DirectStream: true, Protocol: core.ProtocolHLS}, Policy{}); d.Kind != core.DecisionDirectPlay || d.StripDolbyVision {
		t.Fatalf("forced P7 did not DirectPlay intact: %+v", d)
	}
	// Remux still strips P7 in segmented HLS for #328 safety.
	if d := Decide(info, forcedDV, core.DecideOptions{DirectPlay: false, DirectStream: true, Protocol: core.ProtocolHLS}, Policy{}); d.Kind != core.DecisionRemux || !d.StripDolbyVision {
		t.Fatalf("forced P7 remux did not strip DV: %+v", d)
	}
}
