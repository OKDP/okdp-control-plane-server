// Package flux is the gitops.SyncAdapter for Flux: a source.toolkit.fluxcd.io/v1
// GitRepository that fetches the branch and a kustomize.toolkit.fluxcd.io/v1
// Kustomization that applies its path. Both objects are created outside the
// server, the adapter only reads them and annotates the GitRepository.
package flux

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

var (
	// GitRepositoryResource is the Flux source the adapter wakes up.
	GitRepositoryResource = schema.GroupVersionResource{
		Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories",
	}
	// KustomizationResource is the Flux object that applies the source.
	KustomizationResource = schema.GroupVersionResource{
		Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations",
	}
)

const (
	// ReconcileRequestAnnotation makes a Flux controller reconcile now when
	// its value changes.
	ReconcileRequestAnnotation = "reconcile.fluxcd.io/requestedAt"

	gitRepositoryKind   = "GitRepository"
	kustomizationKind   = "Kustomization"
	defaultPatchTimeout = 10 * time.Second
)

// Config names the Flux objects the adapter works on and the repository the
// server commits to.
type Config struct {
	// Namespace holds both objects.
	Namespace         string
	GitRepositoryName string
	KustomizationName string
	// ExpectedRepository is where the server commits. Validate fails when
	// the Flux objects pull from anywhere else.
	ExpectedRepository gitops.Repository

	// Now is the clock, time.Now when nil.
	Now func() time.Time
	// PatchTimeout bounds one GitRepository patch, 10s when zero.
	PatchTimeout time.Duration
}

// Adapter is a gitops.SyncAdapter backed by Flux. Create it with New.
type Adapter struct {
	client dynamic.Interface
	cfg    Config

	mu sync.Mutex
	// requested counts the RequestSync calls, done is the highest count a
	// successful patch covered, failedUpTo the highest count a failed patch
	// covered with failErr.
	requested  uint64
	done       uint64
	failedUpTo uint64
	failErr    error
	inFlight   bool
	// patched is closed and replaced when a patch ends.
	patched chan struct{}
	// lastRequestedAt is the last annotation value, kept strictly increasing.
	lastRequestedAt time.Time
}

var _ gitops.SyncAdapter = (*Adapter)(nil)

// New returns an adapter for the objects of cfg. It does not contact the
// cluster, see Validate.
func New(client dynamic.Interface, cfg Config) (*Adapter, error) {
	var errs []error
	if client == nil {
		errs = append(errs, errors.New("dynamic client is nil"))
	}
	for _, f := range []struct{ name, value string }{
		{"Namespace", cfg.Namespace},
		{"GitRepositoryName", cfg.GitRepositoryName},
		{"KustomizationName", cfg.KustomizationName},
		{"ExpectedRepository.URL", cfg.ExpectedRepository.URL},
		{"ExpectedRepository.Branch", cfg.ExpectedRepository.Branch},
	} {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("%s is empty", f.name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("flux: invalid config: %w", err)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PatchTimeout <= 0 {
		cfg.PatchTimeout = defaultPatchTimeout
	}
	return &Adapter{client: client, cfg: cfg, patched: make(chan struct{})}, nil
}

func (a *Adapter) repoRef() string {
	return fmt.Sprintf("%s %s/%s", gitRepositoryKind, a.cfg.Namespace, a.cfg.GitRepositoryName)
}

func (a *Adapter) ksRef() string {
	return fmt.Sprintf("%s %s/%s", kustomizationKind, a.cfg.Namespace, a.cfg.KustomizationName)
}

func (a *Adapter) repos() dynamic.ResourceInterface {
	return a.client.Resource(GitRepositoryResource).Namespace(a.cfg.Namespace)
}

func (a *Adapter) kustomizations() dynamic.ResourceInterface {
	return a.client.Resource(KustomizationResource).Namespace(a.cfg.Namespace)
}
