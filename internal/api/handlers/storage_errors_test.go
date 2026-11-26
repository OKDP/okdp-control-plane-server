package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	enginefake "github.com/okdp/okdp-control-plane-server/internal/engine/fake"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

var (
	uncertain    = fmt.Errorf("push: %w", gitstore.ErrUncertain)
	unknown      = &engine.UnknownError{Operation: engine.Operation{Outcome: engine.OutcomeUnknown}}
	gitUnavail     = fmt.Errorf("fetch: %w", gitstore.ErrUnavailable)
	gitBusy        = fmt.Errorf("lock: %w", gitstore.ErrBusy)
	nsProvisioning = fmt.Errorf("%w: namespace demo", engine.ErrNamespaceProvisioning)
	gitRejected    = fmt.Errorf("push: %w", gitstore.ErrRejected)
	conflict     = apierrors.NewConflict(schema.GroupResource{Resource: "secretstores"}, "vault", errors.New("changed"))
	duplicate    = apierrors.NewAlreadyExists(schema.GroupResource{Resource: "namespaces"}, "demo")
	badRequest   = apierrors.NewBadRequest("invalid name")
	unavailable  = errors.New("cluster unreachable")
)

func TestStorageStatus(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
		ok     bool
	}{
		{uncertain, http.StatusGatewayTimeout, codeStorageUnknown, true},
		{unknown, http.StatusGatewayTimeout, codeStorageUnknown, true},
		{gitUnavail, http.StatusServiceUnavailable, codeStorageUnavailable, true},
		{gitBusy, http.StatusServiceUnavailable, codeStorageUnavailable, true},
		{nsProvisioning, http.StatusServiceUnavailable, codeStorageUnavailable, true},
		{gitRejected, http.StatusBadGateway, codeStorageRejected, true},
		{conflict, http.StatusConflict, codeStorageConflict, true},
		{duplicate, http.StatusConflict, codeStorageConflict, true},
		{unavailable, 0, "", false},
	} {
		status, code, ok := storageStatus(c.err)
		assert.Equal(t, c.status, status, c.err.Error())
		assert.Equal(t, c.code, code, c.err.Error())
		assert.Equal(t, c.ok, ok, c.err.Error())
	}
}

func serve(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestADeletionAppliedLaterAnswers202WithItsOperation(t *testing.T) {
	f := enginefake.New()
	ctx := context.Background()
	_, err := f.CreateCatalogEntry(ctx, models.PlatformService{
		Name: "jupyterhub", Versions: []string{"1.0.0"}, DefaultVersion: "1.0.0", Category: "Notebooks",
	}, engine.WriteOptions{})
	require.NoError(t, err)
	cluster := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	svc := service.NewDefaultServiceService(f.Engine(), &provisionerSpy{}, cluster, k8sfake.NewClientset(), nil)
	created, err := svc.DeployService(ctx, "demo", models.ServiceRequest{Service: "jupyterhub", InstanceName: "jh", Parameters: map[string]any{}})
	require.NoError(t, err)
	h := NewServiceHandler(svc, f.Engine().Catalog)
	r := gin.New()
	r.DELETE("/api/projects/:name/services/:serviceName", h.DeleteService)

	rec := serve(r, http.MethodDelete, "/api/projects/demo/services/jh", "")

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body models.OperationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(t, body.Operation)
	assert.Equal(t, "accepted", body.Operation.Outcome)
	assert.NotEmpty(t, body.Operation.DesiredRevision)
	assert.NotEqual(t, created.DesiredRevision, body.Operation.DesiredRevision)
}

type projectsFailing struct {
	service.ProjectService
	err error
}

func (p projectsFailing) CreateProject(context.Context, *models.Project) error { return p.err }
func (p projectsFailing) DeleteProject(context.Context, string) error          { return p.err }

type storesFailing struct {
	service.SecretStoreService
	err error
}

func (s storesFailing) DeleteSecretStore(context.Context, string, string) error { return s.err }

type externalsFailing struct {
	service.ExternalSecretService
	err error
}

func (s externalsFailing) DeleteExternalSecret(context.Context, string, string) error { return s.err }

type connectionsFailing struct {
	service.ConnectionService
	err error
}

func (s connectionsFailing) Delete(context.Context, string, string) error { return s.err }

func TestAnUncertainCatalogWriteIsAGatewayTimeout(t *testing.T) {
	for err, status := range map[error]int{
		unknown:      http.StatusGatewayTimeout,
		uncertain:    http.StatusGatewayTimeout,
		gitUnavail:   http.StatusServiceUnavailable,
		gitRejected:  http.StatusBadGateway,
		engine.ErrNotFound: http.StatusNotFound,
		&engine.RejectedError{Operation: engine.Rejected(engine.CodeConflict, "already exists")}: http.StatusConflict,
		unavailable:  http.StatusInternalServerError,
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		(&ServiceHandler{}).writeCatalogError(c, err)
		assert.Equal(t, status, rec.Code, err.Error())
	}
}

func TestAnUncertainWriteIsAGatewayTimeoutOnEveryRoute(t *testing.T) {
	routes := func(err error) *gin.Engine {
		r := gin.New()
		r.POST("/projects", NewProjectHandler(projectsFailing{err: err}).CreateProject)
		r.DELETE("/projects/:name", NewProjectHandler(projectsFailing{err: err}).DeleteProject)
		r.DELETE("/stores/:name/:storeName", NewSecretStoreHandler(storesFailing{err: err}).DeleteSecretStore)
		r.DELETE("/externals/:name/:esName", NewExternalSecretHandler(externalsFailing{err: err}).DeleteExternalSecret)
		r.DELETE("/connections/:name/:connName", NewConnectionHandler(connectionsFailing{err: err}).DeleteConnection)
		return r
	}
	calls := []struct{ method, path, body string }{
		{http.MethodPost, "/projects", `{"name":"demo"}`},
		{http.MethodDelete, "/projects/demo", ""},
		{http.MethodDelete, "/stores/demo/vault", ""},
		{http.MethodDelete, "/externals/demo/db", ""},
		{http.MethodDelete, "/connections/demo/pg", ""},
	}
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{uncertain, http.StatusGatewayTimeout, codeStorageUnknown},
		{unknown, http.StatusGatewayTimeout, codeStorageUnknown},
		{gitUnavail, http.StatusServiceUnavailable, codeStorageUnavailable},
		{nsProvisioning, http.StatusServiceUnavailable, codeStorageUnavailable},
		{gitRejected, http.StatusBadGateway, codeStorageRejected},
		{conflict, http.StatusConflict, codeStorageConflict},
		{unavailable, http.StatusInternalServerError, ""},
	} {
		r := routes(c.err)
		for _, call := range calls {
			rec := serve(r, call.method, call.path, call.body)
			assert.Equal(t, c.status, rec.Code, "%s %s with %v: %s", call.method, call.path, c.err, rec.Body.String())
			if c.code != "" {
				var body models.ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, c.code, body.Code)
				assert.NotEmpty(t, body.Error)
			}
		}
	}

	rec := serve(routes(duplicate), http.MethodPost, "/projects", `{"name":"demo"}`)
	assert.Equal(t, http.StatusConflict, rec.Code, "a project that already exists")

	rec = serve(routes(badRequest), http.MethodPost, "/projects", `{"name":"demo"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "bad request maps to 400 on project create")

	rec = serve(routes(badRequest), http.MethodDelete, "/stores/demo/vault", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "bad request maps to 400 on secret store delete")
}
