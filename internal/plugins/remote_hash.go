package plugins

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"runtime"
	"time"

	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"github.com/braedonsaunders/ether/internal/store"
)

const RemoteHashMaxBytes int64 = 64 << 30
const RemoteHashDailyBytes int64 = 50 << 30
const remoteHashFresh = 5 * time.Minute

func init() {
	store.Register("plugins", 8, `
CREATE TABLE plugins_remote_hashes(plugin TEXT NOT NULL,urlkey TEXT NOT NULL,etag TEXT NOT NULL,modified TEXT NOT NULL,size INTEGER NOT NULL,value BLOB NOT NULL,checked INTEGER NOT NULL,PRIMARY KEY(plugin,urlkey));
CREATE INDEX plugins_remote_hashes_age ON plugins_remote_hashes(plugin,checked);
CREATE TABLE plugins_remote_hash_budget(plugin TEXT PRIMARY KEY,day TEXT NOT NULL,bytes INTEGER NOT NULL);
`)
}

type HashRequest struct {
	URL        string `json:"url"`
	Algo       string `json:"algo"`
	ExpectSize *int64 `json:"expectSize,omitempty"`
}

// HashResult never includes remote bytes. All three digests share a single read.
type HashResult struct {
	Pending   bool   `json:"pending,omitempty"`
	RetryInMS int64  `json:"retryInMs,omitempty"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256,omitempty"`
	SHA1      string `json:"sha1,omitempty"`
	MD5       string `json:"md5,omitempty"`
}
type remoteHashWork struct {
	key    string
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	ended  time.Time
}
type remoteHashEntry struct {
	HashResult
	etag, modified string
	checked        int64
}

func (h *host) hashPermission(ctx context.Context, req HashRequest) error {
	if req.Algo != "sha256" && req.Algo != "sha1" && req.Algo != "md5" {
		return fmt.Errorf("plugins: hash algo must be sha256, sha1 or md5")
	}
	if req.ExpectSize != nil && (*req.ExpectSize < 0 || *req.ExpectSize > RemoteHashMaxBytes) {
		return fmt.Errorf("plugins: hash size outside limit")
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ErrPermissionDenied
	}
	if err := h.allowNetworkURL(ctx, u); err != nil {
		return err
	}
	return nil
}
func (h *host) cachedRemoteHash(ctx context.Context, key string) (remoteHashEntry, error) {
	var e remoteHashEntry
	var data []byte
	err := h.m.store.db.QueryRowContext(ctx, `SELECT etag,modified,value,checked FROM plugins_remote_hashes WHERE plugin=? AND urlkey=?`, h.plugin, key).Scan(&e.etag, &e.modified, &data, &e.checked)
	if err == nil {
		err = json.Unmarshal(data, &e.HashResult)
	}
	return e, err
}
func (h *host) HTTPHash(ctx context.Context, req HashRequest) (HashResult, error) {
	if err := h.hashPermission(ctx, req); err != nil {
		return HashResult{}, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(req.URL)))
	e, err := h.cachedRemoteHash(ctx, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return HashResult{}, err
	}
	if err == nil && h.m.now().Sub(time.Unix(e.checked, 0)) < remoteHashFresh {
		if req.ExpectSize != nil && *req.ExpectSize != e.Size {
			return HashResult{}, fmt.Errorf("plugins: hash size did not match expectSize")
		}
		return e.HashResult, nil
	}
	m := h.m
	m.remoteHashMu.Lock()
	defer m.remoteHashMu.Unlock()
	if m.remoteHashes == nil {
		m.remoteHashes = map[string]*remoteHashWork{}
	}
	if work := m.remoteHashes[h.plugin]; work != nil {
		select {
		case <-work.done:
			// The worker may have committed after our first cache read.
			if work.key == key && work.err == nil {
				if fresh, err := h.cachedRemoteHash(ctx, key); err == nil && m.now().Sub(time.Unix(fresh.checked, 0)) < remoteHashFresh {
					if req.ExpectSize != nil && *req.ExpectSize != fresh.Size {
						return HashResult{}, fmt.Errorf("plugins: hash size did not match expectSize")
					}
					return fresh.HashResult, nil
				}
			}
			if work.key == key && work.err != nil && m.now().Sub(work.ended) < 30*time.Second {
				return HashResult{}, work.err
			}
		default:
			return HashResult{Pending: true, RetryInMS: 1000}, nil
		}
	}
	// Recheck enabled state under the cancellation lock so a disable cannot
	// race between authorization and registering this worker.
	if _, _, err := h.record(ctx); err != nil {
		return HashResult{}, err
	}
	workCtx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	w := &remoteHashWork{key: key, cancel: cancel, done: make(chan struct{})}
	m.remoteHashes[h.plugin] = w
	go func() {
		defer cancel()
		select {
		case folderHashSlots <- struct{}{}:
			defer func() { <-folderHashSlots }()
		case <-workCtx.Done():
			w.err = workCtx.Err()
			close(w.done)
			return
		}
		// Waiting plugins consume a goroutine, never a dedicated OS thread.
		runtime.LockOSThread()
		lowerJobPriority()
		w.err = h.computeRemoteHash(workCtx, req, key, e)
		w.ended = m.now()
		close(w.done)
	}()
	return HashResult{Pending: true, RetryInMS: 1000}, nil
}
func (m *Manager) cancelRemoteHashes(id string) {
	m.remoteHashMu.Lock()
	defer m.remoteHashMu.Unlock()
	for plugin, w := range m.remoteHashes {
		if id == "" || plugin == id {
			w.cancel()
		}
	}
}

