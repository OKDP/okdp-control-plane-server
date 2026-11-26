package auth

import "context"

type identityKey struct{}

// NewContext returns a copy of ctx carrying id.
func NewContext(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext returns the identity ctx carries, nil when it carries none.
func FromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(identityKey{}).(*Identity)
	return id
}
