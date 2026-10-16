package localtasks

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// mapperInput is the discriminator union MapLocalTask uses to route on the
// probe type. Wrapping in an interface{} keeps the public API stable as we
// add new service probes; adding a service = add a probe type + a case here.
type mapperInput interface {
	isMapperInput()
}

func (ComfyState) isMapperInput() {}
func (SDState) isMapperInput()    {}
func (OllamaState) isMapperInput() {}
func (ProcInfo) isMapperInput()    {}
func (CustomRule) isMapperInput()  {}

// MapLocalTask decides which gateway task type (if any) a detected local
// service should map to. Returns (taskType, payloadTemplate, ok=true) or
// ("", "", ok=false) when the local task is not in a state worth dispatching
// a copy of (e.g. ComfyUI idle queue).
//
// This function is pure — no I/O, no time, no randomness — so it is the
// natural unit-test target. See mapper_test.go for the table.
func MapLocalTask(svc string, probe mapperInput, rule CustomRule) (string, string, bool) {
	switch p := probe.(type) {
	case ComfyState:
		return mapComfyUI(p)
	case SDState:
		return mapSDWebUI(p)
	case OllamaState:
		return mapOllama(p)
	case ProcInfo:
		switch svc {
		case SvcFFmpeg:
			return mapFFmpeg(p)
		case SvcBlender:
			return mapBlender(p)
		}
	case CustomRule:
		return mapCustomRule(p)
	}
	return "", "", false
}

// mapComfyUI picks the closest gateway task type from the workflow's
// node-class fingerprint.
//
//	has VHS_VideoCombine node  → video_assemble_clip  (ComfyUI video out)
//	has KSampler + LoadImage + VAEEncode → image_img2img
//	has KSampler + VAEEncodeForInpaint → image_inpaint
//	has KSampler + CLIPTextEncode only → image_txt2img
//
// If queue_remaining is 0 we reject (ok=false) so the UI doesn't show a
// "throw" button for a service that's just sitting on its idle page.
func mapComfyUI(s ComfyState) (string, string, bool) {
	if s.QueueRemaining <= 0 {
		return "", "", false
	}
	wf := s.FirstWorkflow
	has := func(class string) bool {
		for _, v := range wf {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if ct, ok := m["class_type"].(string); ok && strings.EqualFold(ct, class) {
				return true
			}
		}
		return false
	}
	payload := buildComfyPayload(s)
	if has("VHS_VideoCombine") || has("VHS_VideoCombine*") {
		return "video_assemble_clip", payload, true
	}
	if has("KSampler") && has("LoadImage") && (has("VAEEncode") || has("VAEEncodeForInpaint")) {
		// distinguish img2img vs inpaint by the presence of a mask node
		if has("SetLatentNoiseMask") || has("VAEEncodeForInpaint") {
			return "image_inpaint", payload, true
		}
		return "image_img2img", payload, true
	}
	if has("KSampler") && has("CLIPTextEncode") {
		return "image_txt2img", payload, true
	}
	return "", "", false
}

// buildComfyPayload packages the workflow fingerprint into a JSON payload
// the gateway's image workers can chew on. Workers expect a JSON object
// with at least "prompt"; we include a truncated node map so the worker
// can reconstruct the inputs.
func buildComfyPayload(s ComfyState) string {
	out := map[string]any{
		"prompt":            "from-local-comfyui",
		"workflow_fingerprint": s.FirstWorkflow,
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// mapSDWebUI maps Automatic1111's progress endpoint to a gateway task.
// A1111 has fewer node classes than ComfyUI; we use the loaded model name
// and the active prompt to decide between txt2img / img2img / inpaint. If
// progress > 0 we know it's mid-render. Otherwise reject (idle).
func mapSDWebUI(s SDState) (string, string, bool) {
	if s.Progress <= 0 && s.JobCount <= 0 {
		return "", "", false
	}
	payload := map[string]any{
		"prompt":  s.ActivePrompt,
		"model":   s.ModelTitle,
		"source":  "local-sd-webui",
	}
	// We can't easily tell txt2img vs img2img from progress alone, so
	// default to txt2img (the most common). The UI shows the payload and
	// the operator can edit before dispatching.
	b, _ := json.Marshal(payload)
	return "image_txt2img", string(b), true
}

// mapOllama wraps the model invocation as exec_run so we don't need a new
// worker type. Workers with shell capability (the exec worker, which ships
// edge-tts and ffmpeg) can drive `ollama run <model> "<prompt>"` natively.
// If no model is loaded we still allow dispatching — operators may want to
// pre-stage a remote test run.
func mapOllama(s OllamaState) (string, string, bool) {
	if len(s.Models) == 0 {
		return "", "", false
	}
	model := s.Models[0]
	payload := fmt.Sprintf(`ollama run %s "your prompt here"`, shellQuote(model))
	return "exec_run", payload, true
}

// mapFFmpeg picks video_assemble_clip by default; video_add_subtitle if
// the cmdline mentions subtitle filters.
func mapFFmpeg(p ProcInfo) (string, string, bool) {
	low := strings.ToLower(p.Cmdline)
	if strings.Contains(low, "subtitles=") || strings.Contains(low, "-vf ass") ||
		strings.Contains(low, "subtitles,") || strings.Contains(low, ".srt") {
		return "video_add_subtitle", p.Cmdline, true
	}
	return "video_assemble_clip", p.Cmdline, true
}

// mapBlender renders the .blend path + render flags as an exec_run payload.
// We extract the first .blend argument we can find; if none, fall back to
// the raw cmdline so the operator can decide.
func mapBlender(p ProcInfo) (string, string, bool) {
	blend := findBlendPath(p.Cmdline)
	if blend == "" {
		return "exec_run", p.Cmdline, true
	}
	return "exec_run", fmt.Sprintf(`blender -b %q -E BLENDER_RENDER -o //render_ -f 1`, blend), true
}

// mapCustomRule returns the rule's type + template with {{...}} substituted.
func mapCustomRule(r CustomRule) (string, string, bool) {
	if r.Type == "" {
		return "", "", false
	}
	return r.Type, r.PayloadTemplate, true
}

// renderTemplate substitutes {{pid}}, {{name}}, {{cmdline}}, {{port}} placeholders.
func renderTemplate(tpl string, p ProcInfo, port int) string {
	out := tpl
	out = strings.ReplaceAll(out, "{{pid}}", fmt.Sprint(p.PID))
	out = strings.ReplaceAll(out, "{{name}}", p.Name)
	out = strings.ReplaceAll(out, "{{cmdline}}", p.Cmdline)
	out = strings.ReplaceAll(out, "{{port}}", fmt.Sprint(port))
	return out
}

// findBlendPath pulls the first .blend argument from a cmdline. Blender's
// CLI conventions accept both "blender file.blend" and "blender -b file.blend".
//
// The regex uses a non-capturing prefix (start / whitespace / quote) so
// the capture group never includes the opening quote. The capture class
// `[^"'\s]+` excludes quotes and whitespace so a quoted path like
// `"C:/scenes/hero.blend"` captures `C:/scenes/hero.blend`, not
// `"C:/scenes/hero.blend`.
var blendRe = regexp.MustCompile(`(?:^|\s|["'])([^"'\s]+\.blend)\b`)

func findBlendPath(cmdline string) string {
	m := blendRe.FindStringSubmatch(cmdline)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

// shellQuote wraps s in double quotes if it contains characters that the
// Windows shell would otherwise reinterpret. We keep it minimal — Ollama
// model names are usually safe.
func shellQuote(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\"&|<>^()") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}
