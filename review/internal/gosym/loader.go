package gosym

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/vcs"
)

func isImmutableRev(rev string) bool {
	return rev == "" || vcs.IsImmutableHexRev(rev)
}

var sharedDiskCache = newDiskCache(32)

type diskCacheKey struct {
	goos       string
	goarch     string
	importPath string
	absDir     string
	tagsSig    string
}

type diskCache struct {
	mu            sync.RWMutex
	pkgs          map[diskCacheKey]*types.Package
	pkgSet        map[*types.Package]struct{}
	pkgDirs       map[*types.Package]string
	pkgFsets      map[*types.Package]*token.FileSet
	modMu         sync.RWMutex
	modInfos      map[string]*moduleInfo
	semverDirs    map[string]string
	fsetMu        sync.Mutex
	fset          *token.FileSet
	inflight      map[diskCacheKey]chan struct{}
	goFilesMu     sync.Mutex
	goFiles       []string
	repoImportsMu sync.Mutex
	repoImports   []string

	// Directory packages for immutable revisions (impls.go):
	dirPkgs     map[diskCacheKey]map[string]*types.Package
	dirInflight map[diskCacheKey]chan struct{}

	// Bloom filter index for immutable revisions (refs.go & impls.go):
	fileIndexMu       sync.Mutex
	fileIndex         *repoFileIndex
	fileIndexInflight chan struct{}

	// Cached changed files map per (baseRev, targetRev):
	changedFilesMu sync.Mutex
	changedFiles   map[string]map[string]bool

	// Bounded LRU checked package cache with singleflight:
	checkedPkgs *checkedPkgLRU

	// Shared content-addressed blob cache & fallback prev index func:
	blobCache     *blobIndexCache
	prevIndexFunc func() (string, *repoFileIndex)

	// Results caches for immutable revisions:
	refsCache  *refsLRU
	implsCache *implsLRU
}

func newDiskCache(checkedCap ...int) *diskCache {
	cap := 64
	if len(checkedCap) > 0 && checkedCap[0] > 0 {
		cap = checkedCap[0]
	}
	return &diskCache{
		blobCache:    sharedBlobIndexCache,
		pkgs:         make(map[diskCacheKey]*types.Package),
		pkgSet:       make(map[*types.Package]struct{}),
		pkgDirs:      make(map[*types.Package]string),
		pkgFsets:     make(map[*types.Package]*token.FileSet),
		modInfos:     make(map[string]*moduleInfo),
		semverDirs:   make(map[string]string),
		fset:         token.NewFileSet(),
		inflight:     make(map[diskCacheKey]chan struct{}),
		dirPkgs:      make(map[diskCacheKey]map[string]*types.Package),
		dirInflight:  make(map[diskCacheKey]chan struct{}),
		changedFiles: make(map[string]map[string]bool),
		checkedPkgs:  newCheckedPkgLRU(cap),
		refsCache:    newRefsLRU(128),
		implsCache:   newImplsLRU(64),
	}
}

