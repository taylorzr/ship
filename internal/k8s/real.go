package k8s

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type RealClient struct {
	clientset kubernetes.Interface
}

func NewRealClient(kubeconfig, context string) (*RealClient, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	configOverrides := &clientcmd.ConfigOverrides{CurrentContext: context}
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restCfg, err := cfg.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return &RealClient{clientset: clientset}, nil
}

func (c *RealClient) GetDeployment(ctx context.Context, context, namespace, name string) (*Deployment, error) {
	dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
	}

	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("no containers in deployment %s", name)
	}

	image := dep.Spec.Template.Spec.Containers[0].Image
	return &Deployment{
		Image:     image,
		Container: dep.Spec.Template.Spec.Containers[0].Name,
	}, nil
}
