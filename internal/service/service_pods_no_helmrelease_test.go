package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
)

func newServiceWithoutHelmRelease(t *testing.T) *DefaultServiceService {
	t.Helper()
	releaseRepo := &mocks.ServiceRepository{}
	releaseRepo.On("Get", mock.Anything, "my-project", "my-project-trino").Return(&crd.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "my-project-trino", Namespace: "my-project", UID: types.UID("release-uid")},
	}, nil)
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			crd.GetHelmReleaseGVR():                                         "HelmReleaseList",
			{Version: "v1", Resource: "pods"}:                               "PodList",
			{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}: "PodMetricsList",
		},
	)
	return &DefaultServiceService{releaseRepo: releaseRepo, k8sClient: client}
}

func TestListPods_NoHelmReleaseYet_ReturnsEmptyList(t *testing.T) {
	s := newServiceWithoutHelmRelease(t)

	pods, err := s.ListPods(context.Background(), "my-project", "trino")

	require.NoError(t, err)
	assert.NotNil(t, pods)
	assert.Empty(t, pods)
}

func TestGetServiceMetrics_NoHelmReleaseYet_ReturnsEmptyMetrics(t *testing.T) {
	s := newServiceWithoutHelmRelease(t)

	metrics, err := s.GetServiceMetrics(context.Background(), "my-project", "trino")

	require.NoError(t, err)
	require.NotNil(t, metrics)
	assert.Equal(t, buildServiceMetrics(nil, nil), metrics)
}

func TestListPods_ReleaseLookupFails_ReturnsError(t *testing.T) {
	releaseRepo := &mocks.ServiceRepository{}
	releaseRepo.On("Get", mock.Anything, "my-project", "my-project-trino").Return(nil, errors.New("release not found"))
	s := &DefaultServiceService{releaseRepo: releaseRepo}

	_, err := s.ListPods(context.Background(), "my-project", "trino")

	assert.Error(t, err)
}
