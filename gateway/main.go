package main

import (
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

func main() {
	logDir := envOr("LOG_DIR", "./logs")
	internal.InitLogger(logDir)

	store := internal.NewStore()
	hub := internal.NewHub()
	internal.SetStoreRef(store)
	store.SetNotifier(hub.Broadcast)
	store.StartOfflineSweeper(10*time.Second, 2*time.Second)

	srv := internal.NewServer(store, hub)
	engine := srv.Engine()

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
	internal.Info("deskgrid-cloud-gateway listening on %s", addr)
	if err := engine.Run(addr); err != nil {
		internal.Errorf("gateway exited: %v", err)
		os.Exit(1)
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
