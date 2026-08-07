// Package notify detects changes between consecutive refresh snapshots and
// turns them into user-facing notifications: new review requests, review
// decisions, CI failures, merge-readiness, pending tags, health regressions,
// dependency PRs, and new comments. Detection is a pure diff over snapshots so
// it is shared and unit-testable; the TUI persists the previous snapshot and
// the produced events in the store.
package notify

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Kind string

const (
	KindReviewRequested Kind = "new-review"
	KindReviewChange    Kind = "my-review-change"
	KindCIFailed        Kind = "ci-failed"
	KindMergeable       Kind = "mergeable"
	KindPendingTag      Kind = "pending-tag"
	KindHealth          Kind = "health"
	KindDepPR           Kind = "dep-pr"
	KindNewComment      Kind = "new-comment"
)

// Event is a single notification produced by Diff and rendered in the panel.
type Event struct {
	ID        int64
	Kind      Kind
	Repo      string
	Number    int
	Title     string
	Message   string
	Detail    string
	URL       string
	CreatedAt time.Time
	Dismissed bool
}

// PRState is the subset of a PR row the detector needs.
type PRState struct {
	Role       string
	Title      string
	Review     string
	CI         string
	MergeState string
}

// VersionState is the subset of a service version row the detector needs.
type VersionState struct {
	Problems    bool
	PendingTags []string
	Untagged    int
}

// ActivityState is the last-known comment/review activity on a PR.
type ActivityState struct {
	CommentAuthor string
	CommentAt     string
	ReviewState   string
	ReviewAt      string
}

// Snapshot is the previously-known or freshly-refreshed state of everything the
// detector cares about, keyed by identity (see prKey).
type Snapshot struct {
	PRs      map[string]PRState
	Versions map[string]VersionState
	Activity map[string]ActivityState
}

func (s Snapshot) normalized() Snapshot {
	if s.PRs == nil {
		s.PRs = map[string]PRState{}
	}
	if s.Versions == nil {
		s.Versions = map[string]VersionState{}
	}
	if s.Activity == nil {
		s.Activity = map[string]ActivityState{}
	}
	return s
}

// PRKey builds the snapshot key for a PR row: "repo#number#role".
func PRKey(repo string, number int, role string) string {
	return fmt.Sprintf("%s#%d#%s", repo, number, role)
}

func splitPRKey(key string) (string, int) {
	parts := strings.Split(key, "#")
	if len(parts) >= 2 {
		n, _ := strconv.Atoi(parts[1])
		return parts[0], n
	}
	return key, 0
}

func prURL(repo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
}

// Mergeable reports whether GitHub's authoritative MergeStateStatus means "can
// merge now". It relies on GitHub rather than deriving from CI/review, which
// varies by repo: CI or review may not be required, and up-to-date only matters
// where branch protection says so. CLEAN is fully ready, UNSTABLE is mergeable
// with a failing non-required check, HAS_HOOKS is mergeable via pre-receive
// hooks. UNKNOWN (still being computed) is kept so PRs don't flicker out of
// "mergeable" views while GitHub calculates.
func Mergeable(mergeState string) bool {
	switch mergeState {
	case "CLEAN", "UNSTABLE", "HAS_HOOKS", "UNKNOWN":
		return true
	}
	return false
}

