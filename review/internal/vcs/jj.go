package vcs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const jjLogTemplate = `change_id ++ "\x1f" ++ commit_id ++ "\x1f" ++ parents.map(|p| p.commit_id()).join(" ") ++ "\x1f" ++ parents.map(|p| p.change_id()).join(" ") ++ "\x1f" ++ parents.map(|p| p.description().first_line()).join("\n") ++ "\x1f" ++ author.name() ++ " <" ++ author.email() ++ ">" ++ "\x1f" ++ author.timestamp() ++ "\x1f" ++ committer.name() ++ " <" ++ committer.email() ++ ">" ++ "\x1f" ++ committer.timestamp() ++ "\x1f" ++ empty ++ "\x1f" ++ current_working_copy ++ "\x1f" ++ description ++ "\x1e"`

// JJRepo implements the Repo interface for Jujutsu repositories.
type JJRepo struct {
	root        string
	mainRepoDir string
	gitDir      string
	cache       *Cache
	user        string
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
				if !filepath.IsAbs(t) {
					t = filepath.Clean(filepath.Join(filepath.Dir(gitTarget), t))
				}
				r.gitDir = t
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

func (r *JJRepo) Changes() ([]*Change, error) {
	if r.changes != nil {
		return r.changes, nil
	}

	opID := r.getOpID()
	if opID != "" && r.cache != nil {
		r.cache.mu.Lock()
		entry, ok := r.cache.jjChanges[r.root]
		r.cache.mu.Unlock()
		if ok && entry.opID == opID && time.Since(entry.at) < jjCacheTTL {
			r.changes = entry.changes
			return entry.changes, nil
		}
	}

	out, err := r.runCmd("jj", "--config", `ui.paginate="never"`, "log", "--no-graph", "-r", "mutable()", "-T", jjLogTemplate)
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

	r.changes = changes
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
	changes, err := r.Changes()
	if err == nil {
		for _, c := range changes {
			if strings.HasPrefix(c.Rev, rev) || strings.HasPrefix(c.Key, rev) {
				return c, nil
			}
		}
	}

	args := []string{"--config", `ui.paginate="never"`}
	if rev != "@" && !strings.Contains(rev, "@") {
		args = append(args, "--ignore-working-copy")
	}
	args = append(args, "log", "--no-graph", "-r", rev, "-T", jjLogTemplate)
	out, err := r.runCmd("jj", args...)
	if err != nil {
		return nil, fmt.Errorf("resolving jj commit %s: %w", rev, err)
	}

	raw := bytes.TrimRight(out, "\x1e\n")
	if c, ok := r.parseChangeRecord(raw); ok {
		return c, nil
	}
	return nil, fmt.Errorf("commit %s not found", rev)
}

func (r *JJRepo) Files(baseRev, targetRev string) ([]*File, error) {
	if r.gitDir != "" {
		files, err := gitDiffFiles(r.gitDir, r.root, baseRev, targetRev)
		if err == nil {
			return files, nil
		}
	}

	// Fallback to jj diff --summary
	files := []*File{
		{
			Path:   CommitMsgPath,
			Status: FileModified,
		},
	}

	var jjDiffArgs []string
	if baseRev == "" || strings.Trim(baseRev, "0") == "" {
		jjDiffArgs = []string{"--config", `ui.paginate="never"`, "diff", "-r", targetRev, "--summary"}
	} else {
		jjDiffArgs = []string{"--config", `ui.paginate="never"`, "diff", "--from", baseRev, "--to", targetRev, "--summary"}
	}
	out, err := r.runCmd("jj", jjDiffArgs...)
	if err != nil {
		return files, nil
	}

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 3 {
			continue
		}
		statusChar := line[0]
		path := strings.TrimSpace(line[1:])
		var status FileStatus
		switch statusChar {
		case 'A':
			status = FileAdded
		case 'D':
			status = FileDeleted
		case 'R':
			status = FileRenamed
		default:
			status = FileModified
		}
		files = append(files, &File{
			Path:   path,
			Status: status,
		})
	}

	return files, nil
}

func (r *JJRepo) ListDir(rev, dirPath string) ([]string, error) {
	if r.gitDir != "" {
		names, err := gitListDir(r.cache, r.gitDir, r.root, rev, dirPath)
		if err == nil && names != nil {
			return names, nil
		}
	}

	cleanDir := strings.Trim(filepath.ToSlash(dirPath), "/")
	if cleanDir == "." {
		cleanDir = ""
	}

	args := []string{"--ignore-working-copy", "--config", `ui.paginate="never"`, "file", "list"}
	if rev != "" && rev != "working" {
		args = append(args, "-r", rev)
	}
	if cleanDir != "" {
		args = append(args, cleanDir)
	}

	outBytes, err := r.runCmd("jj", args...)
	if err != nil {
		return nil, nil
	}

	lines := strings.Split(strings.TrimSpace(string(outBytes)), "\n")
	var names []string
	prefix := cleanDir
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
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
	if r.gitDir != "" {
		files, err := gitListGoFiles(r.cache, r.gitDir, r.root, rev)
		if err == nil && files != nil {
			return files, nil
		}
	}

	if (rev == "" || rev == "working") && r.root != "" {
		var goFiles []string
		err := filepath.WalkDir(r.root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != r.root && shouldSkipGoDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if strings.HasSuffix(name, ".go") && name[0] != '.' && name[0] != '_' {
				rel, err := filepath.Rel(r.root, path)
				if err == nil {
					goFiles = append(goFiles, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		if err == nil {
			slices.Sort(goFiles)
			return goFiles, nil
		}
	}

	args := []string{"--ignore-working-copy", "--config", `ui.paginate="never"`, "file", "list"}
	if rev != "" && rev != "working" {
		args = append(args, "-r", rev)
	}

	outBytes, err := r.runCmd("jj", args...)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(outBytes)), "\n")
	var goFiles []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
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
		cleanRoot := filepath.Clean(r.root)
		fullPath := filepath.Clean(filepath.Join(cleanRoot, cleanPath))
		rel, err := filepath.Rel(cleanRoot, fullPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("path escapes repository root: %s", path)
		}
		return os.ReadFile(fullPath)
	}

	if r.gitDir != "" {
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

	// Fallback to jj file show
	out, err := r.runCmd("jj", "--ignore-working-copy", "--config", `ui.paginate="never"`, "file", "show", "-r", rev, cleanPath)
	if err != nil {
		if strings.Contains(err.Error(), "No such file") || strings.Contains(err.Error(), "No such path") || strings.Contains(err.Error(), "no such file") {
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
	if r.gitDir != "" {
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
	if r.gitDir != "" {
		cf, err := r.cache.catFile(r.gitDir)
		if err == nil {
			return cf.ListGoFileBlobs(rev)
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
	if rev == "" || rev == "working" || r.gitDir == "" {
		return nil, fmt.Errorf("ReadDirGoFiles not supported on %s", rev)
	}
	cf, err := r.cache.catFile(r.gitDir)
	if err != nil {
		return nil, err
	}
	return cf.ReadDirGoFiles(rev, dir)
}
