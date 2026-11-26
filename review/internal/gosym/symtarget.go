package gosym

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"strings"

	"nicholas.sh/cmd/review/internal/vcs"
)

// SymbolTarget identifies a Go symbol semantically so it can be re-resolved
// across revisions (e.g. switching a popup between Base and Target) even when
// line numbers shift or declarations move between files in the same package.
type SymbolTarget struct {
	InRepo  bool   `json:"in_repo,omitempty"`
	Dir     string `json:"dir,omitempty"`      // repo-relative package directory (when InRepo)
	Pkg     string `json:"pkg,omitempty"`      // package import path
	PkgName string `json:"pkg_name,omitempty"` // Go package name (distinguishes foo vs foo_test)
	Name    string `json:"name,omitempty"`     // identifier name
	Owner   string `json:"owner,omitempty"`    // enclosing named type for methods/fields/interface methods
	Kind    string `json:"kind,omitempty"`     // "pkg", "builtin", "type", "func", "method", "field", "var", "const"
	IsTest  bool   `json:"is_test,omitempty"`  // true if declared in a *_test.go file
	Origin  string `json:"origin,omitempty"`   // optional in-repo path for go.mod resolution of external packages
}

func buildSymbolTarget(req LookupRequest, target *resolvedTarget) *SymbolTarget {
	if target == nil || target.obj == nil || target.isInitFunc {
		return nil
	}

	obj := unwrapOrigin(target.obj)
	if obj == nil {
		return nil
	}

	// 1. Built-ins and universe objects (e.g. error, error.Error, append, make)
	if obj.Pkg() == nil {
		name := obj.Name()
		if name == "" {
			return nil
		}
		owner := ""
		if name == "Error" {
			owner = "error"
		}
		return &SymbolTarget{
			Kind:  "builtin",
			Name:  name,
			Owner: owner,
		}
	}

	// 2. Package names (imports and package clauses)
	if pkgName, ok := obj.(*types.PkgName); ok {
		imported := pkgName.Imported()
		if imported == nil || imported.Path() == "C" {
			return nil
		}
		if !target.isPkgClause && (imported.Path() == "" || imported.Path() == ".") {
			return nil
		}

		pName := imported.Name()
		if pName == "" {
			pName = guessPkgName(imported.Path())
		}

		inRepo := false
		dir := ""
		if target.isPkgClause {
			inRepo = !target.isDisk
			if inRepo {
				dir = target.dir
			}
		} else if target.loader != nil {
			if d, isDisk, err := target.loader.resolvePackageDir(imported.Path()); err == nil && !isDisk {
				inRepo = true
				dir = d
			}
		}

		st := &SymbolTarget{
			InRepo:  inRepo,
			Dir:     dir,
			Pkg:     imported.Path(),
			PkgName: pName,
			Name:    pName,
			Kind:    "pkg",
			IsTest:  target.isPkgClause && strings.HasSuffix(target.cleanPath, "_test.go"),
		}
		if !inRepo && req.FilePath != "" {
			st.Origin = filepath.ToSlash(req.FilePath)
		}
		return st
	}

	// 3. Declared package-level symbols, methods, and struct fields
	var kind, owner string
	switch v := obj.(type) {
	case *types.TypeName:
		if v.Parent() != v.Pkg().Scope() {
			return nil
		}
		kind = "type"
	case *types.Const:
		if v.Parent() != v.Pkg().Scope() {
			return nil
		}
		kind = "const"
	case *types.Var:
		if v.IsField() {
			owner = findDeclaringStructName(v)
			if owner == "" {
				return nil
			}
			kind = "field"
		} else {
			if v.Parent() != v.Pkg().Scope() {
				return nil
			}
			kind = "var"
		}
	case *types.Func:
		if sig, ok := v.Type().(*types.Signature); ok && sig.Recv() != nil {
			owner = recvBaseTypeName(sig.Recv().Type())
			if owner == "" {
				return nil
			}
			kind = "method"
		} else {
			if v.Parent() != v.Pkg().Scope() {
				return nil
			}
			kind = "func"
		}
	default:
		return nil
	}

	inRepo := false
	dir := ""
	isTest := false
	if target.loader != nil {
		pos := target.loader.positionFor(obj, target.rootPkg)
		if pos.IsValid() && req.RepoRoot != "" {
			cleanRepo := filepath.Clean(req.RepoRoot)
			if rel, err := filepath.Rel(cleanRepo, pos.Filename); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
				inRepo = true
				slashRel := filepath.ToSlash(rel)
				dir = path.Dir(slashRel)
				if dir == "." {
					dir = ""
				}
				isTest = strings.HasSuffix(slashRel, "_test.go")
			}
		}
	}

	st := &SymbolTarget{
		InRepo:  inRepo,
		Dir:     dir,
		Pkg:     obj.Pkg().Path(),
		PkgName: obj.Pkg().Name(),
		Name:    obj.Name(),
		Owner:   owner,
		Kind:    kind,
		IsTest:  isTest,
	}
	if !inRepo && req.FilePath != "" {
		st.Origin = filepath.ToSlash(req.FilePath)
	}
	return st
}

