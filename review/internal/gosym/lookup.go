package gosym

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"html"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/vcs"
)

// LookupRequest describes the symbol lookup coordinate and context.
type LookupRequest struct {
	Repo          vcs.Repo
	RepoRoot      string
	BaseRev       string
	TargetRev     string
	Side          string // "L" or "R"
	FilePath      string // slash path relative to RepoRoot
	Line          int    // 1-based line number
	UTF16Col      int    // 0-based UTF-16 code unit offset
	PkgImportPath string // optional: for chained lookup of external/stdlib package
	PkgFile       string // optional: filename e.g. "header.go"
	Site          bool   // optional: resolve the enclosing symbol at reference site (Line)
}

// ExternalFileRequest specifies a request to read an external source file
// (e.g. from GOROOT, GOMODCACHE, GOPATH, or repo).
type ExternalFileRequest struct {
	PkgImportPath string
	PkgFile       string
	OriginPath    string // optional: in-repo file path that referenced this file
	Repo          vcs.Repo
	Rev           string
}

// SnippetLine holds a formatted source line for the definition snippet.
type SnippetLine struct {
	Line   int    `json:"line"`
	HTML   string `json:"html"`
	Active bool   `json:"active"`
}

// Result describes the resolved Go symbol, signature, docs, and definition.
type Result struct {
	Found            bool          `json:"found"`
	SymbolName       string        `json:"symbol_name,omitempty"`
	Signature        string        `json:"signature,omitempty"`
	SignatureHTML    string        `json:"signature_html,omitempty"`
	PkgGoDevURL      string        `json:"pkg_go_dev_url,omitempty"`
	InRepo           bool          `json:"in_repo"`
	DefPath          string        `json:"def_path,omitempty"`
	DisplayPath      string        `json:"display_path,omitempty"`
	DefLine          int           `json:"def_line,omitempty"`
	DefPkg           string        `json:"def_pkg,omitempty"`
	DefFile          string        `json:"def_file,omitempty"`
	SymLine          int           `json:"sym_line,omitempty"`
	SymCol           int           `json:"sym_col,omitempty"`
	Snippet          []SnippetLine `json:"snippet,omitempty"`
	SnippetTruncated bool          `json:"snippet_truncated,omitempty"`
	CanRefs          bool          `json:"can_refs,omitempty"`
	CanImpls         bool          `json:"can_impls,omitempty"`
	ImplsDirection   string        `json:"impls_direction,omitempty"`
}

type revCacheKey struct {
	repoRoot string
	rev      string
}

// Analyzer orchestrates hermetic Go symbol lookups.
type Analyzer struct {
	diskCache       *diskCache
	blobCache       *blobIndexCache
	crossRevPkgs    *crossRevPkgCache
	crossRevChecked *crossRevCheckedLRU
	mu              sync.Mutex
	revOrder        []revCacheKey
	revCaches       map[revCacheKey]*diskCache

	warmMu       sync.Mutex
	warmStarted  bool
	warmCancel   context.CancelFunc
	warmWG       sync.WaitGroup
	warmCh       chan WarmRequest
	warmVCSCache *vcs.Cache
	ownsVCSCache bool
	idleCheck    func() bool
	warmedDirs   map[string]struct{}
}

// NewAnalyzer creates a new hermetic symbol Analyzer.
func NewAnalyzer() *Analyzer {
	return &Analyzer{
		diskCache:       sharedDiskCache,
		blobCache:       sharedBlobIndexCache,
		crossRevPkgs:    sharedCrossRevPkgCache,
		crossRevChecked: sharedCrossRevCheckedLRU,
		revCaches:       make(map[revCacheKey]*diskCache),
		idleCheck:       func() bool { return true },
	}
}

func (a *Analyzer) getRevCache(repoRoot, rev string) *diskCache {
	if !vcs.IsImmutableHexRev(rev) {
		return nil
	}
	cleanRoot := filepath.Clean(repoRoot)
	key := revCacheKey{repoRoot: cleanRoot, rev: rev}

	a.mu.Lock()
	defer a.mu.Unlock()

	if rc, ok := a.revCaches[key]; ok {
		for i, k := range a.revOrder {
			if k == key {
				a.revOrder = append(a.revOrder[:i], a.revOrder[i+1:]...)
				a.revOrder = append(a.revOrder, key)
				break
			}
		}
		return rc
	}

	for len(a.revCaches) >= 8 && len(a.revOrder) > 0 {
		oldest := a.revOrder[0]
		a.revOrder = a.revOrder[1:]
		delete(a.revCaches, oldest)
	}

	rc := newDiskCache()
	rc.blobCache = a.blobCache
	rc.prevIndexFunc = func() (string, *repoFileIndex) {
		a.mu.Lock()
		defer a.mu.Unlock()
		for i := len(a.revOrder) - 1; i >= 0; i-- {
			k := a.revOrder[i]
			if k.repoRoot == cleanRoot && k.rev != rev {
				if prevRC := a.revCaches[k]; prevRC != nil {
					prevRC.fileIndexMu.Lock()
					idx := prevRC.fileIndex
					prevRC.fileIndexMu.Unlock()
					if idx != nil {
						return k.rev, idx
					}
				}
			}
		}
		return "", nil
	}
	a.revCaches[key] = rc
	a.revOrder = append(a.revOrder, key)
	return rc
}

// Evict evicts any caches associated with repoRoot.
func (a *Analyzer) Evict(repoRoot string) {
	cleanRoot := filepath.Clean(repoRoot)

	if a.crossRevPkgs != nil {
		a.crossRevPkgs.evictRepo(cleanRoot)
	}
	if a.crossRevChecked != nil {
		a.crossRevChecked.evictRepo(cleanRoot)
	}

	a.warmMu.Lock()
	vc := a.warmVCSCache
	ownsVC := a.ownsVCSCache
	a.warmMu.Unlock()
	if vc != nil && ownsVC {
		vc.Evict(cleanRoot)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	newOrder := a.revOrder[:0]
	for _, k := range a.revOrder {
		if k.repoRoot == cleanRoot || strings.HasPrefix(k.repoRoot, cleanRoot+string(filepath.Separator)) {
			delete(a.revCaches, k)
		} else {
			newOrder = append(newOrder, k)
		}
	}
	a.revOrder = newOrder
}

// Close releases any resources held by Analyzer.
func (a *Analyzer) Close() {
	a.warmMu.Lock()
	a.warmStarted = false
	cancel := a.warmCancel
	vc := a.warmVCSCache
	ownsVC := a.ownsVCSCache
	a.warmCancel = nil
	a.warmVCSCache = nil
	a.ownsVCSCache = false
	a.warmMu.Unlock()

	if cancel != nil {
		cancel()
		a.warmWG.Wait()
	}
	if vc != nil && ownsVC {
		vc.Close()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.revCaches = make(map[revCacheKey]*diskCache)
	a.revOrder = nil
}

type resolvedTarget struct {
	rev                 string
	cleanPath           string
	isChained           bool
	isDisk              bool
	isPkgClause         bool
	isInitFunc          bool
	dir                 string // dirInRepo or abs disk dir
	rootImportPath      string
	mod                 *moduleInfo
	loader              *lookupLoader
	clickedAST          *ast.File
	pkgASTFiles         []*ast.File
	rootPkg             *types.Package
	info                *types.Info
	clickedIdent        *ast.Ident
	enclosingSelector   *ast.SelectorExpr
	typeSwitchIdent     *ast.Ident
	enclosingTypeSwitch *ast.TypeSwitchStmt
	obj                 types.Object
	// If obj == nil but stubbed selector fallback:
	stubbedPkgPath string
	stubbedPkgName string
	stubbedSymName string
	// Site lookup coordinates
	siteLines []string
	siteLine  int
	symLine   int
	symCol    int
}

// Lookup resolves the Go symbol under the cursor in req.
func (a *Analyzer) Lookup(ctx context.Context, req LookupRequest) (*Result, error) {
	target, err := a.resolveTarget(ctx, req)
	if err != nil || target == nil {
		return &Result{Found: false}, err
	}

	if target.obj == nil {
		if target.stubbedPkgPath != "" {
			return &Result{
				Found:         true,
				SymbolName:    target.stubbedSymName,
				Signature:     target.stubbedPkgName + "." + target.stubbedSymName,
				SignatureHTML: fmt.Sprintf(`<span class="tok-id">%s</span>.<span class="tok-id">%s</span>`, html.EscapeString(target.stubbedPkgName), html.EscapeString(target.stubbedSymName)),
				PkgGoDevURL:   buildPkgGoDevURLForPath(target.stubbedPkgPath, target.stubbedSymName, target.mod, target.loader.env, false),
				CanRefs:       false,
			}, nil
		}
		return &Result{Found: false}, nil
	}

	res, err := a.buildResult(req, target)
	if err != nil {
		return nil, err
	}
	if res != nil && target.obj != nil {
		if (target.obj.Pkg() != nil || target.isPkgClause) && !target.isInitFunc {
			res.CanRefs = true
		}
		res.CanImpls, res.ImplsDirection = canCheckImplementations(target.obj)
	}

	if req.Site && res != nil && res.Found {
		res.DefLine = target.siteLine
		res.SymLine = target.symLine
		res.SymCol = target.symCol

		if !target.isDisk {
			siteRelPath := target.cleanPath
			if siteRelPath == "" && req.PkgFile != "" {
				siteRelPath = path.Join(target.dir, filepath.Base(req.PkgFile))
			}
			res.InRepo = true
			res.DefPath = siteRelPath
			res.DisplayPath = res.DefPath
			res.DefPkg = ""
			res.DefFile = ""
		} else {
			res.InRepo = false
			res.DefPath = ""
			res.DefPkg = req.PkgImportPath
			res.DefFile = filepath.Base(req.PkgFile)
			res.DisplayPath = computeDisplayPath(filepath.Join(target.dir, res.DefFile), target.loader.env)
		}

		res.Snippet, res.SnippetTruncated = buildSnippet(target.siteLines, target.siteLine)
	}

	return res, nil
}

func canCheckImplementations(obj types.Object) (bool, string) {
	if obj == nil {
		return false, ""
	}
	// 1. Built-in error type and error.Error method
	if obj == types.Universe.Lookup("error") {
		return true, "implementations"
	}
	if obj.Pkg() == nil {
		if fn, ok := obj.(*types.Func); ok && fn.Name() == "Error" {
			return true, "implementations"
		}
		return false, ""
	}

	// 2. TypeName
	if tn, ok := obj.(*types.TypeName); ok {
		t := types.Unalias(tn.Type())
		if iface, ok := t.Underlying().(*types.Interface); ok {
			if iface.NumMethods() > 0 && iface.IsMethodSet() {
				return true, "implementations"
			}
			return false, ""
		}
		if named, ok := t.(*types.Named); ok {
			if concreteTypeHasMethods(named) {
				return true, "interfaces"
			}
			return false, ""
		}
		return false, ""
	}

	// 3. Method (*types.Func with non-nil receiver)
	if fn, ok := obj.(*types.Func); ok {
		sig, ok := fn.Type().(*types.Signature)
		if !ok || sig.Recv() == nil {
			return false, ""
		}
		recvType := sig.Recv().Type()
		for {
			if pt, ok := recvType.(*types.Pointer); ok {
				recvType = pt.Elem()
				continue
			}
			break
		}
		recvType = types.Unalias(recvType)
		if iface, ok := recvType.Underlying().(*types.Interface); ok {
			if iface.NumMethods() > 0 && iface.IsMethodSet() {
				return true, "implementations"
			}
			return false, ""
		}
		if _, ok := recvType.(*types.Named); ok {
			return true, "interfaces"
		}
		return false, ""
	}

	return false, ""
}

func instantiateIfGeneric(t types.Type) types.Type {
	if ptr, ok := t.(*types.Pointer); ok {
		elem := instantiateIfGeneric(ptr.Elem())
		if elem != ptr.Elem() {
			return types.NewPointer(elem)
		}
		return ptr
	}
	named, ok := t.(*types.Named)
	if !ok || named.TypeParams() == nil || named.TypeParams().Len() == 0 || (named.TypeArgs() != nil && named.TypeArgs().Len() > 0) {
		return t
	}
	targs := make([]types.Type, named.TypeParams().Len())
	for i := range targs {
		targs[i] = types.NewInterfaceType(nil, nil)
	}
	inst, err := types.Instantiate(nil, named, targs, false)
	if err != nil || inst == nil {
		return t
	}
	return inst
}

func safeMethodSet(t types.Type) (ms *types.MethodSet) {
	if t == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			ms = nil
		}
	}()
	t = instantiateIfGeneric(t)
	return types.NewMethodSet(t)
}

