package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestListSelectableOffersTheProjectConnectionsOfTheContract(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("List", mock.Anything, "demo").Return([]crd.Connection{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "rnacentral", Namespace: "demo"},
			Spec:       crd.ConnectionSpec{Contract: "database-server", Description: "public db"},
			Status:     crd.ConnectionStatus{Phase: "READY"},
		},
		{
			// Managed by a release: still selectable, which is the whole point
			// of a published connection.
			ObjectMeta: metav1.ObjectMeta{Name: "demo-trino-endpoint", Namespace: "demo"},
			Spec:       crd.ConnectionSpec{Contract: "trino", OutputName: "endpoint"},
			Status:     crd.ConnectionStatus{Phase: "READY", Parent: "demo-trino"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "lake", Namespace: "demo"},
			Spec:       crd.ConnectionSpec{Contract: "s3"},
		},
	}, nil)

	selectable, err := svc.ListSelectable(context.Background(), "demo", "database-server")

	require.NoError(t, err)
	require.Len(t, selectable, 1, "only the connections of the project, of that contract")
	assert.Equal(t, "rnacentral", selectable[0].Name)
	assert.Equal(t, "project", selectable[0].Scope)
}

func TestListSelectableIncludesManagedConnections(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("List", mock.Anything, "demo").Return([]crd.Connection{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-trino-endpoint", Namespace: "demo"},
			Spec:       crd.ConnectionSpec{Contract: "trino", OutputName: "endpoint"},
			Status:     crd.ConnectionStatus{Phase: "READY", Parent: "demo-trino"},
		},
	}, nil)
	connectionRepo.On("List", mock.Anything, "").Return([]crd.Connection{}, nil)

	selectable, err := svc.ListSelectable(context.Background(), "demo", "trino")

	require.NoError(t, err)
	require.Len(t, selectable, 1)
	assert.True(t, selectable[0].Managed)
	assert.Equal(t, "demo-trino", selectable[0].ProvidedBy)
}

func TestListSelectableIsEmptyWithoutTheCRDs(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, false)

	selectable, err := svc.ListSelectable(context.Background(), "demo", "database-server")

	require.NoError(t, err)
	assert.Empty(t, selectable)
	connectionRepo.AssertNotCalled(t, "List", mock.Anything, mock.Anything)
}
