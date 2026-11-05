package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// RepoRecord represents a tracked repository in the database.
type RepoRecord struct {
	ID        int64     `json:"id"`
	Slug      string    `json:"slug"`
	RootPath  string    `json:"root_path"`
	CreatedAt time.Time `json:"created_at"`
}

// EnsureRepo ensures that the repository at rootPath exists in the database.
// If it does not exist, it creates a new record assigning a unique slug (e.g. base, base.1, ...).
func (d *DB) EnsureRepo(rootPath string) (*RepoRecord, error) {
	cleanPath := filepath.Clean(rootPath)

	var rec RepoRecord
	err := d.db.QueryRow("SELECT id, slug, root_path, created_at FROM repos WHERE root_path = ?", cleanPath).
		Scan(&rec.ID, &rec.Slug, &rec.RootPath, &rec.CreatedAt)
	if err == nil {
		return &rec, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("checking repo %s: %w", cleanPath, err)
	}

	baseSlug := filepath.Base(cleanPath)
	if baseSlug == "" || baseSlug == "/" || baseSlug == "." {
		baseSlug = "repo"
	}
	// Sanitize slug
	baseSlug = strings.ToLower(baseSlug)

	candidate := baseSlug
	idx := 1
	for {
		var exists int
		err := d.db.QueryRow("SELECT COUNT(*) FROM repos WHERE slug = ?", candidate).Scan(&exists)
		if err != nil {
			return nil, err
		}
		if exists == 0 {
			break
		}
		candidate = fmt.Sprintf("%s.%d", baseSlug, idx)
		idx++
	}

	now := time.Now().UTC()
	res, err := d.db.Exec("INSERT INTO repos (slug, root_path, created_at) VALUES (?, ?, ?)", candidate, cleanPath, now)
	if err != nil {
		// In case of race condition
		err2 := d.db.QueryRow("SELECT id, slug, root_path, created_at FROM repos WHERE root_path = ?", cleanPath).
			Scan(&rec.ID, &rec.Slug, &rec.RootPath, &rec.CreatedAt)
		if err2 == nil {
			return &rec, nil
		}
		return nil, fmt.Errorf("inserting repo %s: %w", cleanPath, err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &RepoRecord{
		ID:        id,
		Slug:      candidate,
		RootPath:  cleanPath,
		CreatedAt: now,
	}, nil
}

// Repos lists all tracked repositories.
func (d *DB) Repos() ([]*RepoRecord, error) {
	rows, err := d.db.Query("SELECT id, slug, root_path, created_at FROM repos ORDER BY slug ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []*RepoRecord
	for rows.Next() {
		var r RepoRecord
		if err := rows.Scan(&r.ID, &r.Slug, &r.RootPath, &r.CreatedAt); err != nil {
			return nil, err
		}
		res = append(res, &r)
	}
	return res, rows.Err()
}

// RepoByID retrieves a repository record by its integer ID.
func (d *DB) RepoByID(id int64) (*RepoRecord, error) {
	var r RepoRecord
	err := d.db.QueryRow("SELECT id, slug, root_path, created_at FROM repos WHERE id = ?", id).
		Scan(&r.ID, &r.Slug, &r.RootPath, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// RepoBySlug finds a repository record by its slug or by its root path.
func (d *DB) RepoBySlug(slugOrPath string) (*RepoRecord, error) {
	var r RepoRecord
	err := d.db.QueryRow("SELECT id, slug, root_path, created_at FROM repos WHERE slug = ? OR root_path = ?", slugOrPath, slugOrPath).
		Scan(&r.ID, &r.Slug, &r.RootPath, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ForgetRepo removes a repository and cascades deletion of its snapshots, threads, and marks.
func (d *DB) ForgetRepo(slugOrPath string) (*RepoRecord, error) {
	rec, err := d.RepoBySlug(slugOrPath)
	if err != nil || rec == nil {
		return rec, err
	}

	_, err = d.db.Exec("DELETE FROM repos WHERE id = ?", rec.ID)
	if err != nil {
		return nil, fmt.Errorf("forgetting repo %s: %w", slugOrPath, err)
	}
	return rec, nil
}
