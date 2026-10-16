package localtasks

import (
	"context"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// Registry holds the live snapshot of detected LocalTasks plus the YAML
// config for custom rules. It is safe for concurrent use: Snapshot/Get are
// wait-free under RLock; Scan/Reload take the write lock for the duration
// of their work (so the snapshot is consistent across readers during the
// swap).
type Registry struct {
	mu          sync.RWMutex
	snapshot    map[string]*LocalTask // id → entry
	cfgPath     string
	yamlRules   []CustomRule
	interval    time.Duration
	maxRules    int
}

// NewRegistry constructs a registry. customCfgPath may be "" — the registry
// will then load no YAML rules and rely entirely on the built-in probes.
// interval ≤ 0 falls back to scanIntervalDefault.
func NewRegistry(customCfgPath string, interval time.Duration) *Registry {
	if interval <= 0 {
		interval = scanIntervalDefault
	}
	r := &Registry{
		snapshot: make(map[string]*LocalTask),
		cfgPath:  customCfgPath,
		interval: interval,
		maxRules: 50,
	}
	r.yamlRules = r.loadYAML()
	return r
}

// Snapshot returns a stable, sorted copy of the current entries.
// Caller may mutate the returned slice; the registry's internal state is
// not exposed.
func (r *Registry) Snapshot() []LocalTask {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]LocalTask, 0, len(r.snapshot))
	for _, lt := range r.snapshot {
		out = append(out, *lt)
	}
	sort.SliceStable(out, func(i, j int) bool {
		// throwable first (has map_to_type), then busy, then by service name
		a, b := out[i], out[j]
		aT, bT := a.MapToType != "", b.MapToType != ""
		if aT != bT {
			return aT
		}
		if a.Status != b.Status {
			return a.Status == StatusBusy
		}
		return a.Service < b.Service
	})
	return out
}

// Get fetches a single entry by id. Used by the dispatch handler.
func (r *Registry) Get(id string) (*LocalTask, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	lt, ok := r.snapshot[id]
	if !ok {
		return nil, false
	}
	cp := *lt
	return &cp, true
}

// Reload re-reads the YAML config. Safe to call from any goroutine; takes
// the write lock briefly to swap the rule slice. Active scans mid-flight
// keep their already-loaded rule set.
func (r *Registry) Reload() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.yamlRules = r.loadYAMLLocked()
}

// loadYAML (write-locked) reads + parses the YAML file. Missing file is
// non-fatal — registry runs with zero custom rules.
func (r *Registry) loadYAMLLocked() []CustomRule {
	if r.cfgPath == "" {
		return nil
	}
	b, err := os.ReadFile(r.cfgPath)
	if err != nil {
		if !os.IsNotExist(err) {
			// log only — caller doesn't need to know
			_ = err
		}
		return nil
	}
	var cfg YAMLConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	if len(cfg.Rules) > r.maxRules {
		cfg.Rules = cfg.Rules[:r.maxRules]
	}
	return cfg.Rules
}

// loadYAML is the unlocked variant for NewRegistry's startup call.
func (r *Registry) loadYAML() []CustomRule {
	return r.loadYAMLLocked()
}

// Start launches the background scan loop. Returns immediately; the loop
// exits when ctx is cancelled. The first scan runs synchronously before the
// ticker fires so the first GET /api/local-tasks returns data.
func (r *Registry) Start(ctx context.Context) {
	// initial scan — log errors via the package's Info if available; we
	// just swallow them so startup never blocks on a scanner hiccup.
	_ = r.Scan(ctx)
	go r.loop(ctx)
}

func (r *Registry) loop(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = r.Scan(ctx)
		}
	}
}

