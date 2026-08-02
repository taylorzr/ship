package k8s

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
//	image|restarts=3|events=OOMKilling+BackOff|ready=false|replicas=2|ready_replicas=1|conditions=ProgressDeadlineExceeded|pending=1|failed=1
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
				for _, e := range strings.Split(val, "+") {
					if e != "" {
						spec.Health.Events = append(spec.Health.Events, e)
					}
				}
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

type MockClient struct {
	Specs map[string]MockSpec
}

func NewMock(specs map[string]MockSpec) *MockClient {
	return &MockClient{Specs: specs}
}

func (m *MockClient) GetDeployment(ctx context.Context, context, namespace, name string) (*Deployment, error) {
	if len(m.Specs) == 0 {
		return nil, fmt.Errorf("mock: no specs configured")
	}
	spec, ok := m.Specs[name]
	if !ok {
		if spec, ok = m.Specs["*"]; !ok {
			return nil, fmt.Errorf("mock: no spec for deployment %q", name)
		}
	}
	return &Deployment{
		Image:     spec.Image,
		Container: name,
		Health:    spec.Health,
	}, nil
}
