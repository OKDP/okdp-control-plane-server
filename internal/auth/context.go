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

// Actor is the caller as the Git writer records it: Username goes in the
// commit subject, Name and Email make the commit author.
type Actor struct {
	Username string
	Name     string
	// Email is empty when the token has no email claim or without
	// authentication; the service identity authors the commit then.
	Email string
}

// ActorFrom returns the caller of ctx. Username follows ActorName; Name falls
// back to Username when the token carries no display name.
func ActorFrom(ctx context.Context) Actor {
	actor := Actor{Username: ActorName(ctx)}
	if identity := IdentityFromContext(ctx); identity != nil {
		actor.Name = identity.Name
		actor.Email = identity.Email
	}
	if actor.Name == "" {
		actor.Name = actor.Username
	}
	return actor
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
