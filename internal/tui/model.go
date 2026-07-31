package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	"github.com/zach/ship/internal/ai"
)

var shipLog *log.Logger

type errorMsg string
type refreshMsg struct{}

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
	inputStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
)

func (m *Model) maxVisibleRows() int {
	if m.height <= 0 {
		return 15
	}
	n := len(m.sections)
	if n == 0 {
		return 15
	}
	// overhead: title + help + per-section (name + header) + active section actions
	overhead := 2 + 2*n + 1
	perSection := (m.height - overhead) / n
	if perSection < 1 {
		return 1
	}
	if perSection > 10 {
		return 10
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
	sha       string // commit SHA
	updatedAt string // RFC 3339 timestamp
	depth     int    // nesting level for releases section
	prodRef   string // prod tag/SHA for compare URLs
	role      string // review role ("review-direct" or "review-team")
	reviewed  bool   // AI review dispatched
	reviewStale bool // AI review exists but head SHA changed
	headSha   string // PR head SHA for stale check
}

type section struct {
	name            string
	rows            []row
	allRows         []row
	scrollOffset    int
	hideDrafts      bool
	showStarred     bool
	sortNewest      bool
	hideTeamReviews bool
	statusFilter    string // "" or "mergeable"
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
	FilterStarred key.Binding
	SortToggle    key.Binding
	Search        key.Binding
	Diff          key.Binding
	Deploy        key.Binding
	TeamFilter    key.Binding
	MergeFilter   key.Binding
	AIReview      key.Binding
	Browse        key.Binding
	HelpToggle    key.Binding
	TagAction     key.Binding
}

var keys = keyMap{
	Up:          key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "move up")),
	Down:        key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "move down")),
	SectionNext: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next section")),
	SectionPrev: key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("⇧+tab", "prev section")),
	Enter:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	Merge:       key.NewBinding(key.WithKeys("M"), key.WithHelp("M", "merge")),
	Close:       key.NewBinding(key.WithKeys("C"), key.WithHelp("C", "close")),
	DraftToggle: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "toggle draft")),
	Refresh:     key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	FilterStarred: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "toggle starred")),
	SortToggle:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "toggle age-sort")),
	Search:        key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
	Diff:          key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "toggle drafts/diff")),
	Deploy:        key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "ship")),
	TeamFilter:    key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "toggle team")),
	MergeFilter:   key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "toggle mergeable")),
	AIReview:      key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "AI-review")),
	Browse:        key.NewBinding(key.WithKeys("B"), key.WithHelp("B", "browse on GitHub")),
	HelpToggle:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	TagAction:     key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "tag/release")),
}

type refreshDoneMsg struct {
	source string
	err    error
	// retryAfterLogin asks Update to re-run this source once: an auto-login
	// completed mid-refresh too late for some services to recover inline.
	retryAfterLogin bool
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
	tagging       bool
	tagQuery      string
	tagMeta       tagState
	showHelp      bool
	gPending      bool
	mockK8sImages map[string]string
}

type tagState struct {
	repo        string
	sha         string
	branch      string
	latest      string
	hasReleases bool
	loading     bool
}

type tagMetaMsg struct {
	latest      string
	hasReleases bool
}

func New(cfg *config.Config, st *store.Store, ghClient *gh.Client, mockK8sImages map[string]string) Model {
	s := spinner.New()
	s.Spinner = spinner.Ellipsis
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	loading := map[string]bool{
		"My PRs":       false,
		"To Review":    false,
		"Services":     false,
		"Dependencies": false,
	}
	if len(cfg.GitHub.Teams) > 0 {
		loading["Team Review"] = false
	}
	m := Model{
		cfg:         cfg,
		store:       st,
		gh:          ghClient,
		spin:        s,
		loading:     loading,
		sectionErrs: map[string]string{},
		mockK8sImages: mockK8sImages,
	}
	m.loadFromCache()
	return m
}

