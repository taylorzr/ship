package k8s

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type RealClient struct {
	clientset kubernetes.Interface
	context   string
}

func NewRealClient(kubeconfig, context string) (*RealClient, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	configOverrides := &clientcmd.ConfigOverrides{CurrentContext: context}
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	rawCfg, err := cfg.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}

	actualCtx := rawCfg.CurrentContext
	if context != "" {
		actualCtx = context
	}
	if actualCtx == "" {
		return nil, fmt.Errorf("kubeconfig: no context set — run kubectl config use-context <ctx> or set context in config.toml")
	}

	restCfg, err := cfg.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig context %q: %w", actualCtx, err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client for %q: %w", actualCtx, err)
	}
	return &RealClient{clientset: clientset, context: actualCtx}, nil
}

func (c *RealClient) GetDeployment(ctx context.Context, context, namespace, name string) (*Deployment, error) {
	dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		msg := fmt.Sprintf("k8s %s: get deployment %s/%s", c.context, namespace, name)
		if strings.Contains(err.Error(), "not found") {
			msg += " — deployment may not exist, or your account may lack RBAC read access"
		} else if strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "forbidden") {
			msg += " — token may be expired or missing RBAC permissions"
		}
		return nil, fmt.Errorf("%s: %w", msg, err)
	}

	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("no containers in deployment %s/%s", namespace, name)
	}

	image := dep.Spec.Template.Spec.Containers[0].Image
	return &Deployment{
		Image:     image,
		Container: dep.Spec.Template.Spec.Containers[0].Name,
	}, nil
}
