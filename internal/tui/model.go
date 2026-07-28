package tui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/zach/ship/internal/config"
	gh "github.com/zach/ship/internal/github"
	"github.com/zach/ship/internal/k8s"
	"github.com/zach/ship/internal/store"
	"github.com/zach/ship/internal/version"
	"time"
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212"))

	sectionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("86")).
			MarginTop(1)

	rowStyle = lipgloss.NewStyle().
			PaddingLeft(2)

	selectedStyle = lipgloss.NewStyle().
			PaddingLeft(2).
			Reverse(true).
			Bold(true)

	helpKey = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	headerStyle = lipgloss.NewStyle().PaddingLeft(2).Foreground(lipgloss.Color("244"))
)

type row struct {
	title  string
	repo   string
	num    int
	url    string
	ci     string
	review string
}

type section struct {
	name    string
	rows    []row
}

type keyMap struct {
	Up      key.Binding
	Down    key.Binding
	Enter   key.Binding
	Refresh key.Binding
	Quit    key.Binding
}

var keys = keyMap{
	Up:      key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "move up")),
	Down:    key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "move down")),
	Enter:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open in browser")),
	Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
}

type refreshDoneMsg struct{ source string }

type Model struct {
	cfg           *config.Config
	store         *store.Store
	gh            *gh.Client
	width         int
	height        int
	sections      []section
	cursor        int
	total         int
	spin          spinner.Model
	loading       map[string]bool
	lastRefreshed time.Time
	err           string
}

func New(cfg *config.Config, st *store.Store, ghClient *gh.Client) Model {
	s := spinner.New()
	s.Spinner = spinner.Ellipsis
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	m := Model{
		cfg:   cfg,
		store: st,
		gh:    ghClient,
		spin:  s,
		loading: map[string]bool{
			"My PRs":        false,
			"To Review":     false,
			"Releases":      false,
			"Dependencies":  false,
		},
	}
	m.loadFromCache()
	return m
}

func (m *Model) loadFromCache() {
	var sections []section

	prs, _ := m.store.CachedPRs("mine")
	s := section{name: "My PRs"}
	for _, p := range prs {
		s.rows = append(s.rows, row{
			title:  p.Title,
			repo:   p.Repo,
			num:    p.Number,
			url:    p.URL,
			ci:     p.CIState,
			review: p.ReviewDecision,
		})
	}
	sections = append(sections, s)

	revs, _ := m.store.CachedPRs("review-direct")
	s2 := section{name: "To Review"}
	for _, p := range revs {
		s2.rows = append(s2.rows, row{
			title:  p.Title,
			repo:   p.Repo,
			num:    p.Number,
			url:    p.URL,
			ci:     p.CIState,
			review: p.ReviewDecision,
		})
	}
	sections = append(sections, s2)

	versions, _ := m.store.CachedVersions()
	s3 := section{name: "Releases"}
	for _, v := range versions {
		title := ""
		if v.Error != "" {
			title = fmt.Sprintf("✗ %s — %s", v.Repo, v.Error)
		} else {
			title = fmt.Sprintf("prod %s", v.ProdRef)
			if v.AheadBy > 0 {
				title += fmt.Sprintf(" · +%d", v.AheadBy)
			}
			if v.PendingTags != "" {
				title += fmt.Sprintf(" · pending %s", v.PendingTags)
			}
		}
		s3.rows = append(s3.rows, row{title: title})
	}
	sections = append(sections, s3)

	deps, _ := m.store.CachedPRs("dep")
	s4 := section{name: "Dependencies"}
	for _, p := range deps {
		s4.rows = append(s4.rows, row{
			title:  p.Title,
			repo:   p.Repo,
			num:    p.Number,
			url:    p.URL,
			ci:     p.CIState,
			review: p.ReviewDecision,
		})
	}
	sections = append(sections, s4)

	m.sections = sections
	m.recalcTotal()
	m.markRefreshed()
}

