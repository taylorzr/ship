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
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/zach/ship/internal/ai"
	"github.com/zach/ship/internal/config"
	gh "github.com/zach/ship/internal/github"
	"github.com/zach/ship/internal/k8s"
	"github.com/zach/ship/internal/notify"
	"github.com/zach/ship/internal/store"
	"github.com/zach/ship/internal/version"
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
	sectionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("86")).
			MarginTop(1)

	rowStyle = lipgloss.NewStyle()

	selectedStyle = lipgloss.NewStyle().
			Reverse(true).
			Bold(true)

	helpKey       = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	headerStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	overflowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	healthWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	healthBad     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	healthInfo    = lipgloss.NewStyle().Foreground(lipgloss.Color("75"))
	healthMuted   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	dialogStyle   = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("205")).
			Padding(1, 2).
			Width(50)
	dialogTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	dialogHelp  = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	inputStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	modeCursor  = selectedStyle
	modePad     = selectedStyle.Copy().Width(3)
)

func (m *Model) maxVisibleRows() int {
	if m.height <= 0 {
		return 15
	}
	idx := m.visibleSectionIndices()
	n := len(idx)
	if n == 0 {
		return 15
	}
	// chrome: one header per section + blank line + help footer
	chrome := n + 1 + 1
	for _, i := range idx {
		s := &m.sections[i]
		if _, ok := m.sectionErrs[s.name]; ok {
			chrome++ // section error line
		}
		if s.name == "To Review" {
			if _, ok := m.sectionErrs["Team Review"]; ok {
				chrome++ // team review error shown under To Review
			}
		}
		if len(s.rows) == 0 {
			if !m.loading[s.name] && s.name != "Services" && s.name != "Releases" {
				chrome++ // "- no results -"
			}
			continue
		}
		if s.rows[0].num > 0 {
			chrome++ // PR column header
		}
		chrome++ // "↓ N more below"
		if s.scrollOffset > 0 {
			chrome++ // "↑ N more above"
		}
	}
	perSection := (m.height - chrome) / n
	if perSection < 1 {
		return 1
	}
	if perSection > 10 {
		return 10
	}
	return perSection
}

func (m *Model) sectionViewHeight() int {
	return m.maxVisibleRows()
}

func (m *Model) ensureSectionViews() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	h := m.sectionViewHeight()
	for i := range m.sections {
		s := &m.sections[i]
		if s.view == nil {
			v := viewport.New(m.width, h)
			v.MouseWheelEnabled = true
			v.KeyMap.HalfPageDown = key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl+d", "half page down"))
			v.KeyMap.Up.Unbind()
			v.KeyMap.Down.Unbind()
			v.KeyMap.Left.Unbind()
			v.KeyMap.Right.Unbind()
			s.view = &v
		} else {
			s.view.Width = m.width
			s.view.Height = h
		}
	}
}

func (m Model) syncViewOffset(s *section) {
	if s.view != nil && s.view.YOffset != s.scrollOffset {
		s.scrollOffset = s.view.YOffset
	}
}

func (m Model) writeSectionPane(s *section, body string, total int, b *strings.Builder) {
	if s.view == nil {
		b.WriteString(body)
		if body != "" {
			b.WriteString("\n")
		}
		return
	}
	lines := strings.Count(body, "\n") + 1
	if lines > 0 && lines < s.view.Height {
		s.view.Height = lines
	}
	s.view.SetContent(body)
	if s.view.YOffset != s.scrollOffset {
		s.view.SetYOffset(s.scrollOffset)
		s.scrollOffset = s.view.YOffset
	}
	if above := s.view.YOffset; above > 0 {
		b.WriteString(overflowStyle.Render(fmt.Sprintf("    ↑ %d more above", above)))
		b.WriteString("\n")
	}
	b.WriteString(s.view.View())
	b.WriteString("\n")
	if below := total - (s.view.YOffset + m.maxVisibleRows()); below > 0 {
		b.WriteString(overflowStyle.Render(fmt.Sprintf("    ↓ %d more below", below)))
		b.WriteString("\n")
	}
}

func (m Model) renderPRRows(s *section, startGlobal int, repoWidth, maxWidth, authorWidth int) string {
	var bb strings.Builder
	for i := range s.rows {
		r := s.rows[i]
		// While the command prompt is open, don't keep a row highlighted as
		// selected in the sections behind it.
		line := renderRow(r, startGlobal+i == m.cursor && !m.cmdMode,
			m.refreshingItem.repo == r.repo && m.refreshingItem.num == r.num,
			m.spin.View(), repoWidth, maxWidth, authorWidth)
		bb.WriteString(line)
		bb.WriteString("\n")
	}
	return strings.TrimSuffix(bb.String(), "\n")
}

func (m Model) renderPaneRows(s *section, startGlobal, maxName, maxStat, maxCur, maxCon int, sep string, ev eventFilter) string {
	var bb strings.Builder
	for i := range s.rows {
		r := s.rows[i]
		isSelected := startGlobal+i == m.cursor && !m.cmdMode
		isRefreshing := m.refreshingItem.repo == r.repo && m.refreshingItem.num == r.num
		loadCol := "   "
		if isRefreshing && r.depth == 0 {
			loadCol = padWidth(m.spin.View(), 3)
		}
		var rest string
		if r.depth == 0 {
			current := r.title
			if n := pendingAhead(r.pending); n > 0 {
				current = current + " " + helpKey.Render(fmt.Sprintf("(<%d)", n))
			}
			rest = fmt.Sprintf("%s%s%s%s%s%s%s",
				padWidth(truncateWidth(r.name, maxName), maxName),
				sep,
				padWidth(truncateWidth(renderStatusColored(r.health), maxStat), maxStat),
				sep,
				padWidth(truncateWidth(current, maxCur), maxCur),
				sep,
				padWidth(truncateWidth(renderDetails(r.health, r.contributors, ev), maxCon), maxCon))
		} else {
			pending := r.pending
			if pending != "" {
				pending = healthMuted.Render(pending)
			}
			details := r.contributors
			if details != "" {
				details = healthMuted.Render(details)
			}
			rest = fmt.Sprintf("%s%s%s%s%s%s%s",
				padWidth("", maxName),
				sep,
				padWidth("", maxStat),
				sep,
				padWidth(truncateWidth(pending, maxCur), maxCur),
				sep,
				padWidth(truncateWidth(details, maxCon), maxCon))
		}
		if m.width > 4 {
			rest = truncateWidth(rest, m.width-4)
		}
		if isSelected {
			bb.WriteString(selectedStyle.Render(loadCol) + " " + rest)
		} else {
			bb.WriteString(loadCol + " " + rest)
		}
		bb.WriteString("\n")
	}
	return strings.TrimSuffix(bb.String(), "\n")
}

type row struct {
	title        string
	repo         string
	num          int
	url          string
	ci           string
	review       string
	draft        bool
	mergeable    string
	name         string // display name (releases section)
	pending      string // pending versions (releases section)
	sha          string // commit SHA
	updatedAt    string // RFC 3339 timestamp
	depth        int    // nesting level for releases section
	prodRef      string // prod tag/SHA for compare URLs
	role         string // review role ("review-direct" or "review-team")
	reviewed     bool   // AI review dispatched
	reviewStale  bool   // AI review exists but head SHA changed
	headSha      string // PR head SHA for stale check
	contributors string // version contributors (releases section)
	mergeState   string // PR merge state; "BEHIND" = needs backmerge
	health       string // service deployment health summary
	author       string // PR author (shown for To Review / Dependencies)
}

type section struct {
	name            string
	rows            []row
	allRows         []row
	scrollOffset    int
	view            *viewport.Model
	draftFilter     string // "" or "draft"
	showStarred     bool
	sortNewest      bool
	hideTeamReviews bool
	statusFilter    string // "" or "mergeable"
}

type keyMap struct {
	Up            key.Binding
	Down          key.Binding
	SectionNext   key.Binding
	SectionPrev   key.Binding
	Enter         key.Binding
	Merge         key.Binding
	Close         key.Binding
	DraftToggle   key.Binding
	RefreshItem   key.Binding
	Refresh       key.Binding
	Quit          key.Binding
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
	Notifications key.Binding
	ModeCommand   key.Binding
}

var keys = keyMap{
	Up:            key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "move up")),
	Down:          key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "move down")),
	SectionNext:   key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next section")),
	SectionPrev:   key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("⇧+tab", "prev section")),
	Enter:         key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	Merge:         key.NewBinding(key.WithKeys("M"), key.WithHelp("M", "merge")),
	Close:         key.NewBinding(key.WithKeys("C"), key.WithHelp("C", "close")),
	DraftToggle:   key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "toggle pr draft")),
	RefreshItem:   key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh item")),
	Refresh:       key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh all")),
	Quit:          key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	FilterStarred: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "toggle starred")),
	SortToggle:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "toggle age-sort")),
	Search:        key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
	Diff:          key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "filter drafts/diff")),
	Deploy:        key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "ship")),
	TeamFilter:    key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "toggle team")),
	MergeFilter:   key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "toggle mergeable")),
	AIReview:      key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "AI-review")),
	Browse:        key.NewBinding(key.WithKeys("B"), key.WithHelp("B", "browse on GitHub")),
	HelpToggle:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	TagAction:     key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "tag/release")),
	Notifications: key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "notifications")),
	ModeCommand:   key.NewBinding(key.WithKeys(">", ":"), key.WithHelp(":", "modes")),
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
	msg      string // optional close-comment message; only used for "close"
}

type actionDoneMsg struct {
	action string
	repo   string
	num    int
	err    error
}

type Model struct {
	cfg            *config.Config
	store          *store.Store
	gh             *gh.Client
	width          int
	height         int
	sections       []section
	cursor         int
	total          int
	spin           spinner.Model
	loading        map[string]bool
	sectionErrs    map[string]string
	confirm        *confirmAction
	msgCursorOn    bool
	lastRefreshed  time.Time
	sectionIdx     int
	searching      bool
	searchQuery    string
	tagging        bool
	tagQuery       string
	tagMeta        tagState
	showHelp       bool
	gPending       bool
	mockK8sSpecs   map[string]k8s.MockSpec
	notifications  []notify.Event
	notifCursor    int
	showNotif      bool
	refreshingItem struct {
		repo string
		num  int
	}
	mode    string
	cmdMode bool
	cmdBuf  string
	cmdSug  int
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

func New(cfg *config.Config, st *store.Store, ghClient *gh.Client, mockK8sSpecs map[string]k8s.MockSpec, mode string) Model {
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
		cfg:          cfg,
		store:        st,
		gh:           ghClient,
		spin:         s,
		loading:      loading,
		sectionErrs:  map[string]string{},
		mockK8sSpecs: mockK8sSpecs,
		cursor:       0,
	}
	if parsed, ok := parseMode(mode); ok {
		m.mode = parsed
	} else {
		m.mode = "all"
	}
	m.loadFromCache()
	m.reloadNotifications()
	// Reset the diff baseline so the first refresh cycle of this session only
	// seeds state instead of flooding the panel with events that piled up while
	// the TUI was closed.
	if st != nil {
		st.SaveSnapshot("")
	}
	return m
}