func (dc *diskCache) registerPkg(p *types.Package) {
	if dc == nil || p == nil {
		return
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.pkgSet[p] = struct{}{}
}

func (dc *diskCache) dirFor(p *types.Package) string {
	if dc == nil || p == nil {
		return ""
	}
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.pkgDirs[p]
}

func (dc *diskCache) registerPkgDir(pkg *types.Package, absDir string) {
	if dc == nil || pkg == nil || absDir == "" {
		return
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.pkgDirs[pkg] = absDir
}

func (dc *diskCache) registerPkgFset(pkg *types.Package, fset *token.FileSet) {
	if dc == nil || pkg == nil || fset == nil {
		return
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.pkgFsets[pkg] = fset
}

func (dc *diskCache) fsetForPkg(pkg *types.Package) *token.FileSet {
	if dc == nil || pkg == nil {
		return nil
	}
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.pkgFsets[pkg]
}

func (dc *diskCache) positionForPkg(pkg *types.Package, pos token.Pos) (token.Position, bool) {
	if dc == nil || pkg == nil || !pos.IsValid() {
		return token.Position{}, false
	}
	dc.mu.RLock()
	fset := dc.pkgFsets[pkg]
	dc.mu.RUnlock()
	if fset != nil {
		return fset.Position(pos), true
	}
	if dc.has(pkg) {
		return dc.position(pos), true
	}
	return token.Position{}, false
}

func (dc *diskCache) getChangedFiles(repo vcs.Repo, baseRev, targetRev string) map[string]bool {
	if repo == nil || baseRev == "" || targetRev == "" {
		return nil
	}
	key := baseRev + "\x00" + targetRev
	if dc != nil {
		dc.changedFilesMu.Lock()
		if m, ok := dc.changedFiles[key]; ok {
			dc.changedFilesMu.Unlock()
			return m
		}
		dc.changedFilesMu.Unlock()
	}

	m := make(map[string]bool)
	if files, err := repo.Files(baseRev, targetRev); err == nil {
		for _, f := range files {
			if f != nil && f.Status != vcs.FileDeleted {
				m[f.Path] = true
			}
		}
	}

	if dc != nil {
		dc.changedFilesMu.Lock()
		dc.changedFiles[key] = m
		dc.changedFilesMu.Unlock()
	}
	return m
}

// 4,096-bit Bloom filter per file for ultra-fast symbol and import matching.
type repoFileEntry struct {
	path             string
	dir              string
	pkgName          string
	oid              string
	constraintHeader string
	bits             [64]uint64
	imports          []string
	importsExact     bool
	hasTypeDecl      bool
	hasInterfaceKw   bool
}

type repoFileIndex struct {
	files           []repoFileEntry
	byPath          map[string]int
	dirFiles        map[string][]int
	dirPkgFiles     map[string]map[string][]string
	dirHasType      map[string]bool
	dirHasInterface map[string]bool
}

func (idx *repoFileIndex) mayContainIdent(path, ident string) bool {
	if idx == nil {
		return true
	}
	i, ok := idx.byPath[path]
	if !ok {
		return true
	}
	return testIdentBloom(&idx.files[i].bits, ident)
}

func (idx *repoFileIndex) mayContainImport(path, pkgPath string) bool {
	if idx == nil {
		return true
	}
	i, ok := idx.byPath[path]
	if !ok {
		return true
	}
	if idx.files[i].importsExact {
		return slices.Contains(idx.files[i].imports, pkgPath)
	}
	return testImportBloom(&idx.files[i].bits, pkgPath)
}

func (idx *repoFileIndex) hasImport(path, pkgPath string) bool {
	return idx.mayContainImport(path, pkgPath)
}

func (idx *repoFileIndex) isImportExact(path string) bool {
	if idx == nil {
		return false
	}
	i, ok := idx.byPath[path]
	return ok && idx.files[i].importsExact
}

const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

func fnv1a64(s string) uint64 {
	var h uint64 = fnvOffset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

func bloomBitPositions(h uint64) (uint16, uint16, uint16, uint16) {
	b0 := uint16(h & 4095)
	b1 := uint16((h >> 16) & 4095)
	b2 := uint16((h >> 32) & 4095)
	b3 := uint16((h >> 48) & 4095)
	return b0, b1, b2, b3
}

func setBloomBit(bits *[64]uint64, pos uint16) {
	word := pos / 64
	bit := pos % 64
	bits[word] |= 1 << bit
}

func testBloomBit(bits *[64]uint64, pos uint16) bool {
	word := pos / 64
	bit := pos % 64
	return (bits[word] & (1 << bit)) != 0
}

func addIdentBloom(bits *[64]uint64, ident string) {
	h := fnv1a64(ident)
	b0, b1, b2, b3 := bloomBitPositions(h)
	setBloomBit(bits, b0)
	setBloomBit(bits, b1)
	setBloomBit(bits, b2)
	setBloomBit(bits, b3)
}

func testIdentBloom(bits *[64]uint64, ident string) bool {
	h := fnv1a64(ident)
	b0, b1, b2, b3 := bloomBitPositions(h)
	return testBloomBit(bits, b0) &&
		testBloomBit(bits, b1) &&
		testBloomBit(bits, b2) &&
		testBloomBit(bits, b3)
}

func addImportBloom(bits *[64]uint64, pkgPath string) {
	addIdentBloom(bits, "imp:"+pkgPath)
}

func testImportBloom(bits *[64]uint64, pkgPath string) bool {
	return testIdentBloom(bits, "imp:"+pkgPath)
}

func scanGoHeaderImports(b []byte) (imports []string, exact bool, pkgName string) {
	fset := token.NewFileSet()
	astF, err := parser.ParseFile(fset, "", b, parser.ImportsOnly)
	if astF == nil || !astF.Package.IsValid() || astF.Name == nil || astF.Name.Name == "_" || astF.Name.Name == "" {
		return nil, false, ""
	}
	pkgName = astF.Name.Name
	unquoteErr := false
	for _, spec := range astF.Imports {
		if spec == nil || spec.Path == nil {
			continue
		}
		imp, uerr := strconv.Unquote(spec.Path.Value)
		if uerr != nil {
			unquoteErr = true
			continue
		}
		imports = append(imports, imp)
	}
	exact = (err == nil && !unquoteErr)
	return imports, exact, pkgName
}

func extractConstraintHeader(b []byte) string {
	if !bytes.Contains(b, []byte("go:build")) && !bytes.Contains(b, []byte("+build")) {
		return ""
	}
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(b))
	var s scanner.Scanner
	s.Init(file, b, nil, scanner.ScanComments)
	pkgOffset := len(b)
	for {
		pos, tok, _ := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT || tok == token.SEMICOLON {
			continue
		}
		pkgOffset = file.Offset(pos)
		break
	}
	if pkgOffset > len(b) {
		pkgOffset = len(b)
	}
	header := b[:pkgOffset]
	if bytes.Contains(header, []byte("go:build")) || bytes.Contains(header, []byte("+build")) {
		return string(header)
	}
	return ""
}

func scanFileBloomAndImports(b []byte, entry *repoFileEntry, collectImports bool, importSet map[string]bool) {
	entry.constraintHeader = extractConstraintHeader(b)

	imports, exact, pkgName := scanGoHeaderImports(b)
	entry.pkgName = pkgName
	entry.imports = imports
	entry.importsExact = exact
	for _, imp := range imports {
		if imp == "" {
			continue
		}
		addImportBloom(&entry.bits, imp)
		if collectImports && imp != "C" && imp != "unsafe" && !strings.HasPrefix(imp, ".") && importSet != nil {
			importSet[imp] = true
		}
	}

	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(b))
	var s scanner.Scanner
	s.Init(file, b, nil, 0)
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.IDENT {
			addIdentBloom(&entry.bits, lit)
			continue
		}
		if tok.IsKeyword() {
			kw := lit
			if kw == "" {
				kw = tok.String()
			}
			addIdentBloom(&entry.bits, kw)
			if tok == token.TYPE {
				entry.hasTypeDecl = true
			} else if tok == token.INTERFACE {
				entry.hasInterfaceKw = true
			}
			continue
		}
		if !entry.importsExact && tok == token.STRING {
			if unq, err := strconv.Unquote(lit); err == nil && unq != "" && len(unq) <= 256 && !strings.ContainsAny(unq, " \t\r\n") {
				addImportBloom(&entry.bits, unq)
			}
		}
	}
}

type blobIndexData struct {
	pkgName          string
	bits             [64]uint64
	imports          []string
	importsExact     bool
	hasTypeDecl      bool
	hasInterfaceKw   bool
	constraintHeader string
}

type blobIndexShard struct {
	mu    sync.RWMutex
	cap   int
	order []string
	items map[string]blobIndexData
}

type blobIndexCache struct {
	shards [16]blobIndexShard
}

func newBlobIndexCache(totalCap int) *blobIndexCache {
	shardCap := totalCap / 16
	if shardCap < 64 {
		shardCap = 64
	}
	c := &blobIndexCache{}
	for i := range c.shards {
		c.shards[i].cap = shardCap
		c.shards[i].items = make(map[string]blobIndexData, shardCap)
		c.shards[i].order = make([]string, 0, shardCap)
	}
	return c
}

func (c *blobIndexCache) get(oid string) (blobIndexData, bool) {
	if c == nil || len(oid) == 0 {
		return blobIndexData{}, false
	}
	h := fnv1a64(oid) & 15
	shard := &c.shards[h]
	shard.mu.RLock()
	data, ok := shard.items[oid]
	shard.mu.RUnlock()
	return data, ok
}

func (c *blobIndexCache) put(oid string, data blobIndexData) {
	if c == nil || len(oid) == 0 {
		return
	}
	h := fnv1a64(oid) & 15
	shard := &c.shards[h]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if _, ok := shard.items[oid]; ok {
		return
	}
	if len(shard.items) >= shard.cap && len(shard.order) > 0 {
		oldest := shard.order[0]
		shard.order = shard.order[1:]
		delete(shard.items, oldest)
	}
	shard.items[oid] = data
	shard.order = append(shard.order, oid)
}

var sharedBlobIndexCache = newBlobIndexCache(32768)

func (dc *diskCache) peekRepoFileIndex() *repoFileIndex {
	if dc == nil {
		return nil
	}
	dc.fileIndexMu.Lock()
	defer dc.fileIndexMu.Unlock()
	return dc.fileIndex
}

func (dc *diskCache) hasPrevRepoFileIndex() bool {
	if dc == nil || dc.prevIndexFunc == nil {
		return false
	}
	_, idx := dc.prevIndexFunc()
	return idx != nil
}

func gitBlobOID(data []byte, revLen int) string {
	var h hash.Hash
	if revLen == 64 {
		h = sha256.New()
	} else {
		h = sha1.New()
	}
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func matchIndexedGoFile(bctx *build.Context, repoRoot string, e *repoFileEntry) bool {
	if bctx == nil || e == nil {
		return true
	}
	cp := *bctx
	cp.OpenFile = func(string) (io.ReadCloser, error) {
		pkg := e.pkgName
		if pkg == "" {
			pkg = "p"
		}
		content := e.constraintHeader + "\npackage " + pkg + "\n"
		return io.NopCloser(strings.NewReader(content)), nil
	}
	match, _ := cp.MatchFile(filepath.Join(repoRoot, filepath.FromSlash(e.dir)), path.Base(e.path))
	return match
}

func readRepoFilesBatch(repo vcs.Repo, rev string, paths []string, oids []string) map[string][]byte {
	out := make(map[string][]byte, len(paths))
	if len(paths) == 0 {
		return out
	}
	blobRepo, hasBlobRepo := repo.(vcs.GoBlobRepo)
	if !hasBlobRepo || len(oids) != len(paths) {
		for _, p := range paths {
			b, err := repo.Content(rev, p)
			if err == nil {
				out[p] = b
			}
		}
		return out
	}

	oidToPaths := make(map[string][]string, len(oids))
	var uniqueOIDs []string
	var missingPaths []string
	for i, p := range paths {
		oid := oids[i]
		if oid == "" {
			missingPaths = append(missingPaths, p)
			continue
		}
		if _, seen := oidToPaths[oid]; !seen {
			uniqueOIDs = append(uniqueOIDs, oid)
		}
		oidToPaths[oid] = append(oidToPaths[oid], p)
	}

	if len(uniqueOIDs) > 0 {
		var mu sync.Mutex
		_ = blobRepo.StreamBlobs(uniqueOIDs, func(idx int, oid string, data []byte) {
			mu.Lock()
			defer mu.Unlock()
			for _, p := range oidToPaths[oid] {
				out[p] = data
			}
		})
	}

	for _, p := range missingPaths {
		if _, ok := out[p]; !ok {
			b, err := repo.Content(rev, p)
			if err == nil {
				out[p] = b
			}
		}
	}

	for _, p := range paths {
		if _, ok := out[p]; !ok {
			b, err := repo.Content(rev, p)
			if err == nil {
				out[p] = b
			}
		}
	}

	return out
}

type crossRevPkgEntry struct {
	repoRoot string
	digest   string
	pkg      *types.Package
	fset     *token.FileSet
}

type crossRevPkgCache struct {
	mu       sync.RWMutex
	cap      int
	order    []string
	byDigest map[string]*crossRevPkgEntry
	byPkg    map[*types.Package]*crossRevPkgEntry
}

func newCrossRevPkgCache(capacity int) *crossRevPkgCache {
	if capacity < 64 {
		capacity = 64
	}
	return &crossRevPkgCache{
		cap:      capacity,
		byDigest: make(map[string]*crossRevPkgEntry, capacity),
		byPkg:    make(map[*types.Package]*crossRevPkgEntry, capacity),
	}
}

func (c *crossRevPkgCache) get(digest string) *crossRevPkgEntry {
	if c == nil || digest == "" {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byDigest[digest]
}

func (c *crossRevPkgCache) digestFor(pkg *types.Package) string {
	if c == nil || pkg == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e := c.byPkg[pkg]; e != nil {
		return e.digest
	}
	return ""
}

func (c *crossRevPkgCache) fsetFor(pkg *types.Package) *token.FileSet {
	if c == nil || pkg == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e := c.byPkg[pkg]; e != nil {
		return e.fset
	}
	return nil
}

func (c *crossRevPkgCache) put(repoRoot, digest string, pkg *types.Package, fset *token.FileSet) {
	if c == nil || digest == "" || pkg == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.byDigest[digest]; ok {
		c.byPkg[pkg] = existing
		return
	}

	if len(c.order) >= c.cap {
		oldest := c.order[0]
		c.order = c.order[1:]
		if oldE, ok := c.byDigest[oldest]; ok {
			delete(c.byDigest, oldest)
			delete(c.byPkg, oldE.pkg)
		}
	}

	entry := &crossRevPkgEntry{
		repoRoot: repoRoot,
		digest:   digest,
		pkg:      pkg,
		fset:     fset,
	}
	c.byDigest[digest] = entry
	c.byPkg[pkg] = entry
	c.order = append(c.order, digest)
}

func (c *crossRevPkgCache) evictRepo(repoRoot string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	newOrder := c.order[:0]
	for _, digest := range c.order {
		e := c.byDigest[digest]
		if e != nil && e.repoRoot == repoRoot {
			delete(c.byDigest, digest)
			delete(c.byPkg, e.pkg)
		} else {
			newOrder = append(newOrder, digest)
		}
	}
	c.order = newOrder
}

var sharedCrossRevPkgCache = newCrossRevPkgCache(2048)

type crossRevCheckedEntry struct {
	repoRoot string
	digest   string
	checked  *checkedPkgEntry
}

type crossRevCheckedLRU struct {
	mu       sync.Mutex
	cap      int
	order    []string
	byDigest map[string]*crossRevCheckedEntry
}

func newCrossRevCheckedLRU(capacity int) *crossRevCheckedLRU {
	if capacity < 32 {
		capacity = 32
	}
	return &crossRevCheckedLRU{
		cap:      capacity,
		byDigest: make(map[string]*crossRevCheckedEntry, capacity),
	}
}

func (c *crossRevCheckedLRU) get(digest string) *checkedPkgEntry {
	if c == nil || digest == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.byDigest[digest]
	if !ok {
		return nil
	}
	for i, d := range c.order {
		if d == digest {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, digest)
	return e.checked
}

func (c *crossRevCheckedLRU) put(digest, repoRoot string, checked *checkedPkgEntry) {
	if c == nil || digest == "" || checked == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.byDigest[digest]; ok {
		for i, d := range c.order {
			if d == digest {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		c.order = append(c.order, digest)
		return
	}

	if len(c.order) >= c.cap {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.byDigest, oldest)
	}

	c.byDigest[digest] = &crossRevCheckedEntry{
		repoRoot: repoRoot,
		digest:   digest,
		checked:  checked,
	}
	c.order = append(c.order, digest)
}

func (c *crossRevCheckedLRU) evictRepo(repoRoot string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	newOrder := c.order[:0]
	for _, digest := range c.order {
		e := c.byDigest[digest]
		if e != nil && e.repoRoot == repoRoot {
			delete(c.byDigest, digest)
		} else {
			newOrder = append(newOrder, digest)
		}
	}
	c.order = newOrder
}

var sharedCrossRevCheckedLRU = newCrossRevCheckedLRU(256)

func computePkgMerkleDigest(
	repoRoot, importPath, dirInRepo, goos, goarch, tagsSig string,
	matchedFiles []*repoFileEntry,
	sortedImports []string,
	impDigests map[string]string,
) string {
	h := sha256.New()
	fmt.Fprintf(h, "pkg:%s|%s|%s|%s|%s|%s\n", repoRoot, importPath, dirInRepo, goos, goarch, tagsSig)
	for _, e := range matchedFiles {
		fmt.Fprintf(h, "f:%s=%s\n", path.Base(e.path), e.oid)
	}
	for _, imp := range sortedImports {
		fmt.Fprintf(h, "i:%s=%s\n", imp, impDigests[imp])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func computeCheckedMerkleDigest(
	repoRoot, checkImportPath, dirInRepo, pkgName string,
	isTest bool,
	goos, goarch, tagsSig, baseDigest string,
	matchedFiles []*repoFileEntry,
	sortedImports []string,
	impDigests map[string]string,
) string {
	h := sha256.New()
	fmt.Fprintf(h, "checked:%s|%s|%s|%s|%t|%s|%s|%s|%s\n", repoRoot, checkImportPath, dirInRepo, pkgName, isTest, goos, goarch, tagsSig, baseDigest)
	for _, e := range matchedFiles {
		fmt.Fprintf(h, "f:%s=%s\n", path.Base(e.path), e.oid)
	}
	for _, imp := range sortedImports {
		fmt.Fprintf(h, "i:%s=%s\n", imp, impDigests[imp])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func isTestdataPath(p string) bool {
	return strings.HasPrefix(p, "testdata/") || strings.Contains(p, "/testdata/")
}

func (dc *diskCache) getRepoFileIndex(ctx context.Context, repo vcs.Repo, rev string) (*repoFileIndex, error) {
	if dc == nil {
		return nil, nil
	}

	for {
		dc.fileIndexMu.Lock()
		if dc.fileIndex != nil {
			idx := dc.fileIndex
			dc.fileIndexMu.Unlock()
			return idx, nil
		}
		if waitCh := dc.fileIndexInflight; waitCh != nil {
			dc.fileIndexMu.Unlock()
			select {
			case <-waitCh:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		doneCh := make(chan struct{})
		dc.fileIndexInflight = doneCh
		dc.fileIndexMu.Unlock()

		defer func() {
			dc.fileIndexMu.Lock()
			dc.fileIndexInflight = nil
			dc.fileIndexMu.Unlock()
			close(doneCh)
		}()
		break
	}

	if repo == nil {
		return nil, nil
	}

	bCache := dc.blobCache
	if bCache == nil {
		bCache = sharedBlobIndexCache
	}

	// Path A: Fast path when repo implements vcs.GoBlobRepo and rev is an immutable revision
	if blobRepo, ok := repo.(vcs.GoBlobRepo); ok && rev != "" && rev != "working" {
		blobs, err := blobRepo.ListGoFileBlobs(rev)
		if err == nil {
			dc.goFilesMu.Lock()
			if dc.goFiles == nil {
				paths := make([]string, len(blobs))
				for i, b := range blobs {
					paths[i] = b.Path
				}
				dc.goFiles = paths
			}
			dc.goFilesMu.Unlock()

			entries := make([]repoFileEntry, len(blobs))
			byPath := make(map[string]int, len(blobs))
			oidToIndices := make(map[string][]int)
			var missingOIDs []string

			for i, gb := range blobs {
				fPath := gb.Path
				byPath[fPath] = i
				d := path.Dir(fPath)
				if d == "." {
					d = ""
				}
				entries[i].path = fPath
				entries[i].dir = d
				entries[i].oid = gb.OID

				if data, ok := bCache.get(gb.OID); ok {
					entries[i].pkgName = data.pkgName
					entries[i].bits = data.bits
					entries[i].imports = data.imports
					entries[i].importsExact = data.importsExact
					entries[i].hasTypeDecl = data.hasTypeDecl
					entries[i].hasInterfaceKw = data.hasInterfaceKw
					entries[i].constraintHeader = data.constraintHeader
				} else {
					if _, seen := oidToIndices[gb.OID]; !seen {
						missingOIDs = append(missingOIDs, gb.OID)
					}
					oidToIndices[gb.OID] = append(oidToIndices[gb.OID], i)
				}
			}

			if len(missingOIDs) > 0 {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}

				type streamItem struct {
					oid  string
					data []byte
				}
				workCh := make(chan streamItem, 256)
				numWorkers := min(8, len(missingOIDs))
				var scanWg sync.WaitGroup
				var scanErrMu sync.Mutex
				var firstScanErr error

				for w := 0; w < numWorkers; w++ {
					scanWg.Add(1)
					go func() {
						defer scanWg.Done()
						var tmp repoFileEntry
						for item := range workCh {
							if ctx.Err() != nil {
								scanErrMu.Lock()
								if firstScanErr == nil {
									firstScanErr = ctx.Err()
								}
								scanErrMu.Unlock()
								continue
							}
							tmp = repoFileEntry{}
							scanFileBloomAndImports(item.data, &tmp, false, nil)
							data := blobIndexData{
								pkgName:          tmp.pkgName,
								bits:             tmp.bits,
								imports:          tmp.imports,
								importsExact:     tmp.importsExact,
								hasTypeDecl:      tmp.hasTypeDecl,
								hasInterfaceKw:   tmp.hasInterfaceKw,
								constraintHeader: tmp.constraintHeader,
							}
							bCache.put(item.oid, data)

							for _, idx := range oidToIndices[item.oid] {
								entries[idx].pkgName = tmp.pkgName
								entries[idx].bits = tmp.bits
								entries[idx].imports = tmp.imports
								entries[idx].importsExact = tmp.importsExact
								entries[idx].hasTypeDecl = tmp.hasTypeDecl
								entries[idx].hasInterfaceKw = tmp.hasInterfaceKw
								entries[idx].constraintHeader = tmp.constraintHeader
							}
						}
					}()
				}

				streamErr := blobRepo.StreamBlobs(missingOIDs, func(idx int, oid string, data []byte) {
					if ctx.Err() != nil {
						return
					}
					workCh <- streamItem{oid: oid, data: data}
				})
				close(workCh)
				scanWg.Wait()

				if streamErr != nil {
					return nil, streamErr
				}
				if firstScanErr != nil {
					return nil, firstScanErr
				}
			}

			dirHasType := make(map[string]bool)
			dirHasInterface := make(map[string]bool)
			dirFiles := make(map[string][]int)
			dirPkgFiles := make(map[string]map[string][]string)
			mergedImports := make(map[string]bool)

			for i := range entries {
				d := entries[i].dir
				dirFiles[d] = append(dirFiles[d], i)
				if entries[i].hasTypeDecl {
					dirHasType[d] = true
				}
				if entries[i].hasTypeDecl && entries[i].hasInterfaceKw {
					dirHasInterface[d] = true
				}
				pkgName := entries[i].pkgName
				if pkgName != "" {
					if dirPkgFiles[d] == nil {
						dirPkgFiles[d] = make(map[string][]string)
					}
					dirPkgFiles[d][pkgName] = append(dirPkgFiles[d][pkgName], entries[i].path)
				}
				if !isTestdataPath(entries[i].path) {
					for _, imp := range entries[i].imports {
						if imp != "" && imp != "C" && imp != "unsafe" && !strings.HasPrefix(imp, ".") {
							mergedImports[imp] = true
						}
					}
				}
			}

			idx := &repoFileIndex{
				files:           entries,
				byPath:          byPath,
				dirFiles:        dirFiles,
				dirPkgFiles:     dirPkgFiles,
				dirHasType:      dirHasType,
				dirHasInterface: dirHasInterface,
			}

			imports := make([]string, 0, len(mergedImports))
			for imp := range mergedImports {
				imports = append(imports, imp)
			}
			slices.Sort(imports)

			dc.repoImportsMu.Lock()
			dc.repoImports = imports
			dc.repoImportsMu.Unlock()

			dc.fileIndexMu.Lock()
			dc.fileIndex = idx
			dc.fileIndexMu.Unlock()

			return idx, nil
		}
	}

	// Path B: Fallback when GoBlobRepo is not available or failed
	goFiles, err := dc.getGoFiles(repo, rev)
	if err != nil {
		return nil, err
	}

	entries := make([]repoFileEntry, len(goFiles))
	byPath := make(map[string]int, len(goFiles))
	for i, fPath := range goFiles {
		byPath[fPath] = i
		d := path.Dir(fPath)
		if d == "." {
			d = ""
		}
		entries[i].path = fPath
		entries[i].dir = d
	}

	var neededIndices []int
	usedIncremental := false
	if dc.prevIndexFunc != nil {
		prevRev, prevIdx := dc.prevIndexFunc()
		if prevIdx != nil && prevRev != "" && prevRev != rev {
			if diffFiles, diffErr := repo.Files(prevRev, rev); diffErr == nil {
				usedIncremental = true
				dirty := make(map[string]bool)
				for _, df := range diffFiles {
					if df != nil {
						if df.Path != "" {
							dirty[df.Path] = true
						}
						if df.OldPath != "" {
							dirty[df.OldPath] = true
						}
					}
				}
				for i, fPath := range goFiles {
					if !dirty[fPath] {
						if prevI, ok := prevIdx.byPath[fPath]; ok {
							prevE := prevIdx.files[prevI]
							entries[i].pkgName = prevE.pkgName
							entries[i].oid = prevE.oid
							entries[i].constraintHeader = prevE.constraintHeader
							entries[i].bits = prevE.bits
							entries[i].imports = prevE.imports
							entries[i].importsExact = prevE.importsExact
							entries[i].hasTypeDecl = prevE.hasTypeDecl
							entries[i].hasInterfaceKw = prevE.hasInterfaceKw
							continue
						}
					}
					neededIndices = append(neededIndices, i)
				}
			}
		}
	}

	if !usedIncremental {
		neededIndices = make([]int, len(goFiles))
		for i := range goFiles {
			neededIndices[i] = i
		}
	}

	if len(neededIndices) > 0 {
		numWorkers := min(8, len(neededIndices))
		if numWorkers < 1 {
			numWorkers = 1
		}

		ch := make(chan int, len(neededIndices))
		for _, idx := range neededIndices {
			ch <- idx
		}
		close(ch)

		var wg sync.WaitGroup
		var scanErrMu sync.Mutex
		var firstScanErr error

		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range ch {
					if ctx.Err() != nil {
						scanErrMu.Lock()
						if firstScanErr == nil {
							firstScanErr = ctx.Err()
						}
						scanErrMu.Unlock()
						return
					}
					fPath := goFiles[i]
					b, err := repo.Content(rev, fPath)
					if err != nil || len(b) == 0 {
						continue
					}
					scanFileBloomAndImports(b, &entries[i], false, nil)
				}
			}()
		}
		wg.Wait()
		if firstScanErr != nil {
			return nil, firstScanErr
		}
	}

	dirHasType := make(map[string]bool)
	dirHasInterface := make(map[string]bool)
	dirFiles := make(map[string][]int)
	dirPkgFiles := make(map[string]map[string][]string)
	mergedImports := make(map[string]bool)

	for i := range entries {
		d := entries[i].dir
		dirFiles[d] = append(dirFiles[d], i)
		if entries[i].hasTypeDecl {
			dirHasType[d] = true
		}
		if entries[i].hasTypeDecl && entries[i].hasInterfaceKw {
			dirHasInterface[d] = true
		}
		pkgName := entries[i].pkgName
		if pkgName != "" {
			if dirPkgFiles[d] == nil {
				dirPkgFiles[d] = make(map[string][]string)
			}
			dirPkgFiles[d][pkgName] = append(dirPkgFiles[d][pkgName], entries[i].path)
		}
		if !isTestdataPath(entries[i].path) {
			for _, imp := range entries[i].imports {
				if imp != "" && imp != "C" && imp != "unsafe" && !strings.HasPrefix(imp, ".") {
					mergedImports[imp] = true
				}
			}
		}
	}

	idx := &repoFileIndex{
		files:           entries,
		byPath:          byPath,
		dirFiles:        dirFiles,
		dirPkgFiles:     dirPkgFiles,
		dirHasType:      dirHasType,
		dirHasInterface: dirHasInterface,
	}

	imports := make([]string, 0, len(mergedImports))
	for imp := range mergedImports {
		imports = append(imports, imp)
	}
	slices.Sort(imports)

	dc.repoImportsMu.Lock()
	dc.repoImports = imports
	dc.repoImportsMu.Unlock()

	dc.fileIndexMu.Lock()
	dc.fileIndex = idx
	dc.fileIndexMu.Unlock()

	return idx, nil
}

type checkedPkgKey struct {
	importPath string
	dir        string
	isDisk     bool
	pkgName    string
	isTest     bool
	tagsSig    string
	goos       string
	goarch     string
}

type checkedFile struct {
	path  string
	ast   *ast.File
	lines []string
}

type indexedIdentUse struct {
	fileIdx int
	id      *ast.Ident
	sel     *ast.SelectorExpr
	obj     types.Object
}

type indexedPkgUse struct {
	fileIdx    int
	id         *ast.Ident
	sel        *ast.SelectorExpr
	pkgName    *types.PkgName
	importSpec *ast.ImportSpec
}

type checkedPkgEntry struct {
	pkg           *types.Package
	fset          *token.FileSet
	files         []*checkedFile
	fileByPath    map[string]*checkedFile
	info          *types.Info
	usesByName    map[string][]indexedIdentUse
	pkgUsesByPath map[string][]indexedPkgUse
	depFsets      map[*types.Package]*token.FileSet
}

type checkedPkgLRU struct {
	mu       sync.Mutex
	cap      int
	items    map[checkedPkgKey]*checkedPkgEntry
	order    []checkedPkgKey
	inflight map[checkedPkgKey]chan struct{}
}

func newCheckedPkgLRU(cap int) *checkedPkgLRU {
	return &checkedPkgLRU{
		cap:      cap,
		items:    make(map[checkedPkgKey]*checkedPkgEntry),
		inflight: make(map[checkedPkgKey]chan struct{}),
	}
}

func (lru *checkedPkgLRU) get(key checkedPkgKey) *checkedPkgEntry {
	if lru == nil {
		return nil
	}
	lru.mu.Lock()
	defer lru.mu.Unlock()
	entry, ok := lru.items[key]
	if !ok {
		return nil
	}
	for i, k := range lru.order {
		if k == key {
			lru.order = append(lru.order[:i], lru.order[i+1:]...)
			lru.order = append(lru.order, key)
			break
		}
	}
	return entry
}

func (lru *checkedPkgLRU) put(key checkedPkgKey, entry *checkedPkgEntry) {
	if lru == nil || entry == nil {
		return
	}
	lru.mu.Lock()
	defer lru.mu.Unlock()
	if _, ok := lru.items[key]; ok {
		lru.items[key] = entry
		return
	}
	for len(lru.items) >= lru.cap && len(lru.order) > 0 {
		oldest := lru.order[0]
		lru.order = lru.order[1:]
		delete(lru.items, oldest)
	}
	lru.items[key] = entry
	lru.order = append(lru.order, key)
}

func buildIndexes(entry *checkedPkgEntry) {
	if entry == nil || entry.info == nil {
		return
	}
	entry.usesByName = make(map[string][]indexedIdentUse)
	entry.pkgUsesByPath = make(map[string][]indexedPkgUse)

	for fIdx, f := range entry.files {
		if f == nil || f.ast == nil {
			continue
		}
		var stack []ast.Node
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if n == nil {
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				return false
			}
			stack = append(stack, n)

			if spec, ok := n.(*ast.ImportSpec); ok && spec.Path != nil {
				if imp, err := strconv.Unquote(spec.Path.Value); err == nil && imp != "" {
					entry.pkgUsesByPath[imp] = append(entry.pkgUsesByPath[imp], indexedPkgUse{
						fileIdx:    fIdx,
						importSpec: spec,
					})
				}
			}

			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			var sel *ast.SelectorExpr
			if len(stack) >= 2 {
				if s, ok := stack[len(stack)-2].(*ast.SelectorExpr); ok && s.Sel == id {
					sel = s
				}
			}
			obj := identObject(id, sel, entry.info)
			if obj != nil {
				if pkgName, ok := obj.(*types.PkgName); ok && pkgName.Imported() != nil {
					p := pkgName.Imported().Path()
					entry.pkgUsesByPath[p] = append(entry.pkgUsesByPath[p], indexedPkgUse{
						fileIdx: fIdx,
						id:      id,
						sel:     sel,
						pkgName: pkgName,
					})
				}
				entry.usesByName[obj.Name()] = append(entry.usesByName[obj.Name()], indexedIdentUse{
					fileIdx: fIdx,
					id:      id,
					sel:     sel,
					obj:     obj,
				})
			}
			return true
		})
	}
}

func isImmutableDiskDir(env goEnv, absDir string) bool {
	cleanDir := filepath.Clean(absDir)
	if env.goroot != "" {
		gRootSrc := filepath.Join(filepath.Clean(env.goroot), "src")
		if cleanDir == gRootSrc || strings.HasPrefix(cleanDir, gRootSrc+string(filepath.Separator)) {
			return true
		}
	}
	if env.gomodcache != "" {
		modCache := filepath.Clean(env.gomodcache)
		if cleanDir == modCache || strings.HasPrefix(cleanDir, modCache+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func cloneRefsResult(r *RefsResult) *RefsResult {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Refs = slices.Clone(r.Refs)
	return &cp
}

func cloneImplsResult(r *ImplsResult) *ImplsResult {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Implementations = slices.Clone(r.Implementations)
	return &cp
}

type refsCacheKey struct {
	baseRev      string
	targetRev    string
	targetID     targetIdentity
	defCleanFile string
	defLine      int
	defCol       int
	isPkgClause  bool
}

type refsLRU struct {
	mu    sync.Mutex
	cap   int
	items map[refsCacheKey]*RefsResult
	order []refsCacheKey
}

func newRefsLRU(cap int) *refsLRU {
	return &refsLRU{
		cap:   cap,
		items: make(map[refsCacheKey]*RefsResult),
	}
}

func (lru *refsLRU) get(key refsCacheKey) (*RefsResult, bool) {
	if lru == nil {
		return nil, false
	}
	lru.mu.Lock()
	defer lru.mu.Unlock()
	res, ok := lru.items[key]
	if !ok {
		return nil, false
	}
	for i, k := range lru.order {
		if k == key {
			lru.order = append(lru.order[:i], lru.order[i+1:]...)
			lru.order = append(lru.order, key)
			break
		}
	}
	return cloneRefsResult(res), true
}

func (lru *refsLRU) put(key refsCacheKey, res *RefsResult) {
	if lru == nil || res == nil {
		return
	}
	lru.mu.Lock()
	defer lru.mu.Unlock()
	cloned := cloneRefsResult(res)
	if _, ok := lru.items[key]; ok {
		lru.items[key] = cloned
		return
	}
	for len(lru.items) >= lru.cap && len(lru.order) > 0 {
		oldest := lru.order[0]
		lru.order = lru.order[1:]
		delete(lru.items, oldest)
	}
	lru.items[key] = cloned
	lru.order = append(lru.order, key)
}

type implsCacheKey struct {
	baseRev          string
	targetRev        string
	direction        string
	targetID         targetIdentity
	defCleanFile     string
	defLine          int
	defCol           int
	targetMethodName string
}

type implsLRU struct {
	mu    sync.Mutex
	cap   int
	items map[implsCacheKey]*ImplsResult
	order []implsCacheKey
}

func newImplsLRU(cap int) *implsLRU {
	return &implsLRU{
		cap:   cap,
		items: make(map[implsCacheKey]*ImplsResult),
	}
}

func (lru *implsLRU) get(key implsCacheKey) (*ImplsResult, bool) {
	if lru == nil {
		return nil, false
	}
	lru.mu.Lock()
	defer lru.mu.Unlock()
	res, ok := lru.items[key]
	if !ok {
		return nil, false
	}
	for i, k := range lru.order {
		if k == key {
			lru.order = append(lru.order[:i], lru.order[i+1:]...)
			lru.order = append(lru.order, key)
			break
		}
	}
	return cloneImplsResult(res), true
}

func (lru *implsLRU) put(key implsCacheKey, res *ImplsResult) {
	if lru == nil || res == nil {
		return
	}
	lru.mu.Lock()
	defer lru.mu.Unlock()
	cloned := cloneImplsResult(res)
	if _, ok := lru.items[key]; ok {
		lru.items[key] = cloned
		return
	}
	for len(lru.items) >= lru.cap && len(lru.order) > 0 {
		oldest := lru.order[0]
		lru.order = lru.order[1:]
		delete(lru.items, oldest)
	}
	lru.items[key] = cloned
	lru.order = append(lru.order, key)
}

func (l *lookupLoader) loadCheckedPackage(dirInRepo, pkgName string, isTest bool) *checkedPkgEntry {
	if l == nil || l.req.Repo == nil || l.revCache == nil {
		return nil
	}
	lru := l.revCache.checkedPkgs
	if lru == nil {
		return nil
	}
	rootImportPath := cleanPathToImport(l.mod, dirInRepo)
	checkImportPath := rootImportPath
	if strings.HasSuffix(pkgName, "_test") {
		checkImportPath += "_test"
	}

	key := checkedPkgKey{
		importPath: checkImportPath,
		dir:        dirInRepo,
		isDisk:     false,
		pkgName:    pkgName,
		isTest:     isTest,
		tagsSig:    l.buildTagsSig(),
		goos:       l.goos,
		goarch:     l.goarch,
	}

	if entry := lru.get(key); entry != nil {
		return entry
	}

	var doneCh chan struct{}
	for {
		lru.mu.Lock()
		if entry, ok := lru.items[key]; ok {
			lru.mu.Unlock()
			return entry
		}
		if waitCh, busy := lru.inflight[key]; busy {
			lru.mu.Unlock()
			select {
			case <-waitCh:
				continue
			case <-l.ctx.Done():
				return nil
			}
		}
		doneCh = make(chan struct{})
		lru.inflight[key] = doneCh
		lru.mu.Unlock()
		break
	}
	defer func() {
		lru.mu.Lock()
		delete(lru.inflight, key)
		lru.mu.Unlock()
		close(doneCh)
	}()

	var checkedMerkleDigest string
	var matchedCheckedEntries []*repoFileEntry
	if l.crossRevChecked != nil && l.revCache != nil {
		if fileIdx := l.revCache.peekRepoFileIndex(); fileIdx != nil {
			indices := fileIdx.dirFiles[dirInRepo]
			relTags := l.releaseTags
			if !l.customBuild && relTags == nil {
				relTags = build.Default.ReleaseTags
			}
			bctx := &build.Context{
				GOOS:        l.goos,
				GOARCH:      l.goarch,
				CgoEnabled:  true,
				BuildTags:   l.buildTags,
				ReleaseTags: relTags,
			}
			canMerkleChecked := true
			for _, idx := range indices {
				e := &fileIdx.files[idx]
				if e.pkgName != pkgName {
					continue
				}
				nameIsTest := strings.HasSuffix(e.path, "_test.go")
				if !isTest && nameIsTest {
					continue
				}
				if strings.HasPrefix(path.Base(e.path), ".") || strings.HasPrefix(path.Base(e.path), "_") {
					continue
				}
				if matchIndexedGoFile(bctx, l.req.RepoRoot, e) {
					if e.oid == "" || !e.importsExact {
						canMerkleChecked = false
						break
					}
					matchedCheckedEntries = append(matchedCheckedEntries, e)
				}
			}

			if canMerkleChecked && len(matchedCheckedEntries) > 0 {
				var baseDigest string
				if strings.HasSuffix(pkgName, "_test") {
					basePkg, err := l.loadInRepoTestBasePackage(rootImportPath, dirInRepo)
					if err == nil && basePkg != nil {
						l.inRepoPkgs[rootImportPath] = basePkg
						if l.crossRevPkgs != nil {
							baseDigest = l.crossRevPkgs.digestFor(basePkg)
						}
					}
					if baseDigest == "" {
						canMerkleChecked = false
					}
				}

				if canMerkleChecked {
					impMap := make(map[string]bool)
					for _, e := range matchedCheckedEntries {
						for _, imp := range e.imports {
							if imp != "" && imp != "C" && imp != "unsafe" && (!strings.HasSuffix(pkgName, "_test") || imp != rootImportPath) {
								impMap[imp] = true
							}
						}
					}
					var directImports []string
					for imp := range impMap {
						directImports = append(directImports, imp)
					}
					slices.Sort(directImports)

					impDigests := make(map[string]string, len(directImports))
					for _, imp := range directImports {
						if l.loading[imp] {
							canMerkleChecked = false
							break
						}
						impPkg, err := l.Import(imp)
						if err != nil || impPkg == nil {
							canMerkleChecked = false
							break
						}
						var d string
						if impPkg == types.Unsafe {
							d = "unsafe"
						} else if l.diskCache != nil && l.diskCache.has(impPkg) {
							d = "disk:" + impPkg.Path() + "@" + l.diskCache.dirFor(impPkg)
						} else if l.crossRevPkgs != nil {
							if cd := l.crossRevPkgs.digestFor(impPkg); cd != "" {
								d = "repo:" + cd
							}
						}
						if d == "" {
							canMerkleChecked = false
							break
						}
						impDigests[imp] = d
					}

					if canMerkleChecked {
						checkedMerkleDigest = computeCheckedMerkleDigest(
							l.req.RepoRoot, checkImportPath, dirInRepo, pkgName,
							isTest, l.goos, l.goarch, l.buildTagsSig(),
							baseDigest, matchedCheckedEntries, directImports, impDigests,
						)
						if hit := l.crossRevChecked.get(checkedMerkleDigest); hit != nil {
							lru.put(key, hit)
							if l.revCache != nil && len(hit.depFsets) > 0 {
								for depPkg, depFset := range hit.depFsets {
									l.revCache.registerPkgFset(depPkg, depFset)
								}
							}
							return hit
						}
					}
				}
			}
		}
	}

	var astFiles []*ast.File
	var checkedFiles []*checkedFile
	fileByPath := make(map[string]*checkedFile)
	fset := token.NewFileSet()

	if checkedMerkleDigest != "" {
		var paths []string
		var oids []string
		for _, e := range matchedCheckedEntries {
			paths = append(paths, e.path)
			oids = append(oids, e.oid)
		}
		contents := readRepoFilesBatch(l.req.Repo, l.rev(), paths, oids)
		for _, e := range matchedCheckedEntries {
			data := contents[e.path]
			if len(data) == 0 {
				continue
			}
			filePath := e.path
			virtualPath := filepath.Join(l.req.RepoRoot, filepath.FromSlash(filePath))
			astF, err := parser.ParseFile(fset, virtualPath, data, parser.SkipObjectResolution)
			if err != nil || astF == nil || astF.Name.Name != pkgName {
				continue
			}
			cf := &checkedFile{
				path:  filePath,
				ast:   astF,
				lines: diff.SplitLines(data),
			}
			astFiles = append(astFiles, astF)
			checkedFiles = append(checkedFiles, cf)
			fileByPath[filePath] = cf
		}
	} else {
		names, err := l.req.Repo.ListDir(l.rev(), dirInRepo)
		if err != nil || len(names) == 0 {
			return nil
		}

		bctx, fc := l.makeBuildContextWithCache()
		for _, name := range names {
			if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			nameIsTest := strings.HasSuffix(name, "_test.go")
			if !isTest && nameIsTest {
				continue
			}
			data, ok := l.matchAndReadInRepo(bctx, fc, dirInRepo, name)
			if !ok || len(data) == 0 {
				continue
			}
			filePath := name
			if dirInRepo != "" && dirInRepo != "." {
				filePath = path.Join(dirInRepo, name)
			}
			virtualPath := filepath.Join(l.req.RepoRoot, filepath.FromSlash(filePath))
			astF, err := parser.ParseFile(fset, virtualPath, data, parser.SkipObjectResolution)
			if err != nil || astF == nil || astF.Name.Name != pkgName {
				continue
			}
			cf := &checkedFile{
				path:  filePath,
				ast:   astF,
				lines: diff.SplitLines(data),
			}
			astFiles = append(astFiles, astF)
			checkedFiles = append(checkedFiles, cf)
			fileByPath[filePath] = cf
		}
	}

	if len(astFiles) == 0 {
		emptyEntry := &checkedPkgEntry{}
		if l.ctx.Err() == nil {
			lru.put(key, emptyEntry)
		}
		return emptyEntry
	}

	if strings.HasSuffix(pkgName, "_test") && l.inRepoPkgs[rootImportPath] == nil {
		if basePkg, err := l.loadInRepoTestBasePackage(rootImportPath, dirInRepo); err == nil && basePkg != nil {
			l.inRepoPkgs[rootImportPath] = basePkg
		}
	}

	info := &types.Info{
		Uses:       make(map[*ast.Ident]types.Object),
		Defs:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Implicits:  make(map[ast.Node]types.Object),
	}

	conf := types.Config{
		IgnoreFuncBodies: false,
		FakeImportC:      true,
		Importer:         l,
		Error:            func(error) {},
	}

	pkg, _ := conf.Check(checkImportPath, fset, astFiles, info)

	entry := &checkedPkgEntry{
		pkg:        pkg,
		fset:       fset,
		files:      checkedFiles,
		fileByPath: fileByPath,
		info:       info,
		depFsets:   l.collectDepFsets(pkg),
	}
	buildIndexes(entry)

	if l.ctx.Err() == nil {
		if checkedMerkleDigest != "" && l.crossRevChecked != nil {
			l.crossRevChecked.put(checkedMerkleDigest, l.req.RepoRoot, entry)
		}
		lru.put(key, entry)
	}
	return entry
}

func (l *lookupLoader) collectDepFsets(rootPkg *types.Package) map[*types.Package]*token.FileSet {
	if rootPkg == nil {
		return nil
	}
	deps := make(map[*types.Package]*token.FileSet)
	visited := make(map[*types.Package]bool)
	var walk func(p *types.Package)
	walk = func(p *types.Package) {
		if p == nil || visited[p] {
			return
		}
		visited[p] = true
		for _, imp := range p.Imports() {
			if imp == nil || visited[imp] {
				continue
			}
			if l.revCache != nil {
				if fs := l.revCache.fsetForPkg(imp); fs != nil {
					deps[imp] = fs
				}
			}
			if _, ok := deps[imp]; !ok && l.crossRevPkgs != nil {
				if fs := l.crossRevPkgs.fsetFor(imp); fs != nil {
					deps[imp] = fs
				}
			}
			if _, ok := deps[imp]; !ok && l.diskCache != nil {
				if fs := l.diskCache.fsetForPkg(imp); fs != nil {
					deps[imp] = fs
				}
			}
			walk(imp)
		}
	}
	walk(rootPkg)
	return deps
}

func (l *lookupLoader) loadDiskCheckedPackage(importPath, absDir, pkgName string, isTest bool) *checkedPkgEntry {
	if l == nil || l.diskCache == nil {
		return nil
	}
	if !isImmutableDiskDir(l.env, absDir) {
		return nil
	}
	lru := l.diskCache.checkedPkgs
	if lru == nil {
		return nil
	}

	key := checkedPkgKey{
		importPath: importPath,
		dir:        absDir,
		isDisk:     true,
		pkgName:    pkgName,
		isTest:     isTest,
		tagsSig:    l.buildTagsSig(),
		goos:       l.goos,
		goarch:     l.goarch,
	}

	if entry := lru.get(key); entry != nil {
		return entry
	}

	var doneCh chan struct{}
	for {
		lru.mu.Lock()
		if entry, ok := lru.items[key]; ok {
			lru.mu.Unlock()
			return entry
		}
		if waitCh, busy := lru.inflight[key]; busy {
			lru.mu.Unlock()
			select {
			case <-waitCh:
				continue
			case <-l.ctx.Done():
				return nil
			}
		}
		doneCh = make(chan struct{})
		lru.inflight[key] = doneCh
		lru.mu.Unlock()
		break
	}
	defer func() {
		lru.mu.Lock()
		delete(lru.inflight, key)
		lru.mu.Unlock()
		close(doneCh)
	}()

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil
	}

	bctx, fc := l.makeBuildContextWithCache()
	fset := token.NewFileSet()
	var astFiles []*ast.File
	var checkedFiles []*checkedFile
	fileByPath := make(map[string]*checkedFile)

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		nameIsTest := strings.HasSuffix(name, "_test.go")
		if !isTest && nameIsTest {
			continue
		}
		data, ok := l.matchAndReadDisk(bctx, fc, absDir, name)
		if !ok || len(data) == 0 {
			continue
		}
		targetPath := filepath.Join(absDir, name)
		astF, err := parser.ParseFile(fset, targetPath, data, parser.SkipObjectResolution)
		if err != nil || astF == nil || astF.Name.Name != pkgName {
			continue
		}
		cf := &checkedFile{
			path:  targetPath,
			ast:   astF,
			lines: diff.SplitLines(data),
		}
		astFiles = append(astFiles, astF)
		checkedFiles = append(checkedFiles, cf)
		fileByPath[targetPath] = cf
	}

	if len(astFiles) == 0 {
		emptyEntry := &checkedPkgEntry{}
		if l.ctx.Err() == nil {
			lru.put(key, emptyEntry)
		}
		return emptyEntry
	}

	if strings.HasSuffix(pkgName, "_test") {
		basePkgImport := strings.TrimSuffix(importPath, "_test")
		if basePkg, err := l.loadDiskTestBasePackage(basePkgImport, absDir); err == nil && basePkg != nil {
			l.inRepoPkgs[basePkgImport] = basePkg
		}
	}

	info := &types.Info{
		Uses:       make(map[*ast.Ident]types.Object),
		Defs:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Implicits:  make(map[ast.Node]types.Object),
	}

	var importer types.Importer
	if l.env.goroot != "" && strings.HasPrefix(absDir, filepath.Join(l.env.goroot, "src")+string(filepath.Separator)) {
		importer = newStdlibImporter(l)
	} else if l.env.gomodcache != "" && strings.HasPrefix(absDir, filepath.Clean(l.env.gomodcache)+string(filepath.Separator)) {
		importer = newModCacheImporter(l, absDir)
	} else {
		importer = l
	}

	conf := types.Config{
		IgnoreFuncBodies: false,
		FakeImportC:      true,
		Importer:         importer,
		Error:            func(error) {},
	}

	pkg, _ := conf.Check(importPath, fset, astFiles, info)

	if pkg != nil {
		l.diskCache.registerPkg(pkg)
		l.diskCache.registerPkgDir(pkg, absDir)
		l.diskCache.registerPkgFset(pkg, fset)
	}

	entry := &checkedPkgEntry{
		pkg:        pkg,
		fset:       fset,
		files:      checkedFiles,
		fileByPath: fileByPath,
		info:       info,
		depFsets:   l.collectDepFsets(pkg),
	}
	buildIndexes(entry)

	if l.ctx.Err() == nil {
		lru.put(key, entry)
	}
	return entry
}

func (dc *diskCache) get(key diskCacheKey) (*types.Package, bool) {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	p, ok := dc.pkgs[key]
	return p, ok
}

func (dc *diskCache) put(key diskCacheKey, p *types.Package) {
	if p == nil {
		return
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.pkgs[key] = p
	dc.pkgSet[p] = struct{}{}
	if key.absDir != "" {
		dc.pkgDirs[p] = key.absDir
	}
}

func (dc *diskCache) has(p *types.Package) bool {
	if dc == nil || p == nil {
		return false
	}
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	_, ok := dc.pkgSet[p]
	return ok
}

func (dc *diskCache) position(pos token.Pos) token.Position {
	if dc == nil || !pos.IsValid() {
		return token.Position{}
	}
	dc.fsetMu.Lock()
	defer dc.fsetMu.Unlock()
	return dc.fset.Position(pos)
}

func (dc *diskCache) getGoFiles(repo vcs.Repo, rev string) ([]string, error) {
	if dc == nil {
		if repo == nil {
			return nil, nil
		}
		return repo.ListGoFiles(rev)
	}
	dc.goFilesMu.Lock()
	defer dc.goFilesMu.Unlock()
	if dc.goFiles != nil {
		return dc.goFiles, nil
	}
	if repo == nil {
		return nil, nil
	}
	files, err := repo.ListGoFiles(rev)
	if err != nil {
		return nil, err
	}
	dc.goFiles = files
	return files, nil
}

func (dc *diskCache) getRepoImports(ctx context.Context, repo vcs.Repo, rev string) ([]string, error) {
	if dc != nil {
		dc.repoImportsMu.Lock()
		if dc.repoImports != nil {
			imports := dc.repoImports
			dc.repoImportsMu.Unlock()
			return imports, nil
		}
		dc.repoImportsMu.Unlock()

		// Building the file index also extracts repoImports in the same pass:
		if _, err := dc.getRepoFileIndex(ctx, repo, rev); err != nil {
			return nil, err
		}
		dc.repoImportsMu.Lock()
		imports := dc.repoImports
		dc.repoImportsMu.Unlock()
		return imports, nil
	}

	if repo == nil {
		return nil, nil
	}

	var goFiles []string
	var err error
	if dc != nil {
		goFiles, err = dc.getGoFiles(repo, rev)
	} else {
		goFiles, err = repo.ListGoFiles(rev)
	}
	if err != nil {
		return nil, err
	}

	importSet := make(map[string]bool)
	fset := token.NewFileSet()

	for _, fPath := range goFiles {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if strings.HasPrefix(fPath, "testdata/") || strings.Contains(fPath, "/testdata/") {
			continue
		}
		b, err := repo.Content(rev, fPath)
		if err != nil || len(b) == 0 || !bytes.Contains(b, []byte("import")) {
			continue
		}
		astF, err := parser.ParseFile(fset, "", b, parser.ImportsOnly)
		if err != nil || astF == nil {
			continue
		}
		for _, spec := range astF.Imports {
			if spec == nil || spec.Path == nil {
				continue
			}
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil || imp == "" || imp == "C" || imp == "unsafe" || strings.HasPrefix(imp, ".") {
				continue
			}
			importSet[imp] = true
		}
	}

	result := make([]string, 0, len(importSet))
	for imp := range importSet {
		result = append(result, imp)
	}
	slices.Sort(result)

	if dc != nil && ctx.Err() == nil {
		dc.repoImportsMu.Lock()
		dc.repoImports = result
		dc.repoImportsMu.Unlock()
	}

	return result, nil
}

func (dc *diskCache) findModCacheModuleInfo(gomodcache, absDir string) *moduleInfo {
	if gomodcache == "" || absDir == "" {
		return nil
	}
	cleanModCache := filepath.Clean(gomodcache)
	curr := filepath.Clean(absDir)

	for {
		if !strings.HasPrefix(curr, cleanModCache) || curr == cleanModCache || curr == "." || curr == "/" {
			break
		}
		modFilePath := filepath.Join(curr, "go.mod")
		if fileExists(modFilePath) {
			if dc != nil {
				dc.modMu.RLock()
				if m, ok := dc.modInfos[curr]; ok {
					dc.modMu.RUnlock()
					return m
				}
				dc.modMu.RUnlock()
			}
			data, err := os.ReadFile(modFilePath)
			if err == nil && len(data) > 0 {
				m := parseGoMod(data)
				m.modDir = curr
				if dc != nil {
					dc.modMu.Lock()
					dc.modInfos[curr] = m
					dc.modMu.Unlock()
				}
				return m
			}
		}
		if strings.Contains(filepath.Base(curr), "@") {
			break
		}
		next := filepath.Dir(curr)
		if next == curr {
			break
		}
		curr = next
	}
	return nil
}

func dirHasGoFiles(absDir string) bool {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasPrefix(e.Name(), ".") {
			return true
		}
	}
	return false
}

type fileReadCache struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newFileReadCache() *fileReadCache {
	return &fileReadCache{files: make(map[string][]byte)}
}

func (c *fileReadCache) get(path string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.files[path]
	return data, ok
}

func (c *fileReadCache) put(path string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[path] = data
}

type lookupLoader struct {
	ctx             context.Context
	req             LookupRequest
	mod             *moduleInfo
	env             goEnv
	goos            string
	goarch          string
	buildTags       []string
	releaseTags     []string
	customBuild     bool
	diskCache       *diskCache
	revCache        *diskCache
	crossRevPkgs    *crossRevPkgCache
	crossRevChecked *crossRevCheckedLRU
	fset            *token.FileSet
	inRepoPkgs      map[string]*types.Package
	testBasePkgs    map[string]*types.Package
	loading         map[string]bool
	diskLoading     map[diskCacheKey]bool
}

func newLookupLoader(ctx context.Context, req LookupRequest, mod *moduleInfo, dc *diskCache, rc *diskCache) *lookupLoader {
	if ctx == nil {
		ctx = context.Background()
	}
	env := getGoEnv()
	goos := env.goos
	goarch := env.goarch

	// Adjust platform if file has explicit suffix
	base := filepath.Base(req.FilePath)
	extIdx := strings.LastIndex(base, ".go")
	if extIdx != -1 {
		stem := base[:extIdx]
		parts := strings.Split(stem, "_")
		if len(parts) >= 2 {
			for _, part := range parts[1:] {
				switch part {
				case "windows", "darwin", "linux", "freebsd", "openbsd", "netbsd", "plan9", "solaris", "aix", "illumos":
					goos = part
				case "amd64", "arm64", "386", "arm", "riscv64", "ppc64", "ppc64le", "s390x", "wasm", "mips", "mipsle", "mips64", "mips64le":
					goarch = part
				}
			}
		}
	}

	return &lookupLoader{
		ctx:             ctx,
		req:             req,
		mod:             mod,
		env:             env,
		goos:            goos,
		goarch:          goarch,
		releaseTags:     build.Default.ReleaseTags,
		diskCache:       dc,
		revCache:        rc,
		crossRevPkgs:    sharedCrossRevPkgCache,
		crossRevChecked: sharedCrossRevCheckedLRU,
		fset:            token.NewFileSet(),
		inRepoPkgs:      make(map[string]*types.Package),
		testBasePkgs:    make(map[string]*types.Package),
		loading:         make(map[string]bool),
		diskLoading:     make(map[diskCacheKey]bool),
	}
}

func (l *lookupLoader) clone() *lookupLoader {
	if l == nil {
		return nil
	}
	inRepo := make(map[string]*types.Package, len(l.inRepoPkgs))
	for k, pkg := range l.inRepoPkgs {
		if (l.revCache != nil && l.revCache.has(pkg)) || (l.diskCache != nil && l.diskCache.has(pkg)) {
			inRepo[k] = pkg
		}
	}
	testBase := make(map[string]*types.Package, len(l.testBasePkgs))
	for k, pkg := range l.testBasePkgs {
		if (l.revCache != nil && l.revCache.has(pkg)) || (l.diskCache != nil && l.diskCache.has(pkg)) {
			testBase[k] = pkg
		}
	}
	return &lookupLoader{
		ctx:             l.ctx,
		req:             l.req,
		mod:             l.mod,
		env:             l.env,
		goos:            l.goos,
		goarch:          l.goarch,
		buildTags:       slices.Clone(l.buildTags),
		releaseTags:     slices.Clone(l.releaseTags),
		customBuild:     l.customBuild,
		diskCache:       l.diskCache,
		revCache:        l.revCache,
		crossRevPkgs:    l.crossRevPkgs,
		crossRevChecked: l.crossRevChecked,
		fset:            token.NewFileSet(),
		inRepoPkgs:      inRepo,
		testBasePkgs:    testBase,
		loading:         make(map[string]bool),
		diskLoading:     make(map[diskCacheKey]bool),
	}
}

func (l *lookupLoader) positionFor(obj types.Object, rootPkg *types.Package) token.Position {
	objPos := obj.Pos()
	if !objPos.IsValid() {
		return token.Position{}
	}
	pkg := obj.Pkg()
	if pkg == nil || pkg == rootPkg {
		return l.fset.Position(objPos)
	}
	if l.crossRevPkgs != nil {
		if fset := l.crossRevPkgs.fsetFor(pkg); fset != nil {
			return fset.Position(objPos)
		}
	}
	if l.revCache != nil {
		if pos, ok := l.revCache.positionForPkg(pkg, objPos); ok {
			return pos
		}
		if l.revCache.has(pkg) {
			return l.revCache.position(objPos)
		}
	}
	if l.diskCache != nil {
		if pos, ok := l.diskCache.positionForPkg(pkg, objPos); ok {
			return pos
		}
		if l.diskCache.has(pkg) {
			return l.diskCache.position(objPos)
		}
	}
	return l.fset.Position(objPos)
}

func (l *lookupLoader) buildTagsSig() string {
	if !l.customBuild {
		return ""
	}
	var parts []string
	if len(l.buildTags) > 0 {
		sorted := slices.Clone(l.buildTags)
		slices.Sort(sorted)
		parts = append(parts, "tags="+strings.Join(sorted, ","))
	}
	if len(l.releaseTags) != len(build.Default.ReleaseTags) {
		parts = append(parts, fmt.Sprintf("rel=%d", len(l.releaseTags)))
	}
	if len(parts) == 0 {
		return "custom"
	}
	return strings.Join(parts, ";")
}

func (l *lookupLoader) makeCacheKey(importPath, absDir string) diskCacheKey {
	return diskCacheKey{
		goos:       l.goos,
		goarch:     l.goarch,
		importPath: importPath,
		absDir:     absDir,
		tagsSig:    l.buildTagsSig(),
	}
}

func (l *lookupLoader) makeBuildContextWithCache() (*build.Context, *fileReadCache) {
	fc := newFileReadCache()
	relTags := l.releaseTags
	if !l.customBuild && relTags == nil {
		relTags = build.Default.ReleaseTags
	}
	bctx := &build.Context{
		GOOS:        l.goos,
		GOARCH:      l.goarch,
		CgoEnabled:  true,
		BuildTags:   l.buildTags,
		ReleaseTags: relTags,
	}
	bctx.OpenFile = func(p string) (io.ReadCloser, error) {
		if data, ok := fc.get(p); ok {
			return io.NopCloser(bytes.NewReader(data)), nil
		}
		if l.req.Repo != nil && l.req.RepoRoot != "" {
			rel, err := filepath.Rel(l.req.RepoRoot, p)
			if err == nil && !strings.HasPrefix(rel, "..") {
				data, err := l.req.Repo.Content(l.rev(), filepath.ToSlash(rel))
				if err == nil {
					fc.put(p, data)
					return io.NopCloser(bytes.NewReader(data)), nil
				}
			}
		}
		data, err := os.ReadFile(p)
		if err == nil {
			fc.put(p, data)
			return io.NopCloser(bytes.NewReader(data)), nil
		}
		return nil, err
	}
	return bctx, fc
}

func (l *lookupLoader) matchAndReadInRepo(bctx *build.Context, fc *fileReadCache, dirInRepo, name string) ([]byte, bool) {
	absDir := filepath.Join(l.req.RepoRoot, filepath.FromSlash(dirInRepo))
	absPath := filepath.Join(absDir, name)
	match, _ := bctx.MatchFile(absDir, name)
	if !match {
		return nil, false
	}
	if data, ok := fc.get(absPath); ok {
		return data, true
	}
	filePath := name
	if dirInRepo != "" && dirInRepo != "." {
		filePath = path.Join(dirInRepo, name)
	}
	data, err := l.req.Repo.Content(l.rev(), filePath)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	fc.put(absPath, data)
	return data, true
}

func (l *lookupLoader) matchAndReadDisk(bctx *build.Context, fc *fileReadCache, absDir, name string) ([]byte, bool) {
	absPath := filepath.Join(absDir, name)
	match, _ := bctx.MatchFile(absDir, name)
	if !match {
		return nil, false
	}
	if data, ok := fc.get(absPath); ok {
		return data, true
	}
	data, err := os.ReadFile(absPath)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	fc.put(absPath, data)
	return data, true
}

func (l *lookupLoader) Import(importPath string) (*types.Package, error) {
	if importPath == "unsafe" {
		return types.Unsafe, nil
	}
	if importPath == "C" {
		return nil, nil
	}

	// 1. Check in-repo cache for this lookup
	if p, ok := l.inRepoPkgs[importPath]; ok {
		return p, nil
	}

	// Cycle protection
	if l.loading[importPath] {
		stub := types.NewPackage(importPath, guessPkgName(importPath))
		stub.MarkComplete()
		return stub, nil
	}
	l.loading[importPath] = true
	defer func() {
		delete(l.loading, importPath)
	}()

	dir, isDisk, err := l.resolvePackageDir(importPath)
	if err == nil {
		if isDisk {
			if pkg, err := l.loadDiskPackage(importPath, dir); err == nil {
				l.inRepoPkgs[importPath] = pkg
				return pkg, nil
			}
		} else {
			if pkg, err := l.loadInRepoPackage(importPath, dir, true); err == nil {
				l.inRepoPkgs[importPath] = pkg
				return pkg, nil
			}
		}
	}

	// Hermetic stub fallback
	stub := types.NewPackage(importPath, guessPkgName(importPath))
	stub.MarkComplete()
	l.inRepoPkgs[importPath] = stub
	return stub, nil
}

func (l *lookupLoader) rev() string {
	if l.req.Side == "L" && l.req.BaseRev != "" {
		return l.req.BaseRev
	}
	if l.req.TargetRev != "" {
		return l.req.TargetRev
	}
	return "working"
}

func (l *lookupLoader) loadInRepoPackage(importPath, dirInRepo string, ignoreFuncBodies bool) (*types.Package, error) {
	useRevCache := ignoreFuncBodies && l.revCache != nil
	var cacheKey diskCacheKey
	var doneCh chan struct{}
	if useRevCache {
		cacheKey = l.makeCacheKey(importPath, dirInRepo)
		for {
			l.revCache.mu.Lock()
			if pkg, ok := l.revCache.pkgs[cacheKey]; ok {
				l.revCache.mu.Unlock()
				return pkg, nil
			}
			if waitCh, busy := l.revCache.inflight[cacheKey]; busy {
				l.revCache.mu.Unlock()
				select {
				case <-waitCh:
					continue
				case <-l.ctx.Done():
					stub := types.NewPackage(importPath, guessPkgName(importPath))
					stub.MarkComplete()
					return stub, l.ctx.Err()
				}
			}
			doneCh = make(chan struct{})
			l.revCache.inflight[cacheKey] = doneCh
			l.revCache.mu.Unlock()
			break
		}
		defer func() {
			l.revCache.mu.Lock()
			delete(l.revCache.inflight, cacheKey)
			l.revCache.mu.Unlock()
			close(doneCh)
		}()
	}

	var merkleDigest string
	var matchedEntries []*repoFileEntry
	if useRevCache && l.crossRevPkgs != nil && l.revCache != nil {
		if fileIdx := l.revCache.peekRepoFileIndex(); fileIdx != nil {
			indices := fileIdx.dirFiles[dirInRepo]
			relTags := l.releaseTags
			if !l.customBuild && relTags == nil {
				relTags = build.Default.ReleaseTags
			}
			bctx := &build.Context{
				GOOS:        l.goos,
				GOARCH:      l.goarch,
				CgoEnabled:  true,
				BuildTags:   l.buildTags,
				ReleaseTags: relTags,
			}
			canMerkle := true
			for _, idx := range indices {
				e := &fileIdx.files[idx]
				if strings.HasSuffix(e.path, "_test.go") || strings.HasPrefix(path.Base(e.path), ".") || strings.HasPrefix(path.Base(e.path), "_") {
					continue
				}
				if matchIndexedGoFile(bctx, l.req.RepoRoot, e) {
					if e.oid == "" || !e.importsExact {
						canMerkle = false
						break
					}
					matchedEntries = append(matchedEntries, e)
				}
			}

			if canMerkle && len(matchedEntries) > 0 {
				impMap := make(map[string]bool)
				for _, e := range matchedEntries {
					for _, imp := range e.imports {
						if imp != "" && imp != "C" && imp != "unsafe" {
							impMap[imp] = true
						}
					}
				}
				var directImports []string
				for imp := range impMap {
					directImports = append(directImports, imp)
				}
				slices.Sort(directImports)

				impDigests := make(map[string]string, len(directImports))
				for _, imp := range directImports {
					if l.loading[imp] {
						canMerkle = false
						break
					}
					impPkg, err := l.Import(imp)
					if err != nil || impPkg == nil {
						canMerkle = false
						break
					}
					var d string
					if impPkg == types.Unsafe {
						d = "unsafe"
					} else if l.diskCache != nil && l.diskCache.has(impPkg) {
						d = "disk:" + impPkg.Path() + "@" + l.diskCache.dirFor(impPkg)
					} else if cd := l.crossRevPkgs.digestFor(impPkg); cd != "" {
						d = "repo:" + cd
					} else {
						canMerkle = false
						break
					}
					impDigests[imp] = d
				}

				if canMerkle {
					merkleDigest = computePkgMerkleDigest(
						l.req.RepoRoot, importPath, dirInRepo, l.goos, l.goarch, l.buildTagsSig(),
						matchedEntries, directImports, impDigests,
					)
					if hit := l.crossRevPkgs.get(merkleDigest); hit != nil {
						l.revCache.registerPkg(hit.pkg)
						l.revCache.put(cacheKey, hit.pkg)
						if hit.fset != nil {
							l.revCache.registerPkgFset(hit.pkg, hit.fset)
						}
						return hit.pkg, nil
					}
				}
			}
		}
	}

	var astFiles []*ast.File
	var targetFset *token.FileSet

	if merkleDigest != "" {
		targetFset = token.NewFileSet()
		var paths []string
		var oids []string
		for _, e := range matchedEntries {
			paths = append(paths, e.path)
			oids = append(oids, e.oid)
		}
		contents := readRepoFilesBatch(l.req.Repo, l.rev(), paths, oids)
		for _, e := range matchedEntries {
			data := contents[e.path]
			if len(data) == 0 {
				continue
			}
			virtualPath := filepath.Join(l.req.RepoRoot, filepath.FromSlash(e.path))
			f, err := parseGoFile(targetFset, virtualPath, data, parser.SkipObjectResolution, ignoreFuncBodies)
			if err == nil && f != nil {
				astFiles = append(astFiles, f)
			}
		}
	} else {
		targetFset = l.fset
		if useRevCache {
			targetFset = token.NewFileSet()
		}

		type fallbackFileHash struct {
			name string
			oid  string
		}
		var fallbackHashes []fallbackFileHash
		bctx, fc := l.makeBuildContextWithCache()
		var names []string
		var listErr error

		if dirRepo, ok := l.req.Repo.(vcs.DirGoBlobRepo); ok && l.rev() != "working" {
			blobs, err := dirRepo.ReadDirGoFiles(l.rev(), dirInRepo)
			if err == nil {
				for _, b := range blobs {
					absDir := filepath.Join(l.req.RepoRoot, filepath.FromSlash(dirInRepo))
					absPath := filepath.Join(absDir, b.Name)
					fc.put(absPath, b.Data)
					if match, _ := bctx.MatchFile(absDir, b.Name); !match {
						continue
					}
					fallbackHashes = append(fallbackHashes, fallbackFileHash{name: b.Name, oid: b.OID})
					filePath := b.Name
					if dirInRepo != "" && dirInRepo != "." {
						filePath = path.Join(dirInRepo, b.Name)
					}
					virtualPath := filepath.Join(l.req.RepoRoot, filepath.FromSlash(filePath))
					f, err := parseGoFile(targetFset, virtualPath, b.Data, parser.SkipObjectResolution, ignoreFuncBodies)
					if err == nil && f != nil {
						astFiles = append(astFiles, f)
					}
				}
				goto afterFilesParsed
			} else if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("no files in %s", dirInRepo)
			}
		}

		names, listErr = l.req.Repo.ListDir(l.rev(), dirInRepo)
		if listErr != nil || len(names) == 0 {
			return nil, fmt.Errorf("no files in %s", dirInRepo)
		}

		slices.Sort(names)
		for _, name := range names {
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			data, ok := l.matchAndReadInRepo(bctx, fc, dirInRepo, name)
			if !ok || len(data) == 0 {
				continue
			}
			fallbackHashes = append(fallbackHashes, fallbackFileHash{name: name, oid: gitBlobOID(data, len(l.rev()))})
			filePath := name
			if dirInRepo != "" && dirInRepo != "." {
				filePath = path.Join(dirInRepo, name)
			}
			virtualPath := filepath.Join(l.req.RepoRoot, filepath.FromSlash(filePath))
			f, err := parseGoFile(targetFset, virtualPath, data, parser.SkipObjectResolution, ignoreFuncBodies)
			if err == nil && f != nil {
				astFiles = append(astFiles, f)
			}
		}

	afterFilesParsed:

		if len(astFiles) > 0 && useRevCache && l.crossRevPkgs != nil {
			impMap := make(map[string]bool)
			for _, f := range astFiles {
				for _, spec := range f.Imports {
					if spec != nil && spec.Path != nil {
						imp, _ := strconv.Unquote(spec.Path.Value)
						if imp != "" && imp != "C" && imp != "unsafe" {
							impMap[imp] = true
						}
					}
				}
			}
			var directImports []string
			for imp := range impMap {
				directImports = append(directImports, imp)
			}
			slices.Sort(directImports)

			canMerkleFallback := true
			impDigests := make(map[string]string, len(directImports))
			for _, imp := range directImports {
				if l.loading[imp] {
					canMerkleFallback = false
					break
				}
				impPkg, err := l.Import(imp)
				if err != nil || impPkg == nil {
					canMerkleFallback = false
					break
				}
				var d string
				if impPkg == types.Unsafe {
					d = "unsafe"
				} else if l.diskCache != nil && l.diskCache.has(impPkg) {
					d = "disk:" + impPkg.Path() + "@" + l.diskCache.dirFor(impPkg)
				} else if cd := l.crossRevPkgs.digestFor(impPkg); cd != "" {
					d = "repo:" + cd
				} else {
					canMerkleFallback = false
					break
				}
				impDigests[imp] = d
			}

			if canMerkleFallback {
				mh := sha256.New()
				fmt.Fprintf(mh, "pkg:%s|%s|%s|%s|%s|%s\n", l.req.RepoRoot, importPath, dirInRepo, l.goos, l.goarch, l.buildTagsSig())
				for _, fh := range fallbackHashes {
					fmt.Fprintf(mh, "f:%s=%s\n", fh.name, fh.oid)
				}
				for _, imp := range directImports {
					fmt.Fprintf(mh, "i:%s=%s\n", imp, impDigests[imp])
				}
				merkleDigest = hex.EncodeToString(mh.Sum(nil))
				if hit := l.crossRevPkgs.get(merkleDigest); hit != nil {
					l.revCache.registerPkg(hit.pkg)
					l.revCache.put(cacheKey, hit.pkg)
					if hit.fset != nil {
						l.revCache.registerPkgFset(hit.pkg, hit.fset)
					}
					return hit.pkg, nil
				}
			}
		}
	}

	if len(astFiles) == 0 {
		stub := types.NewPackage(importPath, guessPkgName(importPath))
		stub.MarkComplete()
		if useRevCache && l.ctx.Err() == nil {
			l.revCache.put(cacheKey, stub)
		}
		return stub, nil
	}

	conf := types.Config{
		IgnoreFuncBodies: ignoreFuncBodies,
		FakeImportC:      true,
		Importer:         l,
		Error:            func(error) {},
	}

	pkg, err := conf.Check(importPath, targetFset, astFiles, nil)
	if pkg != nil {
		if l.ctx.Err() == nil {
			if merkleDigest != "" && l.crossRevPkgs != nil {
				l.crossRevPkgs.put(l.req.RepoRoot, merkleDigest, pkg, targetFset)
			}
			if useRevCache {
				l.revCache.put(cacheKey, pkg)
				l.revCache.registerPkg(pkg)
				l.revCache.registerPkgFset(pkg, targetFset)
			}
		}
		return pkg, nil
	}

	stub := types.NewPackage(importPath, guessPkgName(importPath))
	stub.MarkComplete()
	if useRevCache && l.ctx.Err() == nil {
		l.revCache.put(cacheKey, stub)
	}
	return stub, err
}

func (l *lookupLoader) loadDiskPackage(importPath, absDir string) (*types.Package, error) {
	key := l.makeCacheKey(importPath, absDir)

	if l.diskLoading[key] {
		stub := types.NewPackage(importPath, guessPkgName(importPath))
		stub.MarkComplete()
		return stub, nil
	}

	var doneCh chan struct{}
	for {
		l.diskCache.mu.Lock()
		if pkg, ok := l.diskCache.pkgs[key]; ok {
			l.diskCache.mu.Unlock()
			return pkg, nil
		}
		if waitCh, busy := l.diskCache.inflight[key]; busy {
			l.diskCache.mu.Unlock()
			select {
			case <-waitCh:
				continue
			case <-l.ctx.Done():
				stub := types.NewPackage(importPath, guessPkgName(importPath))
				stub.MarkComplete()
				return stub, l.ctx.Err()
			}
		}
		doneCh = make(chan struct{})
		l.diskCache.inflight[key] = doneCh
		l.diskCache.mu.Unlock()
		break
	}

	l.diskLoading[key] = true
	defer func() {
		delete(l.diskLoading, key)
		l.diskCache.mu.Lock()
		delete(l.diskCache.inflight, key)
		l.diskCache.mu.Unlock()
		close(doneCh)
	}()

	entries, err := os.ReadDir(absDir)
	if err != nil {
		stub := types.NewPackage(importPath, guessPkgName(importPath))
		stub.MarkComplete()
		return stub, nil
	}

	bctx, fc := l.makeBuildContextWithCache()
	var astFiles []*ast.File

	localFset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		data, ok := l.matchAndReadDisk(bctx, fc, absDir, name)
		if !ok || len(data) == 0 {
			continue
		}
		filePath := filepath.Join(absDir, name)
		f, err := parseGoFile(localFset, filePath, data, parser.SkipObjectResolution, true)
		if err == nil && f != nil {
			astFiles = append(astFiles, f)
		}
	}

	if len(astFiles) == 0 {
		stub := types.NewPackage(importPath, guessPkgName(importPath))
		stub.MarkComplete()
		if l.ctx.Err() == nil {
			l.diskCache.put(key, stub)
		}
		return stub, nil
	}

	var importer types.Importer
	if l.env.goroot != "" && strings.HasPrefix(absDir, filepath.Join(l.env.goroot, "src")+string(filepath.Separator)) {
		importer = newStdlibImporter(l)
	} else if l.env.gomodcache != "" && strings.HasPrefix(absDir, filepath.Clean(l.env.gomodcache)+string(filepath.Separator)) {
		importer = newModCacheImporter(l, absDir)
	} else {
		importer = l
	}

	conf := types.Config{
		IgnoreFuncBodies: true,
		FakeImportC:      true,
		Importer:         importer,
		Error:            func(error) {},
	}

	pkg, err := conf.Check(importPath, localFset, astFiles, nil)
	if pkg != nil {
		if l.ctx.Err() == nil {
			l.diskCache.put(key, pkg)
			l.diskCache.registerPkgDir(pkg, absDir)
			l.diskCache.registerPkgFset(pkg, localFset)
		}
		return pkg, nil
	}

	stub := types.NewPackage(importPath, guessPkgName(importPath))
	stub.MarkComplete()
	if l.ctx.Err() == nil {
		l.diskCache.put(key, stub)
	}
	return stub, err
}

type stdlibImporter struct {
	loader *lookupLoader
	pkgs   map[string]*types.Package
}

func newStdlibImporter(loader *lookupLoader) *stdlibImporter {
	return &stdlibImporter{
		loader: loader,
		pkgs:   make(map[string]*types.Package),
	}
}

func (si *stdlibImporter) Import(importPath string) (*types.Package, error) {
	if importPath == "unsafe" {
		return types.Unsafe, nil
	}
	if importPath == "C" {
		return nil, nil
	}
	if p, ok := si.pkgs[importPath]; ok {
		return p, nil
	}

	goroot := si.loader.env.goroot
	if goroot != "" {
		stdDir := filepath.Join(goroot, "src", filepath.FromSlash(importPath))
		if dirExists(stdDir) {
			if pkg, err := si.loader.loadDiskPackage(importPath, stdDir); err == nil {
				si.pkgs[importPath] = pkg
				return pkg, nil
			}
		}
		stdVendorDir := filepath.Join(goroot, "src", "vendor", filepath.FromSlash(importPath))
		if dirExists(stdVendorDir) {
			if pkg, err := si.loader.loadDiskPackage(importPath, stdVendorDir); err == nil {
				si.pkgs[importPath] = pkg
				return pkg, nil
			}
		}
	}

	stub := types.NewPackage(importPath, guessPkgName(importPath))
	stub.MarkComplete()
	si.pkgs[importPath] = stub
	return stub, nil
}

type modCacheImporter struct {
	loader  *lookupLoader
	modRoot string
	mod     *moduleInfo
	pkgs    map[string]*types.Package
}

func newModCacheImporter(loader *lookupLoader, absDir string) *modCacheImporter {
	var modInfo *moduleInfo
	var modRoot string
	if loader.env.gomodcache != "" && strings.HasPrefix(absDir, filepath.Clean(loader.env.gomodcache)+string(filepath.Separator)) {
		modInfo = loader.diskCache.findModCacheModuleInfo(loader.env.gomodcache, absDir)
		if modInfo != nil {
			modRoot = modInfo.modDir
		}
	}
	return &modCacheImporter{
		loader:  loader,
		modRoot: modRoot,
		mod:     modInfo,
		pkgs:    make(map[string]*types.Package),
	}
}

func (mci *modCacheImporter) Import(importPath string) (*types.Package, error) {
	if importPath == "unsafe" {
		return types.Unsafe, nil
	}
	if importPath == "C" {
		return nil, nil
	}
	if p, ok := mci.pkgs[importPath]; ok {
		return p, nil
	}

	// 1. Same module's own subpackages
	if mci.mod != nil && mci.mod.modPath != "" && mci.modRoot != "" {
		if importPath == mci.mod.modPath {
			if dirHasGoFiles(mci.modRoot) {
				if pkg, err := mci.loader.loadDiskPackage(importPath, mci.modRoot); err == nil {
					mci.pkgs[importPath] = pkg
					return pkg, nil
				}
			}
		} else if strings.HasPrefix(importPath, mci.mod.modPath+"/") {
			sub := strings.TrimPrefix(importPath, mci.mod.modPath+"/")
			targetDir := filepath.Join(mci.modRoot, filepath.FromSlash(sub))
			if dirHasGoFiles(targetDir) {
				if pkg, err := mci.loader.loadDiskPackage(importPath, targetDir); err == nil {
					mci.pkgs[importPath] = pkg
					return pkg, nil
				}
			}
		}
	}

	// 2. That module's own replaces
	if rep, sub, ok := mci.mod.matchReplaceTarget(importPath); ok {
		if rep.isLocal {
			target := rep.path
			if !filepath.IsAbs(target) && mci.modRoot != "" {
				target = filepath.Join(mci.modRoot, filepath.FromSlash(target))
			}
			if sub != "" {
				target = filepath.Join(target, filepath.FromSlash(sub))
			}
			target = filepath.Clean(target)
			if dirHasGoFiles(target) {
				if pkg, err := mci.loader.loadDiskPackage(importPath, target); err == nil {
					mci.pkgs[importPath] = pkg
					return pkg, nil
				}
			}
		} else if targetDir, ok := resolveModCacheReplaceDir(rep, sub, mci.loader.env.gomodcache); ok {
			if pkg, err := mci.loader.loadDiskPackage(importPath, targetDir); err == nil {
				mci.pkgs[importPath] = pkg
				return pkg, nil
			}
		}
	}

	// 3. That module's own requires
	if targetDir, ok := mci.mod.resolveRequireDir(importPath, mci.loader.env.gomodcache); ok {
		if pkg, err := mci.loader.loadDiskPackage(importPath, targetDir); err == nil {
			mci.pkgs[importPath] = pkg
			return pkg, nil
		}
	}

	// 4. Fallback to loader.Import
	pkg, err := mci.loader.Import(importPath)
	if err == nil && pkg != nil {
		mci.pkgs[importPath] = pkg
	}
	return pkg, err
}

func (l *lookupLoader) hasInRepoGoFiles(dirInRepo string) bool {
	if l.req.Repo == nil {
		return false
	}
	names, err := l.req.Repo.ListDir(l.rev(), dirInRepo)
	if err != nil {
		return false
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".go") {
			return true
		}
	}
	return false
}

func (l *lookupLoader) resolvePackageDir(importPath string) (dir string, isDisk bool, err error) {
	if importPath == "builtin" {
		if l.env.goroot != "" {
			bDir := filepath.Join(l.env.goroot, "src", "builtin")
			if dirHasGoFiles(bDir) {
				return bDir, true, nil
			}
		}
		return "", false, fmt.Errorf("builtin directory not found")
	}

	// 1. In-repo vendor directory under modDir
	if l.mod.modDir != "" || l.mod.modPath != "" {
		vendorDir := "vendor"
		if l.mod.modDir != "" {
			vendorDir = path.Join(l.mod.modDir, "vendor")
		}
		candidate := path.Join(vendorDir, importPath)
		if l.hasInRepoGoFiles(candidate) {
			return candidate, false, nil
		}
	}

	// 2. Subpackage-aware replace directives in go.mod
	if rep, sub, ok := l.mod.matchReplaceTarget(importPath); ok {
		if rep.isLocal {
			cleanLocal := filepath.ToSlash(filepath.Clean(rep.path))
			if !filepath.IsAbs(rep.path) {
				inRepoTarget := cleanLocal
				if l.mod.modDir != "" {
					inRepoTarget = path.Join(l.mod.modDir, cleanLocal)
				}
				if sub != "" {
					inRepoTarget = path.Join(inRepoTarget, sub)
				}
				inRepoTarget = path.Clean(inRepoTarget)
				if !strings.HasPrefix(inRepoTarget, "..") {
					if l.hasInRepoGoFiles(inRepoTarget) {
						return inRepoTarget, false, nil
					}
				} else {
					absDisk := filepath.Clean(filepath.Join(l.req.RepoRoot, filepath.FromSlash(inRepoTarget)))
					if dirHasGoFiles(absDisk) {
						return absDisk, true, nil
					}
				}
			} else {
				target := rep.path
				if sub != "" {
					target = filepath.Join(target, filepath.FromSlash(sub))
				}
				target = filepath.Clean(target)
				if dirHasGoFiles(target) {
					return target, true, nil
				}
			}
		} else if targetDir, ok := resolveModCacheReplaceDir(rep, sub, l.env.gomodcache); ok {
			return targetDir, true, nil
		}
	}

	// 3. In-repo module
	if l.mod.modPath != "" {
		if importPath == l.mod.modPath {
			if l.hasInRepoGoFiles(l.mod.modDir) {
				return l.mod.modDir, false, nil
			}
		} else if strings.HasPrefix(importPath, l.mod.modPath+"/") {
			sub := strings.TrimPrefix(importPath, l.mod.modPath+"/")
			inRepoDir := sub
			if l.mod.modDir != "" {
				inRepoDir = path.Join(l.mod.modDir, sub)
			}
			if l.hasInRepoGoFiles(inRepoDir) {
				return inRepoDir, false, nil
			}
		}
	}

	// 4. In-repo siblings
	for sibMod, sibDir := range l.mod.siblingMods {
		if importPath == sibMod {
			if l.hasInRepoGoFiles(sibDir) {
				return sibDir, false, nil
			}
		} else if strings.HasPrefix(importPath, sibMod+"/") {
			sub := strings.TrimPrefix(importPath, sibMod+"/")
			inRepoDir := path.Join(sibDir, sub)
			if l.hasInRepoGoFiles(inRepoDir) {
				return inRepoDir, false, nil
			}
		}
	}

	// 5. In-repo std
	if l.mod.modPath == "std" {
		inRepoDir := path.Join(l.mod.modDir, importPath)
		if l.hasInRepoGoFiles(inRepoDir) {
			return inRepoDir, false, nil
		}
	}

	// 6. Subpackage-aware requires in go.mod (GOMODCACHE)
	if targetDir, ok := l.mod.resolveRequireDir(importPath, l.env.gomodcache); ok {
		return targetDir, true, nil
	}

	// 7. Automatic $GOMODCACHE fallback when not in go.mod
	firstSeg := importPath
	if idx := strings.Index(firstSeg, "/"); idx != -1 {
		firstSeg = firstSeg[:idx]
	}
	if strings.Contains(firstSeg, ".") && l.env.gomodcache != "" {
		cacheKey := l.env.gomodcache + "\x00" + importPath
		if l.diskCache != nil {
			l.diskCache.modMu.RLock()
			cachedDir, ok := l.diskCache.semverDirs[cacheKey]
			l.diskCache.modMu.RUnlock()
			if ok && cachedDir != "" && dirHasGoFiles(cachedDir) {
				return cachedDir, true, nil
			}
		}

		parts := strings.Split(importPath, "/")
		for k := len(parts); k >= 1; k-- {
			candMod := strings.Join(parts[:k], "/")
			sub := strings.Join(parts[k:], "/")
			escMod := escapeModPath(candMod)
			targetBase := filepath.Base(escMod)
			parentDir := filepath.Dir(filepath.Join(l.env.gomodcache, filepath.FromSlash(escMod)))
			entries, err := os.ReadDir(parentDir)
			if err != nil {
				continue
			}
			prefix := targetBase + "@"
			var bestVer string
			var bestDir string
			for _, e := range entries {
				if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
					continue
				}
				ver := strings.TrimPrefix(e.Name(), prefix)
				candDir := filepath.Join(parentDir, e.Name())
				if sub != "" {
					candDir = filepath.Join(candDir, filepath.FromSlash(sub))
				}
				if !dirHasGoFiles(candDir) {
					continue
				}
				if bestVer == "" || compareSemver(ver, bestVer) > 0 {
					bestVer = ver
					bestDir = candDir
				}
			}
			if bestDir != "" {
				if l.diskCache != nil {
					l.diskCache.modMu.Lock()
					l.diskCache.semverDirs[cacheKey] = bestDir
					l.diskCache.modMu.Unlock()
				}
				return bestDir, true, nil
			}
		}
	}

	// 8. $GOPATH/src fallback
	if l.env.gopath != "" {
		gpDir := filepath.Join(l.env.gopath, "src", filepath.FromSlash(importPath))
		if dirHasGoFiles(gpDir) {
			return gpDir, true, nil
		}
	}

	// 9. Stdlib on disk
	if l.env.goroot != "" {
		stdDir := filepath.Join(l.env.goroot, "src", filepath.FromSlash(importPath))
		if dirHasGoFiles(stdDir) {
			return stdDir, true, nil
		}
		stdVendorDir := filepath.Join(l.env.goroot, "src", "vendor", filepath.FromSlash(importPath))
		if dirHasGoFiles(stdVendorDir) {
			return stdVendorDir, true, nil
		}
	}

	return "", false, fmt.Errorf("package %s not found", importPath)
}

func (l *lookupLoader) loadInRepoTestBasePackage(importPath, dirInRepo string) (*types.Package, error) {
	if l.testBasePkgs == nil {
		l.testBasePkgs = make(map[string]*types.Package)
	}
	if pkg, ok := l.testBasePkgs[importPath]; ok {
		return pkg, nil
	}

	useRevCache := l.revCache != nil
	var cacheKey diskCacheKey
	var doneCh chan struct{}
	if useRevCache {
		cacheKey = l.makeCacheKey(importPath+"#testbase", dirInRepo)
		for {
			l.revCache.mu.Lock()
			if pkg, ok := l.revCache.pkgs[cacheKey]; ok {
				l.revCache.mu.Unlock()
				l.testBasePkgs[importPath] = pkg
				return pkg, nil
			}
			if ch, ok := l.revCache.inflight[cacheKey]; ok {
				l.revCache.mu.Unlock()
				select {
				case <-ch:
					continue
				case <-l.ctx.Done():
					return nil, l.ctx.Err()
				}
			}
			doneCh = make(chan struct{})
			l.revCache.inflight[cacheKey] = doneCh
			l.revCache.mu.Unlock()
			break
		}
		defer func() {
			l.revCache.mu.Lock()
			delete(l.revCache.inflight, cacheKey)
			l.revCache.mu.Unlock()
			close(doneCh)
		}()
	}

	var merkleDigest string
	var matchedEntries []*repoFileEntry
	if useRevCache && l.crossRevPkgs != nil && l.revCache != nil {
		if fileIdx := l.revCache.peekRepoFileIndex(); fileIdx != nil {
			indices := fileIdx.dirFiles[dirInRepo]
			relTags := l.releaseTags
			if !l.customBuild && relTags == nil {
				relTags = build.Default.ReleaseTags
			}
			bctx := &build.Context{
				GOOS:        l.goos,
				GOARCH:      l.goarch,
				CgoEnabled:  true,
				BuildTags:   l.buildTags,
				ReleaseTags: relTags,
			}
			canMerkle := true
			for _, idx := range indices {
				e := &fileIdx.files[idx]
				if strings.HasSuffix(e.pkgName, "_test") || strings.HasPrefix(path.Base(e.path), ".") || strings.HasPrefix(path.Base(e.path), "_") {
					continue
				}
				if matchIndexedGoFile(bctx, l.req.RepoRoot, e) {
					if e.oid == "" || !e.importsExact {
						canMerkle = false
						break
					}
					matchedEntries = append(matchedEntries, e)
				}
			}

			if canMerkle && len(matchedEntries) > 0 {
				impMap := make(map[string]bool)
				for _, e := range matchedEntries {
					for _, imp := range e.imports {
						if imp != "" && imp != "C" && imp != "unsafe" {
							impMap[imp] = true
						}
					}
				}
				var directImports []string
				for imp := range impMap {
					directImports = append(directImports, imp)
				}
				slices.Sort(directImports)

				impDigests := make(map[string]string, len(directImports))
				for _, imp := range directImports {
					if l.loading[imp] {
						canMerkle = false
						break
					}
					impPkg, err := l.Import(imp)
					if err != nil || impPkg == nil {
						canMerkle = false
						break
					}
					var d string
					if impPkg == types.Unsafe {
						d = "unsafe"
					} else if l.diskCache != nil && l.diskCache.has(impPkg) {
						d = "disk:" + impPkg.Path() + "@" + l.diskCache.dirFor(impPkg)
					} else if cd := l.crossRevPkgs.digestFor(impPkg); cd != "" {
						d = "repo:" + cd
					} else {
						canMerkle = false
						break
					}
					impDigests[imp] = d
				}

				if canMerkle {
					merkleDigest = computePkgMerkleDigest(
						l.req.RepoRoot, importPath+"#testbase", dirInRepo, l.goos, l.goarch, l.buildTagsSig(),
						matchedEntries, directImports, impDigests,
					)
					if hit := l.crossRevPkgs.get(merkleDigest); hit != nil {
						l.revCache.registerPkg(hit.pkg)
						l.revCache.put(cacheKey, hit.pkg)
						if hit.fset != nil {
							l.revCache.registerPkgFset(hit.pkg, hit.fset)
						}
						l.testBasePkgs[importPath] = hit.pkg
						l.inRepoPkgs[importPath] = hit.pkg
						return hit.pkg, nil
					}
				}
			}
		}
	}

	var astFiles []*ast.File
	var basePkgName string
	var targetFset *token.FileSet

	if merkleDigest != "" {
		targetFset = token.NewFileSet()
		var paths []string
		var oids []string
		for _, e := range matchedEntries {
			paths = append(paths, e.path)
			oids = append(oids, e.oid)
		}
		contents := readRepoFilesBatch(l.req.Repo, l.rev(), paths, oids)
		for _, e := range matchedEntries {
			data := contents[e.path]
			if len(data) == 0 {
				continue
			}
			virtualPath := filepath.Join(l.req.RepoRoot, filepath.FromSlash(e.path))
			f, err := parser.ParseFile(targetFset, virtualPath, data, parser.SkipObjectResolution)
			if err != nil || f == nil {
				continue
			}
			if strings.HasSuffix(f.Name.Name, "_test") {
				continue
			}
			if basePkgName == "" {
				basePkgName = f.Name.Name
			} else if f.Name.Name != basePkgName {
				continue
			}
			astFiles = append(astFiles, f)
		}
	} else {
		targetFset = l.fset
		if useRevCache {
			targetFset = token.NewFileSet()
		}
		names, err := l.req.Repo.ListDir(l.rev(), dirInRepo)
		if err != nil || len(names) == 0 {
			return nil, fmt.Errorf("no files in %s", dirInRepo)
		}

		bctx, fc := l.makeBuildContextWithCache()
		for _, name := range names {
			if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			data, ok := l.matchAndReadInRepo(bctx, fc, dirInRepo, name)
			if !ok || len(data) == 0 {
				continue
			}
			filePath := name
			if dirInRepo != "" && dirInRepo != "." {
				filePath = path.Join(dirInRepo, name)
			}
			virtualPath := filepath.Join(l.req.RepoRoot, filepath.FromSlash(filePath))
			mode := parser.SkipObjectResolution
			f, err := parser.ParseFile(targetFset, virtualPath, data, mode)
			if err != nil || f == nil {
				continue
			}
			if strings.HasSuffix(f.Name.Name, "_test") {
				continue
			}
			if basePkgName == "" {
				basePkgName = f.Name.Name
			} else if f.Name.Name != basePkgName {
				continue
			}
			astFiles = append(astFiles, f)
		}
	}

	if len(astFiles) == 0 {
		stub := types.NewPackage(importPath, guessPkgName(importPath))
		stub.MarkComplete()
		if useRevCache && l.ctx.Err() == nil {
			l.revCache.put(cacheKey, stub)
		}
		l.testBasePkgs[importPath] = stub
		return stub, nil
	}

	conf := types.Config{
		IgnoreFuncBodies: true,
		FakeImportC:      true,
		Importer:         l,
		Error:            func(error) {},
	}

	pkg, err := conf.Check(importPath, targetFset, astFiles, nil)
	if pkg != nil {
		if l.ctx.Err() == nil {
			if merkleDigest != "" && l.crossRevPkgs != nil {
				l.crossRevPkgs.put(l.req.RepoRoot, merkleDigest, pkg, targetFset)
			}
			if useRevCache {
				l.revCache.put(cacheKey, pkg)
				l.revCache.registerPkg(pkg)
				l.revCache.registerPkgFset(pkg, targetFset)
			}
		}
		l.testBasePkgs[importPath] = pkg
		l.inRepoPkgs[importPath] = pkg
		return pkg, nil
	}
	stub := types.NewPackage(importPath, basePkgName)
	stub.MarkComplete()
	if useRevCache && l.ctx.Err() == nil {
		l.revCache.put(cacheKey, stub)
	}
	l.testBasePkgs[importPath] = stub
	return stub, err
}

func (l *lookupLoader) loadDiskTestBasePackage(importPath, absDir string) (*types.Package, error) {
	if l.testBasePkgs == nil {
		l.testBasePkgs = make(map[string]*types.Package)
	}
	if pkg, ok := l.testBasePkgs[importPath]; ok {
		return pkg, nil
	}

	useDiskCache := l.diskCache != nil
	var cacheKey diskCacheKey
	var doneCh chan struct{}
	if useDiskCache {
		cacheKey = l.makeCacheKey(importPath+"#testbase", absDir)
		for {
			l.diskCache.mu.Lock()
			if pkg, ok := l.diskCache.pkgs[cacheKey]; ok {
				l.diskCache.mu.Unlock()
				l.testBasePkgs[importPath] = pkg
				return pkg, nil
			}
			if ch, ok := l.diskCache.inflight[cacheKey]; ok {
				l.diskCache.mu.Unlock()
				select {
				case <-ch:
					continue
				case <-l.ctx.Done():
					return nil, l.ctx.Err()
				}
			}
			doneCh = make(chan struct{})
			l.diskCache.inflight[cacheKey] = doneCh
			l.diskCache.mu.Unlock()
			break
		}
		defer func() {
			l.diskCache.mu.Lock()
			delete(l.diskCache.inflight, cacheKey)
			l.diskCache.mu.Unlock()
			close(doneCh)
		}()
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		stub := types.NewPackage(importPath, guessPkgName(importPath))
		stub.MarkComplete()
		if useDiskCache && l.ctx.Err() == nil {
			l.diskCache.put(cacheKey, stub)
		}
		l.testBasePkgs[importPath] = stub
		return stub, nil
	}

	bctx, fc := l.makeBuildContextWithCache()
	targetFset := l.fset
	if useDiskCache {
		targetFset = l.diskCache.fset
	}
	var astFiles []*ast.File
	var basePkgName string

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		data, ok := l.matchAndReadDisk(bctx, fc, absDir, name)
		if !ok || len(data) == 0 {
			continue
		}
		filePath := filepath.Join(absDir, name)
		var f *ast.File
		var err error
		if useDiskCache {
			l.diskCache.fsetMu.Lock()
			f, err = parser.ParseFile(targetFset, filePath, data, parser.SkipObjectResolution)
			l.diskCache.fsetMu.Unlock()
		} else {
			f, err = parser.ParseFile(targetFset, filePath, data, parser.SkipObjectResolution)
		}
		if err != nil || f == nil {
			continue
		}
		if strings.HasSuffix(f.Name.Name, "_test") {
			continue
		}
		if basePkgName == "" {
			basePkgName = f.Name.Name
		} else if f.Name.Name != basePkgName {
			continue
		}
		astFiles = append(astFiles, f)
	}

	if len(astFiles) == 0 {
		stub := types.NewPackage(importPath, guessPkgName(importPath))
		stub.MarkComplete()
		if useDiskCache && l.ctx.Err() == nil {
			l.diskCache.put(cacheKey, stub)
		}
		l.testBasePkgs[importPath] = stub
		return stub, nil
	}

	var importer types.Importer
	if l.env.goroot != "" && strings.HasPrefix(absDir, filepath.Join(l.env.goroot, "src")+string(filepath.Separator)) {
		importer = newStdlibImporter(l)
	} else if l.env.gomodcache != "" && strings.HasPrefix(absDir, filepath.Clean(l.env.gomodcache)+string(filepath.Separator)) {
		importer = newModCacheImporter(l, absDir)
	} else {
		importer = l
	}

	conf := types.Config{
		IgnoreFuncBodies: true,
		FakeImportC:      true,
		Importer:         importer,
		Error:            func(error) {},
	}

	pkg, err := conf.Check(importPath, targetFset, astFiles, nil)
	if pkg != nil {
		if useDiskCache && l.ctx.Err() == nil {
			l.diskCache.put(cacheKey, pkg)
			l.diskCache.registerPkg(pkg)
			l.diskCache.registerPkgDir(pkg, absDir)
		}
		l.testBasePkgs[importPath] = pkg
		l.inRepoPkgs[importPath] = pkg
		return pkg, nil
	}
	stub := types.NewPackage(importPath, basePkgName)
	stub.MarkComplete()
	if useDiskCache && l.ctx.Err() == nil {
		l.diskCache.put(cacheKey, stub)
	}
	l.testBasePkgs[importPath] = stub
	return stub, err
}

func parseBuildConstraints(content []byte) constraint.Expr {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	var goBuildExpr constraint.Expr
	var plusBuildExprs []constraint.Expr
	inBlockComment := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if inBlockComment {
			if end := strings.Index(line, "*/"); end != -1 {
				inBlockComment = false
				line = strings.TrimSpace(line[end+2:])
				if line == "" {
					continue
				}
			} else {
				continue
			}
		}

		for strings.HasPrefix(line, "/*") {
			end := strings.Index(line, "*/")
			if end == -1 {
				inBlockComment = true
				break
			}
			line = strings.TrimSpace(line[end+2:])
		}
		if inBlockComment || line == "" {
			continue
		}

		if strings.HasPrefix(line, "//") {
			if constraint.IsGoBuild(line) {
				if expr, err := constraint.Parse(line); err == nil && expr != nil {
					if goBuildExpr == nil {
						goBuildExpr = expr
					} else {
						goBuildExpr = &constraint.AndExpr{X: goBuildExpr, Y: expr}
					}
				}
			} else if constraint.IsPlusBuild(line) {
				if expr, err := constraint.Parse(line); err == nil && expr != nil {
					plusBuildExprs = append(plusBuildExprs, expr)
				}
			}
			continue
		}

		// Non-blank, non-comment line -> reached code, stop scanning
		break
	}

	if goBuildExpr != nil {
		return goBuildExpr
	}
	if len(plusBuildExprs) == 0 {
		return nil
	}
	res := plusBuildExprs[0]
	for _, e := range plusBuildExprs[1:] {
		res = &constraint.AndExpr{X: res, Y: e}
	}
	return res
}

func collectTags(expr constraint.Expr) (allTags []string, posTags []string) {
	allSet := make(map[string]bool)
	posSet := make(map[string]bool)

	var walk func(e constraint.Expr, negated bool)
	walk = func(e constraint.Expr, negated bool) {
		if e == nil {
			return
		}
		switch node := e.(type) {
		case *constraint.TagExpr:
			allSet[node.Tag] = true
			if !negated {
				posSet[node.Tag] = true
			}
		case *constraint.NotExpr:
			walk(node.X, !negated)
		case *constraint.AndExpr:
			walk(node.X, negated)
			walk(node.Y, negated)
		case *constraint.OrExpr:
			walk(node.X, negated)
			walk(node.Y, negated)
		}
	}
	walk(expr, false)

	for t := range allSet {
		allTags = append(allTags, t)
	}
	for t := range posSet {
		posTags = append(posTags, t)
	}
	slices.Sort(allTags)
	slices.Sort(posTags)
	return allTags, posTags
}

var knownGOOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true,
	"js": true, "linux": true, "nacl": true, "netbsd": true,
	"openbsd": true, "plan9": true, "solaris": true, "wasip1": true,
	"windows": true, "zos": true,
}

