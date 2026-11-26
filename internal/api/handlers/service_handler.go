package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ServiceHandler handles platform service and catalog requests
type ServiceHandler struct {
	service service.ServiceService
	catalog engine.Catalog
}

// NewServiceHandler creates a new ServiceHandler
func NewServiceHandler(svc service.ServiceService, catalog engine.Catalog) *ServiceHandler {
	return &ServiceHandler{service: svc, catalog: catalog}
}

// --- Platform services (managed by OKDP) ---

// GetPlatformServices godoc
// @Summary      List available platform services
// @Description  Get the list of managed OKDP services that can be deployed per project
// @Tags         platform-services
// @Produce      json
// @Success      200  {array}   models.PlatformService
// @Failure      500  {object}  map[string]string
// @Failure      503  {object}  models.ErrorResponse
// @Router       /api/platform-services [get]
func (h *ServiceHandler) GetPlatformServices(c *gin.Context) {
	services, err := h.service.GetPlatformServices(c.Request.Context())
	if err != nil {
		if respondStorageError(c, err) {
			return
		}
		logrus.WithError(err).Error("Failed to get platform services")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, services)
}

// GetMenuCategories godoc
// @Summary      List console navigation categories
// @Description  Get the ordered menu sections (key, label, icon, order) the console groups services under
// @Tags         platform-services
// @Produce      json
// @Success      200  {array}   models.MenuCategory
// @Failure      500  {object}  map[string]string
// @Failure      503  {object}  models.ErrorResponse
// @Router       /api/platform-categories [get]
func (h *ServiceHandler) GetMenuCategories(c *gin.Context) {
	categories, err := h.service.GetMenuCategories(c.Request.Context())
	if err != nil {
		if respondStorageError(c, err) {
			return
		}
		logrus.WithError(err).Error("Failed to get menu categories")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, categories)
}

// CreatePlatformService godoc
// @Summary      Add a service to the catalog
// @Description  Expose a new managed service in the catalog (writes the Control Plane Context)
// @Tags         platform-services
// @Accept       json
// @Produce      json
// @Param        request body models.PlatformService true "Catalog service"
// @Success      201  {object}  models.PlatformService
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  models.ErrorResponse
// @Failure      500  {object}  map[string]string
// @Failure      503  {object}  models.ErrorResponse
// @Failure      504  {object}  models.ErrorResponse
// @Router       /api/platform-services [post]
func (h *ServiceHandler) CreatePlatformService(c *gin.Context) {
	var req models.PlatformService
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	svc, err := h.service.AddPlatformService(c.Request.Context(), req)
	if err != nil {
		h.writeCatalogError(c, err)
		return
	}
	c.JSON(http.StatusCreated, svc)
}

// UpdatePlatformService godoc
// @Summary      Update a catalog service
// @Description  Update an exposed service in the Control Plane Context (versions, default, description, icon, category)
// @Tags         platform-services
// @Accept       json
// @Produce      json
// @Param        serviceName path string true "Service name"
// @Param        request body models.PlatformService true "Catalog service"
// @Success      200  {object}  models.PlatformService
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  models.ErrorResponse
// @Failure      500  {object}  map[string]string
// @Failure      503  {object}  models.ErrorResponse
// @Failure      504  {object}  models.ErrorResponse
// @Router       /api/platform-services/{serviceName} [put]
func (h *ServiceHandler) UpdatePlatformService(c *gin.Context) {
	serviceName := c.Param("serviceName")
	var req models.PlatformService
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	svc, err := h.service.UpdatePlatformService(c.Request.Context(), serviceName, req)
	if err != nil {
		h.writeCatalogError(c, err)
		return
	}
	c.JSON(http.StatusOK, svc)
}

