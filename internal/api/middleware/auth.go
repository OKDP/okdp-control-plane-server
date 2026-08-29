package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
)

// identityKey is where the verified identity is left for the handlers.
const identityKey = "okdp.identity"

// anonymous lists the routes served without a token. /api/capabilities is the
// console's bootstrap: it learns the issuer and client id there before it can
// obtain any token. Matched by pattern, not registration order, which a moved
// line would silently undo.
var anonymous = map[string]bool{
	"/api/capabilities": true,
}

// RequireAuth refuses any request that carries no token the platform's identity
// provider issued for the console. Placed on the whole /api group: a route
// added later is protected because it exists.
func RequireAuth(verifier *auth.Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if anonymous[c.FullPath()] {
			c.Next()
			return
		}

		raw, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			unauthorized(c, "This request carries no bearer token.")
			return
		}

		identity, err := verifier.Verify(c.Request.Context(), raw)
		if err != nil {
			// An unconfigured platform is an operator fault, not a token fault.
			if errors.Is(err, auth.ErrNotConfigured) {
				unauthorized(c, fmt.Sprintf(
					"No request can be authenticated: %s. Declare the provider on the platform Context, under identity.oidc.authority and identity.oidc.clientId or the older oidc.issuerUri, or set OIDC_AUTHORITY and OIDC_CLIENT_ID on the server deployment.",
					err))
				return
			}
			unauthorized(c, err.Error())
			return
		}

		c.Set(identityKey, identity)
		c.Next()
	}
}

// Identity returns the caller established for this request, when there is one.
func Identity(c *gin.Context) (*auth.Identity, bool) {
	v, ok := c.Get(identityKey)
	if !ok {
		return nil, false
	}
	identity, ok := v.(*auth.Identity)
	return identity, ok
}

// bearerToken reads the scheme case-insensitively, as RFC 7235 requires.
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func unauthorized(c *gin.Context, message string) {
	// WWW-Authenticate, per RFC 6750.
	c.Header("WWW-Authenticate", `Bearer realm="okdp-control-plane"`)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": message})
}
