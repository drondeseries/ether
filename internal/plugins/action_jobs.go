package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ActionJobTotal         = 12 * time.Hour
	ActionJobAttempts      = 1000000
	ActionJobConcurrency   = 2
	ResolverJobConcurrency = 4
	actionJobMinDelay      = 100 * time.Millisecond
)

// ActionJobStatus is a bounded, in-memory snapshot. Checkpoints belong in plugin
// KV when they must survive a server restart; tokens are only passed between calls.
type ActionJobStatus struct {
	Path         string    `json:"path"`
	State        string    `json:"state"` // running, done, canceled, error
	Progress     string    `json:"progress,omitempty"`
	Error        string    `json:"error,omitempty"`
	Attempts     int       `json:"attempts"`
	Started      time.Time `json:"started"`
	TotalLimitMS int64     `json:"totalLimitMs"`
	AttemptLimit int       `json:"attemptLimit"`
}

type actionJob struct {
	mu     sync.Mutex
	status ActionJobStatus
	cancel context.CancelFunc
	done   chan struct{}
}

func (j *actionJob) snapshot() ActionJobStatus { j.mu.Lock(); defer j.mu.Unlock(); return j.status }
func (j *actionJob) finish(state, reason string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status.State, j.status.Error = state, reason
}

// ExpandJobRunner executes plugin calls and optional /ether/job/... continuations
// inside an active lowered-priority background expansion job slot.
type ExpandJobRunner struct {
	m        *Manager
	pluginID string
	job      *actionJob
}

// Call invokes a plugin route and increments the job's attempt counter. A fill
// always starts because a viewer opened that studio or person and is waiting
// for it, so it does not yield to other streams the way speculative background
// work does.
func (r *ExpandJobRunner) Call(ctx context.Context, req Request) (Response, error) {
	r.job.mu.Lock()
	r.job.status.Attempts++
	r.job.mu.Unlock()
	return r.m.Call(ctx, r.pluginID, req)
}

// SetProgress updates the job's human-readable progress string.
func (r *ExpandJobRunner) SetProgress(progress string) {
	if len(progress) > 1024 {
		progress = progress[:1024]
	}
	r.job.mu.Lock()
	r.job.status.Progress = progress
	r.job.mu.Unlock()
}

// RunContinuation drives a /ether/job/... continuation inside the same background
// job slot so long-running expansions never consume a second ActionJobConcurrency slot.
func (r *ExpandJobRunner) RunContinuation(ctx context.Context, jobPath string, initial json.RawMessage, onStep func(body json.RawMessage) error) error {
	path := strings.TrimSpace(jobPath)
	if !strings.HasPrefix(path, "/ether/job/") {
		path = "/ether/job/" + strings.TrimPrefix(path, "/")
	}
	if len(path) > 256 || strings.Contains(path, "..") || strings.ContainsAny(path, "?#\\") {
		return fmt.Errorf("invalid job path %q", jobPath)
	}
	return runActionJobWithHook(ctx, r.job, ActionJobAttempts, func(ctx context.Context, token json.RawMessage) (Response, error) {
		payload := map[string]json.RawMessage{}
		_ = json.Unmarshal(initial, &payload)
		if payload == nil {
			payload = map[string]json.RawMessage{}
		}
		if len(token) > 0 {
			payload["continuation"] = token
		}
		encoded, _ := json.Marshal(payload)
		return r.m.Call(ctx, r.pluginID, Request{Method: "POST", Path: path, Body: encoded})
	}, onStep)
}

// IsExpandJobRunning reports whether a background expansion job with dedupKey
// is currently running for pluginID.
func (m *Manager) IsExpandJobRunning(pluginID, dedupKey string) bool {
	if m == nil || pluginID == "" || dedupKey == "" {
		return false
	}
	key := pluginID + "|expand|" + dedupKey
	if v, ok := m.jobs.Load(key); ok {
		return v.(*actionJob).snapshot().State == "running"
	}
	return false
}