// Scan runs one full detection cycle and atomically replaces the snapshot.
// Public so the HTTP handler can force a refresh on demand.
func (r *Registry) Scan(ctx context.Context) error {
	r.mu.Lock()
	rules := append([]CustomRule(nil), r.yamlRules...) // copy so yaml reload can race
	r.mu.Unlock()

	now := time.Now()
	detected := make(map[string]*LocalTask, 8)

	// Port probes — run in parallel via a tiny inline goroutine fan-out.
	type portResult struct {
		svc   string
		port  int
		state mapperInput
		ok    bool
	}
	portJobs := []struct {
		svc  string
		port int
		probe func(context.Context, int) (mapperInput, bool)
	}{
		{SvcComfyUI, defaultComfyUIPort, func(c context.Context, p int) (mapperInput, bool) { return probeComfyUI(c, p) }},
		{SvcSDWebUI, defaultSDWebUIPort, func(c context.Context, p int) (mapperInput, bool) { return probeSDWebUI(c, p) }},
		{SvcOllama,  defaultOllamaPort,  func(c context.Context, p int) (mapperInput, bool) { return probeOllama(c, p) }},
	}
	portCh := make(chan portResult, len(portJobs))
	for _, job := range portJobs {
		job := job
		go func() {
			state, ok := job.probe(ctx, job.port)
			portCh <- portResult{svc: job.svc, port: job.port, state: state, ok: ok}
		}()
	}
	for i := 0; i < len(portJobs); i++ {
		res := <-portCh
		if !res.ok || res.state == nil {
			continue
		}
		typ, payload, ok := MapLocalTask(res.svc, res.state, CustomRule{})
		if !ok {
			// Service is up but idle — still show it (status=idle) so
			// the user knows it's there.
			entry := &LocalTask{
				ID:         uuid.NewString(),
				Service:    res.svc,
				Title:      titleFor(res.svc, res.port, 0),
				Port:       res.port,
				Status:     StatusIdle,
				DetectedAt: now,
				LastSeen:   now,
				SourceHint: SourcePort,
				Metadata:   metadataFor(res.state),
			}
			detected[entityKey(res.svc, res.port, 0)] = entry
			continue
			continue
		}
		status := StatusBusy
		// refine status from probe-specific signals
		switch s := res.state.(type) {
		case ComfyState:
			if s.QueueRemaining <= 0 {
				status = StatusIdle
			}
		case SDState:
			if s.Progress <= 0 && s.JobCount <= 0 {
				status = StatusIdle
			}
		case OllamaState:
			if !s.AnyRunning {
				status = StatusIdle
			}
		}
		entry := &LocalTask{
			ID:         uuid.NewString(),
			Service:    res.svc,
			Title:      titleFor(res.svc, res.port, 0),
			Port:       res.port,
			Status:     status,
			DetectedAt: now,
			LastSeen:   now,
			MapToType:  typ,
			PayloadTpl: payload,
			SourceHint: SourcePort,
			Metadata:   metadataFor(res.state),
		}
		detected[entityKey(res.svc, res.port, 0)] = entry
	}

	// Process probes — also parallel.
	type procResult struct {
		svc string
		list []ProcInfo
		err  error
	}
	procJobs := []struct {
		svc   string
		probe func(context.Context) ([]ProcInfo, error)
	}{
		{SvcFFmpeg, listFFmpeg},
		{SvcBlender, listBlender},
	}
	procCh := make(chan procResult, len(procJobs))
	for _, job := range procJobs {
		job := job
		go func() {
			list, err := job.probe(ctx)
			procCh <- procResult{svc: job.svc, list: list, err: err}
		}()
	}
	for i := 0; i < len(procJobs); i++ {
		res := <-procCh
		if res.err != nil || len(res.list) == 0 {
			continue
		}
		for _, p := range res.list {
			// Populate Cmdline lazily for the mapper (which inspects
			// flags like -vf subtitles). One wmic call per scan.
			p = attachCmdline(p)
			typ, payload, ok := MapLocalTask(res.svc, p, CustomRule{})
			if !ok {
				continue
			}
			entry := &LocalTask{
				ID:         uuid.NewString(),
				Service:    res.svc,
				Title:      titleFor(res.svc, 0, int(p.PID)),
				PID:        p.PID,
				Status:     StatusBusy,
				DetectedAt: now,
				LastSeen:   now,
				MapToType:  typ,
				PayloadTpl: payload,
				SourceHint: SourceProcess,
				Metadata:   metadataFor(p),
			}
			detected[entityKey(res.svc, 0, p.PID)] = entry
		}
	}

	// Custom YAML rules — process-name patterns become ProcInfo-like entries.
	for _, rule := range rules {
		if rule.NamePattern != "" {
			list, _ := listByNamePattern(ctx, rule.NamePattern)
			for _, p := range list {
				p = attachCmdline(p) // populate {{cmdline}} if rule uses it
				payload := renderTemplate(rule.PayloadTemplate, p, 0)
				entry := &LocalTask{
					ID:         uuid.NewString(),
					Service:    rule.Name,
					Title:      titleFor(rule.Name, 0, int(p.PID)),
					PID:        p.PID,
					Status:     StatusBusy,
					DetectedAt: now,
					LastSeen:   now,
					MapToType:  rule.Type,
					PayloadTpl: payload,
					SourceHint: SourceYAML,
					Metadata: map[string]any{
						"name":    p.Name,
						"cmdline": p.Cmdline,
					},
				}
				detected[entityKey(rule.Name, 0, p.PID)] = entry
			}
		} else if rule.Port > 0 {
			// port-based custom rule — single probe
			st, ok := genericPortProbe(ctx, rule.Port)
			if !ok {
				continue
			}
			entry := &LocalTask{
				ID:         uuid.NewString(),
				Service:    rule.Name,
				Title:      titleFor(rule.Name, rule.Port, 0),
				Port:       rule.Port,
				Status:     StatusBusy,
				DetectedAt: now,
				LastSeen:   now,
				MapToType:  rule.Type,
				PayloadTpl: renderTemplate(rule.PayloadTemplate, ProcInfo{}, rule.Port),
				SourceHint: SourceYAML,
				Metadata:   map[string]any{"raw": st},
			}
			detected[entityKey(rule.Name, rule.Port, 0)] = entry
		}
	}

	// Merge with previous snapshot to preserve IDs across rescans.
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]*LocalTask, len(detected))
	for key, fresh := range detected {
		// Try to find an existing entry with the same key, to preserve
		// its UUID so the UI doesn't churn.
		var existingID string
		for _, prev := range r.snapshot {
			if entityKey(prev.Service, prev.Port, prev.PID) == key {
				existingID = prev.ID
				break
			}
		}
		if existingID != "" {
			fresh.ID = existingID
		}
		next[existingIDOrNew(fresh)] = fresh
	}
	// Drop entries that have not been re-seen within disappearanceTTL.
	for id, prev := range r.snapshot {
		if _, stillThere := next[id]; stillThere {
			continue
		}
		if now.Sub(prev.LastSeen) > disappearanceTTL {
			continue // truly dropped
		}
		// still within TTL — keep as-is with refreshed LastSeen=prev
		next[id] = prev
	}
	r.snapshot = next
	return nil
}

