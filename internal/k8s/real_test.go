package k8s

import (
	"context"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
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
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, kind, h)
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