func (a *Analyzer) resolveTargetBySymbol(ctx context.Context, req LookupRequest, rev string) (*resolvedTarget, error) {
	st := req.Symbol
	if st == nil || st.Kind == "" || st.Name == "" {
		return nil, nil
	}

	revCache := a.getRevCache(req.RepoRoot, rev)
	if revCache != nil && req.Repo != nil && vcs.IsImmutableHexRev(rev) {
		if revCache.peekRepoFileIndex() == nil && revCache.hasPrevRepoFileIndex() {
			_, _ = revCache.getRepoFileIndex(ctx, req.Repo, rev)
		}
	}

	modHint := st.Origin
	if modHint == "" && st.InRepo && st.Dir != "" {
		modHint = path.Join(st.Dir, "_")
	}
	mod := findModuleInfo(req.Repo, rev, modHint, revCache)
	loader := newLookupLoader(ctx, req, mod, a.diskCache, revCache)
	if a.crossRevPkgs != nil {
		loader.crossRevPkgs = a.crossRevPkgs
	}
	if a.crossRevChecked != nil {
		loader.crossRevChecked = a.crossRevChecked
	}

	// 1. Builtins
	if st.Kind == "builtin" {
		var obj types.Object
		if st.Owner == "error" && st.Name == "Error" {
			if errObj := types.Universe.Lookup("error"); errObj != nil {
				if iface, ok := errObj.Type().Underlying().(*types.Interface); ok {
					for i := 0; i < iface.NumMethods(); i++ {
						if m := iface.Method(i); m.Name() == "Error" {
							obj = m
							break
						}
					}
				}
			}
		} else {
			obj = types.Universe.Lookup(st.Name)
		}
		if obj == nil {
			return nil, nil
		}
		dir := ""
		if loader.env.goroot != "" {
			dir = filepath.Join(loader.env.goroot, "src", "builtin")
		}
		return &resolvedTarget{
			rev:            rev,
			isChained:      true,
			isDisk:         true,
			dir:            dir,
			rootImportPath: "builtin",
			mod:            mod,
			loader:         loader,
			obj:            obj,
		}, nil
	}

	// 2. In-repo package or symbol
	if st.InRepo {
		if req.Repo == nil {
			return nil, nil
		}
		dirInRepo := path.Clean(filepath.ToSlash(st.Dir))
		if dirInRepo == "." {
			dirInRepo = ""
		}
		if strings.HasPrefix(dirInRepo, "..") || strings.HasPrefix(dirInRepo, "/") {
			return nil, nil
		}

		pkgName := st.PkgName
		isTest := st.IsTest || strings.HasSuffix(pkgName, "_test")
		isExternalTest := strings.HasSuffix(pkgName, "_test")
		rootImportPath := cleanPathToImport(mod, dirInRepo)
		if isExternalTest {
			rootImportPath += "_test"
		}

		var rootPkg *types.Package
		var info *types.Info
		var pkgASTFiles []*ast.File

		if loader.revCache != nil && pkgName != "" {
			if entry := loader.loadCheckedPackage(dirInRepo, pkgName, isTest); entry != nil && entry.pkg != nil {
				loader.fset = entry.fset
				rootPkg = entry.pkg
				info = entry.info
				pkgASTFiles = entryASTFiles(entry)
			}
		}

		if rootPkg == nil {
			names, err := req.Repo.ListDir(rev, dirInRepo)
			if err != nil || len(names) == 0 {
				return nil, nil
			}
			bctx, fc := loader.makeBuildContextWithCache()
			for _, name := range names {
				if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					continue
				}
				nameIsTest := strings.HasSuffix(name, "_test.go")
				if !isTest && nameIsTest {
					continue
				}
				sBytes, ok := loader.matchAndReadInRepo(bctx, fc, dirInRepo, name)
				if !ok || len(sBytes) == 0 {
					continue
				}
				relPath := name
				if dirInRepo != "" {
					relPath = path.Join(dirInRepo, name)
				}
				sVirt := filepath.Join(req.RepoRoot, filepath.FromSlash(relPath))
				sAST, err := parser.ParseFile(loader.fset, sVirt, sBytes, parser.SkipObjectResolution)
				if err != nil || sAST == nil || sAST.Name == nil {
					continue
				}
				if pkgName == "" {
					if !nameIsTest || !strings.HasSuffix(sAST.Name.Name, "_test") {
						pkgName = sAST.Name.Name
					}
				}
				if pkgName != "" && sAST.Name.Name != pkgName {
					continue
				}
				pkgASTFiles = append(pkgASTFiles, sAST)
			}
			if len(pkgASTFiles) == 0 {
				return nil, nil
			}

			if isExternalTest {
				basePkgImport := cleanPathToImport(mod, dirInRepo)
				if basePkg, err := loader.loadInRepoTestBasePackage(basePkgImport, dirInRepo); err == nil && basePkg != nil {
					loader.inRepoPkgs[basePkgImport] = basePkg
				}
			}

			info = &types.Info{
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
			rootPkg, _ = conf.Check(rootImportPath, loader.fset, pkgASTFiles, info)
			if rootPkg == nil {
				return nil, nil
			}
		}

		if st.Kind == "pkg" {
			pkgObj := types.NewPkgName(token.NoPos, rootPkg, rootPkg.Name(), rootPkg)
			return &resolvedTarget{
				rev:            rev,
				isPkgClause:    true,
				dir:            dirInRepo,
				rootImportPath: rootImportPath,
				mod:            mod,
				loader:         loader,
				pkgASTFiles:    pkgASTFiles,
				rootPkg:        rootPkg,
				info:           info,
				obj:            pkgObj,
			}, nil
		}

		obj := lookupSymbolInPkg(rootPkg, st)
		if obj == nil {
			return nil, nil
		}
		return &resolvedTarget{
			rev:            rev,
			dir:            dirInRepo,
			rootImportPath: rootImportPath,
			mod:            mod,
			loader:         loader,
			pkgASTFiles:    pkgASTFiles,
			rootPkg:        rootPkg,
			info:           info,
			obj:            obj,
		}, nil
	}

	// 3. External / stdlib package or symbol
	if !isSafeImportPath(st.Pkg) {
		return nil, nil
	}
	if st.Pkg == "unsafe" && st.Kind != "pkg" {
		if obj := lookupSymbolInPkg(types.Unsafe, st); obj != nil {
			dir := ""
			if loader.env.goroot != "" {
				dir = filepath.Join(loader.env.goroot, "src", "unsafe")
			}
			return &resolvedTarget{
				rev:            rev,
				isChained:      true,
				isDisk:         true,
				dir:            dir,
				rootImportPath: "unsafe",
				mod:            mod,
				loader:         loader,
				rootPkg:        types.Unsafe,
				obj:            obj,
			}, nil
		}
		return nil, nil
	}
	basePkgImport := strings.TrimSuffix(st.Pkg, "_test")
	dir, isDisk, err := loader.resolvePackageDir(basePkgImport)
	if err != nil {
		return nil, nil
	}
	if !isDisk {
		// Resolved back into the repo (e.g. a dependency moved into repo or vice versa).
		inRepoCopy := *st
		inRepoCopy.InRepo = true
		inRepoCopy.Dir = dir
		reqCopy := req
		reqCopy.Symbol = &inRepoCopy
		return a.resolveTargetBySymbol(ctx, reqCopy, rev)
	}

	if loader.env.gomodcache != "" && strings.HasPrefix(dir, filepath.Clean(loader.env.gomodcache)+string(filepath.Separator)) {
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

	if st.Kind == "pkg" {
		pName := st.PkgName
		if pName == "" {
			pName = guessPkgName(st.Pkg)
		}
		imported := types.NewPackage(st.Pkg, pName)
		pkgObj := types.NewPkgName(token.NoPos, nil, pName, imported)
		return &resolvedTarget{
			rev:            rev,
			isChained:      true,
			isDisk:         true,
			dir:            dir,
			rootImportPath: st.Pkg,
			mod:            mod,
			loader:         loader,
			obj:            pkgObj,
		}, nil
	}

	pkgName := st.PkgName
	if pkgName == "" {
		pkgName = guessPkgName(basePkgImport)
	}
	isTest := st.IsTest || strings.HasSuffix(pkgName, "_test")

	var rootPkg *types.Package
	var info *types.Info
	var pkgASTFiles []*ast.File

	if entry := loader.loadDiskCheckedPackage(st.Pkg, dir, pkgName, isTest); entry != nil && entry.pkg != nil {
		loader.fset = entry.fset
		rootPkg = entry.pkg
		info = entry.info
		pkgASTFiles = entryASTFiles(entry)
	}

	if rootPkg == nil {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, nil
		}
		bctx, fc := loader.makeBuildContextWithCache()
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			nameIsTest := strings.HasSuffix(name, "_test.go")
			if !isTest && nameIsTest {
				continue
			}
			data, ok := loader.matchAndReadDisk(bctx, fc, dir, name)
			if !ok || len(data) == 0 {
				continue
			}
			fullPath := filepath.Join(dir, name)
			sAST, err := parser.ParseFile(loader.fset, fullPath, data, parser.SkipObjectResolution)
			if err != nil || sAST == nil || sAST.Name == nil {
				continue
			}
			if pkgName != "" && sAST.Name.Name != pkgName {
				continue
			}
			pkgASTFiles = append(pkgASTFiles, sAST)
		}
		if len(pkgASTFiles) == 0 {
			return nil, nil
		}
		info = &types.Info{
			Uses:       make(map[*ast.Ident]types.Object),
			Defs:       make(map[*ast.Ident]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
			Implicits:  make(map[ast.Node]types.Object),
		}
		conf := types.Config{
			IgnoreFuncBodies: true,
			FakeImportC:      true,
			Importer:         loader,
			Error:            func(error) {},
		}
		rootPkg, _ = conf.Check(st.Pkg, loader.fset, pkgASTFiles, info)
		if rootPkg == nil {
			return nil, nil
		}
	}

	obj := lookupSymbolInPkg(rootPkg, st)
	if obj == nil {
		return nil, nil
	}
	return &resolvedTarget{
		rev:            rev,
		isChained:      true,
		isDisk:         true,
		dir:            dir,
		rootImportPath: st.Pkg,
		mod:            mod,
		loader:         loader,
		pkgASTFiles:    pkgASTFiles,
		rootPkg:        rootPkg,
		info:           info,
		obj:            obj,
	}, nil
}

