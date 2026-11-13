package repository

import (
	"fmt"
	"net/http"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// getK8sConfig returns the Kubernetes REST config (in-cluster or local kubeconfig).
func getK8sConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		configOverrides := &clientcmd.ConfigOverrides{}
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

		config, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get kubernetes config (tried in-cluster and local): %w", err)
		}
	}
	config.QPS = 50
	config.Burst = 100
	return config, nil
}

// callerK8sConfig returns a config whose requests act as the caller found in
// their context, not as the server's ServiceAccount.
func callerK8sConfig() (*rest.Config, error) {
	config, err := getK8sConfig()
	if err != nil {
		return nil, err
	}
	config.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return &impersonatingTransport{next: next}
	})
	return config, nil
}

// InitK8sClient initializes the Kubernetes dynamic client acting as the server.
func InitK8sClient() (dynamic.Interface, error) {
	return newDynamicClient(getK8sConfig)
}

// InitCallerK8sClient initializes the dynamic client acting as the caller.
func InitCallerK8sClient() (dynamic.Interface, error) {
	return newDynamicClient(callerK8sConfig)
}

func newDynamicClient(getConfig func() (*rest.Config, error)) (dynamic.Interface, error) {
	config, err := getConfig()
	if err != nil {
		return nil, err
	}

	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	return client, nil
}

// InitK8sDiscoveryClient initializes the discovery client, used to tell whether
// an optional CRD is installed on the cluster before trying to use it.
func InitK8sDiscoveryClient() (discovery.DiscoveryInterface, error) {
	config, err := getK8sConfig()
	if err != nil {
		return nil, err
	}

	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery client: %w", err)
	}

	return client, nil
}

// InitK8sTypedClient initializes the typed Kubernetes clientset acting as the server.
func InitK8sTypedClient() (kubernetes.Interface, error) {
	return newTypedClient(getK8sConfig)
}

// InitCallerK8sTypedClient initializes the typed clientset acting as the caller.
func InitCallerK8sTypedClient() (kubernetes.Interface, error) {
	return newTypedClient(callerK8sConfig)
}

func newTypedClient(getConfig func() (*rest.Config, error)) (kubernetes.Interface, error) {
	config, err := getConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create typed client: %w", err)
	}

	return clientset, nil
}
