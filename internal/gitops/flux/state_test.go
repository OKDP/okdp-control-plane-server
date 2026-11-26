package flux

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

func TestBareSHA(t *testing.T) {
	sha256 := shaA + "aaaaaaaaaaaaaaaaaaaaaaaa"
	for in, want := range map[string]string{
		"main@sha1:" + shaA:            shaA,
		"refs/heads/main@sha1:" + shaA: shaA,
		"sha1:" + shaA:                 shaA,
		"main/" + shaA:                 shaA,
		shaA:                           shaA,
		"main@sha256:" + sha256:        sha256,
		"":                             "",
		"main@sha1:" + shaA[:39]:       "",
		"main@sha1:" + shaA + "0":      "",
		"v1.0.0@sha1:XYZ":              "",
		"main":                         "",
	} {
		assert.Equal(t, want, bareSHA(in), in)
	}
}

// failingKustomization failed to apply attempted while applied stays the
// last revision applied, as kustomize-controller leaves it: Ready=False and
// Reconciling=True with reason ProgressingWithRetry.
func failingKustomization(t *testing.T, applied, attempted string) *unstructured.Unstructured {
	ks := kustomization(applied)
	set(t, ks, "main@sha1:"+attempted, "status", "lastAttemptedRevision")
	return setConditions(t, ks,
		cond(readyCondition, "False", "ReconciliationFailed", "Deployment/apps/web dry-run failed", 1),
		cond(reconcilingCondition, "True", "ProgressingWithRetry", "Detecting drift for revision main@sha1:"+attempted, 1),
	)
}

