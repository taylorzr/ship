package tui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/zach/ship/internal/config"
	"github.com/zach/ship/internal/github"
	"github.com/zach/ship/internal/k8s"
	"github.com/zach/ship/internal/store"
)

func forceColor() {
	lipgloss.SetColorProfile(termenv.ANSI256)
}

func TestEventReasonAge(t *testing.T) {
	if reason, age, ok := eventReasonAge("Unhealthy"); ok || age != 0 || reason != "Unhealthy" {
		t.Fatalf("plain entry = (%q, %v, %v), want (Unhealthy, 0, false)", reason, age, ok)
	}

	ts := time.Now().Add(-15 * time.Minute).Unix()
	entry := "Unhealthy@" + strconv.FormatInt(ts, 10)
	reason, age, ok := eventReasonAge(entry)
	if !ok {
		t.Fatalf("encoded entry %q: ok = false", entry)
	}
	if reason != "Unhealthy" {
		t.Fatalf("reason = %q, want Unhealthy", reason)
	}
	if age < 14*time.Minute || age > 16*time.Minute {
		t.Fatalf("age = %v, want ~15m", age)
	}

	if _, _, ok := eventReasonAge("BackOff@notanumber"); ok {
		t.Fatal("malformed timestamp accepted")
	}
}

func TestEventReasonAgeEmitsAgedSegment(t *testing.T) {
	ts := time.Now().Add(-15 * time.Minute).Unix()
	h := k8s.Health{OldEvents: []string{"Unhealthy@" + strconv.FormatInt(ts, 10)}}
	segs := healthSegments(h, eventFilter{})
	found := false
	for _, s := range segs {
		if strings.HasPrefix(s.text, "⚠Unhealthy(-") && strings.HasSuffix(s.text, "m)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("segments = %v, want an aged ⚠Unhealthy segment", segs)
	}
}

func TestProgressingHealthShowsReplicaProgress(t *testing.T) {
	h := k8s.Health{Progressing: true, DesiredReplicas: 5, NewReadyReplicas: 2}
	segs := healthSegments(h, eventFilter{})
	if len(segs) == 0 || segs[0].text != "⟳ Progressing 2/5" {
		t.Fatalf("segments = %v, want first segment ⟳ Progressing 2/5", segs)
	}

	zero := k8s.Health{Progressing: true}
	segs = healthSegments(zero, eventFilter{})
	if len(segs) == 0 || segs[0].text != "⟳ Progressing" {
		t.Fatalf("segments = %v, want first segment ⟳ Progressing", segs)
	}

	if got := formatHealth(serializeHealth(h), eventFilter{}); !strings.Contains(got, "⟳ Progressing 2/5") {
		t.Fatalf("formatHealth round-trip = %q, want it to contain ⟳ Progressing 2/5", got)
	}
}

func TestHealthSegmentsRolloutIgnoresTransientSignals(t *testing.T) {
	h := k8s.Health{Progressing: true, DesiredReplicas: 3, NewReadyReplicas: 1, PendingPods: 2, Conditions: []string{"Unavailable"}}
	segs := healthSegments(h, eventFilter{})
	if len(segs) == 0 || segs[0].text != "⟳ Progressing 1/3" {
		t.Fatalf("segments = %v, want first segment ⟳ Progressing 1/3", segs)
	}
	for _, s := range segs {
		if strings.HasPrefix(s.text, "✖") {
			t.Fatalf("mid-rollout flagged unhealthy: %v", segs)
		}
		if strings.Contains(s.text, "Unavailable") {
			t.Fatalf("mid-rollout surfaced Unavailable condition: %v", segs)
		}
	}
	found := false
	for _, s := range segs {
		if s.text == "⌛2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("segments = %v, want pending count ⌛2 still shown", segs)
	}
}

func TestHealthSegmentsRolloutFlagsRealProblems(t *testing.T) {
	cases := []struct {
		name string
		h    k8s.Health
		want string
	}{
		{"stuck waiting", k8s.Health{Progressing: true, DesiredReplicas: 3, NewReadyReplicas: 1, Waiting: []string{"ImagePullBackOff"}}, "∞ImagePullBackOff"},
		{"hard condition", k8s.Health{Progressing: true, DesiredReplicas: 3, NewReadyReplicas: 1, Conditions: []string{"ReplicaFailure"}}, "⚠ReplicaFailure"},
		{"failed pods", k8s.Health{Progressing: true, DesiredReplicas: 3, NewReadyReplicas: 1, FailedPods: 1, FailedReasons: []string{"Evicted"}}, "💀1"},
		{"stuck pending", k8s.Health{Progressing: true, DesiredReplicas: 3, NewReadyReplicas: 0, PendingPods: 1, StuckPendingPods: 1}, "⌛1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			segs := healthSegments(tc.h, eventFilter{})
			hasX := false
			hasWant := false
			hasFraction := false
			for _, s := range segs {
				if strings.HasPrefix(s.text, "✖") {
					hasX = true
				}
				if s.text == tc.want {
					hasWant = true
				}
				if strings.HasPrefix(s.text, "⟳ Progressing") {
					hasFraction = true
				}
			}
			if !hasX {
				t.Fatalf("segments = %v, want ✖ headline", segs)
			}
			if !hasWant {
				t.Fatalf("segments = %v, want segment %q", segs, tc.want)
			}
			if !hasFraction {
				t.Fatalf("segments = %v, want the ⟳ fraction to stay visible while red", segs)
			}
		})
	}
}