func concreteTypeHasMethods(named *types.Named) bool {
	if named == nil {
		return false
	}
	if named.NumMethods() > 0 {
		return true
	}
	mset := safeMethodSet(types.NewPointer(named))
	return mset != nil && mset.Len() > 0
}

func isStringLiteralAtByteIndex(line string, byteIdx int) bool {
	if byteIdx < 0 || byteIdx >= len(line) {
		return false
	}
	inQuote := byte(0)
	quoteStart := -1
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inQuote != 0 {
			if c == '\\' && inQuote != '`' {
				i++
				continue
			}
			if c == inQuote {
				if (inQuote == '"' || inQuote == '`') && byteIdx >= quoteStart && byteIdx <= i {
					return true
				}
				inQuote = 0
				quoteStart = -1
			}
			continue
		}
		if c == '/' && i+1 < len(line) && line[i+1] == '/' {
			break
		}
		if c == '"' || c == '`' || c == '\'' {
			inQuote = c
			quoteStart = i
			continue
		}
	}
	if (inQuote == '"' || inQuote == '`') && byteIdx >= quoteStart {
		return true
	}
	return false
}

func (a *Analyzer) resolveTarget(ctx context.Context, req LookupRequest) (*resolvedTarget, error) {
	rev := req.TargetRev
	if req.Side == "L" && req.BaseRev != "" {
		rev = req.BaseRev
	}
	if rev == "" {
		rev = "working"
	}

	// 1. Chained lookup: looking up a symbol directly inside an imported package file
	if req.PkgImportPath != "" && req.PkgFile != "" {
		return a.resolveTargetChained(ctx, req, rev)
	}

	cleanPath := path.Clean(filepath.ToSlash(req.FilePath))
	if !strings.HasSuffix(cleanPath, ".go") || strings.HasPrefix(cleanPath, "..") {
		return nil, nil
	}

	clickedBytes, err := req.Repo.Content(rev, cleanPath)
	if err != nil || len(clickedBytes) == 0 {
		return nil, nil
	}

	lines := diff.SplitLines(clickedBytes)
	if req.Line < 1 || req.Line > len(lines) {
		return nil, nil
	}

	lineStr := lines[req.Line-1]
	byteIdx := 0
	if !req.Site {
		byteIdx = utf16ColToByteIndex(lineStr, req.UTF16Col)

		// Verify cursor is on an identifier token (or within a string literal)
		if !isStringLiteralAtByteIndex(lineStr, byteIdx) {
			tokSym := extractSymbolAtByteIndex(lineStr, byteIdx)
			if tokSym == "" || isGoKeyword(tokSym) {
				if !hasPotentialDocLinkAtByteIndex(lineStr, byteIdx) {
					return nil, nil
				}
			}
		}
	}

	revCache := a.getRevCache(req.RepoRoot, rev)
	if revCache != nil && req.Repo != nil && vcs.IsImmutableHexRev(rev) {
		if revCache.peekRepoFileIndex() == nil && revCache.hasPrevRepoFileIndex() {
			_, _ = revCache.getRepoFileIndex(ctx, req.Repo, rev)
		}
	}
	mod := findModuleInfo(req.Repo, rev, cleanPath, revCache)
	loader := newLookupLoader(ctx, req, mod, a.diskCache, revCache)
	if a.crossRevPkgs != nil {
		loader.crossRevPkgs = a.crossRevPkgs
	}
	if a.crossRevChecked != nil {
		loader.crossRevChecked = a.crossRevChecked
	}
	if goos, goarch, bTags, relTags, custom := inferBuildConfig(clickedBytes, loader.goos, loader.goarch, loader.releaseTags); custom {
		loader.goos = goos
		loader.goarch = goarch
		loader.buildTags = bTags
		loader.releaseTags = relTags
		loader.customBuild = true
	}

	// Identify package directory in repo
	dirInRepo := path.Dir(cleanPath)
	if dirInRepo == "." {
		dirInRepo = ""
	}

	isTestFile := strings.HasSuffix(cleanPath, "_test.go")
	var clickedPkgName string
	if _, _, pName := scanGoHeaderImports(clickedBytes); pName != "" {
		clickedPkgName = pName
	} else if pAST, err := parser.ParseFile(token.NewFileSet(), cleanPath, clickedBytes, parser.PackageClauseOnly); err == nil && pAST != nil && pAST.Name != nil {
		clickedPkgName = pAST.Name.Name
	}
	if clickedPkgName == "" {
		return nil, nil
	}

	isExternalTest := isTestFile && strings.HasSuffix(clickedPkgName, "_test")

	if loader.revCache != nil {
		if entry := loader.loadCheckedPackage(dirInRepo, clickedPkgName, isTestFile); entry != nil {
			if cf, ok := entry.fileByPath[cleanPath]; ok && cf.ast != nil {
				loader.fset = entry.fset
				rootImportPath := cleanPathToImport(mod, dirInRepo)
				if isExternalTest {
					rootImportPath += "_test"
				}
				cfLines := cf.lines
				if len(cfLines) == 0 {
					cfLines = lines
				}
				if req.Site {
					return a.findTargetAtSite(rev, cleanPath, false, false, dirInRepo, rootImportPath, mod, loader, cf.ast, entryASTFiles(entry), entry.pkg, entry.info, cfLines, req.Line)
				}
				return a.findTargetInAST(rev, cleanPath, false, false, dirInRepo, rootImportPath, mod, loader, cf.ast, entryASTFiles(entry), entry.pkg, entry.info, cfLines, req.Line, byteIdx)
			}
		}
	}

	// Parse clicked file into loader's fset only if checked package wasn't cached
	virtualPath := filepath.Join(req.RepoRoot, filepath.FromSlash(cleanPath))
	clickedAST, err := parser.ParseFile(loader.fset, virtualPath, clickedBytes, parser.SkipObjectResolution)
	if err != nil || clickedAST == nil {
		return nil, nil
	}

	// Collect matching AST files in the same directory
	var pkgASTFiles []*ast.File
	pkgASTFiles = append(pkgASTFiles, clickedAST)

	names, _ := req.Repo.ListDir(rev, dirInRepo)
	bctx, fc := loader.makeBuildContextWithCache()

	for _, name := range names {
		if name == path.Base(cleanPath) || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		nameIsTest := strings.HasSuffix(name, "_test.go")
		if !isTestFile && nameIsTest {
			continue
		}

		sBytes, ok := loader.matchAndReadInRepo(bctx, fc, dirInRepo, name)
		if !ok || len(sBytes) == 0 {
			continue
		}
		siblingPath := name
		if dirInRepo != "" {
			siblingPath = path.Join(dirInRepo, name)
		}
		sVirt := filepath.Join(req.RepoRoot, filepath.FromSlash(siblingPath))
		sAST, err := parser.ParseFile(loader.fset, sVirt, sBytes, parser.SkipObjectResolution)
		if err != nil || sAST == nil {
			continue
		}
		if sAST.Name.Name != clickedPkgName {
			continue
		}
		pkgASTFiles = append(pkgASTFiles, sAST)
	}

	// If this is an external test package (e.g. package foo_test),
	// make the base package (foo) in that directory importable as its import path
	if isExternalTest {
		basePkgImport := cleanPathToImport(mod, dirInRepo)
		if basePkg, err := loader.loadInRepoTestBasePackage(basePkgImport, dirInRepo); err == nil && basePkg != nil {
			loader.inRepoPkgs[basePkgImport] = basePkg
		}
	}

	rootImportPath := cleanPathToImport(mod, dirInRepo)
	if isExternalTest {
		rootImportPath += "_test"
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
		Importer:         loader,
		Error:            func(error) {},
	}

	rootPkg, _ := conf.Check(rootImportPath, loader.fset, pkgASTFiles, info)

	if req.Site {
		return a.findTargetAtSite(rev, cleanPath, false, false, dirInRepo, rootImportPath, mod, loader, clickedAST, pkgASTFiles, rootPkg, info, lines, req.Line)
	}
	return a.findTargetInAST(rev, cleanPath, false, false, dirInRepo, rootImportPath, mod, loader, clickedAST, pkgASTFiles, rootPkg, info, lines, req.Line, byteIdx)
}

