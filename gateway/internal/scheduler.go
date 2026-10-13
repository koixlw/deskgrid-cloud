package internal

import "time"

// workerMatchesType returns true if w can serve taskType. A worker whose Type
// is a prefix of taskType (separated by "_") is eligible. e.g. "image" worker
// can serve "image_txt2img".
func workerMatchesType(w *Worker, taskType string) bool {
	if w.Type == taskType {
		return true
	}
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
//
// Formula:
//
//	cpuFree   = 100 - cpu_usage
//	memFree   = 100 - mem_usage
//	gpuFree   = 100 - gpu_used%   (or 100 if no GPU)
//	queueScore = 100 - min(queue*20, 100)
//	stability = 0.5 .. 1.0  (worker reliability factor)
//
//	score = 0.40 * cpuFree
//	      + 0.25 * memFree
//	      + 0.10 * gpuFree
//	      + 0.15 * queueScore
//	      + 0.10 * (stability * 100)
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

	gpuFree := 100.0
	if w.GPUMemTotalMB > 0 {
		used := float64(w.GPUMemUsedMB) / float64(w.GPUMemTotalMB) * 100
		gpuFree = 100 - used
		if gpuFree < 0 {
			gpuFree = 0
		}
	}

	stability := w.Stability
	if stability < 0.5 {
		stability = 0.5 // clamp to avoid total disqualification from one bad run
	}

	return 0.40*cpuFree +
		0.25*memFree +
		0.10*gpuFree +
		0.15*queueScore +
		0.10*stability*100
}

// PickWorker selects the best worker for the given task type. Optionally
// excludes a worker ID (used during re-dispatch on retry to avoid sending the
// same failing task back to the same node).
func (s *Store) PickWorker(taskType string, excludeWorkerID ...string) *Worker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	excluded := make(map[string]bool, len(excludeWorkerID))
	for _, id := range excludeWorkerID {
		excluded[id] = true
	}
	var best *Worker
	var bestScore float64 = -1
	for _, w := range s.workers {
		if excluded[w.ID] {
			continue
		}
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

// StartTimeoutSweeper fails running tasks whose StartedAt + TimeoutSecs is past.
// Re-dispatches retriable tasks to the next-best worker.
func (s *Store) StartTimeoutSweeper(interval time.Duration, dispatchRetryWithID func(taskID, excludeWorkerID string)) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			retriable := s.SweepTimeouts(0) // 0 = use per-task TimeoutSecs
			for _, task := range retriable {
				failedOn := task.WorkerID // capture before RetryTask clears it
				if s.RetryTask(task.ID) {
					Info("task %s retrying (now=%d, max=%d) after timeout on worker %s",
						task.ID, task.Retries, task.MaxRetries, failedOn)
					dispatchRetryWithID(task.ID, failedOn)
				}
			}
		}
	}()
}