// DeletePlatformService godoc
// @Summary      Remove a catalog service
// @Description  Remove an exposed service from the catalog (writes the Control Plane Context)
// @Tags         platform-services
// @Produce      json
// @Param        serviceName path string true "Service name"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  models.ErrorResponse
// @Failure      500  {object}  map[string]string
// @Failure      503  {object}  models.ErrorResponse
// @Failure      504  {object}  models.ErrorResponse
// @Router       /api/platform-services/{serviceName} [delete]
func (h *ServiceHandler) DeletePlatformService(c *gin.Context) {
	serviceName := c.Param("serviceName")
	if err := h.service.RemovePlatformService(c.Request.Context(), serviceName); err != nil {
		h.writeCatalogError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// writeCatalogError maps catalog management errors to HTTP status codes.
func (h *ServiceHandler) writeCatalogError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrCatalogValidation):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrCatalogServiceExists), engine.IsRejected(err, engine.CodeConflict):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrCatalogServiceNotFound), errors.Is(err, engine.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	default:
		if respondStorageError(c, err) {
			return
		}
		logrus.WithError(err).Error("Catalog operation failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// ListServices godoc
// @Summary      List deployed services for a project
// @Description  Get all deployed service instances for a project
// @Tags         services
// @Produce      json
// @Param        name path string true "Project name"
// @Success      200  {array}   models.ServiceInstance
// @Failure      500  {object}  map[string]string
// @Failure      503  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/services [get]
func (h *ServiceHandler) ListServices(c *gin.Context) {
	project := c.Param("name")

	services, err := h.service.ListServices(c.Request.Context(), project)
	if err != nil {
		if respondStorageError(c, err) {
			return
		}
		logrus.WithError(err).Error("Failed to list services")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, services)
}

// DeployService godoc
// @Summary      Deploy a platform service
// @Description  Deploy a managed platform service into a project. The answer carries the engine's operation.
// @Tags         services
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        request body models.ServiceRequest true "Service Deploy Request"
// @Success      201  {object}  models.ServiceInstance
// @Failure      400  {object}  models.ErrorResponse
// @Failure      409  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Failure      503  {object}  models.ErrorResponse
// @Failure      504  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/services [post]
func (h *ServiceHandler) DeployService(c *gin.Context) {
	project := c.Param("name")

	var req models.ServiceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error(), Code: codeInvalidRequest})
		return
	}

	instance, err := h.service.DeployService(c.Request.Context(), project, req)
	if err != nil {
		instanceName := req.InstanceName
		if instanceName == "" {
			instanceName = req.Service
		}
		writeInstanceError(c, err, project, instanceName, false, "Failed to deploy service")
		return
	}
	c.JSON(http.StatusCreated, instance)
}

// Codes of the error bodies the services routes answer.
const (
	codeInvalidRequest      = "invalid-request"
	codeInvalidPrecondition = "invalid-precondition"
	codeNotFound            = "not-found"
	codeOutcomeUnknown      = "outcome-unknown"
)

// writeInstanceError answers a failed instance request: 404 for a missing
// instance on the routes that address one (notFound true), 409 for a
// duplicate or a stale If-Match, 504 for a write whose outcome is unknown,
// 500 with the error text otherwise. The code
// and the operation classify the failure.
func writeInstanceError(c *gin.Context, err error, project, instance string, notFound bool, logMessage string) {
	var (
		rejected  *engine.RejectedError
		unknown   *engine.UnknownError
		invalid   *service.ParameterError
		operation *models.Operation
		code      string
		status    = http.StatusInternalServerError
		message   = err.Error()
	)
	switch {
	case notFound && errors.Is(err, engine.ErrNotFound):
		c.JSON(http.StatusNotFound, models.ErrorResponse{
			Error: fmt.Sprintf("Service '%s' not found in project '%s'", instance, project),
			Code:  codeNotFound,
		})
		return
	case errors.As(err, &rejected):
		operation = service.OperationOf(rejected.Operation)
		code = engine.CodeRejected
		if len(rejected.Operation.Problems) > 0 {
			code = rejected.Operation.Problems[0].Code
		}
		switch code {
		case engine.CodeInstanceExists:
			status = http.StatusConflict
			message = fmt.Sprintf("Instance '%s' already exists in project '%s'", instance, project)
		case engine.CodeConflict:
			status = http.StatusConflict
		}
	case errors.As(err, &unknown):
		operation = service.OperationOf(unknown.Operation)
		code = codeOutcomeUnknown
		status = http.StatusGatewayTimeout
	case errors.Is(err, gitstore.ErrUncertain):
		code = codeOutcomeUnknown
		status = http.StatusGatewayTimeout
	case errors.Is(err, engine.ErrNamespaceProvisioning), errors.Is(err, gitstore.ErrUnavailable), errors.Is(err, gitstore.ErrBusy):
		code = codeStorageUnavailable
		status = http.StatusServiceUnavailable
	case errors.Is(err, gitstore.ErrRejected):
		code = codeStorageRejected
		status = http.StatusBadGateway
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		status = http.StatusConflict
	case errors.As(err, &invalid):
		if rejection, ok := invalid.Rejection(); ok {
			code = engine.CodeInvalidParameters
			operation = service.OperationOf(rejection)
		}
	}
	if status == http.StatusInternalServerError {
		logrus.WithError(err).Error(logMessage)
	}
	c.JSON(status, models.ErrorResponse{Error: message, Code: code, Operation: operation})
}