// entityKey is the stable identity of a detected entity across rescans.
// Same service + same port + same pid → same entry, same UUID.
func entityKey(svc string, port int, pid int32) string {
	if port > 0 {
		return svc + "@" + itoa(port)
	}
	if pid > 0 {
		return svc + "#" + itoa(int(pid))
	}
	return svc + "@?"
}

func existingIDOrNew(lt *LocalTask) string {
	if lt.ID != "" {
		return lt.ID
	}
	lt.ID = uuid.NewString()
	return lt.ID
}

func titleFor(svc string, port, pid int) string {
	switch {
	case port > 0:
		return svc + " @ :" + itoa(port)
	case pid > 0:
		return svc + " (pid " + itoa(pid) + ")"
	default:
		return svc
	}
}

func metadataFor(v any) map[string]any {
	out := map[string]any{}
	switch s := v.(type) {
	case ComfyState:
		out["queue_remaining"] = s.QueueRemaining
		if s.Raw != nil {
			out["raw"] = s.Raw
		}
	case SDState:
		out["progress"] = s.Progress
		out["job_count"] = s.JobCount
		if s.ModelTitle != "" {
			out["model_title"] = s.ModelTitle
		}
		if s.ActivePrompt != "" {
			out["active_prompt"] = s.ActivePrompt
		}
	case OllamaState:
		out["models"] = s.Models
		out["any_running"] = s.AnyRunning
		out["total_vram"] = s.TotalVRAM
	case ProcInfo:
		out["pid"] = s.PID
		out["name"] = s.Name
		if len(s.Cmdline) < 4096 {
			out["cmdline"] = s.Cmdline
		}
	}
	return out
}

// genericPortProbe is a quick TCP-open + GET / for YAML-driven port rules.
// We don't know what service sits behind the port, so we just confirm it's
// open and capture the response body for the UI.
func genericPortProbe(ctx context.Context, port int) (map[string]any, bool) {
	if !tcpOpen(ctx, "127.0.0.1", port) {
		return nil, false
	}
	body, ok := httpGet(ctx, "http://127.0.0.1:"+itoa(port)+"/")
	if !ok {
		// port open but root unreachable — still call it "busy" so the
		// rule's payload template drives the dispatch
		return map[string]any{"open": true}, true
	}
	out := map[string]any{"open": true, "body_len": len(body)}
	return out, true
}

// itoa avoids importing strconv just for int-to-string in hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