// StartExpandJob starts a lowered-priority background expansion job for
// (pluginID, dedupKey) sharing the plugin's ActionJobConcurrency = 2 background
// pool (never ResolverJobConcurrency). If a job for dedupKey is already running,
// it returns started=false, err=nil. If two background jobs are already running
// for pluginID, it returns ErrJobsBusy.
func (m *Manager) StartExpandJob(pluginID, routePath, dedupKey string, fn func(ctx context.Context, run *ExpandJobRunner) error) (bool, error) {
	if m == nil {
		return false, ErrUnavailable
	}
	key := pluginID + "|expand|" + dedupKey
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	if old, ok := m.jobs.Load(key); ok && old.(*actionJob).snapshot().State == "running" {
		return false, nil
	}
	resolverPrefix := pluginID + "|resolver|"
	activeAction, count := 0, 0
	oldestKey := ""
	var oldest time.Time
	m.jobs.Range(func(k, v any) bool {
		ks := k.(string)
		if !strings.HasPrefix(ks, pluginID+"|") {
			return true
		}
		s := v.(*actionJob).snapshot()
		count++
		if s.State == "running" {
			if !strings.HasPrefix(ks, resolverPrefix) {
				activeAction++
			}
		} else if oldestKey == "" || s.Started.Before(oldest) {
			oldestKey, oldest = ks, s.Started
		}
		return true
	})
	if activeAction >= ActionJobConcurrency {
		return false, &core.JobsBusyError{Message: "Two background jobs are already running; wait for one to finish."}
	}
	if _, exists := m.jobs.Load(key); !exists && count >= 16 && oldestKey != "" {
		m.jobs.Delete(oldestKey)
	}
	started := time.Now().UTC()
	if m.now != nil {
		started = m.now()
	}
	ceilings := m.runtimeCeilings(pluginID)
	ctx, cancel := context.WithTimeout(context.Background(), ceilings.deadline)
	j := &actionJob{
		cancel: cancel,
		done:   make(chan struct{}),
		status: ActionJobStatus{
			Path:         routePath,
			State:        "running",
			Started:      started,
			TotalLimitMS: ceilings.deadline.Milliseconds(),
			AttemptLimit: ActionJobAttempts,
		},
	}
	m.jobs.Store(key, j)
	go func() {
		runtime.LockOSThread() // Do not unlock: this lowered-priority thread is discarded on return.
		lowerJobPriority()
		defer cancel()
		defer close(j.done)
		err := fn(ctx, &ExpandJobRunner{m: m, pluginID: pluginID, job: j})
		if err != nil {
			state := "error"
			if ctx.Err() == context.Canceled {
				state = "canceled"
			}
			if ctx.Err() == context.DeadlineExceeded {
				err = fmt.Errorf("job reached its total time limit of %s", ceilings.deadline)
			}
			reason := m.store.redactKnown(context.Background(), pluginID, err.Error())
			j.finish(state, reason)
			m.log.Warn("plugins: expand job", "plugin", pluginID, "job", routePath, "err", reason)
			if state == "error" {
				durMS := max(time.Since(started).Milliseconds(), int64(0))
				m.emitBusEvent(events.EventPluginJobFailed, pluginID, events.PluginJobFailedPayload{
					PluginID:   pluginID,
					Name:       m.pluginDisplayName(context.Background(), pluginID),
					Job:        routePath,
					Kind:       "expand",
					Error:      reason,
					DurationMS: durMS,
				}, false)
			}
		} else {
			j.finish("done", "")
		}
	}()
	return true, nil
}

// startActionJob runs owner-started work. At most ActionJobConcurrency owner or
// scheduled jobs and ResolverJobConcurrency resolver jobs run concurrently per
// plugin; at most sixteen route snapshots are retained per plugin.
func (m *Manager) startActionJob(pluginID string, body json.RawMessage) error {
	_, err := m.startResolverJob(pluginID, body, nil, "")
	return err
}

