package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-server-new/internal/service"
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
// @Description  Capabilities the platform is configured with (identity provider, user management, OIDC client provisioning), so the UI can adapt
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

// RequireIdentityAPI gates the kubauth-specific identity endpoints: they are
// only part of the API surface when identity.provider is kubauth in the
// Context. Resolved per request, so switching providers needs no restart.
func (h *CapabilitiesHandler) RequireIdentityAPI() gin.HandlerFunc {
	return func(c *gin.Context) {
		enabled, err := h.service.IdentityAPIEnabled(c.Request.Context())
		if err != nil {
			logrus.WithError(err).Error("Failed to resolve identity.provider")
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !enabled {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "identity API is disabled: identity.provider is not kubauth (see /api/capabilities)",
			})
			return
		}
		c.Next()
	}
}