func TestHealthSegmentsFailingRolloutXPrecedesFraction(t *testing.T) {
	h := k8s.Health{Progressing: true, DesiredReplicas: 3, NewReadyReplicas: 1, PendingPods: 2, StuckPendingPods: 1}
	segs := healthSegments(h, eventFilter{})
	if len(segs) < 2 || segs[0].text != "✖" || segs[1].text != "⟳ Progressing 1/3" {
		t.Fatalf("segments = %v, want ✖ then ⟳ Progressing 1/3", segs)
	}
}

func TestSerializeHealthEmptyWithNewFieldDefaults(t *testing.T) {
	if got := serializeHealth(k8s.Health{}); got != "" {
		t.Fatalf("serializeHealth(empty) = %q, want empty", got)
	}
	if got := serializeHealth(k8s.Health{NewReadyReplicas: -1}); got != "" {
		t.Fatalf("serializeHealth with unknown NewReadyReplicas = %q, want empty", got)
	}
}

func TestHealthProblemsConsistentDuringRollout(t *testing.T) {
	if healthProblems(k8s.Health{Progressing: true, ReadyReplicas: 1, Replicas: 3}) {
		t.Fatal("mid-rollout without failures should not be a problem")
	}
	if healthProblems(k8s.Health{Progressing: true, PendingPods: 2, Conditions: []string{"Unavailable"}}) {
		t.Fatal("transient pending pods / Unavailable should not be a problem during rollout")
	}
	if !healthProblems(k8s.Health{Progressing: true, Waiting: []string{"ImagePullBackOff"}}) {
		t.Fatal("stuck waiting during rollout should be a problem")
	}
	if !healthProblems(k8s.Health{Progressing: true, FailedPods: 1}) {
		t.Fatal("failed pods during rollout should be a problem")
	}
	if !healthProblems(k8s.Health{Progressing: true, Conditions: []string{"ReplicaFailure"}}) {
		t.Fatal("hard condition during rollout should be a problem")
	}
	if !healthProblems(k8s.Health{Progressing: true, StuckPendingPods: 1}) {
		t.Fatal("unschedulable pending pod during rollout should be a problem")
	}
	if healthProblems(k8s.Health{Progressing: true, PendingPods: 1}) {
		t.Fatal("benign pending pod during rollout should not be a problem")
	}
	if !healthProblems(k8s.Health{PendingPods: 1}) {
		t.Fatal("non-progressing workload with pending pods should stay a problem")
	}
}

