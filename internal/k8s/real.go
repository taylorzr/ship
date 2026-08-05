package k8s

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// inflight counts k8s API requests across all per-service clients so the TUI
// can show live request activity in its footer.
var inflight atomic.Int64

// InFlight reports how many k8s API requests are currently in flight.
func InFlight() int64 { return inflight.Load() }

type countingRoundTripper struct {
	base http.RoundTripper
}

func (t *countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	inflight.Add(1)
	defer inflight.Add(-1)
	return t.base.RoundTrip(r)
}

type RealClient struct {
	clientset     kubernetes.Interface
	dynamicClient dynamic.Interface
	context       string
	loginCmd      string
	timebox       EventTimebox
}

func NewRealClient(ctx context.Context, kubeconfig, context, loginCmd string, tb EventTimebox) (*RealClient, error) {
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
		// Count API requests through both the typed and dynamic clients so
		// the TUI footer can show live k8s request activity.
		restCfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
			return &countingRoundTripper{base: rt}
		}

		clientset, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			ch <- result{nil, fmt.Errorf("kubernetes client for %q: %w", actualCtx, err)}
			return
		}
		dyn, err := dynamic.NewForConfig(restCfg)
		if err != nil {
			ch <- result{nil, fmt.Errorf("dynamic client for %q: %w", actualCtx, err)}
			return
		}
		ch <- result{&RealClient{clientset: clientset, dynamicClient: dyn, context: actualCtx, loginCmd: loginCmd, timebox: tb.Normalized()}, nil}
	}()

	select {
	case r := <-ch:
		return r.client, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *RealClient) GetWorkload(ctx context.Context, context, namespace, name, resource string) (*Workload, error) {
	var (
		dep *Workload
		err error
	)
	if resource == "" {
		resource = "deployment"
	}
	switch resource {
	case "rollout":
		dep, err = c.getRollout(ctx, namespace, name)
	default:
		dep, err = c.getDeployment(ctx, namespace, name)
	}
	if err != nil && c.loginCmd != "" && isAuthErr(err) {
		if lerr := relogin(c.loginCmd); lerr != nil {
			err = fmt.Errorf("%w (login failed: %v)", err, lerr)
		} else {
			if resource == "rollout" {
				dep, err = c.getRollout(ctx, namespace, name)
			} else {
				dep, err = c.getDeployment(ctx, namespace, name)
			}
		}
	}
	if err != nil {
		msg := fmt.Sprintf("k8s %s: get %s %s/%s", c.context, resource, namespace, name)
		if strings.Contains(err.Error(), "not found") {
			msg += " — workload may not exist, or your account may lack RBAC read access"
		} else if strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "forbidden") {
			msg += " — token may be expired or missing RBAC permissions"
		}
		return nil, fmt.Errorf("%s: %w", msg, err)
	}
	return dep, nil
}

func (c *RealClient) getDeployment(ctx context.Context, namespace, name string) (*Workload, error) {
	dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("no containers in deployment %s/%s", namespace, name)
	}

	d := &Workload{
		Kind:      "deployment",
		Image:     dep.Spec.Template.Spec.Containers[0].Image,
		Container: dep.Spec.Template.Spec.Containers[0].Name,
		Health: Health{
			Ready:         dep.Status.ReadyReplicas == dep.Status.Replicas && dep.Status.Replicas > 0,
			ReadyReplicas: dep.Status.ReadyReplicas,
			Replicas:      dep.Status.Replicas,
		},
	}
	// deployment-level conditions: stuck rollouts and replica failures are
	// not visible via ready replicas alone.
	for _, cond := range dep.Status.Conditions {
		switch {
		case cond.Type == appsv1.DeploymentProgressing && cond.Reason == "ProgressDeadlineExceeded":
			d.Health.Conditions = append(d.Health.Conditions, "ProgressDeadlineExceeded")
		case cond.Type == appsv1.DeploymentProgressing && cond.Reason == "DeploymentPaused":
			d.Health.Paused = true
		case cond.Type == appsv1.DeploymentProgressing && cond.Status == corev1.ConditionTrue && progressingReasons[cond.Reason]:
			d.Health.Progressing = true
		case cond.Type == appsv1.DeploymentReplicaFailure && cond.Status == corev1.ConditionTrue:
			d.Health.Conditions = append(d.Health.Conditions, "ReplicaFailure")
		case cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionFalse:
			d.Health.Conditions = append(d.Health.Conditions, "Unavailable")
		}
	}
	c.collectHealth(ctx, namespace, name, dep.Spec.Selector, "Deployment", &d.Health)
	return d, nil
}

