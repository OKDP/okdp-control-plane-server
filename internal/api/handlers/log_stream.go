package handlers

import (
	"bufio"
	"errors"
	"io"
)

const (
	defaultLogBufferSize = 64 * 1024
	maxLogLineSize       = 1024 * 1024
)

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
