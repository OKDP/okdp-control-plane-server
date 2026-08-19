package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type SparkHandler struct {
	service service.SparkService
}

func NewSparkHandler(svc service.SparkService) *SparkHandler {
	return &SparkHandler{service: svc}
}

// SubmitSparkApp godoc
// @Summary      Submit a Spark application
// @Description  Create and submit a SparkApplication to the Spark operator
// @Tags         spark-apps
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        request body models.SparkAppRequest true "Spark Application Request"
// @Success      201  {object}  models.SparkAppInstance
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/spark-apps [post]
func (h *SparkHandler) SubmitSparkApp(c *gin.Context) {
	project := c.Param("name")

	var req models.SparkAppRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	instance, err := h.service.SubmitApp(c.Request.Context(), project, req)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			c.JSON(http.StatusConflict, gin.H{
				"error": fmt.Sprintf("SparkApplication '%s' already exists in project '%s'", req.Name, project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to submit Spark application")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, instance)
}

// SubmitSparkAppYAML godoc
// @Summary      Submit a Spark application from YAML
// @Description  Create and submit a SparkApplication from raw YAML manifest
// @Tags         spark-apps
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        request body models.SparkAppYAMLRequest true "Spark Application YAML"
// @Success      201  {object}  models.SparkAppInstance
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/spark-apps/yaml [post]
func (h *SparkHandler) SubmitSparkAppYAML(c *gin.Context) {
	project := c.Param("name")

	var req models.SparkAppYAMLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	instance, err := h.service.SubmitAppYAML(c.Request.Context(), project, req)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			c.JSON(http.StatusConflict, gin.H{
				"error": fmt.Sprintf("SparkApplication already exists in project '%s'", project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to submit Spark application from YAML")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, instance)
}

// ListSparkApps godoc
// @Summary      List Spark applications for a project
// @Description  Get all SparkApplication instances for a project
// @Tags         spark-apps
// @Produce      json
// @Param        name path string true "Project name"
// @Success      200  {array}   models.SparkAppInstance
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/spark-apps [get]
func (h *SparkHandler) ListSparkApps(c *gin.Context) {
	project := c.Param("name")

	apps, err := h.service.ListApps(c.Request.Context(), project)
	if err != nil {
		logrus.WithError(err).Error("Failed to list Spark applications")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, apps)
}

// StreamSparkApps godoc
// @Summary      Stream Spark application updates
// @Description  Stream SparkApplication status updates using Server-Sent Events (SSE)
// @Tags         spark-apps
// @Produce      text/event-stream
// @Param        name path string true "Project name"
// @Success      200  {string}  string  "stream"
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/spark-apps/stream [get]
func (h *SparkHandler) StreamSparkApps(c *gin.Context) {
	project := c.Param("name")

	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	watcher, err := h.service.WatchApps(c.Request.Context(), project)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer watcher.Stop()

	c.Writer.Flush()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}

			var instance models.SparkAppInstance
			if u, ok := event.Object.(*unstructured.Unstructured); ok {
				instance = models.FromUnstructuredToSparkAppInstance(u)
			} else {
				continue
			}

			c.SSEvent("message", gin.H{"type": event.Type, "object": instance})
			c.Writer.Flush()
		}
	}
}

// GetSparkApp godoc
// @Summary      Get a Spark application
// @Description  Get a specific SparkApplication instance
// @Tags         spark-apps
// @Produce      json
// @Param        name path string true "Project name"
// @Param        appName path string true "Spark application name"
// @Success      200  {object}  models.SparkAppInstance
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/spark-apps/{appName} [get]
func (h *SparkHandler) GetSparkApp(c *gin.Context) {
	project := c.Param("name")
	appName := c.Param("appName")

	instance, err := h.service.GetApp(c.Request.Context(), project, appName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("SparkApplication '%s' not found in project '%s'", appName, project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to get Spark application")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, instance)
}

// DeleteSparkApp godoc
// @Summary      Delete a Spark application
// @Description  Delete a SparkApplication from a project
// @Tags         spark-apps
// @Produce      json
// @Param        name path string true "Project name"
// @Param        appName path string true "Spark application name"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/spark-apps/{appName} [delete]
func (h *SparkHandler) DeleteSparkApp(c *gin.Context) {
	project := c.Param("name")
	appName := c.Param("appName")

	if err := h.service.DeleteApp(c.Request.Context(), project, appName); err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("SparkApplication '%s' not found in project '%s'", appName, project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to delete Spark application")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// UpdateSparkApp godoc
// @Summary      Update a Spark application
// @Description  Update an existing SparkApplication spec fields
// @Tags         spark-apps
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        appName path string true "Spark application name"
// @Param        request body models.SparkAppUpdateRequest true "Spark Application Update"
// @Success      200  {object}  models.SparkAppInstance
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/spark-apps/{appName} [put]
func (h *SparkHandler) UpdateSparkApp(c *gin.Context) {
	project := c.Param("name")
	appName := c.Param("appName")

	var req models.SparkAppUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	instance, err := h.service.UpdateApp(c.Request.Context(), project, appName, req)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("SparkApplication '%s' not found in project '%s'", appName, project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to update Spark application")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, instance)
}

// GetSparkUI godoc
// @Summary      Get Spark application driver UI info
// @Description  Returns the Spark driver Web UI service info for a running SparkApplication
// @Tags         spark-apps
// @Produce      json
// @Param        name path string true "Project name"
// @Param        appName path string true "Spark application name"
// @Success      200  {object}  models.SparkUIInfo
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/spark-apps/{appName}/ui [get]
func (h *SparkHandler) GetSparkUI(c *gin.Context) {
	project := c.Param("name")
	appName := c.Param("appName")

	info, err := h.service.GetSparkUI(c.Request.Context(), project, appName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("SparkApplication '%s' not found in project '%s'", appName, project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to get Spark UI info")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, info)
}

// GetSparkAppLogs godoc
// @Summary      Get Spark application driver logs
// @Description  Retrieve logs for a Spark application's driver pod. Use follow=true for streaming via SSE.
// @Tags         spark-apps
// @Produce      text/plain
// @Param        name path string true "Project name"
// @Param        appName path string true "Spark application name"
// @Param        container query string false "Container name (defaults to spark-kubernetes-driver)"
// @Param        tailLines query int false "Number of lines from the end (default 100)"
// @Param        follow query bool false "Stream logs in real-time (default false)"
// @Success      200  {string}  string  "log output"
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name}/spark-apps/{appName}/logs [get]
func (h *SparkHandler) GetSparkAppLogs(c *gin.Context) {
	project := c.Param("name")
	appName := c.Param("appName")
	container := c.Query("container")
	follow := c.Query("follow") == "true"

	tailLines := int64(100)
	if tl := c.Query("tailLines"); tl != "" {
		if v, err := strconv.ParseInt(tl, 10, 64); err == nil && v > 0 {
			tailLines = v
		}
	}

	stream, err := h.service.GetDriverLogs(c.Request.Context(), project, appName, container, tailLines, follow)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("SparkApplication '%s' not found in project '%s'", appName, project),
			})
			return
		}
		logrus.WithError(err).Error("Failed to get Spark application logs")
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

		scanner := newLogStreamScanner(stream)
		for scanner.Scan() {
			select {
			case <-c.Request.Context().Done():
				return
			default:
				c.SSEvent("message", scanner.Text())
				w.Flush()
			}
		}
		if err := scanner.Err(); err != nil {
			logrus.WithError(err).Warn("Log stream ended on a scanner error")
			c.SSEvent("error", logStreamErrorMessage(err))
			w.Flush()
		}
	} else {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.DataFromReader(http.StatusOK, -1, "text/plain; charset=utf-8", stream, nil)
	}
}

// GetSparkConfig godoc
// @Summary      Get Spark operator configuration
// @Description  Returns Spark operator configuration from the KuboCD Context (images, defaults, versions)
// @Tags         spark-apps
// @Produce      json
// @Success      200  {object}  models.SparkConfig
// @Failure      500  {object}  map[string]string
// @Router       /api/spark-config [get]
func (h *SparkHandler) GetSparkConfig(c *gin.Context) {
	cfg, err := h.service.GetSparkConfig(c.Request.Context())
	if err != nil {
		logrus.WithError(err).Error("Failed to get Spark configuration")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// GetSparkAppSchema godoc
// @Summary      Get SparkApplication CRD schema
// @Description  Returns the OpenAPI spec schema from the SparkApplication CRD installed by the Spark Operator
// @Tags         spark-apps
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/spark-app-schema [get]
func (h *SparkHandler) GetSparkAppSchema(c *gin.Context) {
	schema, err := h.service.GetAppSchema(c.Request.Context())
	if err != nil {
		logrus.WithError(err).Warn("Failed to get SparkApplication CRD schema")
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, schema)
}
