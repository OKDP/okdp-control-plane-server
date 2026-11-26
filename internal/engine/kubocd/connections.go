package kubocd

import (
	"context"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// connection is what the binding states need to know of a Connection or a
// ClusterConnection.
type connection struct {
	// managed: published by the KuboCD release controller (spec.outputName set).
	managed bool
	// parent names the Release publishing it, in the Connection's namespace.
	parent string
}

// connectionIndex holds the connections and Release phases of one namespace,
// read once for every instance of a request.
type connectionIndex struct {
	namespaced map[string]connection
	cluster    map[string]connection
	phases     map[string]string
}

// indexConnections reads the namespace's Connections, the ClusterConnections,
// and takes the Release phases from releases. A list that fails (CRDs absent,
// no permission) leaves its part empty: the states degrade, the request does not.
func indexConnections(ctx context.Context, cluster dynamic.Interface, namespace string, releases []crd.Release) connectionIndex {
	ix := connectionIndex{
		namespaced: listConnections(ctx, cluster, crd.GetConnectionGVR(), namespace),
		cluster:    listConnections(ctx, cluster, crd.GetClusterConnectionGVR(), ""),
		phases:     make(map[string]string, len(releases)),
	}
	for i := range releases {
		ix.phases[releases[i].Name] = releases[i].Status.Phase
	}
	return ix
}

func listConnections(ctx context.Context, cluster dynamic.Interface, gvr schema.GroupVersionResource, namespace string) map[string]connection {
	out := map[string]connection{}
	resource := cluster.Resource(gvr)
	var (
		list *unstructured.UnstructuredList
		err  error
	)
	if namespace == "" {
		list, err = resource.List(ctx, metav1.ListOptions{})
	} else {
		list, err = resource.Namespace(namespace).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		logrus.WithError(err).WithField("resource", gvr.Resource).Debug("Connection states unavailable")
		return out
	}
	for _, item := range list.Items {
		outputName, _, _ := unstructured.NestedString(item.Object, "spec", "outputName")
		parent, _, _ := unstructured.NestedString(item.Object, "status", "parent")
		out[item.GetName()] = connection{managed: outputName != "", parent: parent}
	}
	return out
}

func (ix connectionIndex) state(b engine.ConnectionBinding) engine.ConnectionState {
	var (
		c      connection
		exists bool
	)
	if b.Kind == crd.ClusterConnectionKind {
		c, exists = ix.cluster[b.Name]
	} else {
		c, exists = ix.namespaced[b.Name]
	}
	switch {
	case b.Bound && exists && c.managed && c.parent != "" && ix.phases[c.parent] == "READY":
		return engine.ConnectionReady
	case b.Bound:
		return engine.ConnectionBound
	case !exists:
		return ""
	case c.managed:
		return engine.ConnectionPublished
	default:
		return engine.ConnectionDeclared
	}
}

// withConnectionStates fills the binding states of views, all in namespace.
// Nothing is read when no view has a binding.
func (a *Adapter) withConnectionStates(ctx context.Context, namespace string, views []engine.InstanceView, releases []crd.Release) {
	hasBindings := false
	for i := range views {
		hasBindings = hasBindings || len(views[i].Bindings) > 0
	}
	if !hasBindings {
		return
	}
	if releases == nil {
		listed, err := a.releases.List(ctx, namespace, namespace)
		if err != nil {
			logrus.WithError(err).Debug("Connection providers unavailable")
		}
		releases = listed
	}
	FillConnectionStates(ctx, a.cluster, namespace, views, releases)
}

// FillConnectionStates fills the binding states of views, all in namespace,
// from the namespace's Connections, the ClusterConnections and the phases of
// releases, the Releases of namespace.
func FillConnectionStates(ctx context.Context, cluster dynamic.Interface, namespace string, views []engine.InstanceView, releases []crd.Release) {
	ix := indexConnections(ctx, cluster, namespace, releases)
	for i := range views {
		for j := range views[i].Bindings {
			views[i].Bindings[j].State = ix.state(views[i].Bindings[j])
		}
	}
}
