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

func LooksLikeSHA(tag string) bool {
	if len(tag) < 7 || len(tag) > 40 {
		return false
	}
	for _, c := range tag {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
