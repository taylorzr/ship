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
)

func (m *Model) maxVisibleRows() int {
	if m.height <= 0 {
		return 15
	}
	n := len(m.sections)
	if n == 0 {
		return 15
	}
	perSection := (m.height - 10) / n
	if perSection < 1 {
		return 1
	}
	if perSection > 15 {
		return 15
	}
	return perSection
}

type row struct {
	title     string
	repo      string
	num       int
	url       string
	ci        string
	review    string
	draft     bool
	mergeable string
	name      string // display name (releases section)
	pending   string // pending versions (releases section)
	updatedAt string // RFC 3339 timestamp
}

type section struct {
	name         string
	rows         []row
	allRows      []row
	scrollOffset int
	hideDrafts   bool
	showStarred  bool
	sortNewest   bool
}

type keyMap struct {
	Up          key.Binding
	Down        key.Binding
	SectionNext key.Binding
	SectionPrev key.Binding
	Enter       key.Binding
	Merge       key.Binding
	Close       key.Binding
	DraftToggle key.Binding
	Refresh     key.Binding
	Quit        key.Binding
	FilterDraft   key.Binding
	FilterStarred key.Binding
	SortToggle    key.Binding
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
	DraftToggle: key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "toggle draft")),
	Refresh:     key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	FilterDraft:   key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "toggle drafts")),
	FilterStarred: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "toggle starred")),
	SortToggle:    key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "toggle sort")),
	Search:        key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
}

type refreshDoneMsg struct {
	source string
	err    error
}

type confirmAction struct {
	action   string // "merge", "close", "draft-toggle", "merge-draft-error", "merge-conflict-error"
	repo     string
	num      int
	title    string
	draft    bool
	warnings []string
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
	gPending      bool
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
			"Team Review":   false,
		},
		sectionErrs: map[string]string{},
	}
	m.loadFromCache()
	return m
}

func (m *Model) sectionProp(name string) (offset int, hideDrafts, showStarred, sortNewest bool) {
	for _, s := range m.sections {
		if s.name == name {
			return s.scrollOffset, s.hideDrafts, s.showStarred, s.sortNewest
		}
	}
	return 0, false, false, true
}

