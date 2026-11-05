package vcs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitRepo implements the Repo interface for pure Git repositories.
type GitRepo struct {
	root    string
	gitDir  string
	cache   *Cache
	user    string
	headSHA string
	changes []*Change
}

func openGit(root string, cache *Cache) (*GitRepo, error) {
	if cache == nil {
		cache = NewCache()
	}
	gitDir, _ := resolveGitDir(root)
	if gitDir == "" {
		gitDir = filepath.Join(root, ".git")
	}
	r := &GitRepo{
		root:   root,
		gitDir: gitDir,
		cache:  cache,
	}
	cache.registerRepo(root, gitDir)

	r.user = cache.user(root, func() string {
		out, err := r.runCmd("config", "user.name")
		if err == nil {
			return strings.TrimSpace(string(out))
		}
		return ""
	})

	return r, nil
}

func (r *GitRepo) Kind() string { return "git" }
func (r *GitRepo) Root() string { return r.root }
func (r *GitRepo) User() string { return r.user }

func (r *GitRepo) runCmd(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.root
	return cmd.Output()
}

func (r *GitRepo) Changes() ([]*Change, error) {
	headSHA := ""
	if out, err := r.runCmd("--no-optional-locks", "rev-parse", "HEAD"); err == nil {
		headSHA = strings.TrimSpace(string(out))
	}

	if r.changes != nil && r.headSHA == headSHA {
		return r.changes, nil
	}

	revRange := ""
	if _, err := r.runCmd("--no-optional-locks", "rev-parse", "--verify", "-q", "@{upstream}"); err == nil {
		revRange = "@{upstream}..HEAD"
	}

	refsOut, _ := r.runCmd("--no-optional-locks", "for-each-ref", "--format=%(refname)",
		"refs/remotes/origin/HEAD",
		"refs/remotes/origin/main",
		"refs/remotes/origin/master")
	hasOriginHEAD := false
	hasOriginMain := false
	hasOriginMaster := false
	for _, ref := range strings.Split(string(refsOut), "\n") {
		ref = strings.TrimSpace(ref)
		switch ref {
		case "refs/remotes/origin/HEAD":
			hasOriginHEAD = true
		case "refs/remotes/origin/main":
			hasOriginMain = true
		case "refs/remotes/origin/master":
			hasOriginMaster = true
		}
	}

	if revRange == "" {
		if hasOriginHEAD {
			revRange = "origin/HEAD..HEAD"
		} else if hasOriginMain {
			revRange = "origin/main..HEAD"
		} else if hasOriginMaster {
			revRange = "origin/master..HEAD"
		} else {
			revRange = "-n 20 HEAD"
		}
	}

	// Query commits
	format := "%H%x1f%P%x1f%an <%ae>%x1f%aI%x1f%cn <%ce>%x1f%cI%x1f%T%x1f%B%x1e"
	args := []string{"--no-optional-locks", "log", "--format=" + format}
	if strings.HasPrefix(revRange, "-n") {
		args = append(args, "-n", "20", "HEAD")
	} else {
		args = append(args, revRange)
	}

	out, err := r.runCmd(args...)
	if err != nil {
		// Try HEAD alone if revRange failed
		out, _ = r.runCmd("--no-optional-locks", "log", "-n", "10", "--format="+format, "HEAD")
	}

	seen := make(map[string]bool)
	var changes []*Change

	// Synthetic working copy change if dirty
	statusOut, _ := r.runCmd("--no-optional-locks", "status", "--porcelain")
	if len(bytes.TrimSpace(statusOut)) > 0 && headSHA != "" {
		changes = append(changes, &Change{
			Key:     "working",
			Rev:     "working",
			Parent:  headSHA,
			Subject: "Working copy changes",
			Message: "Working copy changes",
			Author:  r.user,
			Date:    time.Now(),
			Current: true,
			Working: true,
		})
	}

	type rawCommit struct {
		sha           string
		parent        string
		author        string
		timeStr       string
		committer     string
		commitTimeStr string
		treeSHA       string
		body          string
	}

	rawCommits := bytes.Split(out, []byte("\x1e"))
	var parsed []rawCommit
	trees := make(map[string]string)
	subjectBySHA := make(map[string]string)

	for _, raw := range rawCommits {
		if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), nil) {
			continue
		}
		fields := bytes.SplitN(raw, []byte("\x1f"), 8)
		if len(fields) < 8 {
			continue
		}

		sha := strings.TrimSpace(string(fields[0]))
		if seen[sha] {
			continue
		}
		seen[sha] = true

		parents := strings.Fields(string(fields[1]))
		parent := ""
		if len(parents) > 0 {
			parent = strings.TrimSpace(parents[0])
		}
		author := cleanIdentity(string(fields[2]))
		timeStr := strings.TrimSpace(string(fields[3]))
		committer := cleanIdentity(string(fields[4]))
		commitTimeStr := strings.TrimSpace(string(fields[5]))
		treeSHA := strings.TrimSpace(string(fields[6]))
		body := string(fields[7])

		lines := strings.Split(body, "\n")
		if len(lines) > 0 {
			if firstLine := strings.TrimSpace(lines[0]); firstLine != "" {
				subjectBySHA[sha] = firstLine
			}
		}

		trees[sha] = treeSHA
		parsed = append(parsed, rawCommit{
			sha:           sha,
			parent:        parent,
			author:        author,
			timeStr:       timeStr,
			committer:     committer,
			commitTimeStr: commitTimeStr,
			treeSHA:       treeSHA,
			body:          body,
		})
	}

	var missingParents []string
	seenMissing := make(map[string]bool)
	addMissingParent := func(parent string) {
		if parent == "" || strings.Trim(parent, "0") == "" || seen[parent] || seenMissing[parent] {
			return
		}
		seenMissing[parent] = true
		missingParents = append(missingParents, parent)
	}
	if len(changes) > 0 && changes[0].Working {
		addMissingParent(changes[0].Parent)
	}
	for _, rc := range parsed {
		addMissingParent(rc.parent)
	}
	if len(missingParents) > 0 {
		if pOut, err := r.runCmd(append([]string{"--no-optional-locks", "log", "--no-walk", "--format=%H%x1f%s"}, missingParents...)...); err == nil {
			for _, line := range bytes.Split(pOut, []byte("\n")) {
				parts := bytes.SplitN(line, []byte("\x1f"), 2)
				if len(parts) == 2 {
					psha := strings.TrimSpace(string(parts[0]))
					psubj := strings.TrimSpace(string(parts[1]))
					if psha != "" && psubj != "" {
						subjectBySHA[psha] = psubj
					}
				}
			}
		}
	}

	for _, rc := range parsed {
		var date time.Time
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(rc.timeStr)); err == nil {
			date = t
		}
		var commitDate time.Time
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(rc.commitTimeStr)); err == nil {
			commitDate = t
		}

		subject := subjectBySHA[rc.sha]
		if subject == "" {
			subject = "(no commit message)"
		}

		key := extractGitKey(rc.sha, rc.body)

		// Check if empty via tree SHA comparison (avoids git diff --quiet subprocesses)
		empty := false
		if rc.parent != "" {
			parentTree := trees[rc.parent]
			if parentTree == "" {
				if tOut, err := r.runCmd("--no-optional-locks", "rev-parse", "--verify", rc.parent+"^{tree}"); err == nil {
					parentTree = strings.TrimSpace(string(tOut))
					trees[rc.parent] = parentTree
				}
			}
			if parentTree != "" && rc.treeSHA == parentTree {
				empty = true
			}
		}

		c := &Change{
			Key:           key,
			Rev:           rc.sha,
			Parent:        rc.parent,
			ParentSubject: subjectBySHA[rc.parent],
			Subject:       subject,
			Message:       rc.body,
			Author:        rc.author,
			Date:          date,
			Committer:     rc.committer,
			CommitDate:    commitDate,
			Current:       (rc.sha == headSHA),
			Empty:         empty,
		}
		changes = append(changes, c)
	}
	if len(changes) > 0 && changes[0].Working && changes[0].Parent != "" && changes[0].ParentSubject == "" {
		changes[0].ParentSubject = subjectBySHA[changes[0].Parent]
	}

	r.headSHA = headSHA
	r.changes = changes
	return changes, nil
}

