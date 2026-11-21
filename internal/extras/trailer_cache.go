package extras

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

var ErrCachePreparing = errors.New("trailer cache busy")

// ErrCacheUnsupported leaves non-MP4 single-file sources on their direct path.
var ErrCacheUnsupported = errors.New("extra cannot use MP4 cache")

var ErrCacheDisabled = errors.New("trailer cache disabled")

// CachedTrailer identifies immutable, locally downloaded trailer bytes.
// OpenCachedTrailer must be used to open it; eviction may have removed it.
type CachedTrailer struct {
	Path, MIMEType, SourceGeneration string
	extra                            Extra
}

func (s *Service) trailerGeneration(e Extra) string {
	var identity []byte
	if e.Source == SourcePlexDiscover {
		identity, _ = json.Marshal([]any{"plex-discover-v1", e.ParentKey, e.DiscoverKey, e.PublishedAt})
	} else {
		c := s.cfg()
		identity, _ = json.Marshal([]any{"split-mux-v1", e.ParentKey, e.Site, e.SiteKey, e.PublishedAt, c.ResolverCommand, c.ResolverArgs})
	}
	h := sha256.Sum256(identity)
	return hex.EncodeToString(h[:])
}

type trailerFill struct {
	done   chan struct{}
	cached CachedTrailer
	err    error
	retry  time.Time
}

// CacheTrailer is the polling focus/preview path; playback joins the same fill.
func (s *Service) CacheTrailer(ctx context.Context, e Extra) (CachedTrailer, error) {
	if s.cfg().TrailerCacheMaxBytes < 0 || !e.IsTrailer() {
		return CachedTrailer{}, ErrCacheDisabled
	}
	return s.cachedExtra(ctx, e, false)
}

// CacheExtra waits for a shared, bounded MP4 fill on explicit playback.
func (s *Service) CacheExtra(ctx context.Context, e Extra) (CachedTrailer, error) {
	return s.cachedExtra(ctx, e, true)
}

func (s *Service) cachedExtra(ctx context.Context, e Extra, wait bool) (CachedTrailer, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg().ResolverTimeoutOr(20*time.Second))
	defer cancel()
	if !s.cfg().Enabled || s.cacheDir == "" || s.cfg().TrailerCacheMaxBytes < 0 {
		return CachedTrailer{}, ErrCacheDisabled
	}
	generation := s.trailerGeneration(e)
	s.cacheFlightsMu.Lock()
	if s.cacheFlights == nil {
		s.cacheFlights = make(map[string]*trailerFill)
	}
	if f := s.cacheFlights[generation]; f != nil {
		select {
		case <-f.done:
			if s.now().Before(f.retry) {
				s.cacheFlightsMu.Unlock()
				return f.cached, f.err
			}
		default:
			s.cacheFlightsMu.Unlock()
			if !wait {
				return CachedTrailer{}, ErrCachePreparing
			}
			select {
			case <-ctx.Done():
				return CachedTrailer{}, ctx.Err()
			case <-f.done:
				return f.cached, f.err
			}
		}
	}
	// Bound cooldown metadata, without discarding running fills.
	if len(s.cacheFlights) >= maxResolvedEntries {
		for key, f := range s.cacheFlights {
			select {
			case <-f.done:
				delete(s.cacheFlights, key)
			default:
			}
		}
	}
	f := &trailerFill{done: make(chan struct{})}
	s.cacheFlights[generation] = f
	s.cacheFlightsMu.Unlock()
	f.cached, f.err = s.cacheExtra(ctx, e)
	s.cacheFlightsMu.Lock()
	if f.err != nil && !errors.Is(f.err, ErrCachePreparing) && !errors.Is(f.err, ErrCacheUnsupported) && !errors.Is(f.err, context.Canceled) {
		f.retry = s.now().Add(time.Minute)
		s.log.Info("extra cache fill failed", "key", e.Key, "source", e.Source, "site", e.Site, "siteKey", e.SiteKey, "error", f.err)
	} else {
		delete(s.cacheFlights, generation)
	}
	close(f.done)
	s.cacheFlightsMu.Unlock()
	return f.cached, f.err
}

