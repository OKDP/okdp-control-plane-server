package auth

import "context"

type identityContextKey struct{}

// WithIdentity returns a context carrying the authenticated caller, so layers
// below the HTTP handlers (the Git writer signs its commits with it) can name
// who acted without depending on the web framework.
func WithIdentity(ctx context.Context, identity *Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFromContext returns the caller stored by WithIdentity, or nil.
func IdentityFromContext(ctx context.Context) *Identity {
	identity, _ := ctx.Value(identityContextKey{}).(*Identity)
	return identity
}

// ActorName names the caller for an audit trail: the user name when the token
// carries one, the subject otherwise, "anonymous" without authentication.
func ActorName(ctx context.Context) string {
	identity := IdentityFromContext(ctx)
	switch {
	case identity == nil:
		return "anonymous"
	case identity.Username != "":
		return identity.Username
	case identity.Subject != "":
		return identity.Subject
	default:
		return "anonymous"
	}
}
