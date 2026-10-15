package internal

import (
	"sync"
	"time"
)

// Worker represents a compute node registered with the gateway.
type Worker struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"` // code | doc | csv | image | audio | video | exec
	Name          string    `json:"name"`
	CPU           string    `json:"cpu"`
	Cores         int       `json:"cores"`
	MemoryMB      int       `json:"memory_mb"`
	CPUUsage      float64   `json:"cpu_usage"`
	MemUsage      float64   `json:"mem_usage"`
	QueueLen      int       `json:"queue_len"`
	Status        string    `json:"status"` // online | busy | offline
	LastSeen      time.Time `json:"last_seen"`
	TasksDone     int       `json:"tasks_done"`
	TasksFailed   int       `json:"tasks_failed"` // running count
	Stability     float64   `json:"stability"`    // 1.0 = no failures, 0.5 = half failed
	GPUMemUsedMB  int64     `json:"gpu_mem_used_mb"`
	GPUMemTotalMB int64     `json:"gpu_mem_total_mb"`
	Host          string    `json:"host"`
	Port          int       `json:"port"`
	Capabilities  []string  `json:"capabilities,omitempty"` // self-reported: ["python","ffmpeg","model:sdxl",...]
	RegisteredAt  time.Time `json:"registered_at"`
}

// TaskRequirements describes the hard constraints a task imposes on a worker.
// Empty / zero values are treated as "no constraint" so old clients can submit
// tasks without this struct and fall through to scoring-only selection.
type TaskRequirements struct {
	RequireCaps  []string `json:"require_caps,omitempty"`
	MinCores     int      `json:"min_cores,omitempty"`
	MinMemoryMB  int      `json:"min_memory_mb,omitempty"`
	MinGPUFreeMB int64    `json:"min_gpu_free_mb,omitempty"`
}

// Task represents a unit of work dispatched to a worker.
type Task struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Payload      string           `json:"payload"`
	WorkerID     string           `json:"worker_id"`
	Status       string           `json:"status"` // pending | running | done | failed
	Progress     int              `json:"progress"`
	Priority     int              `json:"priority"`    // higher = more important
	MaxRetries   int              `json:"max_retries"` // 0 = no retry
	Retries      int              `json:"retries"`     // current retry count
	TimeoutSecs  int              `json:"timeout_secs"` // per-task timeout; 0 = use default
	CreatedAt    time.Time        `json:"created_at"`
	StartedAt    time.Time        `json:"started_at,omitempty"`
	FinishedAt   time.Time        `json:"finished_at,omitempty"`
	Result       string           `json:"result,omitempty"`
	Error        string           `json:"error,omitempty"`
	Logs         []string         `json:"logs"`
	Source       string           `json:"source"` // panel | openai
	Requirements TaskRequirements `json:"requirements,omitempty"` // hard filters; empty = no filter
	cancelCh     chan struct{}    `json:"-"` // internal: signaled to request task abort
}

// Stats is the lifetime aggregate of task outcomes. Survives across the
// in-memory task map (cleanup evicts old tasks after taskTTL; Stats does
// not forget them). Persisted to gateway-state/stats.json so the values
// carry across gateway restarts.
type Stats struct {
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// Store is an in-memory store for workers, tasks, and event history.
type Store struct {
	mu      sync.RWMutex
	workers map[string]*Worker
	tasks   map[string]*Task
	stats   Stats
	// notify is the broadcast channel used by ws Hub
	notify func(event string, payload interface{})
}

func NewStore() *Store {
	return &Store{
		workers: make(map[string]*Worker),
		tasks:   make(map[string]*Task),
	}
}

// StatsSnapshot returns a copy of the lifetime counters. Cheap (struct copy).
func (s *Store) StatsSnapshot() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats
}

// SetStats overwrites the lifetime counters. Called once at startup from the
// persistence layer; do not use at runtime (the increments are not
// idempotent — re-applying after a partial run would double-count).
func (s *Store) SetStats(st Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats = st
}

