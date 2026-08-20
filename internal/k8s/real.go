package k8s

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
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
		dep, err = c.getRollout(ctx, namespace, name, nil)
	default:
		dep, err = c.getDeployment(ctx, namespace, name, nil)
	}
	if err != nil && c.loginCmd != "" && isAuthErr(err) {
		if lerr := relogin(c.loginCmd); lerr != nil {
			err = fmt.Errorf("%w (login failed: %v)", err, lerr)
		} else {
			if resource == "rollout" {
				dep, err = c.getRollout(ctx, namespace, name, nil)
			} else {
				dep, err = c.getDeployment(ctx, namespace, name, nil)
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

// GetWorkloadDebug calls the same fetch path as GetWorkload but returns the
// intermediate DeployDuration debug values alongside the workload.
func (c *RealClient) GetWorkloadDebug(ctx context.Context, context, namespace, name, resource string) (*Workload, *DeployDurationDebug, error) {
	if resource == "" {
		resource = "deployment"
	}
	debug := &DeployDurationDebug{}
	var (
		dep *Workload
		err error
	)
	switch resource {
	case "rollout":
		dep, err = c.getRollout(ctx, namespace, name, debug)
	default:
		dep, err = c.getDeployment(ctx, namespace, name, debug)
	}
	return dep, debug, err
}

func (c *RealClient) getDeployment(ctx context.Context, namespace, name string, debug *DeployDurationDebug) (*Workload, error) {
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
			Ready:           dep.Status.ReadyReplicas == dep.Status.Replicas && dep.Status.Replicas > 0,
			ReadyReplicas:   dep.Status.ReadyReplicas,
			Replicas:        dep.Status.Replicas,
			DesiredReplicas: desiredReplicas(dep.Spec.Replicas),
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
	if debug != nil {
		debug.Kind = "deployment"
		debug.Progressing = d.Health.Progressing
		for _, cond := range dep.Status.Conditions {
			if cond.Type == appsv1.DeploymentProgressing {
				debug.ProgressingCond = fmt.Sprintf("%s/%s", cond.Status, cond.Reason)
				debug.ProgressingValue = fmt.Sprintf("lastTransitionTime=%s lastUpdateTime=%s", cond.LastTransitionTime.Time.Format(time.RFC3339), cond.LastUpdateTime.Time.Format(time.RFC3339))
			}
		}
	}
	d.Health.NewReadyReplicas = -1
	var curCreated time.Time
	if n, createdAt, ok := c.currentRS(ctx, namespace, dep.Spec.Selector, ""); ok {
		if d.Health.Progressing {
			d.Health.NewReadyReplicas = n
		}
		curCreated = createdAt
		if debug != nil {
			debug.CurrentRSFound = true
			debug.CurrentRSReady = n
			debug.CurrentRSCreatedAt = createdAt
		}
	} else if !d.Health.Progressing {
		// ReplicaSet listing can be denied by RBAC even when deployments and
		// pods are readable. Fall back to the newest pod's creation as the
		// rollout baseline so the duration still shows.
		curCreated = c.latestPodCreation(ctx, namespace, dep.Spec.Selector)
		if debug != nil {
			debug.LatestPodCreatedAt = curCreated
			debug.FallbackUsed = true
		}
	}
	if debug != nil {
		debug.CurCreated = curCreated
	}
	// Last completed rollout duration: current RS created → rollout became
	// available (all desired replicas ready, incl. image pull and
	// minReadySeconds). Zero while progressing or when the completion
	// condition is missing.
	if !d.Health.Progressing && !curCreated.IsZero() {
		for _, cond := range dep.Status.Conditions {
			if cond.Type == appsv1.DeploymentProgressing && cond.Reason == "NewReplicaSetAvailable" && !cond.LastTransitionTime.Time.Before(curCreated) {
				d.Health.DeployDuration = cond.LastTransitionTime.Time.Sub(curCreated)
				if debug != nil {
					debug.CompletionCondFound = true
					debug.CompletionLTT = cond.LastTransitionTime.Time
					debug.CompletionCond = fmt.Sprintf("%s/%s", cond.Status, cond.Reason)
				}
				break
			}
		}
	}
	if debug != nil {
		debug.DeployDuration = d.Health.DeployDuration
	}
	c.collectHealth(ctx, namespace, name, dep.Spec.Selector, "Deployment", dep.Spec.Template.Spec.Containers[0].Name, &d.Health)
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
		Replicas *int32                `json:"replicas"`
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
		Replicas       int32              `json:"replicas"`
		ReadyReplicas  int32              `json:"readyReplicas"`
		StableRS       string             `json:"stableRS"`
		CurrentPodHash string             `json:"currentPodHash"`
		Conditions     []metav1.Condition `json:"conditions"`
	} `json:"status"`
}

func (c *RealClient) getRollout(ctx context.Context, namespace, name string, debug *DeployDurationDebug) (*Workload, error) {
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
	var stableCreated time.Time
	if ro.Status.StableRS != "" {
		if rs, err := c.clientset.AppsV1().ReplicaSets(namespace).Get(ctx, ro.Status.StableRS, metav1.GetOptions{}); err == nil && len(rs.Spec.Template.Spec.Containers) > 0 {
			image = rs.Spec.Template.Spec.Containers[0].Image
			stableCreated = rs.CreationTimestamp.Time
		}
	}

	w := &Workload{
		Kind:      "rollout",
		Image:     image,
		Container: ro.Spec.Template.Spec.Containers[0].Name,
		Health: Health{
			Ready:           ro.Status.ReadyReplicas == ro.Status.Replicas && ro.Status.Replicas > 0,
			ReadyReplicas:   ro.Status.ReadyReplicas,
			Replicas:        ro.Status.Replicas,
			DesiredReplicas: desiredReplicas(ro.Spec.Replicas),
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
	if debug != nil {
		debug.Kind = "rollout"
		debug.Progressing = w.Health.Progressing
		for _, cond := range ro.Status.Conditions {
			if cond.Type == "Progressing" {
				debug.ProgressingCond = fmt.Sprintf("%s/%s", cond.Status, cond.Reason)
				debug.ProgressingValue = fmt.Sprintf("lastTransitionTime=%s", cond.LastTransitionTime.Time.Format(time.RFC3339))
			}
		}
	}
	w.Health.NewReadyReplicas = -1
	if w.Health.Progressing && ro.Status.CurrentPodHash != "" {
		if n, _, ok := c.currentRS(ctx, namespace, ro.Spec.Selector, ro.Status.CurrentPodHash); ok {
			w.Health.NewReadyReplicas = n
		}
	}
	// Last completed rollout duration: stable RS created → rollout Healthy.
	// Zero while progressing or when the completion condition is missing.
	if !w.Health.Progressing && !stableCreated.IsZero() {
		for _, cond := range ro.Status.Conditions {
			if cond.Type == "Healthy" && cond.Status == metav1.ConditionTrue && !cond.LastTransitionTime.Time.Before(stableCreated) {
				w.Health.DeployDuration = cond.LastTransitionTime.Time.Sub(stableCreated)
				if debug != nil {
					debug.CompletionCondFound = true
					debug.CompletionLTT = cond.LastTransitionTime.Time
					debug.CompletionCond = fmt.Sprintf("%s/%s", cond.Status, cond.Type)
				}
				break
			}
		}
	}
	if debug != nil {
		debug.CurrentRSCreatedAt = stableCreated
		debug.CurCreated = stableCreated
		debug.DeployDuration = w.Health.DeployDuration
	}
	c.collectHealth(ctx, namespace, name, ro.Spec.Selector, "Rollout", ro.Spec.Template.Spec.Containers[0].Name, &w.Health)
	return w, nil
}

// desiredReplicas dereferences a spec.replicas pointer, defaulting to the
// controller's implicit value of 1 when unset.
func desiredReplicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

const (
	// deploymentRevisionAnnotation is stamped on every ReplicaSet a Deployment
	// owns; the current (new) RS is the one with the highest value.
	deploymentRevisionAnnotation = "deployment.kubernetes.io/revision"
	// rolloutsPodTemplateHashLabel ties an Argo Rollout's ReplicaSets back to
	// its status.currentPodHash; the current (canary/preview) RS is the match.
	rolloutsPodTemplateHashLabel = "rollouts-pod-template-hash"
	// startupCapGrace absorbs kubelet<->API server clock skew and informer
	// lag when comparing a pod's startup window against the rollout duration.
	// Gross exceedances (a pod that recovered readiness long after its process
	// started) are still filtered; see collectHealth.
	startupCapGrace = 30 * time.Second
)

// currentRS reports the number of ready pods and creation time of the
// workload's current ReplicaSet. For deployments (podHash empty) that is the
// RS with the highest revision annotation; for rollouts it is the RS labeled
// with the rollout's current pod template hash. ok is false when the current
// RS can't be determined (list denied by RBAC, no matching RS, ...), so
// callers can fall back to "unknown" rather than guessing.
func (c *RealClient) currentRS(ctx context.Context, namespace string, sel *metav1.LabelSelector, podHash string) (ready int32, createdAt time.Time, ok bool) {
	labelSel, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return 0, time.Time{}, false
	}
	rss, err := c.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSel.String()})
	if err != nil {
		return 0, time.Time{}, false
	}
	maxRev := int64(-1)
	found := false
	for i := range rss.Items {
		rs := &rss.Items[i]
		if podHash != "" {
			if rs.Labels[rolloutsPodTemplateHashLabel] != podHash {
				continue
			}
			return rs.Status.ReadyReplicas, rs.CreationTimestamp.Time, true
		}
		rev, err := strconv.ParseInt(rs.Annotations[deploymentRevisionAnnotation], 10, 64)
		if err != nil {
			continue
		}
		if rev > maxRev {
			maxRev = rev
			ready = rs.Status.ReadyReplicas
			createdAt = rs.CreationTimestamp.Time
			found = true
		}
	}
	return ready, createdAt, found
}

// latestPodCreation returns the newest creation time among pods matching the
// selector, or the zero time when none match. Used as a fallback rollout
// baseline when ReplicaSet listing is denied by RBAC.
func (c *RealClient) latestPodCreation(ctx context.Context, namespace string, sel *metav1.LabelSelector) time.Time {
	labelSel, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return time.Time{}
	}
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSel.String()})
	if err != nil {
		return time.Time{}
	}
	var latest time.Time
	for i := range pods.Items {
		if ts := pods.Items[i].CreationTimestamp.Time; ts.After(latest) {
			latest = ts
		}
	}
	return latest
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

