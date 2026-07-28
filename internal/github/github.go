package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

type PR struct {
	Number         int
	Repo           string
	Title          string
	Author         string
	Role           string
	URL            string
	ReviewDecision string
	CIState        string
	Mergeable      string
	UpdatedAt      string
	IsDraft        bool
}

type Client struct {
	gql  *githubv4.Client
	user string
}

func NewClient(token string) *Client {
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	http := oauth2.NewClient(context.Background(), src)
	gql := githubv4.NewClient(http)
	return &Client{gql: gql}
}

func (c *Client) User(ctx context.Context) (string, error) {
	if c.user != "" {
		return c.user, nil
	}
	var q struct {
		Viewer struct {
			Login string
		}
	}
	if err := c.gql.Query(ctx, &q, nil); err != nil {
		return "", err
	}
	c.user = q.Viewer.Login
	return c.user, nil
}

type checkNode struct {
	CheckRun struct {
		Status string
		Conclusion string
	} `graphql:"... on CheckRun"`
}

type statusNode struct {
	StatusContext struct {
		State string
	} `graphql:"... on StatusContext"`
}

type prNode struct {
	PullRequest struct {
		Number         int
		Title          string
		URL            string
		IsDraft        bool
		Author         struct{ Login string }
		ReviewDecision string
		Mergeable      githubv4.MergeableState
		UpdatedAt      githubv4.DateTime
		Repository     struct{ NameWithOwner string }
		Commits        struct {
			Nodes []struct {
				Commit struct {
					StatusCheckRollup struct {
						Contexts struct {
							Nodes []struct {
								checkNode
								statusNode
							}
						} `graphql:"contexts(last: 20)"`
					}
				}
			}
		} `graphql:"commits(last: 1)"`
	} `graphql:"... on PullRequest"`
}

type searchResult struct {
	Search struct {
		PageInfo struct {
			EndCursor   githubv4.String
			HasNextPage bool
		}
		Nodes []prNode
	} `graphql:"search(query: $query, type: ISSUE, first: $first, after: $after)"`
}

func (c *Client) search(ctx context.Context, query string) ([]PR, error) {
	var all []PR
	var after *githubv4.String

	for {
		var q searchResult
		variables := map[string]any{
			"query": githubv4.String(query),
			"first": githubv4.Int(50),
			"after": (*githubv4.String)(nil),
		}
		if after != nil {
			variables["after"] = after
		}

		if err := c.gql.Query(ctx, &q, variables); err != nil {
			return nil, fmt.Errorf("github search %q: %w", query, err)
		}

		for _, n := range q.Search.Nodes {
			pr := n.PullRequest
			ciState := resolveCI(pr.Commits)
			all = append(all, PR{
				Number:         pr.Number,
				Repo:           pr.Repository.NameWithOwner,
				Title:          pr.Title,
				Author:         pr.Author.Login,
				URL:            pr.URL,
				ReviewDecision: string(pr.ReviewDecision),
				CIState:        ciState,
				Mergeable:      string(pr.Mergeable),
				UpdatedAt:      pr.UpdatedAt.Format("2006-01-02T15:04:05Z"),
				IsDraft:        pr.IsDraft,
			})
		}

		if !q.Search.PageInfo.HasNextPage || len(q.Search.Nodes) == 0 {
			break
		}
		after = &q.Search.PageInfo.EndCursor
	}
	return all, nil
}

func resolveCI(commits struct {
	Nodes []struct {
		Commit struct {
			StatusCheckRollup struct {
				Contexts struct {
					Nodes []struct {
						checkNode
						statusNode
					}
				} `graphql:"contexts(last: 20)"`
			}
		}
	}
}) string {
	nodes := commits.Nodes
	if len(nodes) == 0 {
		return "none"
	}
	ctxs := nodes[0].Commit.StatusCheckRollup.Contexts.Nodes
	if len(ctxs) == 0 {
		return "none"
	}
	for _, c := range ctxs {
		state := c.StatusContext.State
		if state == "" {
			// CheckRun: use Conclusion (overrides Status)
			if c.CheckRun.Conclusion == "FAILURE" || c.CheckRun.Conclusion == "TIMED_OUT" || c.CheckRun.Conclusion == "ACTION_REQUIRED" {
				return "failure"
			}
			if c.CheckRun.Status == "IN_PROGRESS" || c.CheckRun.Status == "QUEUED" || c.CheckRun.Status == "WAITING" {
				return "pending"
			}
		} else {
			if state == "FAILURE" || state == "ERROR" {
				return "failure"
			}
			if state == "PENDING" || state == "QUEUED" || state == "IN_PROGRESS" {
				return "pending"
			}
		}
	}
	return "success"
}

func ownerFilter(owners []string) string {
	if len(owners) == 0 {
		return ""
	}
	var b strings.Builder
	for _, o := range owners {
		b.WriteString(" user:")
		b.WriteString(o)
	}
	return b.String()
}

func (c *Client) MyPRs(ctx context.Context, owners []string) ([]PR, error) {
	user, err := c.User(ctx)
	if err != nil {
		return nil, err
	}
	return c.search(ctx, fmt.Sprintf("is:open is:pr author:%s archived:false%s", user, ownerFilter(owners)))
}

