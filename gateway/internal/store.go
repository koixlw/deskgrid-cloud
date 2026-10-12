package internal

import (
	"sync"
	"time"
)

// Worker represents a compute node registered with the gateway.
type Worker struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"` // code | doc | csv
	Name      string    `json:"name"`
	CPU       string    `json:"cpu"`
	Cores     int       `json:"cores"`
	MemoryMB  int       `json:"memory_mb"`
	CPUUsage  float64   `json:"cpu_usage"`
	MemUsage  float64   `json:"mem_usage"`
	QueueLen  int       `json:"queue_len"`
	Status    string    `json:"status"` // online | busy | offline
	LastSeen  time.Time `json:"last_seen"`
	TasksDone int       `json:"tasks_done"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	RegisteredAt time.Time `json:"registered_at"`
}

// Task represents a unit of work dispatched to a worker.
type Task struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Payload    string    `json:"payload"`
	WorkerID   string    `json:"worker_id"`
	Status     string    `json:"status"` // pending | running | done | failed
	Progress   int       `json:"progress"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Result     string    `json:"result,omitempty"`
	Error      string    `json:"error,omitempty"`
	Logs       []string  `json:"logs"`
	Source     string    `json:"source"` // panel | openai
}

// Store is an in-memory store for workers, tasks, and event history.
type Store struct {
	mu      sync.RWMutex
	workers map[string]*Worker
	tasks   map[string]*Task
	// notify is the broadcast channel used by ws Hub
	notify func(event string, payload interface{})
}

func NewStore() *Store {
	return &Store{
		workers: make(map[string]*Worker),
		tasks:   make(map[string]*Task),
	}
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
		existing.LastSeen = time.Now()
		existing.Status = "online"
		s.mu.Unlock()
		s.notify("worker_updated", existing)
		return
	}
	w.RegisteredAt = time.Now()
	w.LastSeen = time.Now()
	w.Status = "online"
	s.workers[w.ID] = w
	s.mu.Unlock()
	s.notify("worker_added", w)
}

func (s *Store) UpdateHeartbeat(id string, cpu, mem float64, queueLen int) *Worker {
	s.mu.Lock()
	w, ok := s.workers[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	w.CPUUsage = cpu
	w.MemUsage = mem
	w.QueueLen = queueLen
	w.LastSeen = time.Now()
	if w.Status == "offline" {
		w.Status = "online"
	}
	wCopy := *w
	s.mu.Unlock()
	s.notify("worker_updated", &wCopy)
	return w
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
	tCopy := *t
	tCopy.Logs = append([]string{}, t.Logs...)
	s.mu.Unlock()
	s.notify("task_updated", &tCopy)
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
	} else {
		t.Status = "failed"
		t.Error = errMsg
	}
	if t.WorkerID != "" {
		if w, ok := s.workers[t.WorkerID]; ok {
			w.TasksDone++
		}
	}
	tCopy := *t
	tCopy.Logs = append([]string{}, t.Logs...)
	s.mu.Unlock()
	s.notify("task_done", &tCopy)
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

// FailTaskDirect marks a task as failed without bumping worker.TasksDone.
// Used at gateway restart to settle in-flight tasks whose workers may not
// still be alive.
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
