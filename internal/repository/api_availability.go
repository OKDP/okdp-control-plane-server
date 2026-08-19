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
	feature         string

	mu        sync.Mutex
	available bool
	probing   bool
	checkedAt time.Time
}

func NewAPIProbe(discoveryClient discovery.DiscoveryInterface, gvr schema.GroupVersionResource, feature string) *APIProbe {
	return &APIProbe{discoveryClient: discoveryClient, gvr: gvr, feature: feature}
}

// Available caches a positive answer for the process lifetime and re-probes a
// negative one, so installing the CRDs takes effect without a restart.
//
// The discovery call runs outside the lock: holding it would queue every
// request gated on this probe behind a slow API server, for a question whose
// answer is already known.
func (p *APIProbe) Available() bool {
	if p == nil {
		return false
	}

	p.mu.Lock()
	switch {
	case p.available:
		p.mu.Unlock()
		return true
	case p.probing, !p.checkedAt.IsZero() && time.Since(p.checkedAt) < crdAvailabilityTTL:
		p.mu.Unlock()
		return false
	}
	p.probing = true
	p.mu.Unlock()

	available := p.probe()

	p.mu.Lock()
	p.probing = false
	p.checkedAt = time.Now()
	p.available = available
	p.mu.Unlock()

	return available
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

	for _, resource := range resources.APIResources {
		if resource.Name == p.gvr.Resource {
			logrus.WithField("feature", p.feature).Info("CRDs detected: the feature is enabled")
			return true
		}
	}
	return false
}
