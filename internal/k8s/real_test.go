package k8s

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testNamespace = "default"

func newTestClient(t *testing.T, objs ...runtime.Object) *RealClient {
	t.Helper()
	return &RealClient{
		clientset: fake.NewSimpleClientset(objs...),
		timebox:   (EventTimebox{}).Normalized(),
	}
}

func (c *RealClient) collectHealthFor(t *testing.T, name, kind string) *Health {
	t.Helper()
	h := &Health{}
	c.collectHealth(context.Background(), testNamespace, name,
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, kind, "", h)
	return h
}

func testPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func warningEvent(name, objKind, objName, reason string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:    metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Type:          corev1.EventTypeWarning,
		Reason:        reason,
		LastTimestamp: metav1.NewTime(time.Now().Add(-30 * time.Second)),
		InvolvedObject: corev1.ObjectReference{
			Kind:      objKind,
			Name:      objName,
			Namespace: testNamespace,
		},
	}
}

func testReplicaSet(name string, ownerKind, ownerName string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       ownerKind,
				Name:       ownerName,
			}},
		},
	}
}

func TestCollectHealthCurrentPodEvent(t *testing.T) {
	c := newTestClient(t,
		testPod("web-abc", corev1.PodRunning),
		warningEvent("oom", "Pod", "web-abc", "OOMKilled"),
	)
	h := c.collectHealthFor(t, "web", "Deployment")
	if !slices.Contains(h.RecentEvents, "OOMKilled") {
		t.Fatalf("RecentEvents = %v, want it to contain OOMKilled", h.RecentEvents)
	}
}

func TestCollectHealthOrphanedPodEventViaRS(t *testing.T) {
	// The evicted pod is gone from the pod list, but its Warning event still
	// lingers in etcd until the cluster's event GC sweeps it. The event must
	// still surface because its name matches the workload's ReplicaSet prefix.
	c := newTestClient(t,
		testPod("web-5b9f8d4d7-7k2xz", corev1.PodRunning),
		testReplicaSet("web-5b9f8d4d7", "Deployment", "web"),
		warningEvent("evicted", "Pod", "web-5b9f8d4d7-old", "Evicted"),
	)
	h := c.collectHealthFor(t, "web", "Deployment")
	if !slices.Contains(h.RecentEvents, "Evicted") {
		t.Fatalf("RecentEvents = %v, want it to contain Evicted", h.RecentEvents)
	}
}

func TestCollectHealthOrphanedEventScopedToWorkload(t *testing.T) {
	// The orphaned event matches a ReplicaSet, but that RS belongs to a
	// different Deployment, so it must not surface for this workload.
	c := newTestClient(t,
		testPod("web-abc", corev1.PodRunning),
		testReplicaSet("other-5b9f8d4d7", "Deployment", "other"),
		warningEvent("evicted", "Pod", "other-5b9f8d4d7-old", "Evicted"),
	)
	h := c.collectHealthFor(t, "web", "Deployment")
	if len(h.RecentEvents)+len(h.Events)+len(h.OldEvents) != 0 {
		t.Fatalf("events = recent:%v warn:%v old:%v, want none", h.RecentEvents, h.Events, h.OldEvents)
	}
}

func TestCollectHealthOrphanedEventViaCurrentPodPrefix(t *testing.T) {
	// Listing ReplicaSets can be denied by read-only RBAC (the failure is
	// swallowed). The orphaned event must still surface via the prefix derived
	// from the current pod's name, which is "<rs>-<rand5>".
	c := newTestClient(t,
		testPod("web-5b9f8d4d7-7k2xz", corev1.PodRunning),
		warningEvent("evicted", "Pod", "web-5b9f8d4d7-old", "Evicted"),
	)
	c.clientset.(*fake.Clientset).PrependReactor("list", "replicasets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("replicasets is forbidden by RBAC")
	})
	h := c.collectHealthFor(t, "web", "Deployment")
	if !slices.Contains(h.RecentEvents, "Evicted") {
		t.Fatalf("RecentEvents = %v, want it to contain Evicted", h.RecentEvents)
	}
}