func TestHealthSegmentsSeparatesAndDimsEvents(t *testing.T) {
	ts := time.Now().Add(-5 * time.Minute).Unix()
	h := k8s.Health{Restarts: 2, OldEvents: []string{"Unhealthy@" + strconv.FormatInt(ts, 10)}}
	segs := healthSegments(h, eventFilter{})
	stateIdx, sepIdx, eventIdx := -1, -1, -1
	for i, s := range segs {
		switch {
		case s.kind == segSep:
			sepIdx = i
		case s.text == "↻2":
			stateIdx = i
		case strings.HasPrefix(s.text, "⚠Unhealthy"):
			eventIdx = i
		}
	}
	if stateIdx < 0 || sepIdx < 0 || eventIdx < 0 {
		t.Fatalf("segments = %v, want state ↻2, separator │, event ⚠Unhealthy", segs)
	}
	if !(stateIdx < sepIdx && sepIdx < eventIdx) {
		t.Fatalf("expected order state < │ < event, got %v", segs)
	}
	if !segs[eventIdx].dim {
		t.Fatalf("event segment should be marked dim: %v", segs[eventIdx])
	}
	if segs[stateIdx].dim {
		t.Fatalf("state segment should not be dim: %v", segs[stateIdx])
	}
}

func TestHealthSegmentsNoSeparatorWithoutEvents(t *testing.T) {
	segs := healthSegments(k8s.Health{Restarts: 2}, eventFilter{})
	for _, s := range segs {
		if s.kind == segSep {
			t.Fatalf("health with no events should have no separator, got %v", segs)
		}
	}
}

func TestFormatHealthIncludesEventDivider(t *testing.T) {
	ts := time.Now().Add(-5 * time.Minute).Unix()
	health := serializeHealth(k8s.Health{Restarts: 2, OldEvents: []string{"Unhealthy@" + strconv.FormatInt(ts, 10)}})
	got := formatHealth(health, eventFilter{})
	if !strings.Contains(got, "│") {
		t.Fatalf("formatHealth = %q, want it to contain │", got)
	}
}

func TestRenderHealthColoredDimsEvents(t *testing.T) {
	forceColor()
	ts := time.Now().Add(-5 * time.Minute).Unix()
	health := serializeHealth(k8s.Health{Restarts: 2, OldEvents: []string{"Unhealthy@" + strconv.FormatInt(ts, 10)}})
	got := renderHealthColored(health, eventFilter{})
	if !strings.Contains(got, "\x1b[2;") {
		t.Fatalf("renderHealthColored = %q, want faint (dim) escape on events", got)
	}
}

func TestHealthRoundTripPreservesEventAges(t *testing.T) {
	h := k8s.Health{
		Ready:         true,
		Replicas:      2,
		ReadyReplicas: 2,
		Events:        []string{"BackOff@123456"},
		OldEvents:     []string{"Unhealthy@654321"},
	}
	s := serializeHealth(h)
	got := parseHealth(s)
	if len(got.Events) != 1 || got.Events[0] != "BackOff@123456" {
		t.Fatalf("Events after round-trip = %v", got.Events)
	}
	if len(got.OldEvents) != 1 || got.OldEvents[0] != "Unhealthy@654321" {
		t.Fatalf("OldEvents after round-trip = %v", got.OldEvents)
	}
}

func TestHealthRoundTripPreservesDesiredReplicas(t *testing.T) {
	h := k8s.Health{Ready: true, Replicas: 2, ReadyReplicas: 2, DesiredReplicas: 4}
	got := parseHealth(serializeHealth(h))
	if got.DesiredReplicas != 4 || got.Replicas != 2 {
		t.Fatalf("round-trip = %+v, want desired 4 current 2", got)
	}
}

func TestParseHealthOldFormatWithoutDesired(t *testing.T) {
	h := k8s.Health{Ready: true, Replicas: 3, ReadyReplicas: 3, Restarts: 2}
	s := serializeHealth(h)
	idx := strings.LastIndex(s, "|")
	old := s[:idx]
	got := parseHealth(old)
	if got.DesiredReplicas != 0 {
		t.Fatalf("old-format DesiredReplicas = %d, want 0", got.DesiredReplicas)
	}
	if got.Replicas != 3 || got.ReadyReplicas != 3 || got.Restarts != 2 {
		t.Fatalf("old-format parse = %+v, want current/ready 3 restarts 2", got)
	}
}

