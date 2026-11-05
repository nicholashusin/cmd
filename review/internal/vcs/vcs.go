package vcs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CommitMsgPath is the virtual path for the commit message in file listings and diffs.
const CommitMsgPath = "/COMMIT_MSG"

// IsCommitMsgPath reports whether path refers to the virtual commit message entry.
func IsCommitMsgPath(path string) bool {
	return path == CommitMsgPath || path == "COMMIT_MSG"
}

func cleanIdentity(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "<>")
	return strings.TrimSpace(s)
}

const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// FileStatus represents file modification status in a diff.
type FileStatus int

const (
	FileModified FileStatus = iota
	FileAdded
	FileDeleted
	FileRenamed
)

// File represents a file touched by a revision or change.
type File struct {
	Path         string
	OldPath      string
	Status       FileStatus
	AddedLines   int
	DeletedLines int
	Binary       bool
}

// StatusCode returns the single-letter annotation ("A", "M", "D", "R")
// for the file, or "" for virtual paths like /COMMIT_MSG.
func (f *File) StatusCode() string {
	if f == nil || IsCommitMsgPath(f.Path) {
		return ""
	}
	switch f.Status {
	case FileAdded:
		return "A"
	case FileDeleted:
		return "D"
	case FileRenamed:
		return "R"
	default:
		return "M"
	}
}

// StatusTitle returns a human-readable tooltip for StatusCode.
func (f *File) StatusTitle() string {
	switch f.StatusCode() {
	case "A":
		return "Added"
	case "D":
		return "Deleted"
	case "R":
		return "Renamed"
	case "M":
		return "Modified"
	default:
		return ""
	}
}

// Change represents a pending or committed change in the repository.
type Change struct {
	Key           string // Stable identifier (jj change_id, Git Change-Id, or commit SHA)
	Rev           string // Commit SHA / commit ID
	Parent        string // Parent commit SHA
	ParentKey     string // Parent change key
	ParentSubject string // Parent commit subject (first line of description)
	Subject       string
	Message       string
	Author        string
	Date          time.Time
	Committer     string
	CommitDate    time.Time
	Current       bool // True if this is the active working copy / HEAD
	Working       bool // True if this represents unstaged/working-copy changes
	Empty         bool // True if change has no file modifications
}

func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') && (b < 'A' || b > 'F') {
			return false
		}
	}
	return true
}

// IsImmutableHexRev returns true if rev is a 40-character (SHA-1) or 64-character (SHA-256)
// hexadecimal string.
func IsImmutableHexRev(rev string) bool {
	return (len(rev) == 40 || len(rev) == 64) && isHexString(rev)
}

// ResolveChange matches a query against a slice of changes by key, ChangeID,
// exact revision, or a unique prefix (min length 3) of ChangeID or Rev.
func ResolveChange(changes []*Change, query string) *Change {
	if query == "" {
		return nil
	}
	for _, c := range changes {
		if c == nil {
			continue
		}
		if c.Key == query || c.Rev == query {
			return c
		}
	}
	if len(query) >= 3 {
		var matched *Change
		matchCount := 0
		for _, c := range changes {
			if c == nil {
				continue
			}
			if strings.HasPrefix(c.Key, query) || strings.HasPrefix(c.Rev, query) {
				matched = c
				matchCount++
			}
		}
		if matchCount == 1 {
			return matched
		}
	}
	return nil
}

// Repo is the common interface for Jujutsu and Git repositories.
type Repo interface {
	Kind() string // "jj" or "git"
	Root() string
	User() string
	Changes() ([]*Change, error)
	Commit(rev string) (*Change, error)
	Files(baseRev, targetRev string) ([]*File, error)
	Content(rev, path string) ([]byte, error)
	ListDir(rev, dirPath string) ([]string, error)
	ListGoFiles(rev string) ([]string, error)
	Pin(refSuffix, rev string) error
	ListPins() ([]string, error)
	DeletePin(ref string) error
}

// BlobOIDer is an optional interface implemented by repos that can quickly return
// the object ID (e.g. Git SHA) for a file at a specific revision without reading
// full content.
type BlobOIDer interface {
	BlobOID(rev, path string) (string, error)
}

// GoFileBlob pairs a repository-relative path with its Git blob hexadecimal OID.
type GoFileBlob struct {
	Path string
	OID  string
}

