package repository

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

var usersGVR = schema.GroupVersionResource{
	Group:    "kubauth.kubotal.io",
	Version:  "v1alpha1",
	Resource: "users",
}

// discoveryServing builds a discovery client that serves exactly the given
// resource lists, standing in for a cluster with or without the CRDs.
func discoveryServing(t *testing.T, lists ...*metav1.APIResourceList) *fakediscovery.FakeDiscovery {
	t.Helper()
	client := k8sfake.NewSimpleClientset()
	fake, ok := client.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatalf("expected a fake discovery client, got %T", client.Discovery())
	}
	fake.Resources = lists
	return fake
}

// A cluster that does not carry the CRDs must report the feature as absent,
// which is what lets the API answer "not installed here" instead of failing.
func TestProbeReportsMissingCRDsAsUnavailable(t *testing.T) {
	probe := NewAPIProbe(discoveryServing(t), usersGVR, "kubauth identity")

	if probe.Available() {
		t.Fatal("expected the feature to be unavailable with no CRD served")
	}
}

func TestProbeReportsInstalledCRDsAsAvailable(t *testing.T) {
	probe := NewAPIProbe(discoveryServing(t, &metav1.APIResourceList{
		GroupVersion: usersGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "users", Kind: "User"}},
	}), usersGVR, "kubauth identity")

	if !probe.Available() {
		t.Fatal("expected the feature to be available once the CRD is served")
	}
}

// The group being served is not enough: another resource of the same group
// says nothing about ours, and treating it as a yes would send every call to a
// resource the cluster does not have.
func TestProbeRejectsAnotherResourceOfTheSameGroup(t *testing.T) {
	probe := NewAPIProbe(discoveryServing(t, &metav1.APIResourceList{
		GroupVersion: usersGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "groups", Kind: "Group"}},
	}), usersGVR, "kubauth identity")

	if probe.Available() {
		t.Fatal("expected the feature to be unavailable when only a sibling resource is served")
	}
}

// A positive answer is cached for the lifetime of the process, so the hot path
// does not hit discovery on every request.
func TestProbeCachesAPositiveAnswer(t *testing.T) {
	discovery := discoveryServing(t, &metav1.APIResourceList{
		GroupVersion: usersGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "users", Kind: "User"}},
	})
	probe := NewAPIProbe(discovery, usersGVR, "kubauth identity")

	if !probe.Available() {
		t.Fatal("expected the first probe to find the CRD")
	}
	discovery.Resources = nil
	if !probe.Available() {
		t.Fatal("expected the positive answer to be cached")
	}
}

// A negative answer is re-probed once the TTL has passed, so installing the
// CRDs takes effect without restarting the server. Without this the operator
// installs kubauth and the console stays broken with nothing to explain why.
func TestProbeRetriesAfterANegativeAnswer(t *testing.T) {
	discovery := discoveryServing(t)
	probe := NewAPIProbe(discovery, usersGVR, "kubauth identity")

	if probe.Available() {
		t.Fatal("expected the feature to be unavailable at first")
	}

	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: usersGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "users", Kind: "User"}},
	}}
	// Age the probe past its TTL rather than sleeping through it.
	probe.checkedAt = probe.checkedAt.Add(-2 * crdAvailabilityTTL)

	if !probe.Available() {
		t.Fatal("expected the probe to pick up CRDs installed after a negative answer")
	}
}

// No discovery client at all is not a reason to claim the feature is there.
func TestProbeWithoutDiscoveryIsUnavailable(t *testing.T) {
	probe := NewAPIProbe(nil, usersGVR, "kubauth identity")

	if probe.Available() {
		t.Fatal("expected an unavailable feature with no discovery client")
	}
}
