package flux

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

// Flux condition types and reasons (github.com/fluxcd/pkg/apis/meta).
const (
	readyCondition       = "Ready"
	reconcilingCondition = "Reconciling"
	progressingReason    = "Progressing"
	dependencyNotReady   = "DependencyNotReady"
)

// preAttemptReasons are the Ready=False reasons kustomize-controller sets
// before it records lastAttemptedRevision, which then still names an
// earlier attempt.
var preAttemptReasons = map[string]bool{
	"ArtifactFailed":       true,
	"AccessDenied":         true,
	"InvalidCELExpression": true,
	"DirCreationFailed":    true,
	dependencyNotReady:     true,
}

// objectPhase is where one Flux object stands for its current generation.
type objectPhase int

const (
	objectReady objectPhase = iota
	objectProgressing
	objectFailed
)

type condition struct {
	status, reason, message string
	observedGeneration      int64
	found                   bool
}

func getCondition(obj *unstructured.Unstructured, typ string) condition {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != typ {
			continue
		}
		out := condition{found: true}
		out.status, _ = m["status"].(string)
		out.reason, _ = m["reason"].(string)
		out.message, _ = m["message"].(string)
		out.observedGeneration, _, _ = unstructured.NestedInt64(m, "observedGeneration")
		return out
	}
	return condition{}
}

// statusIsCurrent tells whether status.observedGeneration matches the spec.
// Flux advances it only when a reconciliation of that spec succeeds.
func statusIsCurrent(obj *unstructured.Unstructured) bool {
	observed, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	return observed == obj.GetGeneration()
}

// phaseOf reads the conditions Flux sets on a GitRepository or a
// Kustomization. An attempt in progress shows Ready=Unknown and
// Reconciling=True with reason Progressing. A failed attempt shows
// Ready=False, with Reconciling=True and reason ProgressingWithRetry while
// Flux retries, so Ready=False is a failure even when Reconciling is True.
//
// A condition counts only when its observedGeneration is the current one.
func phaseOf(obj *unstructured.Unstructured) (objectPhase, string) {
	gen := obj.GetGeneration()
	ready := getCondition(obj, readyCondition)
	reconciling := getCondition(obj, reconcilingCondition)
	if reconciling.observedGeneration != gen {
		reconciling = condition{}
	}
	switch {
	case !ready.found:
		return objectProgressing, "no Ready condition yet"
	case ready.observedGeneration != gen:
		return objectProgressing, fmt.Sprintf("generation %d not observed yet", gen)
	case ready.status == "False":
		if ready.reason == dependencyNotReady ||
			(reconciling.status == "True" && reconciling.reason == progressingReason) {
			return objectProgressing, ready.message
		}
		return objectFailed, joinReason(ready.reason, ready.message)
	case ready.status != "True":
		return objectProgressing, ready.message
	case reconciling.status == "True":
		return objectProgressing, reconciling.message
	case !statusIsCurrent(obj):
		return objectProgressing, fmt.Sprintf("generation %d not observed yet", gen)
	}
	return objectReady, ready.message
}

func joinReason(reason, message string) string {
	switch {
	case reason == "":
		return message
	case message == "":
		return reason
	}
	return reason + ": " + message
}

func suspended(obj *unstructured.Unstructured) bool {
	s, _, _ := unstructured.NestedBool(obj.Object, "spec", "suspend")
	return s
}

// revisionField returns the bare SHA of a revision field.
func revisionField(obj *unstructured.Unstructured, fields ...string) string {
	rev, _, _ := unstructured.NestedString(obj.Object, fields...)
	return bareSHA(rev)
}

// bareSHA extracts the commit SHA from a Flux revision: "main@sha1:<sha>",
// "refs/heads/main@sha1:<sha>", "sha1:<sha>", the "main/<sha>" form of older
// Flux APIs, or a bare SHA. It returns "" for anything else.
func bareSHA(rev string) string {
	for _, algo := range []struct {
		prefix string
		size   int
	}{{"sha1:", 40}, {"sha256:", 64}} {
		if i := strings.LastIndex(rev, algo.prefix); i >= 0 {
			if s := rev[i+len(algo.prefix):]; isHex(s, algo.size) {
				return s
			}
			return ""
		}
	}
	s := rev[strings.LastIndex(rev, "/")+1:]
	if isHex(s, 40) || isHex(s, 64) {
		return s
	}
	return ""
}

func isHex(s string, size int) bool {
	if len(s) != size {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// computeState maps the two Flux objects to a SyncState. A nil object is
// one that does not exist. The phases are checked in this order: Unknown
// (an object is missing), Suspended, Failed (GitRepository first), then
// Progressing until the Kustomization applied what the GitRepository
// fetched, then Ready.
func (a *Adapter) computeState(repo, ks *unstructured.Unstructured, now time.Time) gitops.SyncState {
	st := gitops.SyncState{ObservedAt: now}
	var missing []string
	if repo == nil {
		missing = append(missing, a.repoRef())
	}
	if ks == nil {
		missing = append(missing, a.ksRef())
	}
	if len(missing) > 0 {
		st.Phase = gitops.SyncUnknown
		st.Message = strings.Join(missing, " and ") + " not found"
		return st
	}

	if statusIsCurrent(repo) {
		st.FetchedRevision = revisionField(repo, "status", "artifact", "revision")
	}
	if statusIsCurrent(ks) {
		st.AppliedRevision = revisionField(ks, "status", "lastAppliedRevision")
	}

	var paused []string
	if suspended(repo) {
		paused = append(paused, a.repoRef())
	}
	if suspended(ks) {
		paused = append(paused, a.ksRef())
	}
	if len(paused) > 0 {
		st.Phase = gitops.SyncSuspended
		st.Message = strings.Join(paused, " and ") + " suspended"
		return st
	}

	repoPhase, repoMsg := phaseOf(repo)
	ksPhase, ksMsg := phaseOf(ks)

	var failures []string
	if repoPhase == objectFailed {
		failures = append(failures, a.repoRef()+": "+repoMsg)
	}
	if ksPhase == objectFailed {
		failures = append(failures, a.ksRef()+": "+ksMsg)
		if !preAttemptReasons[getCondition(ks, readyCondition).reason] {
			st.AttemptedRevision = revisionField(ks, "status", "lastAttemptedRevision")
		}
	}
	if len(failures) > 0 {
		st.Phase = gitops.SyncFailed
		st.Message = strings.Join(failures, ". ")
		return st
	}

	st.Phase = gitops.SyncProgressing
	switch {
	case repoPhase == objectProgressing:
		st.Message = a.repoRef() + ": " + repoMsg
	case ksPhase == objectProgressing:
		st.Message = a.ksRef() + ": " + ksMsg
	case st.AppliedRevision == "":
		st.Message = a.ksRef() + " has applied nothing yet"
	case st.FetchedRevision != "" && st.FetchedRevision != st.AppliedRevision:
		st.Message = fmt.Sprintf("%s fetched %s, %s has not applied it yet",
			a.repoRef(), st.FetchedRevision, a.ksRef())
	default:
		st.Phase = gitops.SyncReady
		st.Message = ""
	}
	return st
}
