package tui

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/zach/ship/internal/config"
	gh "github.com/zach/ship/internal/github"
	"github.com/zach/ship/internal/k8s"
	"github.com/zach/ship/internal/store"
	"github.com/zach/ship/internal/version"
)

var shipLog *log.Logger

func init() {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "ship")
	os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "ship.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		shipLog = log.New(f, "", log.LstdFlags)
	}
}

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
	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).PaddingLeft(2)
	overflowStyle = lipgloss.NewStyle().PaddingLeft(2).Foreground(lipgloss.Color("241"))
	dialogStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("205")).
			Padding(1, 2).
			Width(50)
	dialogTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	dialogHelp  = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	maxSectionRows = 15
)

type row struct {
	title  string
	repo   string
	num    int
	url    string
	ci     string
	review string
	draft  bool
	name   string // display name (releases section)
	pending string // pending versions (releases section)
}

type section struct {
	name        string
	rows        []row
	allRows     []row
	scrollOffset int
	hideDrafts  bool
}

type keyMap struct {
	Up          key.Binding
	Down        key.Binding
	SectionNext key.Binding
	SectionPrev key.Binding
	Enter       key.Binding
	Merge       key.Binding
	Close       key.Binding
	Refresh     key.Binding
	Quit        key.Binding
	FilterDraft key.Binding
	Search        key.Binding
}

var keys = keyMap{
	Up:          key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "move up")),
	Down:        key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "move down")),
	SectionNext: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next section")),
	SectionPrev: key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("⇧+tab", "prev section")),
	Enter:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	Merge:       key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "merge")),
	Close:       key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "close")),
	Refresh:     key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	FilterDraft: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "toggle drafts")),
	Search:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
}

type refreshDoneMsg struct {
	source string
	err    error
}

type confirmAction struct {
	action string // "merge" or "close"
	repo   string
	num    int
	title  string
}

type actionDoneMsg struct {
	err error
}

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
	sectionErrs   map[string]string
	confirm       *confirmAction
	lastRefreshed time.Time
	sectionIdx    int
	searching     bool
	searchQuery   string
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
		sectionErrs: map[string]string{},
	}
	m.loadFromCache()
	return m
}

func (m *Model) sectionProp(name string) (offset int, hideDrafts bool) {
	for _, s := range m.sections {
		if s.name == name {
			return s.scrollOffset, s.hideDrafts
		}
	}
	return 0, false
}

func (m *Model) applyFilters() {
	q := strings.ToLower(m.searchQuery)
	for i := range m.sections {
		sectionQ := q
		// search only applies to the active section
		if i != m.sectionIdx {
			sectionQ = ""
		}
		if m.sections[i].hideDrafts || sectionQ != "" {
			var filtered []row
			for _, r := range m.sections[i].allRows {
				if m.sections[i].hideDrafts && r.draft {
					continue
				}
				if sectionQ != "" && !strings.Contains(strings.ToLower(r.title), sectionQ) &&
					!strings.Contains(strings.ToLower(r.repo), sectionQ) &&
					!strings.Contains(strings.ToLower(r.name), sectionQ) &&
					!strings.Contains(strings.ToLower(r.pending), sectionQ) {
					continue
				}
				filtered = append(filtered, r)
			}
			m.sections[i].rows = filtered
		} else {
			m.sections[i].rows = m.sections[i].allRows
		}
		if m.sections[i].scrollOffset >= len(m.sections[i].rows) {
			m.sections[i].scrollOffset = 0
		}
	}
	m.recalcTotal()
	start := m.sectionOffset(m.sectionIdx)
	end := m.sectionEnd(m.sectionIdx)
	if m.cursor < start || m.cursor >= end {
		m.cursor = start
	}
}