func TestFormatHealthScalingUpDown(t *testing.T) {
	got := formatHealth(serializeHealth(k8s.Health{
		Ready: true, Replicas: 2, ReadyReplicas: 2, DesiredReplicas: 4,
	}), eventFilter{})
	if got != "✔ ⇑4" {
		t.Fatalf("scaling up = %q, want %q", got, "✔ ⇑4")
	}
	got = formatHealth(serializeHealth(k8s.Health{
		Ready: true, Replicas: 3, ReadyReplicas: 3, DesiredReplicas: 1,
	}), eventFilter{})
	if got != "✔ ⇓1" {
		t.Fatalf("scaling down = %q, want %q", got, "✔ ⇓1")
	}
}

func TestHealthScalingColdStartNotEmpty(t *testing.T) {
	h := k8s.Health{Ready: false, Replicas: 0, ReadyReplicas: 0, DesiredReplicas: 2}
	if healthEmpty(h) {
		t.Fatal("cold-start scaling workload treated as empty")
	}
	got := formatHealth(serializeHealth(h), eventFilter{})
	if got != "⇑2" {
		t.Fatalf("cold-start scaling = %q, want %q", got, "⇑2")
	}
}

func TestHealthScalingMidScaleNotReady(t *testing.T) {
	h := k8s.Health{Ready: false, Replicas: 4, ReadyReplicas: 3, DesiredReplicas: 4}
	got := formatHealth(serializeHealth(h), eventFilter{})
	if got != "⇑4" {
		t.Fatalf("mid-scale not-ready = %q, want %q", got, "⇑4")
	}
}

func TestHealthScalingProblemShowsArrow(t *testing.T) {
	h := k8s.Health{Ready: false, Replicas: 4, ReadyReplicas: 3, DesiredReplicas: 4, Waiting: []string{"CrashLoopBackOff"}}
	got := formatHealth(serializeHealth(h), eventFilter{})
	if !strings.Contains(got, "✖") || !strings.Contains(got, "⇑4") {
		t.Fatalf("scaling with stuck container = %q, want both ✖ and ⇑4", got)
	}
}

func TestFormatHealthScaleHistory(t *testing.T) {
	h := k8s.Health{Ready: true, Replicas: 2, ReadyReplicas: 2, ScaleUp: 4, ScaleDown: 2}
	got := formatHealth(serializeHealth(h), eventFilter{})
	if got != "✔ │ ⇅HPA ↑4 ↓2" {
		t.Fatalf("scale history = %q, want %q", got, "✔ │ ⇅HPA ↑4 ↓2")
	}
}

func TestFormatHealthScaleHistoryUpOnly(t *testing.T) {
	h := k8s.Health{Ready: true, Replicas: 2, ReadyReplicas: 2, ScaleUp: 4}
	got := formatHealth(serializeHealth(h), eventFilter{})
	if got != "✔ │ ⇅HPA ↑4" {
		t.Fatalf("scale-up history = %q, want %q", got, "✔ │ ⇅HPA ↑4")
	}
}

func TestFormatHealthScaleHistoryDownOnly(t *testing.T) {
	h := k8s.Health{Ready: true, Replicas: 2, ReadyReplicas: 2, ScaleDown: 2}
	got := formatHealth(serializeHealth(h), eventFilter{})
	if got != "✔ │ ⇅HPA ↓2" {
		t.Fatalf("scale-down history = %q, want %q", got, "✔ │ ⇅HPA ↓2")
	}
}

func TestHealthScaleHistoryScaledToZeroNotEmpty(t *testing.T) {
	h := k8s.Health{Replicas: 0, ReadyReplicas: 0, DesiredReplicas: 0, ScaleDown: 6}
	if healthEmpty(h) {
		t.Fatal("scaled-to-zero workload with HPA history treated as empty")
	}
	got := formatHealth(serializeHealth(h), eventFilter{})
	if got != "⇅HPA ↓6" {
		t.Fatalf("scaled-to-zero history = %q, want %q", got, "⇅HPA ↓6")
	}
}

func TestHealthRoundTripPreservesScaleTotals(t *testing.T) {
	h := k8s.Health{Ready: true, Replicas: 2, ReadyReplicas: 2, DesiredReplicas: 4, ScaleUp: 7, ScaleDown: 3, NewReadyReplicas: 2, StuckPendingPods: 1}
	got := parseHealth(serializeHealth(h))
	if got.ScaleUp != 7 || got.ScaleDown != 3 {
		t.Fatalf("round-trip scale totals = %+v, want up 7 down 3", got)
	}
	if got.NewReadyReplicas != 2 || got.StuckPendingPods != 1 {
		t.Fatalf("round-trip = %+v, want NewReadyReplicas 2 StuckPendingPods 1", got)
	}
}

