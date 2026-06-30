package handlers

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/okdp/okdp-server-new/internal/service"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ServiceHandler handles platform service and catalog requests
type ServiceHandler struct {
	service       service.ServiceService
	schemaService service.PackageSchemaService
}

// NewServiceHandler creates a new ServiceHandler
func NewServiceHandler(svc service.ServiceService, schemaSvc service.PackageSchemaService) *ServiceHandler {
	return &ServiceHandler{service: svc, schemaService: schemaSvc}
}

// --- Platform services (managed by OKDP) ---

// GetPlatformServices godoc
// @Summary      List available platform services
// @Description  Get the list of managed OKDP services that can be deployed per project
// @Tags         platform-services
// @Produce      json
// @Success      200  {array}   models.PlatformService
// @Failure      500  {object}  map[string]string
// @Router       /api/platform-services [get]
func (h *ServiceHandler) GetPlatformServices(c *gin.Context) {
	services, err := h.service.GetPlatformServices(c.Request.Context())
	if err != nil {
		logrus.WithError(err).Error("Failed to get platform services")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, services)
}

// CreatePlatformService godoc
// @Summary      Add a service to the catalog
// @Description  Expose a new managed service in the catalog (writes the default KuboCD Context)
// @Tags         platform-services
// @Accept       json
// @Produce      json
// @Param        request body models.PlatformService true "Catalog service"
// @Success      201  {object}  models.PlatformService
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
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
// @Description  Update an exposed service (versions, default, description, icon, category)
// @Tags         platform-services
// @Accept       json
// @Produce      json
// @Param        serviceName path string true "Service name"
// @Param        request body models.PlatformService true "Catalog service"
// @Success      200  {object}  models.PlatformService
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
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
// @Description  Remove an exposed service from the catalog (writes the default KuboCD Context)
// @Tags         platform-services
// @Produce      json
// @Param        serviceName path string true "Service name"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
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
	case errors.Is(err, service.ErrCatalogServiceExists):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrCatalogServiceNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	default:
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
// @Router       /api/projects/{name}/services [get]
func (h *ServiceHandler) ListServices(c *gin.Context) {
	project := c.Param("name")

	services, err := h.service.ListServices(c.Request.Context(), project)
	if err != nil {
		logrus.WithError(err).Error("Failed to list services")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, services)
}

// DeployService godoc
// @Summary      Deploy a platform service
// @Description  Deploy a managed platform service into a project
// @Tags         services
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        request body models.ServiceRequest true "Service Deploy Request"
// @Success      201  {object}  models.ServiceInstance
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/services [post]
func (h *ServiceHandler) DeployService(c *gin.Context) {
	project := c.Param("name")

	var req models.ServiceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	instance, err := h.service.DeployService(c.Request.Context(), project, req)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			instanceName := req.InstanceName
			if instanceName == "" {
				instanceName = req.Service
			}
			c.JSON(http.StatusConflict, gin.H{
				"error": fmt.Sprintf("Instance '%s' already exists in project '%s'", instanceName, project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to deploy service")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, instance)
}

// GetService godoc
// @Summary      Get a deployed service
// @Description  Get a specific deployed service instance
// @Tags         services
// @Produce      json
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service name"
// @Success      200  {object}  models.ServiceInstance
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/services/{serviceName} [get]
func (h *ServiceHandler) GetService(c *gin.Context) {
	project := c.Param("name")
	serviceName := c.Param("serviceName")

	instance, err := h.service.GetService(c.Request.Context(), project, serviceName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("Service '%s' not found in project '%s'", serviceName, project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to get service")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, instance)
}

// DeleteService godoc
// @Summary      Delete a deployed service
// @Description  Remove a deployed service from a project
// @Tags         services
// @Produce      json
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service name"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/services/{serviceName} [delete]
func (h *ServiceHandler) DeleteService(c *gin.Context) {
	project := c.Param("name")
	serviceName := c.Param("serviceName")

	if err := h.service.DeleteService(c.Request.Context(), project, serviceName); err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("Service '%s' not found in project '%s'", serviceName, project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to delete service")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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

	watcher, err := h.service.WatchServices(c.Request.Context(), project)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer watcher.Stop()

	ingressSuffix, _ := h.service.GetIngressSuffix(c.Request.Context())

	c.Writer.Flush()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}

			var instance models.ServiceInstance
			if u, ok := event.Object.(*unstructured.Unstructured); ok {
				instance = models.FromUnstructuredToServiceInstance(u)
			} else {
				continue
			}

			if ingressSuffix != "" && instance.ReleaseName != "" {
				instance.URL = fmt.Sprintf("https://%s.%s", instance.ReleaseName, ingressSuffix)
			}

			h.service.EnrichPodHealth(c.Request.Context(), &instance)

			c.SSEvent("message", gin.H{"type": event.Type, "object": instance})
			c.Writer.Flush()
		}
	}
}

// --- Package Schema ---

// GetServiceVersions godoc
// @Summary      List available versions for a platform service
// @Description  Returns the list of versions declared in the KuboCD Context CR
// @Tags         platform-services
// @Produce      json
// @Param        serviceName path string true "Service name"
// @Success      200  {object}  service.ServiceVersionsResponse
// @Failure      500  {object}  map[string]string
// @Router       /api/platform-services/{serviceName}/versions [get]
func (h *ServiceHandler) GetServiceVersions(c *gin.Context) {
	serviceName := c.Param("serviceName")

	resp, err := h.schemaService.GetServiceVersions(c.Request.Context(), serviceName)
	if err != nil {
		logrus.WithError(err).WithField("service", serviceName).Error("Failed to get service versions")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
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
// @Router       /api/platform-services/{serviceName}/schema [get]
func (h *ServiceHandler) GetServiceSchema(c *gin.Context) {
	serviceName := c.Param("serviceName")
	tag := c.Query("tag")

	schema, err := h.schemaService.GetParameterSchema(c.Request.Context(), serviceName, tag)
	if err != nil {
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
// @Description  Merge new parameters and optionally update the package version of a deployed service
// @Tags         services
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service instance name"
// @Param        request body models.ServiceUpdateRequest true "Update request (tag and/or parameters)"
// @Success      200  {object}  models.ServiceInstance
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/services/{serviceName}/parameters [patch]
func (h *ServiceHandler) UpdateServiceParameters(c *gin.Context) {
	project := c.Param("name")
	serviceName := c.Param("serviceName")

	var req models.ServiceUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	instance, err := h.service.UpdateServiceParameters(c.Request.Context(), project, serviceName, req)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("Service '%s' not found in project '%s'", serviceName, project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to update service")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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

		scanner := bufio.NewScanner(stream)
		for scanner.Scan() {
			select {
			case <-c.Request.Context().Done():
				return
			default:
				c.SSEvent("message", scanner.Text())
				w.Flush()
			}
		}
	} else {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.DataFromReader(http.StatusOK, -1, "text/plain; charset=utf-8", stream, nil)
	}
}

// GetCatalog godoc
// @Summary      Get self-service catalog
// @Description  Get the list of additional packages that clients can deploy on their own
// @Tags         catalog
// @Produce      json
// @Success      200  {array}   models.CatalogCategory
// @Failure      500  {object}  map[string]string
// @Router       /api/catalog [get]
func (h *ServiceHandler) GetCatalog(c *gin.Context) {
	catalog, err := h.service.GetCatalog(c.Request.Context())
	if err != nil {
		logrus.WithError(err).Error("Failed to get catalog")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, catalog)
}
