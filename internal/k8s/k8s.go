package k8s

import (
	"context"
	"fmt"
	"strings"
)

type Health struct {
	Ready         bool
	ReadyReplicas int32
	Replicas      int32
	Restarts      int32
	RestartCauses []string // last termination reason per restarted container, e.g. "OOMKilled", "Exit137"
	Events        []string // recent Warning reasons, e.g. "OOMKilled", "BackOff"
	Conditions    []string // deployment condition reasons, e.g. "ProgressDeadlineExceeded"
	PendingPods   int32    // pods stuck in Pending phase (scheduling, etc.)
	FailedPods    int32    // pods in Failed phase
}

type Workload struct {
	Kind      string // "deployment" or "rollout"
	Image     string // full image ref, e.g. "123456789.dkr.ecr.us-east-1.amazonaws.com/podium-deploy-api:v10.1.0"
	Container string
	Health    Health
}

// Client reads the deployed image and health for a service's k8s workload.
// resource is the workload kind ("deployment" or "rollout").
type Client interface {
	GetWorkload(ctx context.Context, context, namespace, name, resource string) (*Workload, error)
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
