package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"deskgrid-cloud-gateway/internal/localtasks"
)

// Server wires HTTP routes to the store + hub.
type Server struct {
	store        *Store
	hub          *Hub
	engine       *gin.Engine
	authRequired bool
	hmacSecret   []byte
	peers        *PeerSync
	ltReg        *localtasks.Registry // nil until StartLocalTaskScanner is called
}

func NewServer(store *Store, hub *Hub, token string) *Server {
	gin.SetMode(gin.ReleaseMode)
	secret := []byte(os.Getenv("GATEWAY_HMAC_SECRET"))
	s := &Server{
		store:        store,
		hub:          hub,
		authRequired: AuthRequired(token),
		hmacSecret:   secret,
		peers:        NewPeerSync(os.Getenv("GATEWAY_PEERS")),
	}
	s.engine = gin.New()
	s.engine.Use(gin.Recovery())
	s.engine.Use(corsMiddleware())
	s.engine.Use(AuthMiddleware(token))
	s.routes()
	return s
}

// hmacSecret returns the secret so pushToWorker / pushCancel can sign.
func (s *Server) HMAC() []byte { return s.hmacSecret }

func (s *Server) Engine() *gin.Engine { return s.engine }

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

// Per-route request body size limits. Anything larger is rejected before
// being parsed, preventing OOM from a single huge payload. Values are tuned
// to the realistic maximum for each endpoint:
//
//   register/heartbeat/progress: 16 KB (json of CPU/Mem stats)
//   result: 16 MB (image/audio/video task results may embed small files)
//   cancel: 1 KB (just a task id)
//   task create: 64 KB (includes payload string)
//   v1/chat/completions: 64 KB (whole conversation)
//
// On overflow, http.MaxBytesReader returns "request body too large" which the
// downstream handler surfaces as a 400 response. The protection is enforced
// regardless of status code.
func bodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

func (s *Server) routes() {
	api := s.engine.Group("/api")
	{
		api.GET("/auth-check", s.authCheck)
		api.GET("/workers", s.listWorkers)
		api.GET("/workers/:id", s.getWorker)
		api.GET("/stats", s.getStats)
		// Worker-originated endpoints: require HMAC signature (in addition to Bearer)
		hmac := HMACMiddleware(s.hmacSecret)
		api.POST("/workers/register", hmac, bodyLimit(16*1024), s.registerWorker)
		api.DELETE("/workers/:id", hmac, bodyLimit(1024), s.deleteWorker)
		api.POST("/workers/:id/heartbeat", hmac, bodyLimit(16*1024), s.heartbeat)
		api.POST("/workers/:id/progress", hmac, bodyLimit(16*1024), s.workerProgress)
		api.POST("/workers/:id/result", hmac, bodyLimit(16*1024*1024), s.workerResult)
		api.POST("/workers/:id/task/cancel", hmac, bodyLimit(1024), s.cancelTask)

		api.GET("/tasks", s.listTasks)
		api.GET("/tasks/:id", s.getTask)
		api.DELETE("/tasks/:id", s.deleteTask)
		api.POST("/tasks", bodyLimit(64*1024), s.createTask)
		api.POST("/tasks/:id/pause", bodyLimit(1024), s.pauseTask)
		api.POST("/tasks/:id/resume", bodyLimit(1024), s.resumeTask)
		api.POST("/tasks/:id/stop", bodyLimit(1024), s.stopTask)
		api.POST("/tasks/:id/start", bodyLimit(1024), s.startTask)

		// Local-task offload: scan the gateway host for heavy services
		// (ComfyUI / SD WebUI / Ollama / ffmpeg / blender / custom) and
		// dispatch a copy of one to a remote worker. See internal/localtasks.
		api.GET("/local-tasks", s.listLocalTasks)
		api.POST("/local-tasks/scan", bodyLimit(4*1024), s.scanLocalTasksNow)
		api.POST("/local-tasks/:id/dispatch", bodyLimit(16*1024), s.dispatchLocalTask)
		api.POST("/local-tasks/reload", bodyLimit(4*1024), s.reloadLocalTasksConfig)

		// peer-to-peer replication endpoint (gateway-to-gateway only,
		// not auth-protected because it sits on internal network)
		api.POST("/peer-replicate", s.peerReplicate)
	}

	s.engine.GET("/ws", func(c *gin.Context) { s.hub.HandleWS(c.Writer, c.Request) })

	// OpenAI-compatible endpoint
	s.engine.POST("/v1/chat/completions", bodyLimit(64*1024), s.handleChatCompletions)
	s.engine.GET("/v1/models", s.listModels)

	s.engine.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true, "workers": len(s.store.ListWorkers())})
	})
}