// expectedRevision reads the If-Match header: one strong revision, quoted or
// not, or * for any. It answers 400 itself and returns false on anything else.
func expectedRevision(c *gin.Context) (string, bool) {
	values := c.Request.Header.Values("If-Match")
	if len(values) == 0 {
		return "", true
	}
	value := strings.TrimSpace(values[0])
	switch {
	case len(values) > 1, strings.Contains(value, ","):
		return badPrecondition(c, "If-Match takes a single revision")
	case strings.HasPrefix(value, "W/"):
		return badPrecondition(c, "If-Match takes a strong revision, not a weak one")
	case value == "*":
		return "", true
	}
	revision := value
	if strings.HasPrefix(value, `"`) || strings.HasSuffix(value, `"`) {
		if len(value) < 2 || !strings.HasPrefix(value, `"`) || !strings.HasSuffix(value, `"`) {
			return badPrecondition(c, "If-Match has unbalanced quotes")
		}
		revision = value[1 : len(value)-1]
	}
	if revision == "" {
		return badPrecondition(c, "If-Match is empty")
	}
	if strings.Contains(revision, `"`) {
		return badPrecondition(c, "If-Match has unbalanced quotes")
	}
	return revision, true
}

func badPrecondition(c *gin.Context, message string) (string, bool) {
	c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: message, Code: codeInvalidPrecondition})
	return "", false
}

// GetService godoc
// @Summary      Get a deployed service
// @Description  Get a specific deployed service instance
// @Tags         services
// @Produce      json
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service name"
// @Success      200  {object}  models.ServiceInstance
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Failure      503  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/services/{serviceName} [get]
func (h *ServiceHandler) GetService(c *gin.Context) {
	project := c.Param("name")
	serviceName := c.Param("serviceName")

	instance, err := h.service.GetService(c.Request.Context(), project, serviceName)
	if err != nil {
		writeInstanceError(c, err, project, serviceName, true, "Failed to get service")
		return
	}
	c.JSON(http.StatusOK, instance)
}

// DeleteService godoc
// @Summary      Delete a deployed service
// @Description  Remove a deployed service from a project. With If-Match, the deletion is refused with 409 unless the instance is still at that desired revision. 204 when the deletion is done, 202 with the operation when it is accepted and applied later.
// @Tags         services
// @Produce      json
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service name"
// @Param        If-Match header string false "Desired revision the deletion expects"
// @Success      202  {object}  models.OperationResponse
// @Success      204
// @Failure      400  {object}  models.ErrorResponse
// @Failure      404  {object}  models.ErrorResponse
// @Failure      409  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Failure      503  {object}  models.ErrorResponse
// @Failure      504  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/services/{serviceName} [delete]
func (h *ServiceHandler) DeleteService(c *gin.Context) {
	project := c.Param("name")
	serviceName := c.Param("serviceName")
	revision, ok := expectedRevision(c)
	if !ok {
		return
	}

	op, err := h.service.DeleteService(c.Request.Context(), project, serviceName, revision)
	if err != nil {
		writeInstanceError(c, err, project, serviceName, true, "Failed to delete service")
		return
	}
	if op != nil {
		c.JSON(http.StatusAccepted, models.OperationResponse{Operation: op})
		return
	}
	c.Status(http.StatusNoContent)
}

// StreamServices godoc
// @Summary      Stream service updates
// @Description  Stream service status updates using Server-Sent Events (SSE)
// @Tags         services
// @Produce      text/event-stream
// @Param        name path string true "Project name"
// @Success      200  {string}  string  "stream"
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/services/stream [get]
func (h *ServiceHandler) StreamServices(c *gin.Context) {
	project := c.Param("name")

	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	events, err := h.service.WatchServices(c.Request.Context(), project)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	c.Writer.Flush()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-keepalive.C:
			// A comment line keeps proxies from cutting an idle stream.
			if _, err := c.Writer.WriteString(": keepalive\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case event, ok := <-events:
			if !ok {
				return
			}
			c.SSEvent("message", gin.H{"type": event.Type, "object": event.Instance})
			c.Writer.Flush()
		}
	}
}

// --- Package Schema ---