var knownGOARCH = map[string]bool{
	"386": true, "amd64": true, "arm": true, "arm64": true,
	"loong64": true, "mips": true, "mips64": true, "mips64le": true,
	"mipsle": true, "ppc64": true, "ppc64le": true, "riscv64": true,
	"s390x": true, "sparc": true, "sparc64": true, "wasm": true,
}

func inferBuildConfig(clickedBytes []byte, defaultGOOS, defaultGOARCH string, defaultReleaseTags []string) (goos, goarch string, buildTags, releaseTags []string, custom bool) {
	expr := parseBuildConstraints(clickedBytes)
	if expr == nil {
		return defaultGOOS, defaultGOARCH, nil, defaultReleaseTags, false
	}

	eval := func(gos, garch string, tags []string, rels []string) bool {
		tagSet := make(map[string]bool, len(tags)+len(rels)+3)
		if gos != "" {
			tagSet[gos] = true
		}
		if garch != "" {
			tagSet[garch] = true
		}
		tagSet["cgo"] = true
		for _, rt := range rels {
			tagSet[rt] = true
		}
		for _, t := range tags {
			tagSet[t] = true
		}
		return expr.Eval(func(tag string) bool {
			return tagSet[tag]
		})
	}

	if eval(defaultGOOS, defaultGOARCH, nil, defaultReleaseTags) {
		return defaultGOOS, defaultGOARCH, nil, defaultReleaseTags, false
	}

	allTags, posTags := collectTags(expr)
	posSet := make(map[string]bool, len(posTags))
	for _, t := range posTags {
		posSet[t] = true
	}

	var customTags []string
	var candidateGOOS []string
	var candidateGOARCH []string

	candidateGOOS = append(candidateGOOS, defaultGOOS)
	candidateGOARCH = append(candidateGOARCH, defaultGOARCH)

	for _, tag := range allTags {
		if knownGOOS[tag] {
			if !slices.Contains(candidateGOOS, tag) {
				candidateGOOS = append(candidateGOOS, tag)
			}
		} else if knownGOARCH[tag] {
			if !slices.Contains(candidateGOARCH, tag) {
				candidateGOARCH = append(candidateGOARCH, tag)
			}
		} else if strings.HasPrefix(tag, "go1.") || tag == "cgo" {
			// standard release / cgo tag
		} else {
			customTags = append(customTags, tag)
		}
	}

	// Order custom tags: positive tags first, ignore last
	slices.SortFunc(customTags, func(a, b string) int {
		if a == "ignore" && b != "ignore" {
			return 1
		}
		if b == "ignore" && a != "ignore" {
			return -1
		}
		aPos := posSet[a]
		bPos := posSet[b]
		if aPos && !bPos {
			return -1
		}
		if !aPos && bPos {
			return 1
		}
		return strings.Compare(a, b)
	})

	// 1. Try enabling non-OS/non-arch/non-go1.X custom tags
	for _, tag := range customTags {
		if tag == "ignore" {
			continue
		}
		if eval(defaultGOOS, defaultGOARCH, []string{tag}, defaultReleaseTags) {
			return defaultGOOS, defaultGOARCH, []string{tag}, defaultReleaseTags, true
		}
	}

	// 2. Try switching GOOS / GOARCH
	for _, gos := range candidateGOOS {
		for _, garch := range candidateGOARCH {
			if gos == defaultGOOS && garch == defaultGOARCH {
				continue
			}
			if eval(gos, garch, nil, defaultReleaseTags) {
				return gos, garch, nil, defaultReleaseTags, true
			}
		}
	}

	// 3. Try truncating releaseTags to longest prefix satisfying expr
	for k := len(defaultReleaseTags) - 1; k >= 0; k-- {
		relPrefix := defaultReleaseTags[:k]
		if eval(defaultGOOS, defaultGOARCH, nil, relPrefix) {
			return defaultGOOS, defaultGOARCH, nil, relPrefix, true
		}
	}

	// 4. Try combining custom tag + switching GOOS/GOARCH
	for _, tag := range customTags {
		if tag == "ignore" {
			continue
		}
		for _, gos := range candidateGOOS {
			for _, garch := range candidateGOARCH {
				if eval(gos, garch, []string{tag}, defaultReleaseTags) {
					return gos, garch, []string{tag}, defaultReleaseTags, true
				}
			}
		}
	}

	// 5. Try combining custom tag + truncating releaseTags
	for _, tag := range customTags {
		if tag == "ignore" {
			continue
		}
		for k := len(defaultReleaseTags) - 1; k >= 0; k-- {
			relPrefix := defaultReleaseTags[:k]
			if eval(defaultGOOS, defaultGOARCH, []string{tag}, relPrefix) {
				return defaultGOOS, defaultGOARCH, []string{tag}, relPrefix, true
			}
		}
	}

	// 6. Try pairs of custom tags
	for i := 0; i < len(customTags); i++ {
		for j := i + 1; j < len(customTags); j++ {
			t1, t2 := customTags[i], customTags[j]
			if t1 == "ignore" || t2 == "ignore" {
				continue
			}
			if eval(defaultGOOS, defaultGOARCH, []string{t1, t2}, defaultReleaseTags) {
				return defaultGOOS, defaultGOARCH, []string{t1, t2}, defaultReleaseTags, true
			}
		}
	}

	// 7. If "ignore" is present and nothing else worked
	if slices.Contains(customTags, "ignore") {
		if eval(defaultGOOS, defaultGOARCH, []string{"ignore"}, defaultReleaseTags) {
			return defaultGOOS, defaultGOARCH, []string{"ignore"}, defaultReleaseTags, true
		}
	}

	return defaultGOOS, defaultGOARCH, customTags, defaultReleaseTags, true
}

