package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Server wires HTTP routes to the store + hub.
type Server struct {
	store        *Store
	hub          *Hub
	engine       *gin.Engine
	authRequired bool
}

func NewServer(store *Store, hub *Hub, token string) *Server {
	gin.SetMode(gin.ReleaseMode)
	s := &Server{
		store:        store,
		hub:          hub,
		authRequired: AuthRequired(token),
	}
	s.engine = gin.New()
	s.engine.Use(gin.Recovery())
	s.engine.Use(corsMiddleware())
	s.engine.Use(AuthMiddleware(token))
	s.routes()
	return s
}

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
		api.POST("/workers/register", bodyLimit(16*1024), s.registerWorker)
		api.DELETE("/workers/:id", s.deleteWorker)
		api.POST("/workers/:id/heartbeat", bodyLimit(16*1024), s.heartbeat)
		api.POST("/workers/:id/progress", bodyLimit(16*1024), s.workerProgress)
		api.POST("/workers/:id/result", bodyLimit(16*1024*1024), s.workerResult)
		api.POST("/workers/:id/task/cancel", bodyLimit(1024), s.cancelTask)

		api.GET("/tasks", s.listTasks)
		api.GET("/tasks/:id", s.getTask)
		api.POST("/tasks", bodyLimit(64*1024), s.createTask)
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
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	CPU      string `json:"cpu"`
	Cores    int    `json:"cores"`
	MemoryMB int    `json:"memory_mb"`
	Port     int    `json:"port"`
}

func (s *Server) registerWorker(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.ID == "" {
		req.ID = uuid.NewString()
	}
	if req.Type == "" || (req.Type != "code" && req.Type != "doc" && req.Type != "csv" &&
		req.Type != "image" && req.Type != "audio" && req.Type != "video") {
		c.JSON(400, gin.H{"error": "type must be code | doc | csv | image | audio | video"})
		return
	}
	host := c.ClientIP()
	// If client reports a private IP, prefer it; gateway only sees the IP it received from.
	if h := c.GetHeader("X-Worker-Host"); h != "" {
		host = h
	}
	w := &Worker{
		ID:       req.ID,
		Type:     req.Type,
		Name:     req.Name,
		CPU:      req.CPU,
		Cores:    req.Cores,
		MemoryMB: req.MemoryMB,
		Host:     host,
		Port:     req.Port,
	}
	s.store.AddWorker(w)
	Info("worker registered: id=%s type=%s name=%s cpu=%s cores=%d mem=%dMB",
		w.ID, w.Type, w.Name, w.CPU, w.Cores, w.MemoryMB)
	c.JSON(200, gin.H{"ok": true, "id": w.ID})
}

type heartbeatReq struct {
	CPUUsage      float64 `json:"cpu_usage"`
	MemUsage      float64 `json:"mem_usage"`
	QueueLen      int     `json:"queue_len"`
	GPUMemUsedMB  int64   `json:"gpu_mem_used_mb"`
	GPUMemTotalMB int64   `json:"gpu_mem_total_mb"`
}

func (s *Server) heartbeat(c *gin.Context) {
	id := c.Param("id")
	var req heartbeatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	w := s.store.UpdateHeartbeat(id, req.CPUUsage, req.MemUsage, req.QueueLen, req.GPUMemUsedMB, req.GPUMemTotalMB)
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
	if err := c.ShouldBindJSON(&req); err != nil {
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

type progressReq struct {
	TaskID   string `json:"task_id"`
	Progress int    `json:"progress"`
	Log      string `json:"log"`
}

func (s *Server) workerProgress(c *gin.Context) {
	id := c.Param("id")
	var req progressReq
	if err := c.ShouldBindJSON(&req); err != nil {
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
	if err := c.ShouldBindJSON(&req); err != nil {
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
		s.store.FinishTask(req.TaskID, req.Result, "", true)
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

// ---------- Task endpoints ----------

type createTaskReq struct {
	Type        string `json:"type"`
	Payload     string `json:"payload"`
	Source      string `json:"source"`       // panel | openai
	Priority    int    `json:"priority"`     // higher = more important
	MaxRetries  int    `json:"max_retries"`  // 0 = no retry
	TimeoutSecs int    `json:"timeout_secs"` // 0 = use default 300s
}

func (s *Server) createTask(c *gin.Context) {
	var req createTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
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
		req.Priority, req.MaxRetries, req.TimeoutSecs)
	if err != nil {
		c.JSON(503, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, task)
}

func (s *Server) dispatchTask(taskType, payload, source string) (*Task, error) {
	return s.dispatchTaskWithParams(taskType, payload, source, 0, 0, 0)
}

// dispatchTaskWithParams creates a task with the given scheduling hints and
// dispatches it to the best available worker (excluding any worker IDs in
// excludeWorkerID — used during retry).
func (s *Server) dispatchTaskWithParams(taskType, payload, source string,
	priority, maxRetries, timeoutSecs int, excludeWorkerID ...string) (*Task, error) {
	worker := s.store.PickWorker(taskType, excludeWorkerID...)
	if worker == nil {
		return nil, fmt.Errorf("no available worker for type=%s", taskType)
	}
	t := &Task{
		ID:          uuid.NewString(),
		Type:        taskType,
		Payload:     payload,
		Source:      source,
		Priority:    priority,
		MaxRetries:  maxRetries,
		Retries:     0,
		TimeoutSecs: timeoutSecs,
		Status:      "pending",
		Progress:    0,
		CreatedAt:   time.Now(),
		Logs:        []string{},
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
	worker := s.store.PickWorker(t.Type, excludeWorkerID)
	if worker == nil {
		worker = s.store.PickWorker(t.Type)
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

func (s *Server) pushToWorker(w *Worker, t *Task) {
	url := fmt.Sprintf("http://%s:%d/task", resolveWorkerHost(w), w.Port)
	body, _ := json.Marshal(map[string]interface{}{
		"task_id": t.ID,
		"type":    t.Type,
		"payload": t.Payload,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := workerHTTP.Do(req)
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
