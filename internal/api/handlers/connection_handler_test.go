package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// selectableSpy embeds the interface so only the probed method needs a body.
type selectableSpy struct {
	service.ConnectionService
	project  string
	contract string
}

func (s *selectableSpy) ListSelectable(_ context.Context, project, contract string) ([]models.SelectableConnection, error) {
	s.project = project
	s.contract = contract
	return []models.SelectableConnection{}, nil
}

// Pins the wire names the console depends on. Every other test calls the service
// directly, so nothing else would notice the query parameter being renamed.
func TestListSelectableReadsTheContractQuery(t *testing.T) {
	spy := &selectableSpy{}
	engine := gin.New()
	engine.GET("/api/projects/:name/connections/selectable", NewConnectionHandler(spy).ListSelectable)

	recorder := call(engine, http.MethodGet, "/api/projects/demo/connections/selectable?contract=database-server")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if spy.project != "demo" {
		t.Errorf("project = %q, want demo", spy.project)
	}
	if spy.contract != "database-server" {
		t.Errorf("contract = %q, want database-server: the console builds ?contract=", spy.contract)
	}
}

// An input with no declared contract must still reach the service, which
// answers with everything selectable rather than nothing.
func TestListSelectableToleratesAMissingContract(t *testing.T) {
	spy := &selectableSpy{contract: "untouched"}
	engine := gin.New()
	engine.GET("/api/projects/:name/connections/selectable", NewConnectionHandler(spy).ListSelectable)

	recorder := call(engine, http.MethodGet, "/api/projects/demo/connections/selectable")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if spy.contract != "" {
		t.Errorf("contract = %q, want empty", spy.contract)
	}
}

type conflictingUpdate struct {
	service.ConnectionService
}

func (conflictingUpdate) Update(context.Context, string, string, models.ConnectionRequest) (*models.ConnectionResponse, error) {
	return nil, apierrors.NewConflict(schema.GroupResource{Group: "kubocd.kubotal.io", Resource: "connections"}, "warehouse", errors.New("changed since revision abc"))
}

func TestAConnectionChangedSinceItWasReadIsAConflict(t *testing.T) {
	engine := gin.New()
	engine.PUT("/api/projects/:name/connections/:connName", NewConnectionHandler(conflictingUpdate{}).UpdateConnection)

	req := httptest.NewRequest(http.MethodPut, "/api/projects/demo/connections/warehouse", strings.NewReader(`{"name":"warehouse","type":"database-server","values":{}}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
}