// GetServiceVersions godoc
// @Summary      List available versions for a platform service
// @Description  Returns the list of versions declared in the Control Plane Context CR
// @Tags         platform-services
// @Produce      json
// @Param        serviceName path string true "Service name"
// @Success      200  {object}  models.ServiceVersions
// @Failure      500  {object}  map[string]string
// @Failure      503  {object}  models.ErrorResponse
// @Router       /api/platform-services/{serviceName}/versions [get]
func (h *ServiceHandler) GetServiceVersions(c *gin.Context) {
	serviceName := c.Param("serviceName")

	resp, err := h.catalog.Versions(c.Request.Context(), serviceName)
	if err != nil {
		if respondStorageError(c, err) {
			return
		}
		logrus.WithError(err).WithField("service", serviceName).Error("Failed to get service versions")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// GetServiceInputs godoc
// @Summary      Connections a service's package declares it needs
// @Tags         platform-services
// @Produce      json
// @Param        serviceName path string true "Service name"
// @Param        tag query string false "Package version tag (defaults to Context CR tag)"
// @Success      200  {array}  models.PackageInput
// @Failure      500  {object} map[string]string
// @Failure      503  {object} models.ErrorResponse
// @Router       /api/platform-services/{serviceName}/inputs [get]
func (h *ServiceHandler) GetServiceInputs(c *gin.Context) {
	serviceName := c.Param("serviceName")
	tag := c.Query("tag")

	inputs, err := h.catalog.Inputs(c.Request.Context(), serviceName, tag)
	if err != nil {
		if respondStorageError(c, err) {
			return
		}
		logrus.WithError(err).WithField("service", serviceName).Error("Failed to get service inputs")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// A package with no inputs is the normal case today. Answer an empty list
	// rather than null, so the console can iterate without a guard.
	if inputs == nil {
		inputs = []models.PackageInput{}
	}
	c.JSON(http.StatusOK, inputs)
}

// GetServiceSchema godoc
// @Summary      Get JSON Schema for a platform service
// @Description  Returns the JSON Schema (parameters with UI metadata) for a given service version
// @Tags         platform-services
// @Produce      json
// @Param        serviceName path string true "Service name"
// @Param        tag query string false "Package version tag (defaults to Context CR tag)"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Failure      503  {object}  models.ErrorResponse
// @Router       /api/platform-services/{serviceName}/schema [get]
func (h *ServiceHandler) GetServiceSchema(c *gin.Context) {
	serviceName := c.Param("serviceName")
	tag := c.Query("tag")

	schema, err := h.catalog.ParameterSchema(c.Request.Context(), serviceName, tag)
	if err != nil {
		if respondStorageError(c, err) {
			return
		}
		logrus.WithError(err).WithField("service", serviceName).Error("Failed to get service schema")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, schema)
}

// --- Catalog (client self-service) ---

// GetProfileImages godoc
// @Summary      Get available profile images
// @Description  Returns the list of available container images per profile type from the KuboCD Context
// @Tags         platform-services
// @Produce      json
// @Success      200  {object}  map[string][]models.ProfileImage
// @Failure      500  {object}  map[string]string
// @Router       /api/profile-images [get]
func (h *ServiceHandler) GetProfileImages(c *gin.Context) {
	images, err := h.service.GetProfileImages(c.Request.Context())
	if err != nil {
		logrus.WithError(err).Error("Failed to get profile images")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, images)
}

// UpdateServiceParameters godoc
// @Summary      Update service parameters and/or version
// @Description  Merge new parameters and optionally update the package version of a deployed service. With If-Match, the update is refused with 409 unless the instance is still at that desired revision. The answer carries the engine's operation.
// @Tags         services
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service instance name"
// @Param        If-Match header string false "Desired revision the update expects"
// @Param        request body models.ServiceUpdateRequest true "Update request (tag and/or parameters)"
// @Success      200  {object}  models.ServiceInstance
// @Failure      400  {object}  models.ErrorResponse
// @Failure      404  {object}  models.ErrorResponse
// @Failure      409  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Failure      503  {object}  models.ErrorResponse
// @Failure      504  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/services/{serviceName}/parameters [patch]
func (h *ServiceHandler) UpdateServiceParameters(c *gin.Context) {
	project := c.Param("name")
	serviceName := c.Param("serviceName")

	var req models.ServiceUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error(), Code: codeInvalidRequest})
		return
	}
	revision, ok := expectedRevision(c)
	if !ok {
		return
	}

	instance, err := h.service.UpdateServiceParameters(c.Request.Context(), project, serviceName, req, revision)
	if err != nil {
		writeInstanceError(c, err, project, serviceName, true, "Failed to update service")
		return
	}
	c.JSON(http.StatusOK, instance)
}

// --- Pod operations ---

