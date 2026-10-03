package repository

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

var storesGVR = schema.GroupVersionResource{
	Group:    "external-secrets.io",
	Version:  "v1",
	Resource: "secretstores",
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
	probe := NewAPIProbe(discoveryServing(t), storesGVR, "external-secrets")

	if probe.Available() {
		t.Fatal("expected the feature to be unavailable with no CRD served")
	}
}

func TestProbeReportsInstalledCRDsAsAvailable(t *testing.T) {
	probe := NewAPIProbe(discoveryServing(t, &metav1.APIResourceList{
		GroupVersion: storesGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "secretstores", Kind: "SecretStore"}},
	}), storesGVR, "external-secrets")

	if !probe.Available() {
		t.Fatal("expected the feature to be available once the CRD is served")
	}
}

// The group being served is not enough: another resource of the same group
// says nothing about ours, and treating it as a yes would send every call to a
// resource the cluster does not have.
func TestProbeRejectsAnotherResourceOfTheSameGroup(t *testing.T) {
	probe := NewAPIProbe(discoveryServing(t, &metav1.APIResourceList{
		GroupVersion: storesGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "externalsecrets", Kind: "ExternalSecret"}},
	}), storesGVR, "external-secrets")

	if probe.Available() {
		t.Fatal("expected the feature to be unavailable when only a sibling resource is served")
	}
}

// A positive answer is cached for the lifetime of the process, so the hot path
// does not hit discovery on every request.
func TestProbeCachesAPositiveAnswer(t *testing.T) {
	discovery := discoveryServing(t, &metav1.APIResourceList{
		GroupVersion: storesGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "secretstores", Kind: "SecretStore"}},
	})
	probe := NewAPIProbe(discovery, storesGVR, "external-secrets")

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
// installs the CRDs and the console stays broken with nothing to explain why.
func TestProbeRetriesAfterANegativeAnswer(t *testing.T) {
	discovery := discoveryServing(t)
	probe := NewAPIProbe(discovery, storesGVR, "external-secrets")

	if probe.Available() {
		t.Fatal("expected the feature to be unavailable at first")
	}

	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: storesGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "secretstores", Kind: "SecretStore"}},
	}}
	// Age the probe past its TTL rather than sleeping through it.
	probe.checkedAt = probe.checkedAt.Add(-2 * crdAvailabilityTTL)

	if !probe.Available() {
		t.Fatal("expected the probe to pick up CRDs installed after a negative answer")
	}
}

// No discovery client at all is not a reason to claim the feature is there.
func TestProbeWithoutDiscoveryIsUnavailable(t *testing.T) {
	probe := NewAPIProbe(nil, storesGVR, "external-secrets")

	if probe.Available() {
		t.Fatal("expected an unavailable feature with no discovery client")
	}
}

// slowDiscovery answers after a delay, standing in for an API server that is
// slow to reply on a cold start.
type slowDiscovery struct {
	discovery.DiscoveryInterface
	delay time.Duration
	calls atomic.Int32
}

func (d *slowDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	d.calls.Add(1)
	time.Sleep(d.delay)
	return d.DiscoveryInterface.ServerResourcesForGroupVersion(groupVersion)
}

// Callers arriving while the first probe runs must get its answer. Reporting
// "unavailable" to them turns a cold start into 501s on a cluster that does
// carry the CRDs.
func TestProbeMakesConcurrentCallersWaitForTheAnswer(t *testing.T) {
	slow := &slowDiscovery{
		DiscoveryInterface: discoveryServing(t, &metav1.APIResourceList{
			GroupVersion: storesGVR.GroupVersion().String(),
			APIResources: []metav1.APIResource{{Name: "secretstores", Kind: "SecretStore"}},
		}),
		delay: 50 * time.Millisecond,
	}
	probe := NewAPIProbe(slow, storesGVR, "external-secrets")

	const callers = 8
	results := make(chan bool, callers)
	var gate sync.WaitGroup
	gate.Add(1)
	for i := 0; i < callers; i++ {
		go func() {
			gate.Wait()
			results <- probe.Available()
		}()
	}
	gate.Done()

	for i := 0; i < callers; i++ {
		if !<-results {
			t.Fatal("expected every concurrent caller to see the feature as available")
		}
	}
	if calls := slow.calls.Load(); calls != 1 {
		t.Fatalf("expected the probe to run once for all callers, ran %d times", calls)
	}
}

// A half-installed CRD set must not pass for the feature being there, or the
// routes answer 200 for one resource and fail on the others.
func TestProbeRequiresEveryDeclaredResource(t *testing.T) {
	discoveryClient := discoveryServing(t, &metav1.APIResourceList{
		GroupVersion: storesGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "secretstores", Kind: "SecretStore"}},
	})

	partial := NewAPIProbe(discoveryClient, storesGVR, "external-secrets", "externalsecrets", "clustersecretstores")
	if partial.Available() {
		t.Fatal("expected the feature to be unavailable while externalsecrets and clustersecretstores are not served")
	}

	complete := NewAPIProbe(discoveryServing(t, &metav1.APIResourceList{
		GroupVersion: storesGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{
			{Name: "secretstores", Kind: "SecretStore"},
			{Name: "externalsecrets", Kind: "ExternalSecret"},
			{Name: "clustersecretstores", Kind: "ClusterSecretStore"},
		},
	}), storesGVR, "external-secrets", "externalsecrets", "clustersecretstores")
	if !complete.Available() {
		t.Fatal("expected the feature to be available once every resource is served")
	}
}