func TestCollectHealthPaginatesEventPages(t *testing.T) {
	// A namespace with more events than fit on one API page must not lose the
	// newest ones: the collector has to follow the continue token.
	c := newTestClient(t, testPod("web-abc", corev1.PodRunning))
	page := 0
	c.clientset.(*fake.Clientset).PrependReactor("list", "events", func(action k8stesting.Action) (bool, runtime.Object, error) {
		page++
		ev := warningEvent("evicted", "Pod", "web-abc", "Evicted")
		lst := &corev1.EventList{Items: []corev1.Event{*ev}}
		if page == 1 {
			lst.Continue = "next-page"
		}
		return true, lst, nil
	})
	h := c.collectHealthFor(t, "web", "Deployment")
	if page != 2 {
		t.Fatalf("event list pages fetched = %d, want 2 (continue token not followed)", page)
	}
	if !slices.Contains(h.RecentEvents, "Evicted") {
		t.Fatalf("RecentEvents = %v, want it to contain Evicted", h.RecentEvents)
	}
}

func TestCollectHealthNodeEventDropped(t *testing.T) {
	c := newTestClient(t,
		testPod("web-abc", corev1.PodRunning),
		warningEvent("pressure", "Node", "node1", "NodeHasDiskPressure"),
	)
	h := c.collectHealthFor(t, "web", "Deployment")
	if len(h.RecentEvents)+len(h.Events)+len(h.OldEvents) != 0 {
		t.Fatalf("events = recent:%v warn:%v old:%v, want none", h.RecentEvents, h.Events, h.OldEvents)
	}
}

func unhealthyProbeEvent(name, objName, message string) *corev1.Event {
	ev := warningEvent(name, "Pod", objName, "Unhealthy")
	ev.Message = message
	return ev
}

func TestProbeKind(t *testing.T) {
	cases := map[string]string{
		"Startup probe failed: Get \"http://10.0.0.1:8000/livez\": connect: connection refused": "Startup",
		"Liveness probe failed: cat: can't open '/tmp/healthy': No such file or directory":      "Liveness",
		"Readiness probe failed: HTTP probe failed with statuscode: 500":                        "Readiness",
		"":                                     "",
		"Back-off restarting failed container": "",
	}
	for msg, want := range cases {
		if got := probeKind(msg); got != want {
			t.Errorf("probeKind(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestCollectHealthProbeFailureReasons(t *testing.T) {
	c := newTestClient(t,
		testPod("web-abc", corev1.PodRunning),
		unhealthyProbeEvent("su", "web-abc", "Startup probe failed: Get \"http://10.0.0.1:8000/livez\": connect: connection refused"),
		unhealthyProbeEvent("lv", "web-abc", "Liveness probe failed: cat: can't open '/tmp/healthy': No such file or directory"),
		unhealthyProbeEvent("rd", "web-abc", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
	)
	h := c.collectHealthFor(t, "web", "Deployment")
	for _, want := range []string{"StartupProbeFailed", "LivenessProbeFailed", "ReadinessProbeFailed"} {
		if !slices.Contains(h.RecentEvents, want) {
			t.Fatalf("RecentEvents = %v, want it to contain %s", h.RecentEvents, want)
		}
	}
	if slices.Contains(h.RecentEvents, "Unhealthy") {
		t.Fatalf("RecentEvents = %v, want probe reasons to replace Unhealthy", h.RecentEvents)
	}
}

func readyRunningPod(pod, container string, readyAt, startedAt time.Time) *corev1.Pod {
	p := testPod(pod, corev1.PodRunning)
	p.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodReady,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(readyAt),
	}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: container,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(startedAt)},
		},
	}}
	return p
}

func TestCollectHealthStartupMax(t *testing.T) {
	now := time.Now()
	c := newTestClient(t,
		readyRunningPod("web-1", "web", now.Add(-2*time.Second), now.Add(-8*time.Second)),  // startup 6s
		readyRunningPod("web-2", "web", now.Add(-5*time.Second), now.Add(-25*time.Second)), // startup 20s
	)
	h := c.collectHealthFor(t, "web", "Deployment")
	if h.StartupMax != 20*time.Second {
		t.Fatalf("StartupMax = %v, want 20s", h.StartupMax)
	}
}

func TestCollectHealthStartupMaxSkipsUnreadyOrWaiting(t *testing.T) {
	now := time.Now()
	unready := testPod("web-3", corev1.PodRunning) // no Ready condition
	waiting := readyRunningPod("web-4", "web", now, now.Add(-time.Second))
	waiting.Status.ContainerStatuses[0].State.Running = nil
	waiting.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}
	skew := readyRunningPod("web-5", "web", now.Add(-time.Second), now) // started after ready
	c := newTestClient(t, unready, waiting, skew)
	h := c.collectHealthFor(t, "web", "Deployment")
	if h.StartupMax != 0 {
		t.Fatalf("StartupMax = %v, want 0 (no qualifying pods)", h.StartupMax)
	}
}

