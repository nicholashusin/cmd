package vcs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const jjLogTemplate = `change_id ++ "\x1f" ++ commit_id ++ "\x1f" ++ parents.map(|p| p.commit_id()).join(" ") ++ "\x1f" ++ parents.map(|p| p.change_id()).join(" ") ++ "\x1f" ++ parents.map(|p| p.description().first_line()).join("\n") ++ "\x1f" ++ author.name() ++ " <" ++ author.email() ++ ">" ++ "\x1f" ++ author.timestamp() ++ "\x1f" ++ committer.name() ++ " <" ++ committer.email() ++ ">" ++ "\x1f" ++ committer.timestamp() ++ "\x1f" ++ empty ++ "\x1f" ++ current_working_copy ++ "\x1f" ++ description ++ "\x1e"`

const jjDiffTemplate = `status_char ++ "\x1f" ++ source.path() ++ "\x1f" ++ path ++ "\x1e"`

const jjDiffStatTemplate = `commit_id ++ "\x1f" ++ diff.stat().files().len() ++ "\x1f" ++ diff.stat().total_added() ++ "\x1f" ++ diff.stat().total_removed() ++ "\x1e"`

// JJRepo implements the Repo interface for Jujutsu repositories.
type JJRepo struct {
	root        string
	mainRepoDir string
	gitDir      string
	cache       *Cache
	user        string
	mu          sync.Mutex
	changes     []*Change
}

func openJJ(root, mainRepoDir string, cache *Cache) (*JJRepo, error) {
	if cache == nil {
		cache = NewCache()
	}
	r := &JJRepo{
		root:        root,
		mainRepoDir: mainRepoDir,
		cache:       cache,
	}

	// Detect git directory
	gitTarget := filepath.Join(mainRepoDir, "store", "git_target")
	if fi, err := os.Stat(gitTarget); err == nil {
		if fi.IsDir() {
			r.gitDir = gitTarget
		} else {
			if target, err := os.ReadFile(gitTarget); err == nil {
				t := strings.TrimSpace(string(target))
				if t != "" {
					if !filepath.IsAbs(t) {
						t = filepath.Clean(filepath.Join(filepath.Dir(gitTarget), t))
					}
					r.gitDir = t
				}
			}
		}
	}
	if r.gitDir == "" {
		if gd, ok := resolveGitDir(root); ok {
			r.gitDir = gd
		}
	}

	if r.gitDir != "" {
		cache.registerRepo(root, r.gitDir)
	}

	// Detect user via cache
	r.user = cache.user(root, func() string {
		out, err := r.runCmd("jj", "--ignore-working-copy", "config", "get", "user.name")
		if err == nil && len(out) > 0 {
			return strings.TrimSpace(string(out))
		}
		if r.gitDir != "" {
			gOut, err := r.runCmd("git", "--git-dir="+r.gitDir, "--no-optional-locks", "config", "user.name")
			if err == nil {
				return strings.TrimSpace(string(gOut))
			}
		}
		return ""
	})

	return r, nil
}

func (r *JJRepo) Kind() string { return "jj" }
func (r *JJRepo) Root() string { return r.root }
func (r *JJRepo) User() string { return r.user }

func (r *JJRepo) runCmd(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = r.root
	return cmd.Output()
}

func (r *JJRepo) getOpID() string {
	headsDir := filepath.Join(r.mainRepoDir, "op_heads", "heads")
	entries, err := os.ReadDir(headsDir)
	if err != nil || len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(e.Name())
	}
	return sb.String()
}

func isStaleWorkingCopyErr(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && bytes.Contains(bytes.ToLower(exitErr.Stderr), []byte("working copy is stale"))
}