func (m *Model) loadFromCache() {
	var sections []section

	prs, _ := m.store.CachedPRs("mine")
	so, hd := m.sectionProp("My PRs")
	s := section{name: "My PRs", scrollOffset: so, hideDrafts: hd}
	for _, p := range prs {
		r := row{
			title:  p.Title,
			repo:   p.Repo,
			num:    p.Number,
			url:    p.URL,
			ci:     p.CIState,
			review: p.ReviewDecision,
			draft:  p.IsDraft,
		}
		s.allRows = append(s.allRows, r)
		s.rows = append(s.rows, r)
	}
	sections = append(sections, s)

	revs, _ := m.store.CachedPRs("review-direct")
	so, hd = m.sectionProp("To Review")
	s2 := section{name: "To Review", scrollOffset: so, hideDrafts: hd}
	for _, p := range revs {
		r := row{
			title:  p.Title,
			repo:   p.Repo,
			num:    p.Number,
			url:    p.URL,
			ci:     p.CIState,
			review: p.ReviewDecision,
			draft:  p.IsDraft,
		}
		s2.allRows = append(s2.allRows, r)
		s2.rows = append(s2.rows, r)
	}
	sections = append(sections, s2)

	versions, _ := m.store.CachedVersions()
	svcNames := make(map[string]string, len(m.cfg.Services))
	for _, svc := range m.cfg.Services {
		svcNames[svc.Repo] = svc.Name
	}
	s3 := section{name: "Releases"}
	for _, v := range versions {
		title := v.ProdRef
		if title == "" {
			title = "-"
		} else if v.AheadBy > 0 {
			title = fmt.Sprintf("+%d %s", v.AheadBy, title)
		}
		name := v.Repo
		if n, ok := svcNames[v.Repo]; ok && n != "" {
			name = n
		}
		pending := v.PendingTags
		if pending == "" {
			pending = "✓"
		}
		r := row{
			name:    name,
			title:   title,
			pending: pending,
		}
		if v.Error != "" {
			r.title = "✗ " + v.Error
			r.pending = ""
		}
		s3.rows = append(s3.rows, r)
	}
	sections = append(sections, s3)

	deps, _ := m.store.CachedPRs("dep")
	so, hd = m.sectionProp("Dependencies")
	s4 := section{name: "Dependencies", scrollOffset: so, hideDrafts: hd}
	for _, p := range deps {
		r := row{
			title:  p.Title,
			repo:   p.Repo,
			num:    p.Number,
			url:    p.URL,
			ci:     p.CIState,
			review: p.ReviewDecision,
			draft:  p.IsDraft,
		}
		s4.allRows = append(s4.allRows, r)
		s4.rows = append(s4.rows, r)
	}
	sections = append(sections, s4)

	m.sections = sections
	m.applyFilters()
	m.markRefreshed()
}

func (m *Model) markRefreshed() {
	m.lastRefreshed = time.Now()
}

func (m *Model) recalcTotal() {
	m.total = 0
	for _, s := range m.sections {
		m.total += visibleRows(s)
	}
	if m.cursor >= m.total && m.total > 0 {
		m.cursor = m.total - 1
	}
}

func (m *Model) sectionOffset(idx int) int {
	off := 0
	for i := 0; i < idx && i < len(m.sections); i++ {
		off += visibleRows(m.sections[i])
	}
	return off
}

func (m *Model) sectionEnd(idx int) int {
	return m.sectionOffset(idx) + visibleRows(m.sections[idx])
}

func (m *Model) nextRow() int {
	s := &m.sections[m.sectionIdx]
	start := m.sectionOffset(m.sectionIdx)
	end := start + len(s.rows)
	if m.cursor >= end-1 {
		return m.cursor
	}
	newCursor := m.cursor + 1
	// advance scroll offset if cursor leaves the visible window
	visEnd := start + s.scrollOffset + maxSectionRows
	if newCursor >= visEnd {
		s.scrollOffset++
	}
	return newCursor
}

func (m *Model) prevRow() int {
	s := &m.sections[m.sectionIdx]
	start := m.sectionOffset(m.sectionIdx)
	if m.cursor <= start {
		return m.cursor
	}
	newCursor := m.cursor - 1
	// retreat scroll offset if cursor leaves the visible window
	if newCursor < start+s.scrollOffset {
		s.scrollOffset--
	}
	return newCursor
}

func visibleRows(s section) int {
	return len(s.rows)
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick, m.autoRefreshTick()}
	m.sectionErrs = map[string]string{}
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
		return refreshDoneMsg{source: "My PRs", err: err}
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
		return refreshDoneMsg{source: "To Review", err: err}
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
		return refreshDoneMsg{source: "Dependencies", err: err}
	}
}

