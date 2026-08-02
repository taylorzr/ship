package k8s

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type RealClient struct {
	clientset kubernetes.Interface
	context   string
	loginCmd  string
}

func NewRealClient(ctx context.Context, kubeconfig, context, loginCmd string) (*RealClient, error) {
	type result struct {
		client *RealClient
		err    error
	}

	ch := make(chan result, 1)
	go func() {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		if kubeconfig != "" {
			loadingRules.ExplicitPath = kubeconfig
		}
		configOverrides := &clientcmd.ConfigOverrides{CurrentContext: context}
		cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

		rawCfg, err := cfg.RawConfig()
		if err != nil {
			ch <- result{nil, fmt.Errorf("kubeconfig: %w", err)}
			return
		}

		actualCtx := rawCfg.CurrentContext
		if context != "" {
			actualCtx = context
		}
		if actualCtx == "" {
			ch <- result{nil, fmt.Errorf("kubeconfig: no context set — run kubectl config use-context <ctx> or set context in config.toml")}
			return
		}

		restCfg, err := cfg.ClientConfig()
		if err != nil {
			ch <- result{nil, fmt.Errorf("kubeconfig context %q: %w", actualCtx, err)}
			return
		}

		clientset, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			ch <- result{nil, fmt.Errorf("kubernetes client for %q: %w", actualCtx, err)}
			return
		}
		ch <- result{&RealClient{clientset: clientset, context: actualCtx, loginCmd: loginCmd}, nil}
	}()

	select {
	case r := <-ch:
		return r.client, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *RealClient) GetDeployment(ctx context.Context, context, namespace, name string) (*Deployment, error) {
	dep, err := c.getDeployment(ctx, namespace, name)
	if err != nil && c.loginCmd != "" && isAuthErr(err) {
		if lerr := relogin(c.loginCmd); lerr != nil {
			err = fmt.Errorf("%w (login failed: %v)", err, lerr)
		} else {
			dep, err = c.getDeployment(ctx, namespace, name)
		}
	}
	if err != nil {
		msg := fmt.Sprintf("k8s %s: get deployment %s/%s", c.context, namespace, name)
		if strings.Contains(err.Error(), "not found") {
			msg += " — deployment may not exist, or your account may lack RBAC read access"
		} else if strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "forbidden") {
			msg += " — token may be expired or missing RBAC permissions"
		}
		return nil, fmt.Errorf("%s: %w", msg, err)
	}
	return dep, nil
}

func (c *RealClient) getDeployment(ctx context.Context, namespace, name string) (*Deployment, error) {
	dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("no containers in deployment %s/%s", namespace, name)
	}

	image := dep.Spec.Template.Spec.Containers[0].Image
	d := &Deployment{
		Image:     image,
		Container: dep.Spec.Template.Spec.Containers[0].Name,
		Health: Health{
			Ready:         dep.Status.ReadyReplicas == dep.Status.Replicas && dep.Status.Replicas > 0,
			ReadyReplicas: dep.Status.ReadyReplicas,
			Replicas:      dep.Status.Replicas,
		},
	}
	c.collectHealth(ctx, namespace, name, dep, d)
	return d, nil
}

// collectHealth augments the deployment with pod restart counts and recent
// warning events (OOM kills, backoff, failed scheduling, ...).
func (c *RealClient) collectHealth(ctx context.Context, namespace, name string, dep *appsv1.Deployment, d *Deployment) {
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return
	}
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return
	}

	podNames := make(map[string]bool, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		podNames[pod.Name] = true
		switch pod.Status.Phase {
		case corev1.PodPending:
			d.Health.PendingPods++
		case corev1.PodFailed:
			d.Health.FailedPods++
		}
		for _, cs := range pod.Status.ContainerStatuses {
			d.Health.Restarts += cs.RestartCount
		}
	}

	// deployment-level conditions: stuck rollouts and replica failures are
	// not visible via ready replicas alone.
	for _, cond := range dep.Status.Conditions {
		switch {
		case cond.Type == appsv1.DeploymentProgressing && cond.Reason == "ProgressDeadlineExceeded":
			d.Health.Conditions = append(d.Health.Conditions, "ProgressDeadlineExceeded")
		case cond.Type == appsv1.DeploymentReplicaFailure && cond.Status == corev1.ConditionTrue:
			d.Health.Conditions = append(d.Health.Conditions, "ReplicaFailure")
		case cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionFalse:
			d.Health.Conditions = append(d.Health.Conditions, "Unavailable")
		}
	}

	events, err := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	cutoff := metav1.NewTime(metav1.Now().Add(-24 * time.Hour))
	seen := map[string]bool{}
	for i := range events.Items {
		ev := &events.Items[i]
		if ev.Type != "Warning" {
			continue
		}
		if ev.LastTimestamp.Before(&cutoff) {
			continue
		}
		obj := ev.InvolvedObject
		if obj.Kind == "Deployment" && obj.Name == name {
			// deployment-level warning (e.g. FailedRollout)
		} else if obj.Kind == "Pod" && podNames[obj.Name] {
			// pod-level warning (e.g. OOMKilled)
		} else {
			continue
		}
		reason := ev.Reason
		if reason == "" {
			continue
		}
		if seen[reason] {
			continue
		}
		seen[reason] = true
		d.Health.Events = append(d.Health.Events, reason)
	}
}

// isAuthErr reports whether err looks like an expired/invalid credential.
// Besides plain 401s, exec credential plugin failures (e.g. `aws eks
// get-token` with an expired SSO token) surface as transport errors.
func isAuthErr(err error) bool {
	if apierrors.IsUnauthorized(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Unauthorized") || strings.Contains(msg, "getting credentials")
}

// loginMu serializes login attempts across the per-service clients created
// during a refresh, and lastLogin lets concurrent callers that arrive right
// after a successful login skip straight to retrying their request.
var (
	loginMu   sync.Mutex
	lastLogin time.Time
)

// LastLogin returns the time of the last successful auto-login, or the zero
// time if none has happened.
func LastLogin() time.Time {
	loginMu.Lock()
	defer loginMu.Unlock()
	return lastLogin
}

func relogin(loginCmd string) error {
	loginMu.Lock()
	defer loginMu.Unlock()

	if time.Since(lastLogin) < time.Minute {
		return nil
	}

	parts := strings.Fields(loginCmd)
	if len(parts) == 0 {
		return fmt.Errorf("k8s: login_command is empty")
	}

	// Deliberately not tied to the caller's short request context — an
	// interactive login (browser SSO) can take minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, parts[0], parts[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("k8s login (%s): %s: %w", loginCmd, strings.TrimSpace(string(out)), err)
	}
	lastLogin = time.Now()
	return nil
}
