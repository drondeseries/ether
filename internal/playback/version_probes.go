package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
	"github.com/braedonsaunders/ether/internal/vault"
)

func init() {
	// Durable cross-viewer, cross-indexer container probe cache keyed by
	// release content identity (W116 nzb:<sha256>, bt:<infoHash>, pre:...).
	store.Register("playback_release_probe", 1, `CREATE TABLE release_probes (
	content_key TEXT PRIMARY KEY,
	info_json TEXT NOT NULL,
	updated_at BIGINT NOT NULL
);
CREATE INDEX release_probes_updated ON release_probes(updated_at);`)
}

const (
	maxReleaseProbeCache   = 8192
	versionProbeCooldown   = 5 * time.Minute
	versionProbePerTimeout = 10 * time.Second
)

type versionProbeState struct {
	running bool
	doneAt  time.Time
}

func releaseProbeContentKeys(rel core.Release, rawPayload []byte, contentIDOf func(string) string) []string {
	var out []string
	add := func(k string) {
		if k == "" {
			return
		}
		for _, existing := range out {
			if existing == k {
				return
			}
		}
		out = append(out, k)
	}
	if len(rawPayload) > 0 && !bytes.HasPrefix(rawPayload, []byte(debridPayloadPrefix)) {
		if cid := nzb.ContentID(rawPayload); cid != "" {
			add(rel.ScopedContentID(cid))
		}
	}
	if vk := vault.KeyOf(rel); vk != "" {
		add(rel.ScopedContentID(string(vk)))
	}
	if contentIDOf != nil && rel.ID != "" {
		if cid := contentIDOf(rel.ID); cid != "" {
			add(rel.ScopedContentID(cid))
		}
	}
	for _, k := range rel.ProbeContentKeys() {
		add(k)
	}
	return out
}

func vaultTrackTarget(rel core.Release, rawPayload []byte, contentIDOf func(string) string) (vault.Key, int, int) {
	var vk vault.Key
	switch {
	case vault.KeyOf(rel) != "":
		vk = vault.KeyOf(rel)
	case strings.HasPrefix(rel.ContentID, nzb.ContentIDPrefix):
		vk = vault.Key(rel.ContentID)
	case len(rawPayload) > 0 && !bytes.HasPrefix(rawPayload, []byte(debridPayloadPrefix)):
		if cid := nzb.ContentID(rawPayload); cid != "" {
			vk = vault.Key(cid)
		}
	case rel.Protocol == core.ProtocolTorrent && strings.TrimSpace(rel.InfoHash) != "":
		vk = vault.TorrentKey(rel.InfoHash)
	case contentIDOf != nil && rel.ID != "":
		if cid := contentIDOf(rel.ID); strings.HasPrefix(cid, nzb.ContentIDPrefix) {
			vk = vault.Key(cid)
		}
	}
	if vk == "" {
		return "", 0, 0
	}
	if rel.Info.FullSeason && rel.Target != nil && rel.Target.Episode > 0 {
		return vk, rel.Target.Season, rel.Target.Episode
	}
	return vk, 0, 0
}

func (m *Manager) preloadReleaseProbes() {
	if m.deps.DB == nil {
		return
	}
	rows, err := m.deps.DB.Query(`SELECT content_key, info_json FROM release_probes ORDER BY updated_at DESC LIMIT ?`, maxReleaseProbeCache+1)
	if err != nil {
		return
	}
	defer rows.Close()
	loaded := make(map[string]core.MediaInfo)
	count := 0
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			continue
		}
		count++
		if count > maxReleaseProbeCache {
			break
		}
		if len(raw) > maxPersistedProbeBytes {
			continue
		}
		if info, ok := decodePersistedProbe(raw); ok {
			if facts, ok := persistedProbeFacts(info); ok {
				loaded[key] = facts
			}
		}
	}
	if rows.Err() != nil {
		return
	}
	m.releaseProbeMu.Lock()
	m.releaseProbes = loaded
	m.releaseProbesLoaded = count <= maxReleaseProbeCache
	m.releaseProbeMu.Unlock()
}