// probeKind reports which container probe a kubelet Warning "Unhealthy" event
// came from, by parsing its "<Probe> probe failed: ..." message. Returns ""
// when the event isn't a probe failure (or the message is unrecognized).
func probeKind(msg string) string {
	switch {
	case strings.Contains(msg, "Startup probe failed"):
		return "Startup"
	case strings.Contains(msg, "Liveness probe failed"):
		return "Liveness"
	case strings.Contains(msg, "Readiness probe failed"):
		return "Readiness"
	default:
		return ""
	}
}

// collectHealth augments the workload with pod restart counts, Pending/Failed
// pod phases, workload conditions, and recent warning events (OOM kills,
// backoff, failed scheduling, ...).
func (c *RealClient) collectHealth(ctx context.Context, namespace, name string, sel *metav1.LabelSelector, kind, containerName string, h *Health) {
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
	seenFailedReasons := make(map[string]bool)
	var (
		startupMax time.Duration // largest sample consistent with the rollout duration
		startupAny time.Duration // largest sample regardless (shown if nothing is plausible)
	)
	for i := range pods.Items {
		pod := &pods.Items[i]
		podNames[pod.Name] = true
		switch pod.Status.Phase {
		case corev1.PodPending:
			h.PendingPods++
			// A pending pod with a non-empty reason (Unschedulable,
			// FailedMount, ...) is stuck, not merely starting up —
			// benign ContainerCreating startup leaves the reason empty.
			if pod.Status.Reason != "" {
				h.StuckPendingPods++
			}
		case corev1.PodFailed:
			h.FailedPods++
			// Evicted/NodeLost/etc. survive only while the pod object does —
			// kubelet deletes evicted pods quickly, so this is the durable
			// signal until the pod is gone.
			if reason := pod.Status.Reason; reason != "" && !seenFailedReasons[reason] {
				seenFailedReasons[reason] = true
				h.FailedReasons = append(h.FailedReasons, reason)
			}
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
		// Startup time excludes image pull: kubelet sets Running.StartedAt only
		// after the image is pulled and the container process starts, so app
		// start -> Ready transition measures the window a startup probe's
		// failureThreshold*periodSeconds must cover. Only ready pods with a
		// running app container count; clock skew (ready before started) is
		// skipped. A pod's startup can't exceed its rollout's duration (RS
		// created -> all replicas ready bounds container start -> pod ready),
		// so a sample larger than DeployDuration means Ready.LastTransitionTime
		// is a later recovery, not the startup. Those samples are excluded, but
		// if nothing plausible remains the largest one is still shown — an
		// inflated value is easier to spot than a missing one.
		var readyAt metav1.Time
		ready := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				readyAt = cond.LastTransitionTime
				ready = true
				break
			}
		}
		if ready {
			started := time.Time{}
			for _, cs := range pod.Status.ContainerStatuses {
				if containerName != "" && cs.Name != containerName {
					continue
				}
				if cs.State.Running != nil {
					started = cs.State.Running.StartedAt.Time
					break
				}
			}
			if !started.IsZero() && !readyAt.Time.Before(started) {
				d := readyAt.Time.Sub(started)
				if d > startupAny {
					startupAny = d
				}
				if h.DeployDuration == 0 || d <= h.DeployDuration+startupCapGrace {
					if d > startupMax {
						startupMax = d
					}
				}
			}
		}
	}
	if startupMax > 0 {
		h.StartupMax = startupMax
	} else {
		h.StartupMax = startupAny
	}

	// The API server caps list pages (default 500), and a plain List does not
	// follow continuation tokens — in a namespace busy with events, the newest
	// ones (exactly what we're looking for) would be silently dropped. Walk
	// the pages so no recent event is missed.
	var (
		eventList *corev1.EventList
		events    []corev1.Event
		cont      string
	)
	for {
		eventList, err = c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			return
		}
		events = append(events, eventList.Items...)
		if eventList.Continue == "" {
			break
		}
		cont = eventList.Continue
	}
	// Events describe things that happened, so they're bucketed purely by age:
	// kubelet updates an event's LastTimestamp every time it recurs, so an
	// ongoing problem keeps refreshing into the recent (red) bucket while a
	// resolved one ages through yellow and muted before being dropped at the
	// history window. Transient reasons are kept too — hiding them is a
	// display-side preference, not a collection policy. Warning "Unhealthy"
	// events are also annotated with the probe that failed (see probeKind) so
	// the display can tell a startup-probe hiccup apart from a real liveness
	// failure.
	now := metav1.Now()
	recentCutoff := metav1.NewTime(now.Add(-c.timebox.Recent))
	warnCutoff := metav1.NewTime(now.Add(-c.timebox.Warn))
	historyCutoff := metav1.NewTime(now.Add(-c.timebox.History))
	// Pod-level events live and die with their pod, so a deleted pod's events
	// (e.g. an Evicted warning) are orphaned until the cluster's event GC
	// sweeps them. To still surface those within that window, match their
	// involvedObject name against the workload's ReplicaSet name prefixes
	// (`<rs>-`), which identify pods by generation even after they're gone.
	// The RS list is loaded lazily, only when an orphaned pod event is seen;
	// it can be denied by read-only RBAC, so prefixes derived from the current
	// pods' names ("<rs>-<rand5>") serve as a fallback for the current
	// generation.
	var rsPrefixes []string
	rsListed := false
	loadRSPrefixes := func() {
		if rsListed {
			return
		}
		rsListed = true
		rss, err := c.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return // fail-open: fall back to current-pod prefixes below
		}
		for i := range rss.Items {
			rs := &rss.Items[i]
			for _, o := range rs.OwnerReferences {
				if o.Kind == kind && o.Name == name {
					rsPrefixes = append(rsPrefixes, rs.Name+"-")
					break
				}
			}
		}
	}
	var podPrefixes []string
	seenPrefix := make(map[string]bool, len(podNames))
	for n := range podNames {
		if len(n) > 5 && !seenPrefix[n[:len(n)-5]] {
			seenPrefix[n[:len(n)-5]] = true
			podPrefixes = append(podPrefixes, n[:len(n)-5])
		}
	}
	latest := map[string]metav1.Time{}
	for i := range events {
		ev := &events[i]
		if ev.Type != "Warning" {
			continue
		}
		obj := ev.InvolvedObject
		if obj.Kind == kind && obj.Name == name {
			// workload-level warning (e.g. FailedRollout)
		} else if obj.Kind == "Pod" && podNames[obj.Name] {
			// pod-level warning (e.g. OOMKilled)
		} else if obj.Kind == "Pod" {
			// pod-level warning for a pod no longer in the list (e.g. Evicted)
			loadRSPrefixes()
			orphan := false
			for _, p := range rsPrefixes {
				if strings.HasPrefix(obj.Name, p) {
					orphan = true
					break
				}
			}
			if !orphan {
				for _, p := range podPrefixes {
					if strings.HasPrefix(obj.Name, p) {
						orphan = true
						break
					}
				}
			}
			if !orphan {
				continue
			}
		} else {
			continue
		}
		reason := ev.Reason
		if reason == "" {
			continue
		}
		// The kubelet emits a separate Warning "Unhealthy" event per probe
		// type ("Startup probe failed: ...", "Liveness probe failed: ..."), but
		// events are keyed by reason below, which would collapse them all into
		// a bare "Unhealthy". Surface the probe type so the display can keep
		// them apart, mirroring k8s's own per-probe granularity.
		if reason == "Unhealthy" {
			if p := probeKind(ev.Message); p != "" {
				reason = p + "ProbeFailed"
			}
		}
		if prev, ok := latest[reason]; !ok || ev.LastTimestamp.After(prev.Time) {
			latest[reason] = ev.LastTimestamp
		}
	}
	reasons := make([]string, 0, len(latest))
	for reason := range latest {
		reasons = append(reasons, reason)
	}
	sort.Slice(reasons, func(i, j int) bool {
		ti, tj := latest[reasons[i]].Time, latest[reasons[j]].Time
		if ti.Equal(tj) {
			return reasons[i] < reasons[j]
		}
		return ti.After(tj)
	})
	for _, reason := range reasons {
		t := latest[reason]
		switch {
		case !t.Before(&recentCutoff):
			h.RecentEvents = append(h.RecentEvents, reason)
		case !t.Before(&warnCutoff):
			h.Events = append(h.Events, fmt.Sprintf("%s@%d", reason, t.Unix()))
		case !t.Before(&historyCutoff):
			h.OldEvents = append(h.OldEvents, fmt.Sprintf("%s@%d", reason, t.Unix()))
		}
	}

	c.collectHpaRescales(ctx, namespace, name, kind, events, historyCutoff, h)
}

