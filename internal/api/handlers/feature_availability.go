package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// FeatureUnavailable is the body returned when a route rests on CRDs the
// cluster does not carry.
//
// The status is 501: the request is well formed and the route exists, this
// installation just does not implement the feature. 500 said "the server
// broke", which sent readers looking for a fault that was not there; an empty
// 200 list would have been worse, claiming there is no user when the truth is
// that we cannot know.
type FeatureUnavailable struct {
	Error string `json:"error"`
	// Reason is stable and machine-readable, so the console can tell this apart
	// from a genuine failure without matching on the message.
	Reason string `json:"reason"`
	// Feature names what is missing, in the operator's words.
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

// RequireFeature guards a whole route group, which is what makes the guarantee
// worth anything: a route added to the group later is covered without anyone
// remembering to cover it. Guarding each handler by hand is how one route keeps
// answering 500 long after the others were fixed.
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
