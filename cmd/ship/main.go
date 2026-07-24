package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/zach/ship/internal/config"
	gh "github.com/zach/ship/internal/github"
	"github.com/zach/ship/internal/store"
)

var rootCmd = &cobra.Command{
	Use:   "ship",
	Short: "Personal dev hub — GitHub, prod, and what's next",
	RunE:  runDashboard,
}

var countCmd = &cobra.Command{
	Use:   "count",
	Short: "Print cached PR counts (for shell prompts)",
	Run: func(cmd *cobra.Command, args []string) {
		runCount()
	},
}

func init() {
	rootCmd.AddCommand(countCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runDashboard(cmd *cobra.Command, args []string) error {
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

	client := gh.NewClient(token)
	ctx := context.Background()

	user, _ := client.User(ctx)
	fmt.Printf("ship — logged in as %s\n\n", user)

	myPRs, err := client.MyPRs(ctx, cfg.GitHub.Orgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch my PRs: %v\n", err)
	} else {
		cache := make([]store.CachedPR, len(myPRs))
		for i, p := range myPRs {
			cache[i] = toCached(p, "mine")
		}
		st.SavePRs(cache, "mine")
	}

	reviewPRs, err := client.ReviewRequested(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch review PRs: %v\n", err)
	} else {
		cache := make([]store.CachedPR, len(reviewPRs))
		for i, p := range reviewPRs {
			cache[i] = toCached(p, "review-direct")
		}
		st.SavePRs(cache, "review-direct")
	}

	starred := cfg.StarredRepos()
	depPRs, err := client.DepPRs(ctx, starred)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch dep PRs: %v\n", err)
	} else {
		cache := make([]store.CachedPR, len(depPRs))
		for i, p := range depPRs {
			cache[i] = toCached(p, "dep")
		}
		st.SavePRs(cache, "dep")
	}

	st.UpdateRefresh("github", "ok")

	printPRs(st, "mine", "My PRs")
	printPRs(st, "review-direct", "To Review")
	printPRs(st, "dep", "Dependencies")
	return nil
}

func printPRs(st *store.Store, role, label string) {
	prs, err := st.CachedPRs(role)
	if err != nil || len(prs) == 0 {
		return
	}
	fmt.Printf("── %s ──\n", label)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, p := range prs {
		ci := ciIcon(p.CIState)
		dec := reviewIcon(p.ReviewDecision)
		fmt.Fprintf(w, "  %s %s\t%s\t%s\t%s\n", ci, dec, p.Repo, fmt.Sprintf("#%d", p.Number), p.Title)
	}
	w.Flush()
	fmt.Println()
}

func ciIcon(state string) string {
	switch state {
	case "success":
		return "✓"
	case "failure":
		return "✗"
	case "pending":
		return "…"
	default:
		return "·"
	}
}

func reviewIcon(dec string) string {
	switch dec {
	case "APPROVED":
		return "👍"
	case "CHANGES_REQUESTED":
		return "👎"
	case "REVIEW_REQUIRED":
		return "⏳"
	default:
		return " "
	}
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