func TestParseHealthOldFormatScaleZero(t *testing.T) {
	h := k8s.Health{Ready: true, Replicas: 3, ReadyReplicas: 3, Restarts: 2, ScaleUp: 5, ScaleDown: 1, NewReadyReplicas: 2, StuckPendingPods: 1}
	s := serializeHealth(h)
	for range 4 {
		s = s[:strings.LastIndex(s, "|")]
	}
	got := parseHealth(s)
	if got.ScaleUp != 0 || got.ScaleDown != 0 {
		t.Fatalf("old-format scale totals = %d/%d, want 0/0", got.ScaleUp, got.ScaleDown)
	}
	if got.NewReadyReplicas != -1 || got.StuckPendingPods != 0 {
		t.Fatalf("old-format = NewReadyReplicas %d StuckPendingPods %d, want -1/0", got.NewReadyReplicas, got.StuckPendingPods)
	}
	if got.Replicas != 3 || got.ReadyReplicas != 3 || got.Restarts != 2 {
		t.Fatalf("old-format parse = %+v, want current/ready 3 restarts 2", got)
	}
}

func TestParseHealthPreviousVersionPreservesScaleButNotNewFields(t *testing.T) {
	h := k8s.Health{Ready: true, Replicas: 3, ReadyReplicas: 3, Restarts: 2, ScaleUp: 5, ScaleDown: 1, NewReadyReplicas: 2, StuckPendingPods: 1}
	s := serializeHealth(h)
	for range 2 {
		s = s[:strings.LastIndex(s, "|")]
	}
	got := parseHealth(s)
	if got.ScaleUp != 5 || got.ScaleDown != 1 {
		t.Fatalf("previous-version scale totals = %d/%d, want 5/1", got.ScaleUp, got.ScaleDown)
	}
	if got.NewReadyReplicas != -1 || got.StuckPendingPods != 0 {
		t.Fatalf("previous-version = NewReadyReplicas %d StuckPendingPods %d, want -1/0", got.NewReadyReplicas, got.StuckPendingPods)
	}
}

func TestHealthProblemsScalingNotProblem(t *testing.T) {
	h := k8s.Health{Ready: false, Replicas: 4, ReadyReplicas: 3, DesiredReplicas: 4}
	if healthProblems(h) {
		t.Fatalf("clean mid-scale %+v treated as a problem", h)
	}
}

func TestHealthProblemsScalingWithStuckContainer(t *testing.T) {
	h := k8s.Health{Ready: false, Replicas: 4, ReadyReplicas: 3, DesiredReplicas: 4, Waiting: []string{"CrashLoopBackOff"}}
	if !healthProblems(h) {
		t.Fatalf("scaling with stuck container %+v not treated as a problem", h)
	}
}

func TestRenderRowMarginOnlyHighlight(t *testing.T) {
	forceColor()
	r := row{
		title:     "fix the thing",
		repo:      "org/app",
		num:       123,
		ci:        "success",
		updatedAt: "2026-08-05T12:00:00Z",
	}
	const repoWidth = 30
	const maxWidth = 120

	ts := relativeTime(r.updatedAt)
	titleWidth := maxWidth - repoWidth - 33
	rest := fmt.Sprintf("%s%s  #%-5d  %s  %s",
		padWidth(" ", 5), padWidth("org/app", repoWidth), 123,
		padWidth(truncateWidth("fix the thing", titleWidth), titleWidth), padWidth(ts, 6))

	margin := padWidth("    ✓  ·", 10)
	unselected := renderRow(r, false, false, "⠁", repoWidth, maxWidth)
	expected := margin + rest
	if unselected != expected {
		t.Fatalf("unselected row:\n got %q\nwant %q", unselected, expected)
	}

	selected := renderRow(r, true, false, "⠁", repoWidth, maxWidth)
	wantSelected := selectedStyle.Render(margin) + rest
	if selected != wantSelected {
		t.Fatalf("selected row:\n got %q\nwant %q", selected, wantSelected)
	}
}

