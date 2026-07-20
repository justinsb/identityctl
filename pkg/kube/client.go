package kube

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Client wraps a Kubernetes clientset built from the local kubeconfig.
type Client struct {
	clientset   *kubernetes.Clientset
	contextName string
}

// NewClient builds a client from the default kubeconfig loading rules.
// If kubecontext is empty, the current context is used.
func NewClient(kubecontext string) (*Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubecontext}
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	rawConfig, err := clientConfig.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	contextName := kubecontext
	if contextName == "" {
		contextName = rawConfig.CurrentContext
	}

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	return &Client{clientset: clientset, contextName: contextName}, nil
}

// ContextName returns the name of the kubeconfig context in use.
func (c *Client) ContextName() string {
	return c.contextName
}

// OIDCDiscovery holds the fields we need from the cluster's OIDC discovery document.
type OIDCDiscovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// Discover fetches the cluster's OIDC discovery document.
func (c *Client) Discover(ctx context.Context) (*OIDCDiscovery, error) {
	body, err := c.clientset.Discovery().RESTClient().Get().
		AbsPath("/.well-known/openid-configuration").Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("fetching cluster OIDC discovery document: %w", err)
	}
	discovery := &OIDCDiscovery{}
	if err := json.Unmarshal(body, discovery); err != nil {
		return nil, fmt.Errorf("parsing OIDC discovery document: %w", err)
	}
	if discovery.Issuer == "" {
		return nil, fmt.Errorf("cluster OIDC discovery document has no issuer")
	}
	return discovery, nil
}

// JWKS fetches the cluster's OIDC signing keys (JSON Web Key Set).
func (c *Client) JWKS(ctx context.Context) ([]byte, error) {
	body, err := c.clientset.Discovery().RESTClient().Get().
		AbsPath("/openid/v1/jwks").Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("fetching cluster JWKS: %w", err)
	}
	return body, nil
}

// ApplyConfigMap creates or updates a ConfigMap.
func (c *Client) ApplyConfigMap(ctx context.Context, namespace, name string, data map[string]string) error {
	configMaps := c.clientset.CoreV1().ConfigMaps(namespace)
	existing, err := configMaps.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err := configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Data:       data,
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating configmap %s/%s: %w", namespace, name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting configmap %s/%s: %w", namespace, name, err)
	}
	existing.Data = data
	if _, err := configMaps.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating configmap %s/%s: %w", namespace, name, err)
	}
	return nil
}