// stripGoFuncBodies strips function and method bodies from Go source code, replacing
// them with empty bodies preserving all newline characters. It is intended strictly
// for use when types.Config.IgnoreFuncBodies is true.
func stripGoFuncBodies(src []byte) []byte {
	if !bytes.Contains(src, []byte("func")) {
		return src
	}
	if bytes.Contains(src, []byte("//line")) || bytes.Contains(src, []byte("/*line")) {
		return src
	}

	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	hadScanErr := false
	var s scanner.Scanner
	s.Init(file, src, func(token.Position, string) {
		hadScanErr = true
	}, 0)

	var out []byte
	lastCopied := 0
	braceDepth, parenDepth, bracketDepth := 0, 0, 0
	atTopDeclStart := true
	inTopFuncSig := false
	prevTok := token.ILLEGAL

	for {
		pos, tok, _ := s.Scan()
		if hadScanErr || tok == token.ILLEGAL {
			return src
		}
		if tok == token.EOF {
			break
		}

		switch tok {
		case token.SEMICOLON:
			if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				inTopFuncSig = false
				atTopDeclStart = true
			}
			prevTok = tok

		case token.FUNC:
			if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 && atTopDeclStart {
				inTopFuncSig = true
			}
			atTopDeclStart = false
			prevTok = tok

		case token.PACKAGE, token.IMPORT, token.CONST, token.VAR, token.TYPE:
			if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				inTopFuncSig = false
			}
			atTopDeclStart = false
			prevTok = tok

		case token.LPAREN:
			parenDepth++
			atTopDeclStart = false
			prevTok = tok

		case token.RPAREN:
			if parenDepth > 0 {
				parenDepth--
			}
			atTopDeclStart = false
			prevTok = tok

		case token.LBRACK:
			bracketDepth++
			atTopDeclStart = false
			prevTok = tok

		case token.RBRACK:
			if bracketDepth > 0 {
				bracketDepth--
			}
			atTopDeclStart = false
			prevTok = tok

		case token.LBRACE:
			if inTopFuncSig && braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 &&
				prevTok != token.STRUCT && prevTok != token.INTERFACE {
				openOff := file.Offset(pos)
				bodyDepth := 1
				var closeOff int
				for bodyDepth > 0 {
					bPos, bTok, _ := s.Scan()
					if hadScanErr || bTok == token.ILLEGAL || bTok == token.EOF {
						return src
					}
					switch bTok {
					case token.LBRACE:
						bodyDepth++
					case token.RBRACE:
						bodyDepth--
						if bodyDepth == 0 {
							closeOff = file.Offset(bPos)
						}
					}
				}
				if closeOff > openOff+1 {
					if out == nil {
						out = make([]byte, 0, len(src)/4+128)
					}
					nlCount := bytes.Count(src[openOff:closeOff+1], []byte{'\n'})
					out = append(out, src[lastCopied:openOff+1]...)
					for k := 0; k < nlCount; k++ {
						out = append(out, '\n')
					}
					out = append(out, '}')
					lastCopied = closeOff + 1
				}
				inTopFuncSig = false
				prevTok = token.RBRACE
				continue
			}
			braceDepth++
			atTopDeclStart = false
			prevTok = tok

		case token.RBRACE:
			if braceDepth > 0 {
				braceDepth--
			}
			atTopDeclStart = false
			prevTok = tok

		default:
			atTopDeclStart = false
			prevTok = tok
		}
	}

	if hadScanErr || lastCopied == 0 {
		return src
	}
	if lastCopied < len(src) {
		out = append(out, src[lastCopied:]...)
	}
	return out
}

func parseGoFile(fset *token.FileSet, filename string, src []byte, mode parser.Mode, ignoreFuncBodies bool) (*ast.File, error) {
	if !ignoreFuncBodies {
		return parser.ParseFile(fset, filename, src, mode)
	}
	stripped := stripGoFuncBodies(src)
	f, err := parser.ParseFile(fset, filename, stripped, mode)
	if err != nil && len(stripped) != len(src) {
		if f != nil {
			if tf := fset.File(f.FileStart); tf != nil {
				fset.RemoveFile(tf)
			} else if tf := fset.File(f.Package); tf != nil {
				fset.RemoveFile(tf)
			}
		}
		return parser.ParseFile(fset, filename, src, mode)
	}
	return f, err
}