func (m *Model) sectionProp(name string) (offset int, hideDrafts, showStarred, sortNewest, hideTeamReviews bool, statusFilter string) {
	for _, s := range m.sections {
		if s.name == name {
			return s.scrollOffset, s.hideDrafts, s.showStarred, s.sortNewest, s.hideTeamReviews, s.statusFilter
		}
	}
	return 0, false, len(m.cfg.StarredRepos()) > 0, true, false, ""
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
		if m.sections[i].hideDrafts || sectionQ != "" || m.sections[i].showStarred || m.sections[i].hideTeamReviews || m.sections[i].statusFilter != "" {
			var filtered []row
			allRows := m.sections[i].allRows
			if !m.sections[i].sortNewest {
				allRows = reversed(allRows)
			}
			for _, r := range allRows {
				if m.sections[i].hideDrafts && r.draft {
					continue
				}
				if m.sections[i].statusFilter == "mergeable" && (r.ci != "success" || r.review != "APPROVED") {
					continue
				}
				if m.sections[i].hideTeamReviews && r.role == "review-team" {
					continue
				}
				if m.sections[i].showStarred && m.sections[i].name != "Services" && !starredSet[r.repo] {
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
	if visibleRows(m.sections[m.sectionIdx]) == 0 {
		m.cursor = -1
	} else {
		start := m.sectionOffset(m.sectionIdx)
		end := m.sectionEnd(m.sectionIdx)
		if m.cursor < start || m.cursor >= end {
			m.cursor = start
		}
	}
}

func (m *Model) loadReviewState(r *row) {
	storedSha, _, _ := m.store.LoadReview(r.repo, r.num)
	if storedSha != "" {
		r.reviewed = true
		if storedSha != r.headSha {
			r.reviewStale = true
		}
	}
}

func (m *Model) loadFromCache() {
	var sections []section

	prs, _ := m.store.CachedPRs("mine")
	so, hd, ss, sn, _, _ := m.sectionProp("My PRs")
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
			headSha: p.HeadSHA,
		}
		m.loadReviewState(&r)
		s.allRows = append(s.allRows, r)
		s.rows = append(s.rows, r)
		}
		sections = append(sections, s)

	versions, _ := m.store.CachedVersions()
	svcNames := make(map[string]string, len(m.cfg.Services))
	svcRepos := make(map[string]bool, len(m.cfg.Services))
	for _, svc := range m.cfg.Services {
		svcNames[svc.Repo] = svc.Name
		svcRepos[svc.Repo] = true
	}
	so, hd, ss, sn, _, _ = m.sectionProp("Services")
	s3 := section{name: "Services", scrollOffset: so, hideDrafts: hd, showStarred: ss, sortNewest: sn}
	for _, v := range versions {
		if !svcRepos[v.Repo] {
			continue
		}
		title := v.ProdRef
		if title == "" {
			title = "-"
		}
		name := v.Repo
		if n, ok := svcNames[v.Repo]; ok && n != "" {
			name = n
		}
		r := row{
			name:    name,
			title:   title,
			repo:    v.Repo,
			prodRef: v.ProdRef,
			url:     fmt.Sprintf("https://github.com/%s/releases/tag/%s", v.Repo, v.ProdRef),
			depth:   0,
		}
		if v.Error != "" {
			r.title = "✗ " + v.Error
		} else if v.PendingTags == "" {
			r.pending = "-"
		} else {
			tags := strings.Split(v.PendingTags, ", ")
			r.pending = fmt.Sprintf("+%d", len(tags))
		}
		s3.allRows = append(s3.allRows, r)
		s3.rows = append(s3.rows, r)
		if v.PendingTags != "" && v.Error == "" {
			for _, entry := range strings.Split(v.PendingTags, ", ") {
				if entry == "" {
					continue
				}
				tag, title := entry, ""
				if parts := strings.SplitN(entry, "|", 2); len(parts) == 2 {
					tag, title = parts[0], parts[1]
				}
				pending := tag
				if title != "" {
					pending = tag + ": " + title
				}
				pr := row{
					pending: pending,
					repo:    v.Repo,
					prodRef: v.ProdRef,
					url:     fmt.Sprintf("https://github.com/%s/releases/tag/%s", v.Repo, tag),
					depth:   1,
				}
				s3.allRows = append(s3.allRows, pr)
				s3.rows = append(s3.rows, pr)
			}
		}
		if v.UntaggedCommits != "" && v.Error == "" {
			var commits []struct {
				SHA     string `json:"sha"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(v.UntaggedCommits), &commits); err == nil {
				for _, c := range commits {
					pr := row{
						sha:     c.SHA[:7],
						pending: fmt.Sprintf("%s: %s", c.SHA[:7], c.Message),
						repo:    v.Repo,
						prodRef: v.ProdRef,
						depth:   1,
					}
					s3.allRows = append(s3.allRows, pr)
					s3.rows = append(s3.rows, pr)
				}
			}
		}
	}
	sections = append(sections, s3)

	so, hd, ss, sn, htr, sf := m.sectionProp("To Review")
	dir, _ := m.store.CachedPRs("review-direct")
	team, _ := m.store.CachedPRs("review-team")
	seen := map[string]bool{}
	s2 := section{name: "To Review", scrollOffset: so, hideDrafts: hd, showStarred: ss, sortNewest: sn, hideTeamReviews: htr, statusFilter: sf}
	for _, p := range dir {
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
			role:     "review-direct",
			headSha: p.HeadSHA,
		}
		m.loadReviewState(&r)
		s2.allRows = append(s2.allRows, r)
		s2.rows = append(s2.rows, r)
	}
	for _, p := range team {
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
			role:     "review-team",
			headSha: p.HeadSHA,
		}
		m.loadReviewState(&r)
		s2.allRows = append(s2.allRows, r)
		s2.rows = append(s2.rows, r)
	}
	sections = append(sections, s2)

	deps, _ := m.store.CachedPRs("dep")
	so, hd, ss, sn, _, _ = m.sectionProp("Dependencies")
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
			headSha: p.HeadSHA,
		}
		m.loadReviewState(&r)
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

func (m *Model) advanceSection(dir int) {
	m.sectionIdx = (m.sectionIdx + dir) % len(m.sections)
	if m.sectionIdx < 0 {
		m.sectionIdx = len(m.sections) - 1
	}
	if visibleRows(m.sections[m.sectionIdx]) > 0 {
		m.cursor = m.sectionOffset(m.sectionIdx) + m.sections[m.sectionIdx].scrollOffset
	} else {
		m.cursor = -1
	}
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
	if len(m.cfg.GitHub.Teams) > 0 {
		cmds = append(cmds, m.refreshTeamReview(ctx))
	}
	return cmds
}

func (m Model) refreshMyPRs(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		if shipLog != nil {
			shipLog.Printf("refreshing My PRs")
		}
		start := time.Now()
		prs, err := m.gh.MyPRs(ctx, m.cfg.GitHub.Owners)
		if shipLog != nil {
			dur := time.Since(start).Truncate(time.Millisecond)
			if err != nil {
				shipLog.Printf("My PRs: %v (%v)", err, dur)
			} else {
				shipLog.Printf("My PRs: %d PRs (%v)", len(prs), dur)
			}
		}
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
		if shipLog != nil {
			shipLog.Printf("refreshing To Review")
		}
		start := time.Now()
		revs, err := m.gh.ReviewRequested(ctx, m.cfg.GitHub.Owners)
		if shipLog != nil {
			dur := time.Since(start).Truncate(time.Millisecond)
			if err != nil {
				shipLog.Printf("To Review: %v (%v)", err, dur)
			} else {
				shipLog.Printf("To Review: %d PRs (%v)", len(revs), dur)
			}
		}
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
		if shipLog != nil {
			shipLog.Printf("refreshing Team Review")
		}
		start := time.Now()
		prs, err := m.gh.TeamReviewRequested(ctx, m.cfg.GitHub.Teams)
		if shipLog != nil {
			dur := time.Since(start).Truncate(time.Millisecond)
			if err != nil {
				shipLog.Printf("Team Review: %v (%v)", err, dur)
			} else {
				shipLog.Printf("Team Review: %d PRs (%v)", len(prs), dur)
			}
		}
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
		if len(starred) == 0 && len(m.cfg.GitHub.Owners) == 0 && len(m.cfg.GitHub.Teams) == 0 {
			if shipLog != nil {
				shipLog.Printf("Dependencies: skipped (no starred repos, owners, or teams)")
			}
			return refreshDoneMsg{source: "Dependencies"}
		}
		if shipLog != nil {
			shipLog.Printf("refreshing Dependencies")
		}
		start := time.Now()
		deps, err := m.gh.DepPRs(ctx, starred, m.cfg.GitHub.Owners, m.cfg.GitHub.Teams, m.cfg.GitHub.DepAuthors)
		if shipLog != nil {
			dur := time.Since(start).Truncate(time.Millisecond)
			if err != nil {
				shipLog.Printf("Dependencies: %v (%v)", err, dur)
			} else {
				shipLog.Printf("Dependencies: %d PRs (%v)", len(deps), dur)
			}
		}
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
		if shipLog != nil {
			shipLog.Printf("refreshing Releases (%d services)", len(m.cfg.Services))
		}
		start := time.Now()
		loginBefore := k8s.LastLogin()

		type svcResult struct {
			Repo            string
			ProdRef         string
			ProdSHA         string
			AheadBy         int
			PendingTags     string
			UntaggedCommits string
			Error           string
		}

		var wg sync.WaitGroup
		results := make(chan svcResult, len(m.cfg.Services))

		for _, svc := range m.cfg.Services {
			wg.Add(1)
			go func(svc config.ServiceConfig) {
				defer wg.Done()
				svcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()

				svcStart := time.Now()
				var rc k8s.Client
				if len(m.mockK8sImages) > 0 {
					img, ok := m.mockK8sImages[svc.Name]
					if !ok {
						img, ok = m.mockK8sImages[svc.Repo]
					}
					if !ok {
						img, ok = m.mockK8sImages["*"]
					}
					if !ok {
						results <- svcResult{Repo: svc.Repo, Error: fmt.Sprintf("mock: no image for service %q (key by name or repo)", svc.Name)}
						return
					}
					rc = k8s.NewMock(map[string]string{"*": img})
				} else {
					var err error
					rc, err = k8s.NewRealClient(svcCtx, "", svc.Context, m.cfg.K8s.LoginCommand)
					if err != nil {
						results <- svcResult{Repo: svc.Repo, Error: fmt.Sprintf("k8s: %v", err)}
						return
					}
				}
				r := version.Resolve(svcCtx, rc, m.gh, svc)
				pending := ""
				for i, t := range r.PendingTags {
					if i > 0 {
						pending += ", "
					}
					if t.Title != "" && t.Title != t.Name {
						pending += t.Name + "|" + t.Title
					} else {
						pending += t.Name
					}
				}
				untagged := ""
				if len(r.UntaggedCommits) > 0 {
					var buf strings.Builder
					buf.WriteByte('[')
					for i, c := range r.UntaggedCommits {
						if i > 0 {
							buf.WriteByte(',')
						}
						fmt.Fprintf(&buf, `{"sha":%q,"message":%q}`, c.SHA[:7], c.Message)
					}
					buf.WriteByte(']')
					untagged = buf.String()
				}
				if shipLog != nil {
					dur := time.Since(svcStart).Truncate(time.Millisecond)
					if r.Error != "" {
						shipLog.Printf("  %s: %s (%v)", svc.Repo, r.Error, dur)
					} else {
						shipLog.Printf("  %s: ahead=%d tags=%q (%v)", svc.Repo, r.AheadBy, pending, dur)
					}
				}
				results <- svcResult{
					Repo:            svc.Repo,
					ProdRef:         r.ProdRef,
					ProdSHA:         r.ProdSHA,
					AheadBy:         r.AheadBy,
					PendingTags:     pending,
					UntaggedCommits: untagged,
					Error:           r.Error,
				}
			}(svc)
		}

		wg.Wait()
		close(results)

		var lastErr error
		for res := range results {
			m.store.SaveVersion(store.CachedVersion{
				Repo:            res.Repo,
				ProdRef:         res.ProdRef,
				ProdSHA:         res.ProdSHA,
				AheadBy:         res.AheadBy,
				PendingTags:     res.PendingTags,
				UntaggedCommits: res.UntaggedCommits,
				Error:           res.Error,
			})
			if res.Error != "" {
				lastErr = fmt.Errorf("%s: %s", res.Repo, res.Error)
			}
		}
		total := time.Since(start).Truncate(time.Millisecond)
		if shipLog != nil {
			if lastErr != nil {
				shipLog.Printf("Releases: %v (%v)", lastErr, total)
			} else {
				shipLog.Printf("Releases: %d services ok (%v)", len(m.cfg.Services), total)
			}
		}
		// If a k8s auto-login completed during this cycle, some services may
		// have failed before their credentials were refreshed — re-run once.
		retry := lastErr != nil && k8s.LastLogin().After(loginBefore)
		if retry && shipLog != nil {
			shipLog.Printf("Services: re-refreshing after auto-login")
		}
		return refreshDoneMsg{source: "Services", err: lastErr, retryAfterLogin: retry}
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
		if m.tagging {
			m.gPending = false
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
				m.tagging = false
				m.tagQuery = ""
			case key.Matches(msg, keys.Enter):
				tag := strings.TrimSpace(m.tagQuery)
				m.tagging = false
				m.tagQuery = ""
				if tag != "" {
					return m, m.createTagRelease(m.tagMeta.repo, tag, m.tagMeta.sha, m.tagMeta.branch)
				}
		case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+o"))):
			tag := strings.TrimSpace(m.tagQuery)
			m.tagging = false
			m.tagQuery = ""
			if tag != "" {
				url := fmt.Sprintf("https://github.com/%s/releases/new?tag=%s&target=%s", m.tagMeta.repo, url.QueryEscape(tag), m.tagMeta.branch)
				openBrowser(url)
			}
			case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
				if len(m.tagQuery) > 0 {
					m.tagQuery = m.tagQuery[:len(m.tagQuery)-1]
				}
			default:
				r := []rune(msg.String())
				if len(r) == 1 && r[0] >= 32 && r[0] <= 126 {
					m.tagQuery += string(r[0])
				}
			}
			return m, nil
		}
		switch {
		case key.Matches(msg, keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, keys.Down):
			if visibleRows(m.sections[m.sectionIdx]) > 0 {
				s := &m.sections[m.sectionIdx]
				end := m.sectionOffset(m.sectionIdx) + len(s.rows)
				if m.cursor >= end-1 {
					m.advanceSection(1)
				} else {
					m.cursor = m.nextRow()
				}
			} else {
				m.advanceSection(1)
			}
		case key.Matches(msg, keys.Up):
			if visibleRows(m.sections[m.sectionIdx]) > 0 {
				start := m.sectionOffset(m.sectionIdx)
				if m.cursor <= start {
					m.advanceSection(-1)
				} else {
					m.cursor = m.prevRow()
				}
			} else {
				m.advanceSection(-1)
			}
		case key.Matches(msg, keys.SectionNext):
			m.advanceSection(1)
		case key.Matches(msg, keys.SectionPrev):
			m.advanceSection(-1)
		case key.Matches(msg, keys.Enter):
			r := m.currentRow()
			if r != nil && r.url != "" {
				openBrowser(r.url)
			}
		case key.Matches(msg, keys.Diff):
			switch m.sections[m.sectionIdx].name {
			case "Services":
				r := m.currentRow()
				if r != nil && r.repo != "" && r.prodRef != "" {
					var url string
					if r.depth == 0 {
						var branch string
						for _, svc := range m.cfg.Services {
							if svc.Repo == r.repo {
								branch = svc.Branch
								break
							}
						}
						if branch == "" {
							branch = m.gh.DefaultBranch(context.Background(), r.repo)
						}
						url = fmt.Sprintf("https://github.com/%s/compare/%s..%s", r.repo, r.prodRef, branch)
					} else {
						compareRef := r.pending
						if idx := strings.Index(compareRef, ": "); idx != -1 {
							compareRef = compareRef[:idx]
						}
						url = fmt.Sprintf("https://github.com/%s/compare/%s..%s", r.repo, r.prodRef, compareRef)
					}
					if url != "" {
						openBrowser(url)
					}
				}
			default:
				if len(m.sections) > m.sectionIdx {
					s := m.sections[m.sectionIdx]
					if s.name != "Services" {
						s.hideDrafts = !s.hideDrafts
						m.sections[m.sectionIdx] = s
						m.applyFilters()
					}
				}
				return m, nil
			}
		case key.Matches(msg, keys.TeamFilter):
			if m.sections[m.sectionIdx].name == "To Review" {
				s := m.sections[m.sectionIdx]
				s.hideTeamReviews = !s.hideTeamReviews
				m.sections[m.sectionIdx] = s
				m.applyFilters()
			}
		case key.Matches(msg, keys.MergeFilter):
			if m.sections[m.sectionIdx].name != "Services" {
				s := m.sections[m.sectionIdx]
				if s.statusFilter == "mergeable" {
					s.statusFilter = ""
				} else {
					s.statusFilter = "mergeable"
				}
				m.sections[m.sectionIdx] = s
				m.applyFilters()
			}
		case key.Matches(msg, keys.Deploy):
			if m.sections[m.sectionIdx].name == "Services" {
				r := m.currentRow()
				if r != nil && r.repo != "" {
					for _, svc := range m.cfg.Services {
						if svc.Repo == r.repo && svc.DeployURL != "" {
							openBrowser(svc.DeployURL)
							break
						}
					}
				}
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
		case key.Matches(msg, keys.AIReview):
			r := m.currentRow()
			if r != nil && r.num > 0 {
				bg := context.Background()
				headSha, err := m.gh.GetHeadSha(bg, r.repo, r.num)
				if err != nil {
					m.sectionErrs[m.sections[m.sectionIdx].name] = fmt.Sprintf("AI review: %v", err)
					break
				}
				cmd := m.cfg.AI.ReviewCommand
				if cmd == "" {
					cmd = m.cfg.AI.ReviewProvider
				}
				if cmd == "" {
					cmd = "claude"
				}
				if err := ai.LaunchReview(r.repo, r.num, cmd); err != nil {
					m.sectionErrs[m.sections[m.sectionIdx].name] = fmt.Sprintf("AI review: %v", err)
					break
				}
				m.store.SaveReview(r.repo, r.num, headSha)
				// update row in-place
				for i := range m.sections {
					for j := range m.sections[i].allRows {
						if m.sections[i].allRows[j].num == r.num && m.sections[i].allRows[j].repo == r.repo {
							m.sections[i].allRows[j].reviewed = true
							m.sections[i].allRows[j].headSha = headSha
						}
					}
					for j := range m.sections[i].rows {
						if m.sections[i].rows[j].num == r.num && m.sections[i].rows[j].repo == r.repo {
							m.sections[i].rows[j].reviewed = true
							m.sections[i].rows[j].headSha = headSha
						}
					}
				}
			}
		case key.Matches(msg, keys.Browse):
			m.openBrowse()
		case key.Matches(msg, keys.TagAction):
			if len(m.sections) > m.sectionIdx {
				s := m.sections[m.sectionIdx]
				if s.name == "Services" {
					r := m.currentRow()
					if r != nil && r.url == "" && r.sha != "" {
						var versioning, branch string
						for _, svc := range m.cfg.Services {
							if svc.Repo == r.repo {
								versioning = svc.Versioning
								branch = svc.Branch
								break
							}
						}
						if branch == "" {
							branch = m.gh.DefaultBranch(context.Background(), r.repo)
						}
						m.tagging = true
						m.tagQuery = ""
						m.tagMeta = tagState{repo: r.repo, sha: r.sha, branch: branch, loading: true}
						return m, m.fetchTagMetaCmd(r.repo, versioning)
					}
				}
			}
			return m, nil
		case key.Matches(msg, keys.Refresh):
			m.sectionErrs = map[string]string{}
			for k := range m.loading {
				m.loading[k] = true
			}
			return m, m.fullRefreshCmd()
		case key.Matches(msg, keys.FilterStarred):
			if len(m.sections) > m.sectionIdx {
				s := m.sections[m.sectionIdx]
				if s.name != "Services" {
					s.showStarred = !s.showStarred
					m.sections[m.sectionIdx] = s
					m.applyFilters()
				}
			}
			return m, nil
		case key.Matches(msg, keys.SortToggle):
			if len(m.sections) > m.sectionIdx {
				s := m.sections[m.sectionIdx]
				if s.name != "Services" {
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
				if len(m.sections) > m.sectionIdx && visibleRows(m.sections[m.sectionIdx]) > 0 {
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
		case key.Matches(msg, keys.HelpToggle):
			m.showHelp = !m.showHelp
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			if m.showHelp {
				m.showHelp = false
				return m, nil
			}
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
		if msg.retryAfterLogin {
			m.loading[msg.source] = true
			delete(m.sectionErrs, msg.source)
			return m, m.refreshReleases(context.Background())
		}
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

	case tagMetaMsg:
		m.tagMeta.latest = msg.latest
		m.tagMeta.hasReleases = msg.hasReleases
		m.tagMeta.loading = false
		if m.tagging && m.tagQuery == "" {
			for _, svc := range m.cfg.Services {
				if svc.Repo == m.tagMeta.repo && svc.Versioning == "sequential" {
					suggest := nextSequentialVersion(msg.latest)
					m.tagQuery = suggest
				}
			}
		}
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
	if m.total == 0 || m.cursor < 0 {
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

func nextSequentialVersion(latest string) string {
	if latest == "" {
		return "v0.0.1"
	}
	prefix := ""
	if strings.HasPrefix(strings.ToLower(latest), "v") {
		prefix = "v"
	}
	trimmed := strings.TrimLeft(latest, "vV")
	parts := strings.Split(trimmed, ".")
	if len(parts) == 0 {
		return prefix + "0.0.1"
	}
	last := parts[len(parts)-1]
	n, err := strconv.Atoi(last)
	if err != nil {
		return ""
	}
	parts[len(parts)-1] = strconv.Itoa(n + 1)
	return prefix + strings.Join(parts, ".")
}

func (m *Model) fetchTagMetaCmd(repo, versioning string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		latest, _ := m.gh.LatestTag(ctx, repo)
		hasReleases, _ := m.gh.RepoHasReleases(ctx, repo)
		return tagMetaMsg{latest: latest, hasReleases: hasReleases}
	}
}

func (m *Model) createTagRelease(repo, tag, sha, branch string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		hasReleases, err := m.gh.RepoHasReleases(ctx, repo)
		if err != nil {
			return errorMsg(fmt.Sprintf("create tag/release: %v", err))
		}
		if hasReleases {
			err = m.gh.CreateRelease(ctx, repo, tag, branch)
		} else {
			err = m.gh.CreateTag(ctx, repo, tag, sha)
		}
		if err != nil {
			return errorMsg(err.Error())
		}
		return refreshMsg{}
	}
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

	b.WriteString(titleStyle.Render("·································🚀"))
	b.WriteString("\n")

	for i, s := range m.sections {
		headerText := s.name
		if len(s.allRows) > 0 && s.allRows[0].num > 0 {
			if len(s.rows) == len(s.allRows) {
				headerText += fmt.Sprintf(" (%d)", len(s.rows))
			} else {
				headerText += fmt.Sprintf(" (%d/%d)", len(s.rows), len(s.allRows))
			}
		}
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
		if s.showStarred && s.name != "Services" {
			b.WriteString("  ")
			b.WriteString(helpKey.Render("[starred]"))
		}
		if s.hideTeamReviews {
			b.WriteString("  ")
			b.WriteString(helpKey.Render("[mine]"))
		}
		if s.statusFilter == "mergeable" {
			b.WriteString("  ")
			b.WriteString(helpKey.Render("[mergeable]"))
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

		// empty section indicator (PR sections only)
		if len(s.rows) == 0 && !m.loading[s.name] && s.name != "Services" && s.name != "Releases" {
			b.WriteString("  ")
			b.WriteString(helpKey.Render("- no results -"))
			b.WriteString("\n")
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
				padWidth("CI Rev", 10),
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
			maxPen := 7   // "Pending"
			for _, r := range s.rows {
				if r.depth == 0 {
					if w := lipgloss.Width(r.name); w > maxName {
						maxName = w
					}
					if w := lipgloss.Width(r.title); w > maxCur {
						maxCur = w
					}
				}
				if w := lipgloss.Width(r.pending); w > maxPen {
					maxPen = w
				}
			}
			if maxName > 30 {
				maxName = 30
			}
			if maxCur > 40 {
				maxCur = 40
			}
			if maxPen > 60 {
				maxPen = 60
			}
			if m.width > 0 {
				avail := m.width - maxName - maxCur - 4 // 2 separators + padding
				if avail < maxPen {
					maxPen = avail
				}
				if maxPen < 4 {
					maxPen = 4
				}
			}
			sep := "  "
			header := fmt.Sprintf("%s%s%s%s%s",
				padWidth("Name", maxName),
				sep,
				padWidth("Current", maxCur),
				sep,
				padWidth("Pending", maxPen))
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
				isSelected := globalIdx+i == m.cursor
				var line string
				if r.depth == 0 {
					pending := r.pending
					if !isSelected && (strings.HasPrefix(pending, "+") || pending == "-") {
						pending = helpKey.Render(pending)
					}
					line = fmt.Sprintf("%s%s%s%s%s",
						padWidth(truncateWidth(r.name, maxName), maxName),
						sep,
						padWidth(truncateWidth(r.title, maxCur), maxCur),
						sep,
						padWidth(truncateWidth(pending, maxPen), maxPen))
				} else {
					line = fmt.Sprintf("%s%s%s%s%s",
						padWidth("", maxName),
						sep,
						padWidth("", maxCur),
						sep,
						padWidth(truncateWidth(r.pending, maxPen), maxPen))
				}
				if m.width > 0 {
					line = truncateWidth(line, m.width)
				}
				if isSelected {
					b.WriteString(selectedStyle.Render(line))
				} else {
					b.WriteString(rowStyle.Render(line))
				}
				b.WriteString("\n")
			}

			remaining := len(s.rows) - visEnd
			if remaining > 0 {
				b.WriteString(overflowStyle.Render(fmt.Sprintf("↓ %d more below", remaining)))
				b.WriteString("\n")
			}

			globalIdx += len(s.rows)
		}
	}

	b.WriteString("\n")
	b.WriteString(m.viewHelp())

	content := b.String()

	if m.tagging {
		tagDlg := m.renderTagInput()
		if m.width > 0 && m.height > 0 {
			content = lipgloss.Place(m.width, m.height,
				lipgloss.Center, lipgloss.Center,
				tagDlg)
		} else {
			content = strings.Repeat("\n", 5) + tagDlg
		}
	}

	if m.showHelp {
		help := m.renderHelpOverlay()
		if m.width > 0 && m.height > 0 {
			content = lipgloss.Place(m.width, m.height,
				lipgloss.Center, lipgloss.Center,
				help)
		} else {
			content = strings.Repeat("\n", 5) + help
		}
	}

	return content
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
	aiIcon := ""
	if r.reviewed && !r.reviewStale {
		aiIcon = "✦"
	} else if r.reviewed && r.reviewStale {
		aiIcon = "✧"
	}

	icon := ciIcon(r.ci)

	rev := ""
	if r.review != "" {
		rev = reviewIcon(r.review)
	} else if r.num > 0 {
		rev = "·"
	}

	left := aiIcon
	if left != "" {
		left += " "
	}
	left += icon + "  " + rev

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
			padWidth(left, 10), padWidth(repo, repoWidth), r.num, padWidth(truncateWidth(title, titleWidth), titleWidth), padWidth(ts, ageWidth))
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
	line := helpKey.Render("ship")
	if !m.lastRefreshed.IsZero() {
		line += helpKey.Render(" (refresh in " + refreshCountdown(m.lastRefreshed, m.cfg.RefreshInterval) + ")")
	}
	line += "  " + helpKey.Render("?:") + " " + helpKey.Render("help")
	return line
}

const helpKeyWidth = 13

func helpKeyEntry(key, desc string) string {
	padded := key + strings.Repeat(" ", helpKeyWidth-len(key))
	return "  " + helpKey.Render(padded) + " " + helpKey.Render(desc) + "\n"
}

func (m Model) renderHelpOverlay() string {
	var b strings.Builder

	b.WriteString(dialogTitle.Render("Keys"))
	b.WriteString("\n\n")

	b.WriteString(helpKey.Render("nav"))
	b.WriteString("\n")
	b.WriteString(helpKeyEntry("j/k", "move up/down"))
	b.WriteString(helpKeyEntry("tab/shift+tab", "next/prev section"))
	b.WriteString(helpKeyEntry("gg/G", "top/bottom"))
	b.WriteString(helpKeyEntry("r", "refresh"))
	b.WriteString(helpKeyEntry("q", "quit"))
	b.WriteString("\n")

	if len(m.sections) > m.sectionIdx {
		s := m.sections[m.sectionIdx]
		switch s.name {
		case "My PRs", "Dependencies":
			b.WriteString(helpKey.Render("actions"))
			b.WriteString("\n")
			b.WriteString(helpKeyEntry("enter", "open in browser"))
			b.WriteString(helpKeyEntry("R", "toggle pr ready/draft"))
			b.WriteString(helpKeyEntry("M", "merge pr"))
			b.WriteString(helpKeyEntry("C", "close pr"))
			b.WriteString(helpKeyEntry("A", "ai code review"))
			b.WriteString(helpKeyEntry("B", "browse on github"))
			b.WriteString("\n")
			b.WriteString(helpKey.Render("filters"))
			b.WriteString("\n")
			b.WriteString(helpKeyEntry("m", "toggle mergeable"))
			b.WriteString(helpKeyEntry("d", "toggle drafts"))
			b.WriteString(helpKeyEntry("s", "toggle starred"))
			b.WriteString(helpKeyEntry("a", "toggle age sort"))
			b.WriteString(helpKeyEntry("/", "search"))
		case "To Review", "Team Review":
			teamLabel := "show team"
			if s.hideTeamReviews {
				teamLabel = "show mine only"
			}
			b.WriteString(helpKey.Render("actions"))
			b.WriteString("\n")
			b.WriteString(helpKeyEntry("enter", "open in browser"))
			b.WriteString(helpKeyEntry("R", "toggle pr ready/draft"))
			b.WriteString(helpKeyEntry("M", "merge pr"))
			b.WriteString(helpKeyEntry("C", "close pr"))
			b.WriteString(helpKeyEntry("A", "ai code review"))
			b.WriteString(helpKeyEntry("B", "browse on github"))
			b.WriteString("\n")
			b.WriteString(helpKey.Render("filters"))
			b.WriteString("\n")
			b.WriteString(helpKeyEntry("m", "toggle mergeable"))
			b.WriteString(helpKeyEntry("t", teamLabel))
			b.WriteString(helpKeyEntry("d", "toggle drafts"))
			b.WriteString(helpKeyEntry("s", "toggle starred"))
			b.WriteString(helpKeyEntry("a", "toggle age sort"))
			b.WriteString(helpKeyEntry("/", "search"))
		case "Services":
			b.WriteString(helpKey.Render("actions"))
			b.WriteString("\n")
			b.WriteString(helpKeyEntry("enter", "open in browser"))
			b.WriteString(helpKeyEntry("d", "open diff in browser"))
			b.WriteString(helpKeyEntry("T", "create tag/release"))
			if r := m.currentRow(); r != nil && r.repo != "" {
				for _, svc := range m.cfg.Services {
					if svc.Repo == r.repo && svc.DeployURL != "" {
						b.WriteString(helpKeyEntry("S", "open deploy"))
						break
					}
				}
			}
		}
	}

	b.WriteString("\n\n")
	b.WriteString(dialogHelp.Render("? or esc to close"))
	return dialogStyle.Render(b.String())
}

func (m Model) renderTagInput() string {
	if m.tagMeta.loading {
		return dialogStyle.Render(dialogTitle.Render("Loading") + "\n\n" + m.spin.View())
	}

	var b strings.Builder
	action := "Tag"
	if m.tagMeta.hasReleases {
		action = "Release"
	}
	b.WriteString(dialogTitle.Render("Create " + action))
	b.WriteString("\n\n")

	if m.tagMeta.latest != "" {
		b.WriteString(helpKey.Render("Latest: " + m.tagMeta.latest))
		b.WriteString("\n\n")
	} else {
		b.WriteString(helpKey.Render("(no tags yet)"))
		b.WriteString("\n\n")
	}

	b.WriteString(inputStyle.Render("Tag: "))
	b.WriteString(inputStyle.Render(m.tagQuery + "█"))
	b.WriteString("\n\n")
	if m.tagMeta.hasReleases {
		b.WriteString(dialogHelp.Render("release notes will be generated"))
		b.WriteString("\n\n")
		b.WriteString(dialogHelp.Render(strings.Repeat("─", 30)))
		b.WriteString("\n\n")
	}
	b.WriteString(dialogHelp.Render("enter: create  ctrl+o: open in browser  esc: cancel"))

	return dialogStyle.Render(b.String())
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
		HeadSHA:        p.HeadRefOid,
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

func (m Model) openBrowse() {
	if len(m.sections) == 0 {
		return
	}
	s := m.sections[m.sectionIdx]
	q := s.browseQuery(m.cfg, m.gh)
	if q == "" {
		return
	}
	u := fmt.Sprintf("https://github.com/issues?q=%s", url.QueryEscape(q))
	openBrowser(u)
}

func orJoin(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return "(" + strings.Join(parts, " OR ") + ")"
	}
}

func orGroup(qualifier string, values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = qualifier + ":" + v
	}
	return orJoin(parts)
}

func ownerOrGroup(cfg *config.Config, ghClient *gh.Client) string {
	ctx := context.Background()
	var parts []string
	for _, o := range cfg.GitHub.Owners {
		if ghClient.OwnerType(ctx, o) == "user" {
			parts = append(parts, "user:"+o)
		} else {
			parts = append(parts, "org:"+o)
		}
	}
	return orJoin(parts)
}

func (s section) browseQuery(cfg *config.Config, ghClient *gh.Client) string {
	var parts []string
	switch s.name {
	case "My PRs":
		parts = append(parts, "is:open is:pr author:@me archived:false")
		if s.showStarred {
			if g := orGroup("repo", cfg.StarredRepos()); g != "" {
				parts = append(parts, g)
			}
		} else {
			if g := ownerOrGroup(cfg, ghClient); g != "" {
				parts = append(parts, g)
			}
		}
	case "To Review":
		if s.hideTeamReviews {
			parts = append(parts, "is:open is:pr user-review-requested:@me")
		} else {
			parts = append(parts, "is:open is:pr")
			if g := orGroup("team-review-requested", cfg.GitHub.Teams); g != "" {
				parts = append(parts, g)
			}
		}
		if s.showStarred {
			if g := orGroup("repo", cfg.StarredRepos()); g != "" {
				parts = append(parts, g)
			}
		} else {
			if g := ownerOrGroup(cfg, ghClient); g != "" {
				parts = append(parts, g)
			}
		}
	case "Team Review":
		parts = append(parts, "is:open is:pr")
		if g := orGroup("team-review-requested", cfg.GitHub.Teams); g != "" {
			parts = append(parts, g)
		}
	case "Dependencies":
		parts = append(parts, "is:open is:pr")
		if s.showStarred {
			if g := orGroup("repo", cfg.StarredRepos()); g != "" {
				parts = append(parts, g)
			}
		} else if len(cfg.GitHub.Teams) > 0 {
			if g := orGroup("team-review-requested", cfg.GitHub.Teams); g != "" {
				parts = append(parts, g)
			}
		} else {
			if g := ownerOrGroup(cfg, ghClient); g != "" {
				parts = append(parts, g)
			}
		}
		if g := orGroup("author", cfg.GitHub.DepAuthors); g != "" {
			parts = append(parts, g)
		}
	default:
		return ""
	}
	if s.hideDrafts {
		parts = append(parts, "-is:draft")
	}
	if s.statusFilter == "mergeable" {
		parts = append(parts, "review:approved status:success")
	}
	return strings.Join(parts, " ")
}

func refreshCountdown(t time.Time, interval int) string {
	remaining := interval - int(time.Since(t).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	if remaining >= 60 {
		return fmt.Sprintf("%dm", (remaining+30)/60)
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