func (m *Model) applyFilters() {
	q := strings.ToLower(m.searchQuery)
	starred := m.cfg.StarredRepos()
	starredSet := make(map[string]bool, len(starred))
	for _, r := range starred {
		starredSet[r] = true
	}
	for i := range m.sections {
		sectionQ := q
		// search only applies to the active section
		if i != m.sectionIdx {
			sectionQ = ""
		}
		if m.sections[i].hideDrafts || sectionQ != "" || m.sections[i].showStarred {
			var filtered []row
			allRows := m.sections[i].allRows
			if !m.sections[i].sortNewest {
				allRows = reversed(allRows)
			}
			for _, r := range allRows {
				if m.sections[i].hideDrafts && r.draft {
					continue
				}
				if m.sections[i].showStarred && !starredSet[r.repo] {
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
		} else if !m.sections[i].sortNewest {
			m.sections[i].rows = reversed(m.sections[i].allRows)
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
	so, hd, ss, sn := m.sectionProp("My PRs")
	s := section{name: "My PRs", scrollOffset: so, hideDrafts: hd, showStarred: ss, sortNewest: sn}
	for _, p := range prs {
		r := row{
				title:  p.Title,
				repo:   p.Repo,
				num:    p.Number,
				url:    p.URL,
				ci:     p.CIState,
				review: p.ReviewDecision,
				draft:  p.IsDraft,
			updatedAt: p.UpdatedAt,
			mergeable: p.Mergeable,
		}
		s.allRows = append(s.allRows, r)
		s.rows = append(s.rows, r)
		}
		sections = append(sections, s)

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

	so, hd, ss, sn = m.sectionProp("To Review")
	dir, _ := m.store.CachedPRs("review-direct")
	team, _ := m.store.CachedPRs("review-team")
	seen := map[string]bool{}
	all := append(dir, team...)
	s2 := section{name: "To Review", scrollOffset: so, hideDrafts: hd, showStarred: ss, sortNewest: sn}
	for _, p := range all {
		key := fmt.Sprintf("%s#%d", p.Repo, p.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		r := row{
			title:  p.Title,
			repo:   p.Repo,
			num:    p.Number,
			url:    p.URL,
			ci:     p.CIState,
			review: p.ReviewDecision,
			draft:  p.IsDraft,
			updatedAt: p.UpdatedAt,
			mergeable: p.Mergeable,
		}
		s2.allRows = append(s2.allRows, r)
		s2.rows = append(s2.rows, r)
	}
	sections = append(sections, s2)

	deps, _ := m.store.CachedPRs("dep")
	so, hd, ss, sn = m.sectionProp("Dependencies")
	s4 := section{name: "Dependencies", scrollOffset: so, hideDrafts: hd, showStarred: ss, sortNewest: sn}
	for _, p := range deps {
		r := row{
			title:  p.Title,
			repo:   p.Repo,
			num:    p.Number,
			url:    p.URL,
			ci:     p.CIState,
			review: p.ReviewDecision,
			draft:  p.IsDraft,
			updatedAt: p.UpdatedAt,
			mergeable: p.Mergeable,
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
	visEnd := start + s.scrollOffset + m.maxVisibleRows()
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
	cmds := []tea.Cmd{
		m.refreshMyPRs(ctx),
		m.refreshReview(ctx),
		m.refreshDeps(ctx),
		m.refreshReleases(ctx),
	}
	if len(m.cfg.GitHub.ReviewTeams) > 0 {
		cmds = append(cmds, m.refreshTeamReview(ctx))
	}
	return cmds
}

func (m Model) refreshMyPRs(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		prs, err := m.gh.MyPRs(ctx, m.cfg.GitHub.Owners)
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
		revs, err := m.gh.ReviewRequested(ctx, m.cfg.GitHub.Owners)
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

func (m Model) refreshTeamReview(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		prs, err := m.gh.TeamReviewRequested(ctx, m.cfg.GitHub.ReviewTeams)
		if err == nil {
			cache := make([]store.CachedPR, len(prs))
			for i, p := range prs {
				cache[i] = toCached(p, "review-team")
			}
			m.store.SavePRs(cache, "review-team")
		}
		return refreshDoneMsg{source: "Team Review", err: err}
	}
}

func (m Model) refreshDeps(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		starred := m.cfg.StarredRepos()
		deps, err := m.gh.DepPRs(ctx, starred, m.cfg.GitHub.DepAuthors)
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
	return tea.Tick(time.Duration(m.cfg.RefreshInterval)*time.Second, func(t time.Time) tea.Msg {
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
			m.gPending = false
			switch {
			case key.Matches(msg, keys.Quit):
				return m, tea.Quit
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
				m.confirm = nil
			case key.Matches(msg, keys.Enter):
				if m.confirm.action == "merge-draft-error" || m.confirm.action == "merge-conflict-error" {
					m.confirm = nil
					break
				}
				action := m.confirm.action
				repo := m.confirm.repo
				num := m.confirm.num
				draft := m.confirm.draft
				m.confirm = nil
				return m, m.execAction(action, repo, num, draft)
			}
			return m, nil
		}
		if m.searching {
			m.gPending = false
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
			if m.sections[m.sectionIdx].name == "To Review" {
				break
			}
			r := m.currentRow()
			if r != nil && r.num > 0 {
				if r.draft {
					m.confirm = &confirmAction{action: "merge-draft-error", title: r.title}
				} else if r.mergeable == "CONFLICTING" {
					m.confirm = &confirmAction{action: "merge-conflict-error", title: r.title}
				} else {
					warnings := []string{}
					if r.ci == "failure" {
						warnings = append(warnings, "CI: failing")
					}
					if r.review == "CHANGES_REQUESTED" {
						warnings = append(warnings, "Review: changes requested")
					}
					m.confirm = &confirmAction{
						action:   "merge",
						repo:     r.repo,
						num:      r.num,
						title:    r.title,
						warnings: warnings,
					}
				}
			}
		case key.Matches(msg, keys.Close):
			if m.sections[m.sectionIdx].name == "My PRs" || m.sections[m.sectionIdx].name == "Dependencies" {
				r := m.currentRow()
				if r != nil && r.num > 0 {
					m.confirm = &confirmAction{action: "close", repo: r.repo, num: r.num, title: r.title}
				}
			}
		case key.Matches(msg, keys.DraftToggle):
			if m.sections[m.sectionIdx].name == "My PRs" || m.sections[m.sectionIdx].name == "Dependencies" {
				r := m.currentRow()
				if r != nil && r.num > 0 {
					m.confirm = &confirmAction{action: "draft-toggle", repo: r.repo, num: r.num, title: r.title, draft: r.draft}
				}
			}
		case key.Matches(msg, keys.Refresh):
			m.sectionErrs = map[string]string{}
			for k := range m.loading {
				m.loading[k] = true
			}
			return m, m.fullRefreshCmd()
		case key.Matches(msg, keys.FilterDraft):
			if len(m.sections) > m.sectionIdx {
				s := m.sections[m.sectionIdx]
				if s.name != "Releases" {
					s.hideDrafts = !s.hideDrafts
					m.sections[m.sectionIdx] = s
					m.applyFilters()
				}
			}
			return m, nil
		case key.Matches(msg, keys.FilterStarred):
			if len(m.sections) > m.sectionIdx {
				s := m.sections[m.sectionIdx]
				if s.name != "Releases" {
					s.showStarred = !s.showStarred
					m.sections[m.sectionIdx] = s
					m.applyFilters()
				}
			}
			return m, nil
		case key.Matches(msg, keys.SortToggle):
			if len(m.sections) > m.sectionIdx {
				s := m.sections[m.sectionIdx]
				if s.name != "Releases" {
					s.sortNewest = !s.sortNewest
					m.sections[m.sectionIdx] = s
					m.applyFilters()
				}
			}
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("g"))):
			if m.gPending {
				m.gPending = false
				// gg — go to top of section
				if len(m.sections) > m.sectionIdx {
					m.sections[m.sectionIdx].scrollOffset = 0
					m.cursor = m.sectionOffset(m.sectionIdx)
				}
			} else {
				m.gPending = true
			}
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("G"))):
			m.gPending = false
			if len(m.sections) > m.sectionIdx {
				s := m.sections[m.sectionIdx]
				last := len(s.rows) - 1
				if last > 0 {
					visible := m.maxVisibleRows()
					s.scrollOffset = last - visible + 1
					if s.scrollOffset < 0 {
						s.scrollOffset = 0
					}
					m.sections[m.sectionIdx] = s
					m.cursor = m.sectionOffset(m.sectionIdx) + last
				}
			}
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
		default:
			m.gPending = false
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
		if s.showStarred {
			b.WriteString("  ")
			b.WriteString(helpKey.Render("[starred]"))
		}
		if !s.sortNewest {
			b.WriteString("  ")
			b.WriteString(helpKey.Render("[oldest]"))
		}
		if i == m.sectionIdx && (m.searching || m.searchQuery != "") {
			q := m.searchQuery
			if m.searching {
				q += "█"
			}
			b.WriteString("  ")
			b.WriteString(helpKey.Render("/" + q))
		}
		if m.loading[s.name] {
			b.WriteString("  ")
			b.WriteString(m.spin.View())
		}
		b.WriteString("\n")

		if errMsg, ok := m.sectionErrs[s.name]; ok {
			b.WriteString(errorStyle.Render(errMsg))
			b.WriteString("\n")
		} else if s.name == "To Review" {
			if errMsg, ok := m.sectionErrs["Team Review"]; ok {
				b.WriteString(errorStyle.Render(errMsg))
				b.WriteString("\n")
			}
		}

		// column header for PR sections
		if len(s.rows) > 0 && s.rows[0].num > 0 {
			repoWidth := 30
			ageWidth := 6
			if m.width > 0 {
				repoWidth = m.width * 30 / 100
				if repoWidth < 15 {
					repoWidth = 15
				} else if repoWidth > 50 {
					repoWidth = 50
				}
			}
			titleWidth := m.width - repoWidth - 28
			if titleWidth < 1 {
				titleWidth = 1
			}
			header := fmt.Sprintf("%s%s  %s  %s  %s",
				padWidth("CI Rev", 8),
				padWidth("Repo", repoWidth),
				padWidth("#", 6),
				padWidth("Title", titleWidth),
				padWidth("Age", ageWidth))
			b.WriteString(headerStyle.Render(header))
			b.WriteString("\n")

			visStart := s.scrollOffset
			visEnd := visStart + m.maxVisibleRows()
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
	b.WriteString(m.viewHelp())

	return b.String()
}

func truncateWidth(s string, max int) string {
	if max < 1 {
		return ""
	}
	if lipgloss.Width(s) <= max {
		return s
	}
	var out strings.Builder
	var w int
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > max-1 {
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

func reversed[T any](s []T) []T {
	r := make([]T, len(s))
	for i, v := range s {
		r[len(s)-1-i] = v
	}
	return r
}

func relativeTime(s string) string {
	t, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
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

	const ageWidth = 6

	var line string
	if r.num > 0 {
		title := r.title
		if r.draft {
			title = "[DRAFT] " + title
		}
		ts := relativeTime(r.updatedAt)
		repo := truncateWidth(r.repo, repoWidth)
		titleWidth := maxWidth - repoWidth - 28
		if titleWidth < 1 {
			titleWidth = 1
		}
		line = fmt.Sprintf("%s%s  #%-5d  %s  %s",
			padWidth(left, 8), padWidth(repo, repoWidth), r.num, padWidth(truncateWidth(title, titleWidth), titleWidth), padWidth(ts, ageWidth))
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
		sep + helpKey.Render("s") + sep + helpKey.Render("starred") +
		sep + helpKey.Render("S") + sep + helpKey.Render("sort") +
		sep + helpKey.Render("gg") + sep + helpKey.Render("top") +
		sep + helpKey.Render("G") + sep + helpKey.Render("bottom") +
		sep + helpKey.Render("m") + sep + helpKey.Render("merge") +
		sep + helpKey.Render("c") + sep + helpKey.Render("close") +
		sep + helpKey.Render("D") + sep + helpKey.Render("toggle draft") +
		sep + helpKey.Render("r") + sep + helpKey.Render("refresh")
	if !m.lastRefreshed.IsZero() {
		line += helpKey.Render(" (auto in " + refreshCountdown(m.lastRefreshed, m.cfg.RefreshInterval) + ")")
	}
	line += sep + helpKey.Render("q") + sep + helpKey.Render("quit")
	return line
}

func (m Model) fullRefreshCmd() tea.Cmd {
	cmds := []tea.Cmd{m.autoRefreshTick()}
	cmds = append(cmds, m.sectionCommands(context.Background())...)
	return tea.Batch(cmds...)
}

func (m Model) execAction(action, repo string, num int, draft bool) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		var err error
		switch action {
		case "merge":
			err = m.gh.MergePR(ctx, repo, num)
		case "close":
			err = m.gh.ClosePR(ctx, repo, num)
		case "draft-toggle":
			err = m.gh.ToggleDraft(ctx, repo, num, draft)
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

func refreshCountdown(t time.Time, interval int) string {
	remaining := interval - int(time.Since(t).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("%ds", remaining)
}

func (m Model) renderConfirm() string {
	c := m.confirm
	var b strings.Builder

	switch c.action {
	case "merge-draft-error":
		b.WriteString(dialogTitle.Render("Can't merge draft PR"))
		b.WriteString("\n\n")
		b.WriteString("Mark it ready for review first (D) then merge.")
		b.WriteString("\n\n")
		b.WriteString(dialogHelp.Render("esc to dismiss"))
		return dialogStyle.Render(b.String())
	case "merge-conflict-error":
		b.WriteString(dialogTitle.Render("Can't merge — conflicts"))
		b.WriteString("\n\n")
		b.WriteString("This PR has merge conflicts that need to be resolved first.")
		b.WriteString("\n\n")
		b.WriteString(dialogHelp.Render("esc to dismiss"))
		return dialogStyle.Render(b.String())
	case "draft-toggle":
		if c.draft {
			b.WriteString(dialogTitle.Render("Ready for review?"))
		} else {
			b.WriteString(dialogTitle.Render("Convert to draft?"))
		}
	default:
		action := "Merge"
		if c.action == "close" {
			action = "Close"
		}
		b.WriteString(dialogTitle.Render(action + " PR?"))
	}
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("%s#%d: %s", c.repo, c.num, c.title))
	if len(c.warnings) > 0 {
		b.WriteString("\n\n")
		for _, w := range c.warnings {
			b.WriteString(dialogHelp.Render("⚠ " + w))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(dialogHelp.Render("enter to confirm · esc to cancel"))
	return dialogStyle.Render(b.String())
}
