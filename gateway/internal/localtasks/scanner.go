package localtasks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// httpClient is reused across all probes to keep TCP connections warm and
// avoid TLS handshakes on each call. Timeout is intentionally short — a
// hung service must not stall the whole scan loop.
var httpClient = &http.Client{
	Timeout: probeTimeout,
}

// ComfyState is the parsed /prompt endpoint response. We only care about a
// few fields; raw JSON blobs are kept in Metadata for the UI to show.
type ComfyState struct {
	QueueRemaining int            `json:"queue_remaining"`
	FirstWorkflow  map[string]any `json:"first_workflow,omitempty"` // node-id -> {class_type, ...}
	Raw            map[string]any `json:"raw,omitempty"`            // for UI inspection
}

// SDState is the parsed /sdapi/v1/progress + /sdapi/v1/cmd-models state.
type SDState struct {
	Progress     float64 `json:"progress"`     // 0..1, 0 means idle
	JobCount     int     `json:"job_count"`    // jobs remaining in queue
	ActivePrompt string  `json:"active_prompt,omitempty"`
	ModelTitle   string  `json:"model_title,omitempty"`
}

// OllamaState is the parsed /api/ps state.
type OllamaState struct {
	Models       []string `json:"models"`         // loaded model names
	AnyRunning   bool     `json:"any_running"`    // any model with expires_at > now
	TotalVRAM    int64    `json:"total_vram"`     // bytes
}

// dialTimeout is used by the TCP probe that gates the HTTP call. If the
// port is closed, this returns within the timeout instead of waiting the
// full HTTP timeout.
const dialTimeout = 300 * time.Millisecond

// probeComfyUI hits http://127.0.0.1:<port>/prompt and returns the current
// queue + first queued workflow. Best-effort; errors → ok=false.
//
// We use a fast TCP dial gate so a closed port doesn't waste the full HTTP
// timeout. The HTTP call uses an independent context with probeTimeout.
func probeComfyUI(ctx context.Context, port int) (ComfyState, bool) {
	if !tcpOpen(ctx, "127.0.0.1", port) {
		return ComfyState{}, false
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/prompt", port)
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return ComfyState{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ComfyState{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return ComfyState{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return ComfyState{}, false
	}
	st := ComfyState{Raw: raw}
	// exec_info.queue_remaining
	if ei, ok := raw["exec_info"].(map[string]any); ok {
		switch v := ei["queue_remaining"].(type) {
		case float64:
			st.QueueRemaining = int(v)
		}
	}
	// first queued workflow: prompt[0] if non-empty
	if arr, ok := raw["prompt"].([]any); ok && len(arr) > 0 {
		if first, ok := arr[0].(map[string]any); ok {
			if wf, ok := first["prompt"].([]any); ok && len(wf) > 0 {
				if w0, ok := wf[0].(map[string]any); ok {
					if inputs, ok := w0["inputs"].(map[string]any); ok {
						st.FirstWorkflow = inputs
					} else {
						st.FirstWorkflow = w0
					}
				}
			}
		}
	}
	return st, true
}

// probeSDWebUI hits /sdapi/v1/progress (current job %) and optionally
// /sdapi/v1/cmd-models for the loaded model name. Returns ok=false if the
// port is closed or /progress returns non-200.
func probeSDWebUI(ctx context.Context, port int) (SDState, bool) {
	if !tcpOpen(ctx, "127.0.0.1", port) {
		return SDState{}, false
	}
	st := SDState{}
	// 1) progress
	if body, ok := httpGet(ctx, fmt.Sprintf("http://127.0.0.1:%d/sdapi/v1/progress", port)); ok {
		var raw map[string]any
		if json.Unmarshal(body, &raw) == nil {
			if v, ok := raw["progress"].(float64); ok {
				st.Progress = v
			}
			// active prompt from current payload, if exposed
			if pj, ok := raw["state"].(map[string]any); ok {
				if job, ok := pj["job"].(string); ok && job != "" {
					st.ActivePrompt = job
				}
				if jobCount, ok := pj["job_count"].(float64); ok {
					st.JobCount = int(jobCount)
				}
			}
		}
	} else {
		return SDState{}, false
	}
	// 2) model title (optional, for UI display)
	if body, ok := httpGet(ctx, fmt.Sprintf("http://127.0.0.1:%d/sdapi/v1/cmd-models", port)); ok {
		var arr []map[string]any
		if json.Unmarshal(body, &arr) == nil {
			for _, m := range arr {
				if t, ok := m["title"].(string); ok && t != "" {
					st.ModelTitle = t
					break
				}
			}
		}
	}
	if st.Progress == 0 && st.JobCount == 0 {
		// Port is up but no progress endpoint gave us anything — count
		// as idle rather than unknown so the UI can still show it.
	}
	return st, true
}

// probeOllama hits /api/ps and reports loaded models. Returns ok=false if
// port closed.
func probeOllama(ctx context.Context, port int) (OllamaState, bool) {
	if !tcpOpen(ctx, "127.0.0.1", port) {
		return OllamaState{}, false
	}
	body, ok := httpGet(ctx, fmt.Sprintf("http://127.0.0.1:%d/api/ps", port))
	if !ok {
		return OllamaState{}, false
	}
	var raw struct {
		Models []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			Expires  string `json:"expires_at"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return OllamaState{}, false
	}
	st := OllamaState{}
	for _, m := range raw.Models {
		st.Models = append(st.Models, m.Name)
		st.TotalVRAM += m.Size
		// Ollama's expires_at is an RFC3339 timestamp; treat anything in
		// the future as "still loaded".
		if !st.AnyRunning {
			if t, err := time.Parse(time.RFC3339Nano, m.Expires); err == nil {
				if t.After(time.Now()) {
					st.AnyRunning = true
				}
			}
		}
	}
	return st, true
}

// tcpOpen is a fast gate: returns true iff the TCP port on 127.0.0.1
// accepted a SYN within dialTimeout. Cheap, no data exchange.
func tcpOpen(ctx context.Context, host string, port int) bool {
	d := net.Dialer{Timeout: dialTimeout}
	cctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := d.DialContext(cctx, "tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// httpGet is the common path for the three probes: GET with the shared
// client, read up to 64 KB, return raw bytes + ok flag.
func httpGet(ctx context.Context, url string) ([]byte, bool) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, false
	}
	return b, true
}