// ResolveExternalFile locates and returns the display path and contents of an external Go source file.
func (a *Analyzer) ResolveExternalFile(ctx context.Context, req ExternalFileRequest) (displayPath string, content []byte, ok bool) {
	pkgFile := filepath.Base(req.PkgFile)
	if !strings.HasSuffix(pkgFile, ".go") || pkgFile != req.PkgFile || strings.Contains(pkgFile, "..") || strings.ContainsAny(pkgFile, `/\`) {
		return "", nil, false
	}
	if req.PkgImportPath == "" {
		return "", nil, false
	}

	var repoRoot string
	if req.Repo != nil {
		repoRoot = req.Repo.Root()
	}
	revCache := a.getRevCache(repoRoot, req.Rev)
	mod := findModuleInfo(req.Repo, req.Rev, req.OriginPath, revCache)
	lReq := LookupRequest{
		Repo:          req.Repo,
		RepoRoot:      repoRoot,
		TargetRev:     req.Rev,
		PkgImportPath: req.PkgImportPath,
		PkgFile:       pkgFile,
	}
	loader := newLookupLoader(ctx, lReq, mod, a.diskCache, revCache)
	if a.crossRevPkgs != nil {
		loader.crossRevPkgs = a.crossRevPkgs
	}
	if a.crossRevChecked != nil {
		loader.crossRevChecked = a.crossRevChecked
	}

	dir, isDisk, err := loader.resolvePackageDir(req.PkgImportPath)
	if err != nil {
		return "", nil, false
	}

	if isDisk {
		targetPath := filepath.Join(dir, pkgFile)
		data, err := os.ReadFile(targetPath)
		if err != nil || len(data) == 0 {
			return "", nil, false
		}
		return computeDisplayPath(targetPath, loader.env), data, true
	}

	if req.Repo == nil || req.Rev == "" {
		return "", nil, false
	}
	inRepoPath := path.Join(dir, pkgFile)
	data, err := req.Repo.Content(req.Rev, inRepoPath)
	if err != nil || len(data) == 0 {
		return "", nil, false
	}
	return inRepoPath, data, true
}

type builtinFileCache struct {
	fset  *token.FileSet
	ast   *ast.File
	lines []string
}

var (
	builtinCacheMu sync.Mutex
	builtinCache   = make(map[string]*builtinFileCache)
)

func getParsedBuiltin(goroot string) *builtinFileCache {
	if goroot == "" {
		return nil
	}
	builtinCacheMu.Lock()
	defer builtinCacheMu.Unlock()
	if c, ok := builtinCache[goroot]; ok {
		return c
	}
	builtinPath := filepath.Join(goroot, "src", "builtin", "builtin.go")
	bBytes, err := os.ReadFile(builtinPath)
	if err != nil || len(bBytes) == 0 {
		return nil
	}
	bFset := token.NewFileSet()
	bAST, err := parser.ParseFile(bFset, builtinPath, bBytes, parser.SkipObjectResolution)
	if err != nil || bAST == nil {
		return nil
	}
	bLines := diff.SplitLines(bBytes)
	c := &builtinFileCache{
		fset:  bFset,
		ast:   bAST,
		lines: bLines,
	}
	builtinCache[goroot] = c
	return c
}

func (a *Analyzer) resolveTargetChained(ctx context.Context, req LookupRequest, rev string) (*resolvedTarget, error) {
	pkgFile := filepath.Base(req.PkgFile)
	if !strings.HasSuffix(pkgFile, ".go") || pkgFile != req.PkgFile || strings.Contains(pkgFile, "..") {
		return nil, nil
	}

	revCache := a.getRevCache(req.RepoRoot, rev)
	mod := findModuleInfo(req.Repo, rev, req.FilePath, revCache)
	loader := newLookupLoader(ctx, req, mod, a.diskCache, revCache)
	if a.crossRevPkgs != nil {
		loader.crossRevPkgs = a.crossRevPkgs
	}
	if a.crossRevChecked != nil {
		loader.crossRevChecked = a.crossRevChecked
	}

	if req.PkgImportPath == "builtin" && pkgFile == "builtin.go" {
		builtinPath := ""
		var lines []string
		if loader.env.goroot != "" {
			builtinPath = filepath.Join(loader.env.goroot, "src", "builtin", "builtin.go")
			if bc := getParsedBuiltin(loader.env.goroot); bc != nil {
				lines = bc.lines
			}
		}
		if len(lines) == 0 && builtinPath != "" && fileExists(builtinPath) {
			if data, err := os.ReadFile(builtinPath); err == nil {
				lines = diff.SplitLines(data)
			}
		}

		errObj := types.Universe.Lookup("error")
		var targetObj types.Object = errObj
		if errObj != nil {
			if iface, ok := errObj.Type().Underlying().(*types.Interface); ok {
				for i := 0; i < iface.NumMethods(); i++ {
					m := iface.Method(i)
					if m.Name() == "Error" {
						if req.Line > 0 && req.Line <= len(lines) {
							lineStr := lines[req.Line-1]
							if strings.Contains(lineStr, "Error()") {
								targetObj = m
							}
						}
						break
					}
				}
			}
		}

		dir := ""
		if builtinPath != "" {
			dir = filepath.Dir(builtinPath)
		}
		return &resolvedTarget{
			rev:            rev,
			isChained:      true,
			isDisk:         true,
			dir:            dir,
			rootImportPath: "builtin",
			mod:            mod,
			loader:         loader,
			obj:            targetObj,
			siteLines:      lines,
			siteLine:       req.Line,
			symLine:        req.Line,
			symCol:         req.UTF16Col,
		}, nil
	}

	basePkgImport := strings.TrimSuffix(req.PkgImportPath, "_test")
	dir, isDisk, err := loader.resolvePackageDir(basePkgImport)
	if err != nil {
		return nil, nil
	}

	if isDisk && loader.env.gomodcache != "" && strings.HasPrefix(dir, filepath.Clean(loader.env.gomodcache)+string(filepath.Separator)) {
		if modInfo := loader.diskCache.findModCacheModuleInfo(loader.env.gomodcache, dir); modInfo != nil {
			loader.mod = loader.mod.clone()
			for reqMod, ver := range modInfo.requires {
				if _, ok := loader.mod.requires[reqMod]; !ok {
					loader.mod.requires[reqMod] = ver
				}
			}
			for repMod, rep := range modInfo.replaces {
				if _, ok := loader.mod.replaces[repMod]; !ok {
					loader.mod.replaces[repMod] = rep
				}
			}
		}
	}

	var clickedBytes []byte
	var clickedAST *ast.File
	var pkgASTFiles []*ast.File

	if isDisk {
		targetPath := filepath.Join(dir, pkgFile)
		clickedBytes, err = os.ReadFile(targetPath)
		if err != nil || len(clickedBytes) == 0 {
			return nil, nil
		}
		if goos, goarch, bTags, relTags, custom := inferBuildConfig(clickedBytes, loader.goos, loader.goarch, loader.releaseTags); custom {
			loader.goos = goos
			loader.goarch = goarch
			loader.buildTags = bTags
			loader.releaseTags = relTags
			loader.customBuild = true
		}
		var clickedPkgName string
		if _, _, pName := scanGoHeaderImports(clickedBytes); pName != "" {
			clickedPkgName = pName
		} else if pAST, err := parser.ParseFile(token.NewFileSet(), targetPath, clickedBytes, parser.PackageClauseOnly); err == nil && pAST != nil && pAST.Name != nil {
			clickedPkgName = pAST.Name.Name
		}
		if clickedPkgName == "" {
			return nil, nil
		}

		isTestTarget := strings.HasSuffix(pkgFile, "_test.go")
		checkImportPath := req.PkgImportPath
		if isTestTarget && strings.HasSuffix(clickedPkgName, "_test") && !strings.HasSuffix(checkImportPath, "_test") {
			checkImportPath += "_test"
		}

		if entry := loader.loadDiskCheckedPackage(checkImportPath, dir, clickedPkgName, isTestTarget); entry != nil {
			if cf, ok := entry.fileByPath[targetPath]; ok && cf.ast != nil {
				loader.fset = entry.fset
				cfLines := cf.lines
				if len(cfLines) == 0 {
					cfLines = diff.SplitLines(clickedBytes)
				}
				if req.Site {
					return a.findTargetAtSite(rev, "", true, isDisk, dir, checkImportPath, mod, loader, cf.ast, entryASTFiles(entry), entry.pkg, entry.info, cfLines, req.Line)
				}
				if req.Line < 1 || req.Line > len(cfLines) {
					return nil, nil
				}
				lineStr := cfLines[req.Line-1]
				byteIdx := utf16ColToByteIndex(lineStr, req.UTF16Col)
				return a.findTargetInAST(rev, "", true, isDisk, dir, checkImportPath, mod, loader, cf.ast, entryASTFiles(entry), entry.pkg, entry.info, cfLines, req.Line, byteIdx)
			}
		}

		clickedAST, err = parser.ParseFile(loader.fset, targetPath, clickedBytes, parser.SkipObjectResolution)
		if err != nil || clickedAST == nil {
			return nil, nil
		}

		pkgASTFiles = append(pkgASTFiles, clickedAST)

		entries, _ := os.ReadDir(dir)
		bctx, fc := loader.makeBuildContextWithCache()
		isTestTarget = strings.HasSuffix(pkgFile, "_test.go")
		for _, e := range entries {
			name := e.Name()
			if name == pkgFile || !strings.HasSuffix(name, ".go") || (!isTestTarget && strings.HasSuffix(name, "_test.go")) || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			data, ok := loader.matchAndReadDisk(bctx, fc, dir, name)
			if !ok || len(data) == 0 {
				continue
			}
			p := filepath.Join(dir, name)
			sAST, err := parser.ParseFile(loader.fset, p, data, parser.SkipObjectResolution)
			if err == nil && sAST != nil && sAST.Name.Name == clickedAST.Name.Name {
				pkgASTFiles = append(pkgASTFiles, sAST)
			}
		}
	} else {
		targetPath := path.Join(dir, pkgFile)
		clickedBytes, err = req.Repo.Content(rev, targetPath)
		if err != nil || len(clickedBytes) == 0 {
			return nil, nil
		}
		if goos, goarch, bTags, relTags, custom := inferBuildConfig(clickedBytes, loader.goos, loader.goarch, loader.releaseTags); custom {
			loader.goos = goos
			loader.goarch = goarch
			loader.buildTags = bTags
			loader.releaseTags = relTags
			loader.customBuild = true
		}
		var clickedPkgName string
		if _, _, pName := scanGoHeaderImports(clickedBytes); pName != "" {
			clickedPkgName = pName
		} else if pAST, err := parser.ParseFile(token.NewFileSet(), targetPath, clickedBytes, parser.PackageClauseOnly); err == nil && pAST != nil && pAST.Name != nil {
			clickedPkgName = pAST.Name.Name
		}
		if clickedPkgName == "" {
			return nil, nil
		}

		isTestTarget := strings.HasSuffix(pkgFile, "_test.go")
		if loader.revCache != nil {
			if entry := loader.loadCheckedPackage(dir, clickedPkgName, isTestTarget); entry != nil {
				if cf, ok := entry.fileByPath[targetPath]; ok && cf.ast != nil {
					loader.fset = entry.fset
					checkImportPath := req.PkgImportPath
					if isTestTarget && strings.HasSuffix(clickedPkgName, "_test") && !strings.HasSuffix(checkImportPath, "_test") {
						checkImportPath += "_test"
					}
					cfLines := cf.lines
					if len(cfLines) == 0 {
						cfLines = diff.SplitLines(clickedBytes)
					}
					if req.Site {
						return a.findTargetAtSite(rev, targetPath, false, false, dir, checkImportPath, mod, loader, cf.ast, entryASTFiles(entry), entry.pkg, entry.info, cfLines, req.Line)
					}
					if req.Line < 1 || req.Line > len(cfLines) {
						return nil, nil
					}
					lineStr := cfLines[req.Line-1]
					byteIdx := utf16ColToByteIndex(lineStr, req.UTF16Col)
					return a.findTargetInAST(rev, targetPath, false, false, dir, checkImportPath, mod, loader, cf.ast, entryASTFiles(entry), entry.pkg, entry.info, cfLines, req.Line, byteIdx)
				}
			}
		}

		virtualPath := filepath.Join(req.RepoRoot, filepath.FromSlash(targetPath))
		clickedAST, err = parser.ParseFile(loader.fset, virtualPath, clickedBytes, parser.SkipObjectResolution)
		if err != nil || clickedAST == nil {
			return nil, nil
		}
		pkgASTFiles = append(pkgASTFiles, clickedAST)

		names, _ := req.Repo.ListDir(rev, dir)
		bctx, fc := loader.makeBuildContextWithCache()
		for _, name := range names {
			if name == pkgFile || !strings.HasSuffix(name, ".go") || (!isTestTarget && strings.HasSuffix(name, "_test.go")) || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			data, ok := loader.matchAndReadInRepo(bctx, fc, dir, name)
			if !ok || len(data) == 0 {
				continue
			}
			p := path.Join(dir, name)
			vPath := filepath.Join(req.RepoRoot, filepath.FromSlash(p))
			sAST, err := parser.ParseFile(loader.fset, vPath, data, parser.SkipObjectResolution)
			if err == nil && sAST != nil && sAST.Name.Name == clickedAST.Name.Name {
				pkgASTFiles = append(pkgASTFiles, sAST)
			}
		}
	}

	checkImportPath := req.PkgImportPath
	basePkgImport = strings.TrimSuffix(req.PkgImportPath, "_test")
	if clickedAST != nil && strings.HasSuffix(clickedAST.Name.Name, "_test") {
		checkImportPath = basePkgImport + "_test"
		if isDisk {
			if basePkg, err := loader.loadDiskTestBasePackage(basePkgImport, dir); err == nil && basePkg != nil {
				loader.inRepoPkgs[basePkgImport] = basePkg
			}
		} else {
			if basePkg, err := loader.loadInRepoTestBasePackage(basePkgImport, dir); err == nil && basePkg != nil {
				loader.inRepoPkgs[basePkgImport] = basePkg
			}
		}
	}

	lines := diff.SplitLines(clickedBytes)
	if req.Line < 1 || req.Line > len(lines) {
		return nil, nil
	}
	lineStr := lines[req.Line-1]
	byteIdx := 0
	if !req.Site {
		byteIdx = utf16ColToByteIndex(lineStr, req.UTF16Col)

		if !isStringLiteralAtByteIndex(lineStr, byteIdx) {
			tokSym := extractSymbolAtByteIndex(lineStr, byteIdx)
			if tokSym == "" || isGoKeyword(tokSym) {
				if !hasPotentialDocLinkAtByteIndex(lineStr, byteIdx) {
					return nil, nil
				}
			}
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
		Importer:         loader,
		Error:            func(error) {},
	}

	rootPkg, _ := conf.Check(checkImportPath, loader.fset, pkgASTFiles, info)

	if req.Site {
		return a.findTargetAtSite(rev, "", true, isDisk, dir, checkImportPath, mod, loader, clickedAST, pkgASTFiles, rootPkg, info, lines, req.Line)
	}
	return a.findTargetInAST(rev, "", true, isDisk, dir, checkImportPath, mod, loader, clickedAST, pkgASTFiles, rootPkg, info, lines, req.Line, byteIdx)
}

func (a *Analyzer) findTargetAtSite(
	rev, cleanPath string,
	isChained, isDisk bool,
	dir, rootImportPath string,
	mod *moduleInfo,
	loader *lookupLoader,
	clickedAST *ast.File,
	pkgASTFiles []*ast.File,
	rootPkg *types.Package,
	info *types.Info,
	lines []string,
	siteLine int,
) (*resolvedTarget, error) {
	var selectedIdent *ast.Ident
	var selectedObj types.Object
	var isInitFunc bool

	for _, decl := range clickedAST.Decls {
		pos := loader.fset.Position(decl.Pos())
		end := loader.fset.Position(decl.End())
		if siteLine < pos.Line || siteLine > end.Line {
			continue
		}

		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name != nil && d.Name.Name != "_" {
				selectedIdent = d.Name
				selectedObj = info.Defs[d.Name]
				if d.Recv == nil && d.Name.Name == "init" {
					isInitFunc = true
				}
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				sPos := loader.fset.Position(spec.Pos())
				sEnd := loader.fset.Position(spec.End())
				if siteLine < sPos.Line || siteLine > sEnd.Line {
					continue
				}

				switch sp := spec.(type) {
				case *ast.TypeSpec:
					matchedField := false
					if st, ok := sp.Type.(*ast.StructType); ok && st.Fields != nil {
						for _, f := range st.Fields.List {
							fPos := loader.fset.Position(f.Pos())
							fEnd := loader.fset.Position(f.End())
							if siteLine >= fPos.Line && siteLine <= fEnd.Line {
								if len(f.Names) > 0 && f.Names[0].Name != "_" {
									selectedIdent = f.Names[0]
									selectedObj = info.Defs[f.Names[0]]
									matchedField = true
									break
								}
							}
						}
					} else if it, ok := sp.Type.(*ast.InterfaceType); ok && it.Methods != nil {
						for _, m := range it.Methods.List {
							mPos := loader.fset.Position(m.Pos())
							mEnd := loader.fset.Position(m.End())
							if siteLine >= mPos.Line && siteLine <= mEnd.Line {
								if len(m.Names) > 0 && m.Names[0].Name != "_" {
									selectedIdent = m.Names[0]
									selectedObj = info.Defs[m.Names[0]]
									matchedField = true
									break
								}
							}
						}
					}
					if !matchedField && sp.Name != nil && sp.Name.Name != "_" {
						selectedIdent = sp.Name
						selectedObj = info.Defs[sp.Name]
					}
				case *ast.ValueSpec:
					if len(sp.Values) == len(sp.Names) {
						for i, val := range sp.Values {
							if val != nil {
								vPos := loader.fset.Position(val.Pos())
								vEnd := loader.fset.Position(val.End())
								if siteLine >= vPos.Line && siteLine <= vEnd.Line {
									if i < len(sp.Names) && sp.Names[i] != nil && sp.Names[i].Name != "_" {
										selectedIdent = sp.Names[i]
										selectedObj = info.Defs[sp.Names[i]]
										break
									}
								}
							}
						}
					}
					if selectedIdent == nil {
						for _, name := range sp.Names {
							if name != nil && name.Name != "_" {
								nPos := loader.fset.Position(name.Pos())
								if nPos.Line == siteLine || selectedIdent == nil {
									selectedIdent = name
									selectedObj = info.Defs[name]
									if nPos.Line == siteLine {
										break
									}
								}
							}
						}
					}
				}
				if selectedIdent != nil && selectedObj != nil {
					break
				}
			}
		}
		if selectedIdent != nil && selectedObj != nil {
			break
		}
	}

	isPkgClause := false
	var symLine, symCol int
	if selectedIdent != nil && selectedObj != nil {
		p := loader.fset.Position(selectedIdent.Pos())
		symLine = p.Line
		symCol = byteColToUTF16Col(lines, p.Line, p.Column)
	} else {
		// Fallback to package clause
		selectedIdent = clickedAST.Name
		pkgObjTarget := rootPkg
		if pkgObjTarget == nil || pkgObjTarget.Name() != clickedAST.Name.Name {
			pkgObjTarget = types.NewPackage(rootImportPath, clickedAST.Name.Name)
		}
		selectedObj = types.NewPkgName(clickedAST.Name.Pos(), pkgObjTarget, clickedAST.Name.Name, pkgObjTarget)
		isPkgClause = true
		p := loader.fset.Position(clickedAST.Name.Pos())
		symLine = p.Line
		symCol = byteColToUTF16Col(lines, p.Line, p.Column)
	}

	target := &resolvedTarget{
		rev:            rev,
		cleanPath:      cleanPath,
		isChained:      isChained,
		isDisk:         isDisk,
		isPkgClause:    isPkgClause,
		isInitFunc:     isInitFunc,
		dir:            dir,
		rootImportPath: rootImportPath,
		mod:            mod,
		loader:         loader,
		clickedAST:     clickedAST,
		pkgASTFiles:    pkgASTFiles,
		rootPkg:        rootPkg,
		info:           info,
		clickedIdent:   selectedIdent,
		obj:            selectedObj,
		siteLines:      lines,
		siteLine:       siteLine,
		symLine:        symLine,
		symCol:         symCol,
	}
	return target, nil
}

func (a *Analyzer) findTargetInAST(rev, cleanPath string, isChained, isDisk bool, dir, rootImportPath string, mod *moduleInfo, loader *lookupLoader, clickedAST *ast.File, pkgASTFiles []*ast.File, rootPkg *types.Package, info *types.Info, lines []string, targetLine, byteIdx int) (*resolvedTarget, error) {
	var clickedIdent *ast.Ident
	var clickedImport *ast.ImportSpec
	var enclosingSelector *ast.SelectorExpr
	var typeSwitchIdent *ast.Ident
	var enclosingTypeSwitch *ast.TypeSwitchStmt

	ast.Inspect(clickedAST, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		pos := loader.fset.Position(n.Pos())
		end := loader.fset.Position(n.End())
		if targetLine < pos.Line || targetLine > end.Line {
			return false
		}

		if spec, ok := n.(*ast.ImportSpec); ok && spec.Path != nil {
			pPos := loader.fset.Position(spec.Path.Pos())
			pEnd := loader.fset.Position(spec.Path.End())
			if pPos.Line == targetLine && byteIdx+1 >= pPos.Column && (pEnd.Line > targetLine || byteIdx+1 <= pEnd.Column) {
				clickedImport = spec
				return false
			}
		}

		if ts, ok := n.(*ast.TypeSwitchStmt); ok {
			if assign, ok := ts.Assign.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 {
				if lhsIdent, ok := assign.Lhs[0].(*ast.Ident); ok {
					iPos := loader.fset.Position(lhsIdent.Pos())
					iEnd := loader.fset.Position(lhsIdent.End())
					if iPos.Line == targetLine && byteIdx+1 >= iPos.Column && (iEnd.Line > targetLine || byteIdx+1 <= iEnd.Column) {
						typeSwitchIdent = lhsIdent
						enclosingTypeSwitch = ts
					}
				}
			}
		}

		if sel, ok := n.(*ast.SelectorExpr); ok {
			sPos := loader.fset.Position(sel.Sel.Pos())
			sEnd := loader.fset.Position(sel.Sel.End())
			if sPos.Line == targetLine && byteIdx+1 >= sPos.Column && (sEnd.Line > targetLine || byteIdx+1 <= sEnd.Column) {
				enclosingSelector = sel
			}
		}

		if id, ok := n.(*ast.Ident); ok {
			if pos.Line == targetLine && byteIdx+1 >= pos.Column && (end.Line > targetLine || byteIdx+1 <= end.Column) {
				clickedIdent = id
				return false
			}
		}
		return true
	})

	if clickedImport != nil {
		impPath, err := strconv.Unquote(clickedImport.Path.Value)
		if err != nil || impPath == "" || impPath == "C" || strings.HasPrefix(impPath, ".") {
			return nil, nil
		}
		var obj types.Object = info.Implicits[clickedImport]
		if obj == nil && clickedImport.Name != nil {
			obj = info.Defs[clickedImport.Name]
		}
		if obj == nil {
			pkg, err := loader.Import(impPath)
			if err != nil || pkg == nil {
				return nil, nil
			}
			obj = types.NewPkgName(clickedImport.Path.Pos(), rootPkg, pkg.Name(), pkg)
		}
		target := &resolvedTarget{
			rev:            rev,
			cleanPath:      cleanPath,
			isChained:      isChained,
			isDisk:         isDisk,
			dir:            dir,
			rootImportPath: rootImportPath,
			mod:            mod,
			loader:         loader,
			clickedAST:     clickedAST,
			pkgASTFiles:    pkgASTFiles,
			rootPkg:        rootPkg,
			info:           info,
			obj:            obj,
		}
		return target, nil
	}

	if clickedIdent == nil {
		return a.resolveDocLinkTarget(rev, cleanPath, isChained, isDisk, dir, rootImportPath, mod, loader, clickedAST, pkgASTFiles, rootPkg, info, lines, targetLine, byteIdx)
	}

	obj := info.Uses[clickedIdent]
	if obj == nil {
		obj = info.Defs[clickedIdent]
	}
	isPkgClause := false
	if obj == nil && clickedIdent != nil && clickedAST != nil && clickedIdent == clickedAST.Name && clickedIdent.Name != "" && clickedIdent.Name != "_" {
		pkgObjTarget := rootPkg
		if pkgObjTarget == nil || pkgObjTarget.Name() != clickedIdent.Name {
			pkgObjTarget = types.NewPackage(rootImportPath, clickedIdent.Name)
		}
		obj = types.NewPkgName(clickedIdent.Pos(), pkgObjTarget, clickedIdent.Name, pkgObjTarget)
		isPkgClause = true
	}
	if obj == nil && clickedIdent == typeSwitchIdent && enclosingTypeSwitch != nil && enclosingTypeSwitch.Body != nil {
		for _, stmt := range enclosingTypeSwitch.Body.List {
			if clause, ok := stmt.(*ast.CaseClause); ok {
				if imp := info.Implicits[clause]; imp != nil {
					obj = imp
					break
				}
			}
		}
	}

	if pkgName, ok := obj.(*types.PkgName); ok {
		if pkgName.Imported() != nil && pkgName.Imported().Path() == "C" {
			return nil, nil
		}
	}

	target := &resolvedTarget{
		rev:                 rev,
		cleanPath:           cleanPath,
		isChained:           isChained,
		isDisk:              isDisk,
		isPkgClause:         isPkgClause,
		dir:                 dir,
		rootImportPath:      rootImportPath,
		mod:                 mod,
		loader:              loader,
		clickedAST:          clickedAST,
		pkgASTFiles:         pkgASTFiles,
		rootPkg:             rootPkg,
		info:                info,
		clickedIdent:        clickedIdent,
		enclosingSelector:   enclosingSelector,
		typeSwitchIdent:     typeSwitchIdent,
		enclosingTypeSwitch: enclosingTypeSwitch,
		obj:                 obj,
	}

	if obj == nil && enclosingSelector != nil && enclosingSelector.Sel == clickedIdent {
		if xId, ok := enclosingSelector.X.(*ast.Ident); ok {
			if pkgObj := info.Uses[xId]; pkgObj != nil {
				if pkgName, ok := pkgObj.(*types.PkgName); ok {
					target.stubbedPkgPath = pkgName.Imported().Path()
					target.stubbedPkgName = pkgName.Name()
					target.stubbedSymName = clickedIdent.Name
				}
			}
		}
	}

	return target, nil
}

func (a *Analyzer) buildResult(req LookupRequest, target *resolvedTarget) (*Result, error) {
	obj := target.obj
	if obj == nil {
		return &Result{Found: false}, nil
	}

	rootPkg := target.rootPkg
	loader := target.loader
	info := target.info
	mod := target.mod
	rev := target.rev
	enclosingSelector := target.enclosingSelector

	res := &Result{
		SymbolName: obj.Name(),
	}

	// 1. Built-ins & Universe (obj.Pkg() == nil)
	if obj.Pkg() == nil {
		res.Found = true
		res.Signature = formatSignature(obj, rootPkg)
		res.SignatureHTML = highlightSignature(res.Signature)
		if obj.Name() != "nil" {
			res.PkgGoDevURL = "https://pkg.go.dev/builtin#" + obj.Name()
		}
		builtinPath := ""
		if loader.env.goroot != "" {
			builtinPath = filepath.Join(loader.env.goroot, "src", "builtin", "builtin.go")
		}

		if builtinPath != "" && fileExists(builtinPath) {
			if bBytes, err := os.ReadFile(builtinPath); err == nil {
				bFset := token.NewFileSet()
				if bAST, err := parser.ParseFile(bFset, builtinPath, bBytes, parser.SkipObjectResolution); err == nil {
					bLines := diff.SplitLines(bBytes)
				declLoop:
					for _, decl := range bAST.Decls {
						if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == obj.Name() {
							p := bFset.Position(fn.Name.Pos())
							res.DefLine = p.Line
							res.DisplayPath = "builtin/builtin.go"
							res.DefPkg = "builtin"
							res.DefFile = "builtin.go"
							res.Snippet, res.SnippetTruncated = buildSnippet(bLines, res.DefLine)
							if p.Line > 0 && p.Line <= len(bLines) {
								sigLine := strings.TrimSpace(bLines[p.Line-1])
								sigLine = strings.TrimSuffix(sigLine, " {}")
								sigLine = strings.TrimSuffix(sigLine, " {")
								res.Signature = sigLine
								res.SignatureHTML = highlightSignature(sigLine)
							}
							break declLoop
						} else if gd, ok := decl.(*ast.GenDecl); ok {
							for _, sp := range gd.Specs {
								if ts, ok := sp.(*ast.TypeSpec); ok {
									if ts.Name.Name == obj.Name() {
										p := bFset.Position(ts.Name.Pos())
										res.DefLine = p.Line
										res.DisplayPath = "builtin/builtin.go"
										res.DefPkg = "builtin"
										res.DefFile = "builtin.go"
										res.Snippet, res.SnippetTruncated = buildSnippet(bLines, res.DefLine)
										if p.Line > 0 && p.Line <= len(bLines) {
											sigLine := strings.TrimSpace(bLines[p.Line-1])
											res.Signature = sigLine
											res.SignatureHTML = highlightSignature(sigLine)
										}
										break declLoop
									}
									if it, ok := ts.Type.(*ast.InterfaceType); ok && it.Methods != nil {
										for _, m := range it.Methods.List {
											for _, id := range m.Names {
												if id.Name == obj.Name() {
													p := bFset.Position(id.Pos())
													res.DefLine = p.Line
													res.DisplayPath = "builtin/builtin.go"
													res.DefPkg = "builtin"
													res.DefFile = "builtin.go"
													res.Snippet, res.SnippetTruncated = buildSnippet(bLines, res.DefLine)
													if p.Line > 0 && p.Line <= len(bLines) {
														sigLine := strings.TrimSpace(bLines[p.Line-1])
														res.Signature = sigLine
														res.SignatureHTML = highlightSignature(sigLine)
													}
													break declLoop
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
		return res, nil
	}

	// 2. Package Names (*types.PkgName)
	if pkgName, ok := obj.(*types.PkgName); ok {
		imported := pkgName.Imported()
		if imported == nil || imported.Path() == "C" || (!target.isPkgClause && (imported.Path() == "" || imported.Path() == ".")) {
			return &Result{Found: false}, nil
		}
		if res.SymbolName == "" || res.SymbolName == "_" || res.SymbolName == "." {
			res.SymbolName = imported.Name()
			if res.SymbolName == "" {
				res.SymbolName = guessPkgName(imported.Path())
			}
		}

		pkgDispName := imported.Name()
		if pkgDispName == "" {
			pkgDispName = res.SymbolName
		}

		res.Found = true
		isImportable := imported.Path() != "" && imported.Path() != "." && pkgDispName != "main" && !strings.HasSuffix(pkgDispName, "_test")
		if isImportable {
			res.Signature = fmt.Sprintf("package %s (%q)", pkgDispName, imported.Path())
			res.SignatureHTML = fmt.Sprintf(`package <span class="tok-id">%s</span> (<span class="tok-str">%s</span>)`, html.EscapeString(pkgDispName), html.EscapeString(fmt.Sprintf("%q", imported.Path())))
			res.PkgGoDevURL = pkgGoDevForPkg(imported.Path(), mod, loader.env)
		} else {
			res.Signature = fmt.Sprintf("package %s", pkgDispName)
			res.SignatureHTML = fmt.Sprintf(`package <span class="tok-id">%s</span>`, html.EscapeString(pkgDispName))
		}

		var dir string
		var isDisk bool
		var err error
		if target.isPkgClause {
			dir, isDisk, err = target.dir, target.isDisk, nil
		} else {
			dir, isDisk, err = loader.resolvePackageDir(imported.Path())
		}
		if err == nil && (dir != "" || !isDisk) {
			bctx, fc := loader.makeBuildContextWithCache()
			var rawNames []string
			if isDisk {
				if entries, err := os.ReadDir(dir); err == nil {
					for _, e := range entries {
						if !e.IsDir() {
							rawNames = append(rawNames, e.Name())
						}
					}
				}
			} else if req.Repo != nil {
				rawNames, _ = req.Repo.ListDir(rev, dir)
			}

			var nonTestGoFiles []string
			for _, name := range rawNames {
				if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_") {
					nonTestGoFiles = append(nonTestGoFiles, name)
				}
			}
			slices.Sort(nonTestGoFiles)

			// Order candidates by gopls convention ("doc.go" -> <pkg>.go -> <base>.go -> alphabetical)
			// before reading so we only read the chosen primary file instead of every file in the package.
			targetName := pkgDispName + ".go"
			targetBase := path.Base(imported.Path()) + ".go"
			orderedFiles := make([]string, 0, len(nonTestGoFiles))
			seenFile := make(map[string]bool, len(nonTestGoFiles))
			for _, pref := range []string{"doc.go", targetName, targetBase} {
				if !seenFile[pref] && slices.Contains(nonTestGoFiles, pref) {
					seenFile[pref] = true
					orderedFiles = append(orderedFiles, pref)
				}
			}
			for _, name := range nonTestGoFiles {
				if !seenFile[name] {
					orderedFiles = append(orderedFiles, name)
				}
			}

			pickFile := func(candidates []string, matchBuildTags bool) (string, []byte, int) {
				for _, name := range candidates {
					var data []byte
					if matchBuildTags {
						var ok bool
						if isDisk {
							data, ok = loader.matchAndReadDisk(bctx, fc, dir, name)
						} else {
							data, ok = loader.matchAndReadInRepo(bctx, fc, dir, name)
						}
						if !ok || len(data) == 0 {
							continue
						}
					} else {
						if isDisk {
							data, _ = os.ReadFile(filepath.Join(dir, name))
						} else if req.Repo != nil {
							inRepoFilePath := name
							if dir != "" && dir != "." {
								inRepoFilePath = path.Join(dir, name)
							}
							data, _ = req.Repo.Content(rev, inRepoFilePath)
						}
						if len(data) == 0 {
							continue
						}
					}

					fset := token.NewFileSet()
					astF, err := parser.ParseFile(fset, name, data, parser.PackageClauseOnly)
					if (matchBuildTags || target.isPkgClause) && pkgDispName != "" && (err != nil || astF == nil || astF.Name.Name != pkgDispName) {
						continue
					}
					defLine := 1
					if err == nil && astF != nil {
						if p := fset.Position(astF.Name.Pos()); p.IsValid() && p.Line > 0 {
							defLine = p.Line
						}
					}
					return name, data, defLine
				}
				return "", nil, 0
			}

			chosen, defBytes, defLine := pickFile(orderedFiles, true)
			if chosen == "" {
				chosen, defBytes, defLine = pickFile(orderedFiles, false)
			}
			if chosen == "" && target.isPkgClause {
				var testGoFiles []string
				for _, name := range rawNames {
					if strings.HasSuffix(name, "_test.go") && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_") {
						testGoFiles = append(testGoFiles, name)
					}
				}
				slices.Sort(testGoFiles)
				testTargetBase := path.Base(strings.TrimSuffix(imported.Path(), "_test")) + "_test.go"
				var orderedTestFiles []string
				seenTest := make(map[string]bool, len(testGoFiles))
				for _, pref := range []string{"doc_test.go", pkgDispName + ".go", testTargetBase} {
					if !seenTest[pref] && slices.Contains(testGoFiles, pref) {
						seenTest[pref] = true
						orderedTestFiles = append(orderedTestFiles, pref)
					}
				}
				for _, name := range testGoFiles {
					if !seenTest[name] {
						orderedTestFiles = append(orderedTestFiles, name)
					}
				}
				chosen, defBytes, defLine = pickFile(orderedTestFiles, true)
				if chosen == "" {
					chosen, defBytes, defLine = pickFile(orderedTestFiles, false)
				}
			}

			if chosen != "" && len(defBytes) > 0 {
				defAbsPath := ""
				if isDisk {
					defAbsPath = filepath.Join(dir, chosen)
					res.InRepo = false
					res.DisplayPath = computeDisplayPath(defAbsPath, loader.env)
					res.DefFile = chosen
					defPkg := imported.Path()
					if strings.HasSuffix(defPkg, "_test") {
						if req.PkgImportPath != "" {
							defPkg = req.PkgImportPath
						} else {
							defPkg = strings.TrimSuffix(defPkg, "_test")
						}
					}
					res.DefPkg = defPkg
				} else {
					inRepoRel := chosen
					if dir != "" && dir != "." {
						inRepoRel = path.Join(dir, chosen)
					}
					defAbsPath = filepath.Join(req.RepoRoot, filepath.FromSlash(inRepoRel))
					res.InRepo = true
					res.DefPath = inRepoRel
					res.DisplayPath = inRepoRel
					res.DefFile = chosen
					res.DefPkg = cleanPathToImport(mod, dir)
				}

				res.DefLine = defLine
				lines := diff.SplitLines(defBytes)
				res.Snippet, res.SnippetTruncated = buildSnippet(lines, defLine)
				return res, nil
			}
		}

		return res, nil
	}

	// 3. Normal declared objects
	pos := loader.positionFor(obj, rootPkg)
	if !pos.IsValid() {
		res.Found = true
		res.Signature = formatSignature(obj, rootPkg)
		res.SignatureHTML = highlightSignature(res.Signature)
		res.PkgGoDevURL = buildPkgGoDevURL(obj, enclosingSelector, info, "", false, mod, loader.env)
		return res, nil
	}
	defAbsPath := pos.Filename
	res.DefLine = pos.Line
	res.Found = true

	cleanRepo := filepath.Clean(req.RepoRoot)
	rel, relErr := filepath.Rel(cleanRepo, defAbsPath)
	var defBytes []byte

	if relErr == nil && !strings.HasPrefix(rel, "..") && rel != "." {
		res.InRepo = true
		res.DefPath = filepath.ToSlash(rel)
		res.DisplayPath = res.DefPath
		res.DefFile = path.Base(res.DefPath)
		res.DefPkg = cleanPathToImport(mod, path.Dir(res.DefPath))
		defBytes, _ = req.Repo.Content(rev, res.DefPath)
	} else if fileExists(defAbsPath) {
		res.InRepo = false
		res.DisplayPath = computeDisplayPath(defAbsPath, loader.env)
		res.DefFile = filepath.Base(defAbsPath)
		if obj.Pkg() != nil {
			res.DefPkg = obj.Pkg().Path()
		}
		defBytes, _ = os.ReadFile(defAbsPath)
	}

	res.Signature = formatSignature(obj, rootPkg)
	res.SignatureHTML = highlightSignature(res.Signature)
	res.PkgGoDevURL = buildPkgGoDevURL(obj, enclosingSelector, info, defAbsPath, res.InRepo, mod, loader.env)

	if len(defBytes) > 0 {
		defLines := diff.SplitLines(defBytes)
		res.Snippet, res.SnippetTruncated = buildSnippet(defLines, res.DefLine)
	}

	return res, nil
}

func cleanPathToImport(mod *moduleInfo, dirInRepo string) string {
	cleanDir := path.Clean(dirInRepo)
	if cleanDir == "." || cleanDir == "" {
		if mod.modPath != "" && mod.modDir == "" {
			return mod.modPath
		}
		return "main"
	}
	for sibMod, sibDir := range mod.siblingMods {
		if sibDir == cleanDir {
			if sibMod == "std" {
				return ""
			}
			return sibMod
		}
		if sibDir != "" && strings.HasPrefix(cleanDir, sibDir+"/") {
			sub := strings.TrimPrefix(cleanDir, sibDir+"/")
			if sibMod == "std" {
				return sub
			}
			return path.Join(sibMod, sub)
		}
	}
	if mod.modPath == "std" {
		return strings.TrimPrefix(cleanDir, "src/")
	}
	if mod.modPath != "" {
		if mod.modDir == cleanDir {
			return mod.modPath
		}
		if mod.modDir != "" && strings.HasPrefix(cleanDir, mod.modDir+"/") {
			sub := strings.TrimPrefix(cleanDir, mod.modDir+"/")
			return path.Join(mod.modPath, sub)
		}
		if mod.modDir == "" {
			return path.Join(mod.modPath, cleanDir)
		}
	}
	return cleanDir
}

func computeDisplayPath(defAbsPath string, env goEnv) string {
	if env.goroot != "" {
		stdPrefix := filepath.Join(env.goroot, "src") + string(filepath.Separator)
		if strings.HasPrefix(defAbsPath, stdPrefix) {
			rel := strings.TrimPrefix(defAbsPath, stdPrefix)
			rel = strings.TrimPrefix(rel, "vendor"+string(filepath.Separator))
			return filepath.ToSlash(rel)
		}
	}
	if env.gomodcache != "" {
		modPrefix := filepath.Clean(env.gomodcache) + string(filepath.Separator)
		if strings.HasPrefix(defAbsPath, modPrefix) {
			rel := strings.TrimPrefix(defAbsPath, modPrefix)
			return filepath.ToSlash(rel)
		}
	}
	if env.gopath != "" {
		for _, gp := range filepath.SplitList(env.gopath) {
			if gp == "" {
				continue
			}
			gpPrefix := filepath.Join(gp, "src") + string(filepath.Separator)
			if strings.HasPrefix(defAbsPath, gpPrefix) {
				rel := strings.TrimPrefix(defAbsPath, gpPrefix)
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.ToSlash(filepath.Base(defAbsPath))
}

func formatSignature(obj types.Object, rootPkg *types.Package) string {
	relPkg := obj.Pkg()
	if relPkg == nil {
		relPkg = rootPkg
	}
	qf := func(p *types.Package) string {
		if p == nil || p == relPkg {
			return ""
		}
		return p.Name()
	}

	switch v := obj.(type) {
	case *types.Func:
		return types.ObjectString(v, qf)

	case *types.TypeName:
		if v.IsAlias() {
			return "type " + v.Name() + " = " + types.TypeString(v.Type(), qf)
		}
		underlying := v.Type().Underlying()
		if _, ok := underlying.(*types.Struct); ok {
			return "type " + v.Name() + " struct"
		}
		if _, ok := underlying.(*types.Interface); ok {
			return "type " + v.Name() + " interface"
		}
		return "type " + v.Name() + " " + types.TypeString(underlying, qf)

	case *types.Const:
		sig := types.ObjectString(v, qf)
		if v.Val() != nil {
			valStr := v.Val().ExactString()
			if len(valStr) <= 80 {
				sig += " = " + valStr
			}
		}
		return sig

	case *types.Var:
		if v.IsField() {
			structName := findDeclaringStructName(v)
			if structName != "" {
				return fmt.Sprintf("field (%s).%s %s", structName, v.Name(), types.TypeString(v.Type(), qf))
			}
			return fmt.Sprintf("field %s %s", v.Name(), types.TypeString(v.Type(), qf))
		}
		return types.ObjectString(v, qf)

	default:
		return types.ObjectString(obj, qf)
	}
}

func highlightSignature(sig string) string {
	if sig == "" {
		return ""
	}
	sigLines := strings.Split(sig, "\n")
	toks := diff.HighlightLines(sigLines, diff.LangGo)
	rendered := make([]string, len(sigLines))
	for i := range sigLines {
		var lineToks []diff.TokenSpan
		if i < len(toks) {
			lineToks = toks[i]
		}
		rendered[i] = string(diff.RenderTokensHTMLWS(sigLines[i], lineToks, nil, false))
	}
	return strings.Join(rendered, "\n")
}

func entryASTFiles(entry *checkedPkgEntry) []*ast.File {
	if entry == nil || len(entry.files) == 0 {
		return nil
	}
	files := make([]*ast.File, 0, len(entry.files))
	for _, cf := range entry.files {
		if cf != nil && cf.ast != nil {
			files = append(files, cf.ast)
		}
	}
	return files
}

func findDeclaringStructName(v *types.Var) string {
	if v == nil {
		return ""
	}
	vOrig := v.Origin()
	if vOrig.Pkg() == nil {
		return ""
	}
	scope := vOrig.Pkg().Scope()
	for _, name := range scope.Names() {
		if tn, ok := scope.Lookup(name).(*types.TypeName); ok && !tn.IsAlias() {
			if st, ok := types.Unalias(tn.Type()).Underlying().(*types.Struct); ok {
				for i := 0; i < st.NumFields(); i++ {
					if st.Field(i).Origin() == vOrig {
						return tn.Name()
					}
				}
			}
		}
	}
	return ""
}

func isStdlibOrCmd(pkgPath, defAbsPath string, mod *moduleInfo, env goEnv, inRepo bool) bool {
	if pkgPath == "" || pkgPath == "main" || pkgPath == "command-line-arguments" {
		return false
	}
	if pkgPath == "builtin" {
		return true
	}
	cleanPkg := strings.TrimPrefix(pkgPath, "cmd/vendor/")
	cleanPkg = strings.TrimPrefix(cleanPkg, "vendor/")
	firstSeg := cleanPkg
	if i := strings.Index(firstSeg, "/"); i != -1 {
		firstSeg = firstSeg[:i]
	}
	if strings.Contains(firstSeg, ".") {
		return false
	}
	if mod != nil && (mod.modPath == "std" || mod.modPath == "cmd") {
		return true
	}
	if !inRepo && env.goroot != "" {
		gorootSrc := filepath.Join(env.goroot, "src")
		if defAbsPath != "" && strings.HasPrefix(defAbsPath, gorootSrc+string(filepath.Separator)) {
			return true
		}
		if dirExists(filepath.Join(gorootSrc, filepath.FromSlash(cleanPkg))) {
			return true
		}
	}
	return false
}

func pkgGoDevBaseURL(importPath, defAbsPath string, inRepo bool, mod *moduleInfo, env goEnv) (cleanPkg string, ok bool) {
	if importPath == "" || importPath == "main" || importPath == "command-line-arguments" {
		return "", false
	}
	cleanPkg = strings.TrimPrefix(importPath, "cmd/vendor/")
	cleanPkg = strings.TrimPrefix(cleanPkg, "vendor/")
	firstSeg := cleanPkg
	if i := strings.Index(firstSeg, "/"); i != -1 {
		firstSeg = firstSeg[:i]
	}
	hasDot := strings.Contains(firstSeg, ".")
	if !hasDot && !isStdlibOrCmd(cleanPkg, defAbsPath, mod, env, inRepo) {
		return "", false
	}
	return cleanPkg, true
}

func buildPkgGoDevURL(obj types.Object, sel *ast.SelectorExpr, info *types.Info, defAbsPath string, inRepo bool, mod *moduleInfo, env goEnv) string {
	pkg := obj.Pkg()
	if pkg == nil {
		return ""
	}
	cleanPkg, ok := pkgGoDevBaseURL(pkg.Path(), defAbsPath, inRepo, mod, env)
	if !ok {
		return ""
	}

	// 1. Package-level exported object
	if obj.Exported() && obj.Parent() == pkg.Scope() {
		return fmt.Sprintf("https://pkg.go.dev/%s#%s", cleanPkg, obj.Name())
	}

	// 2. Exported Method
	if fn, ok := obj.(*types.Func); ok && fn.Exported() {
		sig, ok := fn.Type().(*types.Signature)
		if ok && sig.Recv() != nil {
			recvType := sig.Recv().Type()
			recvNamed := unwrapNamed(recvType)
			recvName := ""
			if recvNamed != nil {
				recvName = recvNamed.Obj().Name()
			} else if _, ok := recvType.Underlying().(*types.Interface); ok {
				// Interface method (e.g. http.ResponseWriter.WriteHeader)
				if sel != nil && info != nil {
					if s := info.Selections[sel]; s != nil {
						if rNamed := unwrapNamed(s.Recv()); rNamed != nil {
							recvName = rNamed.Obj().Name()
						}
					}
				}
				if recvName == "" {
					scope := pkg.Scope()
					for _, name := range scope.Names() {
						if tn, ok := scope.Lookup(name).(*types.TypeName); ok && tn.Exported() {
							if iface, ok := tn.Type().Underlying().(*types.Interface); ok {
								for i := 0; i < iface.NumMethods(); i++ {
									if iface.Method(i) == fn || iface.Method(i).Name() == fn.Name() {
										recvName = tn.Name()
										break
									}
								}
								if recvName != "" {
									break
								}
							}
						}
					}
				}
			}

			if recvName != "" && unicode.IsUpper(rune(recvName[0])) {
				return fmt.Sprintf("https://pkg.go.dev/%s#%s.%s", cleanPkg, recvName, fn.Name())
			}
		}
	}

	// 3. Exported struct field
	if v, ok := obj.(*types.Var); ok && v.IsField() && v.Exported() {
		structName := findDeclaringStructName(v)
		if structName != "" && unicode.IsUpper(rune(structName[0])) {
			return fmt.Sprintf("https://pkg.go.dev/%s#%s.%s", cleanPkg, structName, v.Name())
		}
	}

	return ""
}

func unwrapNamed(t types.Type) *types.Named {
	for {
		if pt, ok := t.(*types.Pointer); ok {
			t = pt.Elem()
			continue
		}
		if named, ok := t.(*types.Named); ok {
			return named
		}
		if alias, ok := t.(*types.Alias); ok {
			t = types.Unalias(alias)
			continue
		}
		break
	}
	return nil
}

func buildPkgGoDevURLForPath(importPath, symName string, mod *moduleInfo, env goEnv, inRepo bool) string {
	cleanPkg, ok := pkgGoDevBaseURL(importPath, "", inRepo, mod, env)
	if !ok {
		return ""
	}
	if symName != "" && unicode.IsUpper(rune(symName[0])) {
		return fmt.Sprintf("https://pkg.go.dev/%s#%s", cleanPkg, symName)
	}
	return fmt.Sprintf("https://pkg.go.dev/%s", cleanPkg)
}

func pkgGoDevForPkg(importPath string, mod *moduleInfo, env goEnv) string {
	return buildPkgGoDevURLForPath(importPath, "", mod, env, false)
}

func buildSnippet(lines []string, defLine int) (snippet []SnippetLine, truncated bool) {
	if defLine < 1 || defLine > len(lines) {
		return nil, false
	}

	maxCount := 5000
	startLine := 1
	if defLine > maxCount/2 {
		startLine = defLine - maxCount/2
	}
	endLine := startLine + maxCount - 1
	if endLine > len(lines) {
		endLine = len(lines)
		if endLine-maxCount+1 >= 1 {
			startLine = endLine - maxCount + 1
		} else {
			startLine = 1
		}
	}
	truncated = (startLine > 1 || endLine < len(lines))

	var allToks [][]diff.TokenSpan
	if startLine == 1 {
		allToks = diff.HighlightLines(lines[:endLine], diff.LangGo)
	} else {
		keep := make([]bool, endLine)
		for i := startLine - 1; i < endLine; i++ {
			keep[i] = true
		}
		allToks = diff.HighlightLinesKeep(lines[:endLine], diff.LangGo, keep)
	}

	for i := startLine; i <= endLine; i++ {
		lineIdx := i - 1
		var lineToks []diff.TokenSpan
		if lineIdx < len(allToks) {
			lineToks = allToks[lineIdx]
		}
		raw := lines[lineIdx]
		html := string(diff.RenderTokensHTMLWS(raw, lineToks, nil, false))
		snippet = append(snippet, SnippetLine{
			Line:   i,
			HTML:   html,
			Active: (i == defLine),
		})
	}

	return snippet, truncated
}

func byteColToUTF16Col(lines []string, line, byteCol1Based int) int {
	if line < 1 || line > len(lines) || byteCol1Based <= 1 {
		return 0
	}
	lineStr := lines[line-1]
	byteIdx := byteCol1Based - 1
	if byteIdx > len(lineStr) {
		byteIdx = len(lineStr)
	}
	col := 0
	for _, r := range lineStr[:byteIdx] {
		if r >= 0x10000 {
			col += 2
		} else {
			col++
		}
	}
	return col
}

func utf16ColToByteIndex(line string, col16 int) int {
	if col16 <= 0 {
		return 0
	}
	u16 := 0
	for i, r := range line {
		if u16 >= col16 {
			return i
		}
		if r >= 0x10000 {
			u16 += 2
		} else {
			u16++
		}
	}
	return len(line)
}

func extractSymbolAtByteIndex(line string, byteIdx int) string {
	if byteIdx < 0 || byteIdx >= len(line) {
		return ""
	}
	// If byteIdx is pointing to a continuation byte in UTF-8, step back to rune start
	// if and only if it is part of a valid multi-byte rune enclosing origByteIdx.
	if !utf8.RuneStart(line[byteIdx]) {
		origByteIdx := byteIdx
		found := false
		for step := 1; step < utf8.UTFMax && byteIdx-step >= 0; step++ {
			cand := byteIdx - step
			if utf8.RuneStart(line[cand]) {
				r, width := utf8.DecodeRuneInString(line[cand:])
				if r != utf8.RuneError && cand+width > origByteIdx {
					byteIdx = cand
					found = true
				}
				break
			}
		}
		if !found {
			return ""
		}
	}
	start := byteIdx
	for start > 0 {
		r, width := utf8.DecodeLastRuneInString(line[:start])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			start -= width
		} else {
			break
		}
	}
	end := byteIdx
	for end < len(line) {
		r, width := utf8.DecodeRuneInString(line[end:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			end += width
		} else {
			break
		}
	}
	if start >= end {
		return ""
	}
	return line[start:end]
}

func isGoKeyword(word string) bool {
	switch word {
	case "break", "case", "chan", "const", "continue", "default", "defer",
		"else", "fallthrough", "for", "func", "go", "goto", "if", "import",
		"interface", "map", "package", "range", "return", "select", "struct",
		"switch", "type", "var":
		return true
	default:
		return false
	}
}
