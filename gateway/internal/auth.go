package internal

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// AuthMiddleware enforces Bearer-token auth on protected paths.
// If token == "" auth is disabled (development mode).
//
// Public paths (always allowed even when token is set):
//
//	/, /index.html, /health, /api/auth-check, /static/*
func AuthMiddleware(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token == "" {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		if isPublicPath(path) {
			c.Next()
			return
		}
		provided := extractToken(c)
		// constant-time compare to avoid timing leaks
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "unauthorized",
				"hint":  "set Authorization: Bearer <token> or ?token=<token>",
			})
			return
		}
		c.Next()
	}
}

func isPublicPath(p string) bool {
	switch p {
	case "/", "/index.html", "/health", "/api/auth-check":
		return true
	}
	if strings.HasPrefix(p, "/static/") {
		return true
	}
	return false
}

// extractToken reads the token from Authorization header or ?token query.
func extractToken(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return strings.TrimSpace(c.Query("token"))
}

// AuthRequired returns true when the gateway token is non-empty.
func AuthRequired(token string) bool { return token != "" }