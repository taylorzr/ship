package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
	State          string // "OPEN", "CLOSED", "MERGED"
	UpdatedAt      string
	IsDraft        bool
	HeadRefOid     string
}

type Client struct {
	gql  *githubv4.Client
	user string
}

func NewClient(token string) *Client {
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	http := oauth2.NewClient(context.Background(), src)
	http.Timeout = 30 * time.Second
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
		State          githubv4.PullRequestState
		Author         struct{ Login string }
		ReviewDecision string
		Mergeable      githubv4.MergeableState
		UpdatedAt      githubv4.DateTime
		HeadRefOid     string
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

type getPRResult struct {
	Repository struct {
		PullRequest struct {
			Number         int
			Title          string
			URL            string
			IsDraft        bool
			State          githubv4.PullRequestState
			Author         struct{ Login string }
			ReviewDecision string
			Mergeable      githubv4.MergeableState
			UpdatedAt      githubv4.DateTime
			HeadRefOid     string
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
		} `graphql:"pullRequest(number: $number)"`
	} `graphql:"repository(owner: $owner, name: $name)"`
}

func (c *Client) search(ctx context.Context, query string) ([]PR, error) {
	var lastErr error
	backoff := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

	for attempt := range 4 {
		var all []PR
		var after *githubv4.String
		lastErr = nil
		ok := true

	retryLoop:
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
				lastErr = err
				ok = false
				break retryLoop
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
					State:          string(pr.State),
					UpdatedAt:      pr.UpdatedAt.Format("2006-01-02T15:04:05Z"),
					IsDraft:        pr.IsDraft,
					HeadRefOid:     pr.HeadRefOid,
				})
			}

			if !q.Search.PageInfo.HasNextPage || len(q.Search.Nodes) == 0 {
				break retryLoop
			}
			after = &q.Search.PageInfo.EndCursor
		}

		if ok {
			return all, nil
		}

		if attempt == 3 {
			break
		}

		// only retry on 5xx errors
		errStr := lastErr.Error()
		if !strings.Contains(errStr, "502") && !strings.Contains(errStr, "503") &&
			!strings.Contains(errStr, "504") && !strings.Contains(errStr, "5") {
			return nil, lastErr
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff[attempt]):
		}
	}
	return nil, fmt.Errorf("github search %q: %w", query, lastErr)
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

func (c *Client) ownerFilter(ctx context.Context, owners []string) string {
	if len(owners) == 0 {
		return ""
	}
	var b strings.Builder
	for _, o := range owners {
		if c.OwnerType(ctx, o) == "user" {
			b.WriteString(" user:")
		} else {
			b.WriteString(" org:")
		}
		b.WriteString(o)
	}
	return b.String()
}

func (c *Client) MyPRs(ctx context.Context, owners []string) ([]PR, error) {
	user, err := c.User(ctx)
	if err != nil {
		return nil, err
	}
	return c.search(ctx, fmt.Sprintf("is:open is:pr author:%s archived:false%s", user, c.ownerFilter(ctx, owners)))
}

func (c *Client) ReviewRequested(ctx context.Context, owners []string) ([]PR, error) {
	user, err := c.User(ctx)
	if err != nil {
		return nil, err
	}
	return c.search(ctx, fmt.Sprintf("is:open is:pr user-review-requested:%s%s", user, c.ownerFilter(ctx, owners)))
}

func (c *Client) TeamReviewRequested(ctx context.Context, teams []string) ([]PR, error) {
	if len(teams) == 0 {
		return nil, nil
	}
	var q strings.Builder
	q.WriteString("is:open is:pr")
	for _, t := range teams {
		q.WriteString(" team-review-requested:")
		q.WriteString(t)
	}
	return c.search(ctx, q.String())
}

func (c *Client) AllReviewRequested(ctx context.Context) ([]PR, error) {
	user, err := c.User(ctx)
	if err != nil {
		return nil, err
	}
	return c.search(ctx, fmt.Sprintf("is:open is:pr review-requested:%s", user))
}

