package auth

import "context"

type identityContextKey struct{}

// WithIdentity returns a copy of ctx carrying the caller's identity, which is
// what the Kubernetes client impersonates.
func WithIdentity(ctx context.Context, identity *Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFrom returns the caller's identity, or nil when ctx carries none.
func IdentityFrom(ctx context.Context) *Identity {
	identity, _ := ctx.Value(identityContextKey{}).(*Identity)
	return identity
}
