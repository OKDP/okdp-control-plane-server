package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
)

// The only routes under /api served without a token.
var publicPaths = map[string]bool{
	"/api/capabilities": true,
}

var (
	errMissingToken    = errors.New("an Authorization header is required")
	errMalformedHeader = errors.New("the Authorization header must be 'Bearer <token>'")
)

// RequireAuthentication rejects any request without a token this platform's
// issuer signed.
func RequireAuthentication(verifier auth.Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if publicPaths[c.FullPath()] {
			c.Next()
			return
		}

		rawToken, err := bearerToken(c.GetHeader("Authorization"))
		if err != nil {
			unauthorized(c, err.Error())
			return
		}

		if err := verifier.Verify(c.Request.Context(), rawToken); err != nil {
			// The reason stays in the log: naming the failed check tells an
			// attacker which one to work on.
			logrus.WithError(err).WithField("path", c.Request.URL.Path).Warn("Rejected a bearer token")
			unauthorized(c, "the bearer token was rejected")
			return
		}

		c.Next()
	}
}

func bearerToken(header string) (string, error) {
	if header == "" {
		return "", errMissingToken
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", errMalformedHeader
	}
	return strings.TrimSpace(token), nil
}

func unauthorized(c *gin.Context, message string) {
	// Tells a non-browser client this is authentication, not a missing route.
	c.Header("WWW-Authenticate", `Bearer realm="okdp"`)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": message})
}
