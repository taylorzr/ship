package k8s

import "testing"

func TestShortRef(t *testing.T) {
	raw := "abcdef0123456789abcdef0123456789abcdef01"
	sha := "sha-abcdef0123456789abcdef0123456789abcdef01"
	short := "abcdef0"

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"raw long sha", raw, "abcdef0…"},
		{"sha prefixed long", sha, "sha-abcdef0…"},
		{"raw short sha", short, short},
		{"sha prefixed short", "sha-" + short, "sha-" + short},
		{"plain tag", "v1.2.3", "v1.2.3"},
		{"short sha prefixed with ellipsis boundary", "sha-abcdef01", "sha-abcdef0…"},
		{"non hex", "deadbeefzzzz", "deadbeefzzzz"},
		{"too short to be a sha", "abc123", "abc123"},
		{"ecr style tag", "prod-20240101-123456", "prod-20240101-123456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShortRef(tt.ref); got != tt.want {
				t.Errorf("ShortRef(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}