// mergeReady reports whether GitHub considers the PR ready to merge (see
// Mergeable).
func mergeReady(p PRState) bool {
	return Mergeable(p.MergeState)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Diff returns the events that describe what changed between the previously
// known snapshot prev and the freshly refreshed snapshot cur. It only emits
// appearance/change events, never disappearance — a PR that vanished, a tag
// that shipped, or a health state that recovered never produces an event (it
// resolves existing ones instead, via IsResolved).
func Diff(prev, cur Snapshot) []Event {
	prev = prev.normalized()
	cur = cur.normalized()
	var events []Event

	for key, p := range cur.PRs {
		repo, num := splitPRKey(key)
		switch p.Role {
		case "review-direct":
			old, ok := prev.PRs[key]
			if !ok || old.Role != "review-direct" {
				events = append(events, Event{
					Kind: KindReviewRequested, Repo: repo, Number: num, Title: p.Title,
					Message: fmt.Sprintf("🔔 New review request · %s#%d %s", repo, num, p.Title),
					URL:     prURL(repo, num), CreatedAt: time.Now(),
				})
			}
		case "dep":
			old, ok := prev.PRs[key]
			if !ok || old.Role != "dep" {
				events = append(events, Event{
					Kind: KindDepPR, Repo: repo, Number: num, Title: p.Title,
					Message: fmt.Sprintf("🤖 New dependency PR · %s#%d %s", repo, num, p.Title),
					URL:     prURL(repo, num), CreatedAt: time.Now(),
				})
			}
		}
	}

	for key, p := range cur.PRs {
		if p.Role != "mine" {
			continue
		}
		old, ok := prev.PRs[key]
		if !ok || old.Role != "mine" {
			continue
		}
		repo, num := splitPRKey(key)
		if p.Review != old.Review && (p.Review == "APPROVED" || p.Review == "CHANGES_REQUESTED") {
			label := "✗ Changes requested"
			if p.Review == "APPROVED" {
				label = "✓ Approved"
			}
			events = append(events, Event{
				Kind: KindReviewChange, Repo: repo, Number: num, Title: p.Title,
				Message: fmt.Sprintf("%s · %s#%d %s", label, repo, num, p.Title),
				Detail:  p.Review,
				URL:     prURL(repo, num), CreatedAt: time.Now(),
			})
		}
		// A failure on a non-required check (MergeStateStatus UNSTABLE, i.e.
		// mergeable) doesn't block anything, so it doesn't get a notification;
		// only genuinely blocking failures do.
		if p.CI == "failure" && old.CI != "failure" && p.MergeState != "UNSTABLE" {
			events = append(events, Event{
				Kind: KindCIFailed, Repo: repo, Number: num, Title: p.Title,
				Message: fmt.Sprintf("✗ CI failing · %s#%d %s", repo, num, p.Title),
				URL:     prURL(repo, num), CreatedAt: time.Now(),
			})
		}
		if !mergeReady(old) && mergeReady(p) {
			events = append(events, Event{
				Kind: KindMergeable, Repo: repo, Number: num, Title: p.Title,
				Message: fmt.Sprintf("✅ Ready to merge · %s#%d %s", repo, num, p.Title),
				URL:     prURL(repo, num), CreatedAt: time.Now(),
			})
		}
	}

	for key, v := range cur.Versions {
		old, ok := prev.Versions[key]
		if !ok {
			continue
		}
		url := fmt.Sprintf("https://github.com/%s/releases", key)
		for _, t := range v.PendingTags {
			if !contains(old.PendingTags, t) {
				events = append(events, Event{
					Kind: KindPendingTag, Repo: key, Title: key,
					Message: fmt.Sprintf("🏷 %s ready to ship · %s", t, key),
					Detail:  "tag:" + t,
					URL:     url, CreatedAt: time.Now(),
				})
			}
		}
		if v.Untagged > 0 && old.Untagged == 0 {
			events = append(events, Event{
				Kind: KindPendingTag, Repo: key, Title: key,
				Message: fmt.Sprintf("%d untagged commits ahead of prod · %s", v.Untagged, key),
				Detail:  "untagged",
				URL:     url, CreatedAt: time.Now(),
			})
		}
		if v.Problems && !old.Problems {
			events = append(events, Event{
				Kind: KindHealth, Repo: key, Title: key,
				Message: fmt.Sprintf("⚠ %s: health regression", key),
				Detail:  "problems",
				URL:     url, CreatedAt: time.Now(),
			})
		}
	}

	for key, a := range cur.Activity {
		old := prev.Activity[key]
		repo, num := splitPRKey(key)
		p := cur.PRs[PRKey(repo, num, "mine")]
		if a.CommentAt != old.CommentAt && a.CommentAuthor != "" {
			events = append(events, Event{
				Kind: KindNewComment, Repo: repo, Number: num, Title: p.Title,
				Message: fmt.Sprintf("💬 New comment on %s#%d from @%s", repo, num, a.CommentAuthor),
				Detail:  a.CommentAuthor,
				URL:     prURL(repo, num), CreatedAt: time.Now(),
			})
		} else if a.ReviewState == "COMMENTED" && a.ReviewAt != old.ReviewAt && a.ReviewAt != "" {
			events = append(events, Event{
				Kind: KindNewComment, Repo: repo, Number: num, Title: p.Title,
				Message: fmt.Sprintf("💬 New review comment · %s#%d", repo, num),
				URL:     prURL(repo, num), CreatedAt: time.Now(),
			})
		}
	}

	return events
}

// IsResolved reports whether the condition that produced e no longer holds in
// cur, i.e. the notification can be auto-dismissed.
func IsResolved(e Event, cur Snapshot) bool {
	cur = cur.normalized()
	switch e.Kind {
	case KindReviewRequested:
		_, ok := cur.PRs[PRKey(e.Repo, e.Number, "review-direct")]
		return !ok
	case KindDepPR:
		_, ok := cur.PRs[PRKey(e.Repo, e.Number, "dep")]
		return !ok
	case KindReviewChange, KindCIFailed, KindMergeable:
		p, ok := cur.PRs[PRKey(e.Repo, e.Number, "mine")]
		if !ok {
			return true
		}
		switch e.Kind {
		case KindCIFailed:
			return p.CI != "failure"
		case KindMergeable:
			return !mergeReady(p)
		default:
			return p.Review != e.Detail
		}
	case KindPendingTag:
		v, ok := cur.Versions[e.Repo]
		if !ok {
			return true
		}
		if e.Detail == "untagged" {
			return v.Untagged == 0
		}
		return !contains(v.PendingTags, strings.TrimPrefix(e.Detail, "tag:"))
	case KindHealth:
		v, ok := cur.Versions[e.Repo]
		return !ok || !v.Problems
	case KindNewComment:
		_, ok := cur.PRs[PRKey(e.Repo, e.Number, "mine")]
		return !ok
	}
	return false
}
