package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
)

func newServiceWithRelease(t *testing.T, status crd.ReleaseStatus) *DefaultServiceService {
	t.Helper()
	releaseRepo := &mocks.ServiceRepository{}
	releaseRepo.On("Get", mock.Anything, "my-project", "my-project-trino").Return(&crd.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "my-project-trino", Namespace: "my-project"},
		Spec:       crd.ReleaseSpec{TargetNamespace: "my-project"},
		Status:     status,
	}, nil)
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			crd.GetHelmReleaseGVR():           "HelmReleaseList",
			{Version: "v1", Resource: "pods"}: "PodList",
			{Group: "kubocd.kubotal.io", Version: "v1alpha1", Resource: "contexts"}: "ContextList",
		},
	)
	return &DefaultServiceService{
		releaseRepo: releaseRepo,
		contextRepo: repository.NewContextRepository(client, "default", "kubocd-system"),
		k8sClient:   client,
	}
}

func TestGetService_WaitingForDependency_ReturnsReleaseMessage(t *testing.T) {
	s := newServiceWithRelease(t, crd.ReleaseStatus{
		Phase:   "WAIT_DEPS",
		Message: "Waiting for the role 'data-catalog' to be ready",
	})

	instance, err := s.GetService(context.Background(), "my-project", "trino")

	require.NoError(t, err)
	assert.Equal(t, "Installing", instance.Status)
	assert.Equal(t, "Waiting for the role 'data-catalog' to be ready", instance.StatusMessage)
}

func TestGetService_Ready_HasNoStatusMessage(t *testing.T) {
	s := newServiceWithRelease(t, crd.ReleaseStatus{Phase: "READY", Message: "ignored"})

	instance, err := s.GetService(context.Background(), "my-project", "trino")

	require.NoError(t, err)
	assert.Empty(t, instance.StatusMessage)
}