func (m Model) refreshReleases(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		type svcResult struct {
			Repo        string
			ProdRef     string
			ProdSHA     string
			AheadBy     int
			PendingTags string
			Error       string
		}

		var wg sync.WaitGroup
		results := make(chan svcResult, len(m.cfg.Services))

		for _, svc := range m.cfg.Services {
			wg.Add(1)
			go func(svc config.ServiceConfig) {
				defer wg.Done()
				svcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()

				rc, err := k8s.NewRealClient(svcCtx, "", svc.Context)
				if err != nil {
					results <- svcResult{Repo: svc.Repo, Error: fmt.Sprintf("k8s: %v", err)}
					return
				}
				r := version.Resolve(svcCtx, rc, m.gh, svc)
				pending := ""
				for i, t := range r.PendingTags {
					if i > 0 {
						pending += ", "
					}
					pending += t.Name
				}
				results <- svcResult{
					Repo:        svc.Repo,
					ProdRef:     r.ProdRef,
					ProdSHA:     r.ProdSHA,
					AheadBy:     r.AheadBy,
					PendingTags: pending,
					Error:       r.Error,
				}
			}(svc)
		}

		wg.Wait()
		close(results)

		var lastErr error
		for res := range results {
			m.store.SaveVersion(store.CachedVersion{
				Repo:        res.Repo,
				ProdRef:     res.ProdRef,
				ProdSHA:     res.ProdSHA,
				AheadBy:     res.AheadBy,
				PendingTags: res.PendingTags,
				Error:       res.Error,
			})
			if res.Error != "" {
				lastErr = fmt.Errorf("%s: %s", res.Repo, res.Error)
			}
		}
		return refreshDoneMsg{source: "Releases", err: lastErr}
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
		if m.confirm != nil {
			switch {
			case key.Matches(msg, keys.Quit):
				return m, tea.Quit
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
				m.confirm = nil
			case key.Matches(msg, keys.Enter):
				action := m.confirm.action
				repo := m.confirm.repo
				num := m.confirm.num
				m.confirm = nil
				return m, m.execAction(action, repo, num)
			}
			return m, nil
		}
		if m.searching {
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
				m.searching = false
				m.searchQuery = ""
				m.applyFilters()
			case key.Matches(msg, keys.Enter):
				m.searching = false
				m.applyFilters()
			case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
				if len(m.searchQuery) > 0 {
					m.searchQuery = m.searchQuery[:len(m.searchQuery)-1]
					m.applyFilters()
				}
			default:
				r := []rune(msg.String())
				if len(r) == 1 && r[0] >= 32 && r[0] <= 126 {
					m.searchQuery += string(r[0])
					m.applyFilters()
				}
			}
			return m, nil
		}
		switch {
		case key.Matches(msg, keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, keys.Down):
			m.cursor = m.nextRow()
		case key.Matches(msg, keys.Up):
			m.cursor = m.prevRow()
		case key.Matches(msg, keys.SectionNext):
			m.sectionIdx = (m.sectionIdx + 1) % len(m.sections)
			m.cursor = m.sectionOffset(m.sectionIdx) + m.sections[m.sectionIdx].scrollOffset
		case key.Matches(msg, keys.SectionPrev):
			m.sectionIdx--
			if m.sectionIdx < 0 {
				m.sectionIdx = len(m.sections) - 1
			}
			m.cursor = m.sectionOffset(m.sectionIdx) + m.sections[m.sectionIdx].scrollOffset
		case key.Matches(msg, keys.Enter):
			r := m.currentRow()
			if r != nil && r.url != "" {
				openBrowser(r.url)
			}
		case key.Matches(msg, keys.Merge):
			r := m.currentRow()
			if r != nil && r.num > 0 {
				m.confirm = &confirmAction{action: "merge", repo: r.repo, num: r.num, title: r.title}
			}
		case key.Matches(msg, keys.Close):
			r := m.currentRow()
			if r != nil && r.num > 0 {
				m.confirm = &confirmAction{action: "close", repo: r.repo, num: r.num, title: r.title}
			}
		case key.Matches(msg, keys.Refresh):
			m.sectionErrs = map[string]string{}
			for k := range m.loading {
				m.loading[k] = true
			}
			return m, m.fullRefreshCmd()
		case key.Matches(msg, keys.FilterDraft):
			m.sections[m.sectionIdx].hideDrafts = !m.sections[m.sectionIdx].hideDrafts
			m.applyFilters()
			return m, nil
		case key.Matches(msg, keys.Search):
			m.searching = true
			m.searchQuery = ""
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			if m.searchQuery != "" {
				m.searchQuery = ""
				m.applyFilters()
			}
		}

	case refreshDoneMsg:
		m.loading[msg.source] = false
		if msg.err != nil {
			if shipLog != nil {
				shipLog.Printf("%s: %v", msg.source, msg.err)
			}
			m.sectionErrs[msg.source] = msg.err.Error()
		} else {
			delete(m.sectionErrs, msg.source)
		}
		m.loadFromCache()
		if m.allDone() {
			m.markRefreshed()
		}

	case autoRefreshMsg:
		if m.allDone() {
			m.sectionErrs = map[string]string{}
			for k := range m.loading {
				m.loading[k] = true
			}
			return m, m.fullRefreshCmd()
		}
		return m, m.autoRefreshTick()

	case actionDoneMsg:
		m.sectionErrs = map[string]string{}
		if msg.err != nil {
			if shipLog != nil {
				shipLog.Printf("action: %v", msg.err)
			}
		}
		for k := range m.loading {
			m.loading[k] = true
		}
		return m, m.fullRefreshCmd()
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
	if m.confirm != nil {
		if m.width > 0 && m.height > 0 {
			return lipgloss.Place(m.width, m.height,
				lipgloss.Center, lipgloss.Center,
				m.renderConfirm())
		}
		return strings.Repeat("\n", 5) + m.renderConfirm()
	}

	var globalIdx int
	var b strings.Builder

	b.WriteString(titleStyle.Render("ship"))
	b.WriteString("\n")

	for i, s := range m.sections {
		headerText := s.name
		if i == m.sectionIdx {
			headerText = "▸ " + headerText
			b.WriteString(sectionStyle.Copy().Foreground(lipgloss.Color("212")).Render(headerText))
		} else {
			b.WriteString(sectionStyle.Render(headerText))
		}
		if s.hideDrafts {
			b.WriteString("  ")
			b.WriteString(helpKey.Render("[no drafts]"))
		}
		if m.searchQuery != "" && i == m.sectionIdx {
			b.WriteString("  ")
			b.WriteString(helpKey.Render("/" + m.searchQuery))
		}
		if m.loading[s.name] {
			b.WriteString("  ")
			b.WriteString(m.spin.View())
		}
		b.WriteString("\n")

		if errMsg, ok := m.sectionErrs[s.name]; ok {
			b.WriteString(errorStyle.Render(errMsg))
			b.WriteString("\n")
		}

		// column header for PR sections
		if len(s.rows) > 0 && s.rows[0].num > 0 {
			repoWidth := 30
			if m.width > 0 {
				repoWidth = m.width * 30 / 100
				if repoWidth < 15 {
					repoWidth = 15
				} else if repoWidth > 50 {
					repoWidth = 50
				}
			}
			header := fmt.Sprintf("CI Rev  %s  #       Title", padWidth("Repo", repoWidth))
			b.WriteString(headerStyle.Render(header))
			b.WriteString("\n")

			visStart := s.scrollOffset
			visEnd := visStart + maxSectionRows
			if visEnd > len(s.rows) {
				visEnd = len(s.rows)
			}

			if visStart > 0 {
				b.WriteString(overflowStyle.Render(fmt.Sprintf("↑ %d more above", visStart)))
				b.WriteString("\n")
			}

			for i := visStart; i < visEnd; i++ {
				r := s.rows[i]
				line := renderRow(r, globalIdx+i == m.cursor, repoWidth, m.width)
				b.WriteString(line)
				b.WriteString("\n")
			}

			remaining := len(s.rows) - visEnd
			if remaining > 0 {
				b.WriteString(overflowStyle.Render(fmt.Sprintf("↓ %d more below", remaining)))
				b.WriteString("\n")
			}

			globalIdx += len(s.rows)
		} else if len(s.rows) > 0 {
			// compute column widths
			maxName := 4  // "Name"
			maxCur := 7   // "Current"
			for _, r := range s.rows {
				if w := lipgloss.Width(r.name); w > maxName {
					maxName = w
				}
				if w := lipgloss.Width(r.title); w > maxCur {
					maxCur = w
				}
			}
			if maxName > 30 {
				maxName = 30
			}
			if maxCur > 40 {
				maxCur = 40
			}
			sep := "  "
			header := fmt.Sprintf("%s  %s  %s", padWidth("Name", maxName), padWidth("Current", maxCur), "Pending")
			b.WriteString(headerStyle.Render(header))
			b.WriteString("\n")

			for i, r := range s.rows {
				line := fmt.Sprintf("%s%s%s%s%s",
					padWidth(truncateWidth(r.name, maxName), maxName),
					sep,
					padWidth(truncateWidth(r.title, maxCur), maxCur),
					sep,
					r.pending)
				if m.width > 0 {
					line = truncateWidth(line, m.width)
				}
				if globalIdx+i == m.cursor {
					b.WriteString(selectedStyle.Render(line))
				} else {
					b.WriteString(rowStyle.Render(line))
				}
				b.WriteString("\n")
			}
			globalIdx += len(s.rows)
		}
	}

	b.WriteString("\n")
	if m.searching {
		prompt := "/" + m.searchQuery + "█"
		b.WriteString(helpKey.Render(prompt))
		b.WriteString("\n")
	}
	b.WriteString(m.viewHelp())

	return b.String()
}

