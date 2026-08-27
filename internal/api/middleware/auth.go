package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
)

// identityKey is where the verified identity is left for the handlers. Unused
// so far: phase one establishes who is calling and stops there.
const identityKey = "okdp.identity"

// anonymous lists the routes served without a token, by their route pattern
// rather than by registration order.
//
// /api/capabilities is the console's bootstrap: it reads the issuer and the
// client id from it BEFORE it can build its OIDC client, so requiring a token
// here would leave the console unable to ever obtain one.
//
// Matching the pattern, not the order, is deliberate. Gin applies a group
// middleware only to the routes declared after it, so an exemption expressed
// by placement would be undone by whoever moves a line, silently.
var anonymous = map[string]bool{
	"/api/capabilities": true,
}

// RequireAuth refuses any request that carries no token the platform's identity
// provider issued for the console.
//
// It is placed on the whole /api group rather than on each route: a route added
// later is protected because it exists, not because someone remembered.
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
			// Told apart on purpose. A platform with no published OIDC client
			// is an operator's job, and answering "invalid token" would send
			// the reader hunting a token that was never the problem.
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

// bearerToken reads the scheme case-insensitively, as RFC 7235 requires, and
// refuses an empty credential rather than passing "" down to be verified.
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func unauthorized(c *gin.Context, message string) {
	// WWW-Authenticate is what tells a client this is an authentication
	// failure it can act on, rather than a refusal it cannot.
	c.Header("WWW-Authenticate", `Bearer realm="okdp-control-plane"`)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": message})
}
