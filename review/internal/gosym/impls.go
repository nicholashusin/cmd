package gosym

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/vcs"
)

// ImplDirection indicates the search direction: interface to concrete types, or concrete type to interfaces.
type ImplDirection string

const (
	ImplDirImplementations ImplDirection = "implementations" // interface -> concrete types/methods
	ImplDirImplements      ImplDirection = "implements"      // concrete type/method -> interfaces
)

// ImplGroup categorizes implementations for UI grouping and sorting.
type ImplGroup string

const (
	ImplGroupChange   ImplGroup = "change"
	ImplGroupPackage  ImplGroup = "package"
	ImplGroupRepo     ImplGroup = "repo"
	ImplGroupExternal ImplGroup = "external"
)

// Implementation describes a single implementation or implemented interface.
type Implementation struct {
	Name        string    `json:"name"`                   // e.g. "*Server", "(*Server).ServeHTTP", "Reader", "error"
	Path        string    `json:"path,omitempty"`         // repo-relative slash path (when InRepo)
	Display     string    `json:"display"`                // display path or "builtin"
	Pkg         string    `json:"pkg,omitempty"`          // package import path (when !InRepo && Pkg != "")
	PkgFile     string    `json:"pkg_file,omitempty"`     // package filename (when !InRepo && PkgFile != "")
	ExternalURL string    `json:"external_url,omitempty"` // e.g. "https://pkg.go.dev/builtin#error" when built-in error
	InRepo      bool      `json:"in_repo"`
	Group       ImplGroup `json:"group"` // "change", "package", "repo", or "external"
	Line        int       `json:"line,omitempty"`
	Col         int       `json:"col,omitempty"`
	UTF16Col    int       `json:"utf16_col"`
	LineHTML    string    `json:"line_html,omitempty"`
}

// ImplsResult contains all implementations or implemented interfaces found.
type ImplsResult struct {
	Found           bool             `json:"found"`
	Direction       ImplDirection    `json:"direction,omitempty"`
	SymbolName      string           `json:"symbol_name,omitempty"`
	Signature       string           `json:"signature,omitempty"`
	SignatureHTML   string           `json:"signature_html,omitempty"`
	Implementations []Implementation `json:"implementations,omitempty"`
	Truncated       bool             `json:"truncated,omitempty"`
}

var wellKnownStdlibMap = map[string]string{
	"Read":            "io",
	"Write":           "io",
	"Close":           "io",
	"Seek":            "io",
	"ReadFrom":        "io",
	"WriteTo":         "io",
	"ReadAt":          "io",
	"WriteAt":         "io",
	"ReadByte":        "io",
	"UnreadByte":      "io",
	"ReadRune":        "io",
	"WriteString":     "io",
	"Open":            "io/fs",
	"Stat":            "io/fs",
	"ReadDir":         "io/fs",
	"ReadFile":        "io/fs",
	"Sub":             "io/fs",
	"Glob":            "io/fs",
	"String":          "fmt",
	"GoString":        "fmt",
	"Format":          "fmt",
	"Scan":            "fmt",
	"ServeHTTP":       "net/http",
	"RoundTrip":       "net/http",
	"Hijack":          "net/http",
	"Flush":           "net/http",
	"MarshalJSON":     "encoding/json",
	"UnmarshalJSON":   "encoding/json",
	"MarshalText":     "encoding",
	"UnmarshalText":   "encoding",
	"MarshalBinary":   "encoding",
	"UnmarshalBinary": "encoding",
	"Len":             "sort",
	"Less":            "sort",
	"Swap":            "sort",
	"Deadline":        "context",
	"Done":            "context",
	"Err":             "context",
	"Value":           "context",
	"Set":             "flag",
	"Get":             "flag",
	"ReadMsg":         "net",
	"LocalAddr":       "net",
	"RemoteAddr":      "net",
	"SetDeadline":     "net",
	"Addr":            "net",
	"Network":         "net",
}

func safeImplements(V types.Type, T *types.Interface) (res bool) {
	defer func() {
		if r := recover(); r != nil {
			res = false
		}
	}()
	if V == nil || T == nil || T.NumMethods() == 0 || !T.IsMethodSet() {
		return false
	}
	V = instantiateIfGeneric(V)
	return types.Implements(V, T)
}

func isInterfaceType(t types.Type) bool {
	if t == nil {
		return false
	}
	for {
		if pt, ok := t.(*types.Pointer); ok {
			t = pt.Elem()
			continue
		}
		break
	}
	t = types.Unalias(t)
	_, ok := t.Underlying().(*types.Interface)
	return ok
}

func implGroupRank(g ImplGroup) int {
	switch g {
	case ImplGroupChange:
		return 0
	case ImplGroupPackage:
		return 1
	case ImplGroupRepo:
		return 2
	case ImplGroupExternal:
		return 3
	default:
		return 4
	}
}

func sortImpls(impls []Implementation) {
	slices.SortFunc(impls, func(a, b Implementation) int {
		if a.Group != b.Group {
			return implGroupRank(a.Group) - implGroupRank(b.Group)
		}
		if a.Display != b.Display {
			return strings.Compare(a.Display, b.Display)
		}
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return strings.Compare(a.Name, b.Name)
	})
}

func isExternalDepPackage(p *types.Package, target *resolvedTarget) bool {
	if p == nil || p == types.Unsafe || p.Scope() == nil || p.Scope().Len() == 0 || target == nil || target.loader == nil {
		return false
	}
	if p == target.rootPkg {
		return target.isDisk
	}
	if target.loader.revCache != nil && target.loader.revCache.has(p) {
		return false
	}
	if target.loader.diskCache != nil && target.loader.diskCache.has(p) {
		return true
	}
	_, isDisk, err := target.loader.resolvePackageDir(p.Path())
	return err == nil && isDisk
}

