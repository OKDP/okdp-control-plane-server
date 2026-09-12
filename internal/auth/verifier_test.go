package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTheEnvironmentIssuerWinsOverTheContext(t *testing.T) {
	issuer, err := ResolveIssuer(context.Background(), "https://override.example.org/realms/okdp",
		func(context.Context) (string, error) { return "https://context.example.org/realms/okdp", nil })

	require.NoError(t, err)
	assert.Equal(t, "https://override.example.org/realms/okdp", issuer)
}

// The normal deployment: nothing in the environment, the platform Context
// answers, and no chart or package carries the setting.
func TestTheIssuerComesFromTheContextWhenNothingOverridesIt(t *testing.T) {
	issuer, err := ResolveIssuer(context.Background(), "",
		func(context.Context) (string, error) { return "https://keycloak.okdp.sandbox/realms/master", nil })

	require.NoError(t, err)
	assert.Equal(t, "https://keycloak.okdp.sandbox/realms/master", issuer)
}

// Fail closed. Coming up with no issuer at all would mean serving the whole
// API to anyone who reaches the port, which is the state this is fixing.
func TestNoIssuerAnywhereIsAnError(t *testing.T) {
	_, err := ResolveIssuer(context.Background(), "", func(context.Context) (string, error) { return "", nil })

	require.Error(t, err)
	assert.Contains(t, err.Error(), "OIDC_ISSUER")
}

// An unreadable Context is not "no issuer": it must not be mistaken for a
// platform that declares none.
func TestAnUnreadableContextIsReported(t *testing.T) {
	_, err := ResolveIssuer(context.Background(), "",
		func(context.Context) (string, error) {
			return "", errors.New("contexts.kubocd.kubotal.io \"platform\" not found")
		})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
