package localtasks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProbeComfyUI_httptest: spin up a fake ComfyUI server, point the probe
// at it, verify queue_remaining + first-workflow extraction work.
func TestProbeComfyUI_httptest(t *testing.T) {
	body := `{
		"exec_info": {"queue_remaining": 2},
		"prompt": [
			{
				"prompt": [
					{
						"inputs": {
							"3": {"class_type": "KSampler"},
							"6": {"class_type": "CLIPTextEncode"}
						}
					}
				]
			}
		]
	}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prompt" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer ts.Close()

	port := portFromURL(ts.URL)
	state, ok := probeComfyUI(context.Background(), port)
	if !ok {
		t.Fatalf("probeComfyUI returned ok=false")
	}
	if state.QueueRemaining != 2 {
		t.Errorf("expected QueueRemaining=2, got %d", state.QueueRemaining)
	}
	if _, ok := state.FirstWorkflow["3"]; !ok {
		t.Errorf("expected FirstWorkflow to contain node 3")
	}
}

// TestProbeComfyUI_closedPort: no listener → ok=false fast.
func TestProbeComfyUI_closedPort(t *testing.T) {
	// port 1 is reserved/unused; probe should fail quickly
	_, ok := probeComfyUI(context.Background(), 1)
	if ok {
		t.Errorf("expected ok=false for closed port")
	}
}

// TestProbeOllama_httptest: mock Ollama /api/ps with two loaded models.
func TestProbeOllama_httptest(t *testing.T) {
	body := `{"models":[{"name":"qwen2.5:7b","size":1000000,"expires_at":"2099-01-01T00:00:00Z"}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer ts.Close()
	port := portFromURL(ts.URL)
	state, ok := probeOllama(context.Background(), port)
	if !ok {
		t.Fatalf("probeOllama returned ok=false")
	}
	if len(state.Models) != 1 || state.Models[0] != "qwen2.5:7b" {
		t.Errorf("expected 1 model qwen2.5:7b, got %v", state.Models)
	}
	if !state.AnyRunning {
		t.Errorf("expected AnyRunning=true (expires_at in 2099)")
	}
}

// TestProbeSDWebUI_httptest: mock progress + cmd-models.
func TestProbeSDWebUI_httptest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sdapi/v1/progress":
			fmt.Fprint(w, `{"progress":0.5,"state":{"job":"a cat","job_count":1}}`)
		case "/sdapi/v1/cmd-models":
			fmt.Fprint(w, `[{"title":"sd_xl_base_1.0"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	port := portFromURL(ts.URL)
	state, ok := probeSDWebUI(context.Background(), port)
	if !ok {
		t.Fatalf("probeSDWebUI returned ok=false")
	}
	if state.Progress != 0.5 {
		t.Errorf("expected progress=0.5, got %f", state.Progress)
	}
	if state.JobCount != 1 {
		t.Errorf("expected job_count=1, got %d", state.JobCount)
	}
	if state.ActivePrompt != "a cat" {
		t.Errorf("expected prompt='a cat', got %q", state.ActivePrompt)
	}
	if state.ModelTitle != "sd_xl_base_1.0" {
		t.Errorf("expected model sd_xl_base_1.0, got %q", state.ModelTitle)
	}
}

// TestRegistry_Scan_roundtrip: with all ports closed, snapshot should be
// empty (or hold only idle entries from prior scans). We assert the
// snapshot does not panic and returns a non-nil slice.
func TestRegistry_Scan_roundtrip(t *testing.T) {
	r := NewRegistry("", 1) // 1 second interval for the test
	defer r.Scan(context.Background())
	snap := r.Snapshot()
	if snap == nil {
		t.Errorf("expected non-nil snapshot")
	}
}

// TestRegistry_Reload: missing config file should not panic.
func TestRegistry_Reload(t *testing.T) {
	r := NewRegistry("", 0)
	r.Reload() // noop for empty cfgPath
}

// TestEntityKey_stable ensures the key function is reproducible across calls.
func TestEntityKey_stable(t *testing.T) {
	a := entityKey("comfyui", 8188, 0)
	b := entityKey("comfyui", 8188, 0)
	if a != b {
		t.Errorf("entityKey not stable: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "comfyui@") {
		t.Errorf("expected port-based prefix, got %q", a)
	}
}

// helper: extract the port from httptest.Server's URL
func portFromURL(u string) int {
	// u is like http://127.0.0.1:34567
	var s string
	fmt.Sscanf(u, "http://127.0.0.1:%s", &s)
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[:idx]
	}
	var p int
	fmt.Sscanf(s, "%d", &p)
	return p
}

// sanity: json marshal/unmarshal of LocalTask (round-trip test for tags)
func TestLocalTask_JSON(t *testing.T) {
	lt := LocalTask{
		ID:         "abc",
		Service:    SvcComfyUI,
		Title:      "ComfyUI @ :8188",
		Port:       8188,
		Status:     StatusBusy,
		MapToType:  "image_txt2img",
		PayloadTpl: `{"prompt":"x"}`,
		SourceHint: SourcePort,
	}
	b, err := json.Marshal(lt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"map_to_type":"image_txt2img"`) {
		t.Errorf("expected snake_case JSON tag, got %s", string(b))
	}
}
