package k8s

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MockSpec describes a fake deployment for a single service. The Image is
// required; the Health fields let you simulate restarts, warning events,
// unreadiness, etc.
type MockSpec struct {
	Image string
	Health
}

// ParseMockSpec decodes a --mock-k8s value of the form
//
//	image|restarts=3|causes=OOMKilling+Exit137|recent_events=OOMKilling|events=BackOff|old_events=Evicted|waiting=ImagePullBackOff|progressing=true|paused=true|ready=false|replicas=2|ready_replicas=1|conditions=ProgressDeadlineExceeded|pending=1|failed=1
//
// Health fields are optional; image is everything before the first "|".
// If no health fields are given the deployment is healthy. When `ready` is
// not given it is derived from replicas vs ready_replicas (ready when all
// replicas are ready), mirroring the real client.
func ParseMockSpec(v string) MockSpec {
	spec := MockSpec{Image: v, Health: Health{Ready: true, ReadyReplicas: 1, Replicas: 1}}
	readySet := false
	if i := strings.IndexByte(v, '|'); i >= 0 {
		spec.Image = v[:i]
		for _, part := range strings.Split(v[i+1:], "|") {
			k, val, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			switch k {
			case "restarts":
				if n, err := strconv.Atoi(val); err == nil {
					spec.Health.Restarts = int32(n)
				}
			case "causes":
				for _, c := range strings.Split(val, "+") {
					if c != "" {
						spec.Health.RestartCauses = append(spec.Health.RestartCauses, c)
					}
				}
			case "ready":
				spec.Health.Ready = val == "true" || val == "1"
				readySet = true
			case "replicas":
				if n, err := strconv.Atoi(val); err == nil {
					spec.Health.Replicas = int32(n)
				}
			case "ready_replicas":
				if n, err := strconv.Atoi(val); err == nil {
					spec.Health.ReadyReplicas = int32(n)
				}
			case "events":
				spec.Health.Events = mockEvents(strings.Split(val, "+"))
			case "recent_events":
				for _, e := range strings.Split(val, "+") {
					if e != "" {
						spec.Health.RecentEvents = append(spec.Health.RecentEvents, e)
					}
				}
			case "old_events":
				spec.Health.OldEvents = mockEvents(strings.Split(val, "+"))
			case "waiting":
				for _, w := range strings.Split(val, "+") {
					if w != "" {
						spec.Health.Waiting = append(spec.Health.Waiting, w)
					}
				}
			case "progressing":
				spec.Health.Progressing = val == "true" || val == "1"
			case "paused":
				spec.Health.Paused = val == "true" || val == "1"
			case "conditions":
				for _, c := range strings.Split(val, "+") {
					if c != "" {
						spec.Health.Conditions = append(spec.Health.Conditions, c)
					}
				}
			case "pending":
				if n, err := strconv.Atoi(val); err == nil {
					spec.Health.PendingPods = int32(n)
				}
			case "failed":
				if n, err := strconv.Atoi(val); err == nil {
					spec.Health.FailedPods = int32(n)
				}
			}
		}
	}
	if !readySet {
		spec.Health.Ready = spec.Health.ReadyReplicas == spec.Health.Replicas && spec.Health.Replicas > 0
	}
	return spec
}

// mockEvents resolves event entries that may carry a relative age (e.g.
// "BackOff@3m", "Unhealthy@15m") into the "reason@<unix>" form the display
// expects, resolving the age against now. Bare reasons pass through unchanged.
func mockEvents(vals []string) []string {
	var out []string
	for _, v := range vals {
		if v == "" {
			continue
		}
		reason, durStr, ok := strings.Cut(v, "@")
		if ok {
			if dur, err := time.ParseDuration(durStr); err == nil {
				out = append(out, fmt.Sprintf("%s@%d", reason, time.Now().Add(-dur).Unix()))
				continue
			}
		}
		out = append(out, v)
	}
	return out
}

type MockClient struct {
	Specs map[string]MockSpec
}

func NewMock(specs map[string]MockSpec) *MockClient {
	return &MockClient{Specs: specs}
}

func (m *MockClient) GetWorkload(ctx context.Context, context, namespace, name, resource string) (*Workload, error) {
	if len(m.Specs) == 0 {
		return nil, fmt.Errorf("mock: no specs configured")
	}
	spec, ok := m.Specs[name]
	if !ok {
		if spec, ok = m.Specs["*"]; !ok {
			return nil, fmt.Errorf("mock: no spec for workload %q", name)
		}
	}
	if resource == "" {
		resource = "deployment"
	}
	return &Workload{
		Kind:      resource,
		Image:     spec.Image,
		Container: name,
		Health:    spec.Health,
	}, nil
}
