package playback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// slowProbeHarness lands the warm probe after probeAt (the resolver is the
// slow step of the warm-up).
func slowProbeHarness(t *testing.T, probeAt time.Duration) *harness {
	t.Helper()
	h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
	h.resolver.mu.Lock()
	h.resolver.delay = probeAt
	h.resolver.mu.Unlock()
	return h
}

func TestProbeMetadataWaitsUntilCallerDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		probeAt time.Duration
	}{
		{"probe-at-1s", time.Second},
		// Past the 1.5 s default a caller deadline still owns the wait.
		{"probe-at-2s", 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := slowProbeHarness(t, tc.probeAt)
			caps := defaultCaps("android")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := h.m.ProbeMetadata(ctx, owner, movieKey, caps); err != nil {
				t.Fatalf("probe landing inside the deadline returned %v", err)
			}
			if info, ok := h.m.WarmProbeInfo(owner, caps, movieKey); !ok || info.Estimated || len(info.Streams) == 0 {
				t.Fatalf("probed tracks not adopted: ok=%v info=%+v", ok, info)
			}
		})
	}
}

// expiredCtx reports DeadlineExceeded from Err as soon as its deadline has
// passed, so the outcome does not depend on whether the context's own timer
// or the wait's timer fires first.
type expiredCtx struct{ context.Context }

func (c expiredCtx) Err() error {
	if err := c.Context.Err(); err != nil {
		return err
	}
	if dl, ok := c.Deadline(); ok && !time.Now().Before(dl) {
		return context.DeadlineExceeded
	}
	return nil
}

func TestProbeMetadataDeadlineEndsWaitAsSlowAndKeepsWarmup(t *testing.T) {
	t.Parallel()
	h := slowProbeHarness(t, 1200*time.Millisecond)
	caps := defaultCaps("android")
	tctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ctx := expiredCtx{tctx}
	err := h.m.ProbeMetadata(ctx, owner, movieKey, caps)
	if !errors.Is(err, core.ErrPrepareSlow) {
		t.Fatalf("deadline shorter than the probe returned %v, want ErrPrepareSlow", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline end leaked as %v", err)
	}
	// The warm-up is not cancelled: it completes and later reads adopt it.
	waitWarm(t, h, owner.ID, caps.ClientID, movieKey)
	if info, ok := h.m.WarmProbeInfo(owner, caps, movieKey); !ok || info.Estimated || len(info.Streams) == 0 {
		t.Fatalf("warm-up not adopted after the caller gave up: ok=%v info=%+v", ok, info)
	}
	if r, p := counts(h); r != 1 || p != 1 {
		t.Fatalf("warm-up repeated: %d resolves, %d probes", r, p)
	}
}

func TestProbeMetadataPastDeadlineStillSchedulesWarmup(t *testing.T) {
	t.Parallel()
	h := slowProbeHarness(t, time.Second)
	caps := defaultCaps("android")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer cancel()
	err := h.m.ProbeMetadata(ctx, owner, movieKey, caps)
	if !errors.Is(err, core.ErrPrepareSlow) {
		t.Fatalf("past deadline returned %v, want ErrPrepareSlow", err)
	}
	// The warm-up is scheduled even though no wait remained.
	waitWarm(t, h, owner.ID, caps.ClientID, movieKey)
	if info, ok := h.m.WarmProbeInfo(owner, caps, movieKey); !ok || info.Estimated || len(info.Streams) == 0 {
		t.Fatalf("warm-up not scheduled for a past deadline: ok=%v info=%+v", ok, info)
	}
	if r, p := counts(h); r != 1 || p != 1 {
		t.Fatalf("past deadline: %d resolves, %d probes, want 1/1", r, p)
	}
}

func TestProbeMetadataWithoutDeadlineKeepsDefaultWait(t *testing.T) {
	t.Parallel()
	h := slowProbeHarness(t, 2*time.Second)
	caps := defaultCaps("desktop")
	err := h.m.ProbeMetadata(context.Background(), owner, movieKey, caps)
	if !errors.Is(err, core.ErrPrepareSlow) {
		t.Fatalf("probe at 2s without deadline returned %v, want ErrPrepareSlow", err)
	}
	waitWarm(t, h, owner.ID, caps.ClientID, movieKey)
	if _, ok := h.m.WarmProbeInfo(owner, caps, movieKey); !ok {
		t.Fatal("warm-up not adopted after the default wait ended")
	}
}

func TestProbeMetadataCancellationReturnsContextError(t *testing.T) {
	t.Parallel()
	h := slowProbeHarness(t, time.Second)
	caps := defaultCaps("android")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cctx, stop := context.WithCancel(ctx)
	time.AfterFunc(100*time.Millisecond, stop)
	if err := h.m.ProbeMetadata(cctx, owner, movieKey, caps); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait returned %v, want context.Canceled", err)
	}
	// Cancelling the caller does not cancel the warm-up.
	waitWarm(t, h, owner.ID, caps.ClientID, movieKey)
	if _, ok := h.m.WarmProbeInfo(owner, caps, movieKey); !ok {
		t.Fatal("warm-up lost after the caller cancelled")
	}
}

func TestProbeMetadataDisabledWarmupDoesNoSourceIOUnderDeadline(t *testing.T) {
	for _, mode := range []string{"detail-off", "speculation-off", "cache-off"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			h := slowProbeHarness(t, time.Second)
			cfg := h.m.config()
			switch mode {
			case "detail-off":
				cfg.Cache.DetailWarmup = "off"
			case "speculation-off":
				cfg.Cache.DisableSpeculation = true
			case "cache-off":
				cfg.Cache.Mode = "off"
			}
			h.m.SetConfig(cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := h.m.ProbeMetadata(ctx, owner, movieKey, defaultCaps("android")); !errors.Is(err, core.ErrPrepareSlow) {
				t.Fatalf("disabled warm-up returned %v, want ErrPrepareSlow", err)
			}
			if r, p := counts(h); r != 0 || p != 0 {
				t.Fatalf("disabled warm-up resolved/probed: %d/%d", r, p)
			}
		})
	}
}