func (s *Service) cacheExtra(ctx context.Context, e Extra) (CachedTrailer, error) {
	if !s.cacheDownloadMu.TryLock() {
		return CachedTrailer{}, ErrCachePreparing
	}
	defer s.cacheDownloadMu.Unlock()
	cap := s.cfg().TrailerCacheMaxBytes
	if cap <= 0 {
		cap = 512 << 20
	}
	generation := s.trailerGeneration(e)
	cached := CachedTrailer{Path: filepath.Join(s.cacheDir, generation+".mp4"), MIMEType: "video/mp4", SourceGeneration: generation, extra: e}
	if err := os.MkdirAll(s.cacheDir, 0700); err != nil {
		return CachedTrailer{}, err
	}
	// Retire an interrupted staging download before accounting for a new one.
	if err := os.RemoveAll(filepath.Join(s.cacheDir, "source.part")); err != nil {
		return CachedTrailer{}, err
	}
	for _, stale := range []string{"download.part", "mux-video.part", "mux-audio.part"} {
		if err := os.Remove(filepath.Join(s.cacheDir, stale)); err != nil && !os.IsNotExist(err) {
			return CachedTrailer{}, err
		}
	}
	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		return CachedTrailer{}, err
	}
	var files []os.FileInfo
	var used int64
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".mp4") {
			info, err := entry.Info()
			if err != nil {
				return CachedTrailer{}, err
			}
			files = append(files, info)
			used += info.Size()
		}
	}

	// Enforce cap reductions even if resolving the requested source later fails.
	if used > cap || len(files) > 512 {
		if info, err := os.Stat(cached.Path); err == nil && info.Size() <= cap {
			now := s.now()
			_ = os.Chtimes(cached.Path, now, now)
			for i, f := range files {
				if f.Name() == filepath.Base(cached.Path) {
					if fresh, err := os.Stat(cached.Path); err == nil {
						files[i] = fresh
					}
				}
			}
		}
		sort.Slice(files, func(i, j int) bool { return files[i].ModTime().Before(files[j].ModTime()) })
		for len(files) > 0 && (used > cap || len(files) > 512) {
			oldest := files[0]
			files = files[1:]
			s.cacheMu.Lock()
			err := os.Remove(filepath.Join(s.cacheDir, oldest.Name()))
			s.cacheMu.Unlock()
			if err != nil {
				return CachedTrailer{}, err
			}
			used -= oldest.Size()
		}
	}
	var response *http.Response
	var split *Resolved
	direct := false
	reserve := int64(0)
	hit, statErr := os.Stat(cached.Path)
	if statErr != nil || hit.Size() <= 0 || hit.Size() > cap {
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		direct = e.Source == SourceCommand && s.downloadTrailers && len(s.cfg().ResolverArgs) == 0
		if direct {
			reserve = min(cap, int64(128<<20))
		} else {
			resolved, err := s.Resolve(ctx, e)
			if err != nil {
				return CachedTrailer{}, err
			}
			if resolved.AudioURL == "" && resolved.Container != "mp4" {
				return CachedTrailer{}, ErrCacheUnsupported
			}
			reserve = min(cap, int64(64<<20))
			if resolved.AudioURL != "" {
				split = &resolved
			} else {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolved.URL, nil)
				if err != nil {
					return CachedTrailer{}, err
				}
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return CachedTrailer{}, errors.New("unsupported trailer URL")
				}
				for name, value := range resolved.Headers {
					req.Header.Set(name, value)
				}
				client := s.httpClient(90 * time.Second)
				response, err = client.Do(req)
				if err != nil {
					return CachedTrailer{}, scrubProxyErr(err, egress.Current().Proxy)
				}
				defer response.Body.Close()
				reserve = min(cap, int64(64<<20))
				if response.StatusCode != http.StatusOK || response.ContentLength > reserve {
					return CachedTrailer{}, errors.New("trailer unavailable or exceeds cache entry bound")
				}
				if response.ContentLength > 0 {
					reserve = response.ContentLength
				}
			}
		}
	} else {
		// A hit is the most recently used file, including after a cap reduction.
		now := s.now()
		_ = os.Chtimes(cached.Path, now, now)
		for i, f := range files {
			if f.Name() == filepath.Base(cached.Path) {
				if info, err := os.Stat(cached.Path); err == nil {
					files[i] = info
				}
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime().Before(files[j].ModTime()) })
	for len(files) > 0 && (used > cap-reserve || len(files) >= 512 && reserve > 0) {
		oldest := files[0]
		files = files[1:]
		s.cacheMu.Lock()
		err := os.Remove(filepath.Join(s.cacheDir, oldest.Name()))
		s.cacheMu.Unlock()
		if err != nil {
			return CachedTrailer{}, err
		}
		used -= oldest.Size()
	}
	if response == nil && split == nil && !direct {
		return cached, nil
	}
	stage := filepath.Join(s.cacheDir, "download.part")
	if direct {
		if err := s.downloadExtra(ctx, e, stage, reserve/2); err != nil {
			return CachedTrailer{}, err
		}
	}
	flags := os.O_CREATE | os.O_RDWR
	if !direct {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(stage, flags, 0600)
	if err != nil {
		return CachedTrailer{}, err
	}
	defer os.Remove(stage)
	var n int64
	var copyErr error
	var extraBytes int
	if direct {
		if info, err := f.Stat(); err == nil {
			n = info.Size()
		}
	} else if split != nil {
		copyErr = s.muxExtra(ctx, *split, f, reserve)
		if info, err := f.Stat(); err == nil {
			n = info.Size()
		}
	} else {
		n, copyErr = io.Copy(f, io.LimitReader(response.Body, reserve))
		var extra [1]byte
		var tailErr error
		extraBytes, tailErr = response.Body.Read(extra[:])
		if copyErr == nil && tailErr != nil && tailErr != io.EOF {
			copyErr = tailErr
		}
	}
	var header [12]byte
	_, readErr := f.ReadAt(header[:], 0)
	closeErr := f.Close()
	if copyErr != nil {
		return CachedTrailer{}, copyErr
	}
	if closeErr != nil {
		return CachedTrailer{}, closeErr
	}
	if n == 0 || n > reserve || extraBytes != 0 || (response != nil && response.ContentLength >= 0 && n != response.ContentLength) || readErr != nil || string(header[4:8]) != "ftyp" {
		return CachedTrailer{}, errors.New("trailer is not a bounded MP4 file")
	}
	if err := os.Rename(stage, cached.Path); err != nil {
		return CachedTrailer{}, err
	}
	return cached, nil
}

// OpenCachedTrailer serializes opening against eviction (including Windows).
func (s *Service) OpenCachedTrailer(c CachedTrailer) (*os.File, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if !s.cfg().Enabled || s.cfg().TrailerCacheMaxBytes < 0 || c.Path != filepath.Join(s.cacheDir, c.SourceGeneration+".mp4") || len(c.SourceGeneration) != 64 || s.trailerGeneration(c.extra) != c.SourceGeneration {
		return nil, errors.New("invalid cached trailer")
	}
	f, err := os.Open(c.Path)
	if err == nil {
		now := s.now()
		_ = os.Chtimes(c.Path, now, now)
	}
	return f, err
}

// muxExtra produces a seekable MP4 with its moov atom up front.
// No video or audio is re-encoded. One fill owns the staging file.
func (s *Service) muxExtra(ctx context.Context, r Resolved, output *os.File, limit int64) error {
	ff := "ffmpeg"
	if s.themeFFmpeg != nil {
		ff = s.themeFFmpeg()
	}
	if ff == "" {
		return errors.New("extra muxer unavailable")
	}
	pVideo, policyEnabled, _, err := extrasProxyFor(r.URL)
	if err != nil {
		return err
	}
	pAudio, _, _, err := extrasProxyFor(r.AudioURL)
	if err != nil {
		return err
	}
	// FFmpeg only honors http:// in -http_proxy and silently dials direct for
	// https:// or socks5://. Fetch those schemes through Go's egress client so
	// proxied trailers never leak direct.
	if (pVideo != nil && pVideo.Scheme != "http") || (pAudio != nil && pAudio.Scheme != "http") {
		return s.muxExtraViaHTTP(ctx, ff, r, output, limit, pVideo, pAudio)
	}
	inputArgs, err := muxInputArgs([]string{r.URL, r.AudioURL}, r.Headers, []*url.URL{pVideo, pAudio})
	if err != nil {
		return err
	}
	softLimit := max(int64(1), limit-(64<<10))
	args := []string{"-nostdin", "-v", "error", "-y", "-threads", "1"}
	args = append(args, inputArgs...)
	args = append(args, "-map", "0:v:0", "-map", "1:a:0", "-c", "copy", "-movflags", "+faststart", "-fs", fmt.Sprint(softLimit), "-f", "mp4", output.Name())
	proc := mediaproc.CommandContext(ctx, 90*time.Second, ff, args...)
	if env := subprocessEnv(nil, nil, policyEnabled, nil, false); env != nil {
		proc.Env = env
	}
	serr := &cappedBuffer{max: maxResolverStderr}
	proc.Stderr = serr
	if err := proc.Run(); err != nil {
		return scrubProxyErr(fmt.Errorf("extra mux failed: %w", err), pVideo, pAudio)
	}
	info, err := output.Stat()
	if err != nil {
		return err
	}
	// -fs can finish successfully after truncating: reject at the bound.
	if info.Size() >= softLimit {
		return errors.New("muxed extra exceeds cache entry bound")
	}
	return nil
}

func (s *Service) muxExtraViaHTTP(ctx context.Context, ff string, r Resolved, output *os.File, limit int64, pVideo, pAudio *url.URL) error {
	videoPath := filepath.Join(s.cacheDir, "mux-video.part")
	audioPath := filepath.Join(s.cacheDir, "mux-audio.part")
	defer os.Remove(videoPath)
	defer os.Remove(audioPath)
	client := s.httpClient(90 * time.Second)
	remaining := limit
	for _, part := range []struct {
		raw  string
		path string
	}{{r.URL, videoPath}, {r.AudioURL, audioPath}} {
		if remaining <= 0 {
			return errors.New("muxed extra exceeds cache entry bound")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, part.raw, nil)
		if err != nil {
			return err
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("unsupported trailer URL")
		}
		for k, v := range r.Headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return scrubProxyErr(err, pVideo, pAudio)
		}
		if resp.StatusCode != http.StatusOK || resp.ContentLength > remaining {
			resp.Body.Close()
			return errors.New("trailer unavailable or exceeds cache entry bound")
		}
		f, err := os.OpenFile(part.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			resp.Body.Close()
			return err
		}
		n, copyErr := io.Copy(f, io.LimitReader(resp.Body, remaining+1))
		closeErr := f.Close()
		resp.Body.Close()
		if copyErr != nil {
			return scrubProxyErr(copyErr, pVideo, pAudio)
		}
		if closeErr != nil {
			return closeErr
		}
		if n == 0 || n > remaining {
			return errors.New("muxed extra exceeds cache entry bound")
		}
		remaining -= n
	}
	softLimit := max(int64(1), limit-(64<<10))
	args := []string{
		"-nostdin", "-v", "error", "-y", "-threads", "1",
		"-protocol_whitelist", "file,pipe", "-i", videoPath,
		"-protocol_whitelist", "file,pipe", "-i", audioPath,
		"-map", "0:v:0", "-map", "1:a:0", "-c", "copy", "-movflags", "+faststart", "-fs", fmt.Sprint(softLimit), "-f", "mp4", output.Name(),
	}
	proc := mediaproc.CommandContext(ctx, 90*time.Second, ff, args...)
	serr := &cappedBuffer{max: maxResolverStderr}
	proc.Stderr = serr
	if err := proc.Run(); err != nil {
		return fmt.Errorf("extra mux failed: %w", err)
	}
	info, err := output.Stat()
	if err != nil {
		return err
	}
	if info.Size() >= softLimit {
		return errors.New("muxed extra exceeds cache entry bound")
	}
	return nil
}

