package internal

import (
	"strings"
	"sync"
	"time"
)

// Cleanup settings. Both bounded so the in-memory state can't grow unbounded.
const (
	// MaxTasks is the hard cap on tasks kept in memory. Oldest done/failed
	// tasks are evicted when exceeded. Running tasks are never evicted.
	maxTasks = 1000

	// MaxWorkers is the cap on offline workers kept in memory. Oldest offline
	// workers are evicted when exceeded. Online workers are never evicted.
	maxWorkers = 200

	// MaxResultBytes bounds how much of a task result we keep in memory. Anything
	// larger is truncated with a marker so the panel can show "[truncated]".
	maxResultBytes = 256 * 1024

	// TaskTTL is how long completed tasks are kept before eviction. This bounds
	// the on-disk persistence too — old tasks don't accumulate in data/*.json.
	taskTTL = 24 * time.Hour
)

// EvictOldTasks drops oldest done/failed tasks when count exceeds maxTasks, and
// also drops tasks older than TaskTTL regardless of count. Running tasks are
// always preserved. Called periodically by a background goroutine.
func (s *Store) EvictOldTasks() (evicted int) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	// First pass: TTL eviction (done/failed only).
	for id, t := range s.tasks {
		if t.Status == "running" || t.Status == "pending" {
			continue
		}
		if !t.FinishedAt.IsZero() && now.Sub(t.FinishedAt) > taskTTL {
			delete(s.tasks, id)
			evicted++
		}
	}

	// Second pass: cap by count. Sort candidates by FinishedAt and drop oldest
	// until under cap.
	if len(s.tasks) <= maxTasks {
		return evicted
	}
	type cand struct {
		id    string
		fin   time.Time
	}
	var done []*cand
	for id, t := range s.tasks {
		if t.Status == "running" || t.Status == "pending" {
			continue
		}
		done = append(done, &cand{id: id, fin: t.FinishedAt})
	}
	// sort oldest first; drop until under cap
	for i := 1; i < len(done); i++ {
		j := i
		for j > 0 && done[j].fin.Before(done[j-1].fin) {
			done[j-1], done[j] = done[j], done[j-1]
			j--
		}
	}
	overflow := len(s.tasks) - maxTasks
	for i := 0; i < len(done) && overflow > 0; i++ {
		delete(s.tasks, done[i].id)
		evicted++
		overflow--
	}
	return evicted
}

// EvictOldWorkers drops oldest offline workers when count exceeds maxWorkers.
// Online workers are always preserved.
func (s *Store) EvictOldWorkers() (evicted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.workers) <= maxWorkers {
		return 0
	}
	type cand struct {
		id       string
		lastSeen time.Time
	}
	var off []*cand
	for id, w := range s.workers {
		if w.Status != "offline" {
			continue
		}
		off = append(off, &cand{id: id, lastSeen: w.LastSeen})
	}
	for i := 1; i < len(off); i++ {
		j := i
		for j > 0 && off[j].lastSeen.Before(off[j-1].lastSeen) {
			off[j-1], off[j] = off[j], off[j-1]
			j--
		}
	}
	overflow := len(s.workers) - maxWorkers
	for i := 0; i < len(off) && overflow > 0; i++ {
		delete(s.workers, off[i].id)
		evicted++
		overflow--
	}
	return evicted
}

// TruncateResult caps a result string at maxResultBytes, replacing the tail
// with a "[truncated]" marker so users know to look at the original file.
func TruncateResult(result string) string {
	if len(result) <= maxResultBytes {
		return result
	}
	keep := maxResultBytes - 32 // room for marker
	if keep < 0 {
		keep = 0
	}
	return result[:keep] + "...[truncated, original " +
		itoa(len(result)) + " bytes]"
}

// itoa avoids importing strconv for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b strings.Builder
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b.WriteByte(byte('0' + n%10))
		n /= 10
	}
	s := b.String()
	// reverse
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	if neg {
		return "-" + string(runes)
	}
	return string(runes)
}

// StartCleanupLoop runs EvictOldTasks / EvictOldWorkers periodically so the
// in-memory store can't grow without bound during long-running deployments.
func (s *Store) StartCleanupLoop(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if n := s.EvictOldTasks(); n > 0 {
				Info("cleanup: evicted %d old tasks", n)
			}
			if n := s.EvictOldWorkers(); n > 0 {
				Info("cleanup: evicted %d old workers", n)
			}
		}
	}()
}

// suppress unused import warnings for sync in some builds
var _ = sync.Mutex{}