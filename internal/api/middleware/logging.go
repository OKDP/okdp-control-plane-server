package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// RequestLogger logs incoming HTTP requests using Logrus
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		latency := time.Since(start)
		statusCode := c.Writer.Status()
		method := c.Request.Method
		clientIP := c.ClientIP()

		entry := logrus.WithFields(logrus.Fields{
			"status":   statusCode,
			"method":   method,
			"path":     path,
			"ip":       clientIP,
			"latency":  latency.String(), // Text output is cleaner than ns
			"duration": latency,
		})

		if len(c.Errors) > 0 {
			entry.Error(c.Errors.String())
		} else {
			if statusCode >= 500 {
				entry.Error("Internal Server Error")
			} else if statusCode >= 400 {
				entry.Warn("Bad Request")
			} else {
				entry.Info("Request Handled")
			}
		}
	}
}
