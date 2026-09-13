package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
	"github.com/sirupsen/logrus"
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
// @Description  List the projects declared in the deployments repository (projects/<p>/project.yaml), whether written by the console or by hand in Git
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
// @Description  Get a project declared in the deployments repository (projects/<name>/project.yaml)
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
// @Description  Create a project: commits projects/<name>/project.yaml and creates the project Namespace. 409 when the project is already declared, or when a Namespace of that name exists and is not a project's
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        project body models.Project true "Project Object"
// @Success      201  {object}  models.Project
// @Failure      400  {object}  map[string]string "Invalid project name"
// @Failure      409  {object}  map[string]string "Project or Namespace already exists"
// @Failure      500  {object}  map[string]string
// @Router       /api/projects [post]
func (h *ProjectHandler) CreateProject(c *gin.Context) {
	var project models.Project
	if err := c.ShouldBindJSON(&project); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.service.CreateProject(c.Request.Context(), &project); err != nil {
		switch {
		case apierrors.IsBadRequest(err):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		case apierrors.IsAlreadyExists(err):
			c.JSON(http.StatusConflict, gin.H{"error": "Project '" + project.Name + "' already exists, or its namespace exists and is not a project's"})
			return
		}
		logrus.Errorf("Failed to create project: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, project)
}

// UpdateProject godoc
// @Summary      Update a project
// @Description  Update a project's mutable metadata (currently its description) in projects/<name>/project.yaml; the file's other keys are kept
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        name    path string         true "Project Name"
// @Param        project body models.Project true "Project Object"
// @Success      200  {object}  models.Project
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string "Project not found"
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name} [put]
func (h *ProjectHandler) UpdateProject(c *gin.Context) {
	var project models.Project
	if err := c.ShouldBindJSON(&project); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// The path name is authoritative; ignore any name in the body.
	project.Name = c.Param("name")

	updated, err := h.service.UpdateProject(c.Request.Context(), &project)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
			return
		}
		logrus.WithError(err).Error("Failed to update project")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, updated)
}

// DeleteProject godoc
// @Summary      Delete a project
// @Description  Delete a project: removes projects/<name>/ from the deployments repository (the GitOps engine uninstalls its releases), then its Namespace if the console created it
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        name path string true "Project Name"
// @Success      204  {object}  nil
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name} [delete]
func (h *ProjectHandler) DeleteProject(c *gin.Context) {
	name := c.Param("name")
	if err := h.service.DeleteProject(c.Request.Context(), name); err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
			return
		}
		logrus.WithError(err).Error("Failed to delete project")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// StreamProjects godoc
// @Summary      Stream project updates
// @Description  Stream project updates using Server-Sent Events (SSE): an ADDED event per existing project, then ADDED/MODIFIED/DELETED as project.yaml files change in the deployments repository (read every few seconds, at once after a console change)
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

	events, err := h.service.WatchProjects(c.Request.Context())
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
			c.SSEvent("message", event)
			c.Writer.Flush()
		}
	}
}

// Resolve looks a project up by name. It is what middleware.RequireProject
// calls to establish that a path segment really designates a project, before
// any handler passes it to the Kubernetes client as a namespace.
func (h *ProjectHandler) Resolve(c *gin.Context, name string) (*models.Project, error) {
	return h.service.GetProject(c.Request.Context(), name)
}