func (s *Store) SetNotifier(fn func(event string, payload interface{})) {
	s.notify = fn
}

func (s *Store) AddWorker(w *Worker) {
	s.mu.Lock()
	existing, ok := s.workers[w.ID]
	if ok {
		// reuse — update mutable fields
		existing.CPU = w.CPU
		existing.Cores = w.Cores
		existing.MemoryMB = w.MemoryMB
		existing.Name = w.Name
		existing.Host = w.Host
		existing.Port = w.Port
		existing.Capabilities = w.Capabilities
		existing.LastSeen = time.Now()
		existing.Status = "online"
		// reset stability on re-register
		existing.TasksFailed = 0
		existing.Stability = 1.0
		s.mu.Unlock()
		s.notify("worker_updated", existing)
		return
	}
	w.RegisteredAt = time.Now()
	w.LastSeen = time.Now()
	w.Status = "online"
	if w.Stability == 0 {
		w.Stability = 1.0
	}
	s.workers[w.ID] = w
	s.mu.Unlock()
	s.notify("worker_added", w)
}

func (s *Store) UpdateHeartbeat(id string, cpu, mem float64, queueLen int, gpuUsedMB int64, gpuTotalMB int64) *Worker {
	s.mu.Lock()
	w, ok := s.workers[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	w.CPUUsage = cpu
	w.MemUsage = mem
	w.QueueLen = queueLen
	w.GPUMemUsedMB = gpuUsedMB
	w.GPUMemTotalMB = gpuTotalMB
	w.LastSeen = time.Now()
	if w.Status == "offline" {
		w.Status = "online"
	}
	wCopy := *w
	s.mu.Unlock()
	s.notify("worker_updated", &wCopy)
	return w
}

// RecordTaskResult updates worker stability counters when a task finishes.
// success=true increments success; false increments failure. Stability score is
// smoothed so single failures don't tank the rating.
func (s *Store) RecordTaskResult(workerID string, success bool) {
	s.mu.Lock()
	w, ok := s.workers[workerID]
	if !ok {
		s.mu.Unlock()
		return
	}
	// exponential moving average of stability
	const alpha = 0.15
	if success {
		w.Stability = w.Stability*(1-alpha) + 1.0*alpha
	} else {
		w.TasksFailed++
		w.Stability = w.Stability*(1-alpha) + 0.0*alpha
	}
	s.mu.Unlock()
}

// RemoveWorker deletes a worker from the map entirely. Re-registration
// with the same ID is allowed (worker comes back on next heartbeat).
func (s *Store) RemoveWorker(id string) *Worker {
	s.mu.Lock()
	w, ok := s.workers[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.workers, id)
	cp := *w
	s.mu.Unlock()
	s.notify("worker_deleted", &cp)
	return &cp
}

func (s *Store) MarkOffline(id string) {
	s.mu.Lock()
	w, ok := s.workers[id]
	if !ok || w.Status == "offline" {
		s.mu.Unlock()
		return
	}
	w.Status = "offline"
	s.mu.Unlock()
	s.notify("worker_removed", w)
}

func (s *Store) GetWorker(id string) *Worker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.workers[id]
	if !ok {
		return nil
	}
	cp := *w
	return &cp
}

func (s *Store) ListWorkers() []*Worker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Worker, 0, len(s.workers))
	for _, w := range s.workers {
		cp := *w
		out = append(out, &cp)
	}
	return out
}

func (s *Store) AddTask(t *Task) {
	s.mu.Lock()
	s.tasks[t.ID] = t
	if t.Priority == 0 {
		t.Priority = 0 // explicit zero
	}
	if t.TimeoutSecs == 0 {
		t.TimeoutSecs = 300 // default 5 min
	}
	tCopy := *t
	logsCopy := append([]string{}, t.Logs...)
	tCopy.Logs = logsCopy
	s.mu.Unlock()
	s.notify("task_added", &tCopy)
}

