package k8s

import (
	"context"
	"fmt"
)

type MockClient struct {
	Images map[string]string
}

func NewMock(images map[string]string) *MockClient {
	return &MockClient{Images: images}
}

func (m *MockClient) GetDeployment(ctx context.Context, context, namespace, name string) (*Deployment, error) {
	if len(m.Images) == 0 {
		return nil, fmt.Errorf("mock: no images configured")
	}
	image, ok := m.Images[name]
	if !ok {
		if image, ok = m.Images["*"]; !ok {
			return nil, fmt.Errorf("mock: no image for deployment %q", name)
		}
	}
	return &Deployment{
		Image:     image,
		Container: name,
	}, nil
}
