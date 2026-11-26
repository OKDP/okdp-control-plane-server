package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocdgit"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/gitops/flux"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// LabelManagedByControlPlane marks the Flux objects the chart renders for a
// server in git mode, with the release fullname as value.
const LabelManagedByControlPlane = "app.kubernetes.io/managed-by-control-plane"

const (
	gitNetworkTimeout = 30 * time.Second
	syncValidateWait  = 15 * time.Second
	syncValidateTick  = time.Second
	reaperInterval    = 30 * time.Second
)

// clients reach the cluster.
type clients struct {
	dynamic   dynamic.Interface
	typed     kubernetes.Interface
	discovery discovery.DiscoveryInterface
}

// stateOptions is what the desired state stores are built from.
type stateOptions struct {
	engine             kubocd.Config
	platform           repository.ContextRef
	controlPlane       repository.ContextRef
	insecureRegistries []string
	registryAuthFile   string
}

// desiredState holds the repositories the API writes the desired state of
// a project through: the cluster in kubernetes mode, git in git mode.
type desiredState struct {
	projects        repository.ProjectRepository
	connections     repository.ConnectionRepository
	secretStores    repository.SecretStoreRepository
	externalSecrets repository.ExternalSecretRepository
	context         repository.ContextRepository
	engine          engine.Engine
	// start runs the background loops until ctx ends.
	start func(ctx context.Context)
	// wait blocks until all background loops and in-flight sync tasks have stopped.
	wait func()
}

// packageSource lets the git engine read the package schemas through a
// service built after it, on the Context repository the engine provides.
type packageSource struct{ kubocd.PackageSource }

func newPackages(contextRepo repository.ContextRepository, opts stateOptions) *service.DefaultPackageSchemaService {
	packages := service.NewDefaultPackageSchemaService(contextRepo)
	packages.SetInsecureRegistries(opts.insecureRegistries)
	packages.SetRegistryAuthFile(opts.registryAuthFile)
	return packages
}

// buildDesiredState wires the stores of the persistence mode the engine
// configuration selects.
func buildDesiredState(ctx context.Context, cl clients, opts stateOptions) (*desiredState, error) {
	switch opts.engine.Persistence.Mode {
	case kubocd.PersistenceGit:
		return gitDesiredState(ctx, cl, opts)
	default:
		return kubernetesDesiredState(ctx, cl, opts)
	}
}

func kubernetesDesiredState(ctx context.Context, cl clients, opts stateOptions) (*desiredState, error) {
	if err := cleanupSyncObjects(ctx, cl.dynamic, opts.engine.Persistence.Git.SyncNamespace, opts.engine.SyncCleanupName); err != nil {
		return nil, err
	}

	contextRepo := repository.NewSplitContextRepository(cl.dynamic, opts.platform, opts.controlPlane)
	packages := newPackages(contextRepo, opts)
	return &desiredState{
		projects:        repository.NewProjectRepository(cl.typed),
		connections:     repository.NewConnectionRepository(cl.dynamic, cl.typed, cl.discovery),
		secretStores:    repository.NewSecretStoreRepository(cl.dynamic, cl.discovery),
		externalSecrets: repository.NewExternalSecretRepository(cl.dynamic, cl.discovery),
		context:         contextRepo,
		engine: kubocd.New(kubocd.Deps{
			Releases:           repository.NewServiceRepository(cl.dynamic),
			Context:            contextRepo,
			ContextWriter:      repository.NewSplitContextWriterRepository(cl.dynamic, opts.platform, opts.controlPlane),
			Packages:           packages,
			Cluster:            cl.dynamic,
			Config:             opts.engine,
			InsecureRegistries: opts.insecureRegistries,
		}),
		start: func(context.Context) {},
		wait:  func() {},
	}, nil
}

