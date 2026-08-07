package notify

import (
	"encoding/json"
	"testing"
)

func addPR(s Snapshot, repo string, num int, role string, p PRState) Snapshot {
	if s.PRs == nil {
		s.PRs = map[string]PRState{}
	}
	p.Role = role
	s.PRs[PRKey(repo, num, role)] = p
	return s
}

func addVersion(s Snapshot, repo string, v VersionState) Snapshot {
	if s.Versions == nil {
		s.Versions = map[string]VersionState{}
	}
	s.Versions[repo] = v
	return s
}

func addActivity(s Snapshot, repo string, num int, a ActivityState) Snapshot {
	if s.Activity == nil {
		s.Activity = map[string]ActivityState{}
	}
	s.Activity[PRKey(repo, num, "mine")] = a
	return s
}

func kinds(events []Event) []Kind {
	var out []Kind
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func has(kinds []Kind, k Kind) bool {
	for _, v := range kinds {
		if v == k {
			return true
		}
	}
	return false
}

func TestDiffNoChange(t *testing.T) {
	prev := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", Review: "APPROVED", CI: "success", MergeState: "CLEAN"})
	cur := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", Review: "APPROVED", CI: "success", MergeState: "CLEAN"})
	if ev := Diff(prev, cur); len(ev) != 0 {
		t.Fatalf("expected no events, got %v", ev)
	}
}

func TestNewReviewRequest(t *testing.T) {
	prev := Snapshot{}
	cur := addPR(Snapshot{}, "org/repo", 2, "review-direct", PRState{Title: "T"})
	ev := Diff(prev, cur)
	if !has(kinds(ev), KindReviewRequested) {
		t.Fatalf("expected review-requested event, got %v", kinds(ev))
	}
	// a review-team PR promoted to direct also fires
	prev2 := addPR(Snapshot{}, "org/repo", 2, "review-team", PRState{})
	if !has(kinds(Diff(prev2, cur)), KindReviewRequested) {
		t.Fatal("expected review-requested on team->direct promotion")
	}
}

func TestNewDepPR(t *testing.T) {
	cur := addPR(Snapshot{}, "org/repo", 3, "dep", PRState{Title: "d"})
	if !has(kinds(Diff(Snapshot{}, cur)), KindDepPR) {
		t.Fatal("expected dep-pr event")
	}
}

func TestReviewChange(t *testing.T) {
	prev := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", Review: "REVIEW_REQUIRED"})
	cur := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", Review: "APPROVED"})
	ev := Diff(prev, cur)
	if !has(kinds(ev), KindReviewChange) {
		t.Fatalf("expected review-change event, got %v", kinds(ev))
	}
	// new PR in mine role with approved state should not fire
	cur2 := addPR(Snapshot{}, "org/other", 9, "mine", PRState{Title: "new", Review: "APPROVED"})
	if has(kinds(Diff(Snapshot{}, cur2)), KindReviewChange) {
		t.Fatal("new PR should not fire review-change")
	}
}

func TestCIFailed(t *testing.T) {
	prev := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", CI: "success"})
	cur := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", CI: "failure"})
	if !has(kinds(Diff(prev, cur)), KindCIFailed) {
		t.Fatal("expected ci-failed event")
	}
	// recovery should not fire
	rev := Diff(cur, prev)
	if has(kinds(rev), KindCIFailed) {
		t.Fatal("recovery should not fire ci-failed")
	}
}

func TestCIFailedOptionalSuppressed(t *testing.T) {
	// A failure on a non-required check (MergeStateStatus UNSTABLE, i.e.
	// mergeable) doesn't block anything, so it must not notify.
	prev := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", CI: "success", MergeState: "CLEAN"})
	cur := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", CI: "failure", MergeState: "UNSTABLE"})
	if has(kinds(Diff(prev, cur)), KindCIFailed) {
		t.Fatal("non-required ci failure should not fire ci-failed")
	}
	// A blocking failure still notifies.
	prev2 := addPR(Snapshot{}, "org/repo", 2, "mine", PRState{Title: "T", CI: "success", MergeState: "CLEAN"})
	cur2 := addPR(Snapshot{}, "org/repo", 2, "mine", PRState{Title: "T", CI: "failure", MergeState: "BLOCKED"})
	if !has(kinds(Diff(prev2, cur2)), KindCIFailed) {
		t.Fatal("required ci failure should fire ci-failed")
	}
}

func TestMergeable(t *testing.T) {
	// BLOCKED -> CLEAN fires once GitHub computes the PR mergeable.
	prev := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", MergeState: "BLOCKED"})
	cur := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", MergeState: "CLEAN"})
	if !has(kinds(Diff(prev, cur)), KindMergeable) {
		t.Fatal("expected mergeable event")
	}
	// UNSTABLE (mergeable with a failing non-required check) counts too.
	prev2 := addPR(Snapshot{}, "org/repo", 2, "mine", PRState{Title: "T", MergeState: "BEHIND"})
	cur2 := addPR(Snapshot{}, "org/repo", 2, "mine", PRState{Title: "T", MergeState: "UNSTABLE"})
	if !has(kinds(Diff(prev2, cur2)), KindMergeable) {
		t.Fatal("expected mergeable event for UNSTABLE")
	}
	// UNKNOWN -> CLEAN is not a transition: UNKNOWN is already kept visible.
	prev3 := addPR(Snapshot{}, "org/repo", 3, "mine", PRState{Title: "T", MergeState: "UNKNOWN"})
	cur3 := addPR(Snapshot{}, "org/repo", 3, "mine", PRState{Title: "T", MergeState: "CLEAN"})
	if has(kinds(Diff(prev3, cur3)), KindMergeable) {
		t.Fatal("UNKNOWN -> CLEAN should not fire mergeable")
	}
}

func TestPendingTag(t *testing.T) {
	prev := addVersion(Snapshot{}, "org/repo", VersionState{PendingTags: []string{"v10"}})
	cur := addVersion(Snapshot{}, "org/repo", VersionState{PendingTags: []string{"v10", "v11"}})
	if !has(kinds(Diff(prev, cur)), KindPendingTag) {
		t.Fatal("expected pending-tag event")
	}
	cur2 := addVersion(Snapshot{}, "org/repo", VersionState{PendingTags: []string{"v10"}, Untagged: 3})
	if !has(kinds(Diff(prev, cur2)), KindPendingTag) {
		t.Fatal("expected pending-tag event for untagged")
	}
}

func TestHealth(t *testing.T) {
	prev := addVersion(Snapshot{}, "org/repo", VersionState{Problems: false})
	cur := addVersion(Snapshot{}, "org/repo", VersionState{Problems: true})
	if !has(kinds(Diff(prev, cur)), KindHealth) {
		t.Fatal("expected health event")
	}
	if has(kinds(Diff(cur, prev)), KindHealth) {
		t.Fatal("recovery should not fire health")
	}
}

func TestNewComment(t *testing.T) {
	prev := addActivity(Snapshot{}, "org/repo", 1, ActivityState{CommentAuthor: "alice", CommentAt: "2026-01-01T00:00:00Z"})
	cur := addActivity(Snapshot{}, "org/repo", 1, ActivityState{CommentAuthor: "bob", CommentAt: "2026-01-02T00:00:00Z"})
	if !has(kinds(Diff(prev, cur)), KindNewComment) {
		t.Fatal("expected new-comment event")
	}
	// unchanged activity should not fire
	if len(Diff(cur, cur)) != 0 {
		t.Fatal("unchanged activity should not fire")
	}
}

func TestIsResolved(t *testing.T) {
	cur := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", Review: "APPROVED", CI: "success", MergeState: "CLEAN"})

	if !IsResolved(Event{Kind: KindCIFailed, Repo: "org/repo", Number: 1}, cur) {
		t.Fatal("ci-failed should resolve once CI is green again")
	}
	if IsResolved(Event{Kind: KindMergeable, Repo: "org/repo", Number: 1}, cur) {
		t.Fatal("mergeable should not be resolved while still merge-ready")
	}
	if IsResolved(Event{Kind: KindReviewChange, Repo: "org/repo", Number: 1, Detail: "APPROVED"}, cur) {
		t.Fatal("review-change should not be resolved while review still APPROVED")
	}

	curFail := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", Review: "APPROVED", CI: "failure", MergeState: "BLOCKED"})
	if IsResolved(Event{Kind: KindCIFailed, Repo: "org/repo", Number: 1}, curFail) {
		t.Fatal("ci-failed should not resolve while CI is still failing")
	}
	if !IsResolved(Event{Kind: KindMergeable, Repo: "org/repo", Number: 1}, curFail) {
		t.Fatal("mergeable should resolve when no longer merge-ready")
	}

	// PR gone -> resolved
	gone := Snapshot{}
	for _, k := range []Kind{KindReviewRequested, KindDepPR, KindCIFailed, KindNewComment} {
		if !IsResolved(Event{Kind: k, Repo: "org/repo", Number: 1}, gone) {
			t.Fatalf("%s should resolve when PR is gone", k)
		}
	}

	// pending tag resolution
	curV := addVersion(Snapshot{}, "org/repo", VersionState{PendingTags: []string{"v11"}})
	if IsResolved(Event{Kind: KindPendingTag, Repo: "org/repo", Detail: "tag:v11"}, curV) {
		t.Fatal("tag still pending should not resolve")
	}
	if !IsResolved(Event{Kind: KindPendingTag, Repo: "org/repo", Detail: "tag:v10"}, curV) {
		t.Fatal("tag v10 no longer pending should resolve")
	}
	curV2 := addVersion(Snapshot{}, "org/repo", VersionState{PendingTags: []string{}, Untagged: 0})
	if !IsResolved(Event{Kind: KindPendingTag, Repo: "org/repo", Detail: "untagged"}, curV2) {
		t.Fatal("untagged cleared should resolve")
	}

	// health resolution
	if !IsResolved(Event{Kind: KindHealth, Repo: "org/repo"}, addVersion(Snapshot{}, "org/repo", VersionState{Problems: false})) {
		t.Fatal("health should resolve when recovered")
	}
}

func TestSnapshotJSON(t *testing.T) {
	s := addPR(Snapshot{}, "org/repo", 1, "mine", PRState{Title: "T", Review: "APPROVED", CI: "success", MergeState: "CLEAN"})
	s = addVersion(s, "org/svc", VersionState{Problems: true, PendingTags: []string{"v11"}, Untagged: 2})
	s = addActivity(s, "org/repo", 1, ActivityState{CommentAuthor: "alice", CommentAt: "2026-01-01T00:00:00Z"})

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Snapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	back = back.normalized()
	if back.PRs[PRKey("org/repo", 1, "mine")].Review != "APPROVED" {
		t.Fatal("PR round-trip failed")
	}
	if !back.Versions["org/svc"].Problems || back.Versions["org/svc"].Untagged != 2 {
		t.Fatal("version round-trip failed")
	}
	if back.Activity[PRKey("org/repo", 1, "mine")].CommentAuthor != "alice" {
		t.Fatal("activity round-trip failed")
	}
}
