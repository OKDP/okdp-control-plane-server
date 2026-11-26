package kubocdgit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// namespaceProvisioningError reports that a project Namespace does not exist in
// the cluster yet because the sync tool has not applied the project commit.
// It deliberately does not wrap the raw Kubernetes NotFound status error so
// handlers map it to 503 Service Unavailable rather than 404 Not Found.
func namespaceProvisioningError(namespace string) error {
	return fmt.Errorf("%w: namespace %q is not yet created in the cluster; wait for the sync tool to apply the project and retry", engine.ErrNamespaceProvisioning, namespace)
}

// releaseCredentials marks a credentials Secret the control plane owns
// instead of deleting it: the change that drops it reaches the cluster only
// when the sync tool applies the commit, and the consumers keep reading the
// Secret until then. CredentialsReaper deletes it once nothing references
// it. A Secret the control plane does not own is never touched.
func releaseCredentials(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if secret.Labels[crd.LabelManagedBy] != crd.ManagedByValue || secret.Labels[crd.LabelOrphanedCredentials] == "true" {
		return nil
	}
	secret.Labels[crd.LabelOrphanedCredentials] = "true"
	_, err = client.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
	return err
}

// credentialsReferrer reads the objects of one kind that may reference a
// credentials Secret.
type credentialsReferrer interface {
	// credentialsReferences returns the Secrets of namespace the objects
	// reference, in snap and in the cluster. A failed read is an error,
	// never an empty answer.
	credentialsReferences(ctx context.Context, snap *gitstore.Snapshot, namespace string) (map[string]bool, error)
}

// CredentialsReaper deletes the marked credentials Secrets that no
// Connection and no SecretStore references any more, neither in git
// (fetched, never a cached snapshot) nor in the cluster.
type CredentialsReaper struct {
	adapter   *Adapter
	client    kubernetes.Interface
	referrers []credentialsReferrer
}

// NewCredentialsReaper returns the reaper of the credentials Secrets the
// Connections and the SecretStores of git release.
func NewCredentialsReaper(adapter *Adapter, client kubernetes.Interface, connections *ConnectionRepository, stores *SecretStoreRepository) *CredentialsReaper {
	r := &CredentialsReaper{adapter: adapter, client: client}
	if connections != nil {
		r.referrers = append(r.referrers, connections)
	}
	if stores != nil {
		r.referrers = append(r.referrers, stores)
	}
	return r
}

// Reap deletes the marked Secrets nothing references. Any doubt keeps the
// Secrets of the namespace: a failed fetch, a manifest that cannot be read,
// or objects of the cluster that cannot be listed. The deletion is
// conditioned on the Secret being the one read, so a Secret written again
// meanwhile (which drops the mark) stays.
func (r *CredentialsReaper) Reap(ctx context.Context) error {
	if len(r.referrers) == 0 {
		return errors.New("no referrer to check the credentials against")
	}
	secrets, err := r.client.CoreV1().Secrets("").List(ctx, metav1.ListOptions{
		LabelSelector: crd.LabelOrphanedCredentials + "=true," + crd.LabelManagedBy + "=" + crd.ManagedByValue,
	})
	if err != nil {
		return err
	}
	if len(secrets.Items) == 0 {
		return nil
	}
	snap, err := r.adapter.snapshots.strict(ctx)
	if err != nil {
		return fmt.Errorf("keeping the orphaned credentials, git cannot be read: %w", err)
	}
	references := map[string]map[string]bool{}
	for _, secret := range secrets.Items {
		ns := secret.Namespace
		if _, done := references[ns]; !done {
			refs, err := r.references(ctx, snap, ns)
			if err != nil {
				logrus.WithError(err).WithField("namespace", ns).Warn("Keeping the orphaned credentials of a namespace whose objects cannot be read")
				references[ns] = nil
				continue
			}
			references[ns] = refs
		}
		refs := references[ns]
		if refs == nil || refs[secret.Name] {
			continue
		}
		uid, rv := secret.UID, secret.ResourceVersion
		err := r.client.CoreV1().Secrets(ns).Delete(ctx, secret.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		})
		switch {
		case err == nil:
			logrus.WithField("secret", ns+"/"+secret.Name).Info("Deleted credentials nothing references any more")
		case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		default:
			logrus.WithError(err).WithField("secret", ns+"/"+secret.Name).Warn("Failed to delete orphaned credentials")
		}
	}
	return nil
}

func (r *CredentialsReaper) references(ctx context.Context, snap *gitstore.Snapshot, namespace string) (map[string]bool, error) {
	all := map[string]bool{}
	for _, referrer := range r.referrers {
		refs, err := referrer.credentialsReferences(ctx, snap, namespace)
		if err != nil {
			return nil, err
		}
		for name := range refs {
			all[name] = true
		}
	}
	return all, nil
}

// Run calls Reap every interval until ctx ends.
func (r *CredentialsReaper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.Reap(ctx); err != nil && ctx.Err() == nil {
			logrus.WithError(err).Warn("Cannot reap the orphaned credentials")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
