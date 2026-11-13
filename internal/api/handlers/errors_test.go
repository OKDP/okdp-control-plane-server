package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestAKubernetesRefusalIsAForbidden(t *testing.T) {
	refused := apierrors.NewForbidden(schema.GroupResource{Group: "kubocd.kubotal.io", Resource: "releases"}, "demo", errors.New(`User "oidc:alice" cannot create resource`))

	assert.Equal(t, http.StatusForbidden, serverErrorStatus(refused))
	assert.Equal(t, http.StatusForbidden, serverErrorStatus(fmt.Errorf("deploying demo: %w", refused)))
	assert.Equal(t, http.StatusInternalServerError, serverErrorStatus(errors.New("boom")))
}
