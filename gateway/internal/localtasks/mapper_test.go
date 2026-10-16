package localtasks

import (
	"strings"
	"testing"
)

func TestMapComfyUI_txt2img(t *testing.T) {
	state := ComfyState{
		QueueRemaining: 1,
		FirstWorkflow: map[string]any{
			"3": map[string]any{"class_type": "KSampler"},
			"6": map[string]any{"class_type": "CLIPTextEncode"},
		},
	}
	typ, payload, ok := MapLocalTask(SvcComfyUI, state, CustomRule{})
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if typ != "image_txt2img" {
		t.Fatalf("expected image_txt2img, got %s", typ)
	}
	if !strings.Contains(payload, "from-local-comfyui") {
		t.Fatalf("expected payload to contain from-local-comfyui, got %s", payload)
	}
}

func TestMapComfyUI_img2img(t *testing.T) {
	state := ComfyState{
		QueueRemaining: 2,
		FirstWorkflow: map[string]any{
			"10": map[string]any{"class_type": "KSampler"},
			"20": map[string]any{"class_type": "LoadImage"},
			"30": map[string]any{"class_type": "VAEEncode"},
			"40": map[string]any{"class_type": "CLIPTextEncode"},
		},
	}
	typ, _, ok := MapLocalTask(SvcComfyUI, state, CustomRule{})
	if !ok || typ != "image_img2img" {
		t.Fatalf("expected image_img2img, got %s ok=%v", typ, ok)
	}
}

func TestMapComfyUI_inpaint(t *testing.T) {
	state := ComfyState{
		QueueRemaining: 1,
		FirstWorkflow: map[string]any{
			"1": map[string]any{"class_type": "KSampler"},
			"2": map[string]any{"class_type": "LoadImage"},
			"3": map[string]any{"class_type": "VAEEncodeForInpaint"},
			"4": map[string]any{"class_type": "SetLatentNoiseMask"},
		},
	}
	typ, _, ok := MapLocalTask(SvcComfyUI, state, CustomRule{})
	if !ok || typ != "image_inpaint" {
		t.Fatalf("expected image_inpaint, got %s ok=%v", typ, ok)
	}
}

func TestMapComfyUI_video(t *testing.T) {
	state := ComfyState{
		QueueRemaining: 1,
		FirstWorkflow: map[string]any{
			"1": map[string]any{"class_type": "KSampler"},
			"2": map[string]any{"class_type": "VHS_VideoCombine"},
		},
	}
	typ, _, ok := MapLocalTask(SvcComfyUI, state, CustomRule{})
	if !ok || typ != "video_assemble_clip" {
		t.Fatalf("expected video_assemble_clip, got %s ok=%v", typ, ok)
	}
}

func TestMapComfyUI_idleRejected(t *testing.T) {
	state := ComfyState{QueueRemaining: 0, FirstWorkflow: map[string]any{
		"3": map[string]any{"class_type": "KSampler"},
	}}
	typ, _, ok := MapLocalTask(SvcComfyUI, state, CustomRule{})
	if ok {
		t.Fatalf("expected ok=false for idle queue, got ok=true typ=%s", typ)
	}
}

func TestMapOllama_usesExecRun(t *testing.T) {
	state := OllamaState{Models: []string{"qwen2.5:7b"}, AnyRunning: true}
	typ, payload, ok := MapLocalTask(SvcOllama, state, CustomRule{})
	if !ok || typ != "exec_run" {
		t.Fatalf("expected exec_run, got %s ok=%v", typ, ok)
	}
	if !strings.Contains(payload, "ollama run") || !strings.Contains(payload, "qwen2.5:7b") {
		t.Fatalf("expected payload to wrap ollama run qwen2.5:7b, got %s", payload)
	}
}

func TestMapOllama_noModelsRejected(t *testing.T) {
	_, _, ok := MapLocalTask(SvcOllama, OllamaState{Models: nil}, CustomRule{})
	if ok {
		t.Fatalf("expected ok=false for empty model list")
	}
}

func TestMapFFmpeg_subtitles(t *testing.T) {
	p := ProcInfo{Cmdline: `ffmpeg -i a.mp4 -vf "subtitles=subs.srt" -c:v libx264 out.mp4`}
	typ, payload, ok := MapLocalTask(SvcFFmpeg, p, CustomRule{})
	if !ok || typ != "video_add_subtitle" {
		t.Fatalf("expected video_add_subtitle, got %s ok=%v", typ, ok)
	}
	if payload != p.Cmdline {
		t.Fatalf("expected payload to equal cmdline verbatim")
	}
}

func TestMapFFmpeg_assembles(t *testing.T) {
	p := ProcInfo{Cmdline: `ffmpeg -i a.mp4 -i b.mp4 -filter_complex "concat" out.mp4`}
	typ, _, ok := MapLocalTask(SvcFFmpeg, p, CustomRule{})
	if !ok || typ != "video_assemble_clip" {
		t.Fatalf("expected video_assemble_clip, got %s ok=%v", typ, ok)
	}
}

func TestMapBlender(t *testing.T) {
	p := ProcInfo{Cmdline: `blender -b "C:/scenes/hero.blend" -P render.py`}
	typ, payload, ok := MapLocalTask(SvcBlender, p, CustomRule{})
	if !ok || typ != "exec_run" {
		t.Fatalf("expected exec_run, got %s ok=%v", typ, ok)
	}
	if !strings.Contains(payload, "C:/scenes/hero.blend") {
		t.Fatalf("expected payload to embed .blend path, got %s", payload)
	}
}

func TestMapSDWebUI_busy(t *testing.T) {
	state := SDState{Progress: 0.42, JobCount: 1, ActivePrompt: "a cat", ModelTitle: "sd_xl_base_1.0"}
	typ, payload, ok := MapLocalTask(SvcSDWebUI, state, CustomRule{})
	if !ok || typ != "image_txt2img" {
		t.Fatalf("expected image_txt2img, got %s ok=%v", typ, ok)
	}
	if !strings.Contains(payload, "a cat") {
		t.Fatalf("expected payload to embed active prompt, got %s", payload)
	}
}

func TestMapSDWebUI_idleRejected(t *testing.T) {
	_, _, ok := MapLocalTask(SvcSDWebUI, SDState{}, CustomRule{})
	if ok {
		t.Fatalf("expected ok=false for idle SD")
	}
}

func TestRenderTemplate(t *testing.T) {
	p := ProcInfo{PID: 1234, Name: "lister.exe", Cmdline: "lister.exe --queue"}
	out := renderTemplate("pid={{pid}} name={{name}} cmd={{cmdline}}", p, 0)
	want := "pid=1234 name=lister.exe cmd=lister.exe --queue"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestFindBlendPath(t *testing.T) {
	cases := []struct {
		cmdline string
		want    string
	}{
		{`blender -b "C:/scenes/hero.blend" -P render.py`, "C:/scenes/hero.blend"},
		{`blender -b /home/user/scene.blend -f 1`, "/home/user/scene.blend"},
		{`blender -b scene.blend`, "scene.blend"},
		{`blender -E BLENDER_RENDER`, ""},
	}
	for _, tc := range cases {
		got := findBlendPath(tc.cmdline)
		if got != tc.want {
			t.Errorf("findBlendPath(%q) = %q, want %q", tc.cmdline, got, tc.want)
		}
	}
}

func TestEntityKey(t *testing.T) {
	if entityKey("comfyui", 8188, 0) != "comfyui@8188" {
		t.Errorf("port-based key wrong")
	}
	if entityKey("ffmpeg", 0, 4242) != "ffmpeg#4242" {
		t.Errorf("pid-based key wrong")
	}
}
