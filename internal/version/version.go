package version

import (
	"context"
	"fmt"

	"github.com/zach/ship/internal/config"
	gh "github.com/zach/ship/internal/github"
	"github.com/zach/ship/internal/k8s"
)

type Result struct {
	Service         config.ServiceConfig
	ProdRef         string // tag or SHA from k8s image
	ProdSHA         string // resolved git SHA
	ProdTag         string // if the prod ref was a tag
	AheadBy         int
	PendingTags     []gh.PendingTag
	UntaggedCommits []gh.CommitSummary
	Commits         []gh.CommitSummary
	Health          k8s.Health
	Error           string
}

func Resolve(ctx context.Context, k8sClient k8s.Client, ghClient *gh.Client, svc config.ServiceConfig) *Result {
	r := &Result{Service: svc}

	branch := svc.Branch
	if branch == "" {
		branch = ghClient.DefaultBranch(ctx, svc.Repo)
	}

	dep, err := k8sClient.GetWorkload(ctx, svc.Context, svc.Namespace, svc.Workload, svc.ResourceType())
	if err != nil {
		r.Error = fmt.Sprintf("%s: k8s: %v", svc.Repo, err)
		return r
	}
	r.Health = dep.Health

	_, tag, err := k8s.ParseImageTag(dep.Image)
	if err != nil {
		r.Error = fmt.Sprintf("%s: image: %v", svc.Repo, err)
		return r
	}
	r.ProdRef = tag

	if sha, ok := k8s.SHAFromImage(tag); ok {
		// image tag is a raw SHA or "sha-<sha>" — use the bare SHA directly,
		// no tag resolution needed.
		r.ProdSHA = sha
	} else {
		r.ProdTag = tag
		sha, err := ghClient.ResolveRef(ctx, svc.Repo, tag)
		if err != nil {
			// tag is not a git ref (e.g. ECR image tag) — show as-is
			r.ProdSHA = tag
			return r
		}
		r.ProdSHA = sha
	}

	compare, err := ghClient.Compare(ctx, svc.Repo, r.ProdSHA, branch)
	if err != nil {
		r.Error = fmt.Sprintf("%s: compare %s...%s: %v", svc.Repo, r.ProdSHA[:7], branch, err)
		return r
	}
	r.AheadBy = compare.AheadBy
	r.Commits = compare.Commits

	// sha versioning has no release train: every merge is itself a deployable
	// SHA, so git tags are never pending. Skip tag detection entirely and let
	// the untagged-commit list stand in for "what's waiting to ship".
	if svc.VersionStrategy != "sha" {
		pending, err := ghClient.PendingTags(ctx, svc.Repo, r.ProdSHA, branch, svc.ReleaseSource, r.ProdTag, compare.Commits)
		if err != nil {
			// non-fatal, just no pending tags
			return r
		}
		r.PendingTags = pending
	}

	if !svc.SkipUntagged {
		untagged, err := ghClient.UntaggedFirstParent(ctx, svc.Repo, r.ProdSHA, branch, compare.Commits)
		if err == nil {
			r.UntaggedCommits = untagged
		}
	}

	return r
}
