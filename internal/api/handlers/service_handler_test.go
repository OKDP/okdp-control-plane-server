package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// deploySpy embeds the interface so only DeployService needs a body.
type deploySpy struct {
	service.ServiceService
	err error
}

func (s *deploySpy) DeployService(context.Context, string, models.ServiceRequest) (*models.ServiceInstance, error) {
	return nil, s.err
}

func deploy(t *testing.T, err error) (int, map[string]string) {
	t.Helper()
	engine := gin.New()
	engine.POST("/api/projects/:name/services", NewServiceHandler(&deploySpy{err: err}, nil).DeployService)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/projects/a/services", strings.NewReader(`{"service":"trino","instanceName":"b-c"}`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, req)
	body := map[string]string{}
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder.Code, body
}

// Both are 409, but they are not the same mistake: the console must tell a
// duplicate instance from a release name another project already uses.
func TestDeployConflictsCarryDistinctCodes(t *testing.T) {
	code, body := deploy(t, &service.ReleaseNameTakenError{ErrReleaseTaken: &gitops.ErrReleaseTaken{Release: "a-b-c", Project: "a-b", Instance: "c"}})
	if code != http.StatusConflict || body["code"] != ErrorCodeReleaseNameTaken ||
		body["error"] != "release name 'a-b-c' is already used by instance 'c' of project 'a-b'" {
		t.Fatalf("release collision: %d %v", code, body)
	}

	code, body = deploy(t, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "services"}, "b-c"))
	if code != http.StatusConflict || body["code"] != ErrorCodeInstanceExists ||
		body["error"] != "Instance 'b-c' already exists in project 'a'" {
		t.Fatalf("duplicate instance: %d %v", code, body)
	}
}
