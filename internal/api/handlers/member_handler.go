package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// MemberHandler handles the members of a project.
type MemberHandler struct {
	service service.MemberService
}

func NewMemberHandler(service service.MemberService) *MemberHandler {
	return &MemberHandler{service: service}
}

// ListMembers godoc
// @Summary      List the members of a project
// @Description  Users and groups holding a role on the project, and whether the caller may change them
// @Tags         members
// @Produce      json
// @Param        name path string true "Project Name"
// @Success      200  {object}  models.ProjectMembers
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/projects/{name}/members [get]
func (h *MemberHandler) ListMembers(c *gin.Context) {
	members, err := h.service.ListMembers(c.Request.Context(), c.Param("name"))
	if err != nil {
		logrus.WithError(err).Error("Failed to list project members")
		c.JSON(serverErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, members)
}

// SetMember godoc
// @Summary      Grant a role on a project
// @Description  Give a user or a group a role on the project, replacing the role it had
// @Tags         members
// @Accept       json
// @Produce      json
// @Param        name   path string               true "Project Name"
// @Param        member body models.ProjectMember true "Member"
// @Success      200  {object}  models.ProjectMember
// @Failure      400  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Router       /api/projects/{name}/members [put]
func (h *MemberHandler) SetMember(c *gin.Context) {
	var member models.ProjectMember
	if err := c.ShouldBindJSON(&member); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	saved, err := h.service.SetMember(c.Request.Context(), c.Param("name"), member)
	if err != nil {
		if errors.Is(err, service.ErrInvalidMember) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		logrus.WithError(err).Error("Failed to set a project member")
		c.JSON(serverErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, saved)
}

// RemoveMember godoc
// @Summary      Revoke a member
// @Description  Take every role on the project away from a user or a group
// @Tags         members
// @Param        name   path  string true "Project Name"
// @Param        kind   query string true "user or group"
// @Param        member query string true "User or group name"
// @Success      204
// @Failure      400  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/projects/{name}/members [delete]
func (h *MemberHandler) RemoveMember(c *gin.Context) {
	err := h.service.RemoveMember(c.Request.Context(), c.Param("name"), c.Query("kind"), c.Query("member"))
	switch {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, service.ErrInvalidMember):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case apierrors.IsNotFound(err):
		c.JSON(http.StatusNotFound, gin.H{"error": "not a member of this project"})
	default:
		logrus.WithError(err).Error("Failed to remove a project member")
		c.JSON(serverErrorStatus(err), gin.H{"error": err.Error()})
	}
}