func lookupSymbolInPkg(pkg *types.Package, st *SymbolTarget) types.Object {
	if pkg == nil || st == nil || st.Name == "" {
		return nil
	}
	if st.Owner == "" {
		obj := pkg.Scope().Lookup(st.Name)
		if obj == nil {
			return nil
		}
		switch st.Kind {
		case "type":
			if _, ok := obj.(*types.TypeName); !ok {
				return nil
			}
		case "func":
			fn, ok := obj.(*types.Func)
			if !ok {
				return nil
			}
			if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
				return nil
			}
		case "var":
			if _, ok := obj.(*types.Var); !ok {
				return nil
			}
		case "const":
			if _, ok := obj.(*types.Const); !ok {
				return nil
			}
		}
		return obj
	}

	ownerObj := pkg.Scope().Lookup(st.Owner)
	tn, ok := ownerObj.(*types.TypeName)
	if !ok || tn == nil {
		return nil
	}
	t := types.Unalias(tn.Type())
	switch st.Kind {
	case "field":
		if stct, ok := t.Underlying().(*types.Struct); ok {
			for i := 0; i < stct.NumFields(); i++ {
				f := stct.Field(i)
				if f.Name() == st.Name {
					return f
				}
			}
		}
		return nil
	case "method":
		if iface, ok := t.Underlying().(*types.Interface); ok {
			for i := 0; i < iface.NumExplicitMethods(); i++ {
				m := iface.ExplicitMethod(i)
				if m.Name() == st.Name {
					return m
				}
			}
			for i := 0; i < iface.NumMethods(); i++ {
				m := iface.Method(i)
				if m.Name() == st.Name {
					return m
				}
			}
			return nil
		}
		if named, ok := t.(*types.Named); ok {
			for i := 0; i < named.NumMethods(); i++ {
				m := named.Method(i)
				if m.Name() == st.Name {
					return m
				}
			}
		}
		return nil
	}
	return nil
}
