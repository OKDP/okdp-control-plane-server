package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestActorFromCarriesTheNameAndEmail(t *testing.T) {
	ctx := WithIdentity(context.Background(), &Identity{Subject: "e5a0", Username: "alice", Name: "Alice Martin", Email: "alice@example.com"})
	assert.Equal(t, Actor{Username: "alice", Name: "Alice Martin", Email: "alice@example.com"}, ActorFrom(ctx))
}

func TestActorFromFallsBackToTheUsername(t *testing.T) {
	ctx := WithIdentity(context.Background(), &Identity{Subject: "e5a0"})
	assert.Equal(t, Actor{Username: "e5a0", Name: "e5a0"}, ActorFrom(ctx))
}

func TestActorFromWithoutAuthenticationIsAnonymous(t *testing.T) {
	assert.Equal(t, Actor{Username: "anonymous", Name: "anonymous"}, ActorFrom(context.Background()))
}
