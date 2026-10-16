package localtasks

import (
	"context"
	"encoding/csv"
	"os/exec"
	"strings"
	"sync"
)

// ProcInfo is a single matching process. Cmdline is best-effort: on Windows
// the cheap path (tasklist) only gives us process name + PID; we fall back
// to wmic when the operator actually wants the cmdline (rare — most rules
// match on name alone).
type ProcInfo struct {
	PID     int32
	Name    string
	Cmdline string
}

// listByNamePattern enumerates visible processes and returns those whose
// name matches one of the candidate substrings (case-insensitive).
//
// Implementation: uses the Windows built-in `tasklist /fo csv /nh` so we
// don't depend on any third-party Go module. Returns nil + nil if the
// command isn't available (non-Windows) or fails (cross-user permission).
func listByNamePattern(ctx context.Context, candidates ...string) ([]ProcInfo, error) {
	rows, err := tasklistCSV(ctx)
	if err != nil {
		return nil, nil
	}
	out := make([]ProcInfo, 0, 4)
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		name := strings.TrimSpace(row[0])
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		for _, cand := range candidates {
			if strings.Contains(lower, strings.ToLower(cand)) {
				pid, _ := atoi32(row[1])
				out = append(out, ProcInfo{
					PID:  pid,
					Name: name,
					// Cmdline left empty — populated lazily by
					// attachCmdline() if a rule needs it.
				})
				break
			}
		}
	}
	return out, nil
}

// listFFmpeg returns processes whose name contains "ffmpeg".
func listFFmpeg(ctx context.Context) ([]ProcInfo, error) {
	return listByNamePattern(ctx, "ffmpeg")
}

// listBlender returns processes whose name contains "blender".
func listBlender(ctx context.Context) ([]ProcInfo, error) {
	return listByNamePattern(ctx, "blender")
}

// wmicCache caches the (expensive) wmic command-line lookup so a scan
// that finds 5 matching processes doesn't shell out 5 times.
var (
	wmicCacheMu  sync.Mutex
	wmicCache    map[int32]string // pid → cmdline (best-effort)
	wmicCacheHit bool
)

// attachCmdline populates p.Cmdline from the wmic cache, loading it lazily.
// Returns p with Cmdline filled in (possibly empty if wmic is unavailable).
func attachCmdline(p ProcInfo) ProcInfo {
	if p.PID == 0 {
		return p
	}
	wmicCacheMu.Lock()
	defer wmicCacheMu.Unlock()
	if !wmicCacheHit {
		wmicCache = loadWmicCmdline()
		wmicCacheHit = true
	}
	if cl, ok := wmicCache[p.PID]; ok {
		p.Cmdline = truncate(cl, 8*1024)
	}
	return p
}

// loadWmicCmdline shells out to wmic once per scan and parses the CSV.
// Returns nil if wmic isn't on PATH (e.g. some Windows Server Core builds
// or when running under a stripped-down shell).
func loadWmicCmdline() map[int32]string {
	out, err := exec.Command("wmic", "process", "get", "ProcessId,CommandLine", "/format:csv").Output()
	if err != nil {
		return nil
	}
	r := csv.NewReader(strings.NewReader(string(out)))
	rows, err := r.ReadAll()
	if err != nil {
		return nil
	}
	// header: Node,CommandLine,ProcessId
	out2 := make(map[int32]string, len(rows))
	for _, row := range rows[1:] {
		if len(row) < 3 {
			continue
		}
		cl := strings.TrimSpace(row[1])
		pid, _ := atoi32(row[2])
		if pid > 0 && cl != "" {
			out2[pid] = cl
		}
	}
	return out2
}

// tasklistCSV runs `tasklist /fo csv /nh` and parses the output.
// Columns: "Image Name","PID","Session Name","Session#","Mem Usage".
// /nh strips the header row.
func tasklistCSV(ctx context.Context) ([][]string, error) {
	cmd := exec.CommandContext(ctx, "tasklist", "/fo", "csv", "/nh")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	r := csv.NewReader(strings.NewReader(string(out)))
	return r.ReadAll()
}

// atoi32 is a tiny helper used by the CSV parser. Returns 0 on parse error.
func atoi32(s string) (int32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	var n int32
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, nil
		}
		n = n*10 + int32(c-'0')
	}
	return n, nil
}

// truncate clamps s to at most n bytes. Used to keep payload templates
// within the gateway's 64 KB body limit when the cmdline of a long-running
// process is included verbatim.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