func gitDesiredState(ctx context.Context, cl clients, opts stateOptions) (*desiredState, error) {
	if opts.controlPlane == opts.platform {
		return nil, fmt.Errorf("git persistence: control plane Context %s must be distinct from the read-only platform Context %s; configure CONTROL_PLANE_CONTEXT_NAME and PLATFORM_CONTEXT_NAME separately", opts.controlPlane, opts.platform)
	}
	gitCfg := opts.engine.Persistence.Git
	var committer gitstore.Author
	if gitCfg.CommitterName != "" || gitCfg.CommitterEmail != "" {
		committer = gitstore.Author{
			Name:  gitCfg.CommitterName,
			Email: gitCfg.CommitterEmail,
		}
		if committer.Name == "" {
			committer.Name = gitstore.DefaultCommitter.Name
		}
		if committer.Email == "" {
			committer.Email = gitstore.DefaultCommitter.Email
		}
	}
	credsProvider := gitstore.DirCredentialsProvider(gitCfg.URL, gitCfg.CredentialsDir)
	store, err := gitstore.New(gitstore.Config{
		Repository:          gitops.Repository{URL: gitCfg.URL, Branch: gitCfg.Branch, Path: gitCfg.Path},
		CredentialsProvider: credsProvider,
		Committer:           committer,
		NetworkTimeout:      gitNetworkTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("git persistence: %w", err)
	}
	if _, err := credsProvider(); err != nil {
		return nil, fmt.Errorf("git persistence: credentials: %w", err)
	}

	// A literal nil when no sync tool is observed: a nil *flux.Adapter in
	// the interface would be called.
	var syncAdapter gitops.SyncAdapter
	if gitCfg.Sync == kubocd.SyncFlux {
		adapter, err := flux.New(cl.dynamic, flux.Config{
			Namespace:          gitCfg.SyncNamespace,
			GitRepositoryName:  gitCfg.GitRepositoryName,
			KustomizationName:  gitCfg.KustomizationName,
			ExpectedRepository: store.Repository(),
		})
		if err != nil {
			return nil, err
		}
		if err := validateSync(ctx, adapter, syncValidateWait, syncValidateTick); err != nil {
			return nil, err
		}
		syncAdapter = adapter
	}

	if gitCfg.Initialize {
		res, err := store.Bootstrap(ctx)
		if err != nil {
			return nil, fmt.Errorf("git persistence: initialize the branch: %w", err)
		}
		logrus.WithFields(logrus.Fields{"revision": res.Revision, "created": res.Created}).Info("Git branch ready")
	}

	packages := &packageSource{}
	adapter := kubocdgit.New(kubocdgit.Deps{
		Store:               store,
		Sync:                syncAdapter,
		Platform:            repository.NewSplitContextRepository(cl.dynamic, opts.platform, opts.controlPlane),
		Cluster:             cl.dynamic,
		Packages:            packages,
		Config:              opts.engine,
		ControlPlaneContext: opts.controlPlane,
		Catalog: func(ctx context.Context) (map[string]any, bool, error) {
			return repository.ReadServiceCatalog(ctx, cl.dynamic, opts.platform, opts.controlPlane)
		},
		InsecureRegistries: opts.insecureRegistries,
	})
	if gitCfg.Initialize {
		created, err := adapter.BootstrapControlPlaneContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("git persistence: initialize the control plane Context: %w", err)
		}
		if created {
			logrus.WithField("context", opts.controlPlane.String()).Info("Committed the control plane Context")
		}
	}

	contextRepo := adapter.ContextRepository()
	packages.PackageSource = newPackages(contextRepo, opts)

	connections := kubocdgit.NewConnectionRepository(adapter, repository.NewConnectionRepository(cl.dynamic, cl.typed, cl.discovery), cl.typed)
	secretStores := kubocdgit.NewSecretStoreRepository(adapter, repository.NewSecretStoreRepository(cl.dynamic, cl.discovery), cl.typed)
	reaper := kubocdgit.NewCredentialsReaper(adapter, cl.typed, connections, secretStores)
	var bgWG sync.WaitGroup
	return &desiredState{
		projects:        kubocdgit.NewProjectRepository(adapter, cl.typed),
		connections:     connections,
		secretStores:    secretStores,
		externalSecrets: kubocdgit.NewExternalSecretRepository(adapter, repository.NewExternalSecretRepository(cl.dynamic, cl.discovery)),
		context:         contextRepo,
		engine:          adapter.Engine(),
		start: func(ctx context.Context) {
			adapter.Start(ctx)
			bgWG.Add(1)
			go func() {
				defer bgWG.Done()
				reaper.Run(ctx, reaperInterval)
			}()
		},
		wait: func() {
			bgWG.Wait()
			adapter.Wait()
		},
	}, nil
}

// validator is the part of a sync adapter checked at startup.
type validator interface {
	Validate(ctx context.Context) error
}

// validateSync checks the sync objects, waiting up to wait for objects that
// do not exist yet (created by the same helm install). Objects that exist
// and point elsewhere fail at once.
func validateSync(ctx context.Context, v validator, wait, tick time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := v.Validate(ctx)
		if err == nil || !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			return err
		}
		logrus.WithError(err).Info("Waiting for the sync objects")
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(tick):
		}
	}
}

