package k8s

import (
	"context"
	"fmt"
)

type MockClient struct {
	Image string
}

func NewMock(image string) *MockClient {
	return &MockClient{Image: image}
}

func (m *MockClient) GetDeployment(ctx context.Context, context, namespace, name string) (*Deployment, error) {
	if m.Image == "" {
		return nil, fmt.Errorf("mock: no image configured")
	}
	return &Deployment{
		Image:     m.Image,
		Container: name,
	}, nil
}
