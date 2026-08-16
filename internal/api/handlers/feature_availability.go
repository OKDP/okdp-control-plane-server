package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// FeatureUnavailable is the 501 body returned when a route rests on CRDs the
// cluster does not carry: the route exists, this installation does not
// implement it. An empty 200 would claim there is no user rather than that we
// cannot know.
type FeatureUnavailable struct {
	Error string `json:"error"`
	// Reason is stable. The console keys off it rather than off the message.
	Reason  string `json:"reason"`
	Feature string `json:"feature"`
}

// ReasonFeatureNotInstalled marks a feature whose CRDs are absent.
const ReasonFeatureNotInstalled = "feature-not-installed"

// identityFeature names the identity surface in FeatureUnavailable bodies. The
// console keys off it, so it must stay stable.
const identityFeature = "kubauth identity"

// abortUnavailable answers 501 and reports true when the feature is absent, so
// a handler reads: `if abortUnavailable(c, ok, ...) { return }`.
func abortUnavailable(c *gin.Context, available bool, feature, message string) bool {
	if available {
		return false
	}
	c.JSON(http.StatusNotImplemented, FeatureUnavailable{
		Error:   message,
		Reason:  ReasonFeatureNotInstalled,
		Feature: feature,
	})
	return true
}

// RequireFeature guards a whole route group, so a route added later is covered
// without anyone remembering to cover it.
func RequireFeature(available func(c *gin.Context) bool, feature, message string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !available(c) {
			c.AbortWithStatusJSON(http.StatusNotImplemented, FeatureUnavailable{
				Error:   message,
				Reason:  ReasonFeatureNotInstalled,
				Feature: feature,
			})
			return
		}
		c.Next()
	}
}
