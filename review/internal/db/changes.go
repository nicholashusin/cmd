package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"nicholas.sh/cmd/review/internal/vcs"
)

// Snapshot represents a recorded snapshot of a change revision.
type Snapshot struct {
	ID        int64     `json:"id"`
	RepoID    int64     `json:"repo_id"`
	ChangeKey string    `json:"change_key"`
	Num       int       `json:"num"`
	Rev       string    `json:"rev"`
	ParentRev string    `json:"parent_rev"`
	CreatedAt time.Time `json:"created_at"`
}

// Thread represents a comment thread anchored to a file line or range.
type Thread struct {
	ID          int64      `json:"id"`
	RepoID      int64      `json:"repo_id"`
	ChangeKey   string     `json:"change_key"`
	Rev         string     `json:"rev"`
	SnapshotNum int        `json:"snapshot_num"`
	Path        string     `json:"path"`
	Side        string     `json:"side"` // "L" or "R"
	StartLine   int        `json:"start_line"`
	EndLine     int        `json:"end_line"`
	StartCol    int        `json:"start_col,omitempty"`
	EndCol      int        `json:"end_col,omitempty"`
	Resolved    bool       `json:"resolved"`
	AnchorLines string     `json:"anchor_lines"`
	CreatedAt   time.Time  `json:"created_at"`
	Messages    []*Message `json:"messages,omitempty"`
	Stale       bool       `json:"stale,omitempty"`
}