// ListPods godoc
// @Summary      List pods for a service instance
// @Description  Get all pods belonging to a deployed service instance
// @Tags         services
// @Produce      json
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service instance name"
// @Success      200  {array}   models.Pod
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/services/{serviceName}/pods [get]
func (h *ServiceHandler) ListPods(c *gin.Context) {
	project := c.Param("name")
	serviceName := c.Param("serviceName")

	pods, err := h.service.ListPods(c.Request.Context(), project, serviceName)
	if err != nil {
		logrus.WithError(err).Error("Failed to list pods")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, pods)
}

// GetProjectMetrics godoc
// @Summary      Get live resource usage for every instance of a project
// @Description  One call returns the aggregated CPU/memory usage and limits of each deployed instance, keyed by instance name.
// @Tags         services
// @Produce      json
// @Param        name path string true "Project name"
// @Success      200  {object}  map[string]models.ServiceMetrics
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/metrics [get]
func (h *ServiceHandler) GetProjectMetrics(c *gin.Context) {
	project := c.Param("name")
	metrics, err := h.service.GetProjectMetrics(c.Request.Context(), project)
	if err != nil {
		logrus.WithError(err).Error("Failed to get project metrics")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, metrics)
}

// GetServiceMetrics godoc
// @Summary      Get live resource usage for a service instance
// @Description  Aggregate CPU/memory usage (from metrics-server) and limits for every pod belonging to the instance.
// @Tags         services
// @Produce      json
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service instance name"
// @Success      200  {object}  models.ServiceMetrics
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/services/{serviceName}/metrics [get]
func (h *ServiceHandler) GetServiceMetrics(c *gin.Context) {
	project := c.Param("name")
	serviceName := c.Param("serviceName")

	metrics, err := h.service.GetServiceMetrics(c.Request.Context(), project, serviceName)
	if err != nil {
		logrus.WithError(err).Error("Failed to get service metrics")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, metrics)
}

// GetPodLogs godoc
// @Summary      Get pod logs
// @Description  Retrieve logs for a specific pod. Use follow=true for streaming via SSE.
// @Tags         services
// @Produce      text/plain
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service instance name"
// @Param        podName path string true "Pod name"
// @Param        container query string false "Container name (defaults to first app container)"
// @Param        tailLines query int false "Number of lines from the end (default 100)"
// @Param        follow query bool false "Stream logs in real-time (default false)"
// @Success      200  {string}  string  "log output"
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/services/{serviceName}/pods/{podName}/logs [get]
func (h *ServiceHandler) GetPodLogs(c *gin.Context) {
	project := c.Param("name")
	podName := c.Param("podName")
	container := c.Query("container")
	follow := c.Query("follow") == "true"

	tailLines := int64(100)
	if tl := c.Query("tailLines"); tl != "" {
		if v, err := strconv.ParseInt(tl, 10, 64); err == nil && v > 0 {
			tailLines = v
		}
	}

	stream, err := h.service.GetPodLogs(c.Request.Context(), project, podName, container, tailLines, follow)
	if err != nil {
		logrus.WithError(err).Error("Failed to get pod logs")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer stream.Close()

	if follow {
		w := c.Writer
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Flush()

		lines := make(chan string)
		scanErr := make(chan error, 1)
		// Captured here: gin returns the *gin.Context to its pool as soon as
		// the handler returns and overwrites Request, so reading it from the
		// goroutine races and would watch another request's cancellation.
		streamCtx := c.Request.Context()
		go func() {
			scanner := newLogStreamScanner(stream)
			for scanner.Scan() {
				select {
				case lines <- scanner.Text():
				case <-streamCtx.Done():
					close(lines)
					return
				}
			}
			scanErr <- scanner.Err()
			close(lines)
		}()

		keepalive := time.NewTicker(30 * time.Second)
		defer keepalive.Stop()

		for {
			select {
			case <-c.Request.Context().Done():
				return
			case <-keepalive.C:
				// A comment line keeps proxies from cutting an idle stream.
				if _, err := w.WriteString(": keepalive\n\n"); err != nil {
					return
				}
				w.Flush()
			case line, ok := <-lines:
				if !ok {
					select {
					case err := <-scanErr:
						if err != nil {
							logrus.WithError(err).Warn("Log stream ended on a scanner error")
							c.SSEvent("error", logStreamErrorMessage(err))
							w.Flush()
						}
					default:
					}
					return
				}
				c.SSEvent("message", line)
				w.Flush()
			}
		}
	} else {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.DataFromReader(http.StatusOK, -1, "text/plain; charset=utf-8", stream, nil)
	}
}
