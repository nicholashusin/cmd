package vcs

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type catFileProc struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdoutPipe io.ReadCloser
	closeOnce  sync.Once
}

func (p *catFileProc) close() {
	p.closeOnce.Do(func() {
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.stdoutPipe != nil {
			_ = p.stdoutPipe.Close()
		}
		if p.cmd != nil {
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			_ = p.cmd.Wait()
		}
	})
}

// CatFileBatch manages a long-running git cat-file --batch subprocess.
type CatFileBatch struct {
	mu           sync.Mutex
	gitDir       string
	proc         *catFileProc
	stdinBuf     *bufio.Writer
	stdout       *bufio.Reader
	knownTrees   map[string]bool
	subtreeCache map[string]cachedSubtree
	closed       atomic.Bool
}

// NewCatFileBatch starts a git cat-file --batch process for the given git directory.
func NewCatFileBatch(gitDir string) (*CatFileBatch, error) {
	cmd := exec.Command("git", "--git-dir="+gitDir, "--no-optional-locks", "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("git cat-file stdin: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("git cat-file stdout: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdoutPipe.Close()
		return nil, fmt.Errorf("starting git cat-file: %w", err)
	}

	proc := &catFileProc{
		cmd:        cmd,
		stdin:      stdin,
		stdoutPipe: stdoutPipe,
	}

	cf := &CatFileBatch{
		gitDir:       gitDir,
		proc:         proc,
		stdinBuf:     bufio.NewWriterSize(stdin, 64*1024),
		stdout:       bufio.NewReaderSize(stdoutPipe, 256*1024),
		knownTrees:   make(map[string]bool),
		subtreeCache: make(map[string]cachedSubtree),
	}
	runtime.AddCleanup(cf, func(p *catFileProc) {
		p.close()
	}, proc)

	return cf, nil
}

func (c *CatFileBatch) readResponseLocked() (objType, hexSHA string, data []byte, err error) {
	if c.closed.Load() {
		return "", "", nil, fmt.Errorf("cat-file batch closed")
	}

	abort := func(err error) error {
		c.closed.Store(true)
		c.proc.close()
		return err
	}

	line, err := c.stdout.ReadString('\n')
	if err != nil {
		return "", "", nil, abort(fmt.Errorf("reading cat-file header: %w", err))
	}
	line = strings.TrimSpace(line)

	if strings.HasSuffix(line, "missing") {
		return "", "", nil, os.ErrNotExist
	}

	parts := strings.Split(line, " ")
	if len(parts) < 3 {
		return "", "", nil, abort(fmt.Errorf("unexpected cat-file header: %s", line))
	}

	size, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", nil, abort(fmt.Errorf("parsing object size: %w", err))
	}

	buf := make([]byte, size)
	if _, err := io.ReadFull(c.stdout, buf); err != nil {
		return "", "", nil, abort(fmt.Errorf("reading object content: %w", err))
	}

	// Consume trailing newline
	if _, err := c.stdout.ReadByte(); err != nil {
		return "", "", nil, abort(fmt.Errorf("reading cat-file trailing newline: %w", err))
	}

	return parts[1], parts[0], buf, nil
}

func (c *CatFileBatch) readObjectLocked(spec string) (objType, hexSHA string, data []byte, err error) {
	if strings.ContainsAny(spec, "\r\n") {
		return "", "", nil, fmt.Errorf("invalid object spec with newline")
	}
	if c.closed.Load() {
		return "", "", nil, fmt.Errorf("cat-file batch closed")
	}

	abort := func(err error) error {
		c.closed.Store(true)
		c.proc.close()
		return err
	}

	if _, err := io.WriteString(c.stdinBuf, spec+"\n"); err != nil {
		return "", "", nil, abort(fmt.Errorf("writing to cat-file: %w", err))
	}
	if err := c.stdinBuf.Flush(); err != nil {
		return "", "", nil, abort(fmt.Errorf("flushing to cat-file: %w", err))
	}

	return c.readResponseLocked()
}

func (c *CatFileBatch) hasTreeLocked(rev string) bool {
	if rev == "" {
		return false
	}
	if ok, seen := c.knownTrees[rev]; seen {
		return ok
	}
	objType, _, _, err := c.readObjectLocked(rev + "^{tree}")
	ok := (err == nil && objType == "tree")
	if ok {
		c.knownTrees[rev] = true
	}
	return ok
}