func extractGitKey(sha, body string) string {
	// Check Change-Id trailer so amended commits preserve the same change key.
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Change-Id:") {
			cid := strings.TrimSpace(strings.TrimPrefix(line, "Change-Id:"))
			if cid != "" {
				return cid
			}
		}
	}
	return sha
}

func (r *GitRepo) Commit(rev string) (*Change, error) {
	if rev == "working" {
		changes, _ := r.Changes()
		for _, c := range changes {
			if c.Working {
				if c.Parent != "" && c.ParentSubject == "" {
					if pOut, err := r.runCmd("--no-optional-locks", "log", "-1", "--format=%s", c.Parent); err == nil {
						cp := *c
						cp.ParentSubject = strings.TrimSpace(string(pOut))
						return &cp, nil
					}
				}
				return c, nil
			}
		}
	} else if r.changes != nil {
		for _, c := range r.changes {
			if c != nil && !c.Working && c.Rev == rev {
				return c, nil
			}
		}
		var match *Change
		matches := 0
		for _, c := range r.changes {
			if c == nil || c.Working {
				continue
			}
			if c.Key == rev || (len(rev) >= 7 && strings.HasPrefix(c.Rev, rev)) {
				match = c
				matches++
			}
		}
		if matches == 1 {
			return match, nil
		}
	}

	format := "%H%x1f%P%x1f%an <%ae>%x1f%aI%x1f%cn <%ce>%x1f%cI%x1f%B"
	out, err := r.runCmd("--no-optional-locks", "log", "-1", "--format="+format, rev)
	if err != nil {
		return nil, fmt.Errorf("git commit %s: %w", rev, err)
	}

	out = bytes.TrimSuffix(bytes.TrimSuffix(out, []byte("\n")), []byte("\r"))
	fields := bytes.SplitN(out, []byte("\x1f"), 7)
	if len(fields) < 7 {
		return nil, fmt.Errorf("commit %s not found", rev)
	}

	sha := strings.TrimSpace(string(fields[0]))
	parents := strings.Fields(string(fields[1]))
	parent := ""
	if len(parents) > 0 {
		parent = parents[0]
	}
	author := cleanIdentity(string(fields[2]))
	committer := cleanIdentity(string(fields[4]))
	body := string(fields[6])

	lines := strings.Split(body, "\n")
	subject := "(no commit message)"
	if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
		subject = strings.TrimSpace(lines[0])
	}

	key := extractGitKey(sha, body)

	var date time.Time
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(fields[3]))); err == nil {
		date = t
	}
	var commitDate time.Time
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(fields[5]))); err == nil {
		commitDate = t
	}

	parentSubject := ""
	if parent != "" && strings.Trim(parent, "0") != "" {
		for _, ch := range r.changes {
			if ch != nil && ch.Rev == parent {
				if firstLine := strings.TrimSpace(strings.Split(ch.Message, "\n")[0]); firstLine != "" {
					parentSubject = firstLine
				}
				break
			}
		}
		if parentSubject == "" {
			if pOut, err := r.runCmd("--no-optional-locks", "log", "-1", "--format=%s", parent); err == nil {
				parentSubject = strings.TrimSpace(string(pOut))
			}
		}
	}

	return &Change{
		Key:           key,
		Rev:           sha,
		Parent:        parent,
		ParentSubject: parentSubject,
		Subject:       subject,
		Message:       body,
		Author:        author,
		Date:          date,
		Committer:     committer,
		CommitDate:    commitDate,
	}, nil
}

