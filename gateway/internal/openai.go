package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ChatRequest mirrors the OpenAI chat completions request format.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatResponse mirrors OpenAI's chat completions response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// classifyTask maps a free-form user prompt to a concrete worker task type.
// Returns taskType and the payload to forward to the worker.
func classifyTask(content string) (string, string, error) {
	low := strings.ToLower(content)
	// SECURITY: never auto-dispatch exec / shell tasks via natural language.
	// exec_run executes arbitrary commands on worker hosts and must only be
	// dispatched by a trusted admin client (deskgrid-submit --mode remote).
	for _, kw := range []string{"exec", "shell", "shell_exec", "cmd.exe", "/bin/bash", "powershell", "run command"} {
		if strings.Contains(low, kw) {
			return "", "", fmt.Errorf("shell/exec tasks cannot be triggered via the OpenAI bridge; use deskgrid-submit directly")
		}
	}
	// explicit code/format
	if strings.Contains(low, "格式化") || strings.Contains(low, "format") ||
		strings.Contains(low, "代码重构") || strings.Contains(low, "refactor") ||
		strings.Contains(low, "ast") || strings.Contains(low, "代码") {
		payload := extractPayload(content, []string{"格式化", "代码", "重构", "refactor", "ast"})
		if payload == "" {
			payload = content
		}
		if strings.Contains(low, "ast") && !strings.Contains(low, "格式化") {
			return "code_ast", payload, nil
		}
		return "code_format", payload, nil
	}
	// doc summary / wordfreq
	if strings.Contains(low, "摘要") || strings.Contains(low, "summary") ||
		strings.Contains(low, "总结") || strings.Contains(low, "summarize") ||
		strings.Contains(low, "词频") || strings.Contains(low, "wordfreq") ||
		strings.Contains(low, "文档") || strings.Contains(low, "doc") {
		payload := extractPayload(content, []string{"摘要", "总结", "词频", "文档", "doc"})
		if payload == "" {
			payload = content
		}
		if strings.Contains(low, "词频") || strings.Contains(low, "wordfreq") {
			return "doc_wordfreq", payload, nil
		}
		return "doc_summary", payload, nil
	}
	// csv monte carlo / stats
	if strings.Contains(low, "蒙特卡洛") || strings.Contains(low, "monte carlo") ||
		strings.Contains(low, "montecarlo") || strings.Contains(low, "模拟") {
		// Try to extract structured config from natural language
		if cfg := parseMonteCarloConfig(content); cfg != "" {
			return "csv_montecarlo", cfg, nil
		}
		return "csv_montecarlo", content, nil
	}
	if strings.Contains(low, "csv") || strings.Contains(low, "统计") ||
		strings.Contains(low, "stats") || strings.Contains(low, "describe") {
		return "csv_stats", content, nil
	}
	// image: image_txt2img / image_img2img / image_inpaint / image_controlnet
	if strings.Contains(low, "出图") || strings.Contains(low, "画") ||
		strings.Contains(low, "img2img") || strings.Contains(low, "inpaint") ||
		strings.Contains(low, "controlnet") ||
		strings.Contains(low, "txt2img") || strings.Contains(low, "生图") {
		taskType := "image_txt2img"
		if strings.Contains(low, "img2img") {
			taskType = "image_img2img"
		} else if strings.Contains(low, "inpaint") || strings.Contains(low, "重绘") {
			taskType = "image_inpaint"
		} else if strings.Contains(low, "controlnet") {
			taskType = "image_controlnet"
		}
		return taskType, content, nil
	}
	// audio: audio_tts_line / audio_tts_with_emotion
	if strings.Contains(low, "朗读") || strings.Contains(low, "配音") ||
		strings.Contains(low, "tts") || strings.Contains(low, "语音") ||
		strings.Contains(low, "念") || strings.Contains(low, "情感") {
		taskType := "audio_tts_line"
		if strings.Contains(low, "情感") || strings.Contains(low, "emotion") {
			taskType = "audio_tts_with_emotion"
		}
		return taskType, content, nil
	}
	// video: video_assemble_clip / video_add_subtitle / video_kenburns
	if strings.Contains(low, "动画") || strings.Contains(low, "短片") ||
		strings.Contains(low, "kenburns") || strings.Contains(low, "字幕") ||
		strings.Contains(low, "assemble") || strings.Contains(low, "剪辑") {
		taskType := "video_assemble_clip"
		if strings.Contains(low, "字幕") || strings.Contains(low, "subtitle") {
			taskType = "video_add_subtitle"
		} else if strings.Contains(low, "kenburns") {
			taskType = "video_kenburns"
		}
		return taskType, content, nil
	}
	return "", "", fmt.Errorf("unable to classify task from prompt; please include keywords like '格式化代码', '摘要', '蒙特卡洛', '出图', '朗读', '动画'")
}

