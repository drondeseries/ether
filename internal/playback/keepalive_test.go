package playback

import (
	"testing"
	"time"
)

func TestKeepAlivePreservesViewerLeaseAndClientStop(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 100_000, time.Minute)}, []string{"A"},
		func(_ *harness, o *Options) { o.IdleTimeout = 2 * time.Minute; o.StoppedGrace = time.Minute })
	ps := h.prepare(owner, movieKey, "events")
	idle := h.prepare(owner, movieKey, "no-events")
	s := h.m.lookup(ps.ID)
	s.mu.Lock()
	lastRead, position, state := s.lastRead, s.viewerPosition, s.viewerState
	s.mu.Unlock()
	// An open stream sends heartbeats, but no media reads or observations.
	for range 10 {
		h.clock.Advance(15 * time.Second)
		if !h.m.KeepAlive(ps.ID) {
			t.Fatal("existing session disappeared")
		}
		h.m.expire()
	}
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("heartbeat did not preserve session")
	}
	if _, ok := h.m.Session(idle.ID); ok {
		t.Fatal("session without a stream did not idle")
	}
	s.mu.Lock()
	unchanged := s.lastRead.Equal(lastRead) && s.viewerPosition == position && s.viewerState == state
	s.mu.Unlock()
	if !unchanged {
		t.Fatal("liveness changed media activity or timeline")
	}
	h.m.Timeline(owner.ID, movieKey, "stopped")
	for range 5 {
		h.clock.Advance(15 * time.Second)
		h.m.KeepAlive(ps.ID)
		h.m.expire()
	}
	if _, ok := h.m.Session(ps.ID); ok {
		t.Fatal("open stream cancelled client stop")
	}
	if h.m.KeepAlive(ps.ID) {
		t.Fatal("liveness resurrected ended session")
	}
}
