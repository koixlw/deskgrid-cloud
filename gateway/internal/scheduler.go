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

// contains is a tiny string-slice membership check; avoids pulling in
// slices.Contains for a 6-line hot path.
func contains(haystack []string, needle string) bool {
	for _, x := range haystack {
		if x == needle {
			return true
		}
	}
	return false
}

// WorkerMeetsRequirements returns true if w satisfies all the hard
// constraints in req. Zero / empty fields in req are treated as "no limit", so
// this is a no-op for tasks submitted without a requirements struct
// (backwards compatible with Phase 0 clients).
//
// Constraint    | Worker must satisfy
//---------------|------------------------------------------
// RequireCaps   | has every listed cap in W.Capabilities
// MinCores      | W.Cores >= MinCores (only checked if > 0)
// MinMemoryMB   | W.MemoryMB >= MinMemoryMB (only checked if > 0)
// MinGPUFreeMB  | W.GPUMemTotalMB > 0 AND (Total - Used) >= MinGPUFreeMB
//               | (workers without a GPU cannot satisfy a non-zero min)
func WorkerMeetsRequirements(w *Worker, req TaskRequirements) bool {
	if w == nil {
		return false
	}
	for _, c := range req.RequireCaps {
		if !contains(w.Capabilities, c) {
			return false
		}
	}
	if req.MinCores > 0 && w.Cores < req.MinCores {
		return false
	}
	if req.MinMemoryMB > 0 && w.MemoryMB < req.MinMemoryMB {
		return false
	}
	if req.MinGPUFreeMB > 0 {
		if w.GPUMemTotalMB <= 0 {
			return false
		}
		free := w.GPUMemTotalMB - w.GPUMemUsedMB
		if free < req.MinGPUFreeMB {
			return false
		}
	}
	return true
}

// ScoreWorker scores a worker for suitability to run a task of type t.
// Higher score = better candidate. Returns -1 if worker is not eligible
// (wrong type, offline, or fails WorkerMeetsRequirements).
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
func ScoreWorker(w *Worker, taskType string, req TaskRequirements) float64 {
	if !workerMatchesType(w, taskType) {
		return -1
	}
	if w.Status == "offline" {
		return -1
	}
	if !WorkerMeetsRequirements(w, req) {
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

// PickWorker selects the best worker for the given task type and hard
// requirements. Optionally excludes a worker ID (used during re-dispatch on
// retry to avoid sending the same failing task back to the same node).
func (s *Store) PickWorker(taskType string, req TaskRequirements, excludeWorkerID ...string) *Worker {
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
		score := ScoreWorker(w, taskType, req)
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