func TestCollectHealthStartupMaxCapsAtDeployDuration(t *testing.T) {
	// Ready LastTransitionTime is the latest transition: a pod that recovered
	// readiness without a container restart reports a window from the original
	// process start. A startup can't exceed the rollout duration, so samples
	// beyond it are recovery artifacts, not startups — filtered while any
	// plausible sample remains.
	now := time.Now()
	c := newTestClient(t,
		readyRunningPod("web-1", "web", now.Add(-30*time.Second), now.Add(-2*time.Minute)), // startup 90s
		readyRunningPod("web-2", "web", now.Add(-time.Minute), now.Add(-16*time.Hour)),     // recovery artifact
	)
	h := &Health{DeployDuration: 2 * time.Minute}
	c.collectHealth(context.Background(), testNamespace, "web",
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, "Deployment", "", h)
	if h.StartupMax != 90*time.Second {
		t.Fatalf("StartupMax = %v, want 90s (artifact filtered by the rollout cap)", h.StartupMax)
	}
}

func TestCollectHealthStartupMaxShowsInflatedWhenNothingPlausible(t *testing.T) {
	// When every sample exceeds the rollout duration, show the largest one
	// anyway — an inflated value is easier to spot than a missing one.
	now := time.Now()
	c := newTestClient(t,
		readyRunningPod("web-1", "web", now.Add(-time.Minute), now.Add(-16*time.Hour)),
	)
	h := &Health{DeployDuration: 2 * time.Minute}
	c.collectHealth(context.Background(), testNamespace, "web",
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, "Deployment", "", h)
	if h.StartupMax != 16*time.Hour-time.Minute {
		t.Fatalf("StartupMax = %v, want %v (inflated fallback shown)", h.StartupMax, 16*time.Hour-time.Minute)
	}
}

func TestCollectHealthStartupMaxWithoutRolloutDurationShowsAll(t *testing.T) {
	// With no rollout duration to cap against (DeployDuration == 0), every
	// sample counts, including long-running pods.
	now := time.Now()
	c := newTestClient(t,
		readyRunningPod("web-1", "web", now.Add(-30*time.Second), now.Add(-2*time.Minute)), // startup 90s
		readyRunningPod("web-2", "web", now.Add(-time.Minute), now.Add(-16*time.Hour)),     // 16h-1m
	)
	h := c.collectHealthFor(t, "web", "Deployment")
	if h.StartupMax != 16*time.Hour-time.Minute {
		t.Fatalf("StartupMax = %v, want %v (no cap)", h.StartupMax, 16*time.Hour-time.Minute)
	}
}

func TestCollectHealthUnhealthyWithoutProbeMessageKept(t *testing.T) {
	c := newTestClient(t,
		testPod("web-abc", corev1.PodRunning),
		unhealthyProbeEvent("oo", "web-abc", ""),
	)
	h := c.collectHealthFor(t, "web", "Deployment")
	if !slices.Contains(h.RecentEvents, "Unhealthy") {
		t.Fatalf("RecentEvents = %v, want it to contain Unhealthy", h.RecentEvents)
	}
	if slices.Contains(h.RecentEvents, "StartupProbeFailed") {
		t.Fatalf("RecentEvents = %v, want no probe annotation", h.RecentEvents)
	}
}

func TestCollectHealthFailedPodReason(t *testing.T) {
	pod := testPod("web-abc", corev1.PodFailed)
	pod.Status.Reason = "Evicted"
	c := newTestClient(t, pod)
	h := c.collectHealthFor(t, "web", "Deployment")
	if h.FailedPods != 1 {
		t.Fatalf("FailedPods = %d, want 1", h.FailedPods)
	}
	if len(h.FailedReasons) != 1 || h.FailedReasons[0] != "Evicted" {
		t.Fatalf("FailedReasons = %v, want [Evicted]", h.FailedReasons)
	}
}

func TestCollectHealthFailedPodReasonDeduped(t *testing.T) {
	c := newTestClient(t,
		testFailedPod("web-abc", "Evicted"),
		testFailedPod("web-def", "Evicted"),
		testFailedPod("web-ghi", "NodeLost"),
	)
	h := c.collectHealthFor(t, "web", "Deployment")
	if !slices.Equal(h.FailedReasons, []string{"Evicted", "NodeLost"}) {
		t.Fatalf("FailedReasons = %v, want [Evicted NodeLost]", h.FailedReasons)
	}
}