func (m *Model) markRefreshed() {
	m.lastRefreshed = time.Now()
}

func (m *Model) recalcTotal() {
	m.total = 0
	for _, s := range m.sections {
		m.total += len(s.rows)
	}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick, m.autoRefreshTick()}
	for k := range m.loading {
		m.loading[k] = true
	}
	cmds = append(cmds, m.sectionCommands(context.Background())...)
	return tea.Batch(cmds...)
}

func (m Model) sectionCommands(ctx context.Context) []tea.Cmd {
	return []tea.Cmd{
		m.refreshMyPRs(ctx),
		m.refreshReview(ctx),
		m.refreshDeps(ctx),
		m.refreshReleases(ctx),
	}
}

func (m Model) refreshMyPRs(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		prs, err := m.gh.MyPRs(ctx, m.cfg.GitHub.Orgs)
		if err == nil {
			cache := make([]store.CachedPR, len(prs))
			for i, p := range prs {
				cache[i] = toCached(p, "mine")
			}
			m.store.SavePRs(cache, "mine")
		}
		return refreshDoneMsg{source: "My PRs"}
	}
}

func (m Model) refreshReview(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		revs, err := m.gh.ReviewRequested(ctx)
		if err == nil {
			cache := make([]store.CachedPR, len(revs))
			for i, p := range revs {
				cache[i] = toCached(p, "review-direct")
			}
			m.store.SavePRs(cache, "review-direct")
		}
		return refreshDoneMsg{source: "To Review"}
	}
}

func (m Model) refreshDeps(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		starred := m.cfg.StarredRepos()
		deps, err := m.gh.DepPRs(ctx, starred)
		if err == nil {
			cache := make([]store.CachedPR, len(deps))
			for i, p := range deps {
				cache[i] = toCached(p, "dep")
			}
			m.store.SavePRs(cache, "dep")
		}
		return refreshDoneMsg{source: "Dependencies"}
	}
}

func (m Model) refreshReleases(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		for _, svc := range m.cfg.Services {
			rc, err := k8s.NewRealClient("", svc.Context)
			if err != nil {
				m.store.SaveVersion(store.CachedVersion{Repo: svc.Repo, Error: fmt.Sprintf("k8s: %v", err)})
				continue
			}
			r := version.Resolve(ctx, rc, m.gh, svc)
			pending := ""
			for i, t := range r.PendingTags {
				if i > 0 {
					pending += ", "
				}
				pending += t.Name
			}
			m.store.SaveVersion(store.CachedVersion{
				Repo:        svc.Repo,
				ProdRef:     r.ProdRef,
				ProdSHA:     r.ProdSHA,
				AheadBy:     r.AheadBy,
				PendingTags: pending,
				Error:       r.Error,
			})
		}
		return refreshDoneMsg{source: "Releases"}
	}
}

func (m Model) autoRefreshTick() tea.Cmd {
	return tea.Tick(60*time.Second, func(t time.Time) tea.Msg {
		return autoRefreshMsg(t)
	})
}

type autoRefreshMsg time.Time

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, keys.Down):
			if m.cursor < m.total-1 {
				m.cursor++
			}
		case key.Matches(msg, keys.Up):
			if m.cursor > 0 {
				m.cursor--
			}
		case key.Matches(msg, keys.Enter):
			r := m.currentRow()
			if r != nil && r.url != "" {
				openBrowser(r.url)
			}
		case key.Matches(msg, keys.Refresh):
			for k := range m.loading {
				m.loading[k] = true
			}
			return m, m.fullRefreshCmd()
		}

	case refreshDoneMsg:
		m.loading[msg.source] = false
		m.loadFromCache()
		if m.allDone() {
			m.markRefreshed()
		}

	case autoRefreshMsg:
		if m.allDone() {
			for k := range m.loading {
				m.loading[k] = true
			}
			return m, m.fullRefreshCmd()
		}
		return m, m.autoRefreshTick()
	}

	return m, nil
}

