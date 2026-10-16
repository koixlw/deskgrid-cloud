package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	internal "deskgrid-cloud-gateway/internal"
)

//go:embed web/*
var webFS embed.FS

// artifactsDir is the on-disk folder where workers write generated images,
// audio, and video. Exposed read-only via HTTP at /artifacts/.
var artifactsDir = envOr("ARTIFACTS_DIR", `E:\deskgrid-cloud\artifacts`)

// dataDir holds the gateway's persistence snapshots (workers.json, tasks.json).
var dataDir = envOr("GATEWAY_DATA_DIR", "./data")

func main() {
	logDir := envOr("LOG_DIR", "./logs")
	internal.InitLogger(logDir)

	// Auth: shared-secret token. Empty = dev mode (no auth).
	token := envOr("GATEWAY_TOKEN", "")
	authRequired := internal.AuthRequired(token)
	if authRequired {
		internal.Info("auth ENABLED (token set via GATEWAY_TOKEN)")
	} else {
		internal.Info("auth DISABLED (GATEWAY_TOKEN unset; dev mode)")
	}

	store := internal.NewStore()
	hub := internal.NewHub()
	internal.SetStoreRef(store)

	// Persistence subscribes to store events. Chain hub.Broadcast + persistence
	// so every state change broadcasts to WS AND is queued for disk flush.
	persistence := internal.NewPersistence(dataDir, store)
	hubBroadcast := hub.Broadcast
	peers := internal.NewPeerSync(os.Getenv("GATEWAY_PEERS"))
	store.SetNotifier(func(event string, payload interface{}) {
		hubBroadcast(event, payload)
		persistence.OnEvent(event, payload)
		peers.BroadcastEvent(event, payload)
	})
	if peers.Enabled() {
		internal.Info("peer replication ENABLED")
	}
	store.StartOfflineSweeper(6*time.Second, 2*time.Second)
	store.StartCleanupLoop(5 * time.Minute)
	defer persistence.Stop()

	// Restore previous state from disk, then settle in-flight tasks.
	if workers, tasks, inFlight, savedStats := internal.Load(dataDir); len(workers) > 0 || len(tasks) > 0 || savedStats.Completed > 0 || savedStats.Failed > 0 {
		internal.Info("persistence: restored %d workers, %d tasks (%d in-flight); stats completed=%d failed=%d",
			len(workers), len(tasks), len(inFlight), savedStats.Completed, savedStats.Failed)
		store.SetStats(savedStats)
		for _, w := range workers {
			store.AddWorker(w)
		}
		for _, t := range tasks {
			if t.Status == "running" || t.Status == "pending" {
				continue // in-flight, will be marked failed below
			}
			store.AddTask(t)
		}
		for _, id := range inFlight {
			// FailTaskDirect is intentional: in-flight tasks were counted in
			// the prior session's stats, so we do NOT re-count them here.
			store.FailTaskDirect(id, "gateway restarted; task was in-flight when gateway went down")
		}
	}
	go persistence.Run()

	srv := internal.NewServer(store, hub, token)
	engine := srv.Engine()

	// Local-task scanner: detects ComfyUI/SD/Ollama/ffmpeg/blender running
	// on this host so the operator can dispatch a copy to a remote worker.
	// Disabled by passing "" for cfgPath. Interval is read from
	// GATEWAY_LOCALTASKS_INTERVAL (default 10s).
	ltCfg := envOr("GATEWAY_LOCALTASKS_CONFIG", "localtasks.yaml")
	srv.StartLocalTaskScanner(context.Background(), ltCfg, 0)

	// Timeout sweeper dispatches retries via the server's helper.
	store.StartTimeoutSweeper(15*time.Second, srv.DispatchRetryWithID)

	// serve embedded web/
	if sub, err := fs.Sub(webFS, "web"); err == nil {
		fileServer := http.FileServer(http.FS(sub))
		engine.GET("/", func(c *gin.Context) {
			serveIndex(c, sub)
		})
		engine.GET("/index.html", func(c *gin.Context) {
			serveIndex(c, sub)
		})
		// static asset paths
		engine.GET("/static/*filepath", func(c *gin.Context) {
			p := strings.TrimPrefix(c.Param("filepath"), "/")
			c.Request.URL.Path = "/" + p
			fileServer.ServeHTTP(c.Writer, c.Request)
		})
		// SPA fallback: any unmatched non-API GET → try static, else index.html
		engine.NoRoute(func(c *gin.Context) {
			if c.Request.Method != http.MethodGet {
				c.Status(http.StatusNotFound)
				return
			}
			p := c.Request.URL.Path
			if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/v1/") ||
				strings.HasPrefix(p, "/ws") || p == "/health" {
				c.Status(http.StatusNotFound)
				return
			}
			// if the request maps to a real file in the embed FS, serve it; otherwise SPA fallback
			rel := strings.TrimPrefix(p, "/")
			if rel == "" {
				serveIndex(c, sub)
				return
			}
			if _, err := fs.Stat(sub, rel); err == nil {
				fileServer.ServeHTTP(c.Writer, c.Request)
				return
			}
			serveIndex(c, sub)
		})
	}

	// serve generated artifacts (images / audio / video written by workers)
	engine.GET("/artifacts/*filepath", func(c *gin.Context) {
		rel := strings.TrimPrefix(c.Param("filepath"), "/")
		if rel == "" {
			c.String(400, "filename required")
			return
		}
		// path traversal guard
		if strings.Contains(rel, "..") {
			c.String(400, "bad path")
			return
		}
		full := artifactsDir + "/" + filepath.FromSlash(rel)
		c.File(full)
	})

	addr := envOr("GATEWAY_ADDR", ":13000")
	tlsCert := envOr("GATEWAY_TLS_CERT", "")
	tlsKey := envOr("GATEWAY_TLS_KEY", "")
	if HMAC_SECRET := envOr("GATEWAY_HMAC_SECRET", ""); HMAC_SECRET != "" {
		// Log first 12 chars of sha256 to compare with worker logs
		sum := sha256.Sum256([]byte(HMAC_SECRET))
		internal.Info("HMAC worker auth ENABLED (sha256=%x)", sum[:6])
	} else {
		internal.Info("HMAC worker auth disabled (GATEWAY_HMAC_SECRET unset)")
	}
	if tlsCert != "" && tlsKey != "" {
		internal.Info("deskgrid-cloud-gateway listening on %s (TLS)", addr)
		if err := engine.RunTLS(addr, tlsCert, tlsKey); err != nil {
			internal.Errorf("gateway exited: %v", err)
			os.Exit(1)
		}
	} else {
		internal.Info("deskgrid-cloud-gateway listening on %s (no TLS, internal network only)", addr)
		if err := engine.Run(addr); err != nil {
			internal.Errorf("gateway exited: %v", err)
			os.Exit(1)
		}
	}
}

func serveFile(c *gin.Context, h http.Handler, name string) {
	c.Request.URL.Path = "/" + name
	h.ServeHTTP(c.Writer, c.Request)
}

func serveIndex(c *gin.Context, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		c.String(500, "index.html not found: %v", err)
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.String(200, string(data))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}