// ---------- Worker endpoints ----------

type registerReq struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Name         string      `json:"name"`
	CPU          string      `json:"cpu"`
	Cores        int         `json:"cores"`
	MemoryMB     int         `json:"memory_mb"`
	Port         int         `json:"port"`
	Capabilities []string    `json:"capabilities"` // self-reported tools/models; empty = no caps
	GPUs         []GPUDetail `json:"gpus"`         // per-card detail; absent on CPU-only hosts
	GPUCount     int         `json:"gpu_count"`    // convenience: len(GPUs); 0 on CPU hosts
}

func (s *Server) registerWorker(c *gin.Context) {
	var req registerReq
	if err := bindJSON(c, &req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.ID == "" {
		req.ID = uuid.NewString()
	}
	if req.Type == "" || (req.Type != "code" && req.Type != "doc" && req.Type != "csv" &&
		req.Type != "image" && req.Type != "audio" && req.Type != "video" &&
		req.Type != "exec") {
		c.JSON(400, gin.H{"error": "type must be code | doc | csv | image | audio | video | exec"})
		return
	}
	host := c.ClientIP()
	// If client reports a private IP, prefer it; gateway only sees the IP it received from.
	if h := c.GetHeader("X-Worker-Host"); h != "" {
		host = h
	}
	// Take the primary GPU's scalar memory (the one the scheduler's
	// MinGPUFreeMB check looks at) from the first GPU record. If the
	// worker reports no GPUs (CPU-only host), both scalars stay 0.
	var primaryUsed, primaryTotal int64
	if len(req.GPUs) > 0 {
		primaryUsed = req.GPUs[0].MemUsedMB
		primaryTotal = req.GPUs[0].MemTotalMB
	}
	w := &Worker{
		ID:            req.ID,
		Type:          req.Type,
		Name:          req.Name,
		CPU:           req.CPU,
		Cores:         req.Cores,
		MemoryMB:      req.MemoryMB,
		Host:          host,
		Port:          req.Port,
		Capabilities:  req.Capabilities,
		GPUs:          req.GPUs,
		GPUCount:      len(req.GPUs),
		GPUMemUsedMB:  primaryUsed,
		GPUMemTotalMB: primaryTotal,
	}
	s.store.AddWorker(w)
	Info("worker registered: id=%s type=%s name=%s cpu=%s cores=%d mem=%dMB caps=%v gpus=%d",
		w.ID, w.Type, w.Name, w.CPU, w.Cores, w.MemoryMB, w.Capabilities, w.GPUCount)
	c.JSON(200, gin.H{"ok": true, "id": w.ID})
}

type heartbeatReq struct {
	CPUUsage      float64    `json:"cpu_usage"`
	MemUsage      float64    `json:"mem_usage"`
	QueueLen      int        `json:"queue_len"`
	GPUMemUsedMB  int64      `json:"gpu_mem_used_mb"`
	GPUMemTotalMB int64      `json:"gpu_mem_total_mb"`
	GPUs          []GPUDetail `json:"gpus"`
	GPUCount      int        `json:"gpu_count"`
}

func (s *Server) heartbeat(c *gin.Context) {
	id := c.Param("id")
	var req heartbeatReq
	if err := bindJSON(c, &req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	w := s.store.UpdateHeartbeat(id, req.CPUUsage, req.MemUsage, req.QueueLen,
		req.GPUMemUsedMB, req.GPUMemTotalMB, req.GPUs)
	if w == nil {
		c.JSON(404, gin.H{"error": "worker not found"})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

// cancelTask asks the worker to abort a running task. Workers check their
// per-task cancel channel between progress updates.
type cancelTaskReq struct {
	TaskID string `json:"task_id"`
}

func (s *Server) cancelTask(c *gin.Context) {
	id := c.Param("id")
	var req cancelTaskReq
	if err := bindJSON(c, &req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if !s.store.CancelTask(req.TaskID) {
		c.JSON(404, gin.H{"error": "task not running or not found"})
		return
	}
	// forward to worker so it can abort the thread
	go s.pushCancelToWorker(id, req.TaskID)
	c.JSON(200, gin.H{"ok": true})
}

// pauseTask flips a running task to "paused" on the store, then signals the
// owning worker to set its pause_event so the subprocess blocks between
// progress updates.
func (s *Server) pauseTask(c *gin.Context) {
	id := c.Param("id")
	if !s.store.PauseTask(id) {
		c.JSON(409, gin.H{"error": "task not running or not found"})
		return
	}
	t := s.store.GetTask(id)
	if t != nil && t.WorkerID != "" {
		go s.pushControlToWorker(t.WorkerID, id, "pause")
	}
	c.JSON(200, gin.H{"ok": true})
}

// resumeTask flips a paused task back to running and signals the worker to
// clear pause_event. StartedAt is shifted forward by the pause duration inside
// ResumeTask so the timeout clock doesn't tick during pause.
func (s *Server) resumeTask(c *gin.Context) {
	id := c.Param("id")
	if !s.store.ResumeTask(id) {
		c.JSON(409, gin.H{"error": "task not paused or not found"})
		return
	}
	t := s.store.GetTask(id)
	if t != nil && t.WorkerID != "" {
		go s.pushControlToWorker(t.WorkerID, id, "resume")
	}
	c.JSON(200, gin.H{"ok": true})
}

// stopTask hard-stops a task. Marks the task failed immediately so the dashboard
// stops showing it as alive, then asks the worker to terminate its subprocess
// (which may already be exiting in response to cancel_event being set).
func (s *Server) stopTask(c *gin.Context) {
	id := c.Param("id")
	t := s.store.GetTask(id)
	if t == nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}
	if !s.store.StopTask(id) {
		c.JSON(409, gin.H{"error": "task already finished"})
		return
	}
	if t.WorkerID != "" {
		go s.pushControlToWorker(t.WorkerID, id, "stop")
	}
	c.JSON(200, gin.H{"ok": true})
}

// startTask resumes a paused task in place, OR clones a finished task and
// dispatches a brand-new task with the same payload + requirements.
func (s *Server) startTask(c *gin.Context) {
	id := c.Param("id")
	original := s.store.GetTask(id)
	if original == nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}
	switch original.Status {
	case "paused":
		if !s.store.ResumeTask(id) {
			c.JSON(409, gin.H{"error": "task not paused"})
			return
		}
		if original.WorkerID != "" {
			go s.pushControlToWorker(original.WorkerID, id, "resume")
		}
		c.JSON(200, gin.H{"ok": true, "task": original})
	case "running":
		// no-op, but tell the user it's already running
		c.JSON(200, gin.H{"ok": true, "task": original, "noop": "already running"})
	case "pending":
		// re-dispatch in case scheduler hasn't picked it up yet
		newTask, err := s.dispatchTaskWithParams(original.Type, original.Payload,
			original.Source, original.Priority, original.MaxRetries,
			original.TimeoutSecs, original.Requirements)
		if err != nil {
			c.JSON(503, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "task": newTask})
	default: // done | failed
		// Clone as a brand-new task. New ID, fresh CreatedAt, Retries=0.
		newTask, err := s.dispatchTaskWithParams(original.Type, original.Payload,
			original.Source, original.Priority, original.MaxRetries,
			original.TimeoutSecs, original.Requirements)
		if err != nil {
			c.JSON(503, gin.H{"error": err.Error()})
			return
		}
		Info("task restarted: original=%s new=%s type=%s", original.ID, newTask.ID, original.Type)
		c.JSON(200, gin.H{"ok": true, "task": newTask})
	}
}

type progressReq struct {
	TaskID   string `json:"task_id"`
	Progress int    `json:"progress"`
	Log      string `json:"log"`
}

func (s *Server) workerProgress(c *gin.Context) {
	id := c.Param("id")
	var req progressReq
	if err := bindJSON(c, &req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	t := s.store.GetTask(req.TaskID)
	if t == nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}
	if t.WorkerID != id {
		c.JSON(403, gin.H{"error": "task not owned by this worker"})
		return
	}
	s.store.UpdateProgress(req.TaskID, req.Progress, req.Log)
	c.JSON(200, gin.H{"ok": true})
}

type resultReq struct {
	TaskID  string `json:"task_id"`
	Result  string `json:"result"`
	Error   string `json:"error"`
	Success bool   `json:"success"`
}

func (s *Server) workerResult(c *gin.Context) {
	id := c.Param("id")
	var req resultReq
	if err := bindJSON(c, &req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	t := s.store.GetTask(req.TaskID)
	if t == nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}
	if t.WorkerID != id {
		c.JSON(403, gin.H{"error": "task not owned by this worker"})
		return
	}
	if req.Success {
		s.store.FinishTask(req.TaskID, TruncateResult(req.Result), "", true)
		s.store.RecordTaskResult(id, true)
	} else {
		s.store.FinishTask(req.TaskID, "", req.Error, false)
		s.store.RecordTaskResult(id, false)
		// automatic retry if allowed; pick a different worker than the one that failed
		if t := s.store.GetTask(req.TaskID); t != nil {
			if t.Retries < t.MaxRetries {
				if s.store.RetryTask(req.TaskID) {
					Info("task %s retrying (now=%d, max=%d) after worker %s failed: %s",
						req.TaskID, t.Retries, t.MaxRetries, id, req.Error)
					go s.retryTask(req.TaskID, id)
				}
			}
		}
	}
	c.JSON(200, gin.H{"ok": true})
}

func (s *Server) listWorkers(c *gin.Context) {
	c.JSON(200, s.store.ListWorkers())
}

// peerReplicate receives worker events from peer gateways and applies them
// locally. Used for gateway-to-gateway state replication.
type peerEvent struct {
	Event   string      `json:"event"`
	Payload interface{} `json:"payload"`
	TS      int64       `json:"ts"`
}

func (s *Server) peerReplicate(c *gin.Context) {
	var ev peerEvent
	if err := bindJSON(c, &ev); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	// Only replicate worker events. Tasks are ephemeral and not synced.
	switch ev.Event {
	case "worker_added", "worker_updated", "worker_removed", "worker_deleted":
		// payload is a *Worker
		data, _ := json.Marshal(ev.Payload)
		var w Worker
		if err := json.Unmarshal(data, &w); err != nil {
			c.JSON(400, gin.H{"error": "bad worker payload"})
			return
		}
		s.store.AddWorker(&w)
	default:
		// ignore other events (tasks etc.)
	}
	c.JSON(200, gin.H{"ok": true})
}

// authCheck reports whether auth is required. This endpoint is public so the
// panel can decide whether to prompt for a token before making any other call.
func (s *Server) authCheck(c *gin.Context) {
	c.JSON(200, gin.H{
		"required": s.authRequired,
		"hint":     "set Authorization: Bearer <token> or ?token=<token>",
	})
}

func (s *Server) getWorker(c *gin.Context) {
	id := c.Param("id")
	w := s.store.GetWorker(id)
	if w == nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	c.JSON(200, w)
}

func (s *Server) deleteWorker(c *gin.Context) {
	id := c.Param("id")
	w := s.store.RemoveWorker(id)
	if w == nil {
		c.JSON(404, gin.H{"error": "worker not found"})
		return
	}
	Info("worker deleted: id=%s name=%s type=%s", w.ID, w.Name, w.Type)
	c.JSON(200, gin.H{"ok": true})
}

// getStats returns lifetime task counters. Used by the panel's polling
// fallback when the WS is briefly disconnected, so the KPI doesn't flash 0.
func (s *Server) getStats(c *gin.Context) {
	c.JSON(200, s.store.StatsSnapshot())
}

// ---------- Task endpoints ----------

type createTaskReq struct {
	Type         string           `json:"type"`
	Payload      string           `json:"payload"`
	Source       string           `json:"source"`       // panel | openai
	Priority     int              `json:"priority"`     // higher = more important
	MaxRetries   int              `json:"max_retries"`  // 0 = no retry
	TimeoutSecs  int              `json:"timeout_secs"` // 0 = use default 300s
	Requirements TaskRequirements `json:"requirements"` // hard filters; empty = no filter
}

func (s *Server) createTask(c *gin.Context) {
	var req createTaskReq
	if err := bindJSON(c, &req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.Type == "" || req.Payload == "" {
		c.JSON(400, gin.H{"error": "type and payload required"})
		return
	}
	if req.Source == "" {
		req.Source = "panel"
	}
	task, err := s.dispatchTaskWithParams(req.Type, req.Payload, req.Source,
		req.Priority, req.MaxRetries, req.TimeoutSecs, req.Requirements)
	if err != nil {
		c.JSON(503, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, task)
}

func (s *Server) dispatchTask(taskType, payload, source string) (*Task, error) {
	return s.dispatchTaskWithParams(taskType, payload, source, 0, 0, 0, TaskRequirements{})
}

// dispatchTaskWithParams creates a task with the given scheduling hints and
// dispatches it to the best available worker (excluding any worker IDs in
// excludeWorkerID — used during retry). Hard requirements are evaluated by
// PickWorker before scoring; a worker that fails any requirement is not in
// the candidate set.
func (s *Server) dispatchTaskWithParams(taskType, payload, source string,
	priority, maxRetries, timeoutSecs int, requirements TaskRequirements, excludeWorkerID ...string) (*Task, error) {
	worker := s.store.PickWorker(taskType, requirements, excludeWorkerID...)
	if worker == nil {
		return nil, fmt.Errorf("no available worker for type=%s", taskType)
	}
	t := &Task{
		ID:           uuid.NewString(),
		Type:         taskType,
		Payload:      payload,
		Source:       source,
		Priority:     priority,
		MaxRetries:   maxRetries,
		Retries:      0,
		TimeoutSecs:  timeoutSecs,
		Status:       "pending",
		Progress:     0,
		CreatedAt:    time.Now(),
		Logs:         []string{},
		Requirements: requirements,
	}
	s.store.AddTask(t)
	s.store.AssignTask(t.ID, worker.ID)

	go s.pushToWorker(worker, t)
	return t, nil
}

// retryTask re-dispatches an EXISTING task (same ID, incremented retry count)
// to the next-best worker. Used after a failure or timeout.
func (s *Server) retryTask(taskID string, excludeWorkerID string) {
	t := s.store.GetTask(taskID)
	if t == nil {
		Warn("retryTask: task %s not found", taskID)
		return
	}
	// Prefer a different worker than the one that failed; if no alternative
	// is available (single-worker cluster) reuse the same one — better than
	// leaving the task stuck in pending forever.
	worker := s.store.PickWorker(t.Type, t.Requirements, excludeWorkerID)
	if worker == nil {
		worker = s.store.PickWorker(t.Type, t.Requirements)
		if worker == nil {
			Warn("retryTask: no worker available for type=%s", t.Type)
			return
		}
		Info("retryTask: no alternative worker, retrying on same worker %s", worker.ID)
	}
	s.store.AssignTask(taskID, worker.ID)
	go s.pushToWorker(worker, t)
}

// dispatchRetry is invoked by the timeout sweeper and workerResult handler to
// retry an existing task without creating a new ID. Keeps the same task record
// visible in the panel with the incremented Retries counter.
func (s *Server) dispatchRetry(taskType, payload, source string,
	priority, maxRetries, timeoutSecs, retryCount int, excludeWorkerID string) {
	// taskType/payload/etc are unused here — we keep the signature for
	// compatibility with caller hooks. We just need the task ID and
	// excludeWorkerID, but those aren't passed via this helper signature.
	// The actual retry path is via retryTaskByID below.
	_ = taskType
	_ = payload
	_ = source
	_ = priority
	_ = maxRetries
	_ = timeoutSecs
	_ = retryCount
	// (no-op for back-compat — real path below)
}

// DispatchRetryWithID is called by the timeout sweeper with the actual task ID.
func (s *Server) DispatchRetryWithID(taskID, excludeWorkerID string) {
	s.retryTask(taskID, excludeWorkerID)
}

// DispatchRetry exposes the retry helper so the timeout sweeper (started by
// main.go after Server is constructed) can invoke it.
func (s *Server) DispatchRetry(taskType, payload, source string,
	priority, maxRetries, timeoutSecs, retryCount int, excludeWorkerID string) {
	s.dispatchRetry(taskType, payload, source, priority, maxRetries, timeoutSecs, retryCount, excludeWorkerID)
}

// Shared HTTP client with a connection pool. Using http.DefaultClient creates
// a fresh TCP connection per request, which on busy clusters exhausts file
// descriptors under load. The pool keeps idle keep-alive connections.
var workerHTTP = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	},
}

// pushCancelToWorker tells a worker to abort a specific task. Best-effort:
// if the worker is unreachable we still leave the cancel flag in the store so
// the gateway side tracks it correctly.
func (s *Server) pushCancelToWorker(workerID, taskID string) {
	w := s.store.GetWorker(workerID)
	if w == nil {
		return
	}
	url := fmt.Sprintf("http://%s:%d/task/cancel", resolveWorkerHost(w), w.Port)
	body, _ := json.Marshal(map[string]string{"task_id": taskID})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := workerHTTP.Do(req)
	if err != nil {
		Warn("cancel push failed worker=%s task=%s: %v", workerID, taskID, err)
		return
	}
	defer resp.Body.Close()
}

// pushControlToWorker signals a worker to set / clear / kill its per-task
// pause_event (action = "pause" | "resume" | "stop"). Best-effort — if the
// worker is unreachable the gateway-side state is still authoritative.
func (s *Server) pushControlToWorker(workerID, taskID, action string) {
	w := s.store.GetWorker(workerID)
	if w == nil {
		return
	}
	url := fmt.Sprintf("http://%s:%d/task/%s", resolveWorkerHost(w), w.Port, action)
	body, _ := json.Marshal(map[string]string{"task_id": taskID})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := workerHTTP.Do(req)
	if err != nil {
		Warn("control push failed worker=%s task=%s action=%s: %v", workerID, taskID, action, err)
		return
	}
	defer resp.Body.Close()
}

func (s *Server) pushToWorker(w *Worker, t *Task) {
	url := fmt.Sprintf("http://%s:%d/task", resolveWorkerHost(w), w.Port)
	body, _ := json.Marshal(map[string]interface{}{
		"task_id": t.ID,
		"type":    t.Type,
		"payload": t.Payload,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Sign request body with HMAC so the worker can verify the request came
	// from this gateway (and not an attacker on the LAN).
	var resp *http.Response
	var err error
	if s.hmacSecret != nil {
		req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ts := time.Now().Unix()
		req.Header.Set("X-Timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("X-Signature", SignRequest(s.hmacSecret, body, ts))
		resp, err = workerHTTP.Do(req)
	} else {
		req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err = workerHTTP.Do(req)
	}
	if err != nil {
		errMsg := fmt.Sprintf("push to worker failed: %v", err)
		Errorf("%s task=%s", errMsg, t.ID)
		s.store.UpdateProgress(t.ID, 0, errMsg)
		s.store.FinishTask(t.ID, "", errMsg, false)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(resp.Body)
		errMsg := fmt.Sprintf("worker rejected task: status=%d body=%s", resp.StatusCode, string(buf))
		Errorf("%s task=%s", errMsg, t.ID)
		s.store.FinishTask(t.ID, "", errMsg, false)
		return
	}
	Info("task dispatched: id=%s type=%s worker=%s", t.ID, t.Type, w.ID)
}

func resolveWorkerHost(w *Worker) string {
	// Workers on localhost can be reached directly; remote workers use Host header.
	if w.Host == "::1" || w.Host == "127.0.0.1" || w.Host == "localhost" {
		return "127.0.0.1"
	}
	// If host is a private/LAN IP, use as-is.
	if ip := net.ParseIP(w.Host); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() {
			return w.Host
		}
	}
	// Fallback: treat as hostname
	return w.Host
}

func (s *Server) listTasks(c *gin.Context) {
	c.JSON(200, s.store.ListTasks())
}

func (s *Server) getTask(c *gin.Context) {
	id := c.Param("id")
	t := s.store.GetTask(id)
	if t == nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	c.JSON(200, t)
}

func (s *Server) deleteTask(c *gin.Context) {
	id := c.Param("id")
	t := s.store.GetTask(id)
	if t == nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	// If the task is still running, request cancellation first so the
	// worker stops cleanly. The actual delete happens immediately
	// regardless of whether the worker has acknowledged.
	if t.Status == "pending" || t.Status == "running" {
		if t.WorkerID != "" {
			go s.pushCancelToWorker(t.WorkerID, id)
		}
	}
	s.store.RemoveTask(id)
	Info("task deleted: id=%s type=%s was=%s", id, t.Type, t.Status)
	c.JSON(200, gin.H{"ok": true})
}

func (s *Server) listModels(c *gin.Context) {
	c.JSON(200, gin.H{
		"object": "list",
		"data": []gin.H{
			{"id": "deskgrid-cloud-gateway", "object": "model", "owned_by": "deskgrid-cloud"},
			{"id": "deskgrid-cloud-code", "object": "model", "owned_by": "deskgrid-cloud"},
			{"id": "deskgrid-cloud-doc", "object": "model", "owned_by": "deskgrid-cloud"},
			{"id": "deskgrid-cloud-csv", "object": "model", "owned_by": "deskgrid-cloud"},
		},
	})
}

// Keep imports referenced
var _ = strings.TrimSpace

// ---------- Local-task offload (Phase 3) ----------
//
// Scans the gateway host for heavy services (ComfyUI / SD WebUI / Ollama /
// ffmpeg / blender / custom YAML rules). Each detected entity becomes a
// LocalTask that the operator can dispatch a copy of to a remote worker.
// The local task itself is NEVER touched — the dispatch is parallel and
// independent.

// StartLocalTaskScanner wires the localtasks.Registry into the server and
// launches the background scan loop. Safe to call once at startup; calling
// it twice would leak the previous ticker. The interval is read from the
// GATEWAY_LOCALTASKS_INTERVAL env var (default 10s) but can be overridden
// by passing a non-zero interval.
//
// cfgPath may be "" to disable YAML-driven custom rules entirely.
func (s *Server) StartLocalTaskScanner(ctx context.Context, cfgPath string, interval time.Duration) {
	if s.ltReg != nil {
		return
	}
	if interval <= 0 {
		if v := os.Getenv("GATEWAY_LOCALTASKS_INTERVAL"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				interval = d
			}
		}
	}
	s.ltReg = localtasks.NewRegistry(cfgPath, interval)
	s.ltReg.Start(ctx)
	Info("local-task scanner ENABLED (interval=%s, config=%s)", interval, cfgPath)
}

// LocalTaskRegistry exposes the registry so main.go can query it (currently
// unused but kept for future hot-reload / health endpoints).
func (s *Server) LocalTaskRegistry() *localtasks.Registry { return s.ltReg }

// listLocalTasks — GET /api/local-tasks
//
// Returns the current snapshot sorted by (throwable, busy, service). An
// empty slice (not null) is returned when nothing is detected, so the UI's
// .empty placeholder renders correctly.
func (s *Server) listLocalTasks(c *gin.Context) {
	if s.ltReg == nil {
		c.JSON(200, []localtasks.LocalTask{})
		return
	}
	snap := s.ltReg.Snapshot()
	if snap == nil {
		snap = []localtasks.LocalTask{}
	}
	c.JSON(200, snap)
}

// scanLocalTasksNow — POST /api/local-tasks/scan
//
// Forces an immediate rescan (without waiting for the ticker) and returns
// the fresh snapshot. Used by the UI's "手动扫描" button.
func (s *Server) scanLocalTasksNow(c *gin.Context) {
	if s.ltReg == nil {
		c.JSON(503, gin.H{"error": "local-task scanner not enabled"})
		return
	}
	if err := s.ltReg.Scan(c.Request.Context()); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	snap := s.ltReg.Snapshot()
	if snap == nil {
		snap = []localtasks.LocalTask{}
	}
	c.JSON(200, gin.H{"ok": true, "tasks": snap})
}

// dispatchLocalReq is the body shape for POST /api/local-tasks/:id/dispatch.
// All fields are optional — defaults come from the cached LocalTask.
type dispatchLocalReq struct {
	Overrides *struct {
		Payload      string           `json:"payload,omitempty"`
		Requirements TaskRequirements `json:"requirements,omitempty"`
		Priority     int              `json:"priority,omitempty"`
		MaxRetries   int              `json:"max_retries,omitempty"`
		TimeoutSecs  int              `json:"timeout_secs,omitempty"`
	} `json:"overrides,omitempty"`
}

// dispatchLocalTask — POST /api/local-tasks/:id/dispatch
//
// Looks up the LocalTask by id, applies any overrides, then re-enters the
// regular dispatch path. The "local-offload" source tag is stamped on the
// resulting Task so audit trails can distinguish these from panel/openai
// dispatches.
func (s *Server) dispatchLocalTask(c *gin.Context) {
	if s.ltReg == nil {
		c.JSON(503, gin.H{"error": "local-task scanner not enabled"})
		return
	}
	id := c.Param("id")
	lt, ok := s.ltReg.Get(id)
	if !ok {
		c.JSON(404, gin.H{"error": "local task not found (may have stopped)"})
		return
	}

	var req dispatchLocalReq
	// body is optional — bind best-effort
	_ = c.ShouldBindJSON(&req)

	payload := lt.PayloadTpl
	prio, retries, timeout := 0, 0, 0
	var reqs TaskRequirements
	if req.Overrides != nil {
		if req.Overrides.Payload != "" {
			payload = req.Overrides.Payload
		}
		if req.Overrides.Priority > 0 {
			prio = req.Overrides.Priority
		}
		if req.Overrides.MaxRetries > 0 {
			retries = req.Overrides.MaxRetries
		}
		if req.Overrides.TimeoutSecs > 0 {
			timeout = req.Overrides.TimeoutSecs
		}
		reqs = req.Overrides.Requirements
	}

	task, err := s.dispatchTaskWithParams(
		lt.MapToType, payload, "local-offload",
		prio, retries, timeout, reqs,
	)
	if err != nil {
		// dispatchTaskWithParams already returns a formatted error; bubble it up.
		c.JSON(503, gin.H{"error": err.Error()})
		return
	}
	Info("local-offload: localtask=%s → task=%s type=%s worker=%s",
		lt.ID, task.ID, task.Type, task.WorkerID)
	c.JSON(200, task)
}

// reloadLocalTasksConfig — POST /api/local-tasks/reload
//
// Hot-reloads the YAML config. Active scans in flight keep their loaded
// rules; the next scan picks up the new ones.
func (s *Server) reloadLocalTasksConfig(c *gin.Context) {
	if s.ltReg == nil {
		c.JSON(503, gin.H{"error": "local-task scanner not enabled"})
		return
	}
	s.ltReg.Reload()
	c.JSON(200, gin.H{"ok": true, "tasks": len(s.ltReg.Snapshot())})
}
