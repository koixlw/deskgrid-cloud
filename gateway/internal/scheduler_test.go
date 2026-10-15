package internal

import "testing"

// makeWorker returns a Worker with the given caps + cores + memory + GPU.
// Status defaults to "online"; the caller can override by mutating the returned
// pointer.
func makeWorker(id, wtype, name string, caps []string, cores, mem int, gpuUsed, gpuTotal int64) *Worker {
	return &Worker{
		ID:            id,
		Type:          wtype,
		Name:          name,
		Cores:         cores,
		MemoryMB:      mem,
		GPUMemUsedMB:  gpuUsed,
		GPUMemTotalMB: gpuTotal,
		Capabilities:  caps,
		Status:        "online",
		Stability:     1.0,
	}
}

func TestWorkerMeetsRequirements_NoConstraints(t *testing.T) {
	w := makeWorker("w1", "exec", "exec-1", []string{"python"}, 4, 8192, 0, 0)
	if !WorkerMeetsRequirements(w, TaskRequirements{}) {
		t.Fatal("zero-value requirements should be a no-op (worker qualifies)")
	}
}

// newStoreWithNoopNotifier returns a Store whose notifier is wired up to a
// safe no-op. In production main.go always calls SetNotifier before
// AddWorker; tests must do the same or AddWorker panics on s.notify.
func newStoreWithNoopNotifier() *Store {
	s := NewStore()
	s.SetNotifier(func(string, interface{}) {})
	return s
}

func TestWorkerMeetsRequirements_RequireCaps(t *testing.T) {
	w := makeWorker("w1", "exec", "exec-1", []string{"python", "ffmpeg"}, 4, 8192, 0, 0)
	// All caps present → pass
	if !WorkerMeetsRequirements(w, TaskRequirements{RequireCaps: []string{"ffmpeg", "python"}}) {
		t.Fatal("worker has all required caps, should qualify")
	}
	// Missing one → fail
	if WorkerMeetsRequirements(w, TaskRequirements{RequireCaps: []string{"ffmpeg", "edge-tts"}}) {
		t.Fatal("worker is missing edge-tts, should NOT qualify")
	}
}

func TestWorkerMeetsRequirements_MinCoresAndMemory(t *testing.T) {
	w := makeWorker("w1", "exec", "exec-1", []string{"python"}, 4, 8192, 0, 0)
	if !WorkerMeetsRequirements(w, TaskRequirements{MinCores: 4, MinMemoryMB: 8192}) {
		t.Fatal("worker matches its own thresholds")
	}
	if WorkerMeetsRequirements(w, TaskRequirements{MinCores: 5}) {
		t.Fatal("worker has 4 cores, min=5 should fail")
	}
	if WorkerMeetsRequirements(w, TaskRequirements{MinMemoryMB: 16384}) {
		t.Fatal("worker has 8GB, min=12GB should fail")
	}
}

func TestWorkerMeetsRequirements_MinGPUFreeMB(t *testing.T) {
	wGPU := makeWorker("wG", "exec", "exec-G", []string{"python"}, 4, 8192, 2048, 8192)
	wNoGPU := makeWorker("wN", "exec", "exec-N", []string{"python"}, 4, 8192, 0, 0)
	// wGPU has 6144MB free → 4096 passes, 8192 fails
	if !WorkerMeetsRequirements(wGPU, TaskRequirements{MinGPUFreeMB: 4096}) {
		t.Fatal("wGPU has 6144 free, 4096 should pass")
	}
	if WorkerMeetsRequirements(wGPU, TaskRequirements{MinGPUFreeMB: 8192}) {
		t.Fatal("wGPU has only 6144 free, 8192 should fail")
	}
	// wNoGPU has no GPU at all → any non-zero MinGPUFreeMB fails
	if WorkerMeetsRequirements(wNoGPU, TaskRequirements{MinGPUFreeMB: 1}) {
		t.Fatal("wNoGPU has no GPU, min=1 should fail")
	}
}

func TestPickWorker_HardFilterByCapability(t *testing.T) {
	s := newStoreWithNoopNotifier()
	s.AddWorker(makeWorker("w1", "exec", "exec-1", []string{"python", "ffmpeg"}, 4, 8192, 0, 0))
	s.AddWorker(makeWorker("w2", "exec", "exec-2", []string{"python"}, 4, 8192, 0, 0))

	// No requirement: pickWorker picks deterministically by ordering since both
	// have the same score; just verify it returns a worker (any one).
	if got := s.PickWorker("exec_run", TaskRequirements{}); got == nil {
		t.Fatal("expected a worker with no requirements")
	}

	// Require ffmpeg → must pick w1
	got := s.PickWorker("exec_run", TaskRequirements{RequireCaps: []string{"ffmpeg"}})
	if got == nil || got.ID != "w1" {
		t.Fatalf("expected w1 (has ffmpeg), got %+v", got)
	}
}

func TestPickWorker_HardFilterByResource(t *testing.T) {
	s := newStoreWithNoopNotifier()
	s.AddWorker(makeWorker("small", "exec", "exec-s", []string{"python"}, 2, 4096, 0, 0))
	s.AddWorker(makeWorker("big",   "exec", "exec-b", []string{"python"}, 8, 16384, 0, 0))

	// MinCores=4 → big wins
	got := s.PickWorker("exec_run", TaskRequirements{MinCores: 4})
	if got == nil || got.ID != "big" {
		t.Fatalf("expected 'big' for min_cores=4, got %+v", got)
	}

	// MinCores=16 → no worker
	if got := s.PickWorker("exec_run", TaskRequirements{MinCores: 16}); got != nil {
		t.Fatalf("expected nil for min_cores=16, got %+v", got)
	}
}

func TestPickWorker_ExcludeOnRetry(t *testing.T) {
	s := newStoreWithNoopNotifier()
	s.AddWorker(makeWorker("w1", "exec", "exec-1", []string{"python"}, 4, 8192, 0, 0))
	s.AddWorker(makeWorker("w2", "exec", "exec-2", []string{"python"}, 4, 8192, 0, 0))

	got := s.PickWorker("exec_run", TaskRequirements{}, "w1")
	if got == nil || got.ID != "w2" {
		t.Fatalf("exclude w1 should force w2, got %+v", got)
	}
}

func TestPickWorker_BackwardsCompatibleEmptyReq(t *testing.T) {
	// Old behaviour: no requirements → score = cpu/mem/gpu/queue/stability mix.
	s := newStoreWithNoopNotifier()
	s.AddWorker(makeWorker("idle", "exec", "exec-i", []string{"python"}, 4, 8192, 0, 0))
	// All zero load + stability=1.0 → score ≈ 100
	got := s.PickWorker("exec_run", TaskRequirements{})
	if got == nil {
		t.Fatal("old zero-req path should still find a worker")
	}
}