func (r *GitRepo) Files(baseRev, targetRev string) ([]*File, error) {
	return gitDiffFiles(r.gitDir, r.root, baseRev, targetRev)
}

func (r *GitRepo) ListDir(rev, dirPath string) ([]string, error) {
	return gitListDir(r.cache, r.gitDir, r.root, rev, dirPath)
}

func (r *GitRepo) ListGoFiles(rev string) ([]string, error) {
	return gitListGoFiles(r.cache, r.gitDir, r.root, rev)
}

func (r *GitRepo) Content(rev, path string) ([]byte, error) {
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

	cf, err := r.cache.catFile(r.gitDir)
	if err != nil {
		out, err := r.runCmd("--no-optional-locks", "show", rev+":"+cleanPath)
		if err != nil {
			if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "fatal: path") || strings.Contains(err.Error(), "exists on disk, but not in") {
				return nil, os.ErrNotExist
			}
			return nil, err
		}
		return out, nil
	}
	return cf.ReadBlob(rev + ":" + cleanPath)
}

func (r *GitRepo) Pin(refSuffix, rev string) error {
	return pinRef(r.gitDir, refSuffix, rev)
}

func (r *GitRepo) ListPins() ([]string, error) {
	return listPinRefs(r.gitDir)
}

func (r *GitRepo) DeletePin(ref string) error {
	return deletePinRef(r.gitDir, ref)
}

// BlobOID implements BlobOIDer for GitRepo.
func (r *GitRepo) BlobOID(rev, path string) (string, error) {
	if rev == "working" || IsCommitMsgPath(path) {
		return "", errors.New("blob OID not supported for working copy or commit message")
	}
	cleanPath := filepath.Clean(strings.TrimPrefix(path, "/"))
	if cleanPath == "" || cleanPath == "." {
		return "", errors.New("invalid path")
	}
	cf, err := r.cache.catFile(r.gitDir)
	if err != nil {
		return "", err
	}
	return cf.BlobOID(rev, cleanPath)
}

// ListGoFileBlobs implements GoBlobRepo for GitRepo.
func (r *GitRepo) ListGoFileBlobs(rev string) ([]GoFileBlob, error) {
	if rev == "" || rev == "working" {
		return nil, os.ErrNotExist
	}
	cf, err := r.cache.catFile(r.gitDir)
	if err != nil {
		return nil, err
	}
	return cf.ListGoFileBlobs(rev)
}

// StreamBlobs implements GoBlobRepo for GitRepo.
func (r *GitRepo) StreamBlobs(oids []string, visit func(idx int, oid string, data []byte)) error {
	cf, err := r.cache.catFile(r.gitDir)
	if err != nil {
		return err
	}
	return cf.StreamBlobs(oids, visit)
}

// ReadDirGoFiles implements DirGoBlobRepo for GitRepo.
func (r *GitRepo) ReadDirGoFiles(rev, dir string) ([]GoFileBlobContent, error) {
	if rev == "" || rev == "working" {
		return nil, fmt.Errorf("ReadDirGoFiles not supported on %s", rev)
	}
	cf, err := r.cache.catFile(r.gitDir)
	if err != nil {
		return nil, err
	}
	return cf.ReadDirGoFiles(rev, dir)
}