func (r *JJRepo) Changes() ([]*Change, error) {
	r.mu.Lock()
	if r.changes != nil {
		cached := r.changes
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	opID := r.getOpID()
	if opID != "" && r.cache != nil {
		r.cache.mu.Lock()
		entry, ok := r.cache.jjChanges[r.root]
		r.cache.mu.Unlock()
		if ok && entry.opID == opID && time.Since(entry.at) < jjCacheTTL {
			r.mu.Lock()
			r.changes = entry.changes
			r.mu.Unlock()
			return entry.changes, nil
		}
	}

	out, err := r.runCmd("jj", "--config", `ui.paginate="never"`, "log", "--no-graph", "-r", "mutable()", "-T", jjLogTemplate)
	if isStaleWorkingCopyErr(err) {
		out, err = r.runCmd("jj", "--ignore-working-copy", "--config", `ui.paginate="never"`, "log", "--no-graph", "-r", "mutable()", "-T", jjLogTemplate)
	}
	if err != nil {
		return nil, fmt.Errorf("querying jj changes: %w", err)
	}

	rawRecords := bytes.Split(out, []byte("\x1e"))
	var changes []*Change

	for _, rec := range rawRecords {
		if c, ok := r.parseChangeRecord(rec); ok {
			changes = append(changes, c)
		}
	}

	r.mu.Lock()
	r.changes = changes
	r.mu.Unlock()
	opID = r.getOpID()
	if opID != "" && r.cache != nil {
		r.cache.mu.Lock()
		r.cache.jjChanges[r.root] = jjCacheEntry{
			opID:    opID,
			changes: changes,
			at:      time.Now(),
		}
		r.cache.mu.Unlock()
	}

	return changes, nil
}

func (r *JJRepo) parseChangeRecord(rec []byte) (*Change, bool) {
	if len(rec) == 0 || bytes.Equal(bytes.TrimSpace(rec), nil) {
		return nil, false
	}
	fields := bytes.SplitN(rec, []byte("\x1f"), 12)
	if len(fields) < 12 {
		return nil, false
	}

	key := string(fields[0])
	rev := string(fields[1])
	parents := strings.Fields(string(fields[2]))
	parentKeys := strings.Fields(string(fields[3]))
	parentSubjects := strings.Split(string(fields[4]), "\n")
	author := cleanIdentity(string(fields[5]))
	timeStr := string(fields[6])
	committer := cleanIdentity(string(fields[7]))
	commitTimeStr := string(fields[8])
	empty := string(fields[9]) == "true"
	current := string(fields[10]) == "true"
	desc := string(fields[11])

	parent := ""
	if len(parents) > 0 {
		parent = parents[0]
	}
	parentKey := ""
	if len(parentKeys) > 0 {
		parentKey = parentKeys[0]
	}
	parentSubject := ""
	if len(parentSubjects) > 0 {
		parentSubject = strings.TrimSpace(parentSubjects[0])
	}

	date := parseJJTimestamp(timeStr)
	commitDate := parseJJTimestamp(commitTimeStr)

	lines := strings.Split(desc, "\n")
	subject := "(no description set)"
	if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
		subject = strings.TrimSpace(lines[0])
	}

	return &Change{
		Key:           key,
		Rev:           rev,
		Parent:        parent,
		ParentKey:     parentKey,
		ParentSubject: parentSubject,
		Subject:       subject,
		Message:       desc,
		Author:        author,
		Date:          date,
		Committer:     committer,
		CommitDate:    commitDate,
		Current:       current,
		Working:       empty && strings.TrimSpace(desc) == "",
		Empty:         empty,
	}, true
}

func (r *JJRepo) Commit(rev string) (*Change, error) {
	if err := ValidateRev(rev); err != nil {
		return nil, err
	}
	changes, err := r.Changes()
	if err == nil {
		if rev == "@" {
			for _, c := range changes {
				if c != nil && c.Current {
					return c, nil
				}
			}
		} else if c := ResolveChange(changes, rev); c != nil {
			return c, nil
		}
	}

	ignoreWC := rev != "@" && !strings.Contains(rev, "@")
	args := []string{"--config", `ui.paginate="never"`}
	if ignoreWC {
		args = append(args, "--ignore-working-copy")
	}
	args = append(args, "log", "--no-graph", "-r="+rev, "-T", jjLogTemplate)
	out, err := r.runCmd("jj", args...)
	if !ignoreWC && isStaleWorkingCopyErr(err) {
		out, err = r.runCmd("jj", append([]string{"--ignore-working-copy"}, args...)...)
	}
	if err != nil {
		return nil, fmt.Errorf("resolving jj commit %s: %w", rev, err)
	}

	var matched []*Change
	for _, rec := range bytes.Split(out, []byte("\x1e")) {
		if c, ok := r.parseChangeRecord(rec); ok {
			matched = append(matched, c)
		}
	}
	if len(matched) == 1 {
		return matched[0], nil
	}
	if len(matched) > 1 {
		return nil, fmt.Errorf("revset %s matched %d commits", rev, len(matched))
	}
	return nil, fmt.Errorf("commit %s not found", rev)
}