func (m *Model) sectionProp(name string) (offset int, draftFilter string, showStarred, sortNewest, hideTeamReviews bool, statusFilter string) {
	for _, s := range m.sections {
		if s.name == name {
			return s.scrollOffset, s.draftFilter, s.showStarred, s.sortNewest, s.hideTeamReviews, s.statusFilter
		}
	}
	return 0, "", len(m.cfg.StarredRepos()) > 0, true, false, ""
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
		if m.sections[i].draftFilter != "" || sectionQ != "" || m.sections[i].showStarred || m.sections[i].hideTeamReviews || m.sections[i].statusFilter != "" {
			var filtered []row
			allRows := m.sections[i].allRows
			if !m.sections[i].sortNewest {
				allRows = reversed(allRows)
			}
			for _, r := range allRows {
				if m.sections[i].draftFilter == "draft" && m.sections[i].name != "Services" && r.draft {
					continue
				}
				if m.sections[i].statusFilter == "mergeable" && r.mergeState != "" && !notify.Mergeable(r.mergeState) {
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

func removeRow(rows []row, repo string, num int) []row {
	for i := range rows {
		if rows[i].repo == repo && rows[i].num == num {
			return append(rows[:i], rows[i+1:]...)
		}
	}
	return rows
}

func (m *Model) loadFromCache() {
	var sections []section

	prs, _ := m.store.CachedPRs("mine")
	so, df, ss, sn, _, _ := m.sectionProp("My PRs")
	s := section{name: "My PRs", scrollOffset: so, draftFilter: df, showStarred: ss, sortNewest: sn}
	for _, p := range prs {
		r := row{
			title:      p.Title,
			repo:       p.Repo,
			num:        p.Number,
			url:        p.URL,
			ci:         p.CIState,
			review:     p.ReviewDecision,
			draft:      p.IsDraft,
			updatedAt:  p.UpdatedAt,
			mergeable:  p.Mergeable,
			mergeState: p.MergeState,
			headSha:    p.HeadSHA,
		}
		m.loadReviewState(&r)
		s.allRows = append(s.allRows, r)
		s.rows = append(s.rows, r)
	}

	versions, _ := m.store.CachedVersions()
	svcNames := make(map[string]string, len(m.cfg.Services))
	svcRepos := make(map[string]bool, len(m.cfg.Services))
	for _, svc := range m.cfg.Services {
		svcNames[svc.Repo] = svc.Name
		svcRepos[svc.Repo] = true
	}
	so, df, ss, sn, _, _ = m.sectionProp("Services")
	s3 := section{name: "Services", scrollOffset: so, draftFilter: df, showStarred: ss, sortNewest: sn}
	for _, v := range versions {
		if !svcRepos[v.Repo] {
			continue
		}
		title := k8s.ShortRef(v.ProdRef)
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
			sha:     v.ProdSHA,
			url:     serviceRowURL(v.Repo, v.ProdRef),
			depth:   0,
			health:  v.Health,
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
			contribs := map[string]struct {
				ReleaseAuthor string   `json:"release_author"`
				Contributors  []string `json:"contributors"`
			}{}
			if v.PendingContribs != "" {
				json.Unmarshal([]byte(v.PendingContribs), &contribs)
			}
			for _, entry := range strings.Split(v.PendingTags, ", ") {
				if entry == "" {
					continue
				}
				tag, title := entry, ""
				if parts := strings.SplitN(entry, "|", 2); len(parts) == 2 {
					tag, title = parts[0], parts[1]
				}
				details := m.formatContributors(contribs[tag].ReleaseAuthor, contribs[tag].Contributors)
				if title != "" {
					if details != "" {
						details = title + " · " + details
					} else {
						details = title
					}
				}
				pr := row{
					pending:      tag,
					repo:         v.Repo,
					prodRef:      v.ProdRef,
					url:          fmt.Sprintf("https://github.com/%s/releases/tag/%s", v.Repo, tag),
					depth:        1,
					contributors: details,
				}
				s3.allRows = append(s3.allRows, pr)
				s3.rows = append(s3.rows, pr)
			}
		}
		if v.UntaggedCommits != "" && v.Error == "" {
			var commits []struct {
				SHA          string   `json:"sha"`
				Message      string   `json:"message"`
				Author       string   `json:"author"`
				Contributors []string `json:"contributors"`
			}
			if err := json.Unmarshal([]byte(v.UntaggedCommits), &commits); err == nil {
				for _, c := range commits {
					details := m.formatContributors("", c.Contributors)
					if c.Message != "" {
						if details != "" {
							details = c.Message + " · " + details
						} else {
							details = c.Message
						}
					}
					pr := row{
						sha:          c.SHA,
						pending:      c.SHA[:7],
						repo:         v.Repo,
						prodRef:      v.ProdRef,
						depth:        1,
						contributors: details,
					}
					s3.allRows = append(s3.allRows, pr)
					s3.rows = append(s3.rows, pr)
				}
			}
		}
	}
	sections = append(sections, s3)
	sections = append(sections, s)

	so, df, ss, sn, htr, sf := m.sectionProp("To Review")
	dir, _ := m.store.CachedPRs("review-direct")
	team, _ := m.store.CachedPRs("review-team")
	seen := map[string]bool{}
	s2 := section{name: "To Review", scrollOffset: so, draftFilter: df, showStarred: ss, sortNewest: sn, hideTeamReviews: htr, statusFilter: sf}
	for _, p := range dir {
		key := fmt.Sprintf("%s#%d", p.Repo, p.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		r := row{
			title:      p.Title,
			repo:       p.Repo,
			num:        p.Number,
			url:        p.URL,
			ci:         p.CIState,
			review:     p.ReviewDecision,
			draft:      p.IsDraft,
			updatedAt:  p.UpdatedAt,
			mergeable:  p.Mergeable,
			mergeState: p.MergeState,
			role:       "review-direct",
			author:     p.Author,
			headSha:    p.HeadSHA,
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
			title:      p.Title,
			repo:       p.Repo,
			num:        p.Number,
			url:        p.URL,
			ci:         p.CIState,
			review:     p.ReviewDecision,
			draft:      p.IsDraft,
			updatedAt:  p.UpdatedAt,
			mergeable:  p.Mergeable,
			mergeState: p.MergeState,
			role:       "review-team",
			author:     p.Author,
			headSha:    p.HeadSHA,
		}
		m.loadReviewState(&r)
		s2.allRows = append(s2.allRows, r)
		s2.rows = append(s2.rows, r)
	}
	sections = append(sections, s2)

	deps, _ := m.store.CachedPRs("dep")
	so, df, ss, sn, _, _ = m.sectionProp("Dependencies")
	s4 := section{name: "Dependencies", scrollOffset: so, draftFilter: df, showStarred: ss, sortNewest: sn}
	for _, p := range deps {
		r := row{
			title:      p.Title,
			repo:       p.Repo,
			num:        p.Number,
			url:        p.URL,
			ci:         p.CIState,
			review:     p.ReviewDecision,
			draft:      p.IsDraft,
			updatedAt:  p.UpdatedAt,
			mergeable:  p.Mergeable,
			mergeState: p.MergeState,
			headSha:    p.HeadSHA,
			author:     p.Author,
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

// detectNotifications runs after a full refresh cycle completes: it diffs the
// freshly-refreshed store state against the last-known snapshot, emits events,
// auto-dismisses resolved ones, and stores the new snapshot. The first cycle
// only seeds the baseline so a fresh start never floods the panel.
func (m *Model) detectNotifications() {
	cur := m.currentSnapshot()
	prevBlob, err := m.store.LoadSnapshot()
	if err != nil {
		return
	}
	if prevBlob == "" {
		b, _ := json.Marshal(cur)
		m.store.SaveSnapshot(string(b))
		return
	}
	var prev notify.Snapshot
	if err := json.Unmarshal([]byte(prevBlob), &prev); err != nil {
		return
	}
	for _, e := range notify.Diff(prev, cur) {
		if !m.notifyEnabled(e.Kind) {
			continue
		}
		n := store.Notification{
			Kind:      string(e.Kind),
			Repo:      e.Repo,
			Number:    e.Number,
			Title:     e.Title,
			Message:   e.Message,
			Detail:    e.Detail,
			URL:       e.URL,
			CreatedAt: time.Now().Format(time.RFC3339),
		}
		if _, err := m.store.AddNotification(n); err != nil && shipLog != nil {
			shipLog.Printf("notify: add: %v", err)
		}
	}
	notifs, err := m.store.Notifications()
	if err == nil {
		for _, n := range notifs {
			if notify.IsResolved(notifyEvent(n), cur) {
				if err := m.store.DeleteNotification(n.ID); err != nil && shipLog != nil {
					shipLog.Printf("notify: dismiss %d: %v", n.ID, err)
				}
			}
		}
	}
	b, _ := json.Marshal(cur)
	m.store.SaveSnapshot(string(b))
	m.reloadNotifications()
}

func (m *Model) reloadNotifications() {
	notifs, err := m.store.Notifications()
	if err != nil {
		m.notifications = nil
		return
	}
	m.notifications = make([]notify.Event, len(notifs))
	for i, n := range notifs {
		m.notifications[i] = notifyEvent(n)
	}
	if m.notifCursor >= len(m.notifications) {
		m.notifCursor = 0
	}
}

func notifyEvent(n store.Notification) notify.Event {
	at, _ := time.Parse(time.RFC3339, n.CreatedAt)
	return notify.Event{
		ID: n.ID, Kind: notify.Kind(n.Kind), Repo: n.Repo, Number: n.Number,
		Title: n.Title, Message: n.Message, Detail: n.Detail, URL: n.URL,
		CreatedAt: at, Dismissed: n.Dismissed,
	}
}

func (m *Model) notifyEnabled(k notify.Kind) bool {
	c := m.cfg.Notify
	if !c.Enabled {
		return false
	}
	switch k {
	case notify.KindReviewRequested:
		return c.NewReview
	case notify.KindReviewChange:
		return c.MyReviewChange
	case notify.KindCIFailed:
		return c.MyCIFail
	case notify.KindMergeable:
		return c.MyMergeable
	case notify.KindPendingTag:
		return c.PendingTag
	case notify.KindHealth:
		return c.Health
	case notify.KindDepPR:
		return c.DepPR
	case notify.KindNewComment:
		return c.NewComment
	}
	return false
}

// currentSnapshot builds the detector snapshot from the store's freshly
// refreshed rows. PR rows are keyed by repo#number#role; activity markers are
// merged per PR (enrich only records "mine" activity from real people).
func (m *Model) currentSnapshot() notify.Snapshot {
	snap := notify.Snapshot{
		PRs:      map[string]notify.PRState{},
		Versions: map[string]notify.VersionState{},
		Activity: map[string]notify.ActivityState{},
	}
	prs, _ := m.store.CachedPRs("")
	for _, p := range prs {
		snap.PRs[notify.PRKey(p.Repo, p.Number, p.Role)] = notify.PRState{
			Role:       p.Role,
			Title:      p.Title,
			Review:     p.ReviewDecision,
			CI:         p.CIState,
			MergeState: p.MergeState,
		}
	}
	vers, _ := m.store.CachedVersions()
	for _, v := range vers {
		snap.Versions[v.Repo] = notify.VersionState{
			Problems:    healthProblems(parseHealth(v.Health)),
			PendingTags: parsePendingTagNames(v.PendingTags),
			Untagged:    countUntagged(v.UntaggedCommits),
		}
	}
	acts, _ := m.store.ActivityMarkers()
	for _, a := range acts {
		key := fmt.Sprintf("%s#%d", a.Repo, a.Number)
		snap.Activity[key] = notify.ActivityState{
			CommentAuthor: a.LastCommentAuthor,
			CommentAt:     a.LastCommentAt,
			ReviewState:   a.LastReviewState,
			ReviewAt:      a.LastReviewAt,
		}
	}
	return snap
}

// scaleDirection reports whether a deployment is mid-scale rather than
// mid-rollout: desired replicas exceed the ready count (scaling up) or trail
// the running count (scaling down). Progressing deployments and anything
// already at target are not scaling, so a rollout's ready-lag isn't misread as
// scaling.
func scaleDirection(h k8s.Health) (up, down bool) {
	if h.Progressing || h.DesiredReplicas <= 0 {
		return false, false
	}
	return h.DesiredReplicas > h.ReadyReplicas, h.DesiredReplicas < h.Replicas
}

// healthProblems mirrors the TUI's "something to be concerned about" predicate
// for a deployment: not ready and not progressing/scaling, failing conditions,
// pending or failed pods, waiting containers, or fresh warning events. A
// rollout in progress or a scale in flight is only a problem if it's genuinely
// broken (failed pods, stuck waiting containers, hard conditions) — transient
// pending pods and the "Unavailable" condition they pass through are not.
func healthProblems(h k8s.Health) bool {
	up, down := scaleDirection(h)
	if h.Progressing || up || down {
		return h.FailedPods > 0 || len(h.Waiting) > 0 || len(h.FailedReasons) > 0 || h.StuckPendingPods > 0 || hasHardCondition(h.Conditions)
	}
	return !h.Ready || len(h.Conditions) > 0 || h.PendingPods > 0 ||
		h.FailedPods > 0 || len(h.Waiting) > 0 || len(h.RecentEvents) > 0 || len(h.Events) > 0
}

// hasHardCondition reports whether any deployment condition is a genuine
// failure. "Unavailable" is excluded: it's the transient state any rollout
// passes through while re-establishing availability and is already conveyed
// by the ⟳/✖ headline and the ready-replica count.
func hasHardCondition(conds []string) bool {
	for _, c := range conds {
		if c != "Unavailable" {
			return true
		}
	}
	return false
}

// parsePendingTagNames extracts the tag names from the cached "tag|title, tag|title" list.
func parsePendingTagNames(pending string) []string {
	if pending == "" {
		return nil
	}
	var tags []string
	for _, entry := range strings.Split(pending, ", ") {
		if entry == "" {
			continue
		}
		if parts := strings.SplitN(entry, "|", 2); len(parts) == 2 {
			tags = append(tags, parts[0])
		} else {
			tags = append(tags, entry)
		}
	}
	return tags
}

// pendingAhead returns the pending tagged-release count for a depth-0 service
// row, parsed from its "+N" pending value (see buildServicesRows, which sets
// pending to "+N" or "-"). Returns 0 when nothing is behind.
func pendingAhead(pending string) int {
	if !strings.HasPrefix(pending, "+") {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(pending, "+"))
	if err != nil {
		return 0
	}
	return n
}

// countUntagged returns the number of untagged commits ahead of prod from the
// cached JSON array.
func countUntagged(untagged string) int {
	if untagged == "" {
		return 0
	}
	var commits []struct{ SHA string }
	if err := json.Unmarshal([]byte(untagged), &commits); err != nil {
		return 0
	}
	return len(commits)
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

// modeSection maps a mode to the only section it makes visible; the empty
// string ("all") keeps every section visible.
var modeSection = map[string]string{
	"services": "Services",
	"mine":     "My PRs",
	"review":   "To Review",
	"deps":     "Dependencies",
}

// modeTokens lists every autocompletable token for the : prompt (modes plus
// the quit/exit commands).
var modeTokens = []string{"all", "services", "svc", "mine", "prs", "myprs", "review", "toreview", "deps", "dependencies", "quit", "exit"}

// modeMatches returns the command tokens the current buffer is a prefix of,
// in the order Tab cycles through them.
func (m Model) modeMatches() []string {
	prefix := strings.ToLower(m.cmdBuf)
	if prefix == "" {
		return nil
	}
	var matches []string
	for _, tok := range modeTokens {
		if strings.HasPrefix(tok, prefix) {
			matches = append(matches, tok)
		}
	}
	return matches
}

// parseMode normalizes a command token into a canonical mode. An unknown token
// returns ("all", false) so callers can ignore the input instead of switching.
func parseMode(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "all":
		return "all", true
	case "services", "svc":
		return "services", true
	case "mine", "prs", "myprs":
		return "mine", true
	case "review", "toreview":
		return "review", true
	case "deps", "dependencies":
		return "deps", true
	}
	return "all", false
}

// visibleSectionIndices returns the indices of the sections rendered under the
// current mode, preserving their original order.
func (m Model) visibleSectionIndices() []int {
	only := modeSection[m.mode]
	if only == "" {
		indices := make([]int, len(m.sections))
		for i := range m.sections {
			indices[i] = i
		}
		return indices
	}
	var indices []int
	for i := range m.sections {
		if m.sections[i].name == only {
			indices = append(indices, i)
		}
	}
	return indices
}

// enterMode switches the active mode, snapping the cursor and active section
// onto a visible section when the current one no longer renders.
func (m *Model) enterMode(mode string) {
	if mode == m.mode {
		return
	}
	m.mode = mode
	m.ensureSectionViews()
	vis := m.visibleSectionIndices()
	if len(vis) == 0 {
		m.cursor = -1
		return
	}
	visible := false
	for _, idx := range vis {
		if idx == m.sectionIdx {
			visible = true
			break
		}
	}
	if !visible {
		m.sectionIdx = vis[0]
	}
	if visibleRows(m.sections[m.sectionIdx]) > 0 {
		m.cursor = m.sectionOffset(m.sectionIdx) + m.sections[m.sectionIdx].scrollOffset
	} else {
		m.cursor = -1
	}
}

func (m *Model) advanceSection(dir int) {
	vis := m.visibleSectionIndices()
	if len(vis) == 0 {
		m.cursor = -1
		return
	}
	pos := 0
	for i, idx := range vis {
		if idx == m.sectionIdx {
			pos = i
			break
		}
	}
	pos = (pos + dir) % len(vis)
	if pos < 0 {
		pos = len(vis) - 1
	}
	m.sectionIdx = vis[pos]
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
	cmds := []tea.Cmd{m.spin.Tick, m.autoRefreshTick(), m.activeRefreshTick(), m.rateLimitTick()}
	if cmd := m.rateLimitRefreshCmd(); cmd != nil {
		cmds = append(cmds, cmd)
	}
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
			m.enrichPRDetails(ctx, "mine", prs)
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
			m.enrichPRDetails(ctx, "review-direct", revs)
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
			m.enrichPRDetails(ctx, "review-team", prs)
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
			m.enrichPRDetails(ctx, "dep", deps)
		}
		return refreshDoneMsg{source: "Dependencies", err: err}
	}
}

// enrichPRDetails fills in the PR detail columns (CI, review decision,
// mergeability) for a just-refreshed section. REST search returns basic issue
// metadata only, so PRs would otherwise show blank ci:/review:/merge: until a
// manual `r` refresh. It runs on every section refresh — initial load, Shift-R,
// and the auto-refresh — so the details track the list.
func (m Model) enrichPRDetails(ctx context.Context, role string, prs []gh.PR) {
	if len(prs) == 0 {
		return
	}
	// Resolve the signed-in login once (gh caches it) so every goroutine below
	// can compare against it without fanning out GraphQL queries.
	self := ""
	if role == "mine" {
		selfCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if u, err := m.gh.User(selfCtx); err == nil {
			self = u
		}
	}
	var wg sync.WaitGroup
	for _, p := range prs {
		wg.Add(1)
		go func(p gh.PR) {
			defer wg.Done()
			prCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			pr, err := m.gh.GetPR(prCtx, p.Repo, p.Number)
			if err != nil {
				if shipLog != nil {
					shipLog.Printf("  enrich %s#%d: %v", p.Repo, p.Number, err)
				}
				return
			}
			if err := m.store.SavePR(toCached(*pr, role)); err != nil && shipLog != nil {
				shipLog.Printf("  enrich save %s#%d: %v", p.Repo, p.Number, err)
			}
			// Record comment/review activity for the notification detector.
			// Only track "mine" PRs, and only other people's activity (never
			// our own comments, and never bot noise), so a marker change
			// always means a new comment worth notifying about.
			if role == "mine" && !m.isIgnoredActivity(pr, self) {
				act := store.Activity{
					Repo:   pr.Repo,
					Number: pr.Number,
					Role:   role,
				}
				if !selfAuthor(self, pr.LatestCommentAuthor) && pr.LatestCommentAuthor != "" {
					act.LastCommentAuthor = pr.LatestCommentAuthor
					act.LastCommentAt = pr.LatestCommentAt
				}
				if !selfAuthor(self, pr.LatestReviewAuthor) && pr.LatestReviewAuthor != "" {
					act.LastReviewState = pr.LatestReviewState
					act.LastReviewAt = pr.LatestReviewAt
				}
				if err := m.store.SaveActivity(act); err != nil && shipLog != nil {
					shipLog.Printf("  enrich activity %s#%d: %v", pr.Repo, pr.Number, err)
				}
			}
		}(p)
	}
	wg.Wait()
}

// selfAuthor reports whether the activity author is the signed-in user.
func selfAuthor(self, login string) bool {
	return login != "" && strings.EqualFold(login, self)
}

// isIgnoredActivity reports whether a PR's latest comment/review came from a
// bot in IgnoreContributors. When it did, the activity marker is left blank so
// the detector only ever sees real people.
func (m Model) isIgnoredActivity(pr *gh.PR, self string) bool {
	for _, ig := range m.cfg.GitHub.IgnoreContributors {
		if strings.EqualFold(pr.LatestCommentAuthor, ig) || strings.EqualFold(pr.LatestReviewAuthor, ig) {
			return true
		}
	}
	if self != "" {
		if strings.EqualFold(pr.LatestCommentAuthor, self) || strings.EqualFold(pr.LatestReviewAuthor, self) {
			return true
		}
	}
	return false
}

func (m Model) refreshServiceCmd(ctx context.Context, repo string) tea.Cmd {
	return func() tea.Msg {
		var svc *config.ServiceConfig
		for i := range m.cfg.Services {
			if m.cfg.Services[i].Repo == repo {
				svc = &m.cfg.Services[i]
				break
			}
		}
		if svc == nil {
			return refreshDoneMsg{source: "Services", err: fmt.Errorf("service not found: %s", repo)}
		}
		svcCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cv, err := m.resolveServiceVersion(svcCtx, *svc)
		if err != nil {
			return refreshDoneMsg{source: "Services", err: err}
		}
		if shipLog != nil {
			shipLog.Printf("Services: re-resolved %s", svc.Repo)
		}
		m.store.SaveVersion(cv)
		return refreshDoneMsg{source: "Services", err: nil}
	}
}

// resolveServiceVersion resolves a single service against k8s and returns the
// resulting cached version (unpersisted) along with any setup error.
func (m Model) resolveServiceVersion(svcCtx context.Context, svc config.ServiceConfig) (store.CachedVersion, error) {
	var rc k8s.Client
	if len(m.mockK8sSpecs) > 0 {
		spec, ok := m.mockK8sSpecs[svc.Name]
		if !ok {
			spec, ok = m.mockK8sSpecs[svc.Repo]
		}
		if !ok {
			spec, ok = m.mockK8sSpecs["*"]
		}
		if !ok {
			return store.CachedVersion{}, fmt.Errorf("mock: no spec for service %q", svc.Name)
		}
		rc = k8s.NewMock(map[string]k8s.MockSpec{"*": spec})
	} else {
		var err error
		rc, err = k8s.NewRealClient(svcCtx, "", svc.Context, m.cfg.K8s.LoginCommand, m.cfg.K8s.LoginCooldown, m.k8sTimebox())
		if err != nil {
			return store.CachedVersion{}, err
		}
	}
	v := version.Resolve(svcCtx, rc, m.gh, svc)
	pending, contribs := serializePendingTags(v.PendingTags)
	untagged := serializeUntaggedCommits(v.UntaggedCommits)
	return store.CachedVersion{
		Repo:            v.Service.Repo,
		ProdRef:         v.ProdRef,
		ProdSHA:         v.ProdSHA,
		AheadBy:         v.AheadBy,
		PendingTags:     pending,
		PendingContribs: contribs,
		UntaggedCommits: untagged,
		Health:          serializeHealth(v.Health),
		Error:           v.Error,
	}, nil
}

func (m Model) servicesForRepo(repo string) []config.ServiceConfig {
	var svcs []config.ServiceConfig
	for _, svc := range m.cfg.Services {
		if svc.Repo == repo {
			svcs = append(svcs, svc)
		}
	}
	return svcs
}

// refreshServicesForRepoCmd re-resolves every configured service deployed from
// repo and reports back via a single refreshDoneMsg once all are done.
func (m Model) refreshServicesForRepoCmd(ctx context.Context, repo string) tea.Cmd {
	return func() tea.Msg {
		svcs := m.servicesForRepo(repo)
		if len(svcs) == 0 {
			return refreshDoneMsg{source: "Services", err: nil}
		}
		var wg sync.WaitGroup
		results := make(chan store.CachedVersion, len(svcs))
		for _, svc := range svcs {
			wg.Add(1)
			go func(svc config.ServiceConfig) {
				defer wg.Done()
				svcCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				cv, err := m.resolveServiceVersion(svcCtx, svc)
				if err != nil {
					cv = store.CachedVersion{Repo: svc.Repo, Error: err.Error()}
				}
				if shipLog != nil {
					if cv.Error != "" {
						shipLog.Printf("Services: %s: %s", svc.Repo, cv.Error)
					} else {
						shipLog.Printf("Services: re-resolved %s", svc.Repo)
					}
				}
				results <- cv
			}(svc)
		}
		wg.Wait()
		close(results)

		var lastErr error
		for res := range results {
			m.store.SaveVersion(res)
			if res.Error != "" {
				lastErr = fmt.Errorf("%s: %s", res.Repo, res.Error)
			}
		}
		return refreshDoneMsg{source: "Services", err: lastErr}
	}
}

func (m Model) refreshItemCmd(ctx context.Context) tea.Cmd {
	s := m.sections[m.sectionIdx]
	r := m.currentRow()
	return func() tea.Msg {
		if r == nil || r.repo == "" {
			return refreshDoneMsg{source: s.name, err: fmt.Errorf("no item selected")}
		}
		if s.name == "Services" {
			var svc *config.ServiceConfig
			for i := range m.cfg.Services {
				if m.cfg.Services[i].Repo == r.repo {
					svc = &m.cfg.Services[i]
					break
				}
			}
			if svc == nil {
				return refreshDoneMsg{source: s.name, err: fmt.Errorf("service not found: %s", r.repo)}
			}
			svcCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			var rc k8s.Client
			if len(m.mockK8sSpecs) > 0 {
				spec, ok := m.mockK8sSpecs[svc.Name]
				if !ok {
					spec, ok = m.mockK8sSpecs[svc.Repo]
				}
				if !ok {
					spec, ok = m.mockK8sSpecs["*"]
				}
				if !ok {
					return refreshDoneMsg{source: s.name, err: fmt.Errorf("mock: no spec for service %q", svc.Name)}
				}
				rc = k8s.NewMock(map[string]k8s.MockSpec{"*": spec})
			} else {
				var err error
				rc, err = k8s.NewRealClient(svcCtx, "", svc.Context, m.cfg.K8s.LoginCommand, m.cfg.K8s.LoginCooldown, m.k8sTimebox())
				if err != nil {
					return refreshDoneMsg{source: s.name, err: err}
				}
			}
			v := version.Resolve(svcCtx, rc, m.gh, *svc)
			pending, contribs := serializePendingTags(v.PendingTags)
			untagged := serializeUntaggedCommits(v.UntaggedCommits)
			if shipLog != nil {
				shipLog.Printf("Services: re-resolved %s", svc.Repo)
			}
			m.store.SaveVersion(store.CachedVersion{
				Repo:            v.Service.Repo,
				ProdRef:         v.ProdRef,
				ProdSHA:         v.ProdSHA,
				AheadBy:         v.AheadBy,
				PendingTags:     pending,
				PendingContribs: contribs,
				UntaggedCommits: untagged,
				Health:          serializeHealth(v.Health),
				Error:           v.Error,
			})
			return refreshDoneMsg{source: s.name, err: nil}
		}
		pr, err := m.gh.GetPR(ctx, r.repo, r.num)
		if err != nil {
			return refreshDoneMsg{source: s.name, err: err}
		}
		if shipLog != nil {
			shipLog.Printf("refreshed %s#%d", r.repo, r.num)
		}
		role := r.role
		if role == "" {
			role = s.nameToRole()
		}
		if err := m.store.SavePR(toCached(*pr, role)); err != nil {
			return refreshDoneMsg{source: s.name, err: err}
		}
		return refreshDoneMsg{source: s.name, err: nil}
	}
}

// section.nameToRole maps a section name to the store role for that section.
func (s section) nameToRole() string {
	switch s.name {
	case "My PRs":
		return "mine"
	case "To Review", "Team Review":
		return "review-direct"
	case "Dependencies":
		return "dep"
	}
	return ""
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
			PendingContribs string
			UntaggedCommits string
			Health          string
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
				if len(m.mockK8sSpecs) > 0 {
					spec, ok := m.mockK8sSpecs[svc.Name]
					if !ok {
						spec, ok = m.mockK8sSpecs[svc.Repo]
					}
					if !ok {
						spec, ok = m.mockK8sSpecs["*"]
					}
					if !ok {
						results <- svcResult{Repo: svc.Repo, Error: fmt.Sprintf("mock: no spec for service %q (key by name or repo)", svc.Name)}
						return
					}
					rc = k8s.NewMock(map[string]k8s.MockSpec{"*": spec})
				} else {
					var err error
					rc, err = k8s.NewRealClient(svcCtx, "", svc.Context, m.cfg.K8s.LoginCommand, m.cfg.K8s.LoginCooldown, m.k8sTimebox())
					if err != nil {
						results <- svcResult{Repo: svc.Repo, Error: fmt.Sprintf("k8s: %v", err)}
						return
					}
				}
				r := version.Resolve(svcCtx, rc, m.gh, svc)
				pending, contribs := serializePendingTags(r.PendingTags)
				untagged := serializeUntaggedCommits(r.UntaggedCommits)
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
					PendingContribs: contribs,
					UntaggedCommits: untagged,
					Health:          serializeHealth(r.Health),
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
				PendingContribs: res.PendingContribs,
				UntaggedCommits: res.UntaggedCommits,
				Health:          res.Health,
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

func (m Model) activeRefreshTick() tea.Cmd {
	interval := m.cfg.ActiveRefreshInterval
	if interval <= 0 {
		return nil
	}
	return tea.Tick(time.Duration(interval)*time.Second, func(t time.Time) tea.Msg {
		return activeRefreshMsg(t)
	})
}

type activeRefreshMsg time.Time

// rateLimitRefreshInterval is how often the GitHub rate-limit footer is
// refreshed independently of the main refresh cycle. GET /rate_limit is free.
const rateLimitRefreshInterval = 15 * time.Second

// rateLimitTick fires rateLimitTickMsg every rateLimitRefreshInterval so the
// quota footer stays fresh even between full refreshes. No-op without a GitHub
// client.
func (m Model) rateLimitTick() tea.Cmd {
	if m.gh == nil {
		return nil
	}
	return tea.Tick(rateLimitRefreshInterval, func(t time.Time) tea.Msg {
		return rateLimitTickMsg(t)
	})
}

type rateLimitTickMsg time.Time

// rateLimitRefreshCmd polls GitHub's /rate_limit in the background. Failures
// (e.g. offline) are logged and swallowed so the UI keeps the last known
// quota. Returns nil without a GitHub client.
func (m Model) rateLimitRefreshCmd() tea.Cmd {
	if m.gh == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := m.gh.RefreshRateLimits(ctx)
		if err != nil && shipLog != nil {
			shipLog.Printf("rate limit: %v", err)
		}
		return rateLimitDoneMsg{}
	}
}

type rateLimitDoneMsg struct{}

type cursorBlinkMsg struct{}

func cursorBlinkCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return cursorBlinkMsg{} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ensureSectionViews()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case cursorBlinkMsg:
		m.msgCursorOn = !m.msgCursorOn
		if m.confirm != nil && m.confirm.action == "close" {
			return m, cursorBlinkCmd()
		}
		return m, nil

	case tea.KeyMsg:
		if m.confirm != nil {
			m.gPending = false
			switch {
			case key.Matches(msg, keys.Quit):
				return m, tea.Quit
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
				m.confirm = nil
			case key.Matches(msg, keys.Enter):
				if m.confirm.action == "merge-draft-error" || m.confirm.action == "merge-conflict-error" || m.confirm.action == "deploy-error" {
					m.confirm = nil
					break
				}
				action := m.confirm.action
				repo := m.confirm.repo
				num := m.confirm.num
				draft := m.confirm.draft
				msg := m.confirm.msg
				m.confirm = nil
				return m, m.execAction(action, repo, num, draft, msg)
			case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
				if m.confirm.action == "close" && len(m.confirm.msg) > 0 {
					m.confirm.msg = m.confirm.msg[:len(m.confirm.msg)-1]
					m.msgCursorOn = true
				}
			default:
				if m.confirm.action == "close" {
					r := []rune(msg.String())
					if len(r) == 1 && r[0] >= 32 && r[0] <= 126 {
						m.confirm.msg += string(r[0])
						m.msgCursorOn = true
					}
				}
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
					if shipLog != nil {
						shipLog.Printf("tagging: tagMeta.sha=%q branch=%q", m.tagMeta.sha, m.tagMeta.branch)
					}
					return m, m.createTagRelease(m.tagMeta.repo, tag, m.tagMeta.sha, m.tagMeta.branch)
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+o"))):
				tag := strings.TrimSpace(m.tagQuery)
				m.tagging = false
				m.tagQuery = ""
				if tag != "" {
					url := fmt.Sprintf("https://github.com/%s/releases/new?tag=%s&target=%s", m.tagMeta.repo, url.QueryEscape(tag), m.tagMeta.sha)
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
		if m.cmdMode {
			m.gPending = false
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
				m.cmdMode = false
				m.cmdBuf = ""
				m.cmdSug = 0
			case key.Matches(msg, keys.Enter):
				raw := strings.ToLower(strings.TrimSpace(m.cmdBuf))
				if raw == "quit" || raw == "exit" || raw == "q" {
					return m, tea.Quit
				}
				mode, ok := parseMode(raw)
				if !ok {
					if matches := m.modeMatches(); len(matches) > 0 {
						target := matches[m.cmdSug%len(matches)]
						if target == "quit" || target == "exit" {
							return m, tea.Quit
						}
						mode, ok = parseMode(target)
					}
				}
				m.cmdMode = false
				m.cmdBuf = ""
				m.cmdSug = 0
				if ok {
					m.enterMode(mode)
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
				if matches := m.modeMatches(); len(matches) > 0 {
					m.cmdSug = (m.cmdSug + 1) % len(matches)
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
				if len(m.cmdBuf) > 0 {
					m.cmdBuf = m.cmdBuf[:len(m.cmdBuf)-1]
					m.cmdSug = 0
				}
			default:
				r := []rune(msg.String())
				if len(r) == 1 && r[0] >= 32 && r[0] <= 126 {
					m.cmdBuf += string(r[0])
					m.cmdSug = 0
				}
			}
			return m, nil
		}
		if m.showNotif {
			m.gPending = false
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "n", "q"))):
				m.showNotif = false
			case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
				if m.notifCursor > 0 {
					m.notifCursor--
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
				if m.notifCursor < len(m.notifications)-1 {
					m.notifCursor++
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("enter", "o"))):
				if len(m.notifications) > 0 {
					if url := m.notifications[m.notifCursor].URL; url != "" {
						openBrowser(url)
					}
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("x"))):
				if len(m.notifications) > 0 {
					id := m.notifications[m.notifCursor].ID
					if err := m.store.DeleteNotification(id); err != nil && shipLog != nil {
						shipLog.Printf("notify: clear %d: %v", id, err)
					}
					m.reloadNotifications()
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("c"))):
				if err := m.store.ClearNotifications(); err != nil && shipLog != nil {
					shipLog.Printf("notify: clear all: %v", err)
				}
				m.reloadNotifications()
			}
			return m, nil
		}
		m.ensureSectionViews()
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
			if r != nil && m.sections[m.sectionIdx].name == "Services" && r.depth == 0 && r.repo != "" {
				for _, svc := range m.cfg.Services {
					if svc.Repo == r.repo {
						m.openK9s(svc)
						break
					}
				}
			} else if r != nil && r.url != "" {
				openBrowser(r.url)
			} else if r != nil && m.sections[m.sectionIdx].name == "Services" && r.depth > 0 && r.sha != "" && r.repo != "" {
				openBrowser(fmt.Sprintf("https://github.com/%s/commit/%s", r.repo, r.sha))
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
						if s.draftFilter == "draft" {
							s.draftFilter = ""
						} else {
							s.draftFilter = "draft"
						}
						m.sections[m.sectionIdx] = s
						m.applyFilters()
					}
				}
				return m, nil
			}
		case key.Matches(msg, keys.RefreshItem):
			if r := m.currentRow(); r != nil && r.repo != "" {
				m.refreshingItem = struct {
					repo string
					num  int
				}{repo: r.repo, num: r.num}
				return m, m.refreshItemCmd(context.Background())
			}
			return m, nil
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
		case key.Matches(msg, keys.DraftToggle):
			if m.sections[m.sectionIdx].name == "My PRs" || m.sections[m.sectionIdx].name == "Dependencies" {
				r := m.currentRow()
				if r != nil && r.num > 0 {
					m.confirm = &confirmAction{action: "draft-toggle", repo: r.repo, num: r.num, title: r.title, draft: r.draft}
				}
			}
		case key.Matches(msg, keys.Deploy):
			if m.sections[m.sectionIdx].name == "Services" {
				r := m.currentRow()
				if r != nil && r.repo != "" {
					found := false
					for _, svc := range m.cfg.Services {
						if svc.Repo == r.repo && svc.DeployURL != "" {
							openBrowser(svc.DeployURL)
							found = true
							break
						}
					}
					if !found {
						m.confirm = &confirmAction{action: "deploy-error", title: r.title, repo: r.repo}
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
						if r.mergeState == "UNSTABLE" {
							warnings = append(warnings, "CI: failing (not required)")
						} else {
							warnings = append(warnings, "CI: failing")
						}
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
					m.msgCursorOn = true
					return m, cursorBlinkCmd()
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
					if shipLog != nil {
						shipLog.Printf("T pressed: r.url=%q r.sha=%q r.depth=%d", r.url, r.sha, r.depth)
					}
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
						if shipLog != nil {
							shipLog.Printf("T: set tagMeta.sha=%q (from r.sha=%q) branch=%q", m.tagMeta.sha, r.sha, branch)
						}
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
		case key.Matches(msg, keys.Notifications):
			m.showNotif = !m.showNotif
			return m, nil
		case key.Matches(msg, keys.ModeCommand):
			m.cmdMode = true
			m.cmdBuf = ""
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
		case m.sectionIdx < len(m.sections) && m.sections[m.sectionIdx].view != nil &&
			(key.Matches(msg, m.sections[m.sectionIdx].view.KeyMap.PageDown) ||
				key.Matches(msg, m.sections[m.sectionIdx].view.KeyMap.PageUp) ||
				key.Matches(msg, m.sections[m.sectionIdx].view.KeyMap.HalfPageUp) ||
				key.Matches(msg, m.sections[m.sectionIdx].view.KeyMap.HalfPageDown)):
			s := &m.sections[m.sectionIdx]
			updated, _ := s.view.Update(msg)
			*s.view = updated
			m.syncViewOffset(s)
		default:
			m.gPending = false
		}

	case refreshDoneMsg:
		m.loading[msg.source] = false
		m.refreshingItem = struct {
			repo string
			num  int
		}{}
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
			m.detectNotifications()
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

	case activeRefreshMsg:
		if m.cfg.ActiveRefreshInterval > 0 && m.allDone() && m.refreshingItem.repo == "" &&
			m.sections[m.sectionIdx].name == "Services" {
			if r := m.currentRow(); r != nil && r.repo != "" && r.health != "" && parseHealth(r.health).Progressing {
				m.refreshingItem = struct {
					repo string
					num  int
				}{repo: r.repo, num: r.num}
				return m, tea.Batch(m.refreshServiceCmd(context.Background(), r.repo), m.activeRefreshTick())
			}
		}
		return m, m.activeRefreshTick()

	case rateLimitTickMsg:
		return m, tea.Batch(m.rateLimitRefreshCmd(), m.rateLimitTick())

	case rateLimitDoneMsg:
		return m, nil

	case actionDoneMsg:
		m.sectionErrs = map[string]string{}
		if msg.err != nil {
			if shipLog != nil {
				shipLog.Printf("action: %v", msg.err)
			}
			if len(m.sections) > m.sectionIdx {
				m.sectionErrs[m.sections[m.sectionIdx].name] = msg.err.Error()
			}
			return m, nil
		}
		switch msg.action {
		case "merge", "close":
			// PR is no longer open — remove from store and in-memory rows.
			if msg.repo != "" && msg.num > 0 {
				_ = m.store.DeletePR(msg.repo, msg.num)
				for i := range m.sections {
					m.sections[i].allRows = removeRow(m.sections[i].allRows, msg.repo, msg.num)
					m.sections[i].rows = removeRow(m.sections[i].rows, msg.repo, msg.num)
				}
				m.recalcTotal()
				if m.cursor >= m.total {
					m.cursor = m.total - 1
				}
				m.loadFromCache()
			}
			// A merged PR advances the repo, so re-resolve any services that
			// deploy from it and the Services section reflects the new state.
			if msg.action == "merge" && len(m.servicesForRepo(msg.repo)) > 0 {
				m.loading["Services"] = true
				return m, m.refreshServicesForRepoCmd(context.Background(), msg.repo)
			}
			return m, nil
		case "draft-toggle":
			if msg.repo != "" && msg.num > 0 {
				m.refreshingItem = struct {
					repo string
					num  int
				}{repo: msg.repo, num: msg.num}
				return m, m.refreshItemCmd(context.Background())
			}
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

	case tagCreatedMsg:
		m.refreshingItem = struct {
			repo string
			num  int
		}{repo: msg.repo, num: 0}
		return m, m.refreshReleases(context.Background())
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

type tagCreatedMsg struct {
	repo string
}

func (m *Model) createTagRelease(repo, tag, sha, branch string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		hasReleases, err := m.gh.RepoHasReleases(ctx, repo)
		if err != nil {
			return errorMsg(fmt.Sprintf("create tag/release: %v", err))
		}
		if hasReleases {
			err = m.gh.CreateRelease(ctx, repo, tag, sha)
		} else {
			err = m.gh.CreateTag(ctx, repo, tag, sha)
		}
		if err != nil {
			return errorMsg(err.Error())
		}
		return tagCreatedMsg{repo: repo}
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

	m.ensureSectionViews()

	visibleSet := make(map[int]bool, len(m.sections))
	for _, idx := range m.visibleSectionIndices() {
		visibleSet[idx] = true
	}

	firstRendered := true
	for i := range m.sections {
		s := &m.sections[i]
		if !visibleSet[i] {
			// Hidden sections still occupy global row space so cursor indices
			// keep lining up with the sections that do render.
			globalIdx += len(s.rows)
			continue
		}
		isFirst := firstRendered
		firstRendered = false
		if m.cmdMode && isFirst {
			// Reuse the section header's top margin for the command prompt so
			// it doesn't add an extra line.
			b.WriteString(m.renderModePrompt())
			b.WriteString("\n")
		}
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
			sty := sectionStyle
			if isFirst {
				sty = sectionStyle.UnsetMarginTop()
			}
			b.WriteString(sty.Copy().Foreground(lipgloss.Color("212")).Render(headerText))
		} else {
			sty := sectionStyle
			if isFirst {
				sty = sectionStyle.UnsetMarginTop()
			}
			b.WriteString(sty.Render(headerText))
		}
		if s.draftFilter == "draft" && s.name != "Services" {
			b.WriteString("  ")
			b.WriteString(helpKey.Render("[no draft]"))
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
			b.WriteString("    ")
			b.WriteString(helpKey.Render("- no results -"))
			b.WriteString("\n")
		}

		// column header + scrollable row pane
		if len(s.rows) > 0 {
			if s.rows[0].num > 0 {
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
				titleWidth := m.width - repoWidth - 33
				authorWidth := 0
				if s.name == "To Review" || s.name == "Dependencies" {
					authorWidth = 16
					titleWidth -= authorWidth + 2
				}
				if titleWidth < 1 {
					titleWidth = 1
				}
				header := fmt.Sprintf("%s%s%s  %s  %s",
					"    CI Re ",
					padWidth("Up", 5),
					padWidth("Repo", repoWidth),
					padWidth("#", 6),
					padWidth("Title", titleWidth))
				if authorWidth > 0 {
					header += "  " + padWidth("Author", authorWidth)
				}
				header += "  " + padWidth("Age", ageWidth)
				b.WriteString(headerStyle.Render(header))
				b.WriteString("\n")

				body := m.renderPRRows(s, globalIdx, repoWidth, m.width, authorWidth)
				m.writeSectionPane(s, body, len(s.rows), &b)
			} else {
				// compute column widths
				maxName := 4 // "Name"
				maxStat := 6 // "Status"
				maxCur := 7  // "Current"
				maxCon := 11 // "Details"
				ev := m.eventFilter()
				for _, r := range s.rows {
					if r.depth == 0 {
						if w := lipgloss.Width(r.name); w > maxName {
							maxName = w
						}
						if w := lipgloss.Width(statusText(r.health)); w > maxStat {
							maxStat = w
						}
						current := r.title
						if n := pendingAhead(r.pending); n > 0 {
							current += fmt.Sprintf(" (<%d)", n)
						}
						if w := lipgloss.Width(current); w > maxCur {
							maxCur = w
						}
					}
					if w := lipgloss.Width(detailsText(r.health, r.contributors, ev)); w > maxCon {
						maxCon = w
					}
				}
				if maxName > 30 {
					maxName = 30
				}
				if maxCur > 40 {
					maxCur = 40
				}
				// Details keeps every column the other fields don't need so
				// long health summaries (event history, probe failures) stay
				// visible instead of being cut at a fixed cap.
				if m.width > 0 {
					avail := m.width - maxName - maxStat - maxCur - 8 // row prefix + separators + margin
					if avail < 11 {
						avail = 11
					}
					if maxCon > avail {
						maxCon = avail
					}
				}
				sep := "  "
				header := fmt.Sprintf("%s%s%s%s%s%s%s%s",
					"    ",
					padWidth("Name", maxName),
					sep,
					padWidth("Status", maxStat),
					sep,
					padWidth("Current", maxCur),
					sep,
					padWidth("Details", maxCon))
				b.WriteString(headerStyle.Render(header))
				b.WriteString("\n")

				body := m.renderPaneRows(s, globalIdx, maxName, maxStat, maxCur, maxCon, sep, ev)
				m.writeSectionPane(s, body, len(s.rows), &b)
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

	if m.showNotif {
		panel := m.renderNotificationsPanel()
		if m.width > 0 && m.height > 0 {
			content = lipgloss.Place(m.width, m.height,
				lipgloss.Center, lipgloss.Center,
				panel)
		} else {
			content = strings.Repeat("\n", 5) + panel
		}
	}

	if m.width > 0 && m.height > 0 && m.confirm == nil && !m.tagging && !m.showHelp && !m.showNotif {
		content = lipgloss.NewStyle().MaxHeight(m.height).Render(content)
	}

	return content
}

// serializePendingTags turns the pending tag list into the on-disk "tag|title,
// tag|title" string plus a JSON map of tag → {release_author, contributors}
// used to render the contributors column from cache.
func serializePendingTags(tags []gh.PendingTag) (pending string, contribs string) {
	if len(tags) == 0 {
		return "", ""
	}
	cmap := make(map[string]map[string]any, len(tags))
	for i, t := range tags {
		if i > 0 {
			pending += ", "
		}
		if t.Title != "" && t.Title != t.Name {
			pending += t.Name + "|" + t.Title
		} else {
			pending += t.Name
		}
		cmap[t.Name] = map[string]any{"release_author": t.ReleaseAuthor, "contributors": t.Contributors}
	}
	contribs = ""
	if b, err := json.Marshal(cmap); err == nil {
		contribs = string(b)
	}
	return pending, contribs
}

// serializeHealth turns service health into a compact on-disk string so the
// Services column survives cache reloads. List fields (events, conditions,
// waiting, restart causes) are separated with \x1f (US) rather than ',' because
// a FailedScheduling detail message legitimately contains commas; the \x1f
// sentinel keeps a single warning event from being shattered into bogus
// "reasons" on round-trip.
func serializeHealth(h k8s.Health) string {
	if h.Replicas == 0 && h.DesiredReplicas == 0 && h.Restarts == 0 && len(h.RestartCauses) == 0 && len(h.Events) == 0 && len(h.RecentEvents) == 0 && len(h.OldEvents) == 0 && len(h.Waiting) == 0 && len(h.Conditions) == 0 && h.PendingPods == 0 && h.FailedPods == 0 && !h.Progressing && !h.Paused && h.ScaleUp == 0 && h.ScaleDown == 0 && h.StartupMax == 0 && h.DeployDuration == 0 && !h.HpaScaleLimited {
		return ""
	}
	events := strings.Join(h.Events, healthListSep)
	conditions := strings.Join(h.Conditions, healthListSep)
	causes := strings.Join(h.RestartCauses, healthListSep)
	recent := strings.Join(h.RecentEvents, healthListSep)
	old := strings.Join(h.OldEvents, healthListSep)
	waiting := strings.Join(h.Waiting, healthListSep)
	return fmt.Sprintf("%v|%d|%d|%d|%s|%s|%d|%d|%s|%s|%s|%s|%v|%v|%d|%d|%d|%d|%d|%d|%d|%d|%v", h.Ready, h.ReadyReplicas, h.Replicas, h.Restarts, events, conditions, h.PendingPods, h.FailedPods, causes, recent, old, waiting, h.Progressing, h.Paused, h.DesiredReplicas, h.ScaleUp, h.ScaleDown, h.NewReadyReplicas, h.StuckPendingPods, h.StartupMax.Nanoseconds(), h.DeployDuration.Nanoseconds(), h.HpaMaxReplicas, h.HpaScaleLimited)
}

// healthListSep separates list fields inside the serialized health string.
// See serializeHealth for why ',' is unusable.
const healthListSep = "\x1f"

// parseHealth decodes a value produced by serializeHealth. Older cache rows
// with fewer fields are still accepted.
func parseHealth(s string) k8s.Health {
	var h k8s.Health
	h.NewReadyReplicas = -1
	parts := strings.SplitN(s, "|", 23)
	if len(parts) < 5 {
		return h
	}
	fmt.Sscanf(parts[0], "%t", &h.Ready)
	fmt.Sscanf(parts[1], "%d", &h.ReadyReplicas)
	fmt.Sscanf(parts[2], "%d", &h.Replicas)
	fmt.Sscanf(parts[3], "%d", &h.Restarts)
	if parts[4] != "" {
		h.Events = strings.Split(parts[4], healthListSep)
	}
	if len(parts) > 5 && parts[5] != "" {
		h.Conditions = strings.Split(parts[5], healthListSep)
	}
	if len(parts) > 6 {
		fmt.Sscanf(parts[6], "%d", &h.PendingPods)
	}
	if len(parts) > 7 {
		fmt.Sscanf(parts[7], "%d", &h.FailedPods)
	}
	if len(parts) > 8 && parts[8] != "" {
		h.RestartCauses = strings.Split(parts[8], healthListSep)
	}
	if len(parts) > 9 && parts[9] != "" {
		h.RecentEvents = strings.Split(parts[9], healthListSep)
	}
	if len(parts) > 10 && parts[10] != "" {
		h.OldEvents = strings.Split(parts[10], healthListSep)
	}
	if len(parts) > 11 && parts[11] != "" {
		h.Waiting = strings.Split(parts[11], healthListSep)
	}
	if len(parts) > 12 {
		fmt.Sscanf(parts[12], "%t", &h.Progressing)
	}
	if len(parts) > 13 {
		fmt.Sscanf(parts[13], "%t", &h.Paused)
	}
	if len(parts) > 14 {
		fmt.Sscanf(parts[14], "%d", &h.DesiredReplicas)
	}
	if len(parts) > 15 {
		fmt.Sscanf(parts[15], "%d", &h.ScaleUp)
	}
	if len(parts) > 16 {
		fmt.Sscanf(parts[16], "%d", &h.ScaleDown)
	}
	if len(parts) > 17 {
		fmt.Sscanf(parts[17], "%d", &h.NewReadyReplicas)
	}
	if len(parts) > 18 {
		fmt.Sscanf(parts[18], "%d", &h.StuckPendingPods)
	}
	if len(parts) > 19 {
		var ns int64
		fmt.Sscanf(parts[19], "%d", &ns)
		h.StartupMax = time.Duration(ns)
	}
	if len(parts) > 20 {
		var ns int64
		fmt.Sscanf(parts[20], "%d", &ns)
		h.DeployDuration = time.Duration(ns)
	}
	if len(parts) > 21 {
		fmt.Sscanf(parts[21], "%d", &h.HpaMaxReplicas)
	}
	if len(parts) > 22 {
		fmt.Sscanf(parts[22], "%t", &h.HpaScaleLimited)
	}
	return h
}

type healthSeg struct {
	text string
	kind int
	dim  bool // past events: rendered faint but keeping their hue
}

const (
	segOK = iota
	segWarn
	segBad
	segMuted
	segInfo
	segSep
)

// eventFilter controls whether transient warning reasons are hidden from the
// health column, and if so, which reasons count as transient. When enabled,
// filtered events are replaced by a muted "~N" count so hiding is never silent.
type eventFilter struct {
	hide      bool
	transient map[string]bool
}

// k8sTimebox maps the configured event timeboxes onto the k8s client. Zero
// values fall back to the k8s defaults (1m/10m/1h), matching the k8s event
// TTL; raise event_history when the cluster retains events longer.
func (m *Model) k8sTimebox() k8s.EventTimebox {
	return k8s.EventTimebox{
		Recent:  m.cfg.K8s.EventRecent,
		Warn:    m.cfg.K8s.EventWarn,
		History: m.cfg.K8s.EventHistory,
	}
}

// eventFilter resolves the transient-event display filter from config. The
// configured list, when set, replaces the built-in default.
func (m *Model) eventFilter() eventFilter {
	f := eventFilter{hide: m.cfg.K8s.HideTransient}
	if !f.hide {
		return f
	}
	reasons := k8s.DefaultTransientEvents
	if len(m.cfg.K8s.TransientEvents) > 0 {
		reasons = m.cfg.K8s.TransientEvents
	}
	f.transient = make(map[string]bool, len(reasons))
	for _, r := range reasons {
		f.transient[r] = true
	}
	return f
}

// healthSegments splits a workload's health into display parts so callers can
// style each individually. Plain ✔ is current readiness — for at-target
// workloads it carries the ready/desired replica fraction (e.g. "✔3/3"). Blue
// is deployment
// state: ⟳Progressing (with the new-pods-ready fraction over desired, e.g.
// "⟳Progressing 2/5" — ready pods on the current ReplicaSet), ⏸DeploymentPaused
// awaiting manual approval, and ⇑ current/desired / ⇓ current/desired while
// scaling to the target replica count (the arrow is the headline when not yet
// ready, trailing the ✔ otherwise). Yellow is history and waiting: ↻N restarts, their causes, and
// pending pods. Red is current problems: ✖ not-ready (when not mid-rollout and
// not scaling), hard deployment conditions (ReplicaFailure, Degraded,
// ProgressDeadlineExceeded), failed pods and their reasons (e.g. Evicted),
// stuck containers, and pending pods stuck with a reason (Unschedulable,
// FailedMount, ...). A rollout in progress keeps the blue ⟳ fraction and
// prepends a red ✖ only when genuinely broken — the transient "Unavailable"
// condition and benign starting pods don't flip it. Past warning events
// (recent/warn/old buckets, and the hidden ~N count) are grouped at the end
// behind a │ separator and marked dim so they read as past and ignorable while
// keeping their hue. Restarts turn red only when the workload is currently in
// trouble; a rollout in progress shows a blue ⟳ instead of the readiness check.
// statusSegments computes the headline "status" segments for a workload: the
// ✔/✖/⟳/⇑/⇓ switch output that healthSegments leads with. For a failing rollout
// this is [✖, ⟳Progressing N/M]; the Status column shows only the first of
// these (✖) and Details gets the rest. Returns empty when the workload has no
// primary state (e.g. scaled to zero with only history).
func statusSegments(h k8s.Health) []healthSeg {
	up, down := scaleDirection(h)
	scaling := up || down
	problems := h.FailedPods > 0 || len(h.Waiting) > 0 || len(h.FailedReasons) > 0 || h.StuckPendingPods > 0 ||
		(!h.Ready && !h.Progressing && !scaling) || hasHardCondition(h.Conditions)
	if !h.Progressing {
		problems = problems || h.PendingPods > 0
	}
	var segs []healthSeg
	switch {
	case h.Progressing:
		if problems {
			s := "✖"
			if h.DesiredReplicas > 0 && h.NewReadyReplicas >= 0 {
				s = fmt.Sprintf("✖%d/%d", h.NewReadyReplicas, h.DesiredReplicas)
			}
			segs = append(segs, healthSeg{s, segBad, false})
		}
		icon := "⟳Progressing"
		if h.DesiredReplicas > 0 && h.NewReadyReplicas >= 0 {
			icon = fmt.Sprintf("⟳Progressing %d/%d", h.NewReadyReplicas, h.DesiredReplicas)
		}
		segs = append(segs, healthSeg{icon, segInfo, false})
	case h.Ready && !problems:
		icon := "✔"
		if !scaling && h.DesiredReplicas > 0 {
			icon = fmt.Sprintf("✔%d/%d", h.ReadyReplicas, h.DesiredReplicas)
		}
		segs = append(segs, healthSeg{icon, segOK, false})
	case scaling && !problems:
		dir := "⇓"
		if up {
			dir = "⇑"
		}
		if h.DesiredReplicas > 0 {
			segs = append(segs, healthSeg{fmt.Sprintf("%s%d/%d", dir, h.ReadyReplicas, h.DesiredReplicas), segInfo, false})
		} else {
			segs = append(segs, healthSeg{fmt.Sprintf("%s", dir), segInfo, false})
		}
	case h.Replicas > 0:
		s := "✖"
		if h.DesiredReplicas > 0 {
			s = fmt.Sprintf("✖%d/%d", h.ReadyReplicas, h.DesiredReplicas)
		}
		segs = append(segs, healthSeg{s, segBad, false})
	}
	return segs
}

// healthSegments computes all display segments for a health value, starting
// with the status segments and continuing with timings, history, events, and
// contributor-level details.
func healthSegments(h k8s.Health, ev eventFilter) []healthSeg {
	up, down := scaleDirection(h)
	scaling := up || down
	problems := h.FailedPods > 0 || len(h.Waiting) > 0 || len(h.FailedReasons) > 0 || h.StuckPendingPods > 0 ||
		(!h.Ready && !h.Progressing && !scaling) || hasHardCondition(h.Conditions)
	if !h.Progressing {
		problems = problems || h.PendingPods > 0
	}
	restartKind := segWarn
	if problems {
		restartKind = segBad
	}
	segs := statusSegments(h)
	if h.StartupMax > 0 {
		segs = append(segs, healthSeg{fmt.Sprintf("⏱%s", shortDur(h.StartupMax)), segMuted, false})
	}
	if h.DeployDuration > 0 {
		segs = append(segs, healthSeg{fmt.Sprintf("⧗%s", durCompact(h.DeployDuration)), segMuted, false})
	}
	if h.Paused {
		segs = append(segs, healthSeg{"⏸DeploymentPaused", segInfo, false})
	}
	if scaling && (h.Ready || problems) {
		dir := "⇓"
		if up {
			dir = "⇑"
		}
		if h.DesiredReplicas > 0 {
			segs = append(segs, healthSeg{fmt.Sprintf("%s%d/%d", dir, h.ReadyReplicas, h.DesiredReplicas), segInfo, false})
		} else {
			segs = append(segs, healthSeg{fmt.Sprintf("%s", dir), segInfo, false})
		}
	}
	if h.Restarts > 0 {
		segs = append(segs, healthSeg{fmt.Sprintf("↻%d", h.Restarts), restartKind, false})
	}
	for _, c := range h.RestartCauses {
		if s := shortEvent(c); s != "" {
			segs = append(segs, healthSeg{"↻" + s, restartKind, false})
		}
	}
	for _, c := range h.Conditions {
		if (h.Progressing || scaling) && c == "Unavailable" {
			continue
		}
		if s := shortEvent(c); s != "" {
			segs = append(segs, healthSeg{reasonPrefix(s) + s, segBad, false})
		}
	}
	for _, r := range h.FailedReasons {
		if s := shortEvent(r); s != "" {
			segs = append(segs, healthSeg{reasonPrefix(s) + s, segBad, false})
		}
	}
	for _, w := range h.Waiting {
		if s := shortEvent(w); s != "" {
			segs = append(segs, healthSeg{reasonPrefix(s) + s, segBad, false})
		}
	}
	if h.PendingPods > 0 {
		segs = append(segs, healthSeg{fmt.Sprintf("⌛%d", h.PendingPods), segWarn, false})
	}
	if h.FailedPods > 0 {
		segs = append(segs, healthSeg{fmt.Sprintf("💀%d", h.FailedPods), segBad, false})
	}
	// HPA pinned at maxReplicas: a current capacity state, so it sits with the
	// status segments rather than behind the past-events │ separator.
	if h.HpaScaleLimited && h.HpaMaxReplicas > 0 {
		segs = append(segs, healthSeg{"⚠HPA:ScalingLimited", segWarn, false})
	}
	var events []healthSeg
	// HPA rescale totals share the muted │ section with past events, sitting
	// ahead of them so the hour view reads as one auxiliary block.
	if h.ScaleUp > 0 || h.ScaleDown > 0 {
		text := "HPA"
		if h.ScaleUp > 0 {
			text += fmt.Sprintf("↑%d", h.ScaleUp)
		}
		if h.ScaleDown > 0 {
			text += fmt.Sprintf("↓%d", h.ScaleDown)
		}
		events = append(events, healthSeg{text, segInfo, false})
	}
	hidden := 0
	addEvents := func(list []string, kind int) {
		for _, e := range list {
			reason, detail, _, _ := eventReasonAge(e)
			s := shortEvent(reason)
			if s == "" {
				continue
			}
			if ev.hide && ev.transient[reason] {
				hidden++
				continue
			}
			text := reasonPrefix(s) + s
			if detail != "" {
				text += "(" + compactSchedMsg(detail) + ")"
			}
			events = append(events, healthSeg{text, kind, true})
		}
	}
	addEvents(h.RecentEvents, segBad)
	addEvents(h.Events, segWarn)
	addEvents(h.OldEvents, segMuted)
	if hidden > 0 {
		events = append(events, healthSeg{fmt.Sprintf("~%d", hidden), segMuted, true})
	}
	if len(events) > 0 {
		if len(segs) > 0 {
			segs = append(segs, healthSeg{"│", segSep, false})
		}
		segs = append(segs, events...)
	}
	return segs
}

func healthEmpty(h k8s.Health) bool {
	return h.Replicas == 0 && h.DesiredReplicas == 0 && h.Restarts == 0 && len(h.RestartCauses) == 0 &&
		len(h.Events) == 0 && len(h.RecentEvents) == 0 && len(h.OldEvents) == 0 && len(h.Waiting) == 0 &&
		len(h.Conditions) == 0 && len(h.FailedReasons) == 0 && h.PendingPods == 0 && h.FailedPods == 0 &&
		!h.Progressing && !h.Paused && h.ScaleUp == 0 && h.ScaleDown == 0 && h.StartupMax == 0 && h.DeployDuration == 0 &&
		!h.HpaScaleLimited
}

// statusText returns the plain-text Status column value: the first status
// segment (✔N/N, ✔, ✖, ⟳Progressing N/M, ⇑N/M, ⇓). Empty when the workload has
// no primary status (e.g. scaled to zero with only history).
func statusText(health string) string {
	h := parseHealth(health)
	if healthEmpty(h) {
		return ""
	}
	segs := statusSegments(h)
	if len(segs) == 0 {
		return ""
	}
	return segs[0].text
}

// renderStatusColored returns the styled Status column value.
func renderStatusColored(health string) string {
	h := parseHealth(health)
	if healthEmpty(h) {
		return ""
	}
	segs := statusSegments(h)
	if len(segs) == 0 {
		return ""
	}
	s := segs[0]
	switch s.kind {
	case segWarn:
		return healthWarn.Render(s.text)
	case segBad:
		return healthBad.Render(s.text)
	case segMuted:
		return healthMuted.Render(s.text)
	case segInfo:
		return healthInfo.Render(s.text)
	default:
		return s.text
	}
}

// formatHealth renders the cached health string as a compact plain-text column
// value: ✔ healthy (✔N/N when at target: ready/desired replicas), ⟳Progressing (new
// pods ready / desired, e.g. "⟳Progressing 2/5"),
// ⇑ current/desired / ⇓ current/desired scaling to the target replica count, ✖ not ready,
// ⏸DeploymentPaused paused,
// ↻N restarts, ↻OOMKilled restart causes, ⚠ error events (colored by age in
// the TUI), ∞ waiting/retrying, ⌛N pending, 💀N failed, ⚠ conditions.
// ⚠HPA:ScalingLimited (yellow) marks an HPA pinned at maxReplicas that wanted to scale
// higher. HPA rescale totals from the last hour (HPA ↑N ↓N) are grouped with
// past events behind a │ separator.
func formatHealth(health string, ev eventFilter) string {
	h := parseHealth(health)
	if healthEmpty(h) {
		return ""
	}
	segs := healthSegments(h, ev)
	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = s.text
	}
	return strings.Join(parts, " ")
}

// SerializeHealth encodes a k8s.Health value into the compact cache string used
// by the health column. Exported for non-interactive rendering of mock specs.
func SerializeHealth(h k8s.Health) string { return serializeHealth(h) }

// eventFilterFor resolves the transient-event display filter. An empty
// transientReasons falls back to k8s.DefaultTransientEvents.
func eventFilterFor(hideTransient bool, transientReasons []string) eventFilter {
	f := eventFilter{hide: hideTransient}
	if hideTransient {
		if len(transientReasons) == 0 {
			transientReasons = k8s.DefaultTransientEvents
		}
		f.transient = make(map[string]bool, len(transientReasons))
		for _, r := range transientReasons {
			f.transient[r] = true
		}
	}
	return f
}

// FormatHealthText renders a serialized health value as the unstyled plain-text
// health column, applying the transient-event filter.
func FormatHealthText(health string, hideTransient bool, transientReasons []string) string {
	return formatHealth(health, eventFilterFor(hideTransient, transientReasons))
}

// FormatHealthColored renders a serialized health value as the health column
// with the same ANSI styling the TUI uses.
func FormatHealthColored(health string, hideTransient bool, transientReasons []string) string {
	return renderHealthColored(health, eventFilterFor(hideTransient, transientReasons))
}

// shortEvent filters benign k8s event/condition reasons that don't deserve a
// segment, passing everything else through verbatim so the exact reason is
// always visible in the health column.
func shortEvent(reason string) string {
	switch reason {
	case "Started", "Pulled", "Pulling", "Scheduled", "SuccessfulCreate", "MinimumReplicasAvailable":
		return ""
	default:
		return reason
	}
}

// compactSchedMsg condenses a k8s scheduler (FailedScheduling) message for the
// Details column. The verbose node-availability breakdown ("0/69 nodes are
// available: ...") otherwise gets jammed into (...) and chopped by the column
// width into a garbled tail reading like "cpu) ⚠ 4 I…". The schedulable count
// and each reason stay visible; boilerplate plumbing ("nodes are available:",
// "node(s) had", "node(s) didn't match") is dropped. Non-scheduler messages
// pass through unchanged.
func compactSchedMsg(msg string) string {
	if msg == "" {
		return ""
	}
	// Only touch messages shaped like a scheduler result, "N/Total nodes ...
	// : breakdown". Everything else is left verbatim.
	colon := strings.Index(msg, ":")
	if !strings.Contains(msg, " nodes") || colon < 0 {
		return msg
	}
	count := strings.TrimSpace(msg[:colon])
	count = strings.ReplaceAll(count, " nodes are available", " nodes")
	rest := strings.TrimSpace(msg[colon+1:])
	// Drop a trailing "preemption: ..." sentence; it restates the same nodes.
	if i := strings.LastIndex(rest, " preemption:"); i >= 0 {
		rest = rest[:i]
	}
	var parts []string
	for _, p := range strings.Split(rest, ",") {
		if s := compressSchedPart(p); s != "" {
			parts = append(parts, s)
		}
	}
	out := count + ": " + strings.Join(parts, ", ")
	const maxSchedDetail = 80
	runes := []rune(out)
	if len(runes) > maxSchedDetail {
		cut := maxSchedDetail - 1
		for cut > 0 && runes[cut] != ' ' {
			cut--
		}
		out = strings.TrimRight(string(runes[:cut]), " ,") + "…"
	}
	return out
}

// compressSchedPart trims the filler from one comma-separated scheduler reason,
// e.g. "5 node(s) had taint {gpu=true}" -> "5 taint {gpu=true}".
func compressSchedPart(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.Replace(p, " node(s) had taint", " taint", 1)
	p = strings.Replace(p, " node(s) didn't match", "", 1)
	p = strings.Replace(p, " node(s) were unschedulable", " unschedulable", 1)
	p = strings.Replace(p, " node(s) had volume node affinity conflict", " volume node affinity", 1)
	p = strings.Replace(p, " node(s)", "", 1)
	return strings.Trim(strings.TrimSpace(p), ".,")
}

// eventReasonAge splits an event entry into its reason, detail message, and how
// long ago it last occurred. Warn and old events carry their collection
// timestamp as "reason[@detail]@<unix seconds>"; recent events and cached rows
// without one return ok=false so the display renders the bare reason.
func eventReasonAge(entry string) (reason, detail string, age time.Duration, ok bool) {
	// Strip the optional @unix timestamp first.
	prefix, ts, _ := strings.Cut(entry, "@")
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err == nil {
		age = time.Since(time.Unix(secs, 0))
		ok = true
	}
	// Strip the optional #detail message.
	reason, detail, _ = strings.Cut(prefix, "#")
	return
}

// waitingReasons are k8s reasons that mean a container/pod is stuck retrying
// (image pull, backoff, rescheduling) rather than hard-failed.
var waitingReasons = map[string]bool{
	"BackOff":                         true,
	"CrashLoopBackOff":                true,
	"ImagePullBackOff":                true,
	"ErrImagePull":                    true,
	"ErrImageNeverPull":               true,
	"InvalidImageName":                true,
	"RegistryUnavailable":             true,
	"ImageInspectError":               true,
	"ErrImagePullTimeout":             true,
	"FailedToRetrieveImagePullSecret": true,
	"FailedScheduling":                true,
}

// reasonPrefix picks the symbol prefix for a reason: ∞ for waiting/retry
// states, ⚠ for everything else bad. Restart causes use their own ↻ marker
// (see healthSegments). Text glyphs only: emoji glyphs (🔁, ⏳, 💀, ⚠) ignore
// ANSI foreground colors, so they'd never match the segment color.
func reasonPrefix(s string) string {
	if waitingReasons[s] {
		return "∞"
	}
	return "⚠"
}

// detailsText renders the plain (unstyled) Details column value: health segments
// after the first status segment plus any contributor names, used for width
// computation. For a failing rollout this keeps the ⟳Progressing N/M that the
// Status column's ✖ replaces.
func detailsText(health, contributors string, ev eventFilter) string {
	h := parseHealth(health)
	var parts []string
	if !healthEmpty(h) {
		segs := healthSegments(h, ev)
		if len(statusSegments(h)) > 0 && len(segs) > 1 {
			segs = segs[1:]
		} else if len(statusSegments(h)) > 0 {
			segs = nil
		}
		for _, s := range segs {
			parts = append(parts, s.text)
		}
	}
	if contributors != "" {
		parts = append(parts, contributors)
	}
	return strings.Join(parts, " ")
}

// renderHealthColored styles each health segment individually: plain ✔ (or a
// blue ⟳ mid-rollout), yellow history/waiting (restarts, causes, pending
// pods), red current problems (not-ready, conditions, failed, stuck
// containers), and muted older events. Past warning events are grouped at the
// end behind a muted │ and rendered faint (their hue preserved) so they read
// as past and ignorable. When the workload is currently in trouble the
// restarts are red too.
func renderHealthColored(health string, ev eventFilter) string {
	h := parseHealth(health)
	if healthEmpty(h) {
		return ""
	}
	segs := healthSegments(h, ev)
	parts := make([]string, len(segs))
	for i, s := range segs {
		switch s.kind {
		case segWarn:
			style := healthWarn
			if s.dim {
				style = style.Faint(true)
			}
			parts[i] = style.Render(s.text)
		case segBad:
			style := healthBad
			if s.dim {
				style = style.Faint(true)
			}
			parts[i] = style.Render(s.text)
		case segMuted:
			style := healthMuted
			if s.dim {
				style = style.Faint(true)
			}
			parts[i] = style.Render(s.text)
		case segInfo:
			parts[i] = healthInfo.Render(s.text)
		case segSep:
			parts[i] = healthMuted.Render(s.text)
		default:
			parts[i] = s.text
		}
	}
	return strings.Join(parts, " ")
}

// renderDetails styles the Details column: health segments after the first
// status segment plus contributor names. The health segments keep their colors
// on the selected row too — the highlight lives in the left margin, so the
// full-line reverse no longer applies here.
func renderDetails(health, contributors string, ev eventFilter) string {
	h := parseHealth(health)
	var parts []string
	if !healthEmpty(h) {
		segs := healthSegments(h, ev)
		if len(statusSegments(h)) > 0 && len(segs) > 1 {
			segs = segs[1:]
		} else if len(statusSegments(h)) > 0 {
			segs = nil
		}
		for _, s := range segs {
			switch s.kind {
			case segWarn:
				style := healthWarn
				if s.dim {
					style = style.Faint(true)
				}
				parts = append(parts, style.Render(s.text))
			case segBad:
				style := healthBad
				if s.dim {
					style = style.Faint(true)
				}
				parts = append(parts, style.Render(s.text))
			case segMuted:
				style := healthMuted
				if s.dim {
					style = style.Faint(true)
				}
				parts = append(parts, style.Render(s.text))
			case segInfo:
				parts = append(parts, healthInfo.Render(s.text))
			case segSep:
				parts = append(parts, healthMuted.Render(s.text))
			default:
				parts = append(parts, s.text)
			}
		}
	}
	if contributors != "" {
		parts = append(parts, contributors)
	}
	return strings.Join(parts, " ")
}

// serializeUntaggedCommits turns the untagged commit list into the on-disk JSON
// array, including per-commit authors so the contributors column survives cache
// reloads.
func serializeUntaggedCommits(commits []gh.CommitSummary) string {
	if len(commits) == 0 {
		return ""
	}
	var buf strings.Builder
	buf.WriteByte('[')
	for i, c := range commits {
		if i > 0 {
			buf.WriteByte(',')
		}
		cb, _ := json.Marshal(c.Contributors)
		fmt.Fprintf(&buf, `{"sha":%q,"message":%q,"author":%q,"contributors":%s}`, c.SHA, c.Message, c.Author, cb)
	}
	buf.WriteByte(']')
	return buf.String()
}

// formatContributors renders the contributors column for a version: unique
// commit authors joined by ", ", with the release author marked "(release)"
// when it isn't itself one of the committers. Ignored bots are dropped, and
// names are deduped case-insensitively (a release author who also committed
// appears once).
func (m *Model) formatContributors(releaseAuthor string, contributors []string) string {
	ignored := make(map[string]bool, len(m.cfg.GitHub.IgnoreContributors))
	for _, ig := range m.cfg.GitHub.IgnoreContributors {
		ignored[strings.ToLower(ig)] = true
	}
	names := []string{}
	seen := map[string]bool{}
	markRelease := releaseAuthor != ""
	for _, c := range contributors {
		if strings.EqualFold(c, releaseAuthor) {
			markRelease = false
			break
		}
	}
	if releaseAuthor != "" && !ignored[strings.ToLower(releaseAuthor)] {
		seen[strings.ToLower(releaseAuthor)] = true
		if markRelease {
			names = append(names, releaseAuthor+" (release)")
		} else {
			names = append(names, releaseAuthor)
		}
	}
	for _, c := range contributors {
		if c == "" || seen[strings.ToLower(c)] || ignored[strings.ToLower(c)] {
			continue
		}
		seen[strings.ToLower(c)] = true
		names = append(names, c)
	}
	return strings.Join(names, ", ")
}

// truncateWidth cuts s to max display columns, appending an ellipsis. ANSI
// escape sequences are zero-width and copied atomically so colored strings
// truncate at the right width without emitting dangling escapes; a reset is
// appended when the cut lands inside an open sequence.
func truncateWidth(s string, max int) string {
	if max < 1 {
		return ""
	}
	if lipgloss.Width(s) <= max {
		return s
	}
	var out strings.Builder
	var w int
	escaped := false
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			escaped = true
			end := i + 1
			if end < len(s) && s[end] == '[' {
				end = csiEnd(s, i)
			}
			out.WriteString(s[i:end])
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := lipgloss.Width(string(r))
		if w+rw > max-1 {
			out.WriteString("…")
			if escaped {
				out.WriteString("\x1b[0m")
			}
			break
		}
		out.WriteRune(r)
		w += rw
		i += size
	}
	return out.String()
}

// csiEnd returns the index just past the CSI escape sequence starting at the
// ESC at s[start] (e.g. "\x1b[38;5;196m"), scanning to its final byte.
func csiEnd(s string, start int) int {
	i := start + 2 // skip ESC [
	for i < len(s) && s[i] >= 0x20 && s[i] < 0x40 {
		i++
	}
	if i < len(s) {
		i++
	}
	return i
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
	if d >= 7*24*time.Hour {
		return t.Format("Jan 2")
	}
	return relativeDur(d)
}

// relativeDur renders a duration compactly with the most granular whole unit
// ("<1m", "5m", "2h", "3d"), used for how-long-ago labels on events.
func relativeDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func renderRow(r row, selected bool, refreshing bool, refreshIcon string, repoWidth, maxWidth, authorWidth int) string {
	aiIcon := ""
	if refreshing {
		aiIcon = refreshIcon
	} else if r.reviewed && !r.reviewStale {
		aiIcon = "✦"
	} else if r.reviewed && r.reviewStale {
		aiIcon = "✧"
	}

	icon := ciIcon(r.ci, r.ci == "failure" && r.mergeState == "UNSTABLE")

	rev := ""
	if r.review != "" {
		rev = reviewIcon(r.review)
	} else if r.num > 0 {
		rev = "·"
	}

	sync := " "
	switch {
	case r.mergeable == "CONFLICTING" || r.mergeState == "DIRTY":
		sync = "≠"
	case r.mergeState == "BEHIND":
		sync = "↓"
	case r.mergeState == "CLEAN" || r.mergeState == "UNSTABLE":
		sync = "↑"
	}

	left := padWidth(aiIcon, 4) + icon + "  " + rev

	margin := padWidth(left, 10)

	const ageWidth = 6

	if r.num > 0 {
		title := r.title
		if r.draft {
			title = "[DRAFT] " + title
		}
		ts := relativeTime(r.updatedAt)
		repo := truncateWidth(r.repo, repoWidth)
		titleWidth := maxWidth - repoWidth - 33
		if authorWidth > 0 {
			titleWidth -= authorWidth + 2
		}
		if titleWidth < 1 {
			titleWidth = 1
		}
		rest := fmt.Sprintf("%s%s  #%-5d  %s",
			padWidth(sync, 5), padWidth(repo, repoWidth), r.num, padWidth(truncateWidth(title, titleWidth), titleWidth))
		if authorWidth > 0 {
			author := ""
			if r.author != "" {
				author = healthMuted.Render(truncateWidth(r.author, authorWidth))
			}
			rest += "  " + padWidth(author, authorWidth)
		}
		rest += "  " + padWidth(ts, ageWidth)
		if selected {
			return selectedStyle.Render(margin) + rest
		}
		return margin + rest
	}

	line := r.title
	if maxWidth > 0 {
		line = truncateWidth(line, maxWidth)
	}
	if selected {
		return selectedStyle.Render(line)
	}
	return rowStyle.Render(line)
}

func (m Model) viewHelp() string {
	line := helpKey.Render("🛸 ship")
	if !m.lastRefreshed.IsZero() {
		line += helpKey.Render(" (refresh in " + refreshCountdown(m.lastRefreshed, m.cfg.RefreshInterval) + ")")
	}
	if m.gh != nil {
		if rls := m.gh.RateLimits(); len(rls) > 0 {
			var parts []string
			for _, rl := range rls {
				parts = append(parts, rateLimitStyle(rl).Render(rateLimitText(rl)))
			}
			line += helpKey.Render("  " + strings.Join(parts, " · "))
		}
	}
	if n := k8s.InFlight(); n > 0 {
		line += helpKey.Render(fmt.Sprintf("  %d k8s", n))
	}
	if n := len(m.notifications); n > 0 {
		line += helpKey.Render(fmt.Sprintf("  🔔 %d", n))
	} else if m.showNotif {
		line += helpKey.Render("  🔔 0")
	}
	if m.mode != "all" {
		line += helpKey.Render("  mode: " + m.mode)
	}
	line += "  " + helpKey.Render("?:") + " " + helpKey.Render("help")
	return line
}

// rateLimitText renders one resource's GitHub quota as "gql 4.8/5k".
// GitHub's resource keys are shortened for the footer: graphql -> gql,
// core -> rest. Thousands-scale numbers are compacted.
func rateLimitText(rl gh.RateLimit) string {
	name := rl.Resource
	switch name {
	case "graphql":
		name = "gql"
	case "core":
		name = "rest"
	}
	return fmt.Sprintf("%s %s/%s", name, compactRemaining(rl.Remaining), compactLimit(rl.Limit))
}

// compactScale divides a count by 1000, keeping one decimal but dropping a
// trailing ".0": 4800 -> "4.8", 5000 -> "5", 4960 -> "5".
func compactScale(n int) string {
	if n%1000 == 0 {
		return strconv.Itoa(n / 1000)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0")
}

// compactLimit shortens thousands-scale limits: 5000 -> "5k", 4800 -> "4.8k",
// 30 -> "30".
func compactLimit(n int) string {
	if n >= 1000 {
		return compactScale(n) + "k"
	}
	return strconv.Itoa(n)
}

// compactRemaining shortens a remaining count without a suffix: 4800 -> "4.8",
// 5000 -> "5", 27 -> "27".
func compactRemaining(n int) string {
	if n >= 1000 {
		return compactScale(n)
	}
	return strconv.Itoa(n)
}

// rateLimitStyle colors a resource's quota by how much remains: normal gray,
// yellow under 25%, red under 10%.
func rateLimitStyle(rl gh.RateLimit) lipgloss.Style {
	if rl.Limit <= 0 {
		return helpKey
	}
	frac := float64(rl.Remaining) / float64(rl.Limit)
	switch {
	case frac <= 0.10:
		return healthBad
	case frac <= 0.25:
		return healthWarn
	default:
		return helpKey
	}
}

const helpKeyWidth = 13

func helpKeyEntry(key, desc string) string {
	pad := helpKeyWidth - lipgloss.Width(key)
	if pad < 1 {
		pad = 1
	}
	padded := key + strings.Repeat(" ", pad)
	return "  " + helpKey.Render(padded) + " " + helpKey.Render(desc) + "\n"
}

func helpSection(title string, entries [][2]string) string {
	var b strings.Builder
	b.WriteString(helpKey.Render(title))
	b.WriteString("\n")
	for _, e := range entries {
		b.WriteString(helpKeyEntry(e[0], e[1]))
	}
	return b.String()
}

type coloredLegendEntry struct {
	key   string
	style lipgloss.Style
	desc  string
}

// helpSectionColored is helpSection for entries whose key is rendered in a
// caller-chosen style (e.g. the health-column glyphs). The padding and
// description stay in the muted help style so the legend aligns with the rest
// of the overlay.
func helpSectionColored(title string, entries []coloredLegendEntry) string {
	var b strings.Builder
	b.WriteString(helpKey.Render(title))
	b.WriteString("\n")
	for _, e := range entries {
		pad := helpKeyWidth - lipgloss.Width(e.key)
		if pad < 1 {
			pad = 1
		}
		b.WriteString("  ")
		b.WriteString(e.style.Render(e.key))
		b.WriteString(helpKey.Render(strings.Repeat(" ", pad)))
		b.WriteString(" ")
		b.WriteString(helpKey.Render(e.desc))
		b.WriteString("\n")
	}
	return b.String()
}

// shortDur renders a duration compactly (1m, 10m, 1h) instead of Go's padded
// String form (1m0s, 1h0m0s).
func shortDur(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

// durCompact renders a duration with up to two significant units so sub-minute
// precision survives (5m30s, 1h2m, 90s, 45s, 2h) — unlike shortDur, which
// collapses to a single whole unit.
func durCompact(d time.Duration) string {
	switch {
	case d >= time.Hour:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	case d >= time.Minute:
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// rangeDur renders a duration range with an en dash, collapsing a shared unit
// (1–10m, not 1m–10m) but keeping both units when they differ (90s–10m).
func rangeDur(a, b time.Duration) string {
	sa, sb := shortDur(a), shortDur(b)
	if len(sa) > 1 && len(sb) > 1 && sa[len(sa)-1] == sb[len(sb)-1] {
		return sa[:len(sa)-1] + "–" + sb
	}
	return sa + "–" + sb
}

func (m Model) renderHelpOverlay() string {
	nav := helpSection("nav", [][2]string{
		{"j/k", "move up/down"},
		{"tab/shift+tab", "next/prev section"},
		{"gg/G", "top/bottom"},
		{"n", "notifications"},
		{"q", "quit"},
	})

	var actions, filters, legend string
	prHelp := false
	if len(m.sections) > m.sectionIdx {
		s := m.sections[m.sectionIdx]
		switch s.name {
		case "My PRs", "Dependencies":
			prHelp = true
			actions = helpSection("actions", [][2]string{
				{"enter", "open in browser"},
				{"r", "refresh item"},
				{"R", "refresh all"},
				{"M", "merge pr"},
				{"C", "close pr"},
				{"D", "toggle pr draft"},
				{"A", "ai code review"},
				{"B", "browse on github"},
			})
			filters = helpSection("filters", [][2]string{
				{"m", "toggle mergeable"},
				{"d", "toggle drafts"},
				{"s", "toggle starred"},
				{"a", "toggle age sort"},
				{"/", "search"},
			})
			legend = helpSection("legend", [][2]string{
				{"↑", "up to date"},
				{"↓", "branch behind base (backmerge)"},
				{"≠", "merge conflict"},
				{"✗", "CI failing (blocks merge)"},
				{"/", "CI failing (not required)"},
			})
		case "To Review", "Team Review":
			prHelp = true
			teamLabel := "show team"
			if s.hideTeamReviews {
				teamLabel = "show mine only"
			}
			actions = helpSection("actions", [][2]string{
				{"enter", "open in browser"},
				{"r", "refresh item"},
				{"R", "refresh all"},
				{"M", "merge pr"},
				{"C", "close pr"},
				{"D", "toggle pr draft"},
				{"A", "ai code review"},
				{"B", "browse on github"},
			})
			filters = helpSection("filters", [][2]string{
				{"m", "toggle mergeable"},
				{"t", teamLabel},
				{"d", "toggle drafts"},
				{"s", "toggle starred"},
				{"a", "toggle age sort"},
				{"/", "search"},
			})
			legend = helpSection("legend", [][2]string{
				{"↑", "up to date"},
				{"↓", "branch behind base (backmerge)"},
				{"≠", "merge conflict"},
				{"✗", "CI failing (blocks merge)"},
				{"/", "CI failing (not required)"},
			})
		case "Services":
			svcEntries := [][2]string{
				{"enter", "open k9s"},
				{"r", "refresh item"},
				{"R", "refresh all"},
				{"d", "open diff in browser"},
				{"T", "create tag/release"},
			}
			if r := m.currentRow(); r != nil && r.repo != "" {
				for _, svc := range m.cfg.Services {
					if svc.Repo == r.repo && svc.DeployURL != "" {
						svcEntries = append(svcEntries, [2]string{"S", "open deploy"})
						break
					}
				}
			}
			actions = helpSection("actions", svcEntries)
			legend = helpSection("legend", [][2]string{
				{"✔3/3", "healthy · ready/desired replicas"},
				{"✖", "unhealthy"},
				{"⟳Progressing 2/5", "rolling out · new pods ready / desired"},
				{"⇑3/4 / ⇓3/1", "scaling to target replicas (current/desired)"},
				{"HPA↑4↓2", "HPA rescaled pods (last hour)"},
				{"⚠HPA:ScalingLimited", "HPA at maxReplicas · wanted more"},
				{"⏱30s", "startup · max app-start→ready (probe sizing)"},
				{"⧗5m30s", "last deploy · rollout duration (start→healthy, incl. image pull)"},
				{"↻N", "restarts"},
				{"↻OOMKilled", "last restart cause"},
				{"⌛N", "pods pending"},
				{"💀N", "pods failed"},
				{"⚠SomeError", "past error event (dimmed)"},
				{"⚠StartupProbeFailed", "probe failure · startup/liveness/readiness"},
				{"∞SomeBackoff", "retrying event"},
				{"~N", "transient events hidden"},
				{"│", "separates events from details"},
			})
			tb := m.k8sTimebox().Normalized()
			legend += "\n" + helpSectionColored("colors", []coloredLegendEntry{
				{"red", healthBad, "unhealthy · conditions · failed pods · events ≤" + shortDur(tb.Recent)},
				{"yellow", healthWarn, "events " + rangeDur(tb.Recent, tb.Warn) + " · restarts · pending"},
				{"gray", healthMuted, "older events " + rangeDur(tb.Warn, tb.History)},
				{"blue", healthInfo, "⟳Progressing · ⏸DeploymentPaused"},
			})
		}
	}

	left := nav
	if filters != "" {
		left += "\n" + filters
	}
	if actions != "" && !prHelp {
		left += "\n" + actions
	}
	left += "\n" + helpSection("modes", [][2]string{
		{":services", "services only"},
		{":mine", "my prs only"},
		{":review", "to review only"},
		{":deps", "dependencies only"},
		{":all", "all sections"},
		{":quit", "quit"},
	})
	right := legend
	if prHelp && actions != "" {
		right = actions + "\n" + right
	}
	if right != "" {
		left = lipgloss.NewStyle().PaddingRight(6).Render(left)
		left = lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	}

	return dialogStyle.Width(0).Render(dialogTitle.Render("Keys") + "\n\n" + left + "\n\n" + dialogHelp.Render("? or esc to close"))
}

func (m Model) renderNotificationsPanel() string {
	var b strings.Builder
	count := len(m.notifications)
	b.WriteString(dialogTitle.Render(fmt.Sprintf("Notifications (%d)", count)))
	b.WriteString("\n\n")
	if count == 0 {
		b.WriteString(helpKey.Render("No notifications."))
	} else {
		maxW := m.width
		if maxW > 90 {
			maxW = 90
		}
		if maxW < 60 {
			maxW = 60
		}
		for i, e := range m.notifications {
			sel := i == m.notifCursor
			badge := notifBadge(e.Kind)
			repo := e.Repo
			if e.Number > 0 {
				repo = fmt.Sprintf("%s#%d", e.Repo, e.Number)
			}
			title := e.Message
			if e.Title != "" {
				title = e.Title
			}
			line := fmt.Sprintf("%s %s  %s", badge, repo, title)
			if !e.CreatedAt.IsZero() {
				line += "  " + helpKey.Render("("+notifAge(e.CreatedAt)+")")
			}
			line = truncateWidth(line, maxW)
			if sel {
				line = selectedStyle.Render(line)
			} else {
				line = rowStyle.Render(line)
			}
			b.WriteString(line)
			if e.Detail != "" {
				d := "    " + e.Detail
				d = truncateWidth(d, maxW)
				if sel {
					d = selectedStyle.Render(d)
				} else {
					d = helpKey.Render(d)
				}
				b.WriteString("\n" + d)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(dialogHelp.Render("j/k move · enter open · x clear · c clear all · esc close"))
	return dialogStyle.Width(0).Render(b.String())
}

func notifBadge(k notify.Kind) string {
	switch k {
	case notify.KindReviewRequested:
		return "📥"
	case notify.KindReviewChange:
		return "🔁"
	case notify.KindCIFailed:
		return "💥"
	case notify.KindMergeable:
		return "✅"
	case notify.KindPendingTag:
		return "🏷"
	case notify.KindHealth:
		return "🩺"
	case notify.KindDepPR:
		return "⬆"
	case notify.KindNewComment:
		return "💬"
	}
	return "•"
}

func notifAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func (m Model) renderModePrompt() string {
	var b strings.Builder
	// Highlighted left gutter matching the row loader column, then the row
	// separator space, so the prompt text lines up with the section content.
	b.WriteString(modePad.Render(""))
	b.WriteString(" ")
	b.WriteString(inputStyle.Render("> " + m.cmdBuf))
	b.WriteString(modeCursor.Render("█"))
	if matches := m.modeMatches(); len(matches) > 0 {
		b.WriteString("  ")
		var parts []string
		for i, tok := range matches {
			if i == m.cmdSug {
				parts = append(parts, selectedStyle.Render(tok))
			} else {
				parts = append(parts, helpKey.Render(tok))
			}
		}
		b.WriteString(strings.Join(parts, " "))
	}
	return b.String()
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

	if m.tagMeta.sha != "" {
		target := m.tagMeta.sha
		if len(target) > 7 {
			target = target[:7]
		}
		b.WriteString(helpKey.Render("Target: " + target))
		if m.tagMeta.branch != "" {
			b.WriteString(helpKey.Render(" on " + m.tagMeta.branch))
		}
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

func (m Model) execAction(action, repo string, num int, draft bool, msg string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		var err error
		switch action {
		case "merge":
			err = m.gh.MergePR(ctx, repo, num)
		case "close":
			err = m.gh.ClosePR(ctx, repo, num, msg)
		case "draft-toggle":
			err = m.gh.ToggleDraft(ctx, repo, num, draft)
		}
		return actionDoneMsg{action: action, repo: repo, num: num, err: err}
	}
}

// ciIcon renders a PR's CI column glyph. A failure whose merge state is
// UNSTABLE is a failing check that isn't required — it still reads as a fail
// (/), but mergeable, unlike the blocking ✗.
func ciIcon(state string, optional bool) string {
	switch state {
	case "success":
		return "✓"
	case "failure":
		if optional {
			return "/"
		}
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
		MergeState:     p.MergeState,
		UpdatedAt:      p.UpdatedAt,
		IsDraft:        p.IsDraft,
		HeadSHA:        p.HeadRefOid,
	}
}

// serviceRowURL links a service row to its prod version. SHA refs point at
// the commit (releases/tag/<sha> doesn't exist); tags point at the release.
func serviceRowURL(repo, ref string) string {
	if sha, ok := k8s.SHAFromImage(ref); ok {
		return fmt.Sprintf("https://github.com/%s/commit/%s", repo, sha)
	}
	return fmt.Sprintf("https://github.com/%s/releases/tag/%s", repo, ref)
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

// openK9s launches k9s for the service's context/namespace in a new terminal,
// starting in the Deployments or Rollouts view to match the workload kind.
func (m Model) openK9s(svc config.ServiceConfig) {
	args := []string{"k9s"}
	if svc.Context != "" {
		args = append(args, "--context", svc.Context)
	}
	if svc.Namespace != "" {
		args = append(args, "--namespace", svc.Namespace)
	}
	if svc.ResourceType() == "rollout" {
		args = append(args, "--command", "rollout")
	} else {
		args = append(args, "--command", "deploy")
	}
	if shipLog != nil {
		shipLog.Printf("open k9s: launching %q (kitty=%q terminal=%q)", args, os.Getenv("KITTY_WINDOW_ID"), os.Getenv("TERMINAL"))
	}
	if err := launchTerminal(args, "k9s: "+svc.Name); err != nil {
		if shipLog != nil {
			shipLog.Printf("open k9s: %v", err)
		}
	}
}

// launchTerminal runs args in a new terminal. Inside kitty it opens a split
// pane; otherwise it falls back to $TERMINAL, Terminal.app on macOS, or a
// common Linux terminal emulator.
func launchTerminal(args []string, title string) error {
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		kittyArgs := []string{"@", "launch", "--location=vsplit", "--title=" + title}
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "KITTY_") || !strings.Contains(kv, "=") {
				continue
			}
			kittyArgs = append(kittyArgs, "--env", kv)
		}
		cmd := exec.Command("kitty", append(kittyArgs, args...)...)
		if err := cmd.Start(); err != nil {
			return err
		}
		return nil
	}
	if t := os.Getenv("TERMINAL"); t != "" {
		parts := strings.Fields(t)
		cmd := exec.Command(parts[0], append(parts[1:], args...)...)
		if err := cmd.Start(); err != nil {
			return err
		}
		return nil
	}
	if runtime.GOOS == "darwin" {
		script := fmt.Sprintf(`tell application "Terminal" to do script %q`, joinArgs(args))
		cmd := exec.Command("osascript", "-e", script)
		if err := cmd.Start(); err != nil {
			return err
		}
		return nil
	}
	for _, emu := range [][]string{
		{"x-terminal-emulator", "-e"},
		{"xterm", "-e"},
		{"gnome-terminal", "--"},
		{"alacritty", "-e"},
		{"konsole", "-e"},
	} {
		if _, err := exec.LookPath(emu[0]); err != nil {
			continue
		}
		cmd := exec.Command(emu[0], append(emu[1:], args...)...)
		if err := cmd.Start(); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("no terminal emulator found")
}

// joinArgs joins a command line for a terminal that takes a single program
// argument, quoting any value that isn't a plain word.
func joinArgs(args []string) string {
	var b strings.Builder
	for _, a := range args {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		if strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:") == "" {
			b.WriteString(a)
		} else {
			b.WriteString(fmt.Sprintf("%q", a))
		}
	}
	return b.String()
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
	if s.draftFilter == "draft" && s.name != "Services" {
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
	case "deploy-error":
		b.WriteString(dialogTitle.Render("No deploy URL"))
		b.WriteString("\n\n")
		b.WriteString(fmt.Sprintf("%s has no deploy_url configured.", c.repo))
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
	if c.action == "close" {
		cursor := " "
		if m.msgCursorOn {
			cursor = "█"
		}
		b.WriteString("\n\n")
		b.WriteString(dialogHelp.Render("message: ") + inputStyle.Render(c.msg+cursor))
	}
	b.WriteString("\n")
	if c.action == "close" {
		b.WriteString(dialogHelp.Render("type a message (leave blank to just close) · enter to close · esc to cancel"))
	} else {
		b.WriteString(dialogHelp.Render("enter to confirm · esc to cancel"))
	}
	return dialogStyle.Render(b.String())
}
