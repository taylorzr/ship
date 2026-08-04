package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	GitHub          GitHubConfig    `toml:"github"`
	K8s             K8sConfig       `toml:"k8s"`
	RefreshInterval int             `toml:"refresh_interval"`
	AI              AIConfig        `toml:"ai"`
	Repos           []RepoConfig    `toml:"repo"`
	Services        []ServiceConfig `toml:"service"`
	Jira            []JiraConfig    `toml:"jira"`
}

type K8sConfig struct {
	LoginCommand    string        `toml:"login_command"`
	EventRecent     time.Duration `toml:"event_recent"`     // warning events <= this age render red
	EventWarn       time.Duration `toml:"event_warn"`       // warning events <= this age render yellow
	EventHistory    time.Duration `toml:"event_history"`    // warning events <= this age render muted; older are dropped
	HideTransient   bool          `toml:"hide_transient"`   // hide transient warning events from the health column
	TransientEvents []string      `toml:"transient_events"` // override the default transient event reason list
}

type GitHubConfig struct {
	Owners             []string `toml:"owners"`
	Teams              []string `toml:"teams"`
	DepAuthors         []string `toml:"dep_authors"`
	IgnoreContributors []string `toml:"ignore_contributors"`
}

type AIConfig struct {
	ReviewProvider string `toml:"review_provider"`
	ReviewModel    string `toml:"review_model"`
	ReviewCommand  string `toml:"review_command"`
}

type RepoConfig struct {
	Name    string `toml:"name"`
	Starred bool   `toml:"starred"`
}

type ServiceConfig struct {
	Name            string `toml:"name"`
	Repo            string `toml:"repo"`
	Context         string `toml:"context"`
	Namespace       string `toml:"namespace"`
	Workload        string `toml:"workload"`
	Resource        string `toml:"resource"`
	VersionStrategy string `toml:"version_strategy"`
	VersionKey      string `toml:"version_key"`
	Versioning      string `toml:"versioning"`
	DeployURL       string `toml:"deploy_url"`
	Branch          string `toml:"branch"`
	SkipUntagged    bool   `toml:"skip_untagged"`
	ReleaseSource   string `toml:"release_source"`
}

// ResourceType returns the k8s resource kind for the service, defaulting to
// "deployment" when unset. Supported values: "deployment", "rollout".
func (s ServiceConfig) ResourceType() string {
	if s.Resource == "" {
		return "deployment"
	}
	return s.Resource
}

type JiraConfig struct {
	Repo          string `toml:"repo"`
	Project       string `toml:"project"`
	DefaultType   string `toml:"default_type"`
	EpicLinkField string `toml:"epic_link_field"`
	Site          string `toml:"site"`
}

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ship", "config.toml")
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		RefreshInterval: 300,
		GitHub: GitHubConfig{
			DepAuthors:         []string{"app/renovate", "app/dependabot"},
			IgnoreContributors: []string{"github-actions[bot]", "dependabot[bot]", "renovate[bot]"},
		},
		K8s: K8sConfig{
			EventRecent:  time.Minute,
			EventWarn:    10 * time.Minute,
			EventHistory: time.Hour,
		},
		AI: AIConfig{
			ReviewProvider: "claude-cli",
			ReviewModel:    "opus",
		},
	}

	if path == "" {
		path = DefaultPath()
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil
	}

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func Save(cfg *Config, path string) error {
	if path == "" {
		path = DefaultPath()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return toml.NewEncoder(f).Encode(cfg)
}

func (c *Config) StarredRepos() []string {
	var out []string
	for _, r := range c.Repos {
		if r.Starred {
			out = append(out, r.Name)
		}
	}
	return out
}
