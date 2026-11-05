package gosym

import (
	"go/ast"
	"go/scanner"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"nicholas.sh/cmd/review/internal/diff"
)

func hasPotentialDocLinkAtByteIndex(lineStr string, byteIdx int) bool {
	if byteIdx < 0 || byteIdx >= len(lineStr) {
		return false
	}
	openIdx := -1
	for i := byteIdx; i >= 0; i-- {
		if lineStr[i] == '[' {
			openIdx = i
			break
		}
		if i < byteIdx && lineStr[i] == ']' {
			return false
		}
	}
	if openIdx == -1 {
		return false
	}
	closeRel := strings.IndexByte(lineStr[openIdx+1:], ']')
	if closeRel == -1 {
		return false
	}
	closeIdx := openIdx + 1 + closeRel
	if byteIdx > closeIdx {
		return false
	}
	return diff.IsValidGoDocLinkTarget(lineStr[openIdx+1 : closeIdx])
}

func findCommentBodyAtByteIndex(lines []string, targetLine, byteIdx int) (bodyStart, bodyEnd int, ok bool) {
	if targetLine < 1 || targetLine > len(lines) {
		return 0, 0, false
	}
	lineStr := lines[targetLine-1]
	if byteIdx < 0 || byteIdx >= len(lineStr) {
		return 0, 0, false
	}

	var sb strings.Builder
	targetLineStart := 0
	for i := 0; i < targetLine; i++ {
		if i == targetLine-1 {
			targetLineStart = sb.Len()
		}
		sb.WriteString(lines[i])
		sb.WriteByte('\n')
	}
	targetLineEnd := targetLineStart + len(lineStr)
	targetOffset := targetLineStart + byteIdx
	srcBytes := []byte(sb.String())

	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(srcBytes))
	file.SetLinesForContent(srcBytes)

	var s scanner.Scanner
	s.Init(file, srcBytes, func(token.Position, string) {}, scanner.ScanComments)

	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.COMMENT {
			continue
		}
		commStart := file.Offset(pos)
		commEnd := commStart + len(lit)
		if commStart > targetOffset {
			break
		}
		if targetOffset < commStart || targetOffset >= commEnd {
			continue
		}

		if strings.HasPrefix(lit, "//") {
			bodyStart = (commStart + 2) - targetLineStart
			bodyEnd = commEnd - targetLineStart
		} else if strings.HasPrefix(lit, "/*") {
			if commStart >= targetLineStart {
				bodyStart = (commStart + 2) - targetLineStart
			} else {
				bodyStart = 0
			}
			if commEnd <= targetLineEnd && len(lit) >= 4 && strings.HasSuffix(lit, "*/") {
				bodyEnd = (commEnd - 2) - targetLineStart
			} else {
				bodyEnd = len(lineStr)
			}
		} else {
			return 0, 0, false
		}

		if bodyStart < 0 {
			bodyStart = 0
		}
		if bodyEnd > len(lineStr) {
			bodyEnd = len(lineStr)
		}
		if bodyStart > bodyEnd || byteIdx < bodyStart || byteIdx >= bodyEnd {
			return 0, 0, false
		}
		return bodyStart, bodyEnd, true
	}
	return 0, 0, false
}

func collectASTFiles(clickedAST *ast.File, pkgASTFiles []*ast.File) []*ast.File {
	allFiles := make([]*ast.File, 0, 1+len(pkgASTFiles))
	if clickedAST != nil {
		allFiles = append(allFiles, clickedAST)
	}
	for _, f := range pkgASTFiles {
		if f != nil && f != clickedAST {
			allFiles = append(allFiles, f)
		}
	}
	return allFiles
}

func isAllowedPointerDocObj(obj types.Object) bool {
	switch v := obj.(type) {
	case *types.TypeName:
		return true
	case *types.Func:
		sig, ok := v.Type().(*types.Signature)
		return ok && sig.Recv() != nil
	case *types.Var:
		return v.IsField()
	default:
		return false
	}
}

func lookupTypeOrMember(pkg *types.Package, recvName, memberName string) types.Object {
	if pkg == nil || pkg.Scope() == nil {
		return nil
	}
	tObj := pkg.Scope().Lookup(recvName)
	if tObj == nil {
		return nil
	}
	if memberName == "" {
		return tObj
	}
	tn, ok := tObj.(*types.TypeName)
	if !ok {
		return nil
	}
	obj, _, _ := types.LookupFieldOrMethod(tn.Type(), true, pkg, memberName)
	if fn, ok := obj.(*types.Func); ok {
		obj = fn.Origin()
	} else if v, ok := obj.(*types.Var); ok {
		obj = v.Origin()
	}
	return obj
}