func (s *Store) AssignTask(taskID, workerID string) {
	s.mu.Lock()
	t, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return
	}
	t.WorkerID = workerID
	t.Status = "running"
	t.StartedAt = time.Now()
	t.Progress = 0
	t.cancelCh = make(chan struct{})
	tCopy := *t
	tCopy.Logs = append([]string{}, t.Logs...)
	s.mu.Unlock()
	s.notify("task_updated", &tCopy)
}

// RetryTask reverts a finished task back to pending so it can be re-dispatched
// to another node. Used for automatic retries after worker-side failures.
func (s *Store) RetryTask(taskID string) bool {
	s.mu.Lock()
	t, ok := s.tasks[taskID]
	if !ok || t.Status != "failed" {
		s.mu.Unlock()
		return false
	}
	t.Status = "pending"
	t.WorkerID = ""
	t.StartedAt = time.Time{}
	t.FinishedAt = time.Time{}
	t.Progress = 0
	t.Retries++
	t.Error = ""
	tCopy := *t
	tCopy.Logs = append([]string{}, t.Logs...)
	s.mu.Unlock()
	s.notify("task_updated", &tCopy)
	return true
}

func (s *Store) UpdateProgress(taskID string, progress int, log string) {
	s.mu.Lock()
	t, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return
	}
	t.Progress = progress
	if log != "" {
		// bound log buffer to last 200 entries
		if len(t.Logs) >= 200 {
			t.Logs = t.Logs[1:]
		}
		t.Logs = append(t.Logs, log)
	}
	tCopy := *t
	tCopy.Logs = append([]string{}, t.Logs...)
	s.mu.Unlock()
	s.notify("task_updated", &tCopy)
}

func (s *Store) FinishTask(taskID, result, errMsg string, success bool) {
	s.mu.Lock()
	t, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return
	}
	t.FinishedAt = time.Now()
	t.Progress = 100
	if success {
		t.Status = "done"
		t.Result = result
		s.stats.Completed++
		// Only successful completions bump the worker's tally. Stability is
		// updated separately via RecordTaskResult from the caller (so a
		// failure can both fail-stability and report here as a failure).
		if t.WorkerID != "" {
			if w, ok := s.workers[t.WorkerID]; ok {
				w.TasksDone++
			}
		}
	} else {
		t.Status = "failed"
		t.Error = errMsg
		s.stats.Failed++
	}
	tCopy := *t
	tCopy.Logs = append([]string{}, t.Logs...)
	s.mu.Unlock()
	s.notify("task_done", &tCopy)
	s.notify("stats_updated", s.StatsSnapshot())
}

// SweepTimeouts marks running tasks whose StartedAt + TimeoutSecs < now as failed.
// Called by a background goroutine every 30s. Default timeout if 0.
//
// Returns the list of tasks that were just timed out AND have retries left, so
// the caller can re-dispatch them. Tasks that already exhausted retries are
// simply marked failed.
func (s *Store) RemoveTask(id string) *Task {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.tasks, id)
	cp := *t
	logsCopy := append([]string{}, t.Logs...)
	cp.Logs = logsCopy
	s.mu.Unlock()
	s.notify("task_deleted", &cp)
	return &cp
}

func (s *Store) SweepTimeouts(defaultTimeout time.Duration) (retriable []*Task) {
	s.mu.RLock()
	candidates := make([]*Task, 0)
	now := time.Now()
	for _, t := range s.tasks {
		if t.Status != "running" {
			continue
		}
		if t.StartedAt.IsZero() {
			continue
		}
		timeout := defaultTimeout
		if t.TimeoutSecs > 0 {
			timeout = time.Duration(t.TimeoutSecs) * time.Second
		}
		if now.Sub(t.StartedAt) > timeout {
			candidates = append(candidates, t)
		}
	}
	s.mu.RUnlock()
	for _, t := range candidates {
		s.TimeoutTask(t.ID, "task exceeded timeout")
		if t.Retries < t.MaxRetries {
			retriable = append(retriable, t)
		}
	}
	return retriable
}

