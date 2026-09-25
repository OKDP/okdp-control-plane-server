package handlers

import (
	"bufio"
	"errors"
	"io"
	"strconv"

	"github.com/gin-gonic/gin"
)

const (
	defaultLogBufferSize = 64 * 1024
	maxLogLineSize       = 1024 * 1024

	defaultLogTailLines = 100
	// maxLogTailLines caps what one request makes the kubelet read back and
	// this server relay: enough to look back, not to dump a node's disk.
	maxLogTailLines = 10000
)

// tailLinesParam reads the tailLines query parameter: the default when absent
// or not a positive number, capped at maxLogTailLines.
func tailLinesParam(c *gin.Context) int64 {
	tailLines := int64(defaultLogTailLines)
	if tl := c.Query("tailLines"); tl != "" {
		if v, err := strconv.ParseInt(tl, 10, 64); err == nil && v > 0 {
			tailLines = min(v, maxLogTailLines)
		}
	}
	return tailLines
}

func newLogStreamScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, defaultLogBufferSize), maxLogLineSize)
	return scanner
}

func logStreamErrorMessage(err error) string {
	if errors.Is(err, bufio.ErrTooLong) {
		return "log line too long, stream interrupted"
	}
	return "log stream interrupted"
}
