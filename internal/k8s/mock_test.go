package k8s

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseMockSpecEventAges(t *testing.T) {
	spec := ParseMockSpec("img|events=BackOff@3m+ErrImagePull|old_events=Unhealthy@15m")

	if len(spec.Events) != 2 {
		t.Fatalf("Events = %v, want 2 entries", spec.Events)
	}
	if spec.Events[1] != "ErrImagePull" {
		t.Fatalf("bare reason mangled: got %q", spec.Events[1])
	}
	reason, ts, ok := strings.Cut(spec.Events[0], "@")
	if !ok {
		t.Fatalf("Events[0] = %q, want reason@unix", spec.Events[0])
	}
	if reason != "BackOff" {
		t.Fatalf("reason = %q, want BackOff", reason)
	}
	age := time.Since(unixSeconds(t, ts))
	if age < 2*time.Minute || age > 4*time.Minute {
		t.Fatalf("BackOff age = %v, want ~3m", age)
	}

	if len(spec.OldEvents) != 1 {
		t.Fatalf("OldEvents = %v, want 1 entry", spec.OldEvents)
	}
	_, ts, ok = strings.Cut(spec.OldEvents[0], "@")
	if !ok {
		t.Fatalf("OldEvents[0] = %q, want reason@unix", spec.OldEvents[0])
	}
	age = time.Since(unixSeconds(t, ts))
	if age < 14*time.Minute || age > 16*time.Minute {
		t.Fatalf("Unhealthy age = %v, want ~15m", age)
	}
}

func TestParseMockSpecBareReasonsUnchanged(t *testing.T) {
	spec := ParseMockSpec("img|events=BackOff|recent_events=OOMKilled|old_events=Evicted")
	if len(spec.Events) != 1 || spec.Events[0] != "BackOff" {
		t.Fatalf("Events = %v, want [BackOff]", spec.Events)
	}
	if len(spec.RecentEvents) != 1 || spec.RecentEvents[0] != "OOMKilled" {
		t.Fatalf("RecentEvents = %v, want [OOMKilled]", spec.RecentEvents)
	}
	if len(spec.OldEvents) != 1 || spec.OldEvents[0] != "Evicted" {
		t.Fatalf("OldEvents = %v, want [Evicted]", spec.OldEvents)
	}
}

func TestParseMockSpecProbeFailureReasons(t *testing.T) {
	spec := ParseMockSpec("img|recent_events=StartupProbeFailed+LivenessProbeFailed")
	if len(spec.RecentEvents) != 2 || spec.RecentEvents[0] != "StartupProbeFailed" || spec.RecentEvents[1] != "LivenessProbeFailed" {
		t.Fatalf("RecentEvents = %v, want [StartupProbeFailed LivenessProbeFailed]", spec.RecentEvents)
	}
}

func TestParseMockSpecDesiredReplicas(t *testing.T) {
	spec := ParseMockSpec("img|replicas=2|desired_replicas=3|ready_replicas=1")
	if spec.DesiredReplicas != 3 {
		t.Fatalf("DesiredReplicas = %d, want 3", spec.DesiredReplicas)
	}
	if spec.Replicas != 2 || spec.ReadyReplicas != 1 {
		t.Fatalf("Replicas = %d, ReadyReplicas = %d, want 2/1", spec.Replicas, spec.ReadyReplicas)
	}
	if spec.Ready {
		t.Fatal("Ready should be false (2 replicas, 1 ready, ready not given)")
	}
	spec = ParseMockSpec("img")
	if spec.DesiredReplicas != 1 {
		t.Fatalf("default DesiredReplicas = %d, want 1", spec.DesiredReplicas)
	}
}

func TestParseMockSpecScaleTotals(t *testing.T) {
	spec := ParseMockSpec("img|scale_up=4|scale_down=2")
	if spec.ScaleUp != 4 || spec.ScaleDown != 2 {
		t.Fatalf("scale totals = %d/%d, want 4/2", spec.ScaleUp, spec.ScaleDown)
	}
	spec = ParseMockSpec("img")
	if spec.ScaleUp != 0 || spec.ScaleDown != 0 {
		t.Fatalf("default scale totals = %d/%d, want 0/0", spec.ScaleUp, spec.ScaleDown)
	}
}

func TestParseMockSpecNewReadyReplicasAndStuckPending(t *testing.T) {
	spec := ParseMockSpec("img|progressing=true|desired_replicas=3|new_ready_replicas=1|stuck_pending=2")
	if spec.NewReadyReplicas != 1 || spec.StuckPendingPods != 2 {
		t.Fatalf("NewReadyReplicas = %d StuckPendingPods = %d, want 1/2", spec.NewReadyReplicas, spec.StuckPendingPods)
	}
	spec = ParseMockSpec("img|progressing=true")
	if spec.NewReadyReplicas != -1 {
		t.Fatalf("default NewReadyReplicas = %d, want -1 (unknown)", spec.NewReadyReplicas)
	}
}

func TestParseMockSpecStartupMax(t *testing.T) {
	spec := ParseMockSpec("img|startup_max=30s")
	if spec.StartupMax != 30*time.Second {
		t.Fatalf("StartupMax = %v, want 30s", spec.StartupMax)
	}
	spec = ParseMockSpec("img")
	if spec.StartupMax != 0 {
		t.Fatalf("default StartupMax = %v, want 0", spec.StartupMax)
	}
}

func TestParseMockSpecDeployDuration(t *testing.T) {
	spec := ParseMockSpec("img|deploy_duration=5m30s")
	if spec.DeployDuration != 5*time.Minute+30*time.Second {
		t.Fatalf("DeployDuration = %v, want 5m30s", spec.DeployDuration)
	}
	spec = ParseMockSpec("img")
	if spec.DeployDuration != 0 {
		t.Fatalf("default DeployDuration = %v, want 0", spec.DeployDuration)
	}
}

func unixSeconds(t *testing.T, s string) time.Time {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("bad unix timestamp %q: %v", s, err)
	}
	return time.Unix(n, 0)
}
