package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/service"
	"github.com/sirupsen/logrus"
)

type CapabilitiesHandler struct {
	service service.CapabilityService
}

func NewCapabilitiesHandler(service service.CapabilityService) *CapabilitiesHandler {
	return &CapabilitiesHandler{service: service}
}

// GetCapabilities godoc
// @Summary      Get platform capabilities
// @Description  Capabilities the platform is configured with (user management through Keycloak, console OIDC client, OIDC client provisioning backend), so the UI can adapt
// @Tags         capabilities
// @Produce      json
// @Success      200  {object}  models.Capabilities
// @Router       /api/capabilities [get]
func (h *CapabilitiesHandler) GetCapabilities(c *gin.Context) {
	caps, err := h.service.GetCapabilities(c.Request.Context())
	if err != nil {
		logrus.WithError(err).Error("Failed to resolve platform capabilities")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, caps)
}
