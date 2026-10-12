package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Persistence is a debounced JSON snapshot of the store. Subscribes to store
// events and flushes full state to disk at most once per second.
//
// Two files written under <dir>:
//
//	workers.json - all workers
//	tasks.json   - all tasks (last 50 logs each, to keep file size sane)
type Persistence struct {
	mu      sync.Mutex
	dir     string
	store   *Store
	pending bool
	stopCh  chan struct{}
}

func NewPersistence(dir string, store *Store) *Persistence {
	return &Persistence{
		dir:    dir,
		store:  store,
		stopCh: make(chan struct{}),
	}
}

// OnEvent subscribes to store events. Marks state dirty.
func (p *Persistence) OnEvent(event string, payload interface{}) {
	p.mu.Lock()
	p.pending = true
	p.mu.Unlock()
}

// Run flushes to disk every second when there are pending changes.
func (p *Persistence) Run() {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			p.flush() // one last save on exit
			return
		case <-t.C:
			p.mu.Lock()
			pending := p.pending
			p.pending = false
			p.mu.Unlock()
			if pending {
				p.flush()
			}
		}
	}
}

// Stop signals Run() to exit.
func (p *Persistence) Stop() { close(p.stopCh) }

func (p *Persistence) flush() {
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return
	}
	workers, tasks := p.store.ListWorkers(), p.store.ListTasks()
	// trim task logs to keep file small
	for _, t := range tasks {
		if len(t.Logs) > 50 {
			t.Logs = t.Logs[len(t.Logs)-50:]
		}
	}
	wb, _ := json.MarshalIndent(workers, "", "  ")
	tb, _ := json.MarshalIndent(tasks, "", "  ")
	atomicWrite(filepath.Join(p.dir, "workers.json"), wb)
	atomicWrite(filepath.Join(p.dir, "tasks.json"), tb)
}

func atomicWrite(path string, data []byte) error {
	if err := os.WriteFile(path+".tmp", data, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Load restores state from disk.
//
// Returns workers + tasks that were saved + IDs of in-flight tasks (running or
// pending) that the caller should mark as failed because their original worker
// is unlikely to still be alive after gateway restart.
func Load(dir string) (workers []*Worker, tasks []*Task, inFlight []string) {
	if data, err := os.ReadFile(filepath.Join(dir, "workers.json")); err == nil {
		_ = json.Unmarshal(data, &workers)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "tasks.json")); err == nil {
		_ = json.Unmarshal(data, &tasks)
		for _, t := range tasks {
			if t.Status == "running" || t.Status == "pending" {
				inFlight = append(inFlight, t.ID)
			}
		}
	}
	return
}