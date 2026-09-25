package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// DefaultMaxBodyBytes bounds every request body. The API takes forms,
// parameters and a SparkApplication YAML: kilobytes. Kubernetes itself refuses
// objects much past 1 MiB, so nothing legitimate comes close.
const DefaultMaxBodyBytes = 1 << 20

// LimitRequestBody refuses a body larger than limit. A declared length is
// refused at once with 413; an undeclared one (chunked) fails the read, so the
// handler's bind answers 400 instead of buffering whatever the client sends.
func LimitRequestBody(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > limit {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "the request body is too large"})
			return
		}
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	}
}

// SecurityHeaders sets the response headers every answer should carry. API
// answers hold project data and are per user: no cache may keep them. The
// event streams set their own Cache-Control, which replaces this one.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(c.Request.URL.Path, "/api/") || c.Request.URL.Path == "/api" {
			h.Set("Cache-Control", "no-store")
		}
		c.Next()
	}
}