func resolveDocPkgByName(pkgName string, clickedAST *ast.File, pkgASTFiles []*ast.File, rootPkg *types.Package, info *types.Info, loader *lookupLoader) *types.Package {
	allFiles := collectASTFiles(clickedAST, pkgASTFiles)

	for _, f := range allFiles {
		for _, spec := range f.Imports {
			if spec.Path == nil {
				continue
			}
			unquoted, err := strconv.Unquote(spec.Path.Value)
			if err != nil || unquoted == "" {
				continue
			}
			// Explicit alias: import myfmt "fmt" -> myfmt matches pkgName
			if spec.Name != nil && spec.Name.Name == pkgName {
				if imp, err := loader.Import(unquoted); err == nil && imp != nil {
					return imp
				}
			}
			// Implicits
			if info != nil {
				if impObj, ok := info.Implicits[spec].(*types.PkgName); ok && impObj != nil {
					if impObj.Name() == pkgName {
						return impObj.Imported()
					}
				}
			}
			// Direct import name check
			if imp, err := loader.Import(unquoted); err == nil && imp != nil && imp.Name() == pkgName {
				return imp
			}
		}
	}

	// 2. Check rootPkg.Imports()
	if rootPkg != nil {
		for _, imp := range rootPkg.Imports() {
			if imp != nil && imp.Name() == pkgName {
				return imp
			}
		}
	}

	// 3. Fallback for unimported standard library single-segment packages (e.g. io, os, fmt, net)
	if _, _, err := loader.resolvePackageDir(pkgName); err == nil {
		if pkg, err := loader.Import(pkgName); err == nil && pkg != nil && pkg.Name() != "" {
			return pkg
		}
	}

	return nil
}