func testFailedPod(name, reason string) *corev1.Pod {
	pod := testPod(name, corev1.PodFailed)
	pod.Status.Reason = reason
	return pod
}

func testDeployment(replicas *int32) *appsv1.Deployment {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: testNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "img:1"}}},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 1},
	}
	return dep
}

func TestGetDeploymentCapturesDesiredReplicas(t *testing.T) {
	three := int32(3)
	c := newTestClient(t, testDeployment(&three))
	w, err := c.GetWorkload(context.Background(), "", testNamespace, "web", "deployment")
	if err != nil {
		t.Fatal(err)
	}
	if w.Health.DesiredReplicas != 3 || w.Health.Replicas != 2 || w.Health.ReadyReplicas != 1 {
		t.Fatalf("health = %+v, want desired 3 current 2 ready 1", w.Health)
	}
}

func TestGetDeploymentDesiredReplicasDefaultsToOne(t *testing.T) {
	c := newTestClient(t, testDeployment(nil))
	w, err := c.GetWorkload(context.Background(), "", testNamespace, "web", "deployment")
	if err != nil {
		t.Fatal(err)
	}
	if w.Health.DesiredReplicas != 1 {
		t.Fatalf("DesiredReplicas = %d, want default 1", w.Health.DesiredReplicas)
	}
}

func testProgressingDeployment() *appsv1.Deployment {
	dep := testDeployment(nil)
	dep.Status.Replicas = 3
	dep.Status.ReadyReplicas = 2
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:   appsv1.DeploymentProgressing,
		Status: corev1.ConditionTrue,
		Reason: "NewReplicaSetUpdated",
	}}
	return dep
}

func testRS(name string, revision int, replicas, ready int32) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    map[string]string{"app": "web"},
			Annotations: map[string]string{
				deploymentRevisionAnnotation: strconv.Itoa(revision),
			},
		},
		Spec:   appsv1.ReplicaSetSpec{Replicas: &replicas},
		Status: appsv1.ReplicaSetStatus{Replicas: replicas, ReadyReplicas: ready},
	}
}

func TestGetDeploymentCapturesNewReadyReplicas(t *testing.T) {
	c := newTestClient(t, testProgressingDeployment(),
		testRS("web-abc", 1, 3, 2),
		testRS("web-def", 2, 3, 1))
	w, err := c.GetWorkload(context.Background(), "", testNamespace, "web", "deployment")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Health.Progressing {
		t.Fatalf("want progressing, health = %+v", w.Health)
	}
	if w.Health.NewReadyReplicas != 1 {
		t.Fatalf("NewReadyReplicas = %d, want 1 (ready pods on the current RS)", w.Health.NewReadyReplicas)
	}
}

func TestGetDeploymentNotProgressingNewReadyReplicasUnknown(t *testing.T) {
	c := newTestClient(t, testDeployment(nil), testRS("web-abc", 1, 3, 2))
	w, err := c.GetWorkload(context.Background(), "", testNamespace, "web", "deployment")
	if err != nil {
		t.Fatal(err)
	}
	if w.Health.NewReadyReplicas != -1 {
		t.Fatalf("NewReadyReplicas = %d, want -1 (unknown) when not progressing", w.Health.NewReadyReplicas)
	}
}

func TestGetDeploymentComputesDeployDuration(t *testing.T) {
	now := time.Now()
	rs := testRS("web-def", 2, 3, 3)
	rs.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	dep := testDeployment(nil)
	dep.Status.Replicas = 3
	dep.Status.ReadyReplicas = 3
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:               appsv1.DeploymentProgressing,
		Status:             corev1.ConditionTrue,
		Reason:             "NewReplicaSetAvailable",
		LastTransitionTime: metav1.NewTime(now.Add(-4 * time.Minute)),
	}}
	c := newTestClient(t, dep, rs)
	w, err := c.GetWorkload(context.Background(), "", testNamespace, "web", "deployment")
	if err != nil {
		t.Fatal(err)
	}
	if w.Health.DeployDuration != 6*time.Minute {
		t.Fatalf("DeployDuration = %v, want 6m", w.Health.DeployDuration)
	}
}

func TestGetDeploymentNoDeployDurationWhileProgressing(t *testing.T) {
	now := time.Now()
	rs := testRS("web-def", 2, 3, 1)
	rs.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	c := newTestClient(t, testProgressingDeployment(), rs)
	w, err := c.GetWorkload(context.Background(), "", testNamespace, "web", "deployment")
	if err != nil {
		t.Fatal(err)
	}
	if w.Health.DeployDuration != 0 {
		t.Fatalf("DeployDuration = %v, want 0 while progressing", w.Health.DeployDuration)
	}
}