func (m *Manager) startResolverJob(pluginID string, body, initial json.RawMessage, suffix string) (*actionJob, error) {
	var out struct {
		Job string `json:"job"`
	}
	if json.Unmarshal(body, &out) != nil || out.Job == "" {
		return nil, nil
	}
	path := out.Job
	if !strings.HasPrefix(path, "/ether/job/") {
		path = "/ether/job/" + strings.TrimPrefix(path, "/")
	}
	if len(path) > 256 || strings.Contains(path, "..") || strings.ContainsAny(path, "?#\\") {
		return nil, nil
	}
	key := pluginID + "|" + path
	if suffix != "" {
		key = pluginID + "|resolver" + suffix
	}
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	if old, ok := m.jobs.Load(key); ok && old.(*actionJob).snapshot().State == "running" {
		return old.(*actionJob), nil
	}
	isResolver := suffix != ""
	resolverPrefix := pluginID + "|resolver|"
	activeAction, activeResolver, count := 0, 0, 0
	oldestKey := ""
	var oldest time.Time
	m.jobs.Range(func(k, v any) bool {
		ks := k.(string)
		if !strings.HasPrefix(ks, pluginID+"|") {
			return true
		}
		s := v.(*actionJob).snapshot()
		count++
		if s.State == "running" {
			if strings.HasPrefix(ks, resolverPrefix) {
				activeResolver++
			} else {
				activeAction++
			}
		} else if oldestKey == "" || s.Started.Before(oldest) {
			oldestKey, oldest = ks, s.Started
		}
		return true
	})
	if !isResolver && activeAction >= ActionJobConcurrency {
		return nil, fmt.Errorf("two background jobs are already running; wait for one to finish")
	}
	if isResolver && activeResolver >= ResolverJobConcurrency {
		msg := "Four playback preparation jobs are already running; wait for one to finish."
		refusedKey := pluginID + "|resolver:refused|" + path
		if _, exists := m.jobs.Load(refusedKey); !exists && count >= 16 && oldestKey != "" {
			m.jobs.Delete(oldestKey)
		}
		done := make(chan struct{})
		close(done)
		started := time.Now().UTC()
		if m.now != nil {
			started = m.now()
		}
		m.jobs.Store(refusedKey, &actionJob{
			cancel: func() {},
			done:   done,
			status: ActionJobStatus{
				Path:         path,
				State:        "error",
				Error:        msg,
				Started:      started,
				TotalLimitMS: m.runtimeCeilings(pluginID).deadline.Milliseconds(),
				AttemptLimit: ActionJobAttempts,
			},
		})
		m.log.Warn("plugins: resolver job refused", "plugin", pluginID, "job", path, "err", msg)
		return nil, &core.JobsBusyError{Message: msg}
	}
	if _, exists := m.jobs.Load(key); !exists && count >= 16 && oldestKey != "" {
		m.jobs.Delete(oldestKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.runtimeCeilings(pluginID).deadline)
	j := &actionJob{cancel: cancel, done: make(chan struct{}), status: ActionJobStatus{Path: path, State: "running", Started: time.Now().UTC(), TotalLimitMS: m.runtimeCeilings(pluginID).deadline.Milliseconds(), AttemptLimit: ActionJobAttempts}}
	m.jobs.Store(key, j)
	go func() {
		runtime.LockOSThread() // Do not unlock: this lowered-priority thread is discarded on return.
		lowerJobPriority()
		defer cancel()
		defer close(j.done)
		err := runActionJob(ctx, j, ActionJobAttempts, func(ctx context.Context, token json.RawMessage) (Response, error) {
			if err := wasm.WaitBackground(ctx, m.deps.Busy); err != nil {
				return Response{}, err
			}
			payload := map[string]json.RawMessage{}
			_ = json.Unmarshal(initial, &payload)
			if payload == nil {
				payload = map[string]json.RawMessage{}
			}
			if len(token) > 0 {
				payload["continuation"] = token
			}
			encoded, _ := json.Marshal(payload)
			return m.Call(ctx, pluginID, Request{Method: "POST", Path: path, Body: encoded})
		})
		if err != nil {
			state := "error"
			if ctx.Err() == context.Canceled {
				state = "canceled"
			}
			if ctx.Err() == context.DeadlineExceeded {
				err = fmt.Errorf("job reached its total time limit of %s", m.runtimeCeilings(pluginID).deadline)
			}
			reason := m.store.redactKnown(context.Background(), pluginID, err.Error())
			j.finish(state, reason)
			m.log.Warn("plugins: action job", "plugin", pluginID, "job", path, "err", reason)
			if state == "error" {
				kind := "action"
				if isResolver {
					kind = "resolver"
				}
				durMS := max(time.Since(j.snapshot().Started).Milliseconds(), int64(0))
				m.emitBusEvent(events.EventPluginJobFailed, pluginID, events.PluginJobFailedPayload{
					PluginID:   pluginID,
					Name:       m.pluginDisplayName(context.Background(), pluginID),
					Job:        path,
					Kind:       kind,
					Error:      reason,
					DurationMS: durMS,
				}, false)
			}
		} else {
			j.finish("done", "")
		}
	}()
	return j, nil
}

// runActionJob treats an absent continuation flag as completion for existing
// one-call jobs. Explicit failures and non-success HTTP responses always stop.
func runActionJob(ctx context.Context, j *actionJob, attempts int, call func(context.Context, json.RawMessage) (Response, error)) error {
	return runActionJobWithHook(ctx, j, attempts, call, nil)
}

func runActionJobWithHook(ctx context.Context, j *actionJob, attempts int, call func(context.Context, json.RawMessage) (Response, error), onStep func(json.RawMessage) error) error {
	var token json.RawMessage
	for n := 0; n < attempts; n++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("job stopped: %w", err)
		}
		j.mu.Lock()
		j.status.Attempts++
		j.mu.Unlock()
		resp, err := call(ctx, token)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("job stopped: %w", err)
		}
		if resp.Status < 200 || resp.Status >= 300 {
			return fmt.Errorf("job returned HTTP %d", resp.Status)
		}
		var step struct {
			ContinueInMS *int64          `json:"continueInMs"`
			Continuation json.RawMessage `json:"continuation"`
			Progress     string          `json:"progress"`
			Done         bool            `json:"done"`
			Failed       string          `json:"failed"`
		}
		if len(resp.Body) > 0 {
			if err = json.Unmarshal(resp.Body, &step); err != nil {
				return fmt.Errorf("invalid job response: %w", err)
			}
		}
		if len(step.Progress) > 1024 {
			return fmt.Errorf("job progress exceeds 1024 bytes")
		}
		j.mu.Lock()
		if step.Progress != "" {
			j.status.Progress = step.Progress
		}
		j.mu.Unlock()
		if step.Failed != "" {
			return fmt.Errorf("job failed: %.1024s", step.Failed)
		}
		if onStep != nil && len(resp.Body) > 0 {
			if err := onStep(resp.Body); err != nil {
				return err
			}
		}
		if step.Done || step.ContinueInMS == nil {
			return nil
		}
		if *step.ContinueInMS < 0 || *step.ContinueInMS > 300000 {
			return fmt.Errorf("continueInMs must be between 0 and 300000")
		}
		if len(step.Continuation) > 64<<10 {
			return fmt.Errorf("job continuation exceeds 64 KiB")
		}
		token = step.Continuation
		if n+1 == attempts {
			break
		}
		timer := time.NewTimer(max(actionJobMinDelay, time.Duration(*step.ContinueInMS)*time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("job stopped: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("job reached its limit of %d calls", attempts)
}

func (m *Manager) cancelActionJobs(id string) {
	var flights []*scheduleFlight
	m.mu.Lock()
	for k, f := range m.scheduleInFlight {
		if id == "" || strings.HasPrefix(k, id+"|") {
			flights = append(flights, f)
			delete(m.scheduleInFlight, k)
		}
	}
	for k := range m.scheduleJobs {
		if id == "" || strings.HasPrefix(k, id+"|") {
			delete(m.scheduleJobs, k)
		}
	}
	m.mu.Unlock()
	for _, f := range flights {
		f.cancel()
	}
	m.jobs.Range(func(k, v any) bool {
		if id == "" || strings.HasPrefix(k.(string), id+"|") {
			v.(*actionJob).cancel()
		}
		return true
	})
}
func (m *Manager) handleActionJobs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := m.Get(r.Context(), id); err != nil {
		m.fail(w, err)
		return
	}
	jobs := []ActionJobStatus{}
	m.jobs.Range(func(k, v any) bool {
		if strings.HasPrefix(k.(string), id+"|") {
			s := v.(*actionJob).snapshot()
			s.Progress = m.store.redactKnown(r.Context(), id, s.Progress)
			jobs = append(jobs, s)
		}
		return true
	})
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Started.After(jobs[j].Started) })
	writeJSON(w, http.StatusOK, jobs)
}
func (m *Manager) handleCancelActionJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "invalid", "The request was not understood.", nil)
		return
	}
	m.jobs.Range(func(k, v any) bool {
		if strings.HasPrefix(k.(string), r.PathValue("id")+"|") && v.(*actionJob).snapshot().Path == req.Path {
			v.(*actionJob).cancel()
		}
		return true
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