func (a *Analyzer) resolveDocLinkTarget(
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
	targetLine, byteIdx int,
) (*resolvedTarget, error) {
	if targetLine < 1 || targetLine > len(lines) {
		return nil, nil
	}
	bodyStart, bodyEnd, ok := findCommentBodyAtByteIndex(lines, targetLine, byteIdx)
	if !ok {
		return nil, nil
	}
	lineStr := lines[targetLine-1]
	rawLink, ok := diff.ExtractGoDocLinkInRange(lineStr, bodyStart, bodyEnd, byteIdx)
	if !ok || rawLink == "" {
		return nil, nil
	}

	hadLeadingStar := strings.HasPrefix(rawLink, "*")
	s := strings.TrimPrefix(rawLink, "*")
	var obj types.Object

	if strings.Contains(s, "/") {
		impPath, recv, name, ok := diff.ParseGoDocSlashTarget(s)
		if !ok {
			return nil, nil
		}

		var targetPkg *types.Package
		if rootPkg != nil && impPath == strings.TrimSuffix(rootImportPath, "_test") && !strings.HasSuffix(rootPkg.Name(), "_test") {
			targetPkg = rootPkg
		} else if p := loader.inRepoPkgs[impPath]; p != nil {
			targetPkg = p
		} else {
			targetPkg, _ = loader.Import(impPath)
		}

		if targetPkg == nil {
			return nil, nil
		}

		if name == "" {
			if !hadLeadingStar {
				if targetPkg == rootPkg || loader.inRepoPkgs[impPath] != nil {
					obj = types.NewPkgName(token.NoPos, rootPkg, targetPkg.Name(), targetPkg)
				} else if _, _, err := loader.resolvePackageDir(impPath); err == nil {
					obj = types.NewPkgName(token.NoPos, rootPkg, targetPkg.Name(), targetPkg)
				}
			}
		} else if recv != "" {
			obj = lookupTypeOrMember(targetPkg, recv, name)
		} else {
			obj = targetPkg.Scope().Lookup(name)
		}
	} else {
		allFiles := collectASTFiles(clickedAST, pkgASTFiles)
		parts := strings.Split(s, ".")
		if len(parts) == 3 {
			targetPkg := resolveDocPkgByName(parts[0], clickedAST, pkgASTFiles, rootPkg, info, loader)
			if targetPkg != nil {
				obj = lookupTypeOrMember(targetPkg, parts[1], parts[2])
			}
		} else if len(parts) == 2 {
			// First try in-package Recv.Member on rootPkg
			if rootPkg != nil {
				obj = lookupTypeOrMember(rootPkg, parts[0], parts[1])
			}
			// If not found and rootImportPath ends in _test, check base package
			if obj == nil && strings.HasSuffix(rootImportPath, "_test") {
				basePkgImport := strings.TrimSuffix(rootImportPath, "_test")
				basePkg := loader.inRepoPkgs[basePkgImport]
				if basePkg == nil {
					if isDisk {
						basePkg, _ = loader.loadDiskTestBasePackage(basePkgImport, dir)
					} else {
						basePkg, _ = loader.loadInRepoTestBasePackage(basePkgImport, dir)
					}
					if basePkg != nil {
						loader.inRepoPkgs[basePkgImport] = basePkg
					}
				}
				if basePkg != nil {
					obj = lookupTypeOrMember(basePkg, parts[0], parts[1])
				}
			}
			// If still nil, check pkgName.Sym (including [*pkg.Type], validated below).
			if obj == nil {
				targetPkg := resolveDocPkgByName(parts[0], clickedAST, pkgASTFiles, rootPkg, info, loader)
				if targetPkg != nil {
					obj = targetPkg.Scope().Lookup(parts[1])
				}
			}
		} else if len(parts) == 1 {
			name := parts[0]
			if rootPkg != nil {
				obj = rootPkg.Scope().Lookup(name)
			}
			if obj == nil && strings.HasSuffix(rootImportPath, "_test") {
				basePkgImport := strings.TrimSuffix(rootImportPath, "_test")
				basePkg := loader.inRepoPkgs[basePkgImport]
				if basePkg == nil {
					if isDisk {
						basePkg, _ = loader.loadDiskTestBasePackage(basePkgImport, dir)
					} else {
						basePkg, _ = loader.loadInRepoTestBasePackage(basePkgImport, dir)
					}
					if basePkg != nil {
						loader.inRepoPkgs[basePkgImport] = basePkg
					}
				}
				if basePkg != nil {
					obj = basePkg.Scope().Lookup(name)
				}
			}
			// Dot-imported packages across package AST files
			if obj == nil {
				for _, f := range allFiles {
					for _, spec := range f.Imports {
						if spec.Name != nil && spec.Name.Name == "." && spec.Path != nil {
							unquoted, err := strconv.Unquote(spec.Path.Value)
							if err == nil {
								if dotPkg, err := loader.Import(unquoted); err == nil && dotPkg != nil {
									if dotObj := dotPkg.Scope().Lookup(name); dotObj != nil {
										obj = dotObj
										break
									}
								}
							}
						}
					}
					if obj != nil {
						break
					}
				}
			}
			// Check imported packages matching pkgName == name
			if obj == nil && !hadLeadingStar {
				for _, f := range allFiles {
					for _, spec := range f.Imports {
						if spec.Path != nil {
							unquoted, err := strconv.Unquote(spec.Path.Value)
							if err == nil {
								if spec.Name != nil && spec.Name.Name == name {
									if impPkg, err := loader.Import(unquoted); err == nil && impPkg != nil {
										obj = types.NewPkgName(spec.Pos(), rootPkg, impPkg.Name(), impPkg)
										break
									}
								}
								if impPkg, err := loader.Import(unquoted); err == nil && impPkg != nil && impPkg.Name() == name {
									obj = types.NewPkgName(spec.Pos(), rootPkg, impPkg.Name(), impPkg)
									break
								}
							}
						}
					}
					if obj != nil {
						break
					}
				}
				if obj == nil && rootPkg != nil {
					for _, imp := range rootPkg.Imports() {
						if imp != nil && imp.Name() == name {
							obj = types.NewPkgName(token.NoPos, rootPkg, imp.Name(), imp)
							break
						}
					}
				}
			}
			// Builtin Universe (error, any, comparable, append, etc.)
			if obj == nil {
				obj = types.Universe.Lookup(name)
			}
			// Fallback for stdlib single-segment package name (os, io, fmt, context, etc.)
			if obj == nil && !hadLeadingStar {
				if _, _, err := loader.resolvePackageDir(name); err == nil {
					if pkg, err := loader.Import(name); err == nil && pkg != nil && pkg.Name() == name {
						obj = types.NewPkgName(token.NoPos, rootPkg, pkg.Name(), pkg)
					}
				}
			}
		}
	}

	if obj != nil && hadLeadingStar && !isAllowedPointerDocObj(obj) {
		obj = nil
	}
	if obj == nil {
		return nil, nil
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
