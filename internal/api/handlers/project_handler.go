package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/okdp/okdp-server-new/internal/service"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ProjectHandler handles project-related requests
type ProjectHandler struct {
	service service.ProjectService
}

// NewProjectHandler creates a new ProjectHandler
func NewProjectHandler(service service.ProjectService) *ProjectHandler {
	return &ProjectHandler{
		service: service,
	}
}

// ListProjects godoc
// @Summary      List all projects
// @Description  Get a list of all projects (backed by Kubernetes Namespaces)
// @Tags         projects
// @Accept       json
// @Produce      json
// @Success      200  {array}   models.Project
// @Failure      500  {object}  map[string]string
// @Router       /api/projects [get]
func (h *ProjectHandler) ListProjects(c *gin.Context) {
	projects, err := h.service.ListProjects(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, projects)
}

// GetProject godoc
// @Summary      Get a project
// @Description  Get a single project by name
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        name path string true "Project Name"
// @Success      200  {object}  models.Project
// @Failure      404  {object}  map[string]string "Project not found"
// @Failure      500  {object}  map[string]string "Internal server error"
// @Router       /api/projects/{name} [get]
func (h *ProjectHandler) GetProject(c *gin.Context) {
	name := c.Param("name")
	project, err := h.service.GetProject(c.Request.Context(), name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
			return
		}
		logrus.WithError(err).Error("Failed to get project")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, project)
}

// CreateProject godoc
// @Summary      Create a project
// @Description  Create a new project (materialized as a Kubernetes Namespace)
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        project body models.Project true "Project Object"
// @Success      201  {object}  models.Project
// @Failure      400  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects [post]
func (h *ProjectHandler) CreateProject(c *gin.Context) {
	var project models.Project
	if err := c.ShouldBindJSON(&project); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.service.CreateProject(c.Request.Context(), &project); err != nil {
		logrus.Errorf("Failed to create project: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, project)
}

// DeleteProject godoc
// @Summary      Delete a project
// @Description  Delete a project by name
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        name path string true "Project Name"
// @Success      204  {object}  nil
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name} [delete]
func (h *ProjectHandler) DeleteProject(c *gin.Context) {
	name := c.Param("name")
	if err := h.service.DeleteProject(c.Request.Context(), name); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// StreamProjects godoc
// @Summary      Stream project updates
// @Description  Stream project updates using Server-Sent Events (SSE)
// @Tags         projects
// @Produce      text/event-stream
// @Success      200  {string}  string  "stream"
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/stream [get]
func (h *ProjectHandler) StreamProjects(c *gin.Context) {
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	watcher, err := h.service.WatchProjects(c.Request.Context())
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

			ns, ok := event.Object.(*corev1.Namespace)
			if !ok {
				continue
			}

			payload := gin.H{
				"type":   event.Type,
				"object": models.FromNamespaceToProject(ns),
			}

			c.SSEvent("message", payload)
			c.Writer.Flush()
		}
	}
}
