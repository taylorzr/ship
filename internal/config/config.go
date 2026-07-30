package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	GitHub          GitHubConfig   `toml:"github"`
	RefreshInterval int            `toml:"refresh_interval"`
	AI              AIConfig       `toml:"ai"`
	Repos           []RepoConfig   `toml:"repo"`
	Services        []ServiceConfig `toml:"service"`
	Jira            []JiraConfig   `toml:"jira"`
}

type GitHubConfig struct {
	Owners        []string `toml:"owners"`
	ExcludeOwners []string `toml:"exclude_owners"`
	DepAuthors    []string `toml:"dep_authors"`
	ReviewTeams   []string `toml:"review_teams"`
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
	VersionStrategy string `toml:"version_strategy"`
	VersionKey      string `toml:"version_key"`
	DeployURL       string `toml:"deploy_url"`
	Branch          string `toml:"branch"`
	SkipUntagged    bool   `toml:"skip_untagged"`
	ReleaseSource   string `toml:"release_source"`
}

type JiraConfig struct {
	Repo           string `toml:"repo"`
	Project        string `toml:"project"`
	DefaultType    string `toml:"default_type"`
	EpicLinkField  string `toml:"epic_link_field"`
	Site           string `toml:"site"`
}

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ship", "config.toml")
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		RefreshInterval: 300,
		GitHub: GitHubConfig{
			DepAuthors: []string{"app/renovate", "app/dependabot"},
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