// Message represents a single comment or reply inside a thread.
type Message struct {
	ID        int64     `json:"id"`
	ThreadID  int64     `json:"thread_id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	Draft     bool      `json:"draft"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ChangeSummary summarizes change review status, snapshot count, and thread metrics.
type ChangeSummary struct {
	ChangeKey       string    `json:"change_key"`
	SnapshotCount   int       `json:"snapshot_count"`
	LatestSnapshot  *Snapshot `json:"latest_snapshot,omitempty"`
	Reviewed        bool      `json:"reviewed"`
	LGTM            bool      `json:"lgtm"`
	OpenThreads     int       `json:"open_threads"`
	ResolvedThreads int       `json:"resolved_threads"`
	DraftCount      int       `json:"draft_count"`
}

// TakeSnapshot records a new snapshot for change c if c.Rev has not yet been snapshotted.
// Whether newly created or matched to an existing snapshot on c.Rev, it promotes any
// snapshot_num = 0 threads on this revision or parent (for side = 'L') to that snapshot number.
func (d *DB) TakeSnapshot(repoID int64, c *vcs.Change) (*Snapshot, bool, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var existing Snapshot
	err = tx.QueryRow(`
		SELECT id, repo_id, change_key, num, rev, parent_rev, created_at
		FROM snapshots
		WHERE repo_id = ? AND change_key = ? AND rev = ?
		ORDER BY num DESC LIMIT 1
	`, repoID, c.Key, c.Rev).Scan(&existing.ID, &existing.RepoID, &existing.ChangeKey, &existing.Num, &existing.Rev, &existing.ParentRev, &existing.CreatedAt)
	if err == nil {
		if _, err := tx.Exec(`
			UPDATE threads
			SET snapshot_num = ?
			WHERE repo_id = ? AND change_key = ? AND snapshot_num = 0 AND (rev = ? OR (side = 'L' AND rev = ?))
		`, existing.Num, repoID, c.Key, c.Rev, c.Parent); err != nil {
			return nil, false, fmt.Errorf("upgrading thread snapshot nums: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return &existing, false, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}

	var maxNum int
	if err := tx.QueryRow(`
		SELECT COALESCE(MAX(num), 0)
		FROM snapshots
		WHERE repo_id = ? AND change_key = ?
	`, repoID, c.Key).Scan(&maxNum); err != nil {
		return nil, false, err
	}
	nextNum := maxNum + 1

	now := time.Now().UTC()
	res, err := tx.Exec(`
		INSERT INTO snapshots (repo_id, change_key, num, rev, parent_rev, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, repoID, c.Key, nextNum, c.Rev, c.Parent, now)
	if err != nil {
		return nil, false, fmt.Errorf("inserting snapshot: %w", err)
	}

	snapID, err := res.LastInsertId()
	if err != nil {
		return nil, false, err
	}

	// Upgrade pending threads on this revision from snapshot_num = 0 to nextNum
	_, err = tx.Exec(`
		UPDATE threads
		SET snapshot_num = ?
		WHERE repo_id = ? AND change_key = ? AND snapshot_num = 0 AND (rev = ? OR (side = 'L' AND rev = ?))
	`, nextNum, repoID, c.Key, c.Rev, c.Parent)
	if err != nil {
		return nil, false, fmt.Errorf("upgrading thread snapshot nums: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, false, err
	}

	return &Snapshot{
		ID:        snapID,
		RepoID:    repoID,
		ChangeKey: c.Key,
		Num:       nextNum,
		Rev:       c.Rev,
		ParentRev: c.Parent,
		CreatedAt: now,
	}, true, nil
}

// Snapshots lists all snapshots for a change ordered by snapshot number ASC.
func (d *DB) Snapshots(repoID int64, changeKey string) ([]*Snapshot, error) {
	rows, err := d.db.Query(`
		SELECT id, repo_id, change_key, num, rev, parent_rev, created_at
		FROM snapshots
		WHERE repo_id = ? AND change_key = ?
		ORDER BY num ASC
	`, repoID, changeKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []*Snapshot
	for rows.Next() {
		var s Snapshot
		if err := rows.Scan(&s.ID, &s.RepoID, &s.ChangeKey, &s.Num, &s.Rev, &s.ParentRev, &s.CreatedAt); err != nil {
			return nil, err
		}
		snaps = append(snaps, &s)
	}
	return snaps, rows.Err()
}

// SnapshotByNum finds a snapshot by its 1-based number.
func (d *DB) SnapshotByNum(repoID int64, changeKey string, num int) (*Snapshot, error) {
	var s Snapshot
	err := d.db.QueryRow(`
		SELECT id, repo_id, change_key, num, rev, parent_rev, created_at
		FROM snapshots
		WHERE repo_id = ? AND change_key = ? AND num = ?
	`, repoID, changeKey, num).Scan(&s.ID, &s.RepoID, &s.ChangeKey, &s.Num, &s.Rev, &s.ParentRev, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// AddThreadRange creates a new thread with character column range and its initial message in an atomic transaction.
func (d *DB) AddThreadRange(repoID int64, changeKey, rev string, snapNum int, path, side string, startLine, endLine, startCol, endCol int, anchorLines, author, body string, draft bool) (*Thread, *Message, error) {
	if startCol < 0 {
		startCol = 0
	}
	if endCol < 0 {
		endCol = 0
	}
	tx, err := d.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	draftVal := 0
	if draft {
		draftVal = 1
	}

	sideVal := side
	if sideVal == "" {
		sideVal = "R"
	}

	tRes, err := tx.Exec(`
		INSERT INTO threads (repo_id, change_key, rev, snapshot_num, path, side, start_line, end_line, start_col, end_col, resolved, anchor_lines, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
	`, repoID, changeKey, rev, snapNum, path, sideVal, startLine, endLine, startCol, endCol, anchorLines, now)
	if err != nil {
		return nil, nil, fmt.Errorf("inserting thread: %w", err)
	}

	threadID, err := tRes.LastInsertId()
	if err != nil {
		return nil, nil, err
	}

	mRes, err := tx.Exec(`
		INSERT INTO messages (thread_id, author, body, draft, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, threadID, author, body, draftVal, now, now)
	if err != nil {
		return nil, nil, fmt.Errorf("inserting message: %w", err)
	}

	msgID, err := mRes.LastInsertId()
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}

	msg := &Message{
		ID:        msgID,
		ThreadID:  threadID,
		Author:    author,
		Body:      body,
		Draft:     draft,
		CreatedAt: now,
		UpdatedAt: now,
	}

	th := &Thread{
		ID:          threadID,
		RepoID:      repoID,
		ChangeKey:   changeKey,
		Rev:         rev,
		SnapshotNum: snapNum,
		Path:        path,
		Side:        sideVal,
		StartLine:   startLine,
		EndLine:     endLine,
		StartCol:    startCol,
		EndCol:      endCol,
		Resolved:    false,
		AnchorLines: anchorLines,
		CreatedAt:   now,
		Messages:    []*Message{msg},
	}

	return th, msg, nil
}

// AddThread creates a new whole-line thread and its initial message.
func (d *DB) AddThread(repoID int64, changeKey, rev string, snapNum int, path, side string, startLine, endLine int, anchorLines, author, body string, draft bool) (*Thread, *Message, error) {
	return d.AddThreadRange(repoID, changeKey, rev, snapNum, path, side, startLine, endLine, 0, 0, anchorLines, author, body, draft)
}

// AddMessage appends a reply message to an existing thread.
func (d *DB) AddMessage(threadID int64, author, body string, draft bool) (*Message, error) {
	now := time.Now().UTC()
	draftVal := 0
	if draft {
		draftVal = 1
	}

	res, err := d.db.Exec(`
		INSERT INTO messages (thread_id, author, body, draft, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, threadID, author, body, draftVal, now, now)
	if err != nil {
		return nil, fmt.Errorf("inserting message reply: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &Message{
		ID:        id,
		ThreadID:  threadID,
		Author:    author,
		Body:      body,
		Draft:     draft,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// EditMessage updates the body of a message.
func (d *DB) EditMessage(msgID int64, body string) (*Message, *Thread, error) {
	now := time.Now().UTC()
	var threadID int64
	err := d.db.QueryRow("SELECT thread_id FROM messages WHERE id = ?", msgID).Scan(&threadID)
	if err != nil {
		return nil, nil, fmt.Errorf("message %d not found: %w", msgID, err)
	}

	_, err = d.db.Exec("UPDATE messages SET body = ?, updated_at = ? WHERE id = ?", body, now, msgID)
	if err != nil {
		return nil, nil, fmt.Errorf("updating message: %w", err)
	}

	th, err := d.ThreadByID(threadID)
	if err != nil {
		return nil, nil, err
	}

	var updatedMsg *Message
	for _, m := range th.Messages {
		if m.ID == msgID {
			updatedMsg = m
			break
		}
	}

	return updatedMsg, th, nil
}

// DeleteDraftMessage deletes a draft message. If no messages remain in the parent thread,
// the thread is also deleted.
func (d *DB) DeleteDraftMessage(msgID int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var threadID int64
	var isDraft int
	err = tx.QueryRow("SELECT thread_id, draft FROM messages WHERE id = ?", msgID).Scan(&threadID, &isDraft)
	if err != nil {
		return fmt.Errorf("message %d not found: %w", msgID, err)
	}
	if isDraft != 1 {
		return fmt.Errorf("cannot delete published message %d", msgID)
	}

	if _, err := tx.Exec("DELETE FROM messages WHERE id = ?", msgID); err != nil {
		return fmt.Errorf("deleting message: %w", err)
	}

	var remaining int
	_ = tx.QueryRow("SELECT COUNT(*) FROM messages WHERE thread_id = ?", threadID).Scan(&remaining)
	if remaining == 0 {
		_, _ = tx.Exec("DELETE FROM threads WHERE id = ?", threadID)
	}

	return tx.Commit()
}

// SetResolved updates the resolution status of a thread.
func (d *DB) SetResolved(threadID int64, resolved bool) error {
	val := 0
	if resolved {
		val = 1
	}
	res, err := d.db.Exec("UPDATE threads SET resolved = ? WHERE id = ?", val, threadID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("thread %d not found", threadID)
	}
	return nil
}

// ThreadForMessage returns the Thread containing the message with the given ID.
func (d *DB) ThreadForMessage(msgID int64) (*Thread, error) {
	var threadID int64
	err := d.db.QueryRow("SELECT thread_id FROM messages WHERE id = ?", msgID).Scan(&threadID)
	if err != nil {
		return nil, err
	}
	return d.ThreadByID(threadID)
}

// ThreadByID retrieves a thread by ID including all its messages.
func (d *DB) ThreadByID(threadID int64) (*Thread, error) {
	var th Thread
	var resolvedInt int
	err := d.db.QueryRow(`
		SELECT id, repo_id, change_key, rev, snapshot_num, path, side, start_line, end_line, start_col, end_col, resolved, anchor_lines, created_at
		FROM threads WHERE id = ?
	`, threadID).Scan(&th.ID, &th.RepoID, &th.ChangeKey, &th.Rev, &th.SnapshotNum, &th.Path, &th.Side, &th.StartLine, &th.EndLine, &th.StartCol, &th.EndCol, &resolvedInt, &th.AnchorLines, &th.CreatedAt)
	if err != nil {
		return nil, err
	}
	th.Resolved = (resolvedInt == 1)

	rows, err := d.db.Query(`
		SELECT id, thread_id, author, body, draft, created_at, updated_at
		FROM messages WHERE thread_id = ? ORDER BY id ASC
	`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var m Message
		var draftInt int
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Author, &m.Body, &draftInt, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Draft = (draftInt == 1)
		th.Messages = append(th.Messages, &m)
	}

	return &th, rows.Err()
}

// ThreadsForChange retrieves all threads for a change, with messages populated.
func (d *DB) ThreadsForChange(repoID int64, changeKey string) ([]*Thread, error) {
	rows, err := d.db.Query(`
		SELECT id, repo_id, change_key, rev, snapshot_num, path, side, start_line, end_line, start_col, end_col, resolved, anchor_lines, created_at
		FROM threads
		WHERE repo_id = ? AND change_key = ?
		ORDER BY path ASC, start_line ASC, id ASC
	`, repoID, changeKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var threads []*Thread
	threadMap := make(map[int64]*Thread)

	for rows.Next() {
		var th Thread
		var resolvedInt int
		if err := rows.Scan(&th.ID, &th.RepoID, &th.ChangeKey, &th.Rev, &th.SnapshotNum, &th.Path, &th.Side, &th.StartLine, &th.EndLine, &th.StartCol, &th.EndCol, &resolvedInt, &th.AnchorLines, &th.CreatedAt); err != nil {
			return nil, err
		}
		th.Resolved = (resolvedInt == 1)
		threads = append(threads, &th)
		threadMap[th.ID] = &th
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(threads) == 0 {
		return nil, nil
	}

	// Fetch messages for all threads
	mRows, err := d.db.Query(`
		SELECT m.id, m.thread_id, m.author, m.body, m.draft, m.created_at, m.updated_at
		FROM messages m
		JOIN threads t ON m.thread_id = t.id
		WHERE t.repo_id = ? AND t.change_key = ?
		ORDER BY m.id ASC
	`, repoID, changeKey)
	if err != nil {
		return nil, err
	}
	defer mRows.Close()

	for mRows.Next() {
		var m Message
		var draftInt int
		if err := mRows.Scan(&m.ID, &m.ThreadID, &m.Author, &m.Body, &draftInt, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Draft = (draftInt == 1)
		if th, ok := threadMap[m.ThreadID]; ok {
			th.Messages = append(th.Messages, &m)
		}
	}
	if err := mRows.Err(); err != nil {
		return nil, err
	}

	return threads, nil
}

// ThreadsForChanges fetches all threads (with their messages) for multiple change keys in at most 2 SQL queries.
func (d *DB) ThreadsForChanges(repoID int64, changeKeys []string) (map[string][]*Thread, error) {
	result := make(map[string][]*Thread)
	if len(changeKeys) == 0 {
		return result, nil
	}
	for _, k := range changeKeys {
		result[k] = nil
	}

	placeholders := make([]string, len(changeKeys))
	args := make([]any, len(changeKeys)+1)
	args[0] = repoID
	for i, k := range changeKeys {
		placeholders[i] = "?"
		args[i+1] = k
	}
	inClause := strings.Join(placeholders, ",")

	rows, err := d.db.Query(`
		SELECT id, repo_id, change_key, rev, snapshot_num, path, side, start_line, end_line, start_col, end_col, resolved, anchor_lines, created_at
		FROM threads
		WHERE repo_id = ? AND change_key IN (`+inClause+`)
		ORDER BY start_line ASC, id ASC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	threadMap := make(map[int64]*Thread)
	var threadIDs []int64

	for rows.Next() {
		var th Thread
		var resolvedInt int
		if err := rows.Scan(&th.ID, &th.RepoID, &th.ChangeKey, &th.Rev, &th.SnapshotNum, &th.Path, &th.Side, &th.StartLine, &th.EndLine, &th.StartCol, &th.EndCol, &resolvedInt, &th.AnchorLines, &th.CreatedAt); err != nil {
			return nil, err
		}
		th.Resolved = (resolvedInt == 1)
		result[th.ChangeKey] = append(result[th.ChangeKey], &th)
		threadMap[th.ID] = &th
		threadIDs = append(threadIDs, th.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(threadIDs) == 0 {
		return result, nil
	}

	mPlaceholders := make([]string, len(threadIDs))
	mArgs := make([]any, len(threadIDs))
	for i, id := range threadIDs {
		mPlaceholders[i] = "?"
		mArgs[i] = id
	}
	mInClause := strings.Join(mPlaceholders, ",")

	mRows, err := d.db.Query(`
		SELECT id, thread_id, author, body, draft, created_at, updated_at
		FROM messages
		WHERE thread_id IN (`+mInClause+`)
		ORDER BY id ASC
	`, mArgs...)
	if err != nil {
		return nil, err
	}
	defer mRows.Close()

	for mRows.Next() {
		var m Message
		var draftInt int
		if err := mRows.Scan(&m.ID, &m.ThreadID, &m.Author, &m.Body, &draftInt, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Draft = (draftInt == 1)
		if th, ok := threadMap[m.ThreadID]; ok {
			th.Messages = append(th.Messages, &m)
		}
	}
	if err := mRows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

// PublishChange sets draft = 0 on all messages in threads for the given change
// and removes any orphaned in-progress edit form drafts for that change.
func (d *DB) PublishChange(repoID int64, changeKey string) (int64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		UPDATE messages
		SET draft = 0
		WHERE draft = 1 AND thread_id IN (
			SELECT id FROM threads WHERE repo_id = ? AND change_key = ?
		)
	`, repoID, changeKey)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`
		DELETE FROM form_drafts
		WHERE repo_id = ? AND change_key = ? AND message_id != ''
	`, repoID, changeKey); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// PublishRepo sets draft = 0 on all draft messages in the repository
// and removes any orphaned in-progress edit form drafts for that repository.
func (d *DB) PublishRepo(repoID int64) (int64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		UPDATE messages
		SET draft = 0
		WHERE draft = 1 AND thread_id IN (
			SELECT id FROM threads WHERE repo_id = ?
		)
	`, repoID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`
		DELETE FROM form_drafts
		WHERE repo_id = ? AND message_id != ''
	`, repoID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ChangeKeysWithDrafts returns distinct change keys in the repository that currently have draft messages.
func (d *DB) ChangeKeysWithDrafts(repoID int64) ([]string, error) {
	rows, err := d.db.Query(`
		SELECT t.change_key
		FROM threads t
		JOIN messages m ON m.thread_id = t.id
		WHERE t.repo_id = ? AND m.draft = 1
		GROUP BY t.change_key
		ORDER BY MIN(t.id) ASC
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// PromoteOrphanedZeroThreads assigns snapNum to any remaining snapshot_num = 0 threads
// on changeKey that contain draft messages (for example if their original revision is unreachable).
func (d *DB) PromoteOrphanedZeroThreads(repoID int64, changeKey string, snapNum int) error {
	if snapNum <= 0 {
		return nil
	}
	_, err := d.db.Exec(`
		UPDATE threads
		SET snapshot_num = ?
		WHERE repo_id = ? AND change_key = ? AND snapshot_num = 0 AND EXISTS (
			SELECT 1 FROM messages m WHERE m.thread_id = threads.id AND m.draft = 1
		)
	`, snapNum, repoID, changeKey)
	return err
}

// SetFileReviewed marks a file as reviewed or unreviewed for a specific revision.
func (d *DB) SetFileReviewed(repoID int64, changeKey, rev, path string, reviewed bool) error {
	val := 0
	if reviewed {
		val = 1
	}
	now := time.Now().UTC()
	_, err := d.db.Exec(`
		INSERT INTO file_marks (repo_id, change_key, rev, path, reviewed, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, change_key, rev, path) DO UPDATE
		SET reviewed = excluded.reviewed, updated_at = excluded.updated_at
	`, repoID, changeKey, rev, path, val, now)
	return err
}

// SetFileReviewedBatch sets reviewed status for multiple files in a single transaction.
func (d *DB) SetFileReviewedBatch(repoID int64, changeKey, rev string, paths []string, reviewed bool) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	val := 0
	if reviewed {
		val = 1
	}
	now := time.Now().UTC()

	stmt, err := tx.Prepare(`
		INSERT INTO file_marks (repo_id, change_key, rev, path, reviewed, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, change_key, rev, path) DO UPDATE
		SET reviewed = excluded.reviewed, updated_at = excluded.updated_at
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, p := range paths {
		if _, err := stmt.Exec(repoID, changeKey, rev, p, val, now); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// FileMarks returns a map of path -> isReviewed for the given revision.
func (d *DB) FileMarks(repoID int64, changeKey, rev string) (map[string]bool, error) {
	rows, err := d.db.Query(`
		SELECT path, reviewed FROM file_marks
		WHERE repo_id = ? AND change_key = ? AND rev = ?
	`, repoID, changeKey, rev)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make(map[string]bool)
	for rows.Next() {
		var path string
		var revInt int
		if err := rows.Scan(&path, &revInt); err != nil {
			return nil, err
		}
		res[path] = (revInt == 1)
	}
	return res, rows.Err()
}

// SetRevMark updates the reviewed or LGTM mark for a revision.
func (d *DB) SetRevMark(repoID int64, changeKey, rev string, reviewed, lgtm *bool) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()

	var curReviewed, curLGTM int
	err = tx.QueryRow(`
		SELECT reviewed, lgtm FROM rev_marks
		WHERE repo_id = ? AND change_key = ? AND rev = ?
	`, repoID, changeKey, rev).Scan(&curReviewed, &curLGTM)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	if reviewed != nil {
		if *reviewed {
			curReviewed = 1
		} else {
			curReviewed = 0
		}
	}
	if lgtm != nil {
		if *lgtm {
			curLGTM = 1
		} else {
			curLGTM = 0
			_, _ = tx.Exec(`UPDATE rev_marks SET lgtm = 0, updated_at = ? WHERE repo_id = ? AND change_key = ?`, now, repoID, changeKey)
		}
	}

	_, err = tx.Exec(`
		INSERT INTO rev_marks (repo_id, change_key, rev, reviewed, lgtm, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, change_key, rev) DO UPDATE
		SET reviewed = excluded.reviewed, lgtm = excluded.lgtm, updated_at = excluded.updated_at
	`, repoID, changeKey, rev, curReviewed, curLGTM, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RevMark retrieves the reviewed and LGTM status for a revision.
func (d *DB) RevMark(repoID int64, changeKey, rev string) (reviewed bool, lgtm bool, err error) {
	var rInt, lInt int
	err = d.db.QueryRow(`
		SELECT reviewed, lgtm FROM rev_marks
		WHERE repo_id = ? AND change_key = ? AND rev = ?
	`, repoID, changeKey, rev).Scan(&rInt, &lInt)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return rInt == 1, lInt == 1, nil
}

// RepoSummaries computes summary statistics for all changes in repoID using 4 efficient batched queries.
func (d *DB) RepoSummaries(repoID int64) (map[string]*ChangeSummary, error) {
	summaries := make(map[string]*ChangeSummary)

	getSummary := func(key string) *ChangeSummary {
		if s, ok := summaries[key]; ok {
			return s
		}
		s := &ChangeSummary{ChangeKey: key}
		summaries[key] = s
		return s
	}

	// 1. Snapshot counts and latest snapshot
	sRows, err := d.db.Query(`
		SELECT s.id, s.repo_id, s.change_key, s.num, s.rev, s.parent_rev, s.created_at
		FROM snapshots s
		WHERE s.repo_id = ?
		ORDER BY s.change_key ASC, s.num DESC
	`, repoID)
	if err != nil {
		return nil, fmt.Errorf("loading snapshot summaries: %w", err)
	}
	defer sRows.Close()

	for sRows.Next() {
		var s Snapshot
		if err := sRows.Scan(&s.ID, &s.RepoID, &s.ChangeKey, &s.Num, &s.Rev, &s.ParentRev, &s.CreatedAt); err != nil {
			return nil, err
		}
		sum := getSummary(s.ChangeKey)
		sum.SnapshotCount++
		if sum.LatestSnapshot == nil {
			sum.LatestSnapshot = &s
		}
	}
	if err := sRows.Err(); err != nil {
		return nil, err
	}

	// 2. Rev marks (reviewed, lgtm)
	mRows, err := d.db.Query(`
		SELECT change_key, reviewed, lgtm
		FROM rev_marks
		WHERE repo_id = ?
	`, repoID)
	if err != nil {
		return nil, fmt.Errorf("loading rev mark summaries: %w", err)
	}
	defer mRows.Close()

	for mRows.Next() {
		var key string
		var rInt, lInt int
		if err := mRows.Scan(&key, &rInt, &lInt); err != nil {
			return nil, err
		}
		sum := getSummary(key)
		if rInt == 1 {
			sum.Reviewed = true
		}
		if lInt == 1 {
			sum.LGTM = true
		}
	}
	if err := mRows.Err(); err != nil {
		return nil, err
	}

	// 3. Thread counts (open vs resolved)
	tRows, err := d.db.Query(`
		SELECT change_key, resolved, COUNT(*)
		FROM threads
		WHERE repo_id = ?
		GROUP BY change_key, resolved
	`, repoID)
	if err != nil {
		return nil, fmt.Errorf("loading thread summaries: %w", err)
	}
	defer tRows.Close()

	for tRows.Next() {
		var key string
		var resolvedInt, count int
		if err := tRows.Scan(&key, &resolvedInt, &count); err != nil {
			return nil, err
		}
		sum := getSummary(key)
		if resolvedInt == 1 {
			sum.ResolvedThreads += count
		} else {
			sum.OpenThreads += count
		}
	}
	if err := tRows.Err(); err != nil {
		return nil, err
	}

	// 4. Draft message counts
	dRows, err := d.db.Query(`
		SELECT t.change_key, COUNT(*)
		FROM messages m
		JOIN threads t ON m.thread_id = t.id
		WHERE t.repo_id = ? AND m.draft = 1
		GROUP BY t.change_key
	`, repoID)
	if err != nil {
		return nil, fmt.Errorf("loading draft summaries: %w", err)
	}
	defer dRows.Close()

	for dRows.Next() {
		var key string
		var count int
		if err := dRows.Scan(&key, &count); err != nil {
			return nil, err
		}
		sum := getSummary(key)
		sum.DraftCount = count
	}
	if err := dRows.Err(); err != nil {
		return nil, err
	}

	return summaries, nil
}

// RepoSummariesMulti computes summary statistics for all changes across multiple repos using 4 batched queries.
func (d *DB) RepoSummariesMulti(repoIDs []int64) (map[int64]map[string]*ChangeSummary, error) {
	results := make(map[int64]map[string]*ChangeSummary)
	if len(repoIDs) == 0 {
		return results, nil
	}
	for _, id := range repoIDs {
		results[id] = make(map[string]*ChangeSummary)
	}

	placeholders := make([]string, len(repoIDs))
	args := make([]any, len(repoIDs))
	for i, id := range repoIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	inClause := strings.Join(placeholders, ",")

	getSummary := func(repoID int64, key string) *ChangeSummary {
		m := results[repoID]
		if m == nil {
			m = make(map[string]*ChangeSummary)
			results[repoID] = m
		}
		if s, ok := m[key]; ok {
			return s
		}
		s := &ChangeSummary{ChangeKey: key}
		m[key] = s
		return s
	}

	// 1. Snapshot counts and latest snapshot
	sRows, err := d.db.Query(`
		SELECT s.id, s.repo_id, s.change_key, s.num, s.rev, s.parent_rev, s.created_at
		FROM snapshots s
		WHERE s.repo_id IN (`+inClause+`)
		ORDER BY s.repo_id ASC, s.change_key ASC, s.num DESC
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("loading snapshot summaries: %w", err)
	}
	defer sRows.Close()

	for sRows.Next() {
		var s Snapshot
		if err := sRows.Scan(&s.ID, &s.RepoID, &s.ChangeKey, &s.Num, &s.Rev, &s.ParentRev, &s.CreatedAt); err != nil {
			return nil, err
		}
		sum := getSummary(s.RepoID, s.ChangeKey)
		sum.SnapshotCount++
		if sum.LatestSnapshot == nil {
			sum.LatestSnapshot = &s
		}
	}
	if err := sRows.Err(); err != nil {
		return nil, err
	}

	// 2. Rev marks (reviewed, lgtm)
	mRows, err := d.db.Query(`
		SELECT repo_id, change_key, reviewed, lgtm
		FROM rev_marks
		WHERE repo_id IN (`+inClause+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("loading rev mark summaries: %w", err)
	}
	defer mRows.Close()

	for mRows.Next() {
		var rID int64
		var key string
		var rInt, lInt int
		if err := mRows.Scan(&rID, &key, &rInt, &lInt); err != nil {
			return nil, err
		}
		sum := getSummary(rID, key)
		if rInt == 1 {
			sum.Reviewed = true
		}
		if lInt == 1 {
			sum.LGTM = true
		}
	}
	if err := mRows.Err(); err != nil {
		return nil, err
	}

	// 3. Thread counts (open vs resolved)
	tRows, err := d.db.Query(`
		SELECT repo_id, change_key, resolved, COUNT(*)
		FROM threads
		WHERE repo_id IN (`+inClause+`)
		GROUP BY repo_id, change_key, resolved
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("loading thread summaries: %w", err)
	}
	defer tRows.Close()

	for tRows.Next() {
		var rID int64
		var key string
		var resolvedInt, count int
		if err := tRows.Scan(&rID, &key, &resolvedInt, &count); err != nil {
			return nil, err
		}
		sum := getSummary(rID, key)
		if resolvedInt == 1 {
			sum.ResolvedThreads += count
		} else {
			sum.OpenThreads += count
		}
	}
	if err := tRows.Err(); err != nil {
		return nil, err
	}

	// 4. Draft message counts
	dRows, err := d.db.Query(`
		SELECT t.repo_id, t.change_key, COUNT(*)
		FROM messages m
		JOIN threads t ON m.thread_id = t.id
		WHERE t.repo_id IN (`+inClause+`) AND m.draft = 1
		GROUP BY t.repo_id, t.change_key
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("loading draft summaries: %w", err)
	}
	defer dRows.Close()

	for dRows.Next() {
		var rID int64
		var key string
		var count int
		if err := dRows.Scan(&rID, &key, &count); err != nil {
			return nil, err
		}
		sum := getSummary(rID, key)
		sum.DraftCount = count
	}
	if err := dRows.Err(); err != nil {
		return nil, err
	}

	return results, nil
}

// CommentCountsForChange computes unresolved, resolved, and draft comment counts for a change
// using fast SQL aggregation. If filePath is non-empty, it filters to threads on that file.
func (d *DB) CommentCountsForChange(repoID int64, changeKey, filePath string) (unresolved, resolved, draft int, err error) {
	var threadQuery string
	var threadArgs []any
	var msgQuery string
	var msgArgs []any

	if filePath != "" {
		norm := strings.TrimPrefix(filePath, "/")
		withSlash := "/" + norm
		threadQuery = `
			SELECT resolved, COUNT(*)
			FROM threads
			WHERE repo_id = ? AND change_key = ? AND (path = ? OR path = ?)
			GROUP BY resolved
		`
		threadArgs = []any{repoID, changeKey, norm, withSlash}

		msgQuery = `
			SELECT COUNT(*)
			FROM messages m
			JOIN threads t ON m.thread_id = t.id
			WHERE t.repo_id = ? AND t.change_key = ? AND (t.path = ? OR t.path = ?) AND m.draft = 1
		`
		msgArgs = []any{repoID, changeKey, norm, withSlash}
	} else {
		threadQuery = `
			SELECT resolved, COUNT(*)
			FROM threads
			WHERE repo_id = ? AND change_key = ?
			GROUP BY resolved
		`
		threadArgs = []any{repoID, changeKey}

		msgQuery = `
			SELECT COUNT(*)
			FROM messages m
			JOIN threads t ON m.thread_id = t.id
			WHERE t.repo_id = ? AND t.change_key = ? AND m.draft = 1
		`
		msgArgs = []any{repoID, changeKey}
	}

	tRows, err := d.db.Query(threadQuery, threadArgs...)
	if err != nil {
		return 0, 0, 0, err
	}
	defer tRows.Close()

	for tRows.Next() {
		var resolvedInt, count int
		if err := tRows.Scan(&resolvedInt, &count); err != nil {
			return 0, 0, 0, err
		}
		if resolvedInt == 1 {
			resolved += count
		} else {
			unresolved += count
		}
	}
	if err := tRows.Err(); err != nil {
		return 0, 0, 0, err
	}

	if err := d.db.QueryRow(msgQuery, msgArgs...).Scan(&draft); err != nil {
		return 0, 0, 0, err
	}

	return unresolved, resolved, draft, nil
}

// StaleChangeKeys returns change keys in the DB that are not in keepKeys.
func (d *DB) StaleChangeKeys(repoID int64, keepKeys map[string]bool) ([]string, error) {
	rows, err := d.db.Query(`
		SELECT DISTINCT change_key FROM snapshots WHERE repo_id = ?
		UNION
		SELECT DISTINCT change_key FROM threads WHERE repo_id = ?
		UNION
		SELECT DISTINCT change_key FROM rev_marks WHERE repo_id = ?
		UNION
		SELECT DISTINCT change_key FROM file_marks WHERE repo_id = ?
		UNION
		SELECT DISTINCT change_key FROM form_drafts WHERE repo_id = ?
	`, repoID, repoID, repoID, repoID, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		if !keepKeys[key] {
			stale = append(stale, key)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stale, nil
}

// PruneChanges removes records for changes not in keepKeys.
func (d *DB) PruneChanges(repoID int64, keepKeys map[string]bool) ([]string, error) {
	toPrune, err := d.StaleChangeKeys(repoID, keepKeys)
	if err != nil {
		return nil, err
	}
	if len(toPrune) == 0 {
		return nil, nil
	}

	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, k := range toPrune {
		_, _ = tx.Exec("DELETE FROM snapshots WHERE repo_id = ? AND change_key = ?", repoID, k)
		_, _ = tx.Exec("DELETE FROM threads WHERE repo_id = ? AND change_key = ?", repoID, k)
		_, _ = tx.Exec("DELETE FROM file_marks WHERE repo_id = ? AND change_key = ?", repoID, k)
		_, _ = tx.Exec("DELETE FROM rev_marks WHERE repo_id = ? AND change_key = ?", repoID, k)
		_, _ = tx.Exec("DELETE FROM form_drafts WHERE repo_id = ? AND change_key = ?", repoID, k)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return toPrune, nil
}

// FormDraft represents an unsaved comment, reply, or edit form draft.
type FormDraft struct {
	ID         int64     `json:"id"`
	RepoID     int64     `json:"repo_id"`
	ChangeKey  string    `json:"change_key"`
	DraftKey   string    `json:"draft_key"`
	TargetPath string    `json:"target_path"`
	Side       string    `json:"side"`
	Line       int       `json:"line"`
	EndLine    int       `json:"end_line"`
	StartCol   int       `json:"start_col"`
	EndCol     int       `json:"end_col"`
	Unresolved bool      `json:"unresolved"`
	ThreadID   string    `json:"thread_id"`
	MessageID  string    `json:"message_id"`
	Content    string    `json:"content"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// SaveFormDraft stores or updates an unsaved form draft in the database.
// If content is empty or whitespace-only, the draft is deleted instead.
func (d *DB) SaveFormDraft(repoID int64, changeKey, draftKey, targetPath, side string, line, endLine, startCol, endCol int, unresolved bool, threadID, messageID, content string) error {
	if strings.TrimSpace(content) == "" {
		return d.DeleteFormDraft(repoID, changeKey, draftKey)
	}
	d.digestMu.Lock()
	defer d.digestMu.Unlock()

	var beforeTC int64
	beforeErr := d.db.QueryRow("SELECT total_changes()").Scan(&beforeTC)

	unresolvedInt := 0
	if unresolved {
		unresolvedInt = 1
	}

	_, err := d.db.Exec(`
		INSERT INTO form_drafts (repo_id, change_key, draft_key, target_path, side, line, end_line, start_col, end_col, unresolved, thread_id, message_id, content, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo_id, change_key, draft_key) DO UPDATE SET
			target_path = excluded.target_path,
			side = excluded.side,
			line = excluded.line,
			end_line = excluded.end_line,
			start_col = excluded.start_col,
			end_col = excluded.end_col,
			unresolved = excluded.unresolved,
			thread_id = excluded.thread_id,
			message_id = excluded.message_id,
			content = excluded.content,
			updated_at = CURRENT_TIMESTAMP
	`, repoID, changeKey, draftKey, targetPath, side, line, endLine, startCol, endCol, unresolvedInt, threadID, messageID, content)
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

// DeleteFormDraft removes a single form draft by its draft key.
func (d *DB) DeleteFormDraft(repoID int64, changeKey, draftKey string) error {
	d.digestMu.Lock()
	defer d.digestMu.Unlock()

	var beforeTC int64
	beforeErr := d.db.QueryRow("SELECT total_changes()").Scan(&beforeTC)

	_, err := d.db.Exec("DELETE FROM form_drafts WHERE repo_id = ? AND change_key = ? AND draft_key = ?", repoID, changeKey, draftKey)
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

// DeleteFormDraftsForThread removes all form drafts associated with a thread.
func (d *DB) DeleteFormDraftsForThread(repoID int64, changeKey, threadID string) error {
	d.digestMu.Lock()
	defer d.digestMu.Unlock()

	var beforeTC int64
	beforeErr := d.db.QueryRow("SELECT total_changes()").Scan(&beforeTC)

	_, err := d.db.Exec("DELETE FROM form_drafts WHERE repo_id = ? AND change_key = ? AND thread_id = ?", repoID, changeKey, threadID)
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

// DeleteFormDraftForMessage removes all form drafts associated with an edit of a message.
func (d *DB) DeleteFormDraftForMessage(repoID int64, changeKey, messageID string) error {
	d.digestMu.Lock()
	defer d.digestMu.Unlock()

	var beforeTC int64
	beforeErr := d.db.QueryRow("SELECT total_changes()").Scan(&beforeTC)

	_, err := d.db.Exec("DELETE FROM form_drafts WHERE repo_id = ? AND change_key = ? AND message_id = ?", repoID, changeKey, messageID)
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

// FormDraftsForChange returns all saved form drafts for a given change in a repository.
func (d *DB) FormDraftsForChange(repoID int64, changeKey string) ([]*FormDraft, error) {
	rows, err := d.db.Query(`
		SELECT id, repo_id, change_key, draft_key, target_path, side, line, end_line, start_col, end_col, unresolved, thread_id, message_id, content, updated_at
		FROM form_drafts
		WHERE repo_id = ? AND change_key = ?
		ORDER BY id ASC
	`, repoID, changeKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var drafts []*FormDraft
	for rows.Next() {
		var fd FormDraft
		var unresolvedInt int
		if err := rows.Scan(&fd.ID, &fd.RepoID, &fd.ChangeKey, &fd.DraftKey, &fd.TargetPath, &fd.Side, &fd.Line, &fd.EndLine, &fd.StartCol, &fd.EndCol, &unresolvedInt, &fd.ThreadID, &fd.MessageID, &fd.Content, &fd.UpdatedAt); err != nil {
			return nil, err
		}
		fd.Unresolved = unresolvedInt != 0
		drafts = append(drafts, &fd)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return drafts, nil
}
