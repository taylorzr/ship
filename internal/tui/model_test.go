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
	h := k8s.Health{Progressing: true, ReadyReplicas: 2, Replicas: 5}
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
