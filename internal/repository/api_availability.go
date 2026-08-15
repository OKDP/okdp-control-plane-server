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
// Several features of the console rest on CRDs that an installation may simply
// not have: the KuboCD connections, kubauth for identity, external-secrets for
// the secret stores. Without this, a missing CRD surfaces as a 500 and reads as
// a broken server rather than as a feature that was never installed.
//
// Asking discovery is the only unambiguous way to tell the two apart. The error
// returned by a call on an unserved resource is a NotFound, exactly like the one
// returned for an object that does not exist, so the error alone cannot carry
// the distinction.
type APIProbe struct {
	discoveryClient discovery.DiscoveryInterface
	gvr             schema.GroupVersionResource
	// feature names the thing in a log line, in the operator's words rather
	// than the API's ("kubauth identity", not "users.kubauth.kubotal.io").
	feature string

	mu        sync.Mutex
	available bool
	checkedAt time.Time
}

func NewAPIProbe(discoveryClient discovery.DiscoveryInterface, gvr schema.GroupVersionResource, feature string) *APIProbe {
	return &APIProbe{discoveryClient: discoveryClient, gvr: gvr, feature: feature}
}

// Available reports whether the resource is served. A positive result is kept
// for the lifetime of the process, CRDs do not go away in practice; a negative
// one is re-probed, so installing them takes effect without a restart.
func (p *APIProbe) Available() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.available {
		return true
	}
	if !p.checkedAt.IsZero() && time.Since(p.checkedAt) < crdAvailabilityTTL {
		return false
	}
	p.checkedAt = time.Now()

	if p.discoveryClient == nil {
		return false
	}

	groupVersion := p.gvr.GroupVersion().String()
	resources, err := p.discoveryClient.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		// A missing group is the expected state on an installation that does not
		// carry the feature, not a failure worth logging on every request.
		if !apierrors.IsNotFound(err) {
			logrus.WithError(err).WithField("feature", p.feature).Debug("Could not discover the CRDs")
		}
		return false
	}

	for _, resource := range resources.APIResources {
		if resource.Name == p.gvr.Resource {
			p.available = true
			logrus.WithField("feature", p.feature).Info("CRDs detected: the feature is enabled")
			return true
		}
	}
	return false
}
