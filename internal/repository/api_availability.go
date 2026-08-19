package repository

import (
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/sirupsen/logrus"
)

// crdAvailabilityTTL bounds how long a negative probe is trusted, so that
// installing the CRDs takes effect without restarting the server.
const crdAvailabilityTTL = 30 * time.Second

// APIProbe answers "is this custom resource served by the cluster?".
//
// Discovery is the only unambiguous way to know: a call on an unserved resource
// fails with a NotFound, exactly like a call for a missing object.
type APIProbe struct {
	discoveryClient discovery.DiscoveryInterface
	gvr             schema.GroupVersionResource
	// Resources of the same group/version that must be served too. A partial
	// install serving some of them is not the feature being available.
	alsoRequired []string
	feature      string

	mu        sync.Mutex
	available bool
	checkedAt time.Time
	inflight  chan struct{}
}

func NewAPIProbe(discoveryClient discovery.DiscoveryInterface, gvr schema.GroupVersionResource, feature string, alsoRequired ...string) *APIProbe {
	return &APIProbe{discoveryClient: discoveryClient, gvr: gvr, alsoRequired: alsoRequired, feature: feature}
}

// Available caches a positive answer for the process lifetime and re-probes a
// negative one, so installing the CRDs takes effect without a restart.
//
// The discovery call runs outside the lock: holding it would queue every
// request gated on this probe behind a slow API server. Callers arriving while
// a probe runs wait for its answer instead of being told the resource is
// missing, which would turn a cold start into transient 501s.
func (p *APIProbe) Available() bool {
	if p == nil {
		return false
	}

	for {
		p.mu.Lock()
		if p.available {
			p.mu.Unlock()
			return true
		}
		if !p.checkedAt.IsZero() && time.Since(p.checkedAt) < crdAvailabilityTTL {
			p.mu.Unlock()
			return false
		}
		if inflight := p.inflight; inflight != nil {
			p.mu.Unlock()
			<-inflight
			continue
		}
		done := make(chan struct{})
		p.inflight = done
		p.mu.Unlock()

		available := p.probe()

		p.mu.Lock()
		p.inflight = nil
		p.checkedAt = time.Now()
		p.available = available
		p.mu.Unlock()
		close(done)

		return available
	}
}

func (p *APIProbe) probe() bool {
	if p.discoveryClient == nil {
		return false
	}

	groupVersion := p.gvr.GroupVersion().String()
	resources, err := p.discoveryClient.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		// A missing group is expected, not a failure worth logging every time.
		if !apierrors.IsNotFound(err) {
			logrus.WithError(err).WithField("feature", p.feature).Debug("Could not discover the CRDs")
		}
		return false
	}

	served := make(map[string]bool, len(resources.APIResources))
	for _, resource := range resources.APIResources {
		served[resource.Name] = true
	}
	for _, required := range append([]string{p.gvr.Resource}, p.alsoRequired...) {
		if !served[required] {
			return false
		}
	}

	logrus.WithField("feature", p.feature).Info("CRDs detected: the feature is enabled")
	return true
}