// parseMonteCarloConfig tries to extract Monte Carlo parameters from natural language.
// Returns "" if it can't find enough structure.
func parseMonteCarloConfig(content string) string {
	cfg := map[string]interface{}{"samples": 10000, "distribution": "uniform", "low": 0, "high": 1}
	low := strings.ToLower(content)
	if strings.Contains(low, "normal") || strings.Contains(content, "正态") {
		cfg["distribution"] = "normal"
	} else if strings.Contains(content, "指数") || strings.Contains(low, "exponential") {
		cfg["distribution"] = "exponential"
	} else if strings.Contains(content, "均匀") {
		cfg["distribution"] = "uniform"
	}

	// Extract numbers and assign by position
	nums := []float64{}
	for _, tok := range strings.FieldsFunc(content, func(r rune) bool {
		return !(r >= '0' && r <= '9') && r != '.' && r != '-'
	}) {
		if v, err := strconv.ParseFloat(tok, 64); err == nil {
			nums = append(nums, v)
		}
	}

	if len(nums) == 0 {
		return ""
	}
	// First number is typically sample count
	if nums[0] >= 100 {
		cfg["samples"] = int(nums[0])
		remaining := nums[1:]
		if cfg["distribution"] == "normal" && len(remaining) >= 2 {
			cfg["mean"] = remaining[0]
			cfg["stddev"] = remaining[1]
		} else if cfg["distribution"] == "exponential" && len(remaining) >= 1 {
			cfg["rate"] = remaining[0]
		} else if cfg["distribution"] == "uniform" && len(remaining) >= 2 {
			cfg["low"] = remaining[0]
			cfg["high"] = remaining[1]
		}
	} else if cfg["distribution"] == "normal" && len(nums) >= 2 {
		cfg["mean"] = nums[0]
		cfg["stddev"] = nums[1]
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

func extractPayload(content string, markers []string) string {
	// try colon / 冒号 split
	for _, sep := range []string{":", "："} {
		if idx := strings.Index(content, sep); idx >= 0 && idx+len(sep) < len(content) {
			after := strings.TrimSpace(content[idx+len(sep):])
			if len(after) > 5 {
				return after
			}
		}
	}
	// try removing leading keyword
	for _, m := range markers {
		if idx := strings.Index(content, m); idx >= 0 {
			after := strings.TrimSpace(content[idx+len(m):])
			if len(after) > 5 {
				return after
			}
		}
	}
	return ""
}

func (s *Server) handleChatCompletions(c *gin.Context) {
	var req ChatRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "invalid_request_error",
		}})
		return
	}
	if len(req.Messages) == 0 {
		c.JSON(400, gin.H{"error": "messages required"})
		return
	}
	// gather latest user content (or last message)
	var content string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			content = req.Messages[i].Content
			break
		}
	}
	if content == "" {
		content = req.Messages[len(req.Messages)-1].Content
	}

	taskType, payload, err := classifyTask(content)
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{"message": err.Error(), "type": "task_classification_error"}})
		return
	}

	// dispatch and wait for completion (synchronous — TRAE expects a result in the response)
	worker := s.store.PickWorker(taskType)
	if worker == nil {
		c.JSON(503, gin.H{"error": gin.H{
			"message": fmt.Sprintf("no worker available for task type '%s'", taskType),
			"type":    "no_worker",
		}})
		return
	}
	t := &Task{
		ID:        uuid.NewString(),
		Type:      taskType,
		Payload:   payload,
		Source:    "openai",
		Status:    "pending",
		Progress:  0,
		CreatedAt: time.Now(),
		Logs:      []string{fmt.Sprintf("[openai] classified as %s", taskType)},
	}
	s.store.AddTask(t)
	s.store.AssignTask(t.ID, worker.ID)
	s.pushToWorker(worker, t)

	// poll for completion up to 60s
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		cur := s.store.GetTask(t.ID)
		if cur == nil {
			break
		}
		if cur.Status == "done" || cur.Status == "failed" {
			returnChatResponse(c, req, cur)
			return
		}
	}
	// timeout
	cur := s.store.GetTask(t.ID)
	if cur == nil {
		cur = t
	}
	cur.Status = "failed"
	cur.Error = "timeout waiting for worker"
	returnChatResponse(c, req, cur)
}

func returnChatResponse(c *gin.Context, req ChatRequest, t *Task) {
	var assistantContent string
	if t.Status == "done" {
		// Pretty-print the result if it is JSON
		pretty := t.Result
		var any interface{}
		if err := json.Unmarshal([]byte(t.Result), &any); err == nil {
			if b, err := json.MarshalIndent(any, "", "  "); err == nil {
				pretty = string(b)
			}
		}
		assistantContent = fmt.Sprintf(
			"任务已完成 ✅\n\n任务ID: %s\n任务类型: %s\nWorker: %s\n耗时: %s\n\n----- 结果 -----\n%s\n\n----- 最近日志 -----\n%s",
			t.ID, t.Type, t.WorkerID, t.FinishedAt.Sub(t.StartedAt), pretty, strings.Join(lastN(t.Logs, 5), "\n"),
		)
	} else {
		assistantContent = fmt.Sprintf("任务失败 ❌\n\n任务ID: %s\n错误: %s\n日志: %s",
			t.ID, t.Error, strings.Join(lastN(t.Logs, 5), "\n"))
	}
	resp := ChatResponse{
		ID:      "chatcmpl-" + t.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: "assistant", Content: assistantContent},
			FinishReason: "stop",
		}},
		Usage: Usage{PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0},
	}
	c.JSON(http.StatusOK, resp)
}

func lastN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// keep context import alive for future streaming impl
var _ = context.Background