func (r *JJRepo) Files(baseRev, targetRev string) ([]*File, error) {
	if baseRev != "" && strings.Trim(baseRev, "0") != "" {
		if err := ValidateRev(baseRev); err != nil {
			return nil, err
		}
	}
	if targetRev != "" && targetRev != "working" {
		if err := ValidateRev(targetRev); err != nil {
			return nil, err
		}
	}
	if r.gitDir != "" && !r.conflicted(baseRev) && !r.conflicted(targetRev) {
		files, err := gitDiffFiles(r.gitDir, r.root, baseRev, targetRev)
		if err == nil {
			return files, nil
		}
	}

	if baseRev == "" {
		baseRev = "root()"
	}
	out, err := r.runCmd("jj", "--ignore-working-copy", "--config", `ui.paginate="never"`, "diff", "--from="+baseRev, "--to="+targetRev, "-T", jjDiffTemplate)
	if err != nil {
		return nil, fmt.Errorf("listing jj changed files: %w", err)
	}
	files := []*File{
		{
			Path:   CommitMsgPath,
			Status: FileModified,
		},
	}
	for _, rec := range strings.Split(string(out), "\x1e") {
		fields := strings.Split(rec, "\x1f")
		if len(fields) != 3 {
			continue
		}
		f := &File{Path: fields[2]}
		switch fields[0] {
		case "A":
			f.Status = FileAdded
		case "D":
			f.Status = FileDeleted
		case "R", "C":
			f.Status = FileRenamed
			f.OldPath = fields[1]
		}
		files = append(files, f)
	}
	return files, nil
}

// conflicted reports whether rev has unresolved conflicts, whose files Git
// can't read.
func (r *JJRepo) conflicted(rev string) bool {
	if rev == "" || rev == "working" || strings.Trim(rev, "0") == "" {
		return false
	}
	cf, err := r.cache.catFile(r.gitDir)
	return err == nil && cf.JJConflict(rev)
}

func (r *JJRepo) ListDir(rev, dirPath string) ([]string, error) {
	cleanDir := strings.Trim(filepath.ToSlash(dirPath), "/")
	if cleanDir == "." {
		cleanDir = ""
	}
	if (rev == "" || rev == "working") && r.root != "" {
		return listWorkingDir(r.root, cleanDir)
	}
	if err := ValidateRev(rev); err != nil {
		return nil, err
	}
	if r.gitDir != "" && !r.conflicted(rev) {
		names, err := gitListDir(r.cache, r.gitDir, r.root, rev, dirPath)
		if err == nil && names != nil {
			return names, nil
		}
	}

	args := []string{"--ignore-working-copy", "--config", `ui.paginate="never"`, "file", "list", "-r=" + rev}
	if cleanDir != "" {
		if cleanDir == ".." || strings.HasPrefix(cleanDir, "../") || strings.Contains(cleanDir, "/../") || strings.HasSuffix(cleanDir, "/..") {
			return nil, fmt.Errorf("path escapes repository root: %s", dirPath)
		}
		args = append(args, "--", "root:"+strconv.Quote(cleanDir))
	}

	outBytes, err := r.runCmd("jj", args...)
	if err != nil {
		return nil, nil
	}

	lines := strings.Split(strings.TrimRight(string(outBytes), "\r\n"), "\n")
	var names []string
	prefix := cleanDir
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		rel := line
		if prefix != "" {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			rel = strings.TrimPrefix(line, prefix)
		}
		if !strings.Contains(rel, "/") {
			names = append(names, rel)
		}
	}
	return names, nil
}

func (r *JJRepo) ListGoFiles(rev string) ([]string, error) {
	if (rev == "" || rev == "working") && r.root != "" {
		if goFiles, err := walkWorkingGoFiles(r.root); err == nil {
			return goFiles, nil
		}
	}
	if rev != "" && rev != "working" {
		if err := ValidateRev(rev); err != nil {
			return nil, err
		}
	}
	if r.gitDir != "" && !r.conflicted(rev) {
		files, err := gitListGoFiles(r.cache, r.gitDir, r.root, rev)
		if err == nil && files != nil {
			return files, nil
		}
	}

	args := []string{"--ignore-working-copy", "--config", `ui.paginate="never"`, "file", "list"}
	if rev != "" && rev != "working" {
		args = append(args, "-r="+rev)
	}

	outBytes, err := r.runCmd("jj", args...)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimRight(string(outBytes), "\r\n"), "\n")
	var goFiles []string
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		p := filepath.ToSlash(line)
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