func collectExternalDepPackages(ctx context.Context, target *resolvedTarget, extraSeeds ...*types.Package) ([]*types.Package, error) {
	if target == nil || target.loader == nil {
		return nil, nil
	}

	visited := make(map[*types.Package]bool)
	var extPkgs []*types.Package
	var queue []*types.Package

	enqueue := func(p *types.Package) {
		if p == nil || visited[p] {
			return
		}
		visited[p] = true
		if isExternalDepPackage(p, target) {
			extPkgs = append(extPkgs, p)
			queue = append(queue, p)
		}
	}

	drainQueue := func() {
		for len(queue) > 0 {
			curr := queue[0]
			queue = queue[1:]

			imps := make([]*types.Package, len(curr.Imports()))
			copy(imps, curr.Imports())
			slices.SortFunc(imps, func(a, b *types.Package) int {
				if a == nil && b == nil {
					return 0
				}
				if a == nil {
					return -1
				}
				if b == nil {
					return 1
				}
				return strings.Compare(a.Path(), b.Path())
			})
			for _, imp := range imps {
				enqueue(imp)
			}
		}
	}

	// 1. Extra seeds (e.g. obj.Pkg() if external)
	for _, seed := range extraSeeds {
		enqueue(seed)
	}
	drainQueue()

	// 2. Direct imports of target.rootPkg
	if target.rootPkg != nil {
		direct := make([]*types.Package, len(target.rootPkg.Imports()))
		copy(direct, target.rootPkg.Imports())
		slices.SortFunc(direct, func(a, b *types.Package) int {
			if a == nil && b == nil {
				return 0
			}
			if a == nil {
				return -1
			}
			if b == nil {
				return 1
			}
			return strings.Compare(a.Path(), b.Path())
		})
		for _, imp := range direct {
			enqueue(imp)
		}
		drainQueue()
	}

	// 3. Repo-wide external imports
	if target.loader.req.Repo != nil {
		var repoImports []string
		var err error
		if target.loader.revCache != nil {
			repoImports, err = target.loader.revCache.getRepoImports(ctx, target.loader.req.Repo, target.rev)
		} else {
			repoImports, err = (*diskCache)(nil).getRepoImports(ctx, target.loader.req.Repo, target.rev)
		}
		if err != nil {
			return nil, err
		}

		// Concurrently pre-load uncached external imports into shared disk cache
		var toPreload []string
		for _, impPath := range repoImports {
			dir, isDisk, err := target.loader.resolvePackageDir(impPath)
			if err != nil || !isDisk {
				continue
			}
			key := target.loader.makeCacheKey(impPath, dir)
			if target.loader.diskCache != nil {
				if _, ok := target.loader.diskCache.get(key); !ok {
					toPreload = append(toPreload, impPath)
				}
			} else {
				toPreload = append(toPreload, impPath)
			}
		}

		if len(toPreload) > 0 {
			workers := min(len(toPreload), max(1, runtime.NumCPU()), 12)
			sem := make(chan struct{}, workers)
			var wg sync.WaitGroup
			for _, impPath := range toPreload {
				if ctx.Err() != nil {
					break
				}
				impPath := impPath
				ldr := target.loader.clone()
				wg.Add(1)
				sem <- struct{}{}
				go func(ldr *lookupLoader) {
					defer wg.Done()
					defer func() { <-sem }()
					_, _ = ldr.Import(impPath)
				}(ldr)
			}
			wg.Wait()
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		for _, impPath := range repoImports {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			_, isDisk, err := target.loader.resolvePackageDir(impPath)
			if err != nil || !isDisk {
				continue
			}
			pkg, err := target.loader.Import(impPath)
			if err != nil || pkg == nil {
				continue
			}
			enqueue(pkg)
		}
		drainQueue()
	}

	return extPkgs, nil
}

// Implementations finds bidirectional interface implementations for the symbol under cursor in req.
func (a *Analyzer) Implementations(ctx context.Context, req LookupRequest) (*ImplsResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	target, err := a.resolveTarget(ctx, req)
	if err != nil || target == nil || target.obj == nil {
		return &ImplsResult{Found: false}, nil
	}

	obj := unwrapOrigin(target.obj)

	// Built-in error special cases
	isBuiltinErrorType := (obj == types.Universe.Lookup("error"))
	isBuiltinErrorMethod := (obj.Pkg() == nil && obj.Name() == "Error")
	if obj.Pkg() == nil && !isBuiltinErrorType && !isBuiltinErrorMethod {
		return &ImplsResult{Found: false}, nil
	}

	var targetIface *types.Interface
	var targetIfaceName string
	var targetMethodName string

	var targetConcreteNamed *types.Named
	var targetConcreteMethod string
	var targetConcreteMethodPkg *types.Package

	direction := ImplDirection("")

	// Determine direction & targets
	if isBuiltinErrorType {
		targetIface = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
		targetIfaceName = "error"
		targetMethodName = ""
		direction = ImplDirImplementations
	} else if isBuiltinErrorMethod {
		targetIface = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
		targetIfaceName = "error"
		targetMethodName = "Error"
		direction = ImplDirImplementations
	} else if tn, ok := obj.(*types.TypeName); ok {
		t := types.Unalias(tn.Type())
		if iface, ok := t.Underlying().(*types.Interface); ok && iface.NumMethods() > 0 && iface.IsMethodSet() {
			targetIface = iface
			targetIfaceName = tn.Name()
			targetMethodName = ""
			direction = ImplDirImplementations
		} else if named, ok := t.(*types.Named); ok && !isInterfaceType(named) {
			targetConcreteNamed = named
			targetConcreteMethod = ""
			direction = ImplDirImplements
		}
	} else if fn, ok := obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			recvType := sig.Recv().Type()
			for {
				if pt, ok := recvType.(*types.Pointer); ok {
					recvType = pt.Elem()
					continue
				}
				break
			}
			recvType = types.Unalias(recvType)
			if iface, ok := recvType.Underlying().(*types.Interface); ok && iface.NumMethods() > 0 && iface.IsMethodSet() {
				targetIface = iface
				targetIfaceName = recvBaseTypeName(sig.Recv().Type())
				targetMethodName = fn.Name()
				direction = ImplDirImplementations
			} else if named, ok := recvType.(*types.Named); ok && !isInterfaceType(named) {
				targetConcreteNamed = named
				targetConcreteMethod = fn.Name()
				targetConcreteMethodPkg = fn.Pkg()
				direction = ImplDirImplements
			}
		}
	}

	if direction == "" {
		return &ImplsResult{Found: false}, nil
	}

	sig := formatSignature(obj, target.rootPkg)
	if isBuiltinErrorType {
		sig = "type error interface { Error() string }"
	} else if isBuiltinErrorMethod {
		sig = "func (error).Error() string"
	}

	defPos := target.loader.positionFor(obj, target.rootPkg)
	defCleanFile := ""
	if defPos.Filename != "" {
		defCleanFile = filepath.Clean(defPos.Filename)
	}
	defLine := defPos.Line
	defCol := defPos.Column
	targetID := resolveTargetIdentity(obj, target, defCleanFile, defLine, defCol, req.RepoRoot)

	var implsCacheK implsCacheKey
	var canCacheImpls bool
	if target.loader.revCache != nil && !targetID.isLocal && isImmutableRev(req.BaseRev) && isImmutableRev(target.rev) {
		canCacheImpls = true
		implsCacheK = implsCacheKey{
			baseRev:          req.BaseRev,
			targetRev:        target.rev,
			direction:        string(direction),
			targetID:         targetID,
			defCleanFile:     defCleanFile,
			defLine:          defLine,
			defCol:           defCol,
			targetMethodName: targetMethodName,
		}
		if cached, ok := target.loader.revCache.implsCache.get(implsCacheK); ok {
			return cached, nil
		}
	}

	res := &ImplsResult{
		Found:         true,
		Direction:     direction,
		SymbolName:    obj.Name(),
		Signature:     sig,
		SignatureHTML: highlightSignature(sig),
	}

	// Identify changed files in the diff
	changedFiles := make(map[string]bool)
	if req.Repo != nil && req.BaseRev != "" && req.TargetRev != "" {
		if target.loader.revCache != nil {
			changedFiles = target.loader.revCache.getChangedFiles(req.Repo, req.BaseRev, req.TargetRev)
		} else {
			if files, err := req.Repo.Files(req.BaseRev, req.TargetRev); err == nil {
				for _, f := range files {
					if f != nil && f.Status != vcs.FileDeleted {
						changedFiles[f.Path] = true
					}
				}
			}
		}
	}

	changedDirs := make(map[string]bool)
	for f := range changedFiles {
		changedDirs[repoDirOf(f)] = true
	}

	declDir := target.dir

	seen := make(map[string]bool)
	const maxImpls = 200

	addImpl := func(impl Implementation) bool {
		k := fmt.Sprintf("%s:%d:%s", impl.Display, impl.Line, impl.Name)
		if seen[k] {
			return true
		}
		seen[k] = true
		res.Implementations = append(res.Implementations, impl)
		if len(res.Implementations) >= maxImpls {
			res.Truncated = true
			return false
		}
		return true
	}

	var linesCacheMu sync.Mutex
	repoLinesCache := make(map[string][]string)
	getRepoLines := func(relPath string) []string {
		linesCacheMu.Lock()
		if lines, ok := repoLinesCache[relPath]; ok {
			linesCacheMu.Unlock()
			return lines
		}
		linesCacheMu.Unlock()

		if req.Repo != nil {
			if b, err := req.Repo.Content(target.rev, relPath); err == nil {
				lines := diff.SplitLines(b)
				linesCacheMu.Lock()
				repoLinesCache[relPath] = lines
				linesCacheMu.Unlock()
				return lines
			}
		}
		return nil
	}

	diskLinesCache := make(map[string][]string)
	getDiskLines := func(filename string) []string {
		linesCacheMu.Lock()
		if lines, ok := diskLinesCache[filename]; ok {
			linesCacheMu.Unlock()
			return lines
		}
		linesCacheMu.Unlock()

		if fileExists(filename) {
			if b, err := os.ReadFile(filename); err == nil {
				lines := diff.SplitLines(b)
				linesCacheMu.Lock()
				diskLinesCache[filename] = lines
				linesCacheMu.Unlock()
				return lines
			}
		}
		return nil
	}

	finishImpls := func() (*ImplsResult, error) {
		sortImpls(res.Implementations)
		if canCacheImpls && res != nil {
			target.loader.revCache.implsCache.put(implsCacheK, res)
		}
		return cloneImplsResult(res), nil
	}

	resolvePos := func(obj types.Object, pkg *types.Package, localFset *token.FileSet) token.Position {
		return resolveObjectPosition(obj, pkg, localFset, target.loader, target.rootPkg)
	}

	// DIRECTION 1: Interface Type or Interface Method -> Concrete Implementations
	if direction == ImplDirImplementations {
		if req.Repo != nil {
			var methodNames []string
			for i := 0; i < targetIface.NumMethods(); i++ {
				methodNames = append(methodNames, targetIface.Method(i).Name())
			}

			var goFiles []string
			if target.loader.revCache != nil {
				goFiles, _ = target.loader.revCache.getGoFiles(req.Repo, target.rev)
			} else {
				goFiles, _ = req.Repo.ListGoFiles(target.rev)
			}

			var fileIdx *repoFileIndex
			if target.loader.revCache != nil {
				fileIdx, _ = target.loader.revCache.getRepoFileIndex(ctx, req.Repo, target.rev)
			}

			// Candidate directory pre-filtering: check candidate directories without reading unnecessary contents
			candDirSet := make(map[string]bool)
			if fileIdx != nil {
				if len(goFiles) <= 1500 {
					for d := range fileIdx.dirHasType {
						candDirSet[d] = true
					}
				} else if len(methodNames) == 0 && targetIfaceName == "" {
					for d := range fileIdx.dirHasType {
						candDirSet[d] = true
					}
				} else {
					type bloomCand struct {
						dir  string
						path string
						oid  string
					}
					var bloomFiles []bloomCand
					for d := range fileIdx.dirHasType {
						fileIndices := fileIdx.dirFiles[d]
						for _, fi := range fileIndices {
							f := &fileIdx.files[fi]
							match := false
							if targetIfaceName != "" && fileIdx.mayContainIdent(f.path, targetIfaceName) {
								match = true
							} else {
								for _, m := range methodNames {
									if fileIdx.mayContainIdent(f.path, m) {
										match = true
										break
									}
								}
							}
							if match {
								bloomFiles = append(bloomFiles, bloomCand{dir: d, path: f.path, oid: f.oid})
							}
						}
					}

					if len(bloomFiles) > 0 {
						bloomPaths := make([]string, len(bloomFiles))
						bloomOIDs := make([]string, len(bloomFiles))
						for i, bc := range bloomFiles {
							bloomPaths[i] = bc.path
							bloomOIDs[i] = bc.oid
						}
						contents := readRepoFilesBatch(req.Repo, target.rev, bloomPaths, bloomOIDs)
						for _, bc := range bloomFiles {
							if candDirSet[bc.dir] {
								continue
							}
							b := contents[bc.path]
							if len(b) == 0 {
								continue
							}
							if targetIfaceName != "" && containsGoIdent(b, targetIfaceName) {
								candDirSet[bc.dir] = true
								continue
							}
							for _, m := range methodNames {
								if containsGoIdent(b, m) {
									candDirSet[bc.dir] = true
									break
								}
							}
						}
					}
				}
			} else {
				dirHasType := make(map[string]bool)
				dirHasMethodOrName := make(map[string]bool)
				for _, fPath := range goFiles {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					d := repoDirOf(fPath)
					if candDirSet[d] {
						continue
					}

					needType := !dirHasType[d]
					needMethod := len(goFiles) > 1500 && !dirHasMethodOrName[d]

					b, err := req.Repo.Content(target.rev, fPath)
					if err != nil || len(b) == 0 {
						continue
					}

					if needType && containsGoIdent(b, "type") {
						dirHasType[d] = true
					}
					if needMethod {
						if targetIfaceName != "" && containsGoIdent(b, targetIfaceName) {
							dirHasMethodOrName[d] = true
						} else {
							for _, m := range methodNames {
								if containsGoIdent(b, m) {
									dirHasMethodOrName[d] = true
									break
								}
							}
						}
					}

					hasType := dirHasType[d]
					if hasType && (len(goFiles) <= 1500 || dirHasMethodOrName[d]) {
						candDirSet[d] = true
					}
				}
			}

			var candDirs []string
			for d := range candDirSet {
				candDirs = append(candDirs, d)
			}
			slices.Sort(candDirs)

			// Prioritize candidate directories: changed directories first, then declDir, then others
			slices.SortStableFunc(candDirs, func(a, b string) int {
				pA := 2
				if changedDirs[a] {
					pA = 0
				} else if a == declDir || (declDir == "" && a == "") {
					pA = 1
				}

				pB := 2
				if changedDirs[b] {
					pB = 0
				} else if b == declDir || (declDir == "" && b == "") {
					pB = 1
				}

				return pA - pB
			})

			// Search candidate directories concurrently with bounded parallelism
			perDirImpls := make([][]Implementation, len(candDirs))
			if len(candDirs) > 0 {
				workers := min(len(candDirs), max(1, runtime.NumCPU()), 12)
				sem := make(chan struct{}, workers)
				var wg sync.WaitGroup

				for idx, d := range candDirs {
					if ctx.Err() != nil {
						break
					}
					idx, d := idx, d
					ldr := target.loader.clone()
					wg.Add(1)
					sem <- struct{}{}
					go func(ldr *lookupLoader) {
						defer wg.Done()
						defer func() { <-sem }()

						if ctx.Err() != nil {
							return
						}

						fset, pkgs, err := loadRepoPackages(ctx, ldr, target.rev, d)
						if err != nil || len(pkgs) == 0 {
							return
						}

						var dirImpls []Implementation
						for _, pkg := range pkgs {
							if ctx.Err() != nil {
								return
							}
							scope := pkg.Scope()
							for _, name := range scope.Names() {
								obj := scope.Lookup(name)
								tn, ok := obj.(*types.TypeName)
								if !ok || tn.IsAlias() {
									continue
								}
								named, ok := tn.Type().(*types.Named)
								if !ok || isInterfaceType(named) {
									continue
								}

								ptrImpl := safeImplements(types.NewPointer(named), targetIface)
								valImpl := safeImplements(named, targetIface)
								if !ptrImpl && !valImpl {
									continue
								}

								recvDisplay := named.Obj().Name()
								if ptrImpl && !valImpl {
									recvDisplay = "*" + named.Obj().Name()
								}

								if targetMethodName == "" {
									pos := resolveObjectPosition(named.Obj(), pkg, fset, ldr, target.rootPkg)
									relPath := cleanRepoRel(pos.Filename, req.RepoRoot)
									var lines []string
									if b, err := req.Repo.Content(target.rev, relPath); err == nil {
										lines = diff.SplitLines(b)
									}
									group := ImplGroupRepo
									if changedFiles[relPath] {
										group = ImplGroupChange
									} else if d == declDir {
										group = ImplGroupPackage
									}
									impl := Implementation{
										Name:     recvDisplay,
										Path:     relPath,
										Display:  relPath,
										InRepo:   true,
										Group:    group,
										Line:     pos.Line,
										Col:      pos.Column,
										UTF16Col: byteColToUTF16Col(lines, pos.Line, pos.Column),
										LineHTML: formatLineHTML(lines, pos.Line),
									}
									dirImpls = append(dirImpls, impl)
								} else {
									mset := safeMethodSet(types.NewPointer(named))
									if mset == nil {
										continue
									}
									var concreteMethodObj types.Object
									for i := 0; i < mset.Len(); i++ {
										m := mset.At(i)
										if m.Obj().Name() == targetMethodName {
											concreteMethodObj = m.Obj()
											if fn, ok := concreteMethodObj.(*types.Func); ok && fn.Origin() != nil {
												concreteMethodObj = fn.Origin()
											}
											break
										}
									}
									if concreteMethodObj == nil {
										continue
									}

									pos := resolveObjectPosition(concreteMethodObj, pkg, fset, ldr, target.rootPkg)
									relPath := cleanRepoRel(pos.Filename, req.RepoRoot)
									inRepo := (relPath != "" && !strings.HasPrefix(relPath, ".."))
									var lines []string
									if inRepo {
										if b, err := req.Repo.Content(target.rev, relPath); err == nil {
											lines = diff.SplitLines(b)
										}
									} else if fileExists(pos.Filename) {
										if b, err := os.ReadFile(pos.Filename); err == nil {
											lines = diff.SplitLines(b)
										}
									}
									group := ImplGroupRepo
									if inRepo {
										if changedFiles[relPath] {
											group = ImplGroupChange
										} else if d == declDir {
											group = ImplGroupPackage
										}
									} else {
										group = ImplGroupExternal
									}
									disp := relPath
									if !inRepo {
										disp = computeDisplayPath(pos.Filename, ldr.env)
									}
									methodDisplayName := fmt.Sprintf("(%s).%s", recvDisplay, targetMethodName)
									impl := Implementation{
										Name:     methodDisplayName,
										Path:     relPath,
										Display:  disp,
										InRepo:   inRepo,
										Group:    group,
										Line:     pos.Line,
										Col:      pos.Column,
										UTF16Col: byteColToUTF16Col(lines, pos.Line, pos.Column),
										LineHTML: formatLineHTML(lines, pos.Line),
									}
									if !inRepo && concreteMethodObj.Pkg() != nil {
										impl.Pkg = concreteMethodObj.Pkg().Path()
										impl.PkgFile = filepath.Base(pos.Filename)
									}
									dirImpls = append(dirImpls, impl)
								}
							}
						}
						perDirImpls[idx] = dirImpls
					}(ldr)
				}
				wg.Wait()

				if ctx.Err() != nil {
					return nil, ctx.Err()
				}

				for _, dirImpls := range perDirImpls {
					for _, impl := range dirImpls {
						if !addImpl(impl) {
							break
						}
					}
					if res.Truncated {
						break
					}
				}
			}
		}

		if !res.Truncated {
			extPkgs, err := collectExternalDepPackages(ctx, target, obj.Pkg())
			if err != nil {
				return nil, err
			}
			diskFileLines := make(map[string][]string)
			getDiskLines := func(filename string) []string {
				if lines, ok := diskFileLines[filename]; ok {
					return lines
				}
				var lines []string
				if b, err := os.ReadFile(filename); err == nil {
					lines = diff.SplitLines(b)
				}
				diskFileLines[filename] = lines
				return lines
			}
			for _, extPkg := range extPkgs {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if res.Truncated {
					break
				}
				scope := extPkg.Scope()
				for _, name := range scope.Names() {
					if res.Truncated {
						break
					}
					tn, ok := scope.Lookup(name).(*types.TypeName)
					if !ok || tn == nil || tn.IsAlias() {
						continue
					}
					t := types.Unalias(tn.Type())
					named, ok := t.(*types.Named)
					if !ok || isInterfaceType(named) {
						continue
					}
					ptrImpl := safeImplements(types.NewPointer(named), targetIface)
					valImpl := safeImplements(named, targetIface)
					if !ptrImpl && !valImpl {
						continue
					}
					baseName := named.Obj().Name()
					recvDisplay := baseName
					if ptrImpl && !valImpl {
						recvDisplay = "*" + baseName
					}
					if targetMethodName == "" {
						pos := resolvePos(named.Obj(), extPkg, nil)
						if !pos.IsValid() || pos.Filename == "" || pos.Line <= 0 {
							continue
						}
						if req.Repo != nil {
							if rel := cleanRepoRel(pos.Filename, req.RepoRoot); rel != "" && !strings.HasPrefix(rel, "..") {
								continue
							}
						}
						lines := getDiskLines(pos.Filename)
						impl := Implementation{
							Name:     recvDisplay,
							Display:  computeDisplayPath(pos.Filename, target.loader.env),
							Pkg:      extPkg.Path(),
							PkgFile:  filepath.Base(pos.Filename),
							InRepo:   false,
							Group:    ImplGroupExternal,
							Line:     pos.Line,
							Col:      pos.Column,
							UTF16Col: byteColToUTF16Col(lines, pos.Line, pos.Column),
							LineHTML: formatLineHTML(lines, pos.Line),
						}
						if !addImpl(impl) {
							break
						}
					} else {
						mset := safeMethodSet(types.NewPointer(named))
						if mset == nil {
							continue
						}
						var concreteMethodObj types.Object
						for i := 0; i < mset.Len(); i++ {
							m := mset.At(i)
							if m.Obj().Name() == targetMethodName {
								targetIfaceMethodPkgMatches := (obj.Pkg() == nil || (m.Obj().Pkg() != nil && m.Obj().Pkg().Path() == obj.Pkg().Path()))
								if ast.IsExported(targetMethodName) || targetIfaceMethodPkgMatches {
									concreteMethodObj = m.Obj()
									if fn, ok := concreteMethodObj.(*types.Func); ok && fn.Origin() != nil {
										concreteMethodObj = fn.Origin()
									}
									break
								}
							}
						}
						if concreteMethodObj == nil {
							continue
						}
						pos := resolvePos(concreteMethodObj, extPkg, nil)
						if !pos.IsValid() || pos.Filename == "" || pos.Line <= 0 {
							continue
						}
						if req.Repo != nil {
							if rel := cleanRepoRel(pos.Filename, req.RepoRoot); rel != "" && !strings.HasPrefix(rel, "..") {
								continue
							}
						}
						lines := getDiskLines(pos.Filename)
						pkgPath := extPkg.Path()
						if concreteMethodObj.Pkg() != nil && concreteMethodObj.Pkg().Path() != "" {
							pkgPath = concreteMethodObj.Pkg().Path()
						}
						impl := Implementation{
							Name:     fmt.Sprintf("(%s).%s", recvDisplay, targetMethodName),
							Display:  computeDisplayPath(pos.Filename, target.loader.env),
							Pkg:      pkgPath,
							PkgFile:  filepath.Base(pos.Filename),
							InRepo:   false,
							Group:    ImplGroupExternal,
							Line:     pos.Line,
							Col:      pos.Column,
							UTF16Col: byteColToUTF16Col(lines, pos.Line, pos.Column),
							LineHTML: formatLineHTML(lines, pos.Line),
						}
						if !addImpl(impl) {
							break
						}
					}
				}
			}
		}

		return finishImpls()
	}

	// DIRECTION 2: Concrete Type or Concrete Method -> Interfaces Implemented
	mset := safeMethodSet(types.NewPointer(targetConcreteNamed))
	if mset == nil || mset.Len() == 0 {
		return finishImpls()
	}

	concreteMethodNames := make(map[string]bool)
	for i := 0; i < mset.Len(); i++ {
		concreteMethodNames[mset.At(i).Obj().Name()] = true
	}
	if targetConcreteMethod != "" && !concreteMethodNames[targetConcreteMethod] {
		return finishImpls()
	}

	// 1. Built-in error interface
	errIface := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	if safeImplements(targetConcreteNamed, errIface) || safeImplements(types.NewPointer(targetConcreteNamed), errIface) {
		if targetConcreteMethod == "" || targetConcreteMethod == "Error" {
			name := "error"
			if targetConcreteMethod == "Error" {
				name = "error.Error"
			}
			impl := Implementation{
				Name:        name,
				Display:     "builtin",
				ExternalURL: "https://pkg.go.dev/builtin#error",
				InRepo:      false,
				Group:       ImplGroupExternal,
				LineHTML:    `<span class="tok-kw">type</span> <span class="tok-typ">error</span> <span class="tok-kw">interface</span> { <span class="tok-id">Error</span>() <span class="tok-typ">string</span> }`,
			}
			if target.loader.env.goroot != "" {
				if bc := getParsedBuiltin(target.loader.env.goroot); bc != nil {
					bFset := bc.fset
					bAST := bc.ast
					bLines := bc.lines
				findErrorLoop:
					for _, decl := range bAST.Decls {
						gd, ok := decl.(*ast.GenDecl)
						if !ok {
							continue
						}
						for _, sp := range gd.Specs {
							ts, ok := sp.(*ast.TypeSpec)
							if !ok || ts.Name.Name != "error" {
								continue
							}
							if targetConcreteMethod == "Error" {
								if it, ok := ts.Type.(*ast.InterfaceType); ok && it.Methods != nil {
									for _, m := range it.Methods.List {
										for _, id := range m.Names {
											if id.Name == "Error" {
												p := bFset.Position(id.Pos())
												impl.Pkg = "builtin"
												impl.PkgFile = "builtin.go"
												impl.Display = "builtin/builtin.go"
												impl.Line = p.Line
												impl.Col = p.Column
												impl.UTF16Col = byteColToUTF16Col(bLines, p.Line, p.Column)
												impl.ExternalURL = ""
												impl.LineHTML = formatLineHTML(bLines, p.Line)
												break findErrorLoop
											}
										}
									}
								}
							} else {
								p := bFset.Position(ts.Name.Pos())
								impl.Pkg = "builtin"
								impl.PkgFile = "builtin.go"
								impl.Display = "builtin/builtin.go"
								impl.Line = p.Line
								impl.Col = p.Column
								impl.UTF16Col = byteColToUTF16Col(bLines, p.Line, p.Column)
								impl.ExternalURL = ""
								impl.LineHTML = formatLineHTML(bLines, p.Line)
								break findErrorLoop
							}
						}
					}
				}
			}
			addImpl(impl)
		}
	}

	// Helper to test iface and add implementation
	checkAndEmitInterface := func(iface *types.Interface, tn *types.TypeName, pkg *types.Package, localFset *token.FileSet, ldr *lookupLoader, emit func(Implementation)) {
		if iface == nil || tn == nil || iface.NumMethods() == 0 || !iface.IsMethodSet() {
			return
		}
		if !safeImplements(targetConcreteNamed, iface) && !safeImplements(types.NewPointer(targetConcreteNamed), iface) {
			return
		}
		if targetConcreteMethod != "" {
			var mObj types.Object
			mset := safeMethodSet(iface)
			if mset == nil {
				return
			}
			for i := 0; i < mset.Len(); i++ {
				sel := mset.At(i)
				if sel.Obj().Name() == targetConcreteMethod {
					if ast.IsExported(targetConcreteMethod) {
						mObj = sel.Obj()
						break
					}
					if sel.Obj().Pkg() != nil && targetConcreteMethodPkg != nil && sel.Obj().Pkg().Path() == targetConcreteMethodPkg.Path() {
						mObj = sel.Obj()
						break
					}
				}
			}
			if mObj == nil {
				return
			}
			pos := resolveObjectPosition(mObj, pkg, localFset, ldr, target.rootPkg)
			relPath := cleanRepoRel(pos.Filename, req.RepoRoot)
			inRepo := (relPath != "" && !strings.HasPrefix(relPath, ".."))

			var lines []string
			if inRepo {
				lines = getRepoLines(relPath)
			} else {
				lines = getDiskLines(pos.Filename)
			}

			group := ImplGroupExternal
			disp := ""
			if inRepo {
				disp = relPath
				if changedFiles[relPath] {
					group = ImplGroupChange
				} else if repoDirOf(relPath) == declDir {
					group = ImplGroupPackage
				} else {
					group = ImplGroupRepo
				}
			} else {
				disp = computeDisplayPath(pos.Filename, ldr.env)
			}

			name := fmt.Sprintf("%s.%s", tn.Name(), targetConcreteMethod)

			impl := Implementation{
				Name:     name,
				Path:     relPath,
				Display:  disp,
				InRepo:   inRepo,
				Group:    group,
				Line:     pos.Line,
				Col:      pos.Column,
				UTF16Col: byteColToUTF16Col(lines, pos.Line, pos.Column),
				LineHTML: formatLineHTML(lines, pos.Line),
			}
			if !inRepo && pkg != nil {
				impl.Pkg = pkg.Path()
				impl.PkgFile = filepath.Base(pos.Filename)
			}
			emit(impl)
		} else {
			pos := resolveObjectPosition(tn, pkg, localFset, ldr, target.rootPkg)
			relPath := cleanRepoRel(pos.Filename, req.RepoRoot)
			inRepo := (relPath != "" && !strings.HasPrefix(relPath, ".."))

			var lines []string
			if inRepo {
				lines = getRepoLines(relPath)
			} else {
				lines = getDiskLines(pos.Filename)
			}

			group := ImplGroupExternal
			disp := ""
			if inRepo {
				disp = relPath
				if changedFiles[relPath] {
					group = ImplGroupChange
				} else if repoDirOf(relPath) == declDir {
					group = ImplGroupPackage
				} else {
					group = ImplGroupRepo
				}
			} else {
				disp = computeDisplayPath(pos.Filename, ldr.env)
			}

			name := tn.Name()

			impl := Implementation{
				Name:     name,
				Path:     relPath,
				Display:  disp,
				InRepo:   inRepo,
				Group:    group,
				Line:     pos.Line,
				Col:      pos.Column,
				UTF16Col: byteColToUTF16Col(lines, pos.Line, pos.Column),
				LineHTML: formatLineHTML(lines, pos.Line),
			}
			if !inRepo && pkg != nil {
				impl.Pkg = pkg.Path()
				impl.PkgFile = filepath.Base(pos.Filename)
			}
			emit(impl)
		}
	}

	testAndAddInterface := func(iface *types.Interface, tn *types.TypeName, pkg *types.Package, localFset *token.FileSet) {
		checkAndEmitInterface(iface, tn, pkg, localFset, target.loader, func(impl Implementation) {
			addImpl(impl)
		})
	}

	// 2. Duck-typing stdlib discovery
	visitedPkgs := make(map[*types.Package]bool)
	importedStd := make(map[string]bool)
	for mName := range concreteMethodNames {
		if stdPkg, ok := wellKnownStdlibMap[mName]; ok && !importedStd[stdPkg] {
			importedStd[stdPkg] = true
			if p, err := target.loader.Import(stdPkg); err == nil && p != nil {
				visitedPkgs[p] = true
				scope := p.Scope()
				for _, name := range scope.Names() {
					obj := scope.Lookup(name)
					if tn, ok := obj.(*types.TypeName); ok {
						if iface, ok := types.Unalias(tn.Type()).Underlying().(*types.Interface); ok {
							testAndAddInterface(iface, tn, p, nil)
						}
					}
				}
			}
		}
	}

	// 3. Transitive imports of target.rootPkg
	var queue []*types.Package
	if target.rootPkg != nil {
		queue = append(queue, target.rootPkg)
		visitedPkgs[target.rootPkg] = true
	}

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		scope := curr.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			if tn, ok := obj.(*types.TypeName); ok {
				if iface, ok := types.Unalias(tn.Type()).Underlying().(*types.Interface); ok {
					testAndAddInterface(iface, tn, curr, nil)
				}
			}
		}

		for _, imp := range curr.Imports() {
			if imp != nil && !visitedPkgs[imp] {
				visitedPkgs[imp] = true
				queue = append(queue, imp)
			}
		}
	}

	// 4. Repository interfaces
	if req.Repo != nil {
		var goFiles []string
		if target.loader.revCache != nil {
			goFiles, _ = target.loader.revCache.getGoFiles(req.Repo, target.rev)
		} else {
			goFiles, _ = req.Repo.ListGoFiles(target.rev)
		}

		var fileIdx *repoFileIndex
		if target.loader.revCache != nil {
			fileIdx, _ = target.loader.revCache.getRepoFileIndex(ctx, req.Repo, target.rev)
		}

		candDirSet := make(map[string]bool)
		if fileIdx != nil {
			if len(goFiles) <= 1500 {
				for d := range fileIdx.dirHasInterface {
					candDirSet[d] = true
				}
			} else if len(concreteMethodNames) == 0 {
				for d := range fileIdx.dirHasInterface {
					candDirSet[d] = true
				}
			} else {
				type bloomCand struct {
					dir  string
					path string
					oid  string
				}
				var bloomFiles []bloomCand
				for d := range fileIdx.dirHasInterface {
					fileIndices := fileIdx.dirFiles[d]
					for _, fi := range fileIndices {
						f := &fileIdx.files[fi]
						if !f.hasInterfaceKw {
							continue
						}
						match := false
						for mName := range concreteMethodNames {
							if fileIdx.mayContainIdent(f.path, mName) {
								match = true
								break
							}
						}
						if match {
							bloomFiles = append(bloomFiles, bloomCand{dir: d, path: f.path, oid: f.oid})
						}
					}
				}

				if len(bloomFiles) > 0 {
					bloomPaths := make([]string, len(bloomFiles))
					bloomOIDs := make([]string, len(bloomFiles))
					for i, bc := range bloomFiles {
						bloomPaths[i] = bc.path
						bloomOIDs[i] = bc.oid
					}
					contents := readRepoFilesBatch(req.Repo, target.rev, bloomPaths, bloomOIDs)
					for _, bc := range bloomFiles {
						if candDirSet[bc.dir] {
							continue
						}
						b := contents[bc.path]
						if len(b) == 0 {
							continue
						}
						if containsGoIdent(b, "type") && containsGoIdent(b, "interface") {
							for mName := range concreteMethodNames {
								if containsGoIdent(b, mName) {
									candDirSet[bc.dir] = true
									break
								}
							}
						}
					}
				}
			}
		} else {
			dirHasIface := make(map[string]bool)
			dirHasMethod := make(map[string]bool)
			for _, fPath := range goFiles {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				d := repoDirOf(fPath)
				if candDirSet[d] {
					continue
				}

				needIface := !dirHasIface[d]
				needMethod := len(goFiles) > 1500 && !dirHasMethod[d]

				b, err := req.Repo.Content(target.rev, fPath)
				if err != nil || len(b) == 0 {
					continue
				}
				if needIface && containsGoIdent(b, "type") && containsGoIdent(b, "interface") {
					dirHasIface[d] = true
				}
				if needMethod {
					for mName := range concreteMethodNames {
						if containsGoIdent(b, mName) {
							dirHasMethod[d] = true
							break
						}
					}
				}
				hasIface := dirHasIface[d]
				if hasIface && (len(goFiles) <= 1500 || dirHasMethod[d]) {
					candDirSet[d] = true
				}
			}
		}

		var candDirs []string
		for d := range candDirSet {
			candDirs = append(candDirs, d)
		}
		slices.Sort(candDirs)

		// Prioritize candidate directories: changed directories first, then declDir, then others
		slices.SortStableFunc(candDirs, func(a, b string) int {
			pA := 2
			if changedDirs[a] {
				pA = 0
			} else if a == declDir || (declDir == "" && a == "") {
				pA = 1
			}

			pB := 2
			if changedDirs[b] {
				pB = 0
			} else if b == declDir || (declDir == "" && b == "") {
				pB = 1
			}

			return pA - pB
		})

		// Search candidate directories concurrently with bounded parallelism
		perDirImpls := make([][]Implementation, len(candDirs))
		if len(candDirs) > 0 {
			workers := min(len(candDirs), max(1, runtime.NumCPU()), 12)
			sem := make(chan struct{}, workers)
			var wg sync.WaitGroup

			for idx, d := range candDirs {
				if ctx.Err() != nil {
					break
				}
				idx, d := idx, d
				ldr := target.loader.clone()
				wg.Add(1)
				sem <- struct{}{}
				go func(ldr *lookupLoader) {
					defer wg.Done()
					defer func() { <-sem }()

					if ctx.Err() != nil {
						return
					}

					fset, pkgs, err := loadRepoPackages(ctx, ldr, target.rev, d)
					if err != nil || len(pkgs) == 0 {
						return
					}

					var dirImpls []Implementation
					addDirImpl := func(impl Implementation) {
						dirImpls = append(dirImpls, impl)
					}

					for _, pkg := range pkgs {
						if ctx.Err() != nil {
							return
						}
						scope := pkg.Scope()
						for _, name := range scope.Names() {
							obj := scope.Lookup(name)
							tn, ok := obj.(*types.TypeName)
							if !ok {
								continue
							}
							if iface, ok := types.Unalias(tn.Type()).Underlying().(*types.Interface); ok {
								checkAndEmitInterface(iface, tn, pkg, fset, ldr, addDirImpl)
							}
						}
					}
					perDirImpls[idx] = dirImpls
				}(ldr)
			}
			wg.Wait()

			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			for _, dirImpls := range perDirImpls {
				for _, impl := range dirImpls {
					if !addImpl(impl) {
						break
					}
				}
				if res.Truncated {
					break
				}
			}
		}
	}

	// 5. External dependency interfaces across the repository
	if !res.Truncated {
		extPkgs, err := collectExternalDepPackages(ctx, target)
		if err != nil {
			return nil, err
		}
		for _, extPkg := range extPkgs {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if res.Truncated {
				break
			}
			if visitedPkgs[extPkg] {
				continue
			}
			visitedPkgs[extPkg] = true
			scope := extPkg.Scope()
			for _, name := range scope.Names() {
				if res.Truncated {
					break
				}
				obj := scope.Lookup(name)
				tn, ok := obj.(*types.TypeName)
				if !ok {
					continue
				}
				if iface, ok := types.Unalias(tn.Type()).Underlying().(*types.Interface); ok {
					testAndAddInterface(iface, tn, extPkg, nil)
				}
			}
		}
	}

	return finishImpls()
}

