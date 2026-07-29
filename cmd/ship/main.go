package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/zach/ship/internal/config"
	gh "github.com/zach/ship/internal/github"
	"github.com/zach/ship/internal/k8s"
	"github.com/zach/ship/internal/store"
	"github.com/zach/ship/internal/tui"
	"github.com/zach/ship/internal/version"
)

var rootCmd = &cobra.Command{
	Use:   "ship",
	Short: "Personal dev hub — GitHub, prod, and what's next",
	RunE:  runTUI,
}

var countCmd = &cobra.Command{
	Use:   "count",
	Short: "Print cached PR counts (for shell prompts)",
	Run: func(cmd *cobra.Command, args []string) {
		runCount()
	},
}

var releasesCmd = &cobra.Command{
	Use:   "releases",
	Short: "Show prod version status for tracked services",
	RunE:  runReleases,
}

var mockK8sImage string
var releasesRepo string

func init() {
	rootCmd.AddCommand(countCmd)
	rootCmd.AddCommand(releasesCmd)
	rootCmd.Flags().StringVar(&mockK8sImage, "mock-k8s", "", "mock k8s with this image (e.g. repo/app:v10.1.0)")
	releasesCmd.Flags().StringVar(&mockK8sImage, "mock-k8s", "", "mock k8s with this image (e.g. repo/app:v10.1.0)")
	releasesCmd.Flags().StringVar(&releasesRepo, "repo", "", "filter to a specific repo (e.g. taylorzr/kitty-meow)")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runTUI(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load("")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	st, err := store.Open("")
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()

	token, err := ghToken()
	if err != nil {
		return fmt.Errorf("gh auth: %w", err)
	}

	ghClient := gh.NewClient(token, cfg.GitHub.ExcludeOwners)

	m := tui.New(cfg, st, ghClient, mockK8sImage)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

func toCached(p gh.PR, role string) store.CachedPR {
	return store.CachedPR{
		Number:         p.Number,
		Repo:           p.Repo,
		Title:          p.Title,
		Author:         p.Author,
		Role:           role,
		URL:            p.URL,
		ReviewDecision: p.ReviewDecision,
		CIState:        p.CIState,
		Mergeable:      p.Mergeable,
		UpdatedAt:      p.UpdatedAt,
	}
}

func runCount() {
	st, err := store.Open("")
	if err != nil {
		return
	}
	defer st.Close()

	rows, err := st.Query(`
		SELECT role, COUNT(*) FROM pr
		WHERE role IN ('mine', 'review-direct', 'review-team', 'dep')
		GROUP BY role
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var role string
		var n int
		rows.Scan(&role, &n)
		counts[role] = n
	}

	var parts []string
	if n := counts["review-direct"] + counts["review-team"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d review", n))
	}
	if n := counts["mine"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d mine", n))
	}
	if n := counts["dep"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d dep", n))
	}

	if len(parts) == 0 {
		fmt.Print("all clear")
	} else {
		fmt.Print(strings.Join(parts, " · "))
	}
}

func ghToken() (string, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("run `gh auth login` first: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func runReleases(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load("")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	token, err := ghToken()
	if err != nil {
		return fmt.Errorf("gh auth: %w", err)
	}

	ghClient := gh.NewClient(token, cfg.GitHub.ExcludeOwners)
	ctx := context.Background()

	var mockK8s *k8s.MockClient
	if mockK8sImage != "" {
		mockK8s = k8s.NewMock(mockK8sImage)
		fmt.Printf("(using mock k8s — image: %s)\n\n", mockK8sImage)
	}

	if len(cfg.Services) == 0 {
		fmt.Println("no services configured — add [[service]] to ~/.config/ship/config.toml")
		return nil
	}

	for _, svc := range cfg.Services {
		if releasesRepo != "" && !strings.HasSuffix(svc.Repo, releasesRepo) {
			continue
		}
		var svcK8s k8s.Client
		if mockK8s != nil {
			svcK8s = mockK8s
		} else {
			svcCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			rc, err := k8s.NewRealClient(svcCtx, "", svc.Context)
			cancel()
			if err != nil {
				fmt.Printf("── %s ──\n  ✗ k8s: %v\n\n", svc.Repo, err)
				continue
			}
			svcK8s = rc
		}
		r := version.Resolve(ctx, svcK8s, ghClient, svc)
		printVersion(r)
	}
	return nil
}

func printVersion(r *version.Result) {
	if r.Error != "" {
		fmt.Printf("── %s ──\n", r.Service.Repo)
		fmt.Printf("  ✗ %s\n\n", r.Error)
		return
	}

	fmt.Printf("── %s ──\n", r.Service.Repo)

	if r.ProdTag != "" {
		fmt.Printf("  prod %s", r.ProdTag)
	} else {
		fmt.Printf("  prod %s", r.ProdSHA[:7])
	}

	if len(r.PendingTags) > 0 {
		names := make([]string, len(r.PendingTags))
		for i, t := range r.PendingTags {
			names[i] = t.Name
		}
		fmt.Printf(" · pending %s", strings.Join(names, ", "))
	}

	if r.AheadBy > 0 {
		fmt.Printf(" · +%d untagged", r.AheadBy)
	}
	fmt.Println()

	for _, c := range r.Commits {
		fmt.Printf("    %s  %s\n", c.SHA, c.Message)
	}
	fmt.Println()
}
