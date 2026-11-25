package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

// StateParts holds per-category semantic digests for identifying what changed.
type StateParts struct {
	Repos     string `json:"repos,omitempty"`
	Snapshots string `json:"snapshots,omitempty"`
	Comments  string `json:"comments,omitempty"`
	Marks     string `json:"marks,omitempty"`
}

type stateDigestKey struct {
	repoID    int64
	changeKey string
}

type cachedStateDigest struct {
	digest string
	parts  StateParts
}

// DB wraps the SQLite connection for review metadata.
type DB struct {
	db               *sql.DB
	path             string
	digestMu         sync.Mutex
	digestValid      bool
	lastTotalChanges int64
	lastDataVersion  int64
	digestCache      map[stateDigestKey]cachedStateDigest
}

// DefaultPath returns the default SQLite database path in ~/.config/review/review.db
// or $XDG_CONFIG_HOME/review/review.db.
func DefaultPath() string {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "review.db"
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "review", "review.db")
}

// Open initializes or migrates the review database at path.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite db: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	d := &DB{db: sqlDB, path: path}
	if err := d.initSchema(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("initializing db schema: %w", err)
	}

	return d, nil
}

func (d *DB) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS repos (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		slug TEXT UNIQUE NOT NULL,
		root_path TEXT UNIQUE NOT NULL,
		created_at DATETIME NOT NULL
	);
	CREATE TABLE IF NOT EXISTS prefs (
		key TEXT PRIMARY KEY,
		val TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS snapshots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
		change_key TEXT NOT NULL,
		num INTEGER NOT NULL,
		rev TEXT NOT NULL,
		parent_rev TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		UNIQUE(repo_id, change_key, num)
	);
	CREATE TABLE IF NOT EXISTS threads (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
		change_key TEXT NOT NULL,
		rev TEXT NOT NULL,
		snapshot_num INTEGER NOT NULL DEFAULT 0,
		path TEXT NOT NULL,
		side TEXT NOT NULL DEFAULT 'R',
		start_line INTEGER NOT NULL,
		end_line INTEGER NOT NULL,
		start_col INTEGER NOT NULL DEFAULT 0,
		end_col INTEGER NOT NULL DEFAULT 0,
		resolved INTEGER NOT NULL DEFAULT 0,
		anchor_lines TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL
	);
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		thread_id INTEGER NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
		author TEXT NOT NULL,
		body TEXT NOT NULL,
		draft INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);
	CREATE TABLE IF NOT EXISTS file_marks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
		change_key TEXT NOT NULL,
		rev TEXT NOT NULL,
		path TEXT NOT NULL,
		reviewed INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME NOT NULL,
		UNIQUE(repo_id, change_key, rev, path)
	);
	CREATE TABLE IF NOT EXISTS rev_marks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
		change_key TEXT NOT NULL,
		rev TEXT NOT NULL,
		reviewed INTEGER NOT NULL DEFAULT 0,
		lgtm INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME NOT NULL,
		UNIQUE(repo_id, change_key, rev)
	);
	CREATE TABLE IF NOT EXISTS form_drafts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
		change_key TEXT NOT NULL,
		draft_key TEXT NOT NULL,
		target_path TEXT NOT NULL DEFAULT '',
		side TEXT NOT NULL DEFAULT '',
		line INTEGER NOT NULL DEFAULT 0,
		end_line INTEGER NOT NULL DEFAULT 0,
		start_col INTEGER NOT NULL DEFAULT 0,
		end_col INTEGER NOT NULL DEFAULT 0,
		unresolved INTEGER NOT NULL DEFAULT 1,
		thread_id TEXT NOT NULL DEFAULT '',
		message_id TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL DEFAULT '',
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(repo_id, change_key, draft_key)
	);

	CREATE INDEX IF NOT EXISTS idx_threads_lookup ON threads(repo_id, change_key, rev);
	CREATE INDEX IF NOT EXISTS idx_threads_path ON threads(repo_id, change_key, path, start_line);
	CREATE INDEX IF NOT EXISTS idx_messages_thread ON messages(thread_id);
	`

	if _, err := d.db.Exec(schema); err != nil {
		return err
	}
	return nil
}

// Close closes the underlying SQLite database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// Path returns the filesystem path of the database.
func (d *DB) Path() string {
	return d.path
}

// Pref gets a user preference value or returns fallback if unset.
func (d *DB) Pref(key, fallback string) string {
	var val string
	err := d.db.QueryRow("SELECT val FROM prefs WHERE key = ?", key).Scan(&val)
	if err != nil {
		return fallback
	}
	return val
}

// SetPref stores a user preference value without invalidating the cached StateDigest.
func (d *DB) SetPref(key, val string) error {
	d.digestMu.Lock()
	defer d.digestMu.Unlock()

	var beforeTC int64
	beforeErr := d.db.QueryRow("SELECT total_changes()").Scan(&beforeTC)

	_, err := d.db.Exec("INSERT INTO prefs (key, val) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET val = excluded.val", key, val)
	if err != nil {
		return err
	}
	if d.digestValid && beforeErr == nil && beforeTC == d.lastTotalChanges {
		var afterTC int64
		if err := d.db.QueryRow("SELECT total_changes()").Scan(&afterTC); err == nil {
			d.lastTotalChanges = afterTC
		}
	}
	return nil
}

func shortSHA(sum [32]byte) string {
	return hex.EncodeToString(sum[:])[:16]
}

// StateDigest computes a SHA-256 hex digest of the semantic review DB state.
func (d *DB) StateDigest() string {
	dig, _ := d.StateDigestFor(0, "")
	return dig
}

// StateDigestFor computes a semantic DB digest and per-category sub-digests scoped to
// the entire DB (repoID == 0), a single repository (repoID > 0, changeKey == ""), or
// a single change (repoID > 0, changeKey != "").
func (d *DB) StateDigestFor(repoID int64, changeKey string) (string, StateParts) {
	var totalChanges, dataVersion int64
	tcErr := d.db.QueryRow("SELECT total_changes()").Scan(&totalChanges)
	dvErr := d.db.QueryRow("PRAGMA data_version").Scan(&dataVersion)

	key := stateDigestKey{repoID: repoID, changeKey: changeKey}
	d.digestMu.Lock()
	if tcErr == nil && dvErr == nil && d.digestValid &&
		totalChanges == d.lastTotalChanges && dataVersion == d.lastDataVersion {
		if entry, ok := d.digestCache[key]; ok {
			d.digestMu.Unlock()
			return entry.digest, entry.parts
		}
	}
	d.digestMu.Unlock()

	dig, parts, ok := d.computeStateDigest(repoID, changeKey)
	if ok && tcErr == nil && dvErr == nil {
		var afterTC, afterDV int64
		if d.db.QueryRow("SELECT total_changes()").Scan(&afterTC) == nil &&
			d.db.QueryRow("PRAGMA data_version").Scan(&afterDV) == nil &&
			afterTC == totalChanges && afterDV == dataVersion {
			d.digestMu.Lock()
			newer := !d.digestValid || totalChanges > d.lastTotalChanges || dataVersion > d.lastDataVersion
			same := d.digestValid && totalChanges == d.lastTotalChanges && dataVersion == d.lastDataVersion
			if newer {
				d.digestCache = make(map[stateDigestKey]cachedStateDigest)
				d.lastTotalChanges = totalChanges
				d.lastDataVersion = dataVersion
				d.digestValid = true
				d.digestCache[key] = cachedStateDigest{digest: dig, parts: parts}
			} else if same {
				if len(d.digestCache) >= 256 {
					d.digestCache = make(map[stateDigestKey]cachedStateDigest)
				}
				d.digestCache[key] = cachedStateDigest{digest: dig, parts: parts}
			}
			d.digestMu.Unlock()
		}
	}
	return dig, parts
}

func (d *DB) computeStateDigest(repoID int64, changeKey string) (string, StateParts, bool) {
	hRepos := sha256.New()
	hSnaps := sha256.New()
	hComments := sha256.New()
	hMarks := sha256.New()
	queryFailed := false

	// 1. repos
	if changeKey == "" {
		var rows *sql.Rows
		var err error
		if repoID == 0 {
			rows, err = d.db.Query("SELECT id, slug, root_path FROM repos ORDER BY id")
		} else {
			rows, err = d.db.Query("SELECT id, slug, root_path FROM repos WHERE id = ? ORDER BY id", repoID)
		}
		if err == nil {
			for rows.Next() {
				var id int64
				var slug, rootPath string
				if err := rows.Scan(&id, &slug, &rootPath); err == nil {
					fmt.Fprintf(hRepos, "repo:%d:%s:%s\n", id, slug, rootPath)
				}
			}
			if rows.Err() != nil {
				queryFailed = true
			}
			rows.Close()
		} else {
			queryFailed = true
		}
	}

	// 2. snapshots
	{
		var rows *sql.Rows
		var err error
		switch {
		case repoID == 0:
			rows, err = d.db.Query("SELECT id, repo_id, change_key, num, rev, parent_rev FROM snapshots ORDER BY id")
		case changeKey == "":
			rows, err = d.db.Query("SELECT id, repo_id, change_key, num, rev, parent_rev FROM snapshots WHERE repo_id = ? ORDER BY id", repoID)
		default:
			rows, err = d.db.Query("SELECT id, repo_id, change_key, num, rev, parent_rev FROM snapshots WHERE repo_id = ? AND change_key = ? ORDER BY id", repoID, changeKey)
		}
		if err == nil {
			for rows.Next() {
				var id, rID int64
				var ck string
				var num int
				var rev, parentRev string
				if err := rows.Scan(&id, &rID, &ck, &num, &rev, &parentRev); err == nil {
					fmt.Fprintf(hSnaps, "snap:%d:%d:%s:%d:%s:%s\n", id, rID, ck, num, rev, parentRev)
				}
			}
			if rows.Err() != nil {
				queryFailed = true
			}
			rows.Close()
		} else {
			queryFailed = true
		}
	}

	// 3. threads
	{
		var rows *sql.Rows
		var err error
		switch {
		case repoID == 0:
			rows, err = d.db.Query("SELECT id, repo_id, change_key, rev, snapshot_num, path, side, start_line, end_line, start_col, end_col, resolved FROM threads ORDER BY id")
		case changeKey == "":
			rows, err = d.db.Query("SELECT id, repo_id, change_key, rev, snapshot_num, path, side, start_line, end_line, start_col, end_col, resolved FROM threads WHERE repo_id = ? ORDER BY id", repoID)
		default:
			rows, err = d.db.Query("SELECT id, repo_id, change_key, rev, snapshot_num, path, side, start_line, end_line, start_col, end_col, resolved FROM threads WHERE repo_id = ? AND change_key = ? ORDER BY id", repoID, changeKey)
		}
		if err == nil {
			for rows.Next() {
				var id, rID int64
				var ck, rev string
				var snapNum int
				var path, side string
				var startLine, endLine int
				var startCol, endCol int
				var resolved bool
				if err := rows.Scan(&id, &rID, &ck, &rev, &snapNum, &path, &side, &startLine, &endLine, &startCol, &endCol, &resolved); err == nil {
					fmt.Fprintf(hComments, "thread:%d:%d:%s:%s:%d:%s:%s:%d:%d:%d:%d:%t\n", id, rID, ck, rev, snapNum, path, side, startLine, endLine, startCol, endCol, resolved)
				}
			}
			if rows.Err() != nil {
				queryFailed = true
			}
			rows.Close()
		} else {
			queryFailed = true
		}
	}

	// 4. messages
	{
		var rows *sql.Rows
		var err error
		switch {
		case repoID == 0:
			rows, err = d.db.Query("SELECT id, thread_id, author, draft, body FROM messages ORDER BY id")
		case changeKey == "":
			rows, err = d.db.Query("SELECT m.id, m.thread_id, m.author, m.draft, m.body FROM messages m JOIN threads t ON m.thread_id = t.id WHERE t.repo_id = ? ORDER BY m.id", repoID)
		default:
			rows, err = d.db.Query("SELECT m.id, m.thread_id, m.author, m.draft, m.body FROM messages m JOIN threads t ON m.thread_id = t.id WHERE t.repo_id = ? AND t.change_key = ? ORDER BY m.id", repoID, changeKey)
		}
		if err == nil {
			for rows.Next() {
				var id, threadID int64
				var author string
				var draft bool
				var body string
				if err := rows.Scan(&id, &threadID, &author, &draft, &body); err == nil {
					fmt.Fprintf(hComments, "msg:%d:%d:%s:%t:%s\n", id, threadID, author, draft, body)
				}
			}
			if rows.Err() != nil {
				queryFailed = true
			}
			rows.Close()
		} else {
			queryFailed = true
		}
	}

	// 5. file_marks
	{
		var rows *sql.Rows
		var err error
		switch {
		case repoID == 0:
			rows, err = d.db.Query("SELECT repo_id, change_key, rev, path FROM file_marks WHERE reviewed = 1 ORDER BY repo_id, change_key, rev, path")
		case changeKey == "":
			rows, err = d.db.Query("SELECT repo_id, change_key, rev, path FROM file_marks WHERE repo_id = ? AND reviewed = 1 ORDER BY change_key, rev, path", repoID)
		default:
			rows, err = d.db.Query("SELECT repo_id, change_key, rev, path FROM file_marks WHERE repo_id = ? AND change_key = ? AND reviewed = 1 ORDER BY rev, path", repoID, changeKey)
		}
		if err == nil {
			for rows.Next() {
				var rID int64
				var ck, rev, path string
				if err := rows.Scan(&rID, &ck, &rev, &path); err == nil {
					fmt.Fprintf(hMarks, "file_mark:%d:%s:%s:%s\n", rID, ck, rev, path)
				}
			}
			if rows.Err() != nil {
				queryFailed = true
			}
			rows.Close()
		} else {
			queryFailed = true
		}
	}

	// 6. rev_marks
	{
		var rows *sql.Rows
		var err error
		switch {
		case repoID == 0:
			rows, err = d.db.Query("SELECT repo_id, change_key, rev, reviewed, lgtm FROM rev_marks WHERE reviewed = 1 OR lgtm = 1 ORDER BY repo_id, change_key, rev")
		case changeKey == "":
			rows, err = d.db.Query("SELECT repo_id, change_key, rev, reviewed, lgtm FROM rev_marks WHERE repo_id = ? AND (reviewed = 1 OR lgtm = 1) ORDER BY change_key, rev", repoID)
		default:
			rows, err = d.db.Query("SELECT repo_id, change_key, rev, reviewed, lgtm FROM rev_marks WHERE repo_id = ? AND change_key = ? AND (reviewed = 1 OR lgtm = 1) ORDER BY rev", repoID, changeKey)
		}
		if err == nil {
			for rows.Next() {
				var rID int64
				var ck, rev string
				var reviewed, lgtm bool
				if err := rows.Scan(&rID, &ck, &rev, &reviewed, &lgtm); err == nil {
					fmt.Fprintf(hMarks, "rev_mark:%d:%s:%s:%t:%t\n", rID, ck, rev, reviewed, lgtm)
				}
			}
			if rows.Err() != nil {
				queryFailed = true
			}
			rows.Close()
		} else {
			queryFailed = true
		}
	}

	parts := StateParts{
		Snapshots: shortSHA([32]byte(hSnaps.Sum(nil))),
		Comments:  shortSHA([32]byte(hComments.Sum(nil))),
		Marks:     shortSHA([32]byte(hMarks.Sum(nil))),
	}
	if changeKey == "" {
		parts.Repos = shortSHA([32]byte(hRepos.Sum(nil)))
	}

	hAll := sha256.New()
	fmt.Fprintf(hAll, "%s:%s:%s:%s", parts.Repos, parts.Snapshots, parts.Comments, parts.Marks)
	return hex.EncodeToString(hAll.Sum(nil)), parts, !queryFailed
}