func (h *host) computeRemoteHash(ctx context.Context, req HashRequest, key string, old remoteHashEntry) error {
	if err := wasm.WaitBackground(ctx, h.m.deps.Busy); err != nil {
		return err
	}
	headers := map[string]string{"Accept-Encoding": "identity"}
	if old.etag != "" {
		headers["If-None-Match"] = old.etag
	} else if old.modified != "" {
		headers["If-Modified-Since"] = old.modified
	}
	resp, err := h.openFetch(ctx, FetchRequest{URL: req.URL, Headers: headers})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 304 && old.SHA256 != "" {
		_, err = h.m.store.db.ExecContext(ctx, `UPDATE plugins_remote_hashes SET checked=? WHERE plugin=? AND urlkey=?`, h.m.now().Unix(), h.plugin, key)
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("plugins: hash HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > RemoteHashMaxBytes {
		return fmt.Errorf("plugins: hash size outside limit")
	}
	if req.ExpectSize != nil && resp.ContentLength >= 0 && resp.ContentLength != *req.ExpectSize {
		return fmt.Errorf("plugins: hash size did not match expectSize")
	}
	// Reserve the remaining allowance durably before reading. An interrupted
	// process conservatively keeps its reservation; normal completion refunds it.
	day := h.m.now().UTC().Format("2006-01-02")
	_, err = h.m.store.db.ExecContext(ctx, `INSERT INTO plugins_remote_hash_budget(plugin,day,bytes) VALUES(?,?,0) ON CONFLICT(plugin) DO UPDATE SET day=excluded.day,bytes=0 WHERE plugins_remote_hash_budget.day<>excluded.day`, h.plugin, day)
	if err != nil {
		return err
	}
	var used int64
	if err = h.m.store.db.QueryRowContext(ctx, `SELECT bytes FROM plugins_remote_hash_budget WHERE plugin=?`, h.plugin).Scan(&used); err != nil {
		return err
	}
	allowance := RemoteHashDailyBytes - used
	if allowance <= 0 {
		h.m.log.Info("plugins: daily hash byte budget reached", "plugin", h.plugin)
		return fmt.Errorf("plugins: daily hash byte budget reached")
	}
	if resp.ContentLength > allowance {
		h.m.log.Info("plugins: daily hash byte budget reached", "plugin", h.plugin)
		return fmt.Errorf("plugins: daily hash byte budget reached")
	}
	reserve := allowance
	if resp.ContentLength >= 0 {
		reserve = resp.ContentLength
	}
	reservation, err := h.m.store.db.ExecContext(ctx, `UPDATE plugins_remote_hash_budget SET bytes=bytes+? WHERE plugin=? AND day=? AND bytes+?<=?`, reserve, h.plugin, day, reserve, RemoteHashDailyBytes)
	if err != nil {
		return err
	}
	if n, err := reservation.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		h.m.log.Info("plugins: daily hash byte budget reached", "plugin", h.plugin)
		return fmt.Errorf("plugins: daily hash byte budget reached")
	}
	var size int64
	defer func() {
		refundCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = h.m.store.db.ExecContext(refundCtx, `UPDATE plugins_remote_hash_budget SET bytes=bytes-? WHERE plugin=? AND day=?`, max(int64(0), reserve-size), h.plugin, day)
	}()
	a, b, c := sha256.New(), sha1.New(), md5.New()
	buf := make([]byte, 256<<10)
	var checkedGrants time.Time
	limit := min(RemoteHashMaxBytes, reserve)
	for {
		if err = wasm.WaitBackground(ctx, h.m.deps.Busy); err != nil {
			return err
		}
		// Recheck grants at most once a second rather than querying the store
		// for every chunk. Disable/uninstall also cancels the read context.
		if time.Since(checkedGrants) >= time.Second {
			if err = h.hashPermission(ctx, req); err != nil {
				return err
			}
			checkedGrants = time.Now()
		}
		n, readErr := resp.Body.Read(buf[:min(int64(len(buf)), limit-size+1)])
		size += int64(n)
		if size > limit {
			return fmt.Errorf("plugins: hash exceeded byte limit")
		}
		if n > 0 {
			_, _ = a.Write(buf[:n])
			_, _ = b.Write(buf[:n])
			_, _ = c.Write(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fetchError(resp.Request.URL.Hostname(), readErr)
		}
	}
	if req.ExpectSize != nil && size != *req.ExpectSize {
		return fmt.Errorf("plugins: hash size did not match expectSize")
	}
	result := HashResult{Size: size, SHA256: fmt.Sprintf("%x", a.Sum(nil)), SHA1: fmt.Sprintf("%x", b.Sum(nil)), MD5: fmt.Sprintf("%x", c.Sum(nil))}
	data, _ := json.Marshal(result)
	_, err = h.m.store.db.ExecContext(ctx, `INSERT INTO plugins_remote_hashes(plugin,urlkey,etag,modified,size,value,checked) VALUES(?,?,?,?,?,?,?) ON CONFLICT(plugin,urlkey) DO UPDATE SET etag=excluded.etag,modified=excluded.modified,size=excluded.size,value=excluded.value,checked=excluded.checked`, h.plugin, key, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), size, data, h.m.now().Unix())
	if err != nil {
		return err
	}
	_, err = h.m.store.db.ExecContext(ctx, `DELETE FROM plugins_remote_hashes WHERE plugin=? AND urlkey IN (SELECT urlkey FROM plugins_remote_hashes WHERE plugin=? ORDER BY checked DESC LIMIT 2147483647 OFFSET 2048)`, h.plugin, h.plugin)
	return err
}
