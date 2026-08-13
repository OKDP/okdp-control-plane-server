package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

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