func TestComputeState(t *testing.T) {
	cases := []struct {
		name string
		repo func(t *testing.T) *unstructured.Unstructured
		ks   func(t *testing.T) *unstructured.Unstructured
		want gitops.SyncState
		msg  string
	}{
		{
			name: "applied what was fetched",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaA) },
			ks:   func(*testing.T) *unstructured.Unstructured { return kustomization(shaA) },
			want: gitops.SyncState{Phase: gitops.SyncReady, AppliedRevision: shaA, FetchedRevision: shaA},
		},
		{
			name: "fetched, not applied yet",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaB) },
			ks:   func(*testing.T) *unstructured.Unstructured { return kustomization(shaA) },
			want: gitops.SyncState{Phase: gitops.SyncProgressing, AppliedRevision: shaA, FetchedRevision: shaB},
			msg:  "has not applied it yet",
		},
		{
			name: "kustomization applying",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaB) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				return setConditions(t, kustomization(shaA),
					cond(readyCondition, "Unknown", "Progressing", "Reconciliation in progress", 1),
					cond(reconcilingCondition, "True", "Progressing", "Building manifests", 1))
			},
			want: gitops.SyncState{Phase: gitops.SyncProgressing, AppliedRevision: shaA, FetchedRevision: shaB},
			msg:  "Reconciliation in progress",
		},
		{
			name: "kustomization failed on a new commit",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaB) },
			ks:   func(t *testing.T) *unstructured.Unstructured { return failingKustomization(t, shaA, shaB) },
			want: gitops.SyncState{Phase: gitops.SyncFailed, AppliedRevision: shaA, FetchedRevision: shaB, AttemptedRevision: shaB},
			msg:  "Kustomization okdp/okdp-state: ReconciliationFailed: Deployment/apps/web dry-run failed",
		},
		{
			name: "kustomization failed after a spec change",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaB) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				ks := failingKustomization(t, shaA, shaB)
				ks.SetGeneration(2)
				return setConditions(t, ks,
					cond(readyCondition, "False", "BuildFailed", "kustomize build failed", 2),
					cond(reconcilingCondition, "True", "ProgressingWithRetry", "retrying", 2))
			},
			want: gitops.SyncState{Phase: gitops.SyncFailed, FetchedRevision: shaB, AttemptedRevision: shaB},
			msg:  "BuildFailed: kustomize build failed",
		},
		{
			name: "kustomization failed before attempting the new spec",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaB) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				ks := kustomization(shaA)
				ks.SetGeneration(2)
				return setConditions(t, ks,
					cond(readyCondition, "False", "ArtifactFailed", "kustomization path not found", 2),
					cond(reconcilingCondition, "True", "ProgressingWithRetry", "retrying", 2))
			},
			want: gitops.SyncState{Phase: gitops.SyncFailed, FetchedRevision: shaB},
			msg:  "ArtifactFailed: kustomization path not found",
		},
		{
			name: "stale reconciling does not hide a current failure",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaB) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				ks := failingKustomization(t, shaA, shaB)
				ks.SetGeneration(2)
				return setConditions(t, ks,
					cond(readyCondition, "False", "BuildFailed", "kustomize build failed", 2),
					cond(reconcilingCondition, "True", "Progressing", "Fetching manifests", 1))
			},
			want: gitops.SyncState{Phase: gitops.SyncFailed, FetchedRevision: shaB, AttemptedRevision: shaB},
			msg:  "BuildFailed",
		},
		{
			name: "ready condition without observed generation",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaA) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				c := readyCond(1)
				delete(c, "observedGeneration")
				return setConditions(t, kustomization(shaA), c)
			},
			want: gitops.SyncState{Phase: gitops.SyncProgressing, AppliedRevision: shaA, FetchedRevision: shaA},
			msg:  "generation 1 not observed yet",
		},
		{
			name: "kustomization waiting for a dependency",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaB) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				return setConditions(t, kustomization(shaA),
					cond(readyCondition, "False", "DependencyNotReady", "dependency 'okdp/infra' is not ready", 1))
			},
			want: gitops.SyncState{Phase: gitops.SyncProgressing, AppliedRevision: shaA, FetchedRevision: shaB},
			msg:  "is not ready",
		},
		{
			name: "kustomization retrying after a failure",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaB) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				return setConditions(t, failingKustomization(t, shaA, shaB),
					cond(readyCondition, "False", "ReconciliationFailed", "old failure", 1),
					cond(reconcilingCondition, "True", "Progressing", "Fetching manifests", 1))
			},
			want: gitops.SyncState{Phase: gitops.SyncProgressing, AppliedRevision: shaA, FetchedRevision: shaB},
		},
		{
			name: "gitrepository fetch failed",
			repo: func(t *testing.T) *unstructured.Unstructured {
				return setConditions(t, gitRepository(shaA),
					cond(readyCondition, "False", "GitOperationFailed", "authentication required", 1),
					cond("FetchFailed", "True", "GitOperationFailed", "authentication required", 1),
					cond(reconcilingCondition, "True", "ProgressingWithRetry", "retrying", 1))
			},
			ks:   func(*testing.T) *unstructured.Unstructured { return kustomization(shaA) },
			want: gitops.SyncState{Phase: gitops.SyncFailed, AppliedRevision: shaA, FetchedRevision: shaA},
			msg:  "GitRepository okdp/okdp-state: GitOperationFailed: authentication required",
		},
		{
			name: "both failed",
			repo: func(t *testing.T) *unstructured.Unstructured {
				return setConditions(t, gitRepository(shaB),
					cond(readyCondition, "False", "GitOperationFailed", "timeout", 1))
			},
			ks:   func(t *testing.T) *unstructured.Unstructured { return failingKustomization(t, shaA, shaB) },
			want: gitops.SyncState{Phase: gitops.SyncFailed, AppliedRevision: shaA, FetchedRevision: shaB, AttemptedRevision: shaB},
			msg:  "GitRepository okdp/okdp-state: GitOperationFailed: timeout. Kustomization okdp/okdp-state",
		},
		{
			name: "stale kustomization status",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaA) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				ks := kustomization(shaA)
				ks.SetGeneration(2)
				return ks
			},
			want: gitops.SyncState{Phase: gitops.SyncProgressing, FetchedRevision: shaA},
			msg:  "generation 2 not observed yet",
		},
		{
			name: "stale gitrepository status",
			repo: func(*testing.T) *unstructured.Unstructured {
				repo := gitRepository(shaB)
				repo.SetGeneration(3)
				return repo
			},
			ks:   func(*testing.T) *unstructured.Unstructured { return kustomization(shaA) },
			want: gitops.SyncState{Phase: gitops.SyncProgressing, AppliedRevision: shaA},
			msg:  "GitRepository okdp/okdp-state: generation 3 not observed yet",
		},
		{
			name: "ready condition current but status generation behind",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaA) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				ks := kustomization(shaA)
				ks.SetGeneration(2)
				return setConditions(t, ks, readyCond(2))
			},
			want: gitops.SyncState{Phase: gitops.SyncProgressing, FetchedRevision: shaA},
		},
		{
			name: "nothing applied yet",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaA) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				ks := kustomization(shaA)
				unstructured.RemoveNestedField(ks.Object, "status")
				return ks
			},
			want: gitops.SyncState{Phase: gitops.SyncProgressing, FetchedRevision: shaA},
			msg:  "no Ready condition yet",
		},
		{
			name: "kustomization suspended",
			repo: func(*testing.T) *unstructured.Unstructured { return gitRepository(shaB) },
			ks: func(t *testing.T) *unstructured.Unstructured {
				return set(t, kustomization(shaA), true, "spec", "suspend")
			},
			want: gitops.SyncState{Phase: gitops.SyncSuspended, AppliedRevision: shaA, FetchedRevision: shaB},
			msg:  "Kustomization okdp/okdp-state suspended",
		},
		{
			name: "gitrepository suspended while the kustomization failed",
			repo: func(t *testing.T) *unstructured.Unstructured {
				return set(t, gitRepository(shaB), true, "spec", "suspend")
			},
			ks:   func(t *testing.T) *unstructured.Unstructured { return failingKustomization(t, shaA, shaB) },
			want: gitops.SyncState{Phase: gitops.SyncSuspended, AppliedRevision: shaA, FetchedRevision: shaB},
			msg:  "GitRepository okdp/okdp-state suspended",
		},
		{
			name: "missing gitrepository",
			repo: func(*testing.T) *unstructured.Unstructured { return nil },
			ks:   func(*testing.T) *unstructured.Unstructured { return kustomization(shaA) },
			want: gitops.SyncState{Phase: gitops.SyncUnknown},
			msg:  "GitRepository okdp/okdp-state not found",
		},
	}
	a := newAdapter(t, newClient())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := a.computeState(tc.repo(t), tc.ks(t), t0)
			assert.Equal(t, t0, got.ObservedAt)
			assert.Contains(t, got.Message, tc.msg)
			got.Message, got.ObservedAt = "", tc.want.ObservedAt
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestObserveSyncReadsTheCluster(t *testing.T) {
	a := newAdapter(t, newClient(gitRepository(shaB), failingKustomization(t, shaA, shaB)))
	st, err := a.ObserveSync(context.Background())
	require.NoError(t, err)
	assert.Equal(t, gitops.SyncFailed, st.Phase)
	assert.Equal(t, shaA, st.AppliedRevision)
	assert.Equal(t, shaB, st.AttemptedRevision)
	assert.Equal(t, t0, st.ObservedAt)
}

func TestObserveSyncReportsAMissingObjectAsUnknown(t *testing.T) {
	st, err := newAdapter(t, newClient(gitRepository(shaA))).ObserveSync(context.Background())
	require.NoError(t, err)
	assert.Equal(t, gitops.SyncUnknown, st.Phase)
	assert.Contains(t, st.Message, "Kustomization okdp/okdp-state not found")
}

func TestObserveSyncReturnsAReadError(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	forbidden := errors.New("forbidden")
	client.PrependReactor("get", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbidden
	})
	st, err := newAdapter(t, client).ObserveSync(context.Background())
	assert.ErrorIs(t, err, forbidden)
	assert.Equal(t, gitops.SyncUnknown, st.Phase)
	assert.Contains(t, st.Message, "forbidden")
}
