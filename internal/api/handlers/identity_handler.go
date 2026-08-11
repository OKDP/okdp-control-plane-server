package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type IdentityHandler struct {
	service service.IdentityService
}

func NewIdentityHandler(service service.IdentityService) *IdentityHandler {
	return &IdentityHandler{service: service}
}

// --- Users ---

// ListUsers godoc
// @Summary      List all users
// @Description  Get all users from Kubauth
// @Tags         identity
// @Accept       json
// @Produce      json
// @Success      200  {array}   models.User
// @Router       /api/v1/identity/users [get]
func (h *IdentityHandler) ListUsers(c *gin.Context) {
	users, err := h.service.ListUsers(c.Request.Context())
	if err != nil {
		logrus.WithError(err).Error("Failed to list users")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, users)
}

// GetUser godoc
// @Summary      Get user by name
// @Description  Get a user details
// @Tags         identity
// @Accept       json
// @Produce      json
// @Param        name   path      string  true  "User Name"
// @Success      200  {object}  models.User
// @Router       /api/v1/identity/users/{name} [get]
func (h *IdentityHandler) GetUser(c *gin.Context) {
	name := c.Param("name")
	user, err := h.service.GetUser(c.Request.Context(), name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}
		logrus.WithError(err).Error("Failed to get user")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, user)
}

// CreateUser godoc
// @Summary      Create a new user
// @Description  Create a new user in Kubauth
// @Tags         identity
// @Accept       json
// @Produce      json
// @Param        user   body      models.User  true  "User"
// @Success      201  {object}  models.User
// @Router       /api/v1/identity/users [post]
func (h *IdentityHandler) CreateUser(c *gin.Context) {
	var user models.User
	if err := c.ShouldBindJSON(&user); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.service.CreateUser(c.Request.Context(), &user); err != nil {
		logrus.WithError(err).Error("Failed to create user")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, user)
}

// UpdateUser godoc
// @Summary      Update a user
// @Description  Update a user in Kubauth
// @Tags         identity
// @Accept       json
// @Produce      json
// @Param        name   path      string  true  "User Name"
// @Param        user   body      models.User  true  "User"
// @Success      200  {object}  models.User
// @Router       /api/v1/identity/users/{name} [put]
func (h *IdentityHandler) UpdateUser(c *gin.Context) {
	name := c.Param("name")
	var user models.User
	if err := c.ShouldBindJSON(&user); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Ensure name consistency
	user.Name = name

	if err := h.service.UpdateUser(c.Request.Context(), name, &user); err != nil {
		logrus.WithError(err).Error("Failed to update user")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, user)
}

// DeleteUser godoc
// @Summary      Delete a user
// @Description  Delete a user from Kubauth
// @Tags         identity
// @Accept       json
// @Produce      json
// @Param        name   path      string  true  "User Name"
// @Success      204
// @Router       /api/v1/identity/users/{name} [delete]
func (h *IdentityHandler) DeleteUser(c *gin.Context) {
	name := c.Param("name")
	if err := h.service.DeleteUser(c.Request.Context(), name); err != nil {
		logrus.WithError(err).Error("Failed to delete user")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// --- Groups ---

// ListGroups godoc
// @Summary      List all groups
// @Description  Get all groups from Kubauth
// @Tags         identity
// @Accept       json
// @Produce      json
// @Success      200  {array}   models.Group
// @Router       /api/v1/identity/groups [get]
func (h *IdentityHandler) ListGroups(c *gin.Context) {
	groups, err := h.service.ListGroups(c.Request.Context())
	if err != nil {
		logrus.WithError(err).Error("Failed to list groups")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, groups)
}

// CreateGroup godoc
// @Summary      Create a new group
// @Description  Create a new group in Kubauth
// @Tags         identity
// @Accept       json
// @Produce      json
// @Param        group   body      models.Group  true  "Group"
// @Success      201  {object}  models.Group
// @Router       /api/v1/identity/groups [post]
func (h *IdentityHandler) CreateGroup(c *gin.Context) {
	var group models.Group
	if err := c.ShouldBindJSON(&group); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.service.CreateGroup(c.Request.Context(), &group); err != nil {
		logrus.WithError(err).Error("Failed to create group")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, group)
}

// UpdateGroup godoc
// @Summary      Update a group
// @Description  Update a group in Kubauth
// @Tags         identity
// @Accept       json
// @Produce      json
// @Param        name   path      string  true  "Group Name"
// @Param        group   body      models.Group  true  "Group"
// @Success      200  {object}  models.Group
// @Router       /api/v1/identity/groups/{name} [put]
func (h *IdentityHandler) UpdateGroup(c *gin.Context) {
	name := c.Param("name")
	var group models.Group
	if err := c.ShouldBindJSON(&group); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.service.UpdateGroup(c.Request.Context(), name, &group); err != nil {
		logrus.WithError(err).Error("Failed to update group")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, group)
}

// DeleteGroup godoc
// @Summary      Delete a group
// @Description  Delete a group from Kubauth
// @Tags         identity
// @Accept       json
// @Produce      json
// @Param        name   path      string  true  "Group Name"
// @Success      204
// @Router       /api/v1/identity/groups/{name} [delete]
func (h *IdentityHandler) DeleteGroup(c *gin.Context) {
	name := c.Param("name")
	if err := h.service.DeleteGroup(c.Request.Context(), name); err != nil {
		logrus.WithError(err).Error("Failed to delete group")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
