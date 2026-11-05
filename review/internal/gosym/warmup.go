package gosym

import (
	"context"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"strings"
	"time"

	"nicholas.sh/cmd/review/internal/vcs"
)

// WarmRequest specifies a repository revision and set of files to warm in background.
type WarmRequest struct {
	RepoRoot string
	Rev      string
	Files    []string // slash paths in repo
}

// SetVCSCache provides an externally managed vcs.Cache for the symbol analyzer and warmup goroutines.
func (a *Analyzer) SetVCSCache(vc *vcs.Cache) {
	a.warmMu.Lock()
	defer a.warmMu.Unlock()
	a.warmVCSCache = vc
	a.ownsVCSCache = false
}

// SetIdleCheck registers a callback that returns true only when the server is idle.
func (a *Analyzer) SetIdleCheck(fn func() bool) {
	if fn == nil {
		fn = func() bool { return true }
	}
	a.warmMu.Lock()
	a.idleCheck = fn
	a.warmMu.Unlock()
}

// StartBackgroundWarmup idempotently launches a single low-priority background goroutine
// for warming standard-library and repository symbol caches.
func (a *Analyzer) StartBackgroundWarmup() {
	a.warmMu.Lock()
	if a.warmStarted {
		a.warmMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan WarmRequest, 16)
	vc := a.warmVCSCache
	if vc == nil {
		vc = vcs.NewCache()
		a.warmVCSCache = vc
		a.ownsVCSCache = true
	}
	a.warmCancel = cancel
	a.warmCh = ch
	a.warmedDirs = make(map[string]struct{})
	a.warmStarted = true
	a.warmWG.Add(1)
	a.warmMu.Unlock()

	go a.runWarmupLoop(ctx, ch, vc)
}

// EnqueueWarm non-blockingly enqueues a warming request. If the warmup queue is full
// or warmup has not started or Rev is not an immutable hex hash, it returns immediately.
func (a *Analyzer) EnqueueWarm(req WarmRequest) {
	if req.RepoRoot == "" || !vcs.IsImmutableHexRev(req.Rev) {
		return
	}
	a.warmMu.Lock()
	started := a.warmStarted
	ch := a.warmCh
	a.warmMu.Unlock()
	if !started || ch == nil {
		return
	}
	select {
	case ch <- req:
	default:
	}
}