// cleanupSyncObjects deletes the Flux Kustomization, then the GitRepository,
// named name in namespace, when they carry LabelManagedByControlPlane=name:
// the objects a server in git mode had the chart render, left in place by
// helm.sh/resource-policy keep. Objects without that label are left alone,
// and a missing object or CRD is nothing to clean.
//
// The Kustomization is deleted only with deletionPolicy Orphan, which keeps
// what it applied: any other policy would make Flux delete the project
// namespaces and their volumes. It must be gone before the server writes to
// the cluster, so any failure to read it, delete it or see it gone is an
// error. The GitRepository applies nothing on its own: a failure on it is
// logged.
func cleanupSyncObjects(ctx context.Context, client dynamic.Interface, namespace, name string) error {
	if name == "" || namespace == "" {
		return nil
	}
	if err := cleanupKustomization(ctx, client.Resource(flux.KustomizationResource).Namespace(namespace), name); err != nil {
		return fmt.Errorf("cannot switch back to kubernetes mode while Flux applies the git branch: Kustomization %s/%s: %w", namespace, name, err)
	}
	log := logrus.WithFields(logrus.Fields{"resource": flux.GitRepositoryResource.Resource, "object": namespace + "/" + name})
	repos := client.Resource(flux.GitRepositoryResource).Namespace(namespace)
	repo, err := repos.Get(ctx, name, metav1.GetOptions{})
	switch {
	case absent(err):
		return nil
	case err != nil:
		log.WithError(err).Warn("Cannot read a GitRepository left by git mode")
		return nil
	case repo.GetLabels()[LabelManagedByControlPlane] != name:
		log.Info("Leaving a GitRepository the chart did not render")
		return nil
	}
	uid := repo.GetUID()
	err = repos.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	switch {
	case err == nil:
		log.Info("Deleted the GitRepository left by git mode")
	case absent(err):
	default:
		log.WithError(err).Warn("Cannot delete the GitRepository left by git mode")
	}
	return nil
}

// Wait for a deleted Kustomization to be gone: the kustomize-controller
// removes its finalizer once it has orphaned what it applied.
var (
	kustomizationGoneWait = 30 * time.Second
	kustomizationGoneTick = 200 * time.Millisecond
)

const cleanupMaxConflicts = 5

func cleanupKustomization(ctx context.Context, objects dynamic.ResourceInterface, name string) error {
	var namespace string
	for attempt := 0; ; attempt++ {
		ks, err := objects.Get(ctx, name, metav1.GetOptions{})
		if absent(err) {
			return nil
		}
		if err != nil {
			return err
		}
		namespace = ks.GetNamespace()
		if ks.GetLabels()[LabelManagedByControlPlane] != name {
			logrus.WithField("object", namespace+"/"+name).Info("Leaving a Kustomization the chart did not render")
			return nil
		}
		if policy, _, _ := unstructured.NestedString(ks.Object, "spec", "deletionPolicy"); policy != "Orphan" {
			return fmt.Errorf("spec.deletionPolicy is %q, not \"Orphan\": deleting it would delete what it applied. Set it to Orphan, or delete it by hand", policy)
		}
		if ks.GetDeletionTimestamp() != nil {
			// Already terminating with deletionPolicy=Orphan: wait for Flux to
			// orphan what it applied and remove its finalizer.
			break
		}
		uid, rv := ks.GetUID(), ks.GetResourceVersion()
		err = objects.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
		if absent(err) {
			break
		}
		if apierrors.IsConflict(err) && attempt+1 < cleanupMaxConflicts {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	deadline := time.Now().Add(kustomizationGoneWait)
	for {
		_, err := objects.Get(ctx, name, metav1.GetOptions{})
		switch {
		case absent(err):
			logrus.WithField("object", namespace+"/"+name).Info("Deleted the Kustomization left by git mode: Flux no longer applies the branch")
			return nil
		case err != nil:
			return err
		case time.Now().After(deadline):
			return fmt.Errorf("still there %s after its deletion", kustomizationGoneWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(kustomizationGoneTick):
		}
	}
}

// absent tells a missing object or a CRD the cluster does not serve.
func absent(err error) bool {
	return err != nil && (apierrors.IsNotFound(err) || meta.IsNoMatchError(err))
}
