package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func projectRouter(get func(c *gin.Context, name string) (*models.Project, error)) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/projects/:name/services", RequireProject(get), func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

// A caller with no access learns nothing about which projects exist.
func TestAProjectTheCallerCannotReadIsNotFound(t *testing.T) {
	r := projectRouter(func(_ *gin.Context, name string) (*models.Project, error) {
		return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, name, errors.New("denied"))
	})

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/finance/services", nil))

	assert.Equal(t, http.StatusNotFound, response.Code)
}