// rescaleSize parses the replica count from an HPA SuccessfulRescale event
// message ("New size: 4; reason: cpu: 80%/50%"), reporting whether a valid
// count was found. The count is the HPA's new target, which ship turns into
// scale-up/down totals via the deltas between consecutive rescales.
func rescaleSize(msg string) (int32, bool) {
	const prefix = "New size: "
	i := strings.Index(msg, prefix)
	if i < 0 {
		return 0, false
	}
	i += len(prefix)
	end := i
	for end < len(msg) && msg[end] >= '0' && msg[end] <= '9' {
		end++
	}
	if end == i {
		return 0, false
	}
	n, err := strconv.Atoi(msg[i:end])
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// collectHpaRescales reconstructs the last hour of HPA-driven scaling for the
// workload, as pods added (ScaleUp) and removed (ScaleDown). HPA emits a
// Normal SuccessfulRescale event ("New size: N") every time it changes a
// target, and the events persist long enough (max-event-age + GC TTL) for a
// single list to cover the history window. Deployments also emit
// ScalingReplicaSet events, but a single rolling update produces ~7 of them,
// so HPA events are the clean signal. Direction is derived from the deltas
// between consecutive rescales plus a terminal transition to the workload's
// current desired count, because the event message only carries the new size.
//
// Caveat: the API server aggregates events with identical messages and counts,
// so a rapid size oscillation (e.g. 4→5→4) within an aggregation window can
// undercount; typical gradual autoscaling reconstructs accurately.
func (c *RealClient) collectHpaRescales(ctx context.Context, namespace, name, kind string, events []corev1.Event, cutoff metav1.Time, h *Health) {
	// The HPA list is loaded lazily, only when a SuccessfulRescale HPA event
	// is actually seen; it can be denied by read-only RBAC, so no list call is
	// made for services without HPA activity.
	var (
		hpas      []autoscalingv2.HorizontalPodAutoscaler
		hpaListed bool
	)
	loadHPAs := func() {
		if hpaListed {
			return
		}
		hpaListed = true
		hpaList, err := c.clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return // fail-open: nothing to match against
		}
		hpas = append(hpas, hpaList.Items...)
	}

	type rescale struct {
		at   metav1.Time
		size int32
	}
	var rescales []rescale
	for i := range events {
		ev := &events[i]
		obj := ev.InvolvedObject
		if ev.Type != "Normal" || ev.Reason != "SuccessfulRescale" || obj.Kind != "HorizontalPodAutoscaler" {
			continue
		}
		loadHPAs()
		matched := false
		for j := range hpas {
			hpa := &hpas[j]
			if hpa.Name != obj.Name {
				continue
			}
			if hpa.Spec.ScaleTargetRef.Kind == kind && hpa.Spec.ScaleTargetRef.Name == name {
				matched = true
				break
			}
		}
		if !matched || ev.LastTimestamp.Before(&cutoff) {
			continue
		}
		size, ok := rescaleSize(ev.Message)
		if !ok {
			continue
		}
		rescales = append(rescales, rescale{ev.LastTimestamp, size})
	}
	if len(rescales) == 0 {
		return
	}

	sort.Slice(rescales, func(i, j int) bool { return rescales[i].at.Before(&rescales[j].at) })
	prev := rescales[0].size
	for _, r := range rescales[1:] {
		if r.size > prev {
			h.ScaleUp += r.size - prev
		} else if r.size < prev {
			h.ScaleDown += prev - r.size
		}
		prev = r.size
	}
	// Terminal transition: the HPA's last observed target vs. the workload's
	// current desired count. Catches an in-flight rescale (event still
	// pending) and keeps attribution right after a manual `kubectl scale`.
	if prev != h.DesiredReplicas {
		if h.DesiredReplicas > prev {
			h.ScaleUp += h.DesiredReplicas - prev
		} else {
			h.ScaleDown += prev - h.DesiredReplicas
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