func (a *Analyzer) waitForIdle(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	for {
		if ctx.Err() != nil {
			return false
		}
		var isIdle bool
		a.warmMu.Lock()
		check := a.idleCheck
		a.warmMu.Unlock()
		if check != nil {
			isIdle = check()
		} else {
			isIdle = true
		}
		if isIdle {
			return true
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func (a *Analyzer) runWarmupLoop(ctx context.Context, ch <-chan WarmRequest, vc *vcs.Cache) {
	defer a.warmWG.Done()

	// Initial cold stdlib warmup
	if !a.waitForIdle(ctx) {
		return
	}
	env := getGoEnv()
	if env.goroot != "" {
		stdPkgs := []string{"errors", "io", "bytes", "strings", "sync", "time", "context", "os", "fmt"}
		for _, pkg := range stdPkgs {
			if !a.waitForIdle(ctx) {
				return
			}
			stdDir := filepath.Join(env.goroot, "src", filepath.FromSlash(pkg))
			if dirExists(stdDir) {
				loader := newLookupLoader(ctx, LookupRequest{
					RepoRoot: env.goroot,
				}, nil, a.diskCache, nil)
				_, _ = loader.loadDiskPackage(pkg, stdDir)
			}
		}
	}

	// Warm imports for requested repository revisions
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-ch:
			if !ok {
				return
			}
			a.warmChange(ctx, vc, req)
		}
	}
}

func (a *Analyzer) warmChange(ctx context.Context, vc *vcs.Cache, req WarmRequest) {
	if req.RepoRoot == "" || req.Rev == "" || len(req.Files) == 0 {
		return
	}

	uniqueDirs := make([]string, 0, 16)
	seenDirs := make(map[string]string) // dir -> sampleFile
	for _, f := range req.Files {
		fClean := path.Clean(strings.TrimPrefix(filepath.ToSlash(f), "/"))
		if fClean == "" || fClean == "." || !strings.HasSuffix(fClean, ".go") || strings.HasPrefix(fClean, "vendor/") || strings.Contains(fClean, "/vendor/") || isTestdataPath(fClean) {
			continue
		}
		d := path.Dir(fClean)
		if d == "." {
			d = ""
		}
		if _, ok := seenDirs[d]; !ok {
			seenDirs[d] = fClean
			uniqueDirs = append(uniqueDirs, d)
			if len(uniqueDirs) >= 16 {
				break
			}
		}
	}

	if len(uniqueDirs) == 0 {
		return
	}

	if !a.waitForIdle(ctx) {
		return
	}

	repo, err := vcs.Open(req.RepoRoot, vc)
	if err != nil {
		return
	}

	rc := a.getRevCache(req.RepoRoot, req.Rev)
	var fileIdx *repoFileIndex
	if rc != nil {
		if !a.waitForIdle(ctx) {
			return
		}
		fileIdx, _ = rc.getRepoFileIndex(ctx, repo, req.Rev)
	}

	for _, pkgDir := range uniqueDirs {
		sampleFile := seenDirs[pkgDir]
		warmKey := req.RepoRoot + "\x00" + req.Rev + "\x00" + pkgDir

		a.warmMu.Lock()
		if len(a.warmedDirs) > 512 {
			a.warmedDirs = make(map[string]struct{})
		}
		if _, warmed := a.warmedDirs[warmKey]; warmed {
			a.warmMu.Unlock()
			continue
		}
		a.warmedDirs[warmKey] = struct{}{}
		a.warmMu.Unlock()

		if !a.waitForIdle(ctx) {
			return
		}

		mod := findModuleInfo(repo, req.Rev, sampleFile, rc)

		uniqueImports := make(map[string]struct{})
		type pkgToWarm struct {
			name        string
			hasNonTest  bool
			hasTestFile bool
		}
		var pkgs []pkgToWarm

		if fileIdx != nil {
			if indices, ok := fileIdx.dirFiles[pkgDir]; ok {
				var needParsePaths []string
				for _, idx := range indices {
					entry := fileIdx.files[idx]
					if entry.importsExact {
						for _, imp := range entry.imports {
							if imp != "" && imp != "C" && imp != "unsafe" {
								uniqueImports[imp] = struct{}{}
							}
						}
					} else {
						needParsePaths = append(needParsePaths, entry.path)
					}
				}
				if len(needParsePaths) > 0 {
					fset := token.NewFileSet()
					for _, fPath := range needParsePaths {
						data, err := repo.Content(req.Rev, fPath)
						if err != nil || len(data) == 0 {
							continue
						}
						f, err := parser.ParseFile(fset, path.Base(fPath), data, parser.ImportsOnly)
						if err != nil || f == nil {
							continue
						}
						for _, imp := range f.Imports {
							if imp.Path != nil {
								p := strings.Trim(imp.Path.Value, `"`)
								if p != "" && p != "C" && p != "unsafe" {
									uniqueImports[p] = struct{}{}
								}
							}
						}
					}
				}
			}
			if pkgFilesMap, ok := fileIdx.dirPkgFiles[pkgDir]; ok {
				for pName, files := range pkgFilesMap {
					hasTest := false
					hasNonTest := false
					for _, f := range files {
						if strings.HasSuffix(f, "_test.go") {
							hasTest = true
						} else {
							hasNonTest = true
						}
					}
					pkgs = append(pkgs, pkgToWarm{name: pName, hasNonTest: hasNonTest, hasTestFile: hasTest})
				}
			}
		} else {
			names, err := repo.ListDir(req.Rev, pkgDir)
			if err != nil || len(names) == 0 {
				continue
			}
			type pkgTrack struct {
				hasNonTest bool
				hasTest    bool
			}
			pkgMap := make(map[string]*pkgTrack)
			fset := token.NewFileSet()
			for _, name := range names {
				if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					continue
				}
				filePath := name
				if pkgDir != "" && pkgDir != "." {
					filePath = path.Join(pkgDir, name)
				}
				data, err := repo.Content(req.Rev, filePath)
				if err != nil || len(data) == 0 {
					continue
				}
				isTest := strings.HasSuffix(name, "_test.go")
				imps, exact, pName := scanGoHeaderImports(data)
				if pName != "" {
					track := pkgMap[pName]
					if track == nil {
						track = &pkgTrack{}
						pkgMap[pName] = track
					}
					if isTest {
						track.hasTest = true
					} else {
						track.hasNonTest = true
					}
				}
				if exact {
					for _, imp := range imps {
						if imp != "" && imp != "C" && imp != "unsafe" {
							uniqueImports[imp] = struct{}{}
						}
					}
				} else {
					f, err := parser.ParseFile(fset, name, data, parser.ImportsOnly)
					if err == nil && f != nil {
						for _, imp := range f.Imports {
							if imp.Path != nil {
								p := strings.Trim(imp.Path.Value, `"`)
								if p != "" && p != "C" && p != "unsafe" {
									uniqueImports[p] = struct{}{}
								}
							}
						}
					}
				}
			}
			for pName, track := range pkgMap {
				pkgs = append(pkgs, pkgToWarm{name: pName, hasNonTest: track.hasNonTest, hasTestFile: track.hasTest})
			}
		}

		lReq := LookupRequest{
			Repo:      repo,
			RepoRoot:  req.RepoRoot,
			TargetRev: req.Rev,
			Side:      "R",
			FilePath:  sampleFile,
		}
		loader := newLookupLoader(ctx, lReq, mod, a.diskCache, rc)
		if a.crossRevPkgs != nil {
			loader.crossRevPkgs = a.crossRevPkgs
		}
		if a.crossRevChecked != nil {
			loader.crossRevChecked = a.crossRevChecked
		}

		for impPath := range uniqueImports {
			if !a.waitForIdle(ctx) {
				return
			}
			_, _ = loader.Import(impPath)
		}

		if rc != nil {
			for _, p := range pkgs {
				if p.hasNonTest {
					if !a.waitForIdle(ctx) {
						return
					}
					_ = loader.loadCheckedPackage(pkgDir, p.name, false)
				}
				if p.hasTestFile {
					if !a.waitForIdle(ctx) {
						return
					}
					_ = loader.loadCheckedPackage(pkgDir, p.name, true)
				}
			}
		}
	}
}