func TestRenderRowSelectedRefreshIconNotMangled(t *testing.T) {
	forceColor()
	s := spinner.New()
	s.Spinner = spinner.Ellipsis
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	icon := s.View()

	r := row{
		title:     "fix the thing",
		repo:      "org/app",
		num:       123,
		ci:        "success",
		updatedAt: "2026-08-05T12:00:00Z",
	}
	const repoWidth = 30
	const maxWidth = 120

	ts := relativeTime(r.updatedAt)
	titleWidth := maxWidth - repoWidth - 33
	rest := fmt.Sprintf("%s%s  #%-5d  %s  %s",
		padWidth(" ", 5), padWidth("org/app", repoWidth), 123,
		padWidth(truncateWidth("fix the thing", titleWidth), titleWidth), padWidth(ts, 6))

	margin := padWidth(padWidth(icon, 4)+"✓  ·", 10)
	want := selectedStyle.Render(margin) + rest

	got := renderRow(r, true, true, icon, repoWidth, maxWidth)
	if got != want {
		t.Fatalf("selected refreshing row:\n got %q\nwant %q", got, want)
	}
}

func TestCiIconOptional(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		optional bool
		want     string
	}{
		{"optional failure", "failure", true, "/"},
		{"blocking failure", "failure", false, "✗"},
		{"success", "success", true, "✓"},
		{"pending", "pending", true, "…"},
		{"none", "none", true, "·"},
		{"empty", "", true, "·"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ciIcon(tt.state, tt.optional); got != tt.want {
				t.Fatalf("ciIcon(%q, %v) = %q, want %q", tt.state, tt.optional, got, tt.want)
			}
		})
	}
}

func TestRenderRowCiIconFailure(t *testing.T) {
	forceColor()
	const repoWidth = 30
	const maxWidth = 120
	for _, tt := range []struct {
		name       string
		mergeState string
		wantIcon   string
		wantSync   string
	}{
		{"optional non-required", "UNSTABLE", "/", "↑"},
		{"required blocking", "BLOCKED", "✗", " "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := row{
				title:      "fix the thing",
				repo:       "org/app",
				num:        123,
				ci:         "failure",
				mergeState: tt.mergeState,
				updatedAt:  "2026-08-05T12:00:00Z",
			}
			ts := relativeTime(r.updatedAt)
			titleWidth := maxWidth - repoWidth - 33
			rest := fmt.Sprintf("%s%s  #%-5d  %s  %s",
				padWidth(tt.wantSync, 5), padWidth("org/app", repoWidth), 123,
				padWidth(truncateWidth("fix the thing", titleWidth), titleWidth), padWidth(ts, 6))
			margin := padWidth("    "+tt.wantIcon+"  ·", 10)
			got := renderRow(r, false, false, "⠁", repoWidth, maxWidth)
			if want := margin + rest; got != want {
				t.Fatalf("%s row:\n got %q\nwant %q", tt.name, got, want)
			}
		})
	}
}