// noteReleaseProbeMemory updates the in-memory content-ID probe cache without
// performing any disk or database I/O (safe on the play path).
func (m *Manager) noteReleaseProbeMemory(rel core.Release, rawPayload []byte, info core.MediaInfo) (core.MediaInfo, []string, bool) {
	facts, ok := persistedProbeFacts(info)
	if !ok {
		return core.MediaInfo{}, nil, false
	}
	keys := releaseProbeContentKeys(rel, rawPayload, m.deps.ContentIDOf)
	if len(keys) == 0 {
		return core.MediaInfo{}, nil, false
	}
	m.releaseProbeMu.Lock()
	if m.releaseProbes == nil {
		m.releaseProbes = make(map[string]core.MediaInfo)
	}
	if m.releaseProbeKeys == nil {
		m.releaseProbeKeys = make(map[string]string)
	}
	for _, k := range keys {
		if _, exists := m.releaseProbes[k]; !exists && len(m.releaseProbes) >= maxReleaseProbeCache {
			m.releaseProbesLoaded = false
			continue
		}
		m.releaseProbes[k] = core.CloneMediaInfo(facts)
	}
	if rel.ID != "" && len(m.releaseProbeKeys) < maxReleaseProbeCache {
		m.releaseProbeKeys[rel.ID] = keys[0]
	}
	m.releaseProbeMu.Unlock()
	return facts, keys, true
}

// persistReleaseProbe writes sanitized probe facts durably to release_probes
// and, when vaulted, to vault.TrackStore.
func (m *Manager) persistReleaseProbe(ctx context.Context, rel core.Release, rawPayload []byte, info core.MediaInfo) {
	facts, keys, ok := m.noteReleaseProbeMemory(rel, rawPayload, info)
	if !ok {
		return
	}
	if m.deps.DB != nil {
		b, err := json.Marshal(probeFactsRecord{FactsVersion: core.ProbeFactsVersion, Info: facts})
		if err == nil && len(b) <= maxPersistedProbeBytes {
			now := m.now().UnixMilli()
			dbCtx := context.WithoutCancel(ctx)
			for _, k := range keys {
				_, _ = m.deps.DB.ExecContext(dbCtx, `INSERT INTO release_probes(content_key, info_json, updated_at) VALUES(?, ?, ?)
ON CONFLICT(content_key) DO UPDATE SET info_json=excluded.info_json, updated_at=excluded.updated_at`,
					k, string(b), now)
			}
		}
	}
	if ts, ok := m.deps.Vault.(vault.TrackStore); ok {
		if vk, season, episode := vaultTrackTarget(rel, rawPayload, m.deps.ContentIDOf); vk != "" {
			_ = ts.SetTracks(context.WithoutCancel(ctx), vk, season, episode, facts)
		}
	}
}

// RecordReleaseProbe caches probed container facts in memory and durably by
// content identity (and into the NZB vault's TrackStore when vaulted).
func (m *Manager) RecordReleaseProbe(ctx context.Context, rel core.Release, rawPayload []byte, info core.MediaInfo) {
	m.persistReleaseProbe(ctx, rel, rawPayload, info)
}

