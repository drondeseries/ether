package resolver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/json"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// A detail view may label already configured choices, but must not run release
// ranking, namesake lookups or provider checks on the request goroutine. These
// snapshots are populated only when viewed, use no persistent writes, and share
// the resolver's cancellable background lifetime. Admission still revalidates
// every selected source through the normal resolver.
type detailVariantSnapshot struct {
	variants []core.ReleaseVariant
	until    time.Time
	pending  bool
}

func (s *Service) detailVariantRead(req core.ResolveRequest, limit int, slots bool) ([]core.ReleaseVariant, error) {
	if req.Title == nil {
		return nil, nil
	}
	// Gob retains runtime-only identity facts (including alternative titles)
	// that JSON intentionally omits. Serialize once to own the request's slices before starting a worker. The
	// key includes viewer/client/profile facts so snapshots never cross policy.
	var request bytes.Buffer
	err := gob.NewEncoder(&request).Encode(req)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal([]any{request.Bytes(), limit, slots, s.qualityCfg(), s.resolverCfg()})
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256(encoded)
	now := s.now()
	s.bgMu.Lock()
	entry := s.detailVariants[key]
	if entry != nil && now.Before(entry.until) {
		result := cloneDetailVariants(entry.variants)
		s.bgMu.Unlock()
		return result, nil
	}
	if s.bgClosed || (entry != nil && entry.pending) || s.detailVariantWorkers >= 2 {
		s.bgMu.Unlock()
		return nil, nil
	}
	if s.detailVariants == nil {
		s.detailVariants = make(map[[32]byte]*detailVariantSnapshot)
	}
	if len(s.detailVariants) >= 128 {
		for k, v := range s.detailVariants {
			if !v.pending && !now.Before(v.until) {
				delete(s.detailVariants, k)
			}
		}
		if len(s.detailVariants) >= 128 {
			s.bgMu.Unlock()
			return nil, nil
		}
	}
	entry = &detailVariantSnapshot{pending: true}
	s.detailVariants[key] = entry
	s.detailVariantWorkers++
	s.bgWG.Add(1)
	s.bgMu.Unlock()
	var owned core.ResolveRequest
	if err := gob.NewDecoder(bytes.NewReader(request.Bytes())).Decode(&owned); err != nil {
		s.bgMu.Lock()
		delete(s.detailVariants, key)
		s.detailVariantWorkers--
		s.bgMu.Unlock()
		s.bgWG.Done()
		return nil, err
	}
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.bgCtx, 5*time.Second)
		defer cancel()
		var versions []core.ReleaseVariant
		var err error
		if slots {
			versions, err = s.versionSlots(ctx, owned, limit, true)
		} else {
			versions, err = s.variants(ctx, owned, limit)
		}
		s.bgMu.Lock()
		entry.pending = false
		s.detailVariantWorkers--
		if err == nil {
			entry.variants = cloneDetailVariants(versions)
			entry.until = s.now().Add(15 * time.Second)
		} else {
			delete(s.detailVariants, key)
		}
		s.bgMu.Unlock()
	}()
	return nil, nil
}

func cloneDetailVariants(in []core.ReleaseVariant) []core.ReleaseVariant {
	if in == nil {
		return nil
	}
	out := append([]core.ReleaseVariant(nil), in...)
	for i := range out {
		out[i].Info = core.CloneMediaInfo(in[i].Info)
		// Release records are immutable cache entries; only presentation media
		// facts are modified by callers when overlaying a persisted probe.
	}
	return out
}

// InvalidateDetailVariants drops completed cached detail-variant snapshots so
// newly probed container tracks and any updated ranking order appear on the
// next detail read.
func (s *Service) InvalidateDetailVariants() {
	if s == nil {
		return
	}
	s.bgMu.Lock()
	for k, v := range s.detailVariants {
		if !v.pending {
			delete(s.detailVariants, k)
		}
	}
	s.bgMu.Unlock()
}
