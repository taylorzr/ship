package k8s

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Health struct {
	Ready            bool
	ReadyReplicas    int32
	Replicas         int32 // current running replicas (status.replicas)
	DesiredReplicas  int32 // desired spec.replicas; scale direction is desired vs current
	Restarts         int32
	RestartCauses    []string      // last termination reason per restarted container, e.g. "OOMKilled", "Exit137"
	Events           []string      // Warning reasons within the warn window, e.g. "OOMKilled", "BackOff"; FailedScheduling carries "#detail" with the scheduler message; probe failures surface as "StartupProbeFailed"/"LivenessProbeFailed"/"ReadinessProbeFailed"
	RecentEvents     []string      // Warning reasons within the recent window (rendered red); may carry "#detail" suffix
	OldEvents        []string      // Warning reasons within the history window (rendered muted); may carry "#detail" suffix
	Waiting          []string      // container State.Waiting reasons, e.g. "ImagePullBackOff", "CrashLoopBackOff"
	Progressing      bool          // workload is mid-rollout (a deploy is in progress)
	Paused           bool          // rollout paused awaiting manual approval (DeploymentPaused condition reason)
	Conditions       []string      // deployment condition reasons, e.g. "ProgressDeadlineExceeded"
	PendingPods      int32         // pods stuck in Pending phase (scheduling, etc.)
	StuckPendingPods int32         // pending pods with a non-empty Status.Reason (Unschedulable, FailedMount, ...)
	FailedPods       int32         // pods in Failed phase
	FailedReasons    []string      // pod Status.Reason for Failed pods, e.g. "Evicted", "NodeLost"
	ScaleUp          int32         // pods added by HPA rescales within the history window
	ScaleDown        int32         // pods removed by HPA rescales within the history window
	HpaMaxReplicas   int32         // maxReplicas of the HPA targeting this workload (0 = no HPA / unknown)
	HpaScaleLimited  bool          // HPA wanted more than maxReplicas and was clamped (ScalingLimited=True/TooManyReplicas)
	NewReadyReplicas int32         // ready pods on the current ReplicaSet (-1 = unknown/unavailable)
	StartupMax       time.Duration // max app-startup→ready across ready pods (excludes image pull)
	DeployDuration   time.Duration // last completed rollout duration (RS created → healthy), incl. image pull
}

type Workload struct {
	Kind      string // "deployment" or "rollout"
	Image     string // full image ref, e.g. "123456789.dkr.ecr.us-east-1.amazonaws.com/podium-deploy-api:v10.1.0"
	Container string
	Health    Health
}

// DeployDurationDebug captures the intermediate values computed while deriving
// DeployDuration so callers can inspect which branch zeroed the duration.
type DeployDurationDebug struct {
	Kind string // "deployment" or "rollout"

	// Progressing — the condition that gates duration computation.
	Progressing      bool
	ProgressingCond  string // e.g. "True/ReplicaSetUpdated" or "True/NewReplicaSetAvailable"
	ProgressingValue string // full condition as the controller reports it

	// Current ReplicaSet — the baseline (curCreated).
	CurrentRSFound     bool
	CurrentRSName      string
	CurrentRSCreatedAt time.Time
	CurrentRSReady     int32

	// Pod fallback — used when ReplicaSet listing is denied.
	LatestPodCreatedAt time.Time
	FallbackUsed       bool

	// Completion condition — the finish line (deploy_duration = completion - curCreated).
	CompletionCondFound bool
	CompletionLTT       time.Time
	CompletionCond      string // e.g. "True/NewReplicaSetAvailable" or "True/Healthy"

	// Result.
	CurCreated     time.Time // the actual baseline used
	DeployDuration time.Duration
}

// resource is the workload kind ("deployment" or "rollout").
type Client interface {
	GetWorkload(ctx context.Context, context, namespace, name, resource string) (*Workload, error)
}

// EventTimebox configures how long collected warning events stay visible and
// how they're bucketed for display: within Recent renders red, within Warn
// renders yellow, within History renders muted, older events are dropped.
type EventTimebox struct {
	Recent  time.Duration
	Warn    time.Duration
	History time.Duration
}

// Normalized fills in defaults for unset durations and clamps the buckets so
// Warn >= Recent and History >= Warn even with a misconfigured caller.
func (t EventTimebox) Normalized() EventTimebox {
	if t.Recent <= 0 {
		t.Recent = time.Minute
	}
	if t.Warn <= 0 {
		t.Warn = 10 * time.Minute
	}
	if t.History <= 0 {
		t.History = time.Hour
	}
	if t.Warn < t.Recent {
		t.Warn = t.Recent
	}
	if t.History < t.Warn {
		t.History = t.Warn
	}
	return t
}

// DefaultTransientEvents are Warning event reasons that describe conditions
// which may have since resolved (failed probes, restart backoff, scheduling
// or mount hiccups). The collector keeps every event within the timebox; this
// list only drives the display-side "hide transient" filter, so its default
// value is a matter of taste rather than collection policy.
var DefaultTransientEvents = []string{
	"Unhealthy",
	"StartupProbeFailed",
	"LivenessProbeFailed",
	"ReadinessProbeFailed",
	"FailedCreate",
	"FailedCreatePodSandBox",
	"FailedCreatePod",
	"BackOff",
	"CrashLoopBackOff",
	"FailedScheduling",
	"FailedMount",
	"FailedAttachVolume",
	"Killing",
	"KillPodSandbox",
	"NodeNotReady",
	"NodeReady",
}

func ParseImageTag(image string) (string, string, error) {
	parts := strings.SplitN(image, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", "", fmt.Errorf("no tag in image %q", image)
	}
	return parts[0], parts[1], nil
}

// SHAFromImage returns the bare commit SHA when the image tag is a raw SHA or
// a "sha-" prefixed SHA (e.g. "sha-abc1234"), and reports whether it matched.
// The prefix is stripped so callers can use the result directly as a git ref.
func SHAFromImage(tag string) (string, bool) {
	raw := strings.TrimPrefix(tag, "sha-")
	if len(raw) < 7 || len(raw) > 40 {
		return "", false
	}
	for _, c := range raw {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return "", false
		}
	}
	return raw, true
}

// ShortRef shortens a version ref for display. Raw or "sha-" prefixed SHAs
// render as the short SHA with the prefix kept and an ellipsis appended when
// truncation actually happened; anything else (tags) is returned unchanged.
func ShortRef(ref string) string {
	sha, ok := SHAFromImage(ref)
	if !ok {
		return ref
	}
	short := sha
	if len(short) > 7 {
		short = short[:7] + "…"
	}
	if strings.HasPrefix(ref, "sha-") {
		return "sha-" + short
	}
	return short
}