// CachedReleaseProbe returns probed MediaInfo for rel from in-memory caches
// (durable release_probes preload and vault TrackStore) with zero request-path
// database or network I/O.
func (m *Manager) CachedReleaseProbe(rel core.Release) (core.MediaInfo, bool) {
	if m == nil {
		return core.MediaInfo{}, false
	}
	keys := releaseProbeContentKeys(rel, nil, m.deps.ContentIDOf)
	m.releaseProbeMu.RLock()
	for _, k := range keys {
		if info, ok := m.releaseProbes[k]; ok {
			m.releaseProbeMu.RUnlock()
			return core.CloneMediaInfo(info), true
		}
	}
	if rel.ID != "" {
		if aliasKey := m.releaseProbeKeys[rel.ID]; aliasKey != "" {
			if info, ok := m.releaseProbes[aliasKey]; ok {
				m.releaseProbeMu.RUnlock()
				return core.CloneMediaInfo(info), true
			}
		}
	}
	complete := m.releaseProbesLoaded
	m.releaseProbeMu.RUnlock()

	if ts, ok := m.deps.Vault.(vault.TrackStore); ok {
		if vk, season, episode := vaultTrackTarget(rel, nil, m.deps.ContentIDOf); vk != "" {
			if info, hit, err := ts.Tracks(context.Background(), vk, season, episode); err == nil && hit {
				return core.CloneMediaInfo(info), true
			}
		}
	}
	if !complete && m.deps.DB != nil && len(keys) > 0 {
		for _, k := range keys {
			var raw string
			if err := m.deps.DB.QueryRow(`SELECT info_json FROM release_probes WHERE content_key=?`, k).Scan(&raw); err == nil && len(raw) <= maxPersistedProbeBytes {
				if info, ok := decodePersistedProbe(raw); ok {
					if facts, ok := persistedProbeFacts(info); ok {
						return facts, true
					}
				}
			}
		}
	}
	return core.MediaInfo{}, false
}

// enrichEstimatedFromDebridMeta merges concrete languages or size learned from
// an already-fetched debrid file path into an estimated variant without
// pretending the container itself was probed.
func enrichEstimatedFromDebridMeta(v *core.ReleaseVariant, filePath string, size int64) {
	if v == nil || !v.Info.Estimated {
		return
	}
	if size > 0 && v.Info.Size <= 0 {
		v.Info.Size = size
	}
	if filePath == "" {
		return
	}
	parsed := releaseparse.Parse(filePath)
	var extra []string
	for _, lang := range parsed.Languages {
		if lang == "" || lang == releaseparse.LanguageMulti || lang == releaseparse.LanguageDual {
			continue
		}
		code := langtag.CanonicalBCP47(lang)
		if code == "" {
			continue
		}
		dup := false
		for _, e := range extra {
			if e == code {
				dup = true
				break
			}
		}
		if !dup {
			extra = append(extra, code)
		}
	}
	if len(extra) == 0 {
		return
	}
	existing := map[string]bool{}
	var template core.StreamInfo
	hasTemplate := false
	maxIdx := -1
	unknownIdx := -1
	for i, st := range v.Info.Streams {
		if st.Index > maxIdx {
			maxIdx = st.Index
		}
		if st.Type != "audio" {
			continue
		}
		if !hasTemplate {
			template = st
			hasTemplate = true
		}
		if st.Language == "" {
			if unknownIdx < 0 {
				unknownIdx = i
			}
			continue
		}
		if c := langtag.CanonicalBCP47(releaseparse.LanguageName(st.Language)); c != "" {
			existing[c] = true
		}
		existing[strings.ToLower(st.Language)] = true
	}
	for _, iso := range extra {
		if existing[iso] {
			continue
		}
		existing[iso] = true
		if unknownIdx >= 0 {
			v.Info.Streams[unknownIdx].Language = iso
			unknownIdx = -1
			continue
		}
		maxIdx++
		st := core.StreamInfo{
			Index:    maxIdx,
			Type:     "audio",
			Language: iso,
		}
		if hasTemplate {
			st.Codec = template.Codec
			st.Channels = template.Channels
			st.ChannelLayout = template.ChannelLayout
			st.Profile = template.Profile
			st.BitrateKbps = template.BitrateKbps
		} else if parsed.AudioCodec != "" {
			st.Codec = parsed.AudioCodec
		}
		v.Info.Streams = append(v.Info.Streams, st)
	}
}

// BackgroundBusy reports how many background version-probe runs are active.
func (m *Manager) BackgroundBusy() int {
	if m == nil {
		return 0
	}
	m.versionProbeMu.Lock()
	defer m.versionProbeMu.Unlock()
	n := 0
	for _, st := range m.versionProbes {
		if st.running {
			n++
		}
	}
	return n
}

func (m *Manager) isBackgroundBusy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed || m.draining || m.hasActivePlaybackLocked()
}