func (r *JJRepo) Content(rev, path string) ([]byte, error) {
	if rev == "" || strings.Trim(rev, "0") == "" {
		return nil, nil
	}
	if IsCommitMsgPath(path) {
		c, err := r.Commit(rev)
		if err != nil {
			return nil, err
		}
		return []byte(FormatCommitMsg(c)), nil
	}

	cleanPath := strings.TrimPrefix(path, "/")

	if rev == "working" {
		return readWorkingFile(r.root, path)
	}
	if err := ValidateRev(rev); err != nil {
		return nil, err
	}

	if r.gitDir != "" && !r.conflicted(rev) {
		cf, err := r.cache.catFile(r.gitDir)
		if err == nil {
			blob, err := cf.ReadBlob(rev + ":" + cleanPath)
			if err == nil {
				return blob, nil
			}
			if errors.Is(err, os.ErrNotExist) && cf.HasTree(rev) {
				return nil, os.ErrNotExist
			}
		}
	}

	// Fall back to jj, which also renders conflicts with conflict markers.
	out, err := r.runCmd("jj", "--ignore-working-copy", "--config", `ui.paginate="never"`, "file", "show", "-r="+rev, "--", "root-file:"+strconv.Quote(cleanPath))
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && bytes.Contains(exitErr.Stderr, []byte("No such path")) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("reading content %s:%s: %w", rev, cleanPath, err)
	}
	return out, nil
}

func (r *JJRepo) Pin(refSuffix, rev string) error {
	return pinRef(r.gitDir, refSuffix, rev)
}

func (r *JJRepo) ListPins() ([]string, error) {
	return listPinRefs(r.gitDir)
}

func (r *JJRepo) DeletePin(ref string) error {
	return deletePinRef(r.gitDir, ref)
}

func (r *JJRepo) DeletePinsBatch(refs []string) ([]string, error) {
	return deletePinRefsBatch(r.gitDir, refs)
}

func parseJJTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	formats := []string{
		"2006-01-02 15:04:05.999999999 -07:00",
		"2006-01-02 15:04:05.999999999 -0700",
		"2006-01-02 15:04:05 -07:00",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// BlobOID implements BlobOIDer for JJRepo when backed by gitDir.
func (r *JJRepo) BlobOID(rev, path string) (string, error) {
	if rev == "working" || IsCommitMsgPath(path) {
		return "", errors.New("blob OID not supported for working copy or commit message")
	}
	cleanPath := filepath.Clean(strings.TrimPrefix(path, "/"))
	if cleanPath == "" || cleanPath == "." {
		return "", errors.New("invalid path")
	}
	if r.gitDir != "" && !r.conflicted(rev) {
		cf, err := r.cache.catFile(r.gitDir)
		if err == nil {
			return cf.BlobOID(rev, cleanPath)
		}
	}
	return "", errors.New("git backend unavailable for jj repo")
}

// ListGoFileBlobs implements GoBlobRepo for JJRepo when backed by gitDir.
func (r *JJRepo) ListGoFileBlobs(rev string) ([]GoFileBlob, error) {
	if rev == "" || rev == "working" {
		return nil, os.ErrNotExist
	}
	if r.gitDir != "" && !r.conflicted(rev) {
		cf, err := r.cache.catFile(r.gitDir)
		if err == nil {
			return cf.ListGoFileBlobs(rev)
		}
	}
	return nil, errors.New("git backend unavailable for jj repo")
}

// ListAllFileBlobs implements AllBlobRepo for JJRepo when backed by gitDir.
func (r *JJRepo) ListAllFileBlobs(rev string) ([]GoFileBlob, error) {
	if rev == "" || rev == "working" {
		return nil, os.ErrNotExist
	}
	if r.gitDir != "" && !r.conflicted(rev) {
		cf, err := r.cache.catFile(r.gitDir)
		if err == nil {
			return cf.ListAllFileBlobs(rev)
		}
	}
	return nil, errors.New("git backend unavailable for jj repo")
}

// StreamBlobs implements GoBlobRepo for JJRepo when backed by gitDir.
func (r *JJRepo) StreamBlobs(oids []string, visit func(idx int, oid string, data []byte)) error {
	if r.gitDir != "" {
		cf, err := r.cache.catFile(r.gitDir)
		if err == nil {
			return cf.StreamBlobs(oids, visit)
		}
	}
	return errors.New("git backend unavailable for jj repo")
}

// ReadDirGoFiles implements DirGoBlobRepo for JJRepo when backed by gitDir.
func (r *JJRepo) ReadDirGoFiles(rev, dir string) ([]GoFileBlobContent, error) {
	if rev == "" || rev == "working" || r.gitDir == "" || r.conflicted(rev) {
		return nil, fmt.Errorf("ReadDirGoFiles not supported on %s", rev)
	}
	cf, err := r.cache.catFile(r.gitDir)
	if err != nil {
		return nil, err
	}
	return cf.ReadDirGoFiles(rev, dir)
}

// ChangeDiffStats implements DiffStatRepo for JJRepo using cached batched Git queries
// with a batched jj diff.stat() fallback for conflicted or non-Git commits.
func (r *JJRepo) ChangeDiffStats(changes []*Change) map[string]DiffStat {
	out := make(map[string]DiffStat, len(changes))
	var gitChanges []*Change
	var gitRevs []string
	seenGit := make(map[string]bool)
	var jjFallback []*Change

	for _, c := range changes {
		if c == nil || c.Rev == "" || c.Empty {
			continue
		}
		if st, ok := r.cache.getDiffStat(c.Parent, c.Rev); ok {
			out[c.Rev] = st
			continue
		}
		parentOK := c.Parent == "" || strings.Trim(c.Parent, "0") == "" || (IsImmutableHexRev(c.Parent) && !r.conflicted(c.Parent))
		if r.gitDir != "" && IsImmutableHexRev(c.Rev) && !r.conflicted(c.Rev) && parentOK {
			gitChanges = append(gitChanges, c)
			if !seenGit[c.Rev] {
				seenGit[c.Rev] = true
				gitRevs = append(gitRevs, c.Rev)
			}
		} else {
			jjFallback = append(jjFallback, c)
		}
	}

	if len(gitRevs) > 0 {
		stats, err := gitBatchCommitDiffStats(r.gitDir, r.root, gitRevs)
		if err == nil {
			for _, c := range gitChanges {
				if st, ok := stats[c.Rev]; ok {
					out[c.Rev] = st
					r.cache.putDiffStat(c.Parent, c.Rev, st)
				} else {
					jjFallback = append(jjFallback, c)
				}
			}
		} else {
			jjFallback = append(jjFallback, gitChanges...)
		}
	}

	if len(jjFallback) > 0 {
		var revExprs []string
		seenJJ := make(map[string]bool)
		for _, c := range jjFallback {
			if c.Rev != "" && !seenJJ[c.Rev] {
				seenJJ[c.Rev] = true
				revExprs = append(revExprs, c.Rev)
			}
		}
		parsed := make(map[string]DiffStat, len(revExprs))
		const jjChunkSize = 64
		for start := 0; start < len(revExprs); start += jjChunkSize {
			end := min(start+jjChunkSize, len(revExprs))
			if raw, err := r.runCmd("jj", "--ignore-working-copy", "--config", `ui.paginate="never"`, "log", "--no-graph", "-r", strings.Join(revExprs[start:end], "|"), "-T", jjDiffStatTemplate); err == nil {
				for _, rec := range strings.Split(string(raw), "\x1e") {
					fields := strings.Split(rec, "\x1f")
					if len(fields) != 4 {
						continue
					}
					rev := strings.TrimSpace(fields[0])
					filesN, err1 := strconv.Atoi(strings.TrimSpace(fields[1]))
					added, err2 := strconv.Atoi(strings.TrimSpace(fields[2]))
					deleted, err3 := strconv.Atoi(strings.TrimSpace(fields[3]))
					if rev != "" && err1 == nil && err2 == nil && err3 == nil {
						parsed[rev] = DiffStat{Files: filesN, Added: added, Deleted: deleted}
					}
				}
			}
		}
		for _, c := range jjFallback {
			if st, ok := parsed[c.Rev]; ok {
				out[c.Rev] = st
				r.cache.putDiffStat(c.Parent, c.Rev, st)
				continue
			}
			if files, err := r.Files(c.Parent, c.Rev); err == nil {
				st := diffStatFromFiles(files)
				out[c.Rev] = st
				r.cache.putDiffStat(c.Parent, c.Rev, st)
			}
		}
	}
	return out
}
