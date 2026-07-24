package github

import (
	"context"
	"fmt"

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

func (c *Client) MyPRs(ctx context.Context, orgs []string) ([]PR, error) {
	user, err := c.User(ctx)
	if err != nil {
		return nil, err
	}
	return c.search(ctx, fmt.Sprintf("is:open is:pr author:%s archived:false", user))
}

func (c *Client) ReviewRequested(ctx context.Context) ([]PR, error) {
	user, err := c.User(ctx)
	if err != nil {
		return nil, err
	}
	return c.search(ctx, fmt.Sprintf("is:open is:pr user-review-requested:%s", user))
}

func (c *Client) AllReviewRequested(ctx context.Context) ([]PR, error) {
	user, err := c.User(ctx)
	if err != nil {
		return nil, err
	}
	return c.search(ctx, fmt.Sprintf("is:open is:pr review-requested:%s", user))
}

func (c *Client) DepPRs(ctx context.Context, repos []string) ([]PR, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	var all []PR
	for _, repo := range repos {
		prs, err := c.search(ctx, fmt.Sprintf("is:open is:pr repo:%s author:app/renovate author:app/dependabot", repo))
		if err != nil {
			return nil, err
		}
		all = append(all, prs...)
	}
	return all, nil
}