func TestRangeDur(t *testing.T) {
	tests := []struct {
		name string
		a, b time.Duration
		want string
	}{
		{"shared minute unit", time.Minute, 10 * time.Minute, "1–10m"},
		{"shared hour unit", time.Hour, 24 * time.Hour, "1–24h"},
		{"mixed minute and hour", 10 * time.Minute, time.Hour, "10m–1h"},
		{"mixed seconds and minutes", 90 * time.Second, 10 * time.Minute, "90s–10m"},
		{"shared second unit", 30 * time.Second, 90 * time.Second, "30–90s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rangeDur(tt.a, tt.b); got != tt.want {
				t.Errorf("rangeDur(%v, %v) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestRenderRowTitleOnlyStillHighlights(t *testing.T) {
	forceColor()
	r := row{title: "some plain section row"}
	got := renderRow(r, true, false, "⠁", 30, 120)
	if got != selectedStyle.Render("some plain section row") {
		t.Fatalf("title-only selected row:\n got %q", got)
	}
}

func testModel(width, height int) Model {
	s := spinner.New()
	s.Spinner = spinner.Ellipsis
	return Model{
		cfg:          &config.Config{},
		spin:         s,
		width:        width,
		height:       height,
		loading:      map[string]bool{},
		sectionErrs:  map[string]string{},
		mockK8sSpecs: map[string]k8s.MockSpec{},
	}
}

func TestViewRendersViewportPanes(t *testing.T) {
	forceColor()
	m := testModel(100, 30)
	m.sections = []section{
		{
			name:    "My PRs",
			rows:    []row{{repo: "org/app", num: 1, title: "fix the thing"}},
			allRows: []row{{repo: "org/app", num: 1, title: "fix the thing"}},
		},
		{
			name:    "Services",
			rows:    []row{{name: "api", health: "ready"}},
			allRows: []row{{name: "api", health: "ready"}},
		},
	}
	out := m.View()
	for _, want := range []string{"My PRs", "Services", "fix the thing", "api"} {
		if !strings.Contains(out, want) {
			t.Fatalf("View() missing %q:\n%s", want, out)
		}
	}
	if m.sections[0].view == nil {
		t.Fatal("viewport not created for PR section")
	}
}

func TestViewportPageDownScrollsActiveSection(t *testing.T) {
	forceColor()
	m := testModel(100, 30)
	rows := make([]row, 40)
	for i := range rows {
		rows[i] = row{repo: "org/app", num: i + 1, title: fmt.Sprintf("pr %d", i+1)}
	}
	m.sections = []section{{name: "My PRs", rows: rows, allRows: rows}}
	_ = m.View() // seed viewport content

	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	got := nm.(Model)
	if got.sections[0].scrollOffset == 0 {
		t.Fatal("PageDown did not advance scrollOffset")
	}
	out := got.View()
	if !strings.Contains(out, "more above") || !strings.Contains(out, "more below") {
		t.Fatalf("scrolled pane missing overflow indicators:\n%s", out)
	}
	if got.sections[0].view.YOffset != got.sections[0].scrollOffset {
		t.Fatalf("view YOffset %d != scrollOffset %d", got.sections[0].view.YOffset, got.sections[0].scrollOffset)
	}
}

func TestViewportWheelScrollsActiveSection(t *testing.T) {
	forceColor()
	m := testModel(100, 30)
	rows := make([]row, 40)
	for i := range rows {
		rows[i] = row{repo: "org/app", num: i + 1, title: fmt.Sprintf("pr %d", i+1)}
	}
	m.sections = []section{{name: "My PRs", rows: rows, allRows: rows}}
	_ = m.View() // seed viewport content

	nm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown,
		X:      50, Y: 10,
	})
	got := nm.(Model)
	if got.sections[0].scrollOffset == 0 {
		t.Fatal("mouse wheel down did not advance scrollOffset")
	}
}

func TestViewportScrollOffsetClampedWhenContentShrinks(t *testing.T) {
	forceColor()
	m := testModel(100, 30)
	rows := make([]row, 40)
	for i := range rows {
		rows[i] = row{repo: "org/app", num: i + 1, title: fmt.Sprintf("pr %d", i+1)}
	}
	m.sections = []section{{name: "My PRs", rows: rows, allRows: rows}}

	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	got := nm.(Model)
	// shrink the section to a handful of rows, then re-render
	got.sections[0].rows = got.sections[0].rows[:3]
	got.sections[0].allRows = got.sections[0].rows
	_ = got.View()
	if got.sections[0].scrollOffset != 0 {
		t.Fatalf("scrollOffset = %d, want 0 after content shrank", got.sections[0].scrollOffset)
	}
}

func TestServicesForRepo(t *testing.T) {
	m := testModel(100, 30)
	m.cfg.Services = []config.ServiceConfig{
		{Name: "api", Repo: "org/app", Context: "ctx-a"},
		{Name: "worker", Repo: "org/app", Context: "ctx-b"},
		{Name: "web", Repo: "org/web"},
	}
	if got := m.servicesForRepo("org/app"); len(got) != 2 {
		t.Fatalf("servicesForRepo(org/app) = %d entries, want 2", len(got))
	}
	if got := m.servicesForRepo("org/missing"); len(got) != 0 {
		t.Fatalf("servicesForRepo(org/missing) = %d entries, want 0", len(got))
	}
}

func TestActionDoneRefreshesMatchingServices(t *testing.T) {
	forceColor()
	st, err := store.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	m := testModel(100, 30)
	m.store = st
	m.cfg.Services = []config.ServiceConfig{
		{Name: "api", Repo: "org/app", Context: "ctx-a"},
		{Name: "web", Repo: "org/web"},
	}

	nm, cmd := m.Update(actionDoneMsg{action: "merge", repo: "org/app", num: 5})
	got := nm.(Model)
	if cmd == nil {
		t.Fatal("merge with matching service returned nil cmd, want refresh cmd")
	}
	if !got.loading["Services"] {
		t.Fatal("loading[Services] not set during merge refresh")
	}

	if _, cmd := got.Update(actionDoneMsg{action: "close", repo: "org/app", num: 6}); cmd != nil {
		t.Fatal("close returned a cmd, want nil")
	}

	if _, cmd := got.Update(actionDoneMsg{action: "merge", repo: "org/other", num: 7}); cmd != nil {
		t.Fatal("merge with no matching service returned a cmd, want nil")
	}
}

func TestRateLimitText(t *testing.T) {
	got := rateLimitText(github.RateLimit{Resource: "graphql", Limit: 5000, Remaining: 4800})
	if got != "gql 4.8/5k" {
		t.Fatalf("rateLimitText(graphql) = %q, want %q", got, "gql 4.8/5k")
	}
	got = rateLimitText(github.RateLimit{Resource: "search", Limit: 30, Remaining: 27})
	if got != "search 27/30" {
		t.Fatalf("rateLimitText(search) = %q, want %q", got, "search 27/30")
	}
	got = rateLimitText(github.RateLimit{Resource: "core", Limit: 5000, Remaining: 3663})
	if got != "rest 3.7/5k" {
		t.Fatalf("rateLimitText(core) = %q, want %q", got, "rest 3.7/5k")
	}
}

func TestCompactNumbers(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"limit 5000", compactLimit(5000), "5k"},
		{"limit 4800", compactLimit(4800), "4.8k"},
		{"limit 995", compactLimit(995), "995"},
		{"limit 1000", compactLimit(1000), "1k"},
		{"remaining 5000", compactRemaining(5000), "5"},
		{"remaining 4800", compactRemaining(4800), "4.8"},
		{"remaining 995", compactRemaining(995), "995"},
		{"remaining 4960", compactRemaining(4960), "5"},
		{"limit 4960", compactLimit(4960), "5k"},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestRateLimitTickNilWithoutGH(t *testing.T) {
	m := testModel(100, 30)
	if cmd := m.rateLimitTick(); cmd != nil {
		t.Fatal("rateLimitTick with no gh client returned a cmd, want nil")
	}
}

func TestRateLimitTickRefreshesAndRearms(t *testing.T) {
	m := testModel(100, 30)
	m.gh = github.NewClient("")
	nm, cmd := m.Update(rateLimitTickMsg{})
	got := nm.(Model)
	if cmd == nil {
		t.Fatal("rateLimitTickMsg returned nil cmd, want batch(refresh, re-arm)")
	}
	if got.gh == nil {
		t.Fatal("gh client lost during rate-limit tick")
	}
}

func TestRateLimitStyle(t *testing.T) {
	forceColor()
	frac := func(remaining, limit int) int {
		s := rateLimitStyle(github.RateLimit{Limit: limit, Remaining: remaining})
		out := s.Render("x")
		if !strings.Contains(out, "\x1b[") {
			t.Fatalf("expected styled output, got plain %q", out)
		}
		if strings.Contains(out, "91;") || strings.Contains(out, "38;5;1") {
			return 2 // red
		}
		if strings.Contains(out, "38;5;220") {
			return 1 // yellow
		}
		return 0
	}
	if got := frac(50, 100); got != 0 {
		t.Fatalf("50%% remaining styled red/yellow (got %d), want gray", got)
	}
	if got := frac(20, 100); got != 1 {
		t.Fatalf("20%% remaining styled %d, want yellow", got)
	}
	if got := frac(5, 100); got != 2 {
		t.Fatalf("5%% remaining styled %d, want red", got)
	}
}
