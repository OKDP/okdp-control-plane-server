package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

type SparkAppRepository interface {
	Create(ctx context.Context, namespace string, app *crd.SparkApplication) error
	CreateRaw(ctx context.Context, namespace string, obj *unstructured.Unstructured) error
	Get(ctx context.Context, namespace, name string) (*crd.SparkApplication, error)
	Update(ctx context.Context, namespace string, app *crd.SparkApplication) error
	List(ctx context.Context, namespace, project string) ([]crd.SparkApplication, error)
	Delete(ctx context.Context, namespace, name string) error
	Watch(ctx context.Context, namespace, project string) (watch.Interface, error)
	GetCRDSchema(ctx context.Context) (map[string]interface{}, error)
}

type k8sSparkAppRepository struct {
	client dynamic.Interface
	gvr    schema.GroupVersionResource
}

func NewSparkAppRepository(client dynamic.Interface) SparkAppRepository {
	return &k8sSparkAppRepository{
		client: client,
		gvr:    crd.GetSparkAppGVR(),
	}
}

func (r *k8sSparkAppRepository) Create(ctx context.Context, namespace string, app *crd.SparkApplication) error {
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(app)
	if err != nil {
		return fmt.Errorf("failed to convert SparkApplication to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.gvr).Namespace(namespace).Create(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.CreateOptions{},
	)
	return err
}

func (r *k8sSparkAppRepository) CreateRaw(ctx context.Context, namespace string, obj *unstructured.Unstructured) error {
	obj.SetNamespace(namespace)
	_, err := r.client.Resource(r.gvr).Namespace(namespace).Create(ctx, obj, metav1.CreateOptions{})
	return err
}

func (r *k8sSparkAppRepository) Update(ctx context.Context, namespace string, app *crd.SparkApplication) error {
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(app)
	if err != nil {
		return fmt.Errorf("failed to convert SparkApplication to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.gvr).Namespace(namespace).Update(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.UpdateOptions{},
	)
	return err
}

func (r *k8sSparkAppRepository) Get(ctx context.Context, namespace, name string) (*crd.SparkApplication, error) {
	u, err := r.client.Resource(r.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var app crd.SparkApplication
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &app); err != nil {
		return nil, fmt.Errorf("failed to convert from unstructured: %w", err)
	}
	return &app, nil
}

func (r *k8sSparkAppRepository) List(ctx context.Context, namespace, project string) ([]crd.SparkApplication, error) {
	opts := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", crd.LabelProject, project),
	}

	list, err := r.client.Resource(r.gvr).Namespace(namespace).List(ctx, opts)
	if err != nil {
		if isCRDNotInstalled(err) {
			logrus.Warn("SparkApplication CRD not installed on cluster, returning empty list")
			return []crd.SparkApplication{}, nil
		}
		return nil, err
	}

	var apps []crd.SparkApplication
	for _, item := range list.Items {
		var app crd.SparkApplication
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &app); err != nil {
			logrus.WithError(err).Warn("Failed to convert SparkApplication from unstructured")
			continue
		}
		apps = append(apps, app)
	}
	return apps, nil
}

func (r *k8sSparkAppRepository) Delete(ctx context.Context, namespace, name string) error {
	return r.client.Resource(r.gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (r *k8sSparkAppRepository) Watch(ctx context.Context, namespace, project string) (watch.Interface, error) {
	opts := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", crd.LabelProject, project),
	}
	w, err := r.client.Resource(r.gvr).Namespace(namespace).Watch(ctx, opts)
	if err != nil {
		if isCRDNotInstalled(err) {
			logrus.Warn("SparkApplication CRD not installed on cluster, returning empty watcher")
			w := watch.NewFake()
			w.Stop()
			return w, nil
		}
		return nil, err
	}
	return w, nil
}

func (r *k8sSparkAppRepository) GetCRDSchema(ctx context.Context) (map[string]interface{}, error) {
	crdGVR := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}

	u, err := r.client.Resource(crdGVR).Get(ctx, "sparkapplications.sparkoperator.k8s.io", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("SparkApplication CRD not found on cluster: %w", err)
	}

	versions, found, _ := unstructured.NestedSlice(u.Object, "spec", "versions")
	if !found || len(versions) == 0 {
		return nil, fmt.Errorf("no versions found in SparkApplication CRD")
	}

	firstVersion, ok := versions[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid version format in CRD")
	}

	specSchema, found, _ := unstructured.NestedMap(firstVersion, "schema", "openAPIV3Schema", "properties", "spec", "properties")
	if !found {
		return nil, fmt.Errorf("could not extract spec schema from CRD")
	}

	return specSchema, nil
}

func isCRDNotInstalled(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "the server could not find the requested resource") ||
		strings.Contains(msg, "no matches for kind")
}