// HasTree reports whether rev resolves to a valid tree in this git repository.
func (c *CatFileBatch) HasTree(rev string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasTreeLocked(rev)
}

// ReadBlob retrieves the raw bytes of an object (e.g. "rev:path" or blob SHA).
func (c *CatFileBatch) ReadBlob(spec string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _, buf, err := c.readObjectLocked(spec)
	return buf, err
}

// ErrRevNotInGit is returned when a requested revision does not exist in the Git object store.
var ErrRevNotInGit = errors.New("rev not in git object store")

// BlobOID retrieves the hexadecimal object ID for cleanPath at rev by reading only
// its parent directory tree over the persistent cat-file pipe.
func (c *CatFileBatch) BlobOID(rev, cleanPath string) (string, error) {
	if rev == "" {
		return "", ErrRevNotInGit
	}
	cleanPath = filepath.Clean(strings.Trim(cleanPath, "/"))
	if cleanPath == "" || cleanPath == "." {
		return "", errors.New("invalid path")
	}

	dir := filepath.Dir(cleanPath)
	baseName := filepath.Base(cleanPath)
	if dir == "." || dir == "/" {
		dir = ""
	}

	spec := rev + "^{tree}"
	if dir != "" {
		spec = rev + ":" + filepath.ToSlash(dir)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	objType, hexSHA, buf, err := c.readObjectLocked(spec)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if dir != "" && c.hasTreeLocked(rev) {
				return "", os.ErrNotExist
			}
			return "", ErrRevNotInGit
		}
		return "", err
	}
	c.knownTrees[rev] = true
	if objType != "tree" {
		return "", os.ErrNotExist
	}

	hashLen := len(hexSHA) / 2
	if hashLen != 20 && hashLen != 32 {
		hashLen = 20
	}

	pos := 0
	for pos < len(buf) {
		sp := bytes.IndexByte(buf[pos:], ' ')
		if sp == -1 {
			break
		}
		mode := string(buf[pos : pos+sp])
		pos += sp + 1
		nul := bytes.IndexByte(buf[pos:], 0)
		if nul == -1 {
			break
		}
		name := string(buf[pos : pos+nul])
		rawOIDStart := pos + nul + 1
		pos = rawOIDStart + hashLen
		if pos > len(buf) {
			break
		}
		if name == baseName {
			if mode == "40000" || mode == "040000" || mode == "160000" {
				return "", os.ErrNotExist
			}
			rawOID := buf[rawOIDStart : rawOIDStart+hashLen]
			return hex.EncodeToString(rawOID), nil
		}
	}
	return "", os.ErrNotExist
}

