package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "ship", "cache.db")
}

func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite3", path+"?_journal=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS pr (
			number      INTEGER,
			repo        TEXT,
			title       TEXT,
			author      TEXT,
			role        TEXT,
			url         TEXT,
			review_decision TEXT,
			ci_state    TEXT,
			mergeable   TEXT,
			updated_at  TEXT,
			PRIMARY KEY (repo, number)
		);

		CREATE TABLE IF NOT EXISTS version (
			repo          TEXT PRIMARY KEY,
			prod_ref      TEXT,
			prod_sha      TEXT,
			ahead_by      INTEGER,
			pending_tags  TEXT,
			resolved_at   TEXT,
			error         TEXT
		);

		CREATE TABLE IF NOT EXISTS reflection (
			window     TEXT,
			commits    INTEGER,
			prs_opened INTEGER,
			prs_merged INTEGER,
			reviews    INTEGER,
			fetched_at TEXT,
			PRIMARY KEY (window)
		);

		CREATE TABLE IF NOT EXISTS pr_issue_link (
			pr_url    TEXT PRIMARY KEY,
			jira_key  TEXT
		);

		CREATE TABLE IF NOT EXISTS refresh (
			source      TEXT PRIMARY KEY,
			last_ok     TEXT,
			last_attempt TEXT,
			status      TEXT
		);
	`)
	if err != nil {
		return err
	}

	// migrate: add is_draft column if missing
	var hasDraft bool
	err = s.db.QueryRow(`SELECT COUNT(*) > 0 FROM pragma_table_info('pr') WHERE name = 'is_draft'`).Scan(&hasDraft)
	if err == nil && !hasDraft {
		s.db.Exec(`ALTER TABLE pr ADD COLUMN is_draft INTEGER NOT NULL DEFAULT 0`)
	}

	return nil
}

type CachedPR struct {
	Number         int
	Repo           string
	Title          string
	Author         string
	Role           string
	URL            string
	ReviewDecision string
	CIState        string
	Mergeable      string
	UpdatedAt      string
	IsDraft        bool
}

func (s *Store) SavePRs(prs []CachedPR, role string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// clear old PRs for this role
	if _, err := tx.Exec(`DELETE FROM pr WHERE role = ?`, role); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO pr
		(number, repo, title, author, role, url, review_decision, ci_state, mergeable, updated_at, is_draft)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, p := range prs {
		if _, err := stmt.Exec(p.Number, p.Repo, p.Title, p.Author, p.Role,
			p.URL, p.ReviewDecision, p.CIState, p.Mergeable, p.UpdatedAt, p.IsDraft); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CachedPRs(role string) ([]CachedPR, error) {
	var where string
	var args []any
	if role != "" {
		where = "WHERE role = ?"
		args = append(args, role)
	}
	rows, err := s.db.Query(`SELECT number, repo, title, author, role, url, review_decision, ci_state, mergeable, updated_at, is_draft FROM pr `+where+` ORDER BY updated_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CachedPR
	for rows.Next() {
		var p CachedPR
		if err := rows.Scan(&p.Number, &p.Repo, &p.Title, &p.Author, &p.Role,
			&p.URL, &p.ReviewDecision, &p.CIState, &p.Mergeable, &p.UpdatedAt, &p.IsDraft); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Store) UpdateRefresh(source, status string) error {
	_, err := s.db.Exec(`INSERT INTO refresh (source, last_ok, last_attempt, status)
		VALUES (?, datetime('now'), datetime('now'), ?)
		ON CONFLICT(source) DO UPDATE SET
			last_ok = CASE WHEN ? = 'ok' THEN datetime('now') ELSE refresh.last_ok END,
			last_attempt = datetime('now'),
			status = ?`,
		source, status, status, status)
	return err
}

func (s *Store) RefreshStatus(source string) (lastOk, status string, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(last_ok, ''), COALESCE(status, '') FROM refresh WHERE source = ?`, source).Scan(&lastOk, &status)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return
}

func (s *Store) Query(query string, args ...any) (*sql.Rows, error) {
	return s.db.Query(query, args...)
}

type CachedVersion struct {
	Repo        string
	ProdRef     string
	ProdSHA     string
	AheadBy     int
	PendingTags string
	ResolvedAt  string
	Error       string
}

func (s *Store) SaveVersion(v CachedVersion) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO version (repo, prod_ref, prod_sha, ahead_by, pending_tags, resolved_at, error)
		VALUES (?, ?, ?, ?, ?, datetime('now'), ?)`,
		v.Repo, v.ProdRef, v.ProdSHA, v.AheadBy, v.PendingTags, v.Error)
	return err
}

func (s *Store) CachedVersions() ([]CachedVersion, error) {
	rows, err := s.db.Query(`SELECT repo, prod_ref, prod_sha, ahead_by, COALESCE(pending_tags, ''), COALESCE(resolved_at, ''), COALESCE(error, '') FROM version ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CachedVersion
	for rows.Next() {
		var v CachedVersion
		if err := rows.Scan(&v.Repo, &v.ProdRef, &v.ProdSHA, &v.AheadBy, &v.PendingTags, &v.ResolvedAt, &v.Error); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