// GoBlobRepo is an optional interface for repositories that can quickly list
// and stream Git blob objects for Go source files without reading files one-by-one.
type GoBlobRepo interface {
	ListGoFileBlobs(rev string) ([]GoFileBlob, error)
	StreamBlobs(oids []string, visit func(idx int, oid string, data []byte)) error
}

// DirGoBlobRepo is an optional interface for repositories that can quickly read all
// non-test .go file blobs in a directory at a given revision in a single pipelined operation.
type DirGoBlobRepo interface {
	ReadDirGoFiles(rev, dir string) ([]GoFileBlobContent, error)
}

const jjCacheTTL = 250 * time.Millisecond

type jjCacheEntry struct {
	opID    string
	changes []*Change
	at      time.Time
}

// Cache holds in-memory caches across operations to avoid redundant subprocess invocations.
type Cache struct {
	mu          sync.Mutex
	catfiles    map[string]*CatFileBatch
	repoGitDirs map[string]string
	jjChanges   map[string]jjCacheEntry
	repoUsers   map[string]string
}

// NewCache creates an initialized VCS cache.
func NewCache() *Cache {
	return &Cache{
		catfiles:    make(map[string]*CatFileBatch),
		repoGitDirs: make(map[string]string),
		jjChanges:   make(map[string]jjCacheEntry),
		repoUsers:   make(map[string]string),
	}
}

func (c *Cache) user(root string, detect func() string) string {
	if c == nil {
		return detect()
	}
	c.mu.Lock()
	if u, ok := c.repoUsers[root]; ok {
		c.mu.Unlock()
		return u
	}
	c.mu.Unlock()

	u := detect()

	c.mu.Lock()
	c.repoUsers[root] = u
	c.mu.Unlock()
	return u
}

func (c *Cache) registerRepo(root, gitDir string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repoGitDirs[root] = gitDir
}

// catFile retrieves or creates a cached CatFileBatch instance for gitDir.
func (c *Cache) catFile(gitDir string) (*CatFileBatch, error) {
	if gitDir == "" {
		return nil, fmt.Errorf("no git directory available for cat-file")
	}
	if c == nil {
		return NewCatFileBatch(gitDir)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if cf, ok := c.catfiles[gitDir]; ok && !cf.closed.Load() {
		return cf, nil
	}
	cf, err := NewCatFileBatch(gitDir)
	if err != nil {
		return nil, err
	}
	c.catfiles[gitDir] = cf
	return cf, nil
}

// Evict clears cached resources for a repository root and closes background cat-file processes.
func (c *Cache) Evict(root string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	isPrefix := func(k string) bool {
		return k == root || strings.HasPrefix(k, root+string(filepath.Separator))
	}

	var evictedGitDirs []string
	for r, gd := range c.repoGitDirs {
		if isPrefix(r) {
			evictedGitDirs = append(evictedGitDirs, gd)
			delete(c.repoGitDirs, r)
		}
	}

	for k, cf := range c.catfiles {
		evict := isPrefix(k)
		if !evict {
			for _, gd := range evictedGitDirs {
				if k == gd {
					evict = true
					break
				}
			}
		}
		if evict {
			_ = cf.Close()
			delete(c.catfiles, k)
		}
	}
	for k := range c.jjChanges {
		if isPrefix(k) {
			delete(c.jjChanges, k)
		}
	}
	for k := range c.repoUsers {
		if isPrefix(k) {
			delete(c.repoUsers, k)
		}
	}
}

// Close terminates all active subprocesses held by the cache.
func (c *Cache) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, cf := range c.catfiles {
		_ = cf.Close()
		delete(c.catfiles, k)
	}
}

// resolveJJRepoDir resolves the underlying repository directory from a .jj directory.
func resolveJJRepoDir(jjDir string) string {
	repoPath := filepath.Join(jjDir, "repo")
	info, err := os.Stat(repoPath)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return repoPath
	}
	target, err := os.ReadFile(repoPath)
	if err != nil {
		return ""
	}
	t := strings.TrimSpace(string(target))
	if t == "" {
		return ""
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(jjDir, t)
	}
	return filepath.Clean(t)
}