// GetPR fetches a single pull request by repo and number, returning the same
// shape as the search results so it can be written to the store.
func (c *Client) GetPR(ctx context.Context, repo string, number int) (*PR, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repo %q", repo)
	}
	var q getPRResult
	variables := map[string]any{
		"owner":  githubv4.String(parts[0]),
		"name":   githubv4.String(parts[1]),
		"number": githubv4.Int(number),
	}
	if err := c.gql.Query(ctx, &q, variables); err != nil {
		return nil, err
	}
	pr := q.Repository.PullRequest
	if pr.Number == 0 {
		return nil, fmt.Errorf("PR #%d not found in %s", number, repo)
	}
	return &PR{
		Number:         pr.Number,
		Repo:           pr.Repository.NameWithOwner,
		Title:          pr.Title,
		Author:         pr.Author.Login,
		URL:            pr.URL,
		ReviewDecision: string(pr.ReviewDecision),
		CIState:        resolveCI(pr.Commits),
		Mergeable:      string(pr.Mergeable),
		State:          string(pr.State),
		UpdatedAt:      pr.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		IsDraft:        pr.IsDraft,
		HeadRefOid:     pr.HeadRefOid,
	}, nil
}

func (c *Client) DepPRs(ctx context.Context, repos, owners, teams, authors []string) ([]PR, error) {
	var authorQ strings.Builder
	for _, a := range authors {
		authorQ.WriteString(" author:")
		authorQ.WriteString(a)
	}
	// GitHub can't OR different qualifier types in one query, so each scope
	// is a separate search and the results are unioned. When teams are
	// configured, skip the org-wide owner search — it returns too many
	// results across the entire org, and team-review-requested already
	// provides the right scoping.
	var queries []string
	for _, repo := range repos {
		queries = append(queries, fmt.Sprintf("is:open is:pr repo:%s%s", repo, authorQ.String()))
	}
	if len(teams) == 0 {
		for _, o := range owners {
			qualifier := " org:" + o
			if c.OwnerType(ctx, o) == "user" {
				qualifier = " user:" + o
			}
			queries = append(queries, fmt.Sprintf("is:open is:pr%s%s", qualifier, authorQ.String()))
		}
	}
	for _, t := range teams {
		queries = append(queries, fmt.Sprintf("is:open is:pr team-review-requested:%s%s", t, authorQ.String()))
	}
	seen := make(map[string]bool)
	var all []PR
	for _, q := range queries {
		prs, err := c.search(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, p := range prs {
			key := fmt.Sprintf("%s#%d", p.Repo, p.Number)
			if !seen[key] {
				seen[key] = true
				all = append(all, p)
			}
		}
	}
	return all, nil
}

func (c *Client) GetHeadSha(ctx context.Context, repo string, number int) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "pr", "view", fmt.Sprintf("%d", number),
		"-R", repo, "--json", "headRefOid", "--jq", ".headRefOid").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("get head sha: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

type CompareResult struct {
	AheadBy  int
	Commits  []CommitSummary
}

type CommitSummary struct {
	SHA          string
	Message      string
	Author       string
	Parents      []string
	Contributors []string // unique commit authors covered by this version
}

// VersionContributors returns the unique commit authors that make up a version
// starting at startSHA. A tag or merge commit spans multiple commits, so the
// walk follows all parents down to the previous tagged commit (or prodSHA),
// collecting every author in that range. stopSHA (optional) hard-stops the walk
// without including it, used to delimit consecutive untagged commits.
//
// Commits provided in the commits slice are used as-is; commits outside it
// (e.g. tags pointing at a non-default branch) are fetched on demand.
func (c *Client) VersionContributors(ctx context.Context, repo, startSHA, stopSHA, prodSHA string, commits []CommitSummary, tagged map[string]bool) []string {
	if startSHA == "" || startSHA == prodSHA {
		return nil
	}
	bySHA := make(map[string]CommitSummary, len(commits))
	for _, cm := range commits {
		bySHA[cm.SHA] = cm
	}
	var out []string
	seen := make(map[string]bool)
	var walk func(string)
	walk = func(cur string) {
		if cur == "" || cur == stopSHA || cur == prodSHA || seen[cur] {
			return
		}
		seen[cur] = true
		cm, ok := bySHA[cur]
		if !ok {
			fetched, err := c.commitSummary(ctx, repo, cur)
			if err != nil {
				return
			}
			cm = fetched
		}
		if cur != startSHA && tagged[cur] {
			return // previous version boundary
		}
		if cm.Author != "" && !contains(out, cm.Author) {
			out = append(out, cm.Author)
		}
		for _, p := range cm.Parents {
			walk(p)
		}
	}
	walk(startSHA)
	return out
}

// commitSummary fetches a single commit's summary (author + parents) from the
// GitHub API. Used by VersionContributors for commits outside the prod-to-branch
// compare range.
func (c *Client) commitSummary(ctx context.Context, repo, sha string) (CommitSummary, error) {
	out, err := exec.CommandContext(ctx, "gh", "api", fmt.Sprintf("repos/%s/commits/%s", repo, sha), "--jq", "{sha, parents: [.parents[].sha], author: (.author.login // \"\"), name: (.commit.author.name // \"\")}").CombinedOutput()
	if err != nil {
		return CommitSummary{}, fmt.Errorf("gh api commit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var resp struct {
		SHA     string   `json:"sha"`
		Parents []string `json:"parents"`
		Author  string   `json:"author"`
		Name    string   `json:"name"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return CommitSummary{}, fmt.Errorf("parse commit: %w", err)
	}
	return CommitSummary{
		SHA:     resp.SHA,
		Author:  resp.Author,
		Parents: resp.Parents,
	}, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (c *Client) Compare(ctx context.Context, repo, base, head string) (*CompareResult, error) {
	out, err := exec.CommandContext(ctx, "gh", "api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, base, head)).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api compare: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var resp struct {
		AheadBy  int `json:"ahead_by"`
		Commits  []struct {
			SHA     string `json:"sha"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
			Commit struct {
				Message  string `json:"message"`
				Author   struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"commit"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse compare: %w", err)
	}
	result := &CompareResult{AheadBy: resp.AheadBy}
	for _, c := range resp.Commits {
		cs := CommitSummary{
			SHA:     c.SHA,
			Message: strings.Split(c.Commit.Message, "\n")[0],
		}
		// Prefer the GitHub login so contributors match release authors
		// (also logins); fall back to the commit author name.
		cs.Author = c.Author.Login
		if cs.Author == "" {
			cs.Author = c.Commit.Author.Name
		}
		for _, p := range c.Parents {
			cs.Parents = append(cs.Parents, p.SHA)
		}
		result.Commits = append(result.Commits, cs)
	}
	return result, nil
}

type TagInfo struct {
	Name string
	SHA  string
}

func (c *Client) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "api", fmt.Sprintf("repos/%s/git/ref/tags/%s", repo, ref), "--jq", ".object.sha").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve ref %s: %s: %w", ref, strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveCommit resolves a short or full SHA to the full SHA via the
// commits API. Needed because `gh release create --target` does not resolve
// short SHAs and falls back to the branch HEAD.
func (c *Client) ResolveCommit(ctx context.Context, repo, sha string) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "api", fmt.Sprintf("repos/%s/commits/%s", repo, sha), "--jq", ".sha").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve commit %s: %s: %w", sha, strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *Client) DefaultBranch(ctx context.Context, repo string) string {
	out, err := exec.CommandContext(ctx, "gh", "api", fmt.Sprintf("repos/%s", repo), "--jq", ".default_branch").CombinedOutput()
	if err != nil {
		return "main"
	}
	return strings.TrimSpace(string(out))
}

