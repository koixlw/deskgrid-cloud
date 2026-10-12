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
	store    *Store
	hub      *Hub
	engine   *gin.Engine
}

func NewServer(store *Store, hub *Hub) *Server {
	gin.SetMode(gin.ReleaseMode)
	s := &Server{store: store, hub: hub}
	s.engine = gin.New()
	s.engine.Use(gin.Recovery())
	s.engine.Use(corsMiddleware())
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

func (s *Server) routes() {
	api := s.engine.Group("/api")
	{
		api.GET("/workers", s.listWorkers)
		api.GET("/workers/:id", s.getWorker)
		api.POST("/workers/register", s.registerWorker)
		api.DELETE("/workers/:id", s.deleteWorker)
		api.POST("/workers/:id/heartbeat", s.heartbeat)
		api.POST("/workers/:id/progress", s.workerProgress)
		api.POST("/workers/:id/result", s.workerResult)

		api.GET("/tasks", s.listTasks)
		api.GET("/tasks/:id", s.getTask)
		api.POST("/tasks", s.createTask)
	}

	s.engine.GET("/ws", func(c *gin.Context) { s.hub.HandleWS(c.Writer, c.Request) })

	// OpenAI-compatible endpoint
	s.engine.POST("/v1/chat/completions", s.handleChatCompletions)
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
	CPUUsage float64 `json:"cpu_usage"`
	MemUsage float64 `json:"mem_usage"`
	QueueLen int     `json:"queue_len"`
}

func (s *Server) heartbeat(c *gin.Context) {
	id := c.Param("id")
	var req heartbeatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	w := s.store.UpdateHeartbeat(id, req.CPUUsage, req.MemUsage, req.QueueLen)
	if w == nil {
		c.JSON(404, gin.H{"error": "worker not found"})
		return
	}
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
	} else {
		s.store.FinishTask(req.TaskID, "", req.Error, false)
	}
	c.JSON(200, gin.H{"ok": true})
}

func (s *Server) listWorkers(c *gin.Context) {
	c.JSON(200, s.store.ListWorkers())
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
	Type    string `json:"type"`
	Payload string `json:"payload"`
	Source  string `json:"source"` // panel | openai
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
	task, err := s.dispatchTask(req.Type, req.Payload, req.Source)
	if err != nil {
		c.JSON(503, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, task)
}

func (s *Server) dispatchTask(taskType, payload, source string) (*Task, error) {
	worker := s.store.PickWorker(taskType)
	if worker == nil {
		return nil, fmt.Errorf("no available worker for type=%s", taskType)
	}
	t := &Task{
		ID:        uuid.NewString(),
		Type:      taskType,
		Payload:   payload,
		Source:    source,
		Status:    "pending",
		Progress:  0,
		CreatedAt: time.Now(),
		Logs:      []string{},
	}
	s.store.AddTask(t)
	s.store.AssignTask(t.ID, worker.ID)

	// async push to worker
	go s.pushToWorker(worker, t)
	return t, nil
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
	resp, err := http.DefaultClient.Do(req)
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