func (m *Model) allDone() bool {
	for _, v := range m.loading {
		if v {
			return false
		}
	}
	return true
}

func (m *Model) currentRow() *row {
	if m.total == 0 {
		return nil
	}
	remaining := m.cursor
	for _, s := range m.sections {
		if remaining < len(s.rows) {
			return &s.rows[remaining]
		}
		remaining -= len(s.rows)
	}
	return nil
}

func (m Model) View() string {
	var globalIdx int
	var b strings.Builder

	b.WriteString(titleStyle.Render("ship"))
	b.WriteString("\n")

	for _, s := range m.sections {
		b.WriteString(sectionStyle.Render(s.name))
		if m.loading[s.name] {
			b.WriteString("  ")
			b.WriteString(m.spin.View())
		}
		b.WriteString("\n")

		// column header for PR sections
		if len(s.rows) > 0 && s.rows[0].num > 0 {
			b.WriteString(headerStyle.Render("CI Rev  Repo                              #       Title"))
			b.WriteString("\n")
		}

		for _, r := range s.rows {
			line := renderRow(r, globalIdx == m.cursor)
			b.WriteString(line)
			b.WriteString("\n")
			globalIdx++
		}
	}

	b.WriteString("\n")
	b.WriteString(m.viewHelp())

	return b.String()
}

func renderRow(r row, selected bool) string {
	icon := ""
	if r.ci != "" {
		icon = ciIcon(r.ci)
	}

	rev := ""
	if r.review != "" {
		rev = reviewIcon(r.review)
	} else if r.num > 0 {
		rev = "·"
	}

	var line string
	if r.num > 0 {
		line = fmt.Sprintf("%s %s  %s  #%-5d  %s",
			icon, rev, padRight(r.repo, 30), r.num, r.title)
	} else {
		line = r.title
	}

	if selected {
		return selectedStyle.Render(line)
	}
	return rowStyle.Render(line)
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func (m Model) viewHelp() string {
	sep := helpKey.Render(" · ")
	line := helpKey.Render("j/k") + sep + helpKey.Render("move") +
		sep + helpKey.Render("enter") + sep + helpKey.Render("open") +
		sep + helpKey.Render("r") + sep + helpKey.Render("refresh")
	if !m.lastRefreshed.IsZero() {
		line += helpKey.Render(" (auto in " + refreshCountdown(m.lastRefreshed) + ")")
	}
	line += sep + helpKey.Render("q") + sep + helpKey.Render("quit")
	return line
}

func (m Model) fullRefreshCmd() tea.Cmd {
	cmds := []tea.Cmd{m.autoRefreshTick()}
	cmds = append(cmds, m.sectionCommands(context.Background())...)
	return tea.Batch(cmds...)
}

func ciIcon(state string) string {
	switch state {
	case "success":
		return "✓"
	case "failure":
		return "✗"
	case "pending":
		return "…"
	default:
		return "·"
	}
}

func reviewIcon(dec string) string {
	switch dec {
	case "APPROVED":
		return "👍"
	case "CHANGES_REQUESTED":
		return "👎"
	case "REVIEW_REQUIRED":
		return "⏳"
	default:
		return " "
	}
}

func toCached(p gh.PR, role string) store.CachedPR {
	return store.CachedPR{
		Number:         p.Number,
		Repo:           p.Repo,
		Title:          p.Title,
		Author:         p.Author,
		Role:           role,
		URL:            p.URL,
		ReviewDecision: p.ReviewDecision,
		CIState:        p.CIState,
		Mergeable:      p.Mergeable,
		UpdatedAt:      p.UpdatedAt,
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	}
	if cmd != nil {
		cmd.Start()
	}
}

func refreshCountdown(t time.Time) string {
	remaining := 60 - int(time.Since(t).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("%ds", remaining)
}
