package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ExternalSecretHandler handles external-secret-related requests
type ExternalSecretHandler struct {
	service service.ExternalSecretService
}

// NewExternalSecretHandler creates a new ExternalSecretHandler
func NewExternalSecretHandler(service service.ExternalSecretService) *ExternalSecretHandler {
	return &ExternalSecretHandler{service: service}
}

// Available reports whether the ExternalSecret CRD backs this API, for the
// guard the router puts in front of the external-secret group.
func (h *ExternalSecretHandler) Available(ctx context.Context) bool {
	return h.service.Available(ctx)
}

// isRejectedInput reports whether the failure is the caller's to fix, so the
// API answers 400 rather than blaming the platform. A rejected field looks the
// same whether this service caught it or the API server's own schema did, and
// reported as 500 the console can only say the server broke when the fix is to
// correct a value.
func isRejectedInput(err error) bool {
	return service.IsValidationError(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)
}

// ListExternalSecrets godoc
// @Summary      List external secrets for a project
// @Description  Get all external secrets configured in the project namespace
// @Tags         external-secrets
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Success      200  {array}   models.ExternalSecretResponse
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{projectId}/external-secrets [get]
func (h *ExternalSecretHandler) ListExternalSecrets(c *gin.Context) {
	namespace := c.Param("name")

	items, err := h.service.ListExternalSecrets(c.Request.Context(), namespace)
	if err != nil {
		logrus.WithError(err).Error("Failed to list external secrets")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, items)
}

// CreateExternalSecret godoc
// @Summary      Create an external secret
// @Description  Create a new ExternalSecret in the project namespace. Validates that the referenced SecretStore exists.
// @Tags         external-secrets
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        request body models.ExternalSecretRequest true "External Secret Request"
// @Success      201  {object}  models.ExternalSecretResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{projectId}/external-secrets [post]
func (h *ExternalSecretHandler) CreateExternalSecret(c *gin.Context) {
	namespace := c.Param("name")

	var req models.ExternalSecretRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	es, err := h.service.CreateExternalSecret(c.Request.Context(), namespace, req)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			c.JSON(http.StatusConflict, gin.H{
				"error": fmt.Sprintf("ExternalSecret '%s' already exists in project '%s'", req.Name, namespace),
			})
			return
		}
		// SecretStore not found error from service
		if apierrors.IsNotFound(err) || isSecretStoreNotFoundError(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		if isRejectedInput(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		logrus.WithError(err).Error("Failed to create external secret")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, es)
}

// UpdateExternalSecret godoc
// @Summary      Update an external secret
// @Description  Update an existing ExternalSecret (name comes from URL, ignored in body)
// @Tags         external-secrets
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        esName path string true "External Secret name"
// @Param        request body models.ExternalSecretRequest true "External Secret Request"
// @Success      200  {object}  models.ExternalSecretResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{projectId}/external-secrets/{esName} [put]
func (h *ExternalSecretHandler) UpdateExternalSecret(c *gin.Context) {
	namespace := c.Param("name")
	esName := c.Param("esName")

	var req models.ExternalSecretRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	es, err := h.service.UpdateExternalSecret(c.Request.Context(), namespace, esName, req)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("ExternalSecret '%s' not found in project '%s'", esName, namespace),
			})
			return
		}
		if isSecretStoreNotFoundError(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		if isRejectedInput(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		logrus.WithError(err).Error("Failed to update external secret")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, es)
}

// DeleteExternalSecret godoc
// @Summary      Delete an external secret
// @Description  Delete an ExternalSecret. ESO will automatically remove the target Secret (creationPolicy: Owner).
// @Tags         external-secrets
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        esName path string true "External Secret name"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{projectId}/external-secrets/{esName} [delete]
func (h *ExternalSecretHandler) DeleteExternalSecret(c *gin.Context) {
	namespace := c.Param("name")
	esName := c.Param("esName")

	if err := h.service.DeleteExternalSecret(c.Request.Context(), namespace, esName); err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("ExternalSecret '%s' not found in project '%s'", esName, namespace),
			})
			return
		}
		logrus.WithError(err).Error("Failed to delete external secret")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// GetExternalSecretStatus godoc
// @Summary      Get external secret status
// @Description  Get the detailed status including ESO conditions and last sync time
// @Tags         external-secrets
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        esName path string true "External Secret name"
// @Success      200  {object}  models.ExternalSecretStatusResponse
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{projectId}/external-secrets/{esName}/status [get]
func (h *ExternalSecretHandler) GetExternalSecretStatus(c *gin.Context) {
	namespace := c.Param("name")
	esName := c.Param("esName")

	status, err := h.service.GetExternalSecretStatus(c.Request.Context(), namespace, esName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("ExternalSecret '%s' not found in project '%s'", esName, namespace),
			})
			return
		}
		logrus.WithError(err).Error("Failed to get external secret status")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}

// isSecretStoreNotFoundError checks if the error is a "SecretStore not found" error from the service layer
func isSecretStoreNotFoundError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SecretStore") && strings.Contains(err.Error(), "not found")
}