func (m *Manager) cancelVersionProbes() {
	m.versionProbeMu.Lock()
	cancel := m.versionProbeCancel
	m.versionProbeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) closeVersionProbes() {
	m.versionProbeMu.Lock()
	m.versionProbeClosed = true
	cancel := m.versionProbeCancel
	m.versionProbeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// scheduleVersionProbes launches a single low-priority, bounded container
// header probe pass for a title's Play Version menu entries when a viewer opens
// the title's details page. It never searches indexers, never grabs NZBs, runs
// at most one probe at a time per title (and globally), and yields immediately
// to active playback.
func (m *Manager) scheduleVersionProbes(user *core.User, key core.RatingKey, caps core.ClientCaps) {
	if m == nil || user == nil || key == 0 {
		return
	}
	if m.deps.OpenVaulted == nil && m.deps.OpenCachedDebrid == nil {
		return
	}
	if m.deps.Catalog == nil || m.deps.Resolver == nil {
		return
	}
	if _, ok := m.deps.Resolver.(core.VariantResolver); !ok {
		return
	}
	cfg := m.config().Cache
	if cfg.DisableSpeculation || cfg.Mode == "off" || cfg.DetailWarmup == "off" {
		return
	}
	if m.isBackgroundBusy() {
		return
	}

	now := m.now()
	m.versionProbeMu.Lock()
	if m.versionProbeClosed {
		m.versionProbeMu.Unlock()
		return
	}
	if m.versionProbeSem == nil {
		m.versionProbeSem = make(chan struct{}, 1)
	}
	if m.versionProbes == nil {
		m.versionProbes = make(map[core.RatingKey]*versionProbeState)
	}
	st := m.versionProbes[key]
	if st != nil && (st.running || (!st.doneAt.IsZero() && now.Sub(st.doneAt) < versionProbeCooldown)) {
		m.versionProbeMu.Unlock()
		return
	}
	if st == nil {
		st = &versionProbeState{}
		m.versionProbes[key] = st
	}
	st.running = true
	m.versionProbeWG.Add(1)
	m.versionProbeMu.Unlock()

	u := *user
	go func() {
		defer m.versionProbeWG.Done()
		completed := false
		defer func() {
			m.versionProbeMu.Lock()
			if cur := m.versionProbes[key]; cur != nil {
				cur.running = false
				if completed {
					cur.doneAt = m.now()
				}
			}
			m.versionProbeMu.Unlock()
		}()
		completed = m.runVersionProbes(&u, key, caps)
	}()
}

func (m *Manager) runVersionProbes(user *core.User, key core.RatingKey, caps core.ClientCaps) bool {
	// Acquire the single global background header-probe slot, yielding if
	// playback starts or the manager closes while waiting.
	select {
	case m.versionProbeSem <- struct{}{}:
		defer func() { <-m.versionProbeSem }()
	case <-m.ctx.Done():
		return false
	default:
		// Another title's header probe is already running; do not queue a
		// backlog of background probes.
		return false
	}
	if m.isBackgroundBusy() {
		return false
	}

	runCtx, cancel := context.WithCancel(m.ctx)
	m.versionProbeMu.Lock()
	m.versionProbeCancel = cancel
	m.versionProbeMu.Unlock()
	defer func() {
		cancel()
		m.versionProbeMu.Lock()
		m.versionProbeCancel = nil
		m.versionProbeMu.Unlock()
	}()

	// Check once more after publishing cancel so a concurrent reserveLocked
	// cannot race past us.
	if m.isBackgroundBusy() {
		return false
	}

	localCtx := core.WithLocalResponse(runCtx)
	title, err := m.deps.Catalog.Get(localCtx, key)
	if err != nil || title == nil {
		return false
	}
	vr, ok := m.deps.Resolver.(core.VariantResolver)
	if !ok {
		return false
	}
	// Variants reads only the resolver's search cache (never queries indexers)
	// and returns concrete releases rather than logical placeholder slots.
	variants, err := vr.Variants(runCtx, core.ResolveRequest{
		Title:  title,
		User:   user,
		Client: caps,
	}, core.MaxVariants)
	if err != nil || len(variants) == 0 {
		return false
	}
	if len(variants) > core.MaxVariants {
		variants = variants[:core.MaxVariants]
	}

	budget := m.deps.HeaderProbeBudget
	if budget <= 0 {
		budget = transcode.DefaultHeaderProbeBudget
	}

	for _, v := range variants {
		if runCtx.Err() != nil || m.isBackgroundBusy() {
			return false
		}
		rel := v.Release
		if rel.ID == "" {
			continue
		}
		if _, hit := m.CachedReleaseProbe(rel); hit {
			continue
		}
		if _, hit := m.CachedVariantInfo(runCtx, user.ID, key, rel.ID); hit {
			continue
		}
		if !m.probeOneVersionHeader(runCtx, key, rel, budget) {
			if runCtx.Err() != nil || m.isBackgroundBusy() {
				return false
			}
		}
	}
	return true
}

func (m *Manager) probeOneVersionHeader(ctx context.Context, key core.RatingKey, rel core.Release, budget int) bool {
	bgCtx := ctx
	if !core.IsBackgroundCheck(bgCtx) {
		bgCtx = core.WithBackgroundBudget(bgCtx, func(context.Context, string, string, int64) error { return nil })
	}
	probeCtx, cancel := context.WithTimeout(bgCtx, versionProbePerTimeout)
	defer cancel()

	var (
		src        core.MediaSource
		rawPayload []byte
		err        error
	)
	switch rel.Protocol {
	case core.ProtocolTorrent, core.ProtocolHTTP:
		if m.deps.OpenCachedDebrid == nil {
			return false
		}
		src, err = m.deps.OpenCachedDebrid(probeCtx, rel)
	case core.ProtocolUsenet:
		if m.deps.OpenVaulted == nil {
			return false
		}
		src, rawPayload, err = m.deps.OpenVaulted(probeCtx, rel)
	default:
		return false
	}
	if err != nil || src == nil {
		return false
	}
	defer func() { _ = src.Close() }()

	if probeCtx.Err() != nil || m.isBackgroundBusy() {
		return false
	}
	size := src.Size()
	if size <= 0 {
		return false
	}
	reader := &budgetedHeaderReader{
		src:   src,
		yield: m.isBackgroundBusy,
	}
	reader.remaining.Store(int64(budget))

	info, err := transcode.ProbeHeader(probeCtx, reader, size, budget)
	if err != nil || info == nil || probeCtx.Err() != nil || m.isBackgroundBusy() {
		return false
	}
	mi := core.CloneMediaInfo(*info)
	if mi.Size <= 0 {
		mi.Size = size
	}
	if mi.Container == "" {
		mi.Container = containerFromName(src.Name())
	}
	mi.Estimated = false

	m.persistReleaseProbe(ctx, rel, rawPayload, mi)
	if m.deps.OnVersionProbed != nil {
		m.deps.OnVersionProbed(key, rel)
	}
	return true
}

type prefetchReaderAt interface {
	ReadAtPrefetchContext(ctx context.Context, p []byte, off int64) (int, error)
}

type metadataReaderAt interface {
	ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error)
}

type budgetedHeaderReader struct {
	src       core.MediaSource
	remaining atomic.Int64
	yield     func() bool
}

func (r *budgetedHeaderReader) ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if r.yield != nil && r.yield() {
		return 0, context.Canceled
	}
	if r.remaining.Add(-int64(len(p))) < 0 {
		return 0, transcode.ErrProbeIncomplete
	}
	var (
		n   int
		err error
	)
	if pr, ok := r.src.(prefetchReaderAt); ok {
		n, err = pr.ReadAtPrefetchContext(ctx, p, off)
	} else if mr, ok := r.src.(metadataReaderAt); ok {
		n, err = mr.ReadMetadataAtContext(ctx, p, off)
	} else {
		n, err = r.src.ReadAt(p, off)
	}
	if r.yield != nil && r.yield() {
		return 0, context.Canceled
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return n, err
	}
	return n, err
}

func (r *budgetedHeaderReader) ReadAt(p []byte, off int64) (int, error) {
	return r.ReadMetadataAtContext(context.Background(), p, off)
}
