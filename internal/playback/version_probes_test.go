package playback

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/vault"
)

type variantProbeResolver struct {
	*fakeResolver
	variants   []core.ReleaseVariant
	searchHits atomic.Int64
	grabHits   atomic.Int64
}

func (r *variantProbeResolver) Resolve(ctx context.Context, req core.ResolveRequest) (*core.Resolution, error) {
	r.searchHits.Add(1)
	return r.fakeResolver.Resolve(ctx, req)
}

func (r *variantProbeResolver) Variants(_ context.Context, _ core.ResolveRequest, max int) ([]core.ReleaseVariant, error) {
	out := append([]core.ReleaseVariant(nil), r.variants...)
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, nil
}

type fixtureMediaSource struct {
	id         string
	name       string
	data       []byte
	onRead     func(ctx context.Context) error
	onClose    func()
	closed     atomic.Bool
	prefetch   atomic.Int64
	readEvents chan struct{}
}

func (s *fixtureMediaSource) ReadAt(p []byte, off int64) (int, error) {
	return s.ReadAtPrefetchContext(context.Background(), p, off)
}

func (s *fixtureMediaSource) ReadAtPrefetchContext(ctx context.Context, p []byte, off int64) (int, error) {
	s.prefetch.Add(1)
	if s.readEvents != nil {
		select {
		case s.readEvents <- struct{}{}:
		default:
		}
	}
	if s.onRead != nil {
		if err := s.onRead(ctx); err != nil {
			return 0, err
		}
	}
	return bytes.NewReader(s.data).ReadAt(p, off)
}

func (s *fixtureMediaSource) Size() int64       { return int64(len(s.data)) }
func (s *fixtureMediaSource) Name() string      { return s.name }
func (s *fixtureMediaSource) ReleaseID() string { return s.id }
func (s *fixtureMediaSource) Close() error {
	if s.closed.CompareAndSwap(false, true) && s.onClose != nil {
		s.onClose()
	}
	return nil
}
func (s *fixtureMediaSource) SourceID() string { return s.id }

func loadMKVFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "transcode", "testdata", "nativeprobe", "hevc_hdr10_eac3_ac3_ass.mkv"))
	if err != nil {
		t.Fatalf("read mkv fixture: %v", err)
	}
	return b
}

