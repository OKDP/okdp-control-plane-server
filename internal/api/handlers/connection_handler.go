package handlers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ConnectionHandler handles connection-related requests, for both the
// project-scoped and the platform-wide (admin) routes.
type ConnectionHandler struct {
	service service.ConnectionService
}

// NewConnectionHandler creates a new ConnectionHandler
func NewConnectionHandler(service service.ConnectionService) *ConnectionHandler {
	return &ConnectionHandler{service: service}
}

// projectNamespace is the namespace addressed by a project-scoped route.
func projectNamespace(c *gin.Context) string {
	return c.Param("name")
}

// GetContracts godoc
// @Summary      List the available contracts
// @Description  Descriptors of every contract, used to build the creation form, plus whether connections can currently be persisted
// @Tags         connections
// @Produce      json
// @Success      200  {object}  models.ConnectionCatalogResponse
// @Router       /api/contracts [get]
func (h *ConnectionHandler) GetContracts(c *gin.Context) {
	c.JSON(http.StatusOK, h.service.Catalog(c.Request.Context()))
}

// ListConnections godoc
// @Summary      List the external connections of a project
// @Description  Connections declared in the project namespace. Connections owned by a deployed release are excluded. They are returned by the internal endpoint.
// @Tags         connections
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Success      200  {array}   models.ConnectionResponse
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/connections [get]
func (h *ConnectionHandler) ListConnections(c *gin.Context) {
	h.list(c, projectNamespace(c))
}

func (h *ConnectionHandler) list(c *gin.Context, namespace string) {
	connections, err := h.service.List(c.Request.Context(), namespace)
	if err != nil {
		logrus.WithError(err).Error("Failed to list connections")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, connections)
}

// ListSelectable godoc
// @Summary      Connections offered for a package input of a given contract
// @Tags         connections
// @Produce      json
// @Param        name path string true "Project name"
// @Param        contract query string false "Contract the connections must satisfy"
// @Success      200 {array} models.SelectableConnection
// @Router       /api/projects/{name}/connections/selectable [get]
func (h *ConnectionHandler) ListSelectable(c *gin.Context) {
	project := c.Param("name")
	contract := c.Query("contract")

	connections, err := h.service.ListSelectable(c.Request.Context(), project, contract)
	if err != nil {
		logrus.WithError(err).WithField("project", project).Error("Failed to list selectable connections")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list selectable connections"})
		return
	}
	c.JSON(http.StatusOK, connections)
}

// ListConsumers godoc
// @Summary      Services of the project bound to a connection
// @Tags         connections
// @Produce      json
// @Param        name path string true "Project name"
// @Param        connName path string true "Connection name"
// @Success      200 {array} models.ConnectionConsumer
// @Router       /api/projects/{name}/connections/{connName}/consumers [get]
func (h *ConnectionHandler) ListConsumers(c *gin.Context) {
	project := c.Param("name")
	name := c.Param("connName")

	consumers, err := h.service.ListConsumers(c.Request.Context(), project, name)
	if err != nil {
		logrus.WithError(err).WithField("connection", name).Error("Failed to list the consumers of a connection")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list the consumers of this connection"})
		return
	}
	c.JSON(http.StatusOK, consumers)
}

// ListInternalConnections godoc
// @Summary      List the internal connections of a project
// @Description  Connections provided by the services already deployed in the project, that the project's other services can consume
// @Tags         connections
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Success      200  {array}   models.InternalConnection
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/connections/internal [get]
func (h *ConnectionHandler) ListInternalConnections(c *gin.Context) {
	connections, err := h.service.ListInternal(c.Request.Context(), projectNamespace(c))
	if err != nil {
		logrus.WithError(err).Error("Failed to list internal connections")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, connections)
}

// CreateConnection godoc
// @Summary      Create an external connection in a project
// @Description  Stores the credential fields in a Kubernetes Secret and creates the Connection CRD referencing it
// @Tags         connections
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        request body models.ConnectionRequest true "Connection"
// @Success      201  {object}  models.ConnectionResponse
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      501  {object}  map[string]string
// @Router       /api/projects/{name}/connections [post]
func (h *ConnectionHandler) CreateConnection(c *gin.Context) {
	h.create(c, projectNamespace(c))
}

func (h *ConnectionHandler) create(c *gin.Context, namespace string) {
	var req models.ConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	connection, err := h.service.Create(c.Request.Context(), namespace, req)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			c.JSON(http.StatusConflict, gin.H{
				"error": fmt.Sprintf("Connection '%s' already exists", req.Name),
			})
			return
		}
		h.writeError(c, err, "Failed to create connection")
		return
	}
	c.JSON(http.StatusCreated, connection)
}

// UpdateConnection godoc
// @Summary      Update an external connection of a project
// @Tags         connections
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        connName path string true "Connection name"
// @Param        request body models.ConnectionRequest true "Connection"
// @Success      200  {object}  models.ConnectionResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      501  {object}  map[string]string
// @Router       /api/projects/{name}/connections/{connName} [put]
func (h *ConnectionHandler) UpdateConnection(c *gin.Context) {
	h.update(c, projectNamespace(c))
}

func (h *ConnectionHandler) update(c *gin.Context, namespace string) {
	var req models.ConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	connection, err := h.service.Update(c.Request.Context(), namespace, c.Param("connName"), req)
	if err != nil {
		h.writeError(c, err, "Failed to update connection")
		return
	}
	c.JSON(http.StatusOK, connection)
}

// DeleteConnection godoc
// @Summary      Delete an external connection of a project
// @Description  Removes the Connection CRD and the Secret holding its credentials
// @Tags         connections
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        connName path string true "Connection name"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Failure      501  {object}  map[string]string
// @Router       /api/projects/{name}/connections/{connName} [delete]
func (h *ConnectionHandler) DeleteConnection(c *gin.Context) {
	h.delete(c, projectNamespace(c))
}

func (h *ConnectionHandler) delete(c *gin.Context, namespace string) {
	if err := h.service.Delete(c.Request.Context(), namespace, c.Param("connName")); err != nil {
		h.writeError(c, err, "Failed to delete connection")
		return
	}
	c.Status(http.StatusNoContent)
}

// TestConnection godoc
// @Summary      Test a connection from the platform
// @Description  Opens a real connection to the endpoint and checks the credentials. Nothing is written to the cluster, so a connection can be validated before being created. Always answers 200: the outcome is in the body.
// @Tags         connections
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        request body models.ConnectionTestRequest true "Connection settings to probe"
// @Success      200  {object}  models.ConnectionTestResult
// @Failure      400  {object}  map[string]string
// @Router       /api/projects/{name}/connections/test [post]
func (h *ConnectionHandler) TestConnection(c *gin.Context) {
	h.test(c)
}

// test answers 200 even when the endpoint refused the connection: a failed
// probe is a result to display, not a failed request.
func (h *ConnectionHandler) test(c *gin.Context) {
	var req models.ConnectionTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, h.service.Test(c.Request.Context(), req))
}

// writeError maps a service error to the status that describes it: a missing
// CRD is a capability the platform does not have yet (501), rejected input is
// the caller's problem (400), and anything else is ours (500).
func (h *ConnectionHandler) writeError(c *gin.Context, err error, logMessage string) {
	switch {
	case errors.Is(err, service.ErrConnectionsUnavailable):
		c.JSON(http.StatusNotImplemented, gin.H{"error": err.Error()})
	case apierrors.IsNotFound(err):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case service.IsValidationError(err):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		logrus.WithError(err).Error(logMessage)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