func (c *Client) ReviewRequested(ctx context.Context, owners []string) ([]PR, error) {
	user, err := c.User(ctx)
	if err != nil {
		return nil, err
	}
	return c.search(ctx, fmt.Sprintf("is:open is:pr user-review-requested:%s%s", user, ownerFilter(owners)))
}

func (c *Client) AllReviewRequested(ctx context.Context) ([]PR, error) {
	user, err := c.User(ctx)
	if err != nil {
		return nil, err
	}
	return c.search(ctx, fmt.Sprintf("is:open is:pr review-requested:%s", user))
}

func (c *Client) DepPRs(ctx context.Context, repos []string, authors []string) ([]PR, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	var authorQ strings.Builder
	for i, a := range authors {
		if i > 0 {
			authorQ.WriteString(" ")
		}
		authorQ.WriteString("author:")
		authorQ.WriteString(a)
	}
	var all []PR
	for _, repo := range repos {
		prs, err := c.search(ctx, fmt.Sprintf("is:open is:pr repo:%s %s", repo, authorQ.String()))
		if err != nil {
			return nil, err
		}
		all = append(all, prs...)
	}
	return all, nil
}

type CompareResult struct {
	AheadBy  int
	Commits  []CommitSummary
}

type CommitSummary struct {
	SHA     string
	Message string
	Author  string
}

func (c *Client) Compare(ctx context.Context, repo, base, head string) (*CompareResult, error) {
	out, err := exec.Command("gh", "api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, base, head)).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api compare: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var resp struct {
		AheadBy  int `json:"ahead_by"`
		Commits  []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message  string `json:"message"`
				Author   struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"commit"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse compare: %w", err)
	}
	result := &CompareResult{AheadBy: resp.AheadBy}
	for _, c := range resp.Commits {
		result.Commits = append(result.Commits, CommitSummary{
			SHA:     c.SHA[:7],
			Message: strings.Split(c.Commit.Message, "\n")[0],
			Author:  c.Commit.Author.Name,
		})
	}
	return result, nil
}

type TagInfo struct {
	Name string
	SHA  string
}

func (c *Client) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	out, err := exec.Command("gh", "api", fmt.Sprintf("repos/%s/git/ref/refs/tags/%s", repo, ref), "--jq", ".object.sha").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve ref %s: %s: %w", ref, strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *Client) ListTags(ctx context.Context, repo string) ([]TagInfo, error) {
	out, err := exec.Command("gh", "api", fmt.Sprintf("repos/%s/git/refs/tags?per_page=100", repo)).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list tags: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var resp []struct {
		Ref  string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse tags: %w", err)
	}
	var tags []TagInfo
	for _, t := range resp {
		name := strings.TrimPrefix(t.Ref, "refs/tags/")
		tags = append(tags, TagInfo{Name: name, SHA: t.Object.SHA})
	}
	return tags, nil
}

func (c *Client) ListTagsReachableFrom(ctx context.Context, repo, branch string) ([]TagInfo, error) {
	allTags, err := c.ListTags(ctx, repo)
	if err != nil {
		return nil, err
	}

	var result []TagInfo
	for _, tag := range allTags {
		// check if tag commit is reachable from branch using merge-base
		out, err := exec.Command("gh", "api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, tag.SHA, branch),
			"--jq", ".status").CombinedOutput()
		if err != nil {
			continue
		}
		status := strings.TrimSpace(string(out))
		if status == "identical" || status == "behind" {
			result = append(result, tag)
		}
	}
	return result, nil
}

type PendingTag struct {
	Name string
	SHA  string
}

func (c *Client) PendingTags(ctx context.Context, repo, prodSHA, branch string) ([]PendingTag, error) {
	tags, err := c.ListTags(ctx, repo)
	if err != nil {
		return nil, err
	}

	var pending []PendingTag
	for _, tag := range tags {
		// skip the tag that matches prod
		if tag.SHA == prodSHA {
			continue
		}
		// check if this tag is ahead of prod (i.e. contains commits not in prod)
		comp, err := c.Compare(ctx, repo, prodSHA, tag.Name)
		if err != nil {
			continue
		}
		if comp.AheadBy > 0 {
			pending = append(pending, PendingTag{Name: tag.Name, SHA: tag.SHA[:7]})
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		return pending[i].Name < pending[j].Name
	})
	return pending, nil
}

func (c *Client) MergePR(ctx context.Context, repo string, number int) error {
	out, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%d/merge", repo, number),
		"-f", "merge_method=squash",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("merge PR #%d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (c *Client) ClosePR(ctx context.Context, repo string, number int) error {
	out, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%d", repo, number),
		"-X", "PATCH",
		"-f", "state=closed",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("close PR #%d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (c *Client) ToggleDraft(ctx context.Context, repo string, number int, isDraft bool) error {
	if isDraft {
		out, err := exec.Command("gh", "pr", "ready",
			fmt.Sprintf("%d", number),
			"-R", repo,
		).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mark PR #%d ready: %s: %w", number, strings.TrimSpace(string(out)), err)
		}
		return nil
	}
	out, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%d", repo, number),
		"-X", "PATCH",
		"-f", "draft=true",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("convert PR #%d to draft: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	return nil
}
