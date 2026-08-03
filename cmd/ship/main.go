package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
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

var myPRsCmd = &cobra.Command{
	Use:   "my-prs",
	Short: "Run the My PRs GitHub search query and print results",
	RunE:  runMyPRs,
}

var reviewPRsCmd = &cobra.Command{
	Use:   "review-prs",
	Short: "Run the To Review GitHub search queries and print results",
	RunE:  runReviewPRs,
}

var depPRsCmd = &cobra.Command{
	Use:   "dep-prs",
	Short: "Run the Dependencies GitHub search queries and print results",
	RunE:  runDepPRs,
}

var mockK8sSpecs map[string]string
var releasesRepo string
var reviewMeOnly bool
var reviewTeamOnly bool
var depRepos, depOwners, depTeams []string
var useGraphQL bool

func init() {
	rootCmd.AddCommand(countCmd)
	rootCmd.AddCommand(releasesCmd)
	rootCmd.AddCommand(myPRsCmd)
	rootCmd.AddCommand(reviewPRsCmd)
	rootCmd.AddCommand(depPRsCmd)
	rootCmd.Flags().StringToStringVar(&mockK8sSpecs, "mock-k8s", nil, "mock k8s per service (e.g. svc1=repo/app:v10.1.0,svc2=repo/other:v2.0.0|restarts=3|events=OOMKilling+BackOff)")
	releasesCmd.Flags().StringToStringVar(&mockK8sSpecs, "mock-k8s", nil, "mock k8s per service (e.g. svc1=repo/app:v10.1.0,svc2=repo/other:v2.0.0|restarts=3|events=OOMKilling+BackOff)")
	releasesCmd.Flags().StringVar(&releasesRepo, "repo", "", "filter to a specific repo (e.g. taylorzr/kitty-meow)")
	reviewPRsCmd.Flags().BoolVar(&reviewMeOnly, "me", false, "only run the user-review-requested query")
	reviewPRsCmd.Flags().BoolVar(&reviewTeamOnly, "team", false, "only run the team-review-requested query")
	depPRsCmd.Flags().StringSliceVar(&depRepos, "repo", nil, "only run the query for this repo (repeatable)")
	depPRsCmd.Flags().StringSliceVar(&depOwners, "owner", nil, "only run the query for this owner (repeatable)")
	depPRsCmd.Flags().StringSliceVar(&depTeams, "team", nil, "only run the query for this team (repeatable)")
	myPRsCmd.Flags().BoolVar(&useGraphQL, "graphql", false, "use the GraphQL search field (the flaky path) instead of REST")
	reviewPRsCmd.Flags().BoolVar(&useGraphQL, "graphql", false, "use the GraphQL search field (the flaky path) instead of REST")
	depPRsCmd.Flags().BoolVar(&useGraphQL, "graphql", false, "use the GraphQL search field (the flaky path) instead of REST")
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

	ghClient := gh.NewClient(token)

	m := tui.New(cfg, st, ghClient, parseMockSpecs(mockK8sSpecs))
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

// parseMockSpecs converts the raw --mock-k8s string map into per-service
// k8s.MockSpec values.
func parseMockSpecs(raw map[string]string) map[string]k8s.MockSpec {
	if len(raw) == 0 {
		return nil
	}
	specs := make(map[string]k8s.MockSpec, len(raw))
	for k, v := range raw {
		specs[k] = k8s.ParseMockSpec(v)
	}
	return specs
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

func loadClient() (*config.Config, *gh.Client, error) {
	cfg, err := config.Load("")
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	token, err := ghToken()
	if err != nil {
		return nil, nil, fmt.Errorf("gh auth: %w", err)
	}
	return cfg, gh.NewClient(token), nil
}

func runMyPRs(cmd *cobra.Command, args []string) error {
	cfg, client, err := loadClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	q, err := client.MyPRsQuery(ctx, cfg.GitHub.Owners)
	if err != nil {
		return err
	}
	return runPRQueries(ctx, client, []string{q})
}

func runReviewPRs(cmd *cobra.Command, args []string) error {
	cfg, client, err := loadClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	var queries []string
	if !reviewTeamOnly {
		q, err := client.ReviewRequestedQuery(ctx, cfg.GitHub.Owners)
		if err != nil {
			return err
		}
		queries = append(queries, q)
	}
	if !reviewMeOnly && len(cfg.GitHub.Teams) > 0 {
		q, err := client.TeamReviewRequestedQuery(ctx, cfg.GitHub.Teams)
		if err != nil {
			return err
		}
		if q != "" {
			queries = append(queries, q)
		}
	}
	if len(queries) == 0 {
		fmt.Println("no queries to run (check --me/--team flags and configured teams)")
		return nil
	}
	return runPRQueries(ctx, client, queries)
}

func runDepPRs(cmd *cobra.Command, args []string) error {
	cfg, client, err := loadClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	repos, owners, teams := depRepos, depOwners, depTeams
	if len(repos) == 0 && len(owners) == 0 && len(teams) == 0 {
		repos = cfg.StarredRepos()
		owners = cfg.GitHub.Owners
		teams = cfg.GitHub.Teams
	}
	queries, err := client.DepQueries(ctx, repos, owners, teams, cfg.GitHub.DepAuthors)
	if err != nil {
		return err
	}
	if len(queries) == 0 {
		fmt.Println("no dep queries to run (configure starred repos, owners, or teams)")
		return nil
	}
	return runPRQueries(ctx, client, queries)
}

// runPRQueries fires each query concurrently — mirroring the TUI's parallel
// section refresh — and prints each one's query, timing, results, or error.
// By default it uses the REST /search/issues path (the fix for the flaky
// GraphQL search field); pass --graphql to exercise the old path instead.
func runPRQueries(ctx context.Context, client *gh.Client, queries []string) error {
	type prQueryResult struct {
		query string
		prs   []gh.PR
		dur   time.Duration
		err   error
	}
	results := make([]prQueryResult, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			start := time.Now()
			var prs []gh.PR
			var err error
			if useGraphQL {
				prs, err = client.Search(ctx, q)
			} else {
				prs, err = client.SearchIssues(ctx, q)
			}
			results[i] = prQueryResult{query: q, prs: prs, dur: time.Since(start), err: err}
		}(i, q)
	}
	wg.Wait()

	failed := false
	for _, r := range results {
		fmt.Printf("\n── %s ──\n", r.query)
		if r.err != nil {
			fmt.Printf("  ✗ %v (%v)\n", r.err, r.dur.Truncate(time.Millisecond))
			failed = true
			continue
		}
		fmt.Printf("  %d PRs (%v)\n", len(r.prs), r.dur.Truncate(time.Millisecond))
		for _, p := range r.prs {
			fmt.Printf("  #%d %-32s %-12s ci:%-7s merge:%-6s %s  %s\n",
				p.Number, p.Repo, p.Author, p.CIState, p.Mergeable, p.UpdatedAt, p.Title)
		}
	}
	if failed {
		return fmt.Errorf("one or more queries failed")
	}
	return nil
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

	ghClient := gh.NewClient(token)
	ctx := context.Background()

	if len(cfg.Services) == 0 {
		fmt.Println("no services configured — add [[service]] to ~/.config/ship/config.toml")
		return nil
	}

	for _, svc := range cfg.Services {
		if releasesRepo != "" && !strings.HasSuffix(svc.Repo, releasesRepo) {
			continue
		}
		var svcK8s k8s.Client
		if len(mockK8sSpecs) > 0 {
			spec, ok := mockK8sSpecs[svc.Name]
			if !ok {
				spec, ok = mockK8sSpecs[svc.Repo]
			}
			if !ok {
				spec, ok = mockK8sSpecs["*"]
			}
			if !ok {
				fmt.Printf("── %s ──\n  ✗ mock: no spec for service %q (key by name or repo)\n\n", svc.Repo, svc.Name)
				continue
			}
			parsed := k8s.ParseMockSpec(spec)
			svcK8s = k8s.NewMock(map[string]k8s.MockSpec{"*": parsed})
			fmt.Printf("(using mock k8s — service: %s, image: %s)\n\n", svc.Name, parsed.Image)
		} else {
			svcCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			rc, err := k8s.NewRealClient(svcCtx, "", svc.Context, cfg.K8s.LoginCommand)
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
			if t.Title != "" && t.Title != t.Name {
				names[i] = fmt.Sprintf("%s (%s)", t.Name, t.Title)
			} else {
				names[i] = t.Name
			}
		}
		fmt.Printf(" · pending %s", strings.Join(names, ", "))
	}

	if r.AheadBy > 0 {
		fmt.Printf(" · +%d untagged", r.AheadBy)
	}
	fmt.Println()

	for _, c := range r.Commits {
		fmt.Printf("    %s  %s\n", c.SHA[:7], c.Message)
	}
	fmt.Println()
}
