package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestTailLinesIsDefaultedAndCapped(t *testing.T) {
	for query, want := range map[string]int64{
		"":                         defaultLogTailLines,
		"?tailLines=50":            50,
		"?tailLines=0":             defaultLogTailLines,
		"?tailLines=-5":            defaultLogTailLines,
		"?tailLines=abc":           defaultLogTailLines,
		"?tailLines=10000":         10000,
		"?tailLines=9999999999999": maxLogTailLines,
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/logs"+query, nil)
		if got := tailLinesParam(c); got != want {
			t.Errorf("%q: got %d, want %d", query, got, want)
		}
	}
}
