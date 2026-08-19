package handlers

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// SecretStoreHandler handles secret-store-related requests
type SecretStoreHandler struct {
	service service.SecretStoreService
}

// NewSecretStoreHandler creates a new SecretStoreHandler
func NewSecretStoreHandler(service service.SecretStoreService) *SecretStoreHandler {
	return &SecretStoreHandler{service: service}
}

// Available reports whether the SecretStore CRD backs this API, for the guard
// the router puts in front of the secret-store group.
func (h *SecretStoreHandler) Available(ctx context.Context) bool {
	return h.service.Available(ctx)
}

// ListSecretStores godoc
// @Summary      List secret stores for a project
// @Description  Get all secret stores configured in the project namespace
// @Tags         secret-stores
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Success      200  {array}   models.SecretStoreResponse
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{projectId}/secret-stores [get]
func (h *SecretStoreHandler) ListSecretStores(c *gin.Context) {
	namespace := c.Param("name")

	stores, err := h.service.ListSecretStores(c.Request.Context(), namespace)
	if err != nil {
		logrus.WithError(err).Error("Failed to list secret stores")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, stores)
}

// CreateSecretStore godoc
// @Summary      Create a secret store
// @Description  Create a new secret store in the project namespace (creates K8s Secret + SecretStore CRD)
// @Tags         secret-stores
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        request body models.SecretStoreRequest true "Secret Store Request"
// @Success      201  {object}  models.SecretStoreResponse
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{projectId}/secret-stores [post]
func (h *SecretStoreHandler) CreateSecretStore(c *gin.Context) {
	namespace := c.Param("name")

	var req models.SecretStoreRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	store, err := h.service.CreateSecretStore(c.Request.Context(), namespace, req)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			c.JSON(http.StatusConflict, gin.H{
				"error": fmt.Sprintf("Secret store '%s' already exists in project '%s'", req.Name, namespace),
			})
			return
		}
		logrus.WithError(err).Error("Failed to create secret store")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, store)
}

// UpdateSecretStore godoc
// @Summary      Update a secret store
// @Description  Update an existing secret store (token preserved if not re-sent)
// @Tags         secret-stores
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        storeName path string true "Secret Store name"
// @Param        request body models.SecretStoreRequest true "Secret Store Request"
// @Success      200  {object}  models.SecretStoreResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{projectId}/secret-stores/{storeName} [put]
func (h *SecretStoreHandler) UpdateSecretStore(c *gin.Context) {
	namespace := c.Param("name")
	storeName := c.Param("storeName")

	var req models.SecretStoreRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	store, err := h.service.UpdateSecretStore(c.Request.Context(), namespace, storeName, req)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("Secret store '%s' not found in project '%s'", storeName, namespace),
			})
			return
		}
		logrus.WithError(err).Error("Failed to update secret store")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, store)
}

// DeleteSecretStore godoc
// @Summary      Delete a secret store
// @Description  Delete a secret store and its associated credentials secret
// @Tags         secret-stores
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        storeName path string true "Secret Store name"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{projectId}/secret-stores/{storeName} [delete]
func (h *SecretStoreHandler) DeleteSecretStore(c *gin.Context) {
	namespace := c.Param("name")
	storeName := c.Param("storeName")

	if err := h.service.DeleteSecretStore(c.Request.Context(), namespace, storeName); err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("Secret store '%s' not found in project '%s'", storeName, namespace),
			})
			return
		}
		logrus.WithError(err).Error("Failed to delete secret store")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// TestConnection godoc
// @Summary      Test secret store connection
// @Description  Test connectivity to a Vault server without persisting anything
// @Tags         secret-stores
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        request body models.SecretStoreRequest true "Secret Store Request"
// @Success      200  {object}  models.TestConnectionResponse
// @Failure      400  {object}  map[string]string
// @Router       /api/projects/{projectId}/secret-stores/test [post]
func (h *SecretStoreHandler) TestConnection(c *gin.Context) {
	var req models.SecretStoreRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.service.TestConnection(c.Request.Context(), req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, models.TestConnectionResponse{Message: "Connection successful"})
}

// GetStatus godoc
// @Summary      Get secret store status
// @Description  Get the detailed status including ESO conditions
// @Tags         secret-stores
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Param        storeName path string true "Secret Store name"
// @Success      200  {object}  models.SecretStoreStatusResponse
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{projectId}/secret-stores/{storeName}/status [get]
func (h *SecretStoreHandler) GetStatus(c *gin.Context) {
	namespace := c.Param("name")
	storeName := c.Param("storeName")

	status, err := h.service.GetSecretStoreStatus(c.Request.Context(), namespace, storeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("Secret store '%s' not found in project '%s'", storeName, namespace),
			})
			return
		}
		logrus.WithError(err).Error("Failed to get secret store status")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}