func TestVersionProbesUseKnownFactsAndPersistByContentID(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	dir := payloadTempDir(t)

	vs, err := vault.Open(ctx, vault.Options{Dir: t.TempDir(), DB: db})
	if err != nil {
		t.Fatal(err)
	}
	sampleNZB := []byte(`<nzb><file subject="Anime.2024.1080p.MULTi.mkv" poster="p"><groups><group>alt.test</group></groups><segments><segment bytes="1024" number="1">a1@test</segment></segments></file></nzb>`)
	cid := nzb.ContentID(sampleNZB)
	vaultKey, err := vs.Put(ctx, vault.PutRequest{
		NZB:     sampleNZB,
		Release: core.Release{ID: "rel-vault-a", IndexerID: "idx-a", Protocol: core.ProtocolUsenet},
	})
	if err != nil {
		t.Fatal(err)
	}

	probedInfo := core.MediaInfo{
		Container: "mkv",
		Duration:  95 * time.Minute,
		Size:      5 << 30,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "eac3", Channels: 6, Language: "jpn", Default: true},
			{Index: 2, Type: "audio", Codec: "eac3", Channels: 6, Language: "eng"},
		},
	}

	// Store tracks ONLY in the vault for vaultKey2 (never written to release_probes)
	// to prove WithCachedVariantInfo reads vault.TrackStore directly.
	sampleNZB2 := []byte(`<nzb><file subject="Anime2.2024.1080p.MULTi.mkv" poster="p"><groups><group>alt.test</group></groups><segments><segment bytes="1024" number="1">b1@test</segment></segments></file></nzb>`)
	vaultKey2, err := vs.Put(ctx, vault.PutRequest{
		NZB:     sampleNZB2,
		Release: core.Release{ID: "rel-vault-only", IndexerID: "idx-b", Protocol: core.ProtocolUsenet},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := vs.(vault.TrackStore).SetTracks(ctx, vaultKey, 0, 0, probedInfo); err != nil {
		t.Fatal(err)
	}
	if err := vs.(vault.TrackStore).SetTracks(ctx, vaultKey2, 0, 0, probedInfo); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, nil, nil)
	cfg := config.Defaults(dir)
	m1, err := New(Deps{
		Resolver:   h.resolver,
		Engine:     h.engine,
		Transcoder: h.tr,
		Catalog:    h.catalog,
		SelectProfile: func(core.ClientCaps, *core.User, string) core.QualityProfile {
			return config.DefaultQuality().Profiles[1]
		},
		DB:    db,
		Vault: vs,
		DebridFileMeta: func(rel core.Release) (string, int64, bool) {
			if rel.ID == "rel-debrid-unprobed" {
				return "Anime.2024.1080p.BluRay.DUAL.ENG.JPN.DDP5.1-GRP.mkv", 4 << 30, true
			}
			return "", 0, false
		},
	}, cfg, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer m1.Close()

	// Record a probe under release A; release B from another indexer shares its ContentID.
	m1.RecordReleaseProbe(ctx, core.Release{
		ID:        "rel-indexer-1",
		Protocol:  core.ProtocolUsenet,
		ContentID: cid,
	}, sampleNZB, probedInfo)

	// Re-open manager to prove durable cross-restart preload by ContentID.
	m2, err := New(Deps{
		Resolver:   h.resolver,
		Engine:     h.engine,
		Transcoder: h.tr,
		Catalog:    h.catalog,
		SelectProfile: func(core.ClientCaps, *core.User, string) core.QualityProfile {
			return config.DefaultQuality().Profiles[1]
		},
		DB:    db,
		Vault: vs,
		DebridFileMeta: func(rel core.Release) (string, int64, bool) {
			if rel.ID == "rel-debrid-unprobed" {
				return "Anime.2024.1080p.BluRay.DUAL.ENG.JPN.DDP5.1-GRP.mkv", 4 << 30, true
			}
			return "", 0, false
		},
	}, cfg, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()

	title := &core.Title{Key: movieKey, Type: core.TypeMovie, Title: "Anime", Language: "ja"}
	otherViewer := &core.User{ID: 77, Username: "guest"}
	variants := []core.ReleaseVariant{
		{
			ID:      "rel-indexer-2",
			Auto:    true,
			Release: core.Release{ID: "rel-indexer-2", Protocol: core.ProtocolUsenet, ContentID: cid},
			Info:    core.MediaInfo{Estimated: true, Streams: []core.StreamInfo{{Index: 1, Type: "audio", Language: "ja"}}},
		},
		{
			ID:      "rel-debrid-unprobed",
			Release: core.Release{ID: "rel-debrid-unprobed", Protocol: core.ProtocolTorrent, InfoHash: "0123456789abcdef0123456789abcdef01234567"},
			Info:    core.MediaInfo{Estimated: true, Streams: []core.StreamInfo{{Index: 1, Type: "audio", Codec: "eac3", Language: "ja"}, {Index: 2, Type: "audio", Codec: "eac3", Language: ""}}},
		},
		{
			ID:      "vault-" + string(vaultKey2),
			Release: core.Release{ID: "vault-" + string(vaultKey2), GUID: "vault:" + string(vaultKey2), IndexerID: vault.IndexerID, Protocol: core.ProtocolUsenet},
			Info:    core.MediaInfo{Estimated: true, Streams: []core.StreamInfo{{Index: 1, Type: "audio", Language: "ja"}}},
		},
	}

	got := m2.WithCachedVariantInfo(ctx, otherViewer, title, variants)
	if got[0].Info.Estimated {
		t.Fatal("variant matching durable ContentID probe stayed estimated")
	}
	if got[2].Info.Estimated {
		t.Fatal("variant matching vault TrackStore stayed estimated")
	}
	var langs0 []string
	for _, st := range got[0].Info.Streams {
		if st.Type == "audio" {
			langs0 = append(langs0, st.Language)
		}
	}
	if len(langs0) != 2 || langs0[0] != "jpn" || langs0[1] != "eng" {
		t.Fatalf("cross-viewer ContentID variant audio = %v, want [jpn eng]", langs0)
	}

	// Unprobed debrid variant picks up concrete languages from already-fetched file path.
	if !got[1].Info.Estimated {
		t.Fatal("debrid filename metadata should remain estimated until container-probed")
	}
	var langs1 []string
	for _, st := range got[1].Info.Streams {
		if st.Type == "audio" {
			langs1 = append(langs1, st.Language)
		}
	}
	hasEn := false
	for _, l := range langs1 {
		if l == "en" {
			hasEn = true
		}
	}
	if !hasEn {
		t.Fatalf("debrid file metadata did not surface English track: %v", langs1)
	}
}

func TestDetailVersionHeaderProbeFreeBoundedAndYieldsToPlayback(t *testing.T) {
	ctx := context.Background()
	mkvBytes := loadMKVFixture(t)
	db := store.OpenTest(t)
	dir := payloadTempDir(t)

	vaultRel := core.Release{
		ID:        "rel-vaulted",
		Protocol:  core.ProtocolUsenet,
		ContentID: "nzb:1111111111111111111111111111111111111111111111111111111111111111",
		Name:      "Anime.2024.1080p.MULTi-GRP",
	}
	debridRel := core.Release{
		ID:       "rel-debrid-cached",
		Protocol: core.ProtocolTorrent,
		InfoHash: "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Name:     "Anime.2024.1080p.Dual-Audio-GRP",
	}
	unvaultedRel := core.Release{
		ID:       "rel-unvaulted-usenet",
		Protocol: core.ProtocolUsenet,
		Name:     "Anime.2024.1080p.MULTi-OTHER",
	}

	h := newHarness(t, nil, nil)
	res := &variantProbeResolver{
		fakeResolver: h.resolver,
		variants: []core.ReleaseVariant{
			{ID: vaultRel.ID, Auto: true, Release: vaultRel, Info: core.MediaInfo{Estimated: true}},
			{ID: debridRel.ID, Release: debridRel, Info: core.MediaInfo{Estimated: true}},
			{ID: unvaultedRel.ID, Release: unvaultedRel, Info: core.MediaInfo{Estimated: true}},
		},
	}

	var (
		mu             sync.Mutex
		inFlight       int
		maxConcurrent  int
		probedReleases []string
	)
	probedCh := make(chan string, 4)

	openTracked := func(id string) core.MediaSource {
		mu.Lock()
		inFlight++
		if inFlight > maxConcurrent {
			maxConcurrent = inFlight
		}
		mu.Unlock()
		return &fixtureMediaSource{
			id:   id,
			name: id + ".mkv",
			data: mkvBytes,
			onClose: func() {
				mu.Lock()
				inFlight--
				mu.Unlock()
			},
		}
	}

	cfg := config.Defaults(dir)
	m, err := New(Deps{
		Resolver:   res,
		Engine:     h.engine,
		Transcoder: h.tr,
		Catalog:    h.catalog,
		SelectProfile: func(core.ClientCaps, *core.User, string) core.QualityProfile {
			return config.DefaultQuality().Profiles[1]
		},
		DB: db,
		FetchNZB: func(context.Context, core.Release) ([]byte, error) {
			res.grabHits.Add(1)
			t.Error("background version probe must never grab an NZB")
			return nil, core.ErrNotAvailable
		},
		OpenVaulted: func(_ context.Context, rel core.Release) (core.MediaSource, []byte, error) {
			if rel.ID != vaultRel.ID {
				return nil, nil, nil // unvaulted Usenet release skipped with zero grab
			}
			src := openTracked(rel.ID)
			return src, nil, nil
		},
		OpenCachedDebrid: func(_ context.Context, rel core.Release) (core.MediaSource, error) {
			if rel.ID != debridRel.ID {
				return nil, nil
			}
			return openTracked(rel.ID), nil
		},
		OnVersionProbed: func(_ core.RatingKey, rel core.Release) {
			mu.Lock()
			probedReleases = append(probedReleases, rel.ID)
			mu.Unlock()
			probedCh <- rel.ID
		},
	}, cfg, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// Browse prewarm must NOT trigger version header probes.
	browseCaps := defaultCaps("browse-client")
	m.Prewarm(ctx, owner, movieKey, browseCaps, core.PrewarmOptions{Reason: core.PrewarmBrowse})
	m.WaitWarmProbe(ctx, owner, browseCaps, movieKey, 2*time.Second)
	m.versionProbeWG.Wait()
	if _, hit := m.CachedReleaseProbe(vaultRel); hit {
		t.Fatal("browse prewarm triggered version header probe")
	}
	if _, hit := m.CachedReleaseProbe(debridRel); hit {
		t.Fatal("browse prewarm triggered debrid version header probe")
	}

	// Reset search counter after the browse prewarm completes so we verify the
	// viewer-triggered version header probe itself performs zero indexer searches.
	res.searchHits.Store(0)

	// Viewer-triggered detail schedule probes only vaulted Usenet and cached debrid.
	m.scheduleVersionProbes(owner, movieKey, defaultCaps("detail-client"))

	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case id := <-probedCh:
			seen[id] = true
		case <-deadline:
			t.Fatalf("timed out waiting for version probes; got %v", seen)
		}
	}
	if !seen[vaultRel.ID] || !seen[debridRel.ID] {
		t.Fatalf("probed = %v, want vaulted and cached debrid releases", seen)
	}
	if res.grabHits.Load() != 0 {
		t.Fatalf("background probe performed %d NZB grabs", res.grabHits.Load())
	}
	if res.searchHits.Load() != 0 {
		t.Fatalf("background probe performed %d indexer searches", res.searchHits.Load())
	}
	if maxConcurrent > 1 {
		t.Fatalf("max concurrent version probes = %d, want 1", maxConcurrent)
	}

	// CachedReleaseProbe now serves real probed tracks for both releases.
	for _, rel := range []core.Release{vaultRel, debridRel} {
		info, hit := m.CachedReleaseProbe(rel)
		if !hit || info.Estimated || info.Video() == nil {
			t.Fatalf("CachedReleaseProbe(%s) hit=%v info=%+v", rel.ID, hit, info)
		}
	}
	if _, hit := m.CachedReleaseProbe(unvaultedRel); hit {
		t.Fatal("unvaulted Usenet release was unexpectedly probed")
	}
}

func TestVersionHeaderProbeYieldsImmediatelyToActivePlayback(t *testing.T) {
	mkvBytes := loadMKVFixture(t)
	db := store.OpenTest(t)
	dir := payloadTempDir(t)

	slowRel := core.Release{
		ID:       "rel-slow-debrid",
		Protocol: core.ProtocolTorrent,
		InfoHash: "1111222233334444555566667777888899990000",
	}
	h := newHarness(t, nil, nil)
	res := &variantProbeResolver{
		fakeResolver: h.resolver,
		variants: []core.ReleaseVariant{
			{ID: slowRel.ID, Auto: true, Release: slowRel, Info: core.MediaInfo{Estimated: true}},
		},
	}

	readStarted := make(chan struct{}, 1)
	readCanceled := make(chan struct{}, 1)
	src := &fixtureMediaSource{
		id:         slowRel.ID,
		name:       "slow.mkv",
		data:       mkvBytes,
		readEvents: readStarted,
		onRead: func(ctx context.Context) error {
			<-ctx.Done()
			select {
			case readCanceled <- struct{}{}:
			default:
			}
			return ctx.Err()
		},
	}

	m, err := New(Deps{
		Resolver:   res,
		Engine:     h.engine,
		Transcoder: h.tr,
		Catalog:    h.catalog,
		SelectProfile: func(core.ClientCaps, *core.User, string) core.QualityProfile {
			return config.DefaultQuality().Profiles[1]
		},
		DB: db,
		OpenCachedDebrid: func(context.Context, core.Release) (core.MediaSource, error) {
			return src, nil
		},
	}, config.Defaults(dir), Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	m.scheduleVersionProbes(owner, movieKey, defaultCaps("detail-client"))
	select {
	case <-readStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("version probe did not start reading")
	}

	// Activating playback must immediately cancel the in-flight version header probe.
	m.mu.Lock()
	m.pendingN++
	m.mu.Unlock()
	m.cancelVersionProbes()

	select {
	case <-readCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("version probe did not yield when playback started")
	}
	m.versionProbeWG.Wait()
	if !src.closed.Load() {
		t.Fatal("yielded version probe did not close its source")
	}
	if _, hit := m.CachedReleaseProbe(slowRel); hit {
		t.Fatal("canceled probe must not store partial facts")
	}
}