func truncateWidth(s string, max int) string {
	if lipgloss.Width(s) <= max {
		return s
	}
	var out strings.Builder
	var w int
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > max {
			out.WriteString("…")
			break
		}
		out.WriteRune(r)
		w += rw
	}
	return out.String()
}

func padWidth(s string, w int) string {
	sw := lipgloss.Width(s)
	if sw >= w {
		return s
	}
	return s + strings.Repeat(" ", w-sw)
}

func renderRow(r row, selected bool, repoWidth, maxWidth int) string {
	icon := ciIcon(r.ci)

	rev := ""
	if r.review != "" {
		rev = reviewIcon(r.review)
	} else if r.num > 0 {
		rev = "·"
	}

	left := icon + " " + rev

	var line string
	if r.num > 0 {
		title := r.title
		if r.draft {
			title = "[DRAFT] " + title
		}
		repo := truncateWidth(r.repo, repoWidth)
		line = fmt.Sprintf("%s%s  #%-5d  %s",
			padWidth(left, 8), padWidth(repo, repoWidth), r.num, title)
		if maxWidth > 0 {
			line = truncateWidth(line, maxWidth)
		}
	} else {
		line = r.title
		if maxWidth > 0 {
			line = truncateWidth(line, maxWidth)
		}
	}

	if selected {
		return selectedStyle.Render(line)
	}
	return rowStyle.Render(line)
}

