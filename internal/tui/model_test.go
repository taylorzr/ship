package tui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/zach/ship/internal/k8s"
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
	wantSelected := selectedStyle.Render(margin[:3]) + margin[3:] + rest
	if selected != wantSelected {
		t.Fatalf("selected row:\n got %q\nwant %q", selected, wantSelected)
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
