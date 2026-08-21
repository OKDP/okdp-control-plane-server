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

// RequireIdentityAPI gates the identity endpoints on two conditions: the
// configured provider must be kubauth, and its CRDs must be installed. Both
// answer 501 with the FeatureUnavailable contract, with a distinct message. The
// provider is resolved per request, so switching it needs no restart.
func (h *CapabilitiesHandler) RequireIdentityAPI(crdsInstalled func(c *gin.Context) bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		enabled, err := h.service.IdentityAPIEnabled(c.Request.Context())
		if err != nil {
			logrus.WithError(err).Error("Failed to resolve identity.provider")
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		message := ""
		switch {
		case !enabled:
			message = "Identity management is not available on this platform: identity.provider is not kubauth (see /api/capabilities)."
		case !crdsInstalled(c):
			message = "Identity management is unavailable: identity.provider is kubauth but its CRDs are not installed on this cluster."
		}
		if message != "" {
			c.AbortWithStatusJSON(http.StatusNotImplemented, FeatureUnavailable{
				Error:   message,
				Reason:  ReasonFeatureNotInstalled,
				Feature: identityFeature,
			})
			return
		}
		c.Next()
	}
}
