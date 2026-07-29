package version

import (
	"context"
	"fmt"

	"github.com/zach/ship/internal/config"
	gh "github.com/zach/ship/internal/github"
	"github.com/zach/ship/internal/k8s"
)

type Result struct {
	Service        config.ServiceConfig
	ProdRef        string // tag or SHA from k8s image
	ProdSHA        string // resolved git SHA
	ProdTag        string // if the prod ref was a tag
	AheadBy        int
	PendingTags    []gh.PendingTag
	UntaggedCommits []gh.CommitSummary
	Commits        []gh.CommitSummary
	Error          string
}

func Resolve(ctx context.Context, k8sClient k8s.Client, ghClient *gh.Client, svc config.ServiceConfig) *Result {
	r := &Result{Service: svc}

	branch := svc.Branch
	if branch == "" {
		branch = "main"
	}

	dep, err := k8sClient.GetDeployment(ctx, svc.Context, svc.Namespace, svc.Workload)
	if err != nil {
		r.Error = fmt.Sprintf("%s: k8s: %v", svc.Repo, err)
		return r
	}

	_, tag, err := k8s.ParseImageTag(dep.Image)
	if err != nil {
		r.Error = fmt.Sprintf("%s: image: %v", svc.Repo, err)
		return r
	}
	r.ProdRef = tag

	if k8s.LooksLikeSHA(tag) {
		r.ProdSHA = tag
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

	pending, err := ghClient.PendingTags(ctx, svc.Repo, r.ProdSHA, branch, svc.ReleaseSource)
	if err != nil {
		// non-fatal, just no pending tags
		return r
	}
	r.PendingTags = pending

	if !svc.SkipUntagged {
		untagged, err := ghClient.UntaggedFirstParent(ctx, svc.Repo, r.ProdSHA, branch, compare.Commits)
		if err == nil {
			r.UntaggedCommits = untagged
		}
	}

	return r
}