// downloadExtra lets yt-dlp apply its site-specific chunking and merge fixes.
// Inputs plus output reserve at most twice the entry limit. Staging lives only
// for this demand-driven fill and is removed even on timeout or failure.
func (s *Service) downloadExtra(ctx context.Context, e Extra, stage string, limit int64) error {
	page, ok := PageURL(e.Site, e.SiteKey)
	if !ok {
		return errors.New("invalid extra site/key")
	}
	proxy, policyEnabled, bypass, err := extrasProxyFor(page)
	if err != nil {
		return err
	}
	cfg := s.cfg()
	cmd, ok := s.effectiveCommand(cfg)
	if !ok {
		return errors.New("extras resolver unavailable")
	}
	dir := filepath.Join(s.cacheDir, "source.part")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ff := "ffmpeg"
	if s.themeFFmpeg != nil {
		ff = s.themeFFmpeg()
	}
	if ff == "" {
		return errors.New("extra muxer unavailable")
	}
	softLimit := max(int64(1), limit-(64<<10))
	args := make([]string, 0, 26)
	if proxy != nil {
		args = append(args, "--proxy", proxy.String())
	}
	args = append(args, "--no-playlist", "--no-cache-dir", "--no-progress", "--no-warnings", "-f", defaultTrailerFormat, "-N", "8", "--http-chunk-size", "10M", "--merge-output-format", "mp4", "--remux-video", "mp4", "--ffmpeg-location", ff, "--postprocessor-args", fmt.Sprintf("ffmpeg_o:-movflags +faststart -fs %d", softLimit), "--max-filesize", fmt.Sprint(max(int64(1), softLimit/2)), "-o", filepath.Join(dir, "media.%(ext)s"), "--", page)
	runCtx := withRunnerProxy(ctx, runnerProxyConfig{proxy: proxy, policyEnabled: policyEnabled, bypass: bypass})
	if _, err := s.runner(runCtx, cmd, args); err != nil {
		return scrubProxyErr(err, proxy)
	}
	path := filepath.Join(dir, "media.mp4")
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 || info.Size() >= softLimit {
		return errors.New("downloaded extra exceeds cache entry bound")
	}
	return os.Rename(path, stage)
}