func (m Model) viewHelp() string {
	sep := helpKey.Render(" · ")
	line := helpKey.Render("tab/⇧+tab") + sep + helpKey.Render("section") +
		sep + helpKey.Render("j/k") + sep + helpKey.Render("move") +
		sep + helpKey.Render("/") + sep + helpKey.Render("search") +
		sep + helpKey.Render("enter") + sep + helpKey.Render("open") +
		sep + helpKey.Render("d") + sep + helpKey.Render("drafts") +
		sep + helpKey.Render("m") + sep + helpKey.Render("merge") +
		sep + helpKey.Render("c") + sep + helpKey.Render("close") +
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

func (m Model) execAction(action, repo string, num int) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		var err error
		if action == "merge" {
			err = m.gh.MergePR(ctx, repo, num)
		} else {
			err = m.gh.ClosePR(ctx, repo, num)
		}
		return actionDoneMsg{err: err}
	}
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
		IsDraft:        p.IsDraft,
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

func (m Model) renderConfirm() string {
	c := m.confirm
	action := "Merge"
	if c.action == "close" {
		action = "Close"
	}
	var b strings.Builder
	b.WriteString(dialogTitle.Render(action + " PR?"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("%s#%d: %s", c.repo, c.num, c.title))
	b.WriteString("\n\n")
	b.WriteString(dialogHelp.Render("enter to confirm · esc to cancel"))
	return dialogStyle.Render(b.String())
}