// TimeoutTask is like FailTaskDirect but tagged with a timeout reason, and
// unlike FailTaskDirect it DOES count as a real failure (worker ran past its
// allotted time → that is a task outcome).
func (s *Store) TimeoutTask(taskID, reason string) {
	s.mu.Lock()
	t, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return
	}
	t.Status = "failed"
	t.Error = reason
	t.FinishedAt = time.Now()
	t.Progress = 100
	tCopy := *t
	tCopy.Logs = append([]string{}, t.Logs...)
	wID := t.WorkerID
	s.stats.Failed++
	s.mu.Unlock()
	s.notify("task_done", &tCopy)
	s.notify("stats_updated", s.StatsSnapshot())
	if wID != "" {
		s.RecordTaskResult(wID, false)
	}
}

// CancelTask signals the in-flight task to abort. The worker checks its cancel
// channel between progress updates and aborts cooperatively. Returns true if a
// cancel signal was delivered.
func (s *Store) CancelTask(taskID string) bool {
	s.mu.Lock()
	t, ok := s.tasks[taskID]
	if !ok || t.Status != "running" {
		s.mu.Unlock()
		return false
	}
	if t.cancelCh != nil {
		close(t.cancelCh)
		t.cancelCh = nil
	}
	s.mu.Unlock()
	return true
}

// GetTaskCancelCh returns the per-task cancellation channel for a worker to
// wait on. Returns nil if task not running or channel not set up.
func (s *Store) GetTaskCancelCh(taskID string) chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return nil
	}
	return t.cancelCh
}

// TaskCancelCh creates and stores a new cancellation channel for a task.
// Called when a task transitions to running.
func (s *Store) TaskCancelCh(taskID string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return nil
	}
	ch := make(chan struct{})
	t.cancelCh = ch
	return ch
}

func (s *Store) GetTask(id string) *Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil
	}
	cp := *t
	cp.Logs = append([]string{}, t.Logs...)
	return &cp
}

func (s *Store) ListTasks() []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		cp := *t
		cp.Logs = append([]string{}, t.Logs...)
		out = append(out, &cp)
	}
	return out
}

// FailTaskDirect marks a task as failed WITHOUT recording Stats and WITHOUT
// touching worker.TasksDone. Two reasons this is intentionally silent:
//
//   - Used at gateway restart to settle in-flight tasks whose original
//     worker is unlikely to still be alive. These tasks were already
//     counted in the prior session's persisted Stats — re-counting them
//     here would double-count.
//   - Runtime failures go through FinishTask(false) or TimeoutTask, both
//     of which DO bump Stats.Failed and emit stats_updated.
//
// Do NOT call this at runtime as a shortcut to "fail a task" — use
// FinishTask(false) instead.
func (s *Store) FailTaskDirect(taskID, errMsg string) {
	s.mu.Lock()
	t, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return
	}
	t.Status = "failed"
	t.Error = errMsg
	t.FinishedAt = time.Now()
	t.Progress = 100
	tCopy := *t
	tCopy.Logs = append([]string{}, t.Logs...)
	s.mu.Unlock()
	s.notify("task_done", &tCopy)
}

// SweepOffline marks workers without heartbeat for >timeout as offline.
func (s *Store) SweepOffline(timeout time.Duration) {
	s.mu.RLock()
	var stale []*Worker
	now := time.Now()
	for _, w := range s.workers {
		if w.Status != "offline" && now.Sub(w.LastSeen) > timeout {
			stale = append(stale, w)
		}
	}
	s.mu.RUnlock()
	for _, w := range stale {
		s.MarkOffline(w.ID)
	}
}
