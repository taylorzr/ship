package version

import (
	"context"
	"fmt"

	"github.com/zach/ship/internal/config"
	gh "github.com/zach/ship/internal/github"
	"github.com/zach/ship/internal/k8s"
)

type Result struct {
	Service      config.ServiceConfig
	ProdRef      string // tag or SHA from k8s image
	ProdSHA      string // resolved git SHA
	ProdTag      string // if the prod ref was a tag
	AheadBy      int
	PendingTags  []gh.PendingTag
	Commits      []gh.CommitSummary
	Error        string
}

func Resolve(ctx context.Context, k8sClient k8s.Client, ghClient *gh.Client, svc config.ServiceConfig) *Result {
	r := &Result{Service: svc}

	branch := svc.Branch
	if branch == "" {
		branch = "main"
	}

	dep, err := k8sClient.GetDeployment(ctx, svc.Context, svc.Namespace, svc.Workload)
	if err != nil {
		r.Error = fmt.Sprintf("k8s: %v", err)
		return r
	}

	_, tag, err := k8s.ParseImageTag(dep.Image)
	if err != nil {
		r.Error = fmt.Sprintf("image: %v", err)
		return r
	}
	r.ProdRef = tag

	if k8s.LooksLikeSHA(tag) {
		r.ProdSHA = tag
	} else {
		r.ProdTag = tag
		sha, err := ghClient.ResolveRef(ctx, svc.Repo, tag)
		if err != nil {
			r.Error = fmt.Sprintf("resolve %s: %v", tag, err)
			return r
		}
		r.ProdSHA = sha
	}

	compare, err := ghClient.Compare(ctx, svc.Repo, r.ProdSHA, branch)
	if err != nil {
		r.Error = fmt.Sprintf("compare: %v", err)
		return r
	}
	r.AheadBy = compare.AheadBy
	r.Commits = compare.Commits

	pending, err := ghClient.PendingTags(ctx, svc.Repo, r.ProdSHA, branch)
	if err != nil {
		// non-fatal, just no pending tags
		return r
	}
	r.PendingTags = pending

	return r
}