func (c *Client) ListTags(ctx context.Context, repo string) ([]TagInfo, error) {
	out, err := exec.CommandContext(ctx, "gh", "api", fmt.Sprintf("repos/%s/git/refs/tags?per_page=100", repo)).CombinedOutput()
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
		out, err := exec.CommandContext(ctx, "gh", "api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, tag.SHA, branch),
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
	Name          string
	Title         string
	ReleaseAuthor string
	Contributors  []string
}

func (c *Client) PendingTags(ctx context.Context, repo, prodSHA, branch, source, prodTag string, ahead []CommitSummary) ([]PendingTag, error) {
	var pending []PendingTag
	var err error

	if source == "tags" {
		pending, err = c.pendingTagsFromGit(ctx, repo, prodSHA)
	} else {
		var releasesExist bool
		pending, releasesExist, err = c.pendingTagsFromReleases(ctx, repo, prodSHA, prodTag)
		if err != nil && source == "" {
			tag, tagErr := c.pendingTagsFromGit(ctx, repo, prodSHA)
			if tagErr == nil && len(tag) > 0 {
				pending = tag
				err = nil
			}
		} else if err == nil && source == "" && !releasesExist {
			tag, tagErr := c.pendingTagsFromGit(ctx, repo, prodSHA)
			if tagErr == nil && len(tag) > 0 {
				pending = tag
			}
		}
	}
	if err != nil {
		return nil, err
	}

	// The releases list API is eventually consistent and can briefly miss a
	// just-created release, while git refs are updated immediately. Reconcile:
	// any tag pointing directly at a commit ahead of prod that isn't already
	// listed must be pending.
	var tags []TagInfo
	if list, tagErr := c.ListTags(ctx, repo); tagErr == nil && len(list) > 0 {
		tags = list
		aheadSet := make(map[string]bool, len(ahead))
		for _, cm := range ahead {
			aheadSet[cm.SHA] = true
		}
		have := make(map[string]bool, len(pending))
		for _, p := range pending {
			have[p.Name] = true
		}
		for _, t := range tags {
			if have[t.Name] {
				continue
			}
			if aheadSet[t.SHA] {
				pending = append(pending, PendingTag{Name: t.Name})
				have[t.Name] = true
			}
		}
	}

	// Attach contributors: each pending tag is a version spanning its own
	// commits down to the previous tagged commit.
	if len(tags) > 0 {
		tagged := make(map[string]bool, len(tags))
		tagSHA := make(map[string]string, len(tags))
		for _, t := range tags {
			tagged[t.SHA] = true
			tagSHA[t.Name] = t.SHA
		}
		for i := range pending {
			sha, ok := tagSHA[pending[i].Name]
			if !ok {
				continue
			}
			pending[i].Contributors = c.VersionContributors(ctx, repo, sha, "", prodSHA, ahead, tagged)
		}
	}

	return pending, nil
}

func (c *Client) pendingTagsFromReleases(ctx context.Context, repo, prodSHA, prodTag string) ([]PendingTag, bool, error) {
	type ghRelease struct {
		TagName    string `json:"tag_name"`
		Name       string `json:"name"`
		Prerelease bool   `json:"prerelease"`
		CreatedAt  string `json:"created_at"`
		Author     struct {
			Login string `json:"login"`
		} `json:"author"`
	}

	var allReleases []ghRelease
	page := 1

	for {
		out, err := exec.CommandContext(ctx, "gh", "api",
			fmt.Sprintf("repos/%s/releases?per_page=100&page=%d", repo, page)).CombinedOutput()
		if err != nil {
			return nil, false, fmt.Errorf("list releases: %s: %w", strings.TrimSpace(string(out)), err)
		}

		var releases []ghRelease
		if err := json.Unmarshal(out, &releases); err != nil {
			return nil, false, fmt.Errorf("parse releases: %w", err)
		}
		if len(releases) == 0 {
			break
		}

		allReleases = append(allReleases, releases...)
		page++
	}

	if len(allReleases) == 0 {
		return nil, false, nil
	}

	// If we have a known prod tag, use its creation time to determine what's pending.
	if prodTag != "" {
		var prodTime time.Time
		found := false
		for _, rel := range allReleases {
			if rel.TagName == prodTag {
				t, err := time.Parse(time.RFC3339, rel.CreatedAt)
				if err == nil {
					prodTime = t
					found = true
				}
				break
			}
		}
		if found {
			var pending []PendingTag
			for _, rel := range allReleases {
				if rel.Prerelease || rel.TagName == prodTag {
					continue
				}
				t, err := time.Parse(time.RFC3339, rel.CreatedAt)
				if err != nil {
					continue
				}
				if t.After(prodTime) {
					pending = append(pending, PendingTag{Name: rel.TagName, Title: rel.Name, ReleaseAuthor: rel.Author.Login})
				}
			}
			return pending, true, nil
		}
	}

	// Without a prod tag or prod tag not found among releases, we can't do
	// reliable ordering. Return empty — caller decides fallback strategy.
	return nil, true, nil
}

func (c *Client) pendingTagsFromGit(ctx context.Context, repo, prodSHA string) ([]PendingTag, error) {
	tags, err := c.ListTags(ctx, repo)
	if err != nil {
		return nil, err
	}

	var pending []PendingTag
	for _, tag := range tags {
		if tag.SHA == prodSHA {
			continue
		}
		comp, err := c.Compare(ctx, repo, prodSHA, tag.Name)
		if err != nil {
			continue
		}
		if comp.AheadBy > 0 {
			pending = append(pending, PendingTag{Name: tag.Name})
		}
	}
	return pending, nil
}

func (c *Client) UntaggedFirstParent(ctx context.Context, repo, prodSHA, branch string, commits []CommitSummary) ([]CommitSummary, error) {
	if len(commits) == 0 {
		return nil, nil
	}

	tags, err := c.ListTags(ctx, repo)
	if err != nil {
		return nil, err
	}

	tagged := make(map[string]bool, len(tags))
	for _, t := range tags {
		tagged[t.SHA] = true
	}

	commitMap := make(map[string]CommitSummary, len(commits))
	for _, cm := range commits {
		commitMap[cm.SHA] = cm
	}

	var untagged []CommitSummary
	cur := commits[len(commits)-1].SHA
	for cur != "" {
		cm, ok := commitMap[cur]
		if !ok {
			break
		}
		// Stop at a tagged commit: everything below it is an ancestor of the
		// tag and already covered by it.
		if tagged[cur] {
			break
		}
		untagged = append(untagged, cm)
		if len(cm.Parents) > 0 {
			cur = cm.Parents[0]
		} else {
			break
		}
	}

	// Attach contributors per commit: each merge/squash commit is its own
	// version, spanning its ancestors down to the next untagged commit, the
	// previous tag, or prod.
	for i := range untagged {
		stop := ""
		if i+1 < len(untagged) {
			stop = untagged[i+1].SHA
		}
		untagged[i].Contributors = c.VersionContributors(ctx, repo, untagged[i].SHA, stop, prodSHA, commits, tagged)
	}

	return untagged, nil
}

func (c *Client) MergePR(ctx context.Context, repo string, number int) error {
	out, err := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/%s/pulls/%d/merge", repo, number),
		"-X", "PUT",
		"-F", "merge_method=squash",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("merge PR #%d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (c *Client) ClosePR(ctx context.Context, repo string, number int) error {
	out, err := exec.CommandContext(ctx, "gh", "api",
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
	var args []string
	if isDraft {
		args = []string{"pr", "ready"}
	} else {
		args = []string{"pr", "ready", "--undo"}
	}
	args = append(args, fmt.Sprintf("%d", number), "-R", repo)
	out, err := exec.CommandContext(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		action := "convert to draft"
		if isDraft {
			action = "mark ready"
		}
		return fmt.Errorf("%s PR #%d: %s: %w", action, number, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (c *Client) LatestTag(ctx context.Context, repo string) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/%s/tags?per_page=1", repo),
		"--jq", ".[0].name",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("latest tag: %w", err)
	}
	tag := strings.TrimSpace(string(out))
	if tag == "" || tag == "null" {
		return "", nil
	}
	return tag, nil
}

func (c *Client) RepoHasReleases(ctx context.Context, repo string) (bool, error) {
	out, err := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/%s/releases?per_page=1", repo),
		"--jq", "length",
	).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("check releases: %w", err)
	}
	return strings.TrimSpace(string(out)) != "0", nil
}

func logToFile(format string, args ...any) {
	home, _ := os.UserHomeDir()
	if f, err := os.OpenFile(filepath.Join(home, ".config", "ship", "ship.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, format, args...)
		f.Close()
	}
}

func (c *Client) CreateRelease(ctx context.Context, repo, tag, sha string) error {
	full, err := c.ResolveCommit(ctx, repo, sha)
	if err != nil {
		return err
	}
	args := []string{"release", "create", tag, "--target", full, "--generate-notes", "-R", repo}
	logToFile("CreateRelease: gh %s (input sha=%s, resolved sha=%s)\n", strings.Join(args, " "), sha, full)
	out, err := exec.CommandContext(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create release: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (c *Client) CreateTag(ctx context.Context, repo, tag, sha string) error {
	full, err := c.ResolveCommit(ctx, repo, sha)
	if err != nil {
		return err
	}
	args := []string{"api", fmt.Sprintf("repos/%s/git/refs", repo), "-X", "POST",
		"-F", fmt.Sprintf("ref=refs/tags/%s", tag),
		"-F", fmt.Sprintf("sha=%s", full),
	}
	logToFile("CreateTag: gh %s (input sha=%s, resolved sha=%s)\n", strings.Join(args, " "), sha, full)
	out, err := exec.CommandContext(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create tag: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (c *Client) OwnerType(ctx context.Context, owner string) string {
	out, err := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("users/%s", owner),
		"--jq", ".type",
	).CombinedOutput()
	if err != nil {
		return "org"
	}
	t := strings.TrimSpace(string(out))
	if t == "User" {
		return "user"
	}
	return "org"
}