// rolloutGVR identifies Argo Rollouts CRDs, accessed via the dynamic client
// so we don't pull in the argo-rollouts module.
var rolloutGVR = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "rollouts",
}

// rollout is the subset of the Argo Rollout CRD that ship reads.
type rollout struct {
	Spec struct {
		Selector *metav1.LabelSelector `json:"selector"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas      int32              `json:"replicas"`
		ReadyReplicas int32              `json:"readyReplicas"`
		StableRS      string             `json:"stableRS"`
		Conditions    []metav1.Condition `json:"conditions"`
	} `json:"status"`
}

func (c *RealClient) getRollout(ctx context.Context, namespace, name string) (*Workload, error) {
	u, err := c.dynamicClient.Resource(rolloutGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var ro rollout
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ro); err != nil {
		return nil, fmt.Errorf("decode rollout %s/%s: %w", namespace, name, err)
	}
	if len(ro.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("no containers in rollout %s/%s", namespace, name)
	}

	// The rollout spec template is the *canary target*. The stable (prod)
	// image lives in the ReplicaSet named by status.stableRS.
	image := ro.Spec.Template.Spec.Containers[0].Image
	if ro.Status.StableRS != "" {
		if rs, err := c.clientset.AppsV1().ReplicaSets(namespace).Get(ctx, ro.Status.StableRS, metav1.GetOptions{}); err == nil && len(rs.Spec.Template.Spec.Containers) > 0 {
			image = rs.Spec.Template.Spec.Containers[0].Image
		}
	}

	w := &Workload{
		Kind:      "rollout",
		Image:     image,
		Container: ro.Spec.Template.Spec.Containers[0].Name,
		Health: Health{
			Ready:         ro.Status.ReadyReplicas == ro.Status.Replicas && ro.Status.Replicas > 0,
			ReadyReplicas: ro.Status.ReadyReplicas,
			Replicas:      ro.Status.Replicas,
		},
	}
	// rollout conditions follow the same shapes as deployments, plus
	// Argo-specific Degraded when an analysis step fails.
	for _, cond := range ro.Status.Conditions {
		switch {
		case cond.Type == "Progressing" && cond.Reason == "ProgressDeadlineExceeded":
			w.Health.Conditions = append(w.Health.Conditions, "ProgressDeadlineExceeded")
		case cond.Type == "Progressing" && cond.Reason == "DeploymentPaused":
			w.Health.Paused = true
		case cond.Type == "Progressing" && cond.Status == metav1.ConditionTrue && progressingReasons[cond.Reason]:
			w.Health.Progressing = true
		case cond.Type == "ReplicaFailure" && cond.Status == metav1.ConditionTrue:
			w.Health.Conditions = append(w.Health.Conditions, "ReplicaFailure")
		case cond.Type == "Available" && cond.Status == metav1.ConditionFalse:
			w.Health.Conditions = append(w.Health.Conditions, "Unavailable")
		case cond.Type == "Degraded" && cond.Status == metav1.ConditionTrue:
			w.Health.Conditions = append(w.Health.Conditions, "Degraded")
		}
	}
	c.collectHealth(ctx, namespace, name, ro.Spec.Selector, "Rollout", &w.Health)
	return w, nil
}

// progressingReasons are the Deployment/Rollout "Progressing" condition
// reasons that indicate a rollout is actively underway. NewReplicaSetAvailable
// (rollout complete), ProgressDeadlineExceeded (stuck), and DeploymentPaused
// are deliberately excluded.
var progressingReasons = map[string]bool{
	"NewReplicaSetCreated": true,
	"FoundNewReplicaSet":   true,
	"NewReplicaSetUpdated": true,
	"ReplicaSetUpdated":    true,
	"DeploymentUpdated":    true,
	"DeploymentResumed":    true,
}

// benignWaitingReasons are container State.Waiting reasons that are normal
// startup, not problems. Everything else surfaced by the waiting pass is a
// real stuck state (ImagePullBackOff, CrashLoopBackOff, ...).
var benignWaitingReasons = map[string]bool{
	"ContainerCreating": true,
	"PodInitializing":   true,
}

// terminationCause returns a human label for why a container last terminated,
// preferring a stable reason (e.g. "OOMKilled") over a generic "Error" plus
// exit code (e.g. "Exit137"). Returns "" for containers that never terminated.
// Unlike events, this lives in the pod status, so it survives kubelet event
// GC and shows up even for containers that restarted and are now healthy.
func terminationCause(cs corev1.ContainerStatus) string {
	var t *corev1.ContainerStateTerminated
	if cs.State.Terminated != nil {
		t = cs.State.Terminated
	} else if cs.LastTerminationState.Terminated != nil {
		t = cs.LastTerminationState.Terminated
	}
	if t == nil {
		return ""
	}
	if t.Reason != "" && t.Reason != "Error" {
		return t.Reason
	}
	if t.ExitCode != 0 {
		return fmt.Sprintf("Exit%d", t.ExitCode)
	}
	return t.Reason
}

// collectHealth augments the workload with pod restart counts, Pending/Failed
// pod phases, workload conditions, and recent warning events (OOM kills,
// backoff, failed scheduling, ...).
func (c *RealClient) collectHealth(ctx context.Context, namespace, name string, sel *metav1.LabelSelector, kind string, h *Health) {
	labelSel, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return
	}
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSel.String()})
	if err != nil {
		return
	}

	podNames := make(map[string]bool, len(pods.Items))
	seenCauses := make(map[string]bool)
	seenWaiting := make(map[string]bool)
	for i := range pods.Items {
		pod := &pods.Items[i]
		podNames[pod.Name] = true
		switch pod.Status.Phase {
		case corev1.PodPending:
			h.PendingPods++
		case corev1.PodFailed:
			h.FailedPods++
		}
		for _, cs := range pod.Status.ContainerStatuses {
			h.Restarts += cs.RestartCount
			if cause := terminationCause(cs); cause != "" && !seenCauses[cause] {
				seenCauses[cause] = true
				h.RestartCauses = append(h.RestartCauses, cause)
			}
			// Current-state waiting reasons (ImagePullBackOff, CrashLoopBackOff,
			// ...) mark a container that is stuck right now. Benign startup
			// states are skipped.
			if cs.State.Waiting != nil {
				if reason := cs.State.Waiting.Reason; reason != "" && !benignWaitingReasons[reason] && !seenWaiting[reason] {
					seenWaiting[reason] = true
					h.Waiting = append(h.Waiting, reason)
				}
			}
		}
	}

	events, err := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	// Events describe things that happened, so they're bucketed purely by age:
	// kubelet updates an event's LastTimestamp every time it recurs, so an
	// ongoing problem keeps refreshing into the recent (red) bucket while a
	// resolved one ages through yellow and muted before being dropped at the
	// history window. Transient reasons are kept too — hiding them is a
	// display-side preference, not a collection policy.
	now := metav1.Now()
	recentCutoff := metav1.NewTime(now.Add(-c.timebox.Recent))
	warnCutoff := metav1.NewTime(now.Add(-c.timebox.Warn))
	historyCutoff := metav1.NewTime(now.Add(-c.timebox.History))
	seen := map[string]bool{}
	for i := range events.Items {
		ev := &events.Items[i]
		if ev.Type != "Warning" {
			continue
		}
		obj := ev.InvolvedObject
		if obj.Kind == kind && obj.Name == name {
			// workload-level warning (e.g. FailedRollout)
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
		switch {
		case !ev.LastTimestamp.Before(&recentCutoff):
			h.RecentEvents = append(h.RecentEvents, reason)
		case !ev.LastTimestamp.Before(&warnCutoff):
			h.Events = append(h.Events, reason)
		case !ev.LastTimestamp.Before(&historyCutoff):
			h.OldEvents = append(h.OldEvents, reason)
		}
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