func TestGetDeploymentNoDeployDurationWithoutCompletion(t *testing.T) {
	now := time.Now()
	rs := testRS("web-def", 2, 3, 3)
	rs.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	c := newTestClient(t, testDeployment(nil), rs)
	w, err := c.GetWorkload(context.Background(), "", testNamespace, "web", "deployment")
	if err != nil {
		t.Fatal(err)
	}
	if w.Health.DeployDuration != 0 {
		t.Fatalf("DeployDuration = %v, want 0 without a completion condition", w.Health.DeployDuration)
	}
}

func TestGetDeploymentDeployDurationFallsBackToPodsWithoutRBAC(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	dep := testDeployment(nil)
	dep.Status.Replicas = 3
	dep.Status.ReadyReplicas = 3
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:               appsv1.DeploymentProgressing,
		Status:             corev1.ConditionTrue,
		Reason:             "NewReplicaSetAvailable",
		LastTransitionTime: metav1.NewTime(now.Add(-4 * time.Minute)),
	}}
	pod := testPod("web-abc-xyz", corev1.PodRunning)
	pod.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	c := newTestClient(t, dep, pod)
	c.clientset.(*fake.Clientset).PrependReactor("list", "replicasets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("replicasets is forbidden")
	})
	w, err := c.GetWorkload(context.Background(), "", testNamespace, "web", "deployment")
	if err != nil {
		t.Fatal(err)
	}
	if w.Health.DeployDuration != 6*time.Minute {
		t.Fatalf("DeployDuration = %v, want 6m from the pod baseline", w.Health.DeployDuration)
	}
}

func TestGetRolloutComputesDeployDuration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ro := &rollout{}
	ro.Spec.Replicas = int32Ptr(3)
	ro.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	ro.Spec.Template.Spec.Containers = []struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	}{{Name: "web", Image: "img:1"}}
	ro.Status.Replicas = 3
	ro.Status.ReadyReplicas = 3
	ro.Status.StableRS = "web-abc"
	ro.Status.Conditions = []metav1.Condition{{
		Type:               "Healthy",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(now.Add(-4 * time.Minute)),
	}}
	roMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ro)
	if err != nil {
		t.Fatal(err)
	}
	roMap["apiVersion"] = rolloutGVR.GroupVersion().String()
	roMap["kind"] = "Rollout"
	roMap["metadata"] = map[string]interface{}{"name": "web", "namespace": testNamespace}
	rs := testRS("web-abc", 2, 3, 3)
	rs.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	rs.Spec.Template.Spec.Containers = []corev1.Container{{Name: "web", Image: "img:1"}}
	c := &RealClient{
		clientset:     fake.NewSimpleClientset(rs),
		dynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: roMap}),
		timebox:       (EventTimebox{}).Normalized(),
	}
	w, err := c.GetWorkload(context.Background(), "", testNamespace, "web", "rollout")
	if err != nil {
		t.Fatal(err)
	}
	if w.Health.DeployDuration != 6*time.Minute {
		t.Fatalf("DeployDuration = %v, want 6m", w.Health.DeployDuration)
	}
}

func int32Ptr(v int32) *int32 { return &v }

func TestCollectHealthStuckPendingPods(t *testing.T) {
	pod := testPod("web-abc12345", corev1.PodPending)
	pod.Status.Reason = "Unschedulable"
	c := newTestClient(t, pod)
	h := c.collectHealthFor(t, "web", "Deployment")
	if h.StuckPendingPods != 1 || h.PendingPods != 1 {
		t.Fatalf("StuckPendingPods = %d PendingPods = %d, want 1/1", h.StuckPendingPods, h.PendingPods)
	}
}

func TestCollectHealthBenignPendingNotStuck(t *testing.T) {
	c := newTestClient(t, testPod("web-abc12345", corev1.PodPending))
	h := c.collectHealthFor(t, "web", "Deployment")
	if h.StuckPendingPods != 0 {
		t.Fatalf("StuckPendingPods = %d, want 0 for a benign starting pod", h.StuckPendingPods)
	}
}