func loadRepoPackages(ctx context.Context, loader *lookupLoader, rev, dirInRepo string) (*token.FileSet, map[string]*types.Package, error) {
	if loader.revCache != nil {
		dirKey := loader.makeCacheKey("", dirInRepo)
		var doneCh chan struct{}
		for {
			loader.revCache.mu.Lock()
			if pkgs, ok := loader.revCache.dirPkgs[dirKey]; ok {
				loader.revCache.mu.Unlock()
				return nil, pkgs, nil
			}
			if waitCh, busy := loader.revCache.dirInflight[dirKey]; busy {
				loader.revCache.mu.Unlock()
				select {
				case <-waitCh:
					continue
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
			}
			doneCh = make(chan struct{})
			loader.revCache.dirInflight[dirKey] = doneCh
			loader.revCache.mu.Unlock()
			break
		}
		defer func() {
			loader.revCache.mu.Lock()
			delete(loader.revCache.dirInflight, dirKey)
			loader.revCache.mu.Unlock()
			close(doneCh)
		}()

		rootImportPath := cleanPathToImport(loader.mod, dirInRepo)
		pkgs := make(map[string]*types.Package)
		localFset := token.NewFileSet()

		type fileEntry struct {
			name    string
			fPath   string
			oid     string
			pkgName string
		}

		var fileEntries []fileEntry
		fileIdx, _ := loader.revCache.getRepoFileIndex(ctx, loader.req.Repo, rev)
		if fileIdx != nil {
			bctx, _ := loader.makeBuildContextWithCache()
			indices := fileIdx.dirFiles[dirInRepo]
			for _, idx := range indices {
				e := &fileIdx.files[idx]
				base := path.Base(e.path)
				if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") {
					continue
				}
				if !matchIndexedGoFile(bctx, loader.req.RepoRoot, e) {
					continue
				}
				fileEntries = append(fileEntries, fileEntry{
					name:    base,
					fPath:   e.path,
					oid:     e.oid,
					pkgName: e.pkgName,
				})
			}
		} else {
			names, err := loader.req.Repo.ListDir(rev, dirInRepo)
			if err != nil || len(names) == 0 {
				return nil, nil, err
			}
			slices.Sort(names)
			bctx, _ := loader.makeBuildContextWithCache()
			for _, name := range names {
				if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					continue
				}
				fPath := name
				if dirInRepo != "" && dirInRepo != "." {
					fPath = path.Join(dirInRepo, name)
				}
				absDir := filepath.Join(loader.req.RepoRoot, filepath.FromSlash(dirInRepo))
				if match, _ := bctx.MatchFile(absDir, name); !match {
					continue
				}
				fileEntries = append(fileEntries, fileEntry{
					name:  name,
					fPath: fPath,
				})
			}
		}

		if len(fileEntries) == 0 {
			return nil, nil, nil
		}

		hasInternalTestFiles := false
		for _, fe := range fileEntries {
			if strings.HasSuffix(fe.name, "_test.go") {
				if fe.pkgName == "" || !strings.HasSuffix(fe.pkgName, "_test") {
					hasInternalTestFiles = true
					break
				}
			}
		}

		nonTestPkgCount := 0
		knownNonTestPkg := ""
		for _, fe := range fileEntries {
			if fe.pkgName != "" && !strings.HasSuffix(fe.pkgName, "_test") {
				if knownNonTestPkg == "" {
					knownNonTestPkg = fe.pkgName
					nonTestPkgCount = 1
				} else if knownNonTestPkg != fe.pkgName {
					nonTestPkgCount++
				}
			}
		}

		var neededFiles []fileEntry
		if !hasInternalTestFiles && nonTestPkgCount == 1 && knownNonTestPkg != "" {
			pkgKey := loader.makeCacheKey(rootImportPath, dirInRepo)
			if cachedPkg, ok := loader.revCache.get(pkgKey); ok && cachedPkg != nil {
				pkgs[knownNonTestPkg] = cachedPkg
				for _, fe := range fileEntries {
					if fe.pkgName != knownNonTestPkg {
						neededFiles = append(neededFiles, fe)
					}
				}
			} else {
				neededFiles = fileEntries
			}
		} else {
			neededFiles = fileEntries
		}

		if len(neededFiles) > 0 {
			paths := make([]string, len(neededFiles))
			oids := make([]string, len(neededFiles))
			for i, nf := range neededFiles {
				paths[i] = nf.fPath
				oids[i] = nf.oid
			}
			contents := readRepoFilesBatch(loader.req.Repo, rev, paths, oids)

			type parsedFile struct {
				name    string
				astFile *ast.File
			}
			pkgFiles := make(map[string][]parsedFile)
			for _, nf := range neededFiles {
				data := contents[nf.fPath]
				if len(data) == 0 {
					continue
				}
				vPath := filepath.Join(loader.req.RepoRoot, filepath.FromSlash(nf.fPath))
				astF, err := parseGoFile(localFset, vPath, data, parser.SkipObjectResolution, true)
				if err != nil || astF == nil {
					continue
				}
				pkgName := astF.Name.Name
				pkgFiles[pkgName] = append(pkgFiles[pkgName], parsedFile{name: nf.name, astFile: astF})
			}

			actualNonTestCount := 0
			if pkgs[knownNonTestPkg] != nil {
				actualNonTestCount++
			}
			for pName := range pkgFiles {
				if !strings.HasSuffix(pName, "_test") {
					actualNonTestCount++
				}
			}

			var pkgNames []string
			for pName := range pkgFiles {
				pkgNames = append(pkgNames, pName)
			}
			slices.SortFunc(pkgNames, func(a, b string) int {
				aTest := strings.HasSuffix(a, "_test")
				bTest := strings.HasSuffix(b, "_test")
				if aTest != bTest {
					if !aTest {
						return -1
					}
					return 1
				}
				return strings.Compare(a, b)
			})

			for _, pkgName := range pkgNames {
				if ctx.Err() != nil || loader.ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				if pkgs[pkgName] != nil {
					continue
				}
				isExtTest := strings.HasSuffix(pkgName, "_test")
				impPath := rootImportPath
				if isExtTest {
					impPath += "_test"
				}

				var astFiles []*ast.File
				pkgHasTestFiles := false
				for _, pf := range pkgFiles[pkgName] {
					astFiles = append(astFiles, pf.astFile)
					if strings.HasSuffix(pf.name, "_test.go") {
						pkgHasTestFiles = true
					}
				}

				canUseRevCache := !isExtTest && actualNonTestCount == 1 && !pkgHasTestFiles
				var pkgKey diskCacheKey
				if canUseRevCache {
					pkgKey = loader.makeCacheKey(impPath, dirInRepo)
					if cachedPkg, ok := loader.revCache.get(pkgKey); ok && cachedPkg != nil {
						pkgs[pkgName] = cachedPkg
						continue
					}
				}

				conf := types.Config{
					IgnoreFuncBodies: true,
					FakeImportC:      true,
					Importer:         loader,
					Error:            func(error) {},
				}

				p, _ := conf.Check(impPath, localFset, astFiles, nil)
				if ctx.Err() != nil || loader.ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				if p != nil {
					loader.revCache.registerPkg(p)
					loader.revCache.registerPkgFset(p, localFset)
					if canUseRevCache {
						loader.revCache.put(pkgKey, p)
					}
					pkgs[pkgName] = p
				}
			}
		}

		if ctx.Err() != nil || loader.ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}

		loader.revCache.mu.Lock()
		loader.revCache.dirPkgs[dirKey] = pkgs
		loader.revCache.mu.Unlock()

		return localFset, pkgs, nil
	}

	names, err := loader.req.Repo.ListDir(rev, dirInRepo)
	if err != nil || len(names) == 0 {
		return nil, nil, err
	}
	slices.Sort(names)

	bctx, fc := loader.makeBuildContextWithCache()
	fset := token.NewFileSet()

	type fileEntry struct {
		name    string
		fPath   string
		astFile *ast.File
	}
	pkgFiles := make(map[string][]fileEntry)

	for _, name := range names {
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		fPath := name
		if dirInRepo != "" && dirInRepo != "." {
			fPath = path.Join(dirInRepo, name)
		}
		data, ok := loader.matchAndReadInRepo(bctx, fc, dirInRepo, name)
		if !ok || len(data) == 0 {
			continue
		}

		vPath := filepath.Join(loader.req.RepoRoot, filepath.FromSlash(fPath))
		astF, err := parseGoFile(fset, vPath, data, parser.SkipObjectResolution, true)
		if err != nil || astF == nil {
			continue
		}
		pkgName := astF.Name.Name
		pkgFiles[pkgName] = append(pkgFiles[pkgName], fileEntry{
			name:    name,
			fPath:   fPath,
			astFile: astF,
		})
	}

	rootImportPath := cleanPathToImport(loader.mod, dirInRepo)
	pkgs := make(map[string]*types.Package)

	var pkgNames []string
	for pName := range pkgFiles {
		pkgNames = append(pkgNames, pName)
	}
	slices.SortFunc(pkgNames, func(a, b string) int {
		aTest := strings.HasSuffix(a, "_test")
		bTest := strings.HasSuffix(b, "_test")
		if aTest != bTest {
			if !aTest {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})

	for _, pkgName := range pkgNames {
		if ctx.Err() != nil || loader.ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		entries := pkgFiles[pkgName]
		var astFiles []*ast.File
		for _, e := range entries {
			astFiles = append(astFiles, e.astFile)
		}

		conf := types.Config{
			IgnoreFuncBodies: true,
			FakeImportC:      true,
			Importer:         loader,
			Error:            func(error) {},
		}

		isExtTest := strings.HasSuffix(pkgName, "_test")
		impPath := rootImportPath
		if isExtTest {
			impPath += "_test"
		}
		p, _ := conf.Check(impPath, fset, astFiles, nil)
		if ctx.Err() != nil || loader.ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if p != nil {
			pkgs[pkgName] = p
		}
	}

	return fset, pkgs, nil
}
