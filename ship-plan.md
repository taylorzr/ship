# ship — Personal Dev Hub

A terminal dashboard (`ship` binary) that tracks your work from GitHub all the
way to what's running in prod — the gap `gh` can't show you — plus reflection, AI
review, and Jira drafting. Personal, single-user, terminal-native. Reuses existing
CLI creds (`gh`, `claude`) and your kubeconfig; no new auth to manage.

> Naming note: the tool is `ship`. The prod-version-tracking view is named
> **Releases** (not "Ship") to avoid colliding with the tool name.

## Core thesis

The differentiated value is **end-to-end GitHub → prod version tracking**
(prod-on-v10, v11/v12 tagged, N untagged commits pending). PR/review/dependency
tracking is table-stakes context around it. AI review and Jira drafting are
force-multipliers that reuse the `claude` CLI.

## Two things to verify before building much

1. **Version-resolution chain (M2, the whole thesis).** For one pilot service,
   confirm: the prod image tag resolves to a git ref you can `compare` against
   (or a reliable label/annotation fallback exists); and you can reach the prod
   cluster read-only from your laptop (VPN, RBAC to `get deployment`). If this
   chain doesn't resolve cleanly, the Releases feature needs rethinking first.
2. **Atlassian MCP in headless mode (M10).** Confirm the Atlassian MCP is
   available under `claude -p` (interactively-authenticated MCPs sometimes
   aren't). If not, fall back to direct Jira REST.

---

## Stack

- **Language:** Go + Bubble Tea / Lipgloss / Bubbles. Single static binary `ship`.
- **GitHub read:** `githubv4` (GraphQL), poll + cache.
- **GitHub actions:** shell out to `gh` (merge, approve, rerun, tag/release, PR comment).
- **k8s:** `client-go`, read-only, kubeconfig contexts (EKS exec-auth honored transparently).
- **AI:** shell out to `claude -p` (print mode) — no API key, reuses Claude Code login.
- **Jira:** draft via `claude -p`; create via Atlassian MCP through `claude` (fallback: direct REST).
- **Config:** TOML (hand-editable) at `~/.config/ship/config.toml`.
- **Cache + counts:** SQLite at `~/.cache/ship/cache.db`.
- **Task interface:** `just` — `just init`, `just run`, `just check`. `direnv` `.envrc` for local config.

## Auth model (all borrowed)

- GitHub: `gh auth token` → bearer for `githubv4`; handles FanDuel SSO/enterprise.
- k8s: default kubeconfig loader; per-service config names the context.
- AI + Jira: existing `claude` CLI login (+ its Atlassian MCP connection).

Requirements on PATH: `gh`, `claude`, a working kubeconfig. No secrets stored by the tool.

---

## Project layout

```
ship/
  justfile
  .envrc
  cmd/ship/main.go           # arg parsing: default TUI, --count, --refresh
  internal/
    config/                  # TOML load/save: stars, service mappings, jira mappings
    store/                   # SQLite: cache, refresh metadata, interaction/reflection counts
    github/                  # githubv4 queries; gh shell-outs for actions
    k8s/                     # client-go deployment reads
    version/                 # resolver chain + git-compare (the crux)
    ai/                      # claude -p dispatch: review + jira draft
    jira/                    # create transport (MCP-via-claude | REST)
    reflect/                 # contribution/merge aggregation
    tui/                     # bubbletea models/views/keys
    ambient/                 # --count formatter
```

---

## Config (`~/.config/ship/config.toml`)

```toml
[github]
owners = ["fanduel"]                      # scope PR search to these owners/orgs (user: qualifier)
teams  = ["fanduel/podium"]               # team-review-requested scope: team review section + dep PRs

[k8s]
login_command = "aws sso login"           # run to re-auth when k8s requests fail with expired creds

[ai]
review_provider = "claude-cli"            # claude-cli | api
review_model    = "opus"                  # claude CLI model alias; "sonnet" for cheaper

[[repo]]
name    = "fanduel/podium-deploy-api"
starred = true

# Releases tracking — only for services tracked to prod
[[service]]
repo             = "fanduel/podium-deploy-api"
context          = "prd-use1"             # kubeconfig context
namespace        = "podium"
workload         = "podium-deploy-api"    # Deployment (or Rollout) name
version_strategy = "image-tag"            # image-tag | annotation
version_key      = ""                     # annotation/label key when strategy=annotation
deploy_url       = "https://buildkite.com/fanduel/podium-deploy-api"

# Jira defaults — convention-seeded, confirmed once per starred repo
[[jira]]
repo            = "fanduel/podium-deploy-api"
project         = "PODIUM"
default_type    = "Story"
epic_link_field = "customfield_10014"     # only used by the direct-REST transport
site            = "fanduel.atlassian.net"
```

Hand-editable; the tool also writes it (star/unstar, confirmed mappings).

## State (SQLite `~/.cache/ship/cache.db`)

- `pr` — number, repo, title, author, role (mine/review-direct/review-team/dep),
  url, review_decision, ci_state, mergeable, updated_at
- `version` — repo, prod_ref, prod_sha, ahead_by, pending_tags(json), resolved_at, error
- `reflection` — window (day/week/quarter), commits, prs_opened, prs_merged, reviews, fetched_at
- `pr_issue_link` — pr_url, jira_key (local backref after Jira creation)
- `refresh` — source (github/k8s), last_ok, last_attempt, status

Cache is disposable; deleting it forces a full refresh.

---

## Sections (TUI)

Ordered: a "needs action now" summary line, then:

1. **My PRs** — CI failing / approved-ready / changes-requested / waiting.
2. **To review** — direct (`user-review-requested:@me`) up top; team-routed
   (`review-requested:@me` minus direct) muted in an expandable "Other".
3. **Releases** — prod version vs pending tags/commits *(the crux)*.
4. **Dependencies** — renovate/dependabot bot PRs, CI-green first.
5. **Reflect** — toggle/tab, not always-on: commits/PRs shipped per window.

### Relevance

Tiered focus/muted. Starred repos = focus (default view); everything else
collapses into an expandable "Other". Nothing hidden, noise out of the way.
Solves the noisy-team-review-request problem. Star suggestions (later) from
GitHub interaction signal.

---

## GitHub layer (`internal/github`)

One GraphQL round per role, with `statusCheckRollup` + `reviewDecision` +
`mergeable` pulled inline on search nodes (no N+1):

- **My PRs:** `is:open is:pr author:@me archived:false`
- **Review — direct:** `is:open is:pr user-review-requested:@me`
- **Review — team:** `is:open is:pr review-requested:@me` minus the direct set → muted
- **Dependencies:** per starred repo, `is:open is:pr author:app/renovate` + `author:app/dependabot`
- CI state from `commits(last:1).statusCheckRollup.state`.

Actions via `gh` shell-out, each gated by a confirm keypress:

- merge → `gh pr merge --squash` (incl. single renovate PR — one at a time)
- approve → `gh pr review --approve`
- rerun CI → `gh run rerun`
- tag/release → `gh release create`
- PR comment (Jira backlink) → `gh pr comment`

---

## Releases / version tracking (`internal/version` + `internal/k8s`) — the crux

Per-service config, convention-seeded + confirm-once. For each tracked service:

1. `k8s.Deployment(context, ns, name)` via client-go, read-only.
2. Extract container image; resolve version (ordered fallback):
   - `image-tag`: parse tag after `:`; accept if it looks like a semver tag or 7–40 hex SHA.
   - `annotation`: read `version_key` from pod template labels/annotations.
3. Resolve ref → commit SHA via GitHub.
4. `compare(base: prodSHA, head: "main")` → `ahead_by` + commit list.
5. Pending tags: list tags, resolve to commits, keep those reachable from `main`
   but ahead of `prodSHA`, order topologically.
6. Render: `prod v10 · pending v11, v12 · +7 untagged`. On any failure (VPN down,
   RBAC, unparseable tag) store the error and show muted "version unknown" — never block.

**Releases actions:** read-only display + cut tag/release via `gh` (confirm); the
"deploy" action opens the per-service `deploy_url` in a browser. No pipeline coupling.

---

## Reflect (`internal/reflect`)

- **Data:** `viewer.contributionsCollection(from, to)` →
  `totalCommitContributions` + `totalPullRequestContributions` +
  `totalPullRequestReviewContributions` in one call per window. For "shipped"
  (merged), use search: `is:pr author:@me is:merged merged:>=<date>`.
- **Windows:** today / this week / this quarter, `from`/`to` computed client-side.
- **Display:** compact table — commits, PRs opened, PRs merged, reviews given,
  per window. Optionally scoped to starred repos.
- Cached in SQLite with fetch timestamp; refreshed lazily (not time-critical).

---

## AI review (`internal/ai`)

Action on any PR, keyed `A`. Dispatch to the `claude` CLI — no API key.

```bash
gh pr diff <n> | claude -p "<review stance>" \
  --output-format stream-json \
  --model opus \
  --append-system-prompt "<report-everything stance>" \
  --disallowedTools "Bash Edit Write MultiEdit"
```

- **Stance:** report every concern with severity + confidence; do NOT self-filter
  (the TUI filters/sorts). Sort high-severity first.
- **Structured findings:** prompt for JSON and parse the envelope's `.result`
  (Claude Code has no schema constraint). Be lenient — strip a ```json fence.
  ```
  { summary, concerns:[{file, line?, severity, confidence, issue, why}], questions:[] }
  ```
- **Input size:** most diffs fit Opus's context; if a diff is huge, warn + offer
  to review only changed files rather than silently truncating.
- **Streaming:** `--output-format stream-json` → render deltas live in the review pane.
- **Config:** `review_model` (opus default; sonnet for cheaper). `review_provider`
  left as a seam for a future SDK path.
- **Flavors:** (1) piped diff — default, bounded, deterministic; (2) agentic
  `claude -p "review PR #n"` with read tools — richer, slower, offered as "deep review" later.

---

## Jira drafting (`internal/ai` draft + `internal/jira` create)

Create-focused, not status tracking. Invoked from a PR, a starred repo, or
free-form intent. Flow: **draft → review/edit → confirm → create → link back.**

1. **Draft (AI, `claude -p`):** gather context, return structured issue:
   ```
   { project, issueType, summary, descriptionADF,
     parentEpic?, epicChildren?:[{summary, descriptionADF}], labels[] }
   ```
   For an epic, drafts the epic + first-pass child stories in one shot. Reuses the
   `fanduel-core:create-jira-ticket` skill's ADF/epic-linking logic.
2. **Review/edit:** render draft in TUI; edit summary/description/labels; approve or discard.
3. **Create (transport):** default **Atlassian MCP via `claude`** —
   `claude -p "create this issue: <json>" --allowedTools "mcp__…__createJiraIssue"`.
   Zero new creds. **Verify MCP works headless (see top).** Fallback: direct REST
   `POST /rest/api/3/issue` with an API token in keychain (build ADF yourself).
4. **Link back:** `gh pr comment` the new issue key on the source PR; store the
   PR↔issue link in `pr_issue_link`.

Per-project config convention-seeded like the k8s mapping (confirm once on star).

---

## TUI (`internal/tui`, Bubble Tea)

- Root model = ordered sections (Summary, My PRs, To Review, Releases, Dependencies) + Reflect tab.
- **Cache-first:** paint from SQLite immediately; fire `refreshGitHub` + `refreshK8s`
  as concurrent `tea.Cmd`s; each returns a message that patches its section in
  place. Per-source "⟳/stale" indicator. Releases never blocks launch.
- **Keys:** `j/k` move · `tab` expand muted · `enter` open in browser · `r` refresh ·
  `s` star/unstar · `a` approve · `m` merge · `t` tag/release · `d` deploy-url ·
  `A` AI review · `J` draft Jira · `R` reflect tab · `q` quit.
- Mutations (`m/a/t/J`-create) prompt a `y/n` confirm.

## Ambient (`internal/ambient`)

- `ship --count`: reads SQLite only (no network), prints `3 review · 1 ci✗ · 2 dep`.
- Spaceship async custom segment shells to `ship --count` (reads cache; non-blocking).

---

## Milestones (thin end-to-end, one real prod service first)

| # | Deliverable | Proves |
|---|---|---|
| **M0** | Scaffold: module, justfile, `.envrc`, config load, SQLite init, `gh` token wired | plumbing |
| **M1** | GitHub read → cache → plain stdout dump of My PRs + review-requested | GraphQL + cache |
| **M2** | **Releases spike for ONE configured service** — k8s read, version resolve, compare to tags/main, print `prod/pending/ahead` | **the whole thesis** |
| **M3** | TUI shell: render all cached sections, navigate, open-in-browser, `r` refresh | UX skeleton |
| **M4** | Relevance: stars in TOML, focus/muted tiering, direct-vs-team split | signal quality |
| **M5** | Actions via `gh` + confirm: merge (incl. renovate), approve, rerun, tag/release, deploy-url | the hub verbs |
| **M6** | Async per-source refresh + stale indicators + VPN-down graceful degrade | robustness |
| **M7** | Reflect section: contributions + merged-PR counts, 3 windows, cached | reflection |
| **M8** | AI review action: `gh` diff → `claude -p` (stream, structured) → sorted concerns | AI review |
| **M9** | `ship --count` + spaceship segment | ambient |
| **M10** | Jira create flow: context → `claude -p` draft → TUI review/edit → confirm → create (MCP-via-`claude`) → link back | ticketing |

**M2 is deliberately second** — riskiest, most-differentiated. If the
deployed-artifact → git-commit chain doesn't resolve for the pilot service, learn
it before building the TUI around it.

## Later (post-v1)

- Star suggestions from GitHub interaction signal.
- Daemon + OS notifications (launchd).
- "Deep review" agentic mode (claude reads surrounding files).
- SDK-based AI path (`review_provider = "api"`) if headless `claude` proves limiting.
- Broader deploy integration beyond opening a URL.
