package internal

import "time"

// ScoreWorker scores a worker for suitability to run a task of type t.
// Higher score = better candidate. Returns -1 if worker is not eligible.
// A worker whose Type is a prefix of taskType (separated by "_") is eligible.
func workerMatchesType(w *Worker, taskType string) bool {
	if w.Type == taskType {
		return true
	}
	// accept "code_format" -> worker.Type=="code", "doc_summary" -> "doc", etc.
	prefix := taskType
	if i := indexByte(taskType, '_'); i >= 0 {
		prefix = taskType[:i]
	}
	return w.Type == prefix
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// ScoreWorker scores a worker for suitability to run a task of type t.
// Higher score = better candidate. Returns -1 if worker is not eligible.
func ScoreWorker(w *Worker, taskType string) float64 {
	if !workerMatchesType(w, taskType) {
		return -1
	}
	if w.Status == "offline" {
		return -1
	}
	cpuFree := 100 - w.CPUUsage
	if cpuFree < 0 {
		cpuFree = 0
	}
	memFree := 100 - w.MemUsage
	if memFree < 0 {
		memFree = 0
	}
	queueScore := 100 - float64(w.QueueLen*20)
	if queueScore < 0 {
		queueScore = 0
	}
	return 0.5*cpuFree + 0.3*memFree + 0.2*queueScore
}

// PickWorker selects the best worker for the given task type.
func (s *Store) PickWorker(taskType string) *Worker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *Worker
	var bestScore float64 = -1
	for _, w := range s.workers {
		score := ScoreWorker(w, taskType)
		if score > bestScore {
			bestScore = score
			cp := *w
			best = &cp
		}
	}
	return best
}

// StartOfflineSweeper marks workers as offline if they miss heartbeats.
func (s *Store) StartOfflineSweeper(timeout, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			s.SweepOffline(timeout)
		}
	}()
}