// resolveGitDir checks if root contains a .git directory or worktree file, returning
// the absolute path to the underlying git dir and true if found.
func resolveGitDir(root string) (string, bool) {
	gitDir := filepath.Join(root, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return gitDir, true
	}
	content, err := os.ReadFile(gitDir)
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir:") {
		return "", false
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	return filepath.Clean(target), true
}

// WatchSignature computes a lightweight signature string of the repository state
// purely via os.Stat and os.ReadDir on .jj and .git metadata (zero subprocesses!).
func (c *Cache) WatchSignature(roots []string) string {
	h := sha256.New()
	for _, root := range roots {
		h.Write([]byte(root))
		// Check .jj
		jjDir := filepath.Join(root, ".jj")
		if mainRepoDir := resolveJJRepoDir(jjDir); mainRepoDir != "" {
			headsDir := filepath.Join(mainRepoDir, "op_heads", "heads")
			if entries, err := os.ReadDir(headsDir); err == nil {
				for _, e := range entries {
					h.Write([]byte(e.Name()))
					if fi, err := e.Info(); err == nil {
						fmt.Fprintf(h, ":%d", fi.ModTime().UnixNano())
					}
				}
			}
			for _, wcFile := range []string{"tree_state", "checkout"} {
				if fi, err := os.Stat(filepath.Join(jjDir, "working_copy", wcFile)); err == nil {
					fmt.Fprintf(h, ":jjwc:%s:%d:%d", wcFile, fi.Size(), fi.ModTime().UnixNano())
				}
			}
		}

		// Check .git
		if realGitDir, ok := resolveGitDir(root); ok {
			// index
			indexFile := filepath.Join(realGitDir, "index")
			if fi, err := os.Stat(indexFile); err == nil {
				fmt.Fprintf(h, ":gitidx:%d:%d", fi.Size(), fi.ModTime().UnixNano())
			}
			// HEAD
			headFile := filepath.Join(realGitDir, "HEAD")
			if fi, err := os.Stat(headFile); err == nil {
				fmt.Fprintf(h, ":head:%d:%d", fi.Size(), fi.ModTime().UnixNano())
			}
			// refs/heads
			refsDir := filepath.Join(realGitDir, "refs", "heads")
			if entries, err := os.ReadDir(refsDir); err == nil {
				for _, e := range entries {
					h.Write([]byte(e.Name()))
					if fi, err := e.Info(); err == nil {
						fmt.Fprintf(h, ":%d", fi.ModTime().UnixNano())
					}
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Open discovers and opens a repository at dir or one of its ancestors.
// Checks for Jujutsu (.jj) first, then Git (.git).
func Open(dir string, cache *Cache) (Repo, error) {
	if cache == nil {
		cache = NewCache()
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving path %s: %w", dir, err)
	}

	curr := absDir
	for {
		// Check .jj
		jjDir := filepath.Join(curr, ".jj")
		if fi, err := os.Stat(jjDir); err == nil && fi.IsDir() {
			if mainRepoDir := resolveJJRepoDir(jjDir); mainRepoDir != "" {
				return openJJ(curr, mainRepoDir, cache)
			}
		}

		// Check .git
		if _, ok := resolveGitDir(curr); ok {
			return openGit(curr, cache)
		}

		parent := filepath.Dir(curr)
		if parent == curr {
			break
		}
		curr = parent
	}

	return nil, fmt.Errorf("no .jj or .git repository found in %s or any parent directory", dir)
}

// Pin helpers shared across VCS implementations.
func pinRef(gitDir, refSuffix, rev string) error {
	if gitDir == "" {
		return nil
	}
	ref := "refs/reviewed/" + refSuffix
	cmd := exec.Command("git", "--git-dir="+gitDir, "--no-optional-locks", "update-ref", ref, rev)
	return cmd.Run()
}

func listPinRefs(gitDir string) ([]string, error) {
	if gitDir == "" {
		return nil, nil
	}
	cmd := exec.Command("git", "--git-dir="+gitDir, "--no-optional-locks", "for-each-ref", "--format=%(refname)", "refs/reviewed/")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			refs = append(refs, line)
		}
	}
	return refs, nil
}

func deletePinRef(gitDir, ref string) error {
	if gitDir == "" {
		return nil
	}
	cmd := exec.Command("git", "--git-dir="+gitDir, "--no-optional-locks", "update-ref", "-d", ref)
	return cmd.Run()
}

func gitDiffFiles(gitDir, workDir, baseRev, targetRev string) ([]*File, error) {
	files := []*File{
		{
			Path:   CommitMsgPath,
			Status: FileModified,
		},
	}

	bRev := baseRev
	if bRev == "" || strings.Trim(bRev, "0") == "" {
		bRev = emptyTreeSHA
	}

	var args []string
	if gitDir != "" {
		args = append(args, "--git-dir="+gitDir)
	}
	args = append(args, "--no-optional-locks", "diff", "--raw", "--numstat", "-M", "-z", bRev)

	if targetRev != "" && targetRev != "working" {
		args = append(args, targetRev)
	}

	cmd := exec.Command("git", args...)
	if targetRev == "working" && workDir != "" {
		cmd.Dir = workDir
	}

	out, err := cmd.Output()
	if err != nil {
		return files, err
	}

	fileMap := make(map[string]*File)
	parts := bytes.Split(out, []byte{0})
	i := 0
	for i < len(parts) {
		p := parts[i]
		if len(p) == 0 {
			i++
			continue
		}
		if p[0] == ':' {
			meta := string(p)
			i++
			statusField := meta[strings.LastIndexByte(meta, ' ')+1:]
			var status FileStatus
			var oldPath, path string
			switch {
			case strings.HasPrefix(statusField, "A"):
				status = FileAdded
				if i < len(parts) {
					path = string(parts[i])
					i++
				}
			case strings.HasPrefix(statusField, "D"):
				status = FileDeleted
				if i < len(parts) {
					path = string(parts[i])
					i++
				}
			case strings.HasPrefix(statusField, "R"), strings.HasPrefix(statusField, "C"):
				status = FileRenamed
				if i < len(parts) {
					oldPath = string(parts[i])
					i++
				}
				if i < len(parts) {
					path = string(parts[i])
					i++
				}
			default:
				status = FileModified
				if i < len(parts) {
					path = string(parts[i])
					i++
				}
			}
			if path != "" {
				f := &File{
					Path:    path,
					OldPath: oldPath,
					Status:  status,
				}
				files = append(files, f)
				fileMap[path] = f
			}
		} else {
			tabs := bytes.Split(p, []byte{'\t'})
			if len(tabs) >= 3 {
				addStr := string(tabs[0])
				delStr := string(tabs[1])
				path := string(tabs[2])
				if path == "" && i+2 < len(parts) {
					i += 2
					path = string(parts[i])
				}
				if f, ok := fileMap[path]; ok {
					if addStr == "-" && delStr == "-" {
						f.Binary = true
					} else {
						f.AddedLines, _ = strconv.Atoi(addStr)
						f.DeletedLines, _ = strconv.Atoi(delStr)
					}
				}
			}
			i++
		}
	}

	return files, nil
}

const commitMsgDateFormat = "2006-01-02 15:04:05 -0700"

func hasChangeIDTrailer(msg string) bool {
	for _, line := range strings.Split(msg, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= len("change-id:") && strings.EqualFold(trimmed[:len("change-id:")], "change-id:") {
			return true
		}
	}
	return false
}

// FormatCommitMsg generates a clean in-memory /COMMIT_MSG representation
// with commit metadata headers and message body.
func FormatCommitMsg(c *Change) string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	if c.Parent != "" && strings.Trim(c.Parent, "0") != "" {
		shortParent := c.Parent
		if len(shortParent) > 8 && isHexString(shortParent) {
			shortParent = shortParent[:8]
		}
		if c.ParentSubject != "" {
			fmt.Fprintf(&sb, "Parent:     %s (%s)\n", shortParent, c.ParentSubject)
		} else {
			fmt.Fprintf(&sb, "Parent:     %s\n", shortParent)
		}
	}
	if c.Author != "" {
		fmt.Fprintf(&sb, "Author:     %s\n", c.Author)
	}
	if !c.Date.IsZero() {
		fmt.Fprintf(&sb, "AuthorDate: %s\n", c.Date.Format(commitMsgDateFormat))
	}
	if c.Committer != "" {
		fmt.Fprintf(&sb, "Commit:     %s\n", c.Committer)
	}
	if !c.CommitDate.IsZero() {
		fmt.Fprintf(&sb, "CommitDate: %s\n", c.CommitDate.Format(commitMsgDateFormat))
	}
	if c.Key != "" && c.Key != c.Rev && c.Key != "working" && !hasChangeIDTrailer(c.Message) {
		fmt.Fprintf(&sb, "Change-Id:  %s\n", c.Key)
	}
	if sb.Len() > 0 && c.Message != "" {
		sb.WriteString("\n")
	}
	sb.WriteString(c.Message)
	if c.Message != "" && !strings.HasSuffix(c.Message, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}

// gitListDir lists direct non-directory files inside dirPath at rev.
func gitListDir(cache *Cache, gitDir, workDir, rev, dirPath string) ([]string, error) {
	cleanDir := strings.Trim(filepath.ToSlash(dirPath), "/")
	if cleanDir == "." {
		cleanDir = ""
	}

	if (rev == "" || rev == "working") && workDir != "" {
		targetDir := workDir
		if cleanDir != "" {
			targetDir = filepath.Join(workDir, filepath.FromSlash(cleanDir))
		}
		entries, err := os.ReadDir(targetDir)
		if err != nil {
			if os.IsNotExist(err) {
				return []string{}, nil
			}
			return nil, err
		}
		names := []string{}
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		return names, nil
	}

	if gitDir != "" && rev != "" && rev != "working" {
		if cf, err := cache.catFile(gitDir); err == nil {
			if names, err := cf.ListTree(rev, cleanDir); err == nil {
				return names, nil
			}
		}

		treeSpec := rev
		if cleanDir != "" {
			treeSpec = rev + ":" + cleanDir
		}
		cmd := exec.Command("git", "--git-dir="+gitDir, "--no-optional-locks", "ls-tree", "-z", treeSpec)
		out, err := cmd.Output()
		if err != nil {
			return nil, nil
		}
		names := []string{}
		entries := bytes.Split(out, []byte{0})
		for _, entry := range entries {
			if len(entry) == 0 {
				continue
			}
			tabIdx := bytes.IndexByte(entry, '\t')
			if tabIdx == -1 {
				continue
			}
			meta := string(entry[:tabIdx])
			name := string(entry[tabIdx+1:])
			fields := strings.Fields(meta)
			if len(fields) >= 2 {
				entryType := fields[1]
				if entryType == "blob" || entryType == "symlink" {
					names = append(names, name)
				}
			}
		}
		return names, nil
	}

	return []string{}, nil
}

// gitListGoFiles recursively lists repo-relative .go files at rev,
// skipping directories matching shouldSkipGoDir.
func gitListGoFiles(cache *Cache, gitDir, workDir, rev string) ([]string, error) {
	if (rev == "" || rev == "working") && workDir != "" {
		var goFiles []string
		err := filepath.WalkDir(workDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != workDir && shouldSkipGoDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if strings.HasSuffix(name, ".go") && name[0] != '.' && name[0] != '_' {
				rel, err := filepath.Rel(workDir, path)
				if err == nil {
					goFiles = append(goFiles, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		slices.Sort(goFiles)
		return goFiles, nil
	}

	if gitDir != "" && rev != "" && rev != "working" {
		if cache != nil {
			if cf, err := cache.catFile(gitDir); err == nil {
				if files, err := cf.ListGoFiles(rev); err == nil {
					return files, nil
				}
			}
		}

		cmd := exec.Command("git", "--git-dir="+gitDir, "--no-optional-locks", "ls-tree", "-r", "-z", rev)
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}

		var goFiles []string
		entries := bytes.Split(out, []byte{0})
		for _, entry := range entries {
			if len(entry) == 0 {
				continue
			}
			tabIdx := bytes.IndexByte(entry, '\t')
			if tabIdx == -1 {
				continue
			}
			p := string(entry[tabIdx+1:])
			parts := strings.Split(p, "/")
			skip := false
			for i := 0; i < len(parts)-1; i++ {
				if shouldSkipGoDir(parts[i]) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			fileName := parts[len(parts)-1]
			if strings.HasSuffix(fileName, ".go") && fileName[0] != '.' && fileName[0] != '_' {
				goFiles = append(goFiles, p)
			}
		}
		slices.Sort(goFiles)
		return goFiles, nil
	}

	return []string{}, nil
}