func rescaleEvent(name, hpaName string, size int32, age time.Duration) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:    metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Type:          corev1.EventTypeNormal,
		Reason:        "SuccessfulRescale",
		Message:       fmt.Sprintf("New size: %d; reason: cpu utilization (80%%/50%%) above target", size),
		LastTimestamp: metav1.NewTime(time.Now().Add(-age)),
		InvolvedObject: corev1.ObjectReference{
			Kind:      "HorizontalPodAutoscaler",
			Name:      hpaName,
			Namespace: testNamespace,
		},
	}
}

func testHPA(name, targetKind, targetName string) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: targetKind, Name: targetName},
		},
	}
}

func collectHealthForWithDesired(t *testing.T, c *RealClient, desired int32) *Health {
	t.Helper()
	h := &Health{DesiredReplicas: desired}
	c.collectHealth(context.Background(), testNamespace, "web",
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, "Deployment", "", h)
	return h
}

func TestCollectHealthHPARescaleUpTotals(t *testing.T) {
	c := newTestClient(t,
		testPod("web-abc", corev1.PodRunning),
		testHPA("web-hpa", "Deployment", "web"),
		rescaleEvent("r1", "web-hpa", 2, 2*time.Minute),
		rescaleEvent("r2", "web-hpa", 4, time.Minute),
	)
	h := collectHealthForWithDesired(t, c, 4)
	if h.ScaleUp != 2 {
		t.Fatalf("ScaleUp = %d, want 2", h.ScaleUp)
	}
	if h.ScaleDown != 0 {
		t.Fatalf("ScaleDown = %d, want 0", h.ScaleDown)
	}
}

func TestCollectHealthHPARescaleDownTotals(t *testing.T) {
	c := newTestClient(t,
		testPod("web-abc", corev1.PodRunning),
		testHPA("web-hpa", "Deployment", "web"),
		rescaleEvent("r1", "web-hpa", 6, 3*time.Minute),
		rescaleEvent("r2", "web-hpa", 2, time.Minute),
	)
	h := collectHealthForWithDesired(t, c, 2)
	if h.ScaleDown != 4 {
		t.Fatalf("ScaleDown = %d, want 4", h.ScaleDown)
	}
	if h.ScaleUp != 0 {
		t.Fatalf("ScaleUp = %d, want 0", h.ScaleUp)
	}
}

func TestCollectHealthHPARescaleTerminalToDesired(t *testing.T) {
	// Last HPA rescale set 3; the workload now wants 5 (e.g. a post-event
	// change). The terminal transition accounts for the pods added.
	c := newTestClient(t,
		testPod("web-abc", corev1.PodRunning),
		testHPA("web-hpa", "Deployment", "web"),
		rescaleEvent("r1", "web-hpa", 3, time.Minute),
	)
	h := collectHealthForWithDesired(t, c, 5)
	if h.ScaleUp != 2 {
		t.Fatalf("ScaleUp = %d, want 2", h.ScaleUp)
	}
}

func TestCollectHealthHPARescaleIgnoresOtherHPA(t *testing.T) {
	c := newTestClient(t,
		testPod("web-abc", corev1.PodRunning),
		testHPA("other-hpa", "Deployment", "other"),
		rescaleEvent("r1", "other-hpa", 2, time.Minute),
	)
	h := collectHealthForWithDesired(t, c, 2)
	if h.ScaleUp != 0 || h.ScaleDown != 0 {
		t.Fatalf("scale totals = %d/%d, want 0/0 for unrelated HPA", h.ScaleUp, h.ScaleDown)
	}
}

func TestCollectHealthHPARescaleOutsideWindow(t *testing.T) {
	c := newTestClient(t,
		testPod("web-abc", corev1.PodRunning),
		testHPA("web-hpa", "Deployment", "web"),
		rescaleEvent("r1", "web-hpa", 4, 2*time.Hour),
	)
	h := collectHealthForWithDesired(t, c, 4)
	if h.ScaleUp != 0 || h.ScaleDown != 0 {
		t.Fatalf("scale totals = %d/%d, want 0/0 for stale event", h.ScaleUp, h.ScaleDown)
	}
}

func TestRescaleSize(t *testing.T) {
	tests := []struct {
		msg  string
		want int32
		ok   bool
	}{
		{"New size: 4; reason: cpu utilization (80%/50%) above target", 4, true},
		{"New size: 1", 1, true},
		{"Scaled up replica set web-abc to 5", 0, false},
		{"", 0, false},
		{"New size: ; reason: x", 0, false},
	}
	for _, tt := range tests {
		got, ok := rescaleSize(tt.msg)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("rescaleSize(%q) = (%d, %v), want (%d, %v)", tt.msg, got, ok, tt.want, tt.ok)
		}
	}
}
