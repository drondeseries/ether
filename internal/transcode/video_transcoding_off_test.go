package transcode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestVideoTranscodingOffRejectsEncodeAllowsCopyAndAudioConversion(t *testing.T) {
	cfg := config.Transcode{
		VideoTranscoding:   "off",
		FastStart:          true,
		ABRLadder:          true,
		ABRLadderUnderCopy: true,
		SpikeGovernor:      true,
		RTXVideo:           config.RTXVideo{Mode: "on"},
	}
	m := newTestManager(t, newFakeRunner(), cfg)

	if !m.VideoTranscodingOff() {
		t.Fatal("expected VideoTranscodingOff() to be true")
	}
	if m.ProactiveFallbackAvailable() {
		t.Fatal("ProactiveFallbackAvailable() must be false when video transcoding is off")
	}

	// Non-live video encode is rejected for HLS and HTTP.
	treq := transcodeRequest(t, "encode-hls")
	if _, err := m.StartHLS(context.Background(), treq); !errors.Is(err, ErrVideoTranscodingDisabled) || !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("StartHLS(encode) err = %v, want ErrVideoTranscodingDisabled", err)
	}
	hreq := transcodeRequest(t, "encode-http")
	hreq.Protocol = core.ProtocolHTTP
	if _, err := m.StartHLS(context.Background(), hreq); !errors.Is(err, ErrVideoTranscodingDisabled) {
		t.Fatalf("StartHLS(HTTP encode) err = %v, want ErrVideoTranscodingDisabled", err)
	}

	// Video copy + audio conversion (TrueHD -> EAC3/AAC) still works, with
	// no adaptive encode rungs, no fast-start encode ramp, and no governor.
	rreq := remuxRequest(t, "audio-remux", 10*time.Minute)
	rreq.QualityRungs = true
	rreq.QualityVariants = []core.QualityVariant{{
		Rung:     core.HLSRung{ID: "720p4m", Height: 720, Bandwidth: 4_000_000},
		Decision: core.Decision{Kind: core.DecisionTranscode, VideoCopy: false, VideoCodec: "h264", AudioCodec: "aac", MaxHeight: 720, BitrateKbps: 4000},
	}}
	rreq.QualityRungIDs = []string{"720p4m"}
	if !rreq.Decision.VideoCopy || rreq.Decision.AudioCopy {
		t.Fatalf("expected video copy + audio conversion decision, got %+v", rreq.Decision)
	}
	s := startSession(t, m, rreq)
	defer m.Stop(s.ID())
	if len(s.rungs) != 1 || !s.rungs[0].decision.VideoCopy {
		t.Fatalf("expected only base copy rung when video transcoding is off, got %d rungs (%+v)", len(s.rungs), s.rungs)
	}
	if s.governorOn {
		t.Fatal("spike governor must be disabled when video transcoding is off")
	}
	if s.plan != nil && s.plan.rampSegs > 0 {
		t.Fatalf("fast-start ramp must be disabled when video transcoding is off, got rampSegs=%d", s.plan.rampSegs)
	}

	// Copy recovery helpers must not silently flip to video encoding.
	fakeRun := &run{spec: runSpec{Decision: core.Decision{VideoCopy: true}}}
	if s.recoverCopyLocked(fakeRun, "Error applying bitstream filters") {
		t.Fatal("recoverCopyLocked must not start a video encode when video transcoding is off")
	}
	if s.recoverHeaderLocked(fakeRun) {
		t.Fatal("recoverHeaderLocked must not start a video encode when video transcoding is off")
	}

	// Live TV is unaffected: LiveInput video transcode still starts.
	liveReq := transcodeRequest(t, "live-tv")
	liveReq.LiveInput = true
	liveSess := startSession(t, m, liveReq)
	defer m.Stop(liveSess.ID())
	if liveSess.decision.VideoCopy {
		t.Fatal("expected live TV session to keep video transcode decision")
	}

	// AuthorDistributedRamp must not add an encoded ramp when off.
	rampReq := transcodeRequest(t, "dist-ramp")
	rampReq.Decision.AudioCopy = true
	rampReq.Decision.Container = ContainerFMP4
	if got := AuthorDistributedRamp(rampReq, cfg); got.Ramp.Version != 0 {
		t.Fatalf("AuthorDistributedRamp authored ramp %+v while video transcoding is off", got.Ramp)
	}

	// RTX Video enhancement must be rejected while off.
	info := mustParse(t, "mkv_truehd_atmos.json")
	dec := m.Decide(info, appleTV, allow())
	if enhancementSelected(dec) {
		t.Fatalf("RTX enhancement selected while video transcoding is off: %+v", dec.Enhancement)
	}

	// CheckSwap rejects a replacement that would force a video encode.
	hevcHDR := mustParse(t, "mkv_hevc_hdr10.json")
	if err := s.CheckSwap(hevcHDR); !errors.Is(err, ErrVideoTranscodingDisabled) {
		t.Fatalf("CheckSwap(incompatible) = %v, want ErrVideoTranscodingDisabled", err)
	}
}