// ListTree lists direct non-directory files inside cleanDir at rev over the persistent cat-file pipe.
// If rev exists in gitDir and cleanDir does not exist, it returns an empty non-nil slice and nil error.
// If rev itself does not exist in gitDir, it returns os.ErrNotExist so callers like JJRepo can fall back.
func (c *CatFileBatch) ListTree(rev, cleanDir string) ([]string, error) {
	cleanDir = strings.Trim(cleanDir, "/")
	spec := rev + "^{tree}"
	if cleanDir != "" {
		spec = rev + ":" + cleanDir
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	objType, hexSHA, buf, err := c.readObjectLocked(spec)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if cleanDir != "" && c.hasTreeLocked(rev) {
				return []string{}, nil
			}
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	c.knownTrees[rev] = true
	if objType != "tree" {
		return []string{}, nil
	}

	hashLen := len(hexSHA) / 2
	if hashLen != 20 && hashLen != 32 {
		hashLen = 20
	}

	names := []string{}
	pos := 0
	for pos < len(buf) {
		sp := bytes.IndexByte(buf[pos:], ' ')
		if sp == -1 {
			break
		}
		mode := string(buf[pos : pos+sp])
		pos += sp + 1
		nul := bytes.IndexByte(buf[pos:], 0)
		if nul == -1 {
			break
		}
		name := string(buf[pos : pos+nul])
		pos += nul + 1 + hashLen
		if pos > len(buf) {
			break
		}
		if mode != "40000" && mode != "040000" && mode != "160000" {
			names = append(names, name)
		}
	}
	return names, nil
}

// shouldSkipGoDir returns true if directory name matches standard directories to ignore.
func shouldSkipGoDir(name string) bool {
	return name == "" || name[0] == '.' || name[0] == '_' || name == "testdata" || name == "vendor" || name == "node_modules"
}

type treeChild struct {
	name string
	oid  string
}

type cachedSubtree struct {
	subdirs []treeChild
	goFiles []treeChild
}

const maxCachedSubtrees = 16384

// nextGitTreeEntry parses one binary Git tree entry from buf.
// Returns mode, name, hex-encoded entrySHA, the remaining bytes, and ok.
func nextGitTreeEntry(buf []byte, hashLen int) (mode, name, entrySHA string, rest []byte, ok bool) {
	if hashLen != 20 && hashLen != 32 {
		hashLen = 20
	}
	sp := bytes.IndexByte(buf, ' ')
	if sp == -1 {
		return "", "", "", buf, false
	}
	mode = string(buf[:sp])
	buf = buf[sp+1:]
	nul := bytes.IndexByte(buf, 0)
	if nul == -1 {
		return "", "", "", buf, false
	}
	name = string(buf[:nul])
	buf = buf[nul+1:]
	if len(buf) < hashLen {
		return "", "", "", buf, false
	}
	entrySHA = hex.EncodeToString(buf[:hashLen])
	return mode, name, entrySHA, buf[hashLen:], true
}

func parseSubtree(buf []byte, hexSHA string) cachedSubtree {
	hashLen := len(hexSHA) / 2
	if hashLen != 20 && hashLen != 32 {
		hashLen = 20
	}

	var st cachedSubtree
	raw := buf
	for len(raw) > 0 {
		mode, name, entrySHA, rest, ok := nextGitTreeEntry(raw, hashLen)
		if !ok {
			break
		}
		raw = rest

		if mode == "40000" || mode == "040000" {
			if !shouldSkipGoDir(name) {
				st.subdirs = append(st.subdirs, treeChild{name: name, oid: entrySHA})
			}
		} else if mode == "160000" {
			// Submodule gitlink: skip
		} else {
			if strings.HasSuffix(name, ".go") && len(name) > 0 && name[0] != '.' && name[0] != '_' {
				st.goFiles = append(st.goFiles, treeChild{name: name, oid: entrySHA})
			}
		}
	}
	return st
}

// ListGoFileBlobs recursively finds all .go files at rev along with their Git blob OIDs,
// leveraging content-addressed subtree caching and pipelined cat-file streaming.
func (c *CatFileBatch) ListGoFileBlobs(rev string) ([]GoFileBlob, error) {
	if rev == "" {
		return nil, os.ErrNotExist
	}

	c.mu.Lock()
	objType, hexSHA, buf, err := c.readObjectLocked(rev + "^{tree}")
	if err != nil {
		c.mu.Unlock()
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	c.knownTrees[rev] = true
	if objType != "tree" {
		c.mu.Unlock()
		return nil, os.ErrNotExist
	}

	rootSubtree := parseSubtree(buf, hexSHA)
	if IsImmutableHexRev(hexSHA) {
		if len(c.subtreeCache) >= maxCachedSubtrees {
			c.subtreeCache = make(map[string]cachedSubtree)
		}
		c.subtreeCache[hexSHA] = rootSubtree
	}
	c.mu.Unlock()

	type treeItem struct {
		dir string
		oid string
	}

	var blobs []GoFileBlob
	for _, f := range rootSubtree.goFiles {
		blobs = append(blobs, GoFileBlob{Path: f.name, OID: f.oid})
	}

	var currentLevel []treeItem
	for _, sub := range rootSubtree.subdirs {
		currentLevel = append(currentLevel, treeItem{dir: sub.name, oid: sub.oid})
	}

	for len(currentLevel) > 0 {
		var nextLevel []treeItem
		var uncachedItems []treeItem

		// Partition into cached hits vs misses under c.mu
		c.mu.Lock()
		for _, item := range currentLevel {
			if st, ok := c.subtreeCache[item.oid]; ok {
				for _, f := range st.goFiles {
					p := f.name
					if item.dir != "" {
						p = item.dir + "/" + f.name
					}
					blobs = append(blobs, GoFileBlob{Path: p, OID: f.oid})
				}
				for _, sub := range st.subdirs {
					subDir := sub.name
					if item.dir != "" {
						subDir = item.dir + "/" + sub.name
					}
					nextLevel = append(nextLevel, treeItem{dir: subDir, oid: sub.oid})
				}
			} else {
				uncachedItems = append(uncachedItems, item)
			}
		}
		c.mu.Unlock()

		// Process uncached items in chunks of up to 128
		const chunkSize = 128
		for start := 0; start < len(uncachedItems); start += chunkSize {
			end := min(start+chunkSize, len(uncachedItems))
			chunk := uncachedItems[start:end]

			type queryResult struct {
				buf     []byte
				hexSHA  string
				objType string
				err     error
			}
			resultsByOID := make(map[string]*queryResult)
			var uniqueOIDs []string
			for _, item := range chunk {
				if _, seen := resultsByOID[item.oid]; !seen {
					resultsByOID[item.oid] = nil
					uniqueOIDs = append(uniqueOIDs, item.oid)
				}
			}

			c.mu.Lock()
			if c.closed.Load() {
				c.mu.Unlock()
				return nil, fmt.Errorf("cat-file batch closed")
			}

			// Pipeline all writes
			for _, oid := range uniqueOIDs {
				if _, err := io.WriteString(c.stdinBuf, oid+"\n"); err != nil {
					c.closed.Store(true)
					c.proc.close()
					c.mu.Unlock()
					return nil, fmt.Errorf("writing tree to cat-file: %w", err)
				}
			}
			if err := c.stdinBuf.Flush(); err != nil {
				c.closed.Store(true)
				c.proc.close()
				c.mu.Unlock()
				return nil, fmt.Errorf("flushing tree to cat-file: %w", err)
			}

			// Pipeline all reads
			for _, oid := range uniqueOIDs {
				objType, hexSHA, buf, err := c.readResponseLocked()
				resultsByOID[oid] = &queryResult{
					buf:     buf,
					hexSHA:  hexSHA,
					objType: objType,
					err:     err,
				}
			}

			// Parse and cache under c.mu
			for _, item := range chunk {
				res := resultsByOID[item.oid]
				if res == nil || res.err != nil || res.objType != "tree" {
					continue
				}
				st, cached := c.subtreeCache[item.oid]
				if !cached {
					st = parseSubtree(res.buf, res.hexSHA)
					if IsImmutableHexRev(item.oid) {
						if len(c.subtreeCache) >= maxCachedSubtrees {
							c.subtreeCache = make(map[string]cachedSubtree)
						}
						c.subtreeCache[item.oid] = st
					}
				}
				for _, f := range st.goFiles {
					p := f.name
					if item.dir != "" {
						p = item.dir + "/" + f.name
					}
					blobs = append(blobs, GoFileBlob{Path: p, OID: f.oid})
				}
				for _, sub := range st.subdirs {
					subDir := sub.name
					if item.dir != "" {
						subDir = item.dir + "/" + sub.name
					}
					nextLevel = append(nextLevel, treeItem{dir: subDir, oid: sub.oid})
				}
			}
			c.mu.Unlock()
		}

		currentLevel = nextLevel
	}

	slices.SortFunc(blobs, func(a, b GoFileBlob) int {
		return strings.Compare(a.Path, b.Path)
	})
	return blobs, nil
}

// ListGoFiles recursively finds all .go files at rev over the persistent cat-file pipe,
// skipping directories matching shouldSkipGoDir. It leverages ListGoFileBlobs with subtree caching.
func (c *CatFileBatch) ListGoFiles(rev string) ([]string, error) {
	blobs, err := c.ListGoFileBlobs(rev)
	if err != nil {
		return nil, err
	}
	res := make([]string, len(blobs))
	for i, b := range blobs {
		res[i] = b.Path
	}
	return res, nil
}

// StreamBlobs streams raw blob contents for the given slice of hexadecimal OIDs in pipelined chunks.
func (c *CatFileBatch) StreamBlobs(oids []string, visit func(idx int, oid string, data []byte)) error {
	const chunkSize = 128
	for start := 0; start < len(oids); start += chunkSize {
		end := min(start+chunkSize, len(oids))
		chunk := oids[start:end]

		type chunkResult struct {
			idx   int
			oid   string
			data  []byte
			found bool
		}
		var results []chunkResult

		c.mu.Lock()
		if c.closed.Load() {
			c.mu.Unlock()
			return fmt.Errorf("cat-file batch closed")
		}

		// Write all OIDs in chunk
		for _, oid := range chunk {
			if strings.ContainsAny(oid, "\r\n") {
				c.mu.Unlock()
				return fmt.Errorf("invalid oid with newline")
			}
			if _, err := io.WriteString(c.stdinBuf, oid+"\n"); err != nil {
				c.closed.Store(true)
				c.proc.close()
				c.mu.Unlock()
				return fmt.Errorf("writing to cat-file: %w", err)
			}
		}
		if err := c.stdinBuf.Flush(); err != nil {
			c.closed.Store(true)
			c.proc.close()
			c.mu.Unlock()
			return fmt.Errorf("flushing to cat-file: %w", err)
		}

		// Read all responses for chunk
		for k, oid := range chunk {
			_, _, data, err := c.readResponseLocked()
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				c.mu.Unlock()
				return err
			}
			results = append(results, chunkResult{
				idx:   start + k,
				oid:   oid,
				data:  data,
				found: err == nil,
			})
		}
		c.mu.Unlock()

		for _, r := range results {
			if r.found {
				visit(r.idx, r.oid, r.data)
			}
		}
	}
	return nil
}

// GoFileBlobContent holds the name, SHA/OID, and raw blob contents of a Go source file.
type GoFileBlobContent struct {
	Name string
	OID  string
	Data []byte
}

// ReadDirGoFiles reads the Git tree at rev:dir, collects all non-test .go files,
// and streams their blob contents in a single pipelined operation.
func (c *CatFileBatch) ReadDirGoFiles(rev, dir string) ([]GoFileBlobContent, error) {
	spec := rev + "^{tree}"
	cleanDir := path.Clean(strings.TrimPrefix(filepath.ToSlash(dir), "/"))
	if cleanDir != "" && cleanDir != "." {
		spec = rev + ":" + cleanDir
	}

	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return nil, fmt.Errorf("cat-file batch closed")
	}
	objType, hexSHA, treeBuf, err := c.readObjectLocked(spec)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if cleanDir != "" && cleanDir != "." && c.hasTreeLocked(rev) {
				c.mu.Unlock()
				return nil, os.ErrNotExist
			}
			c.mu.Unlock()
			return nil, ErrRevNotInGit
		}
		c.mu.Unlock()
		return nil, err
	}
	c.knownTrees[rev] = true
	c.mu.Unlock()
	if objType != "tree" {
		return nil, os.ErrNotExist
	}

	hashLen := len(hexSHA) / 2
	if hashLen != 20 && hashLen != 32 {
		hashLen = 20
	}

	type entryInfo struct {
		name string
		oid  string
	}
	var entries []entryInfo
	raw := treeBuf
	for len(raw) > 0 {
		mode, name, sha, rest, ok := nextGitTreeEntry(raw, hashLen)
		if !ok {
			break
		}
		raw = rest
		if mode == "40000" || mode == "040000" || mode == "160000" {
			continue
		}
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		entries = append(entries, entryInfo{name: name, oid: sha})
	}

	if len(entries) == 0 {
		return nil, nil
	}

	slices.SortFunc(entries, func(a, b entryInfo) int {
		return strings.Compare(a.name, b.name)
	})

	oids := make([]string, len(entries))
	for i, e := range entries {
		oids[i] = e.oid
	}

	contents := make([]GoFileBlobContent, len(entries))
	for i, e := range entries {
		contents[i] = GoFileBlobContent{Name: e.name, OID: e.oid}
	}

	err = c.StreamBlobs(oids, func(idx int, oid string, data []byte) {
		if idx >= 0 && idx < len(contents) {
			contents[idx].Data = data
		}
	})
	if err != nil {
		return nil, err
	}
	return contents, nil
}

// Close terminates the persistent cat-file process.
func (c *CatFileBatch) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed.Load() {
		return nil
	}
	c.closed.Store(true)
	c.proc.close()
	return nil
}
