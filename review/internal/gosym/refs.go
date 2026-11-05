package gosym

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/vcs"
)

// RefGroup categorizes references for UI grouping and sorting.
type RefGroup string

const (
	RefGroupChange  RefGroup = "change"
	RefGroupPackage RefGroup = "package"
	RefGroupRepo    RefGroup = "repo"
)

// Reference describes a single reference site for a Go symbol.
type Reference struct {
	Path              string   `json:"path,omitempty"`     // repo-relative slash path (when InRepo)
	Display           string   `json:"display"`            // display path shown in UI
	Pkg               string   `json:"pkg,omitempty"`      // package import path (when !InRepo)
	PkgFile           string   `json:"pkg_file,omitempty"` // package filename (when !InRepo)
	InRepo            bool     `json:"in_repo"`
	Group             RefGroup `json:"group"` // "change", "package", or "repo"
	Line              int      `json:"line"`
	Col               int      `json:"col"`       // 1-based byte column
	LineHTML          string   `json:"line_html"` // syntax-highlighted trimmed 1-line preview
	Container         string   `json:"container,omitempty"`
	ContainerLine     int      `json:"container_line,omitempty"`
	ContainerUTF16Col int      `json:"container_utf16_col,omitempty"`
}

// RefsResult contains all references found for a symbol.
type RefsResult struct {
	Found         bool        `json:"found"`
	SymbolName    string      `json:"symbol_name,omitempty"`
	Signature     string      `json:"signature,omitempty"`
	SignatureHTML string      `json:"signature_html,omitempty"`
	Refs          []Reference `json:"refs,omitempty"`
	Truncated     bool        `json:"truncated,omitempty"`
}

type targetIdentity struct {
	isPkgName    bool
	pkgPath      string
	name         string
	isField      bool
	isMethod     bool
	isLocal      bool
	declDir      string
	isUnexported bool
	fieldBase    string
	fieldClean   string
	fieldLine    int
	fieldCol     int
	recvType     string
}

func cleanRepoRel(absOrRel, repoRoot string) string {
	if absOrRel == "" {
		return ""
	}
	p := filepath.Clean(absOrRel)
	if repoRoot != "" {
		root := filepath.Clean(repoRoot)
		if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			return filepath.ToSlash(rel)
		}
	}
	if filepath.IsAbs(p) {
		return ""
	}
	return filepath.ToSlash(p)
}

func repoDirOf(relPath string) string {
	d := path.Dir(relPath)
	if d == "." {
		return ""
	}
	return d
}

func recvBaseTypeName(t types.Type) string {
	for {
		if pt, ok := t.(*types.Pointer); ok {
			t = pt.Elem()
			continue
		}
		if named, ok := t.(*types.Named); ok {
			if named.Obj() != nil && named.Obj().Pkg() != nil && named.Obj().Parent() != nil && named.Obj().Parent() != named.Obj().Pkg().Scope() {
				return ""
			}
			if named.Obj() != nil {
				return named.Obj().Name()
			}
			return ""
		}
		if alias, ok := t.(*types.Alias); ok {
			t = types.Unalias(alias)
			continue
		}
		break
	}
	return ""
}

func resolveTargetIdentity(obj types.Object, target *resolvedTarget, defCleanFile string, defLine, defCol int, repoRoot string) targetIdentity {
	var targetID targetIdentity
	if obj == nil {
		return targetID
	}
	symName := obj.Name()
	targetID.name = symName
	if pkgName, ok := obj.(*types.PkgName); ok {
		targetID.isPkgName = true
		if pkgName.Imported() != nil {
			targetID.pkgPath = pkgName.Imported().Path()
		}
	} else if obj.Pkg() != nil {
		targetID.pkgPath = obj.Pkg().Path()
	}

	defBase := filepath.Base(defCleanFile)

	if v, ok := obj.(*types.Var); ok && v.IsField() {
		targetID.isField = true
		targetID.fieldBase = defBase
		targetID.fieldClean = defCleanFile
		targetID.fieldLine = defLine
		targetID.fieldCol = defCol
	} else if fn, ok := obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			targetID.isMethod = true
			targetID.recvType = recvBaseTypeName(sig.Recv().Type())
		}
	} else if !targetID.isPkgName && obj.Parent() != nil && obj.Pkg() != nil && obj.Parent() != obj.Pkg().Scope() {
		targetID.isLocal = true
	}

	declDir := ""
	if target != nil {
		declDir = target.dir
	}
	if defRel := cleanRepoRel(defCleanFile, repoRoot); defRel != "" {
		declDir = repoDirOf(defRel)
	}
	targetID.declDir = declDir
	targetID.isUnexported = !targetID.isPkgName && !obj.Exported()
	return targetID
}

func unwrapOrigin(obj types.Object) types.Object {
	if obj == nil {
		return nil
	}
	if v, ok := obj.(*types.Var); ok && v.Origin() != nil {
		return v.Origin()
	}
	if fn, ok := obj.(*types.Func); ok && fn.Origin() != nil {
		return fn.Origin()
	}
	return obj
}

func resolveObjectPosition(obj types.Object, curPkg *types.Package, localFset *token.FileSet, loader *lookupLoader, rootPkg *types.Package) token.Position {
	if obj == nil || !obj.Pos().IsValid() {
		return token.Position{}
	}
	if localFset != nil && curPkg != nil && obj.Pkg() == curPkg {
		return localFset.Position(obj.Pos())
	}
	if loader != nil {
		return loader.positionFor(obj, rootPkg)
	}
	if localFset != nil {
		return localFset.Position(obj.Pos())
	}
	return token.Position{}
}

func identObject(id *ast.Ident, sel *ast.SelectorExpr, info *types.Info) types.Object {
	if info == nil || id == nil {
		return nil
	}
	var res types.Object
	if sel != nil && sel.Sel == id {
		if s := info.Selections[sel]; s != nil {
			res = s.Obj()
		}
	}
	if res == nil {
		if obj := info.Uses[id]; obj != nil {
			res = obj
		}
	}
	return unwrapOrigin(res)
}

type methodMatcher struct {
	targetFn     *types.Func
	effRecv      types.Type
	targetIface  *types.Interface
	defCleanFile string
	defBase      string
	defLine      int
	defCol       int
	mu           sync.Mutex
	memo         map[types.Type]bool
}

func newMethodMatcher(target *resolvedTarget, targetID targetIdentity, defCleanFile, defBase string, defLine, defCol int) *methodMatcher {
	if !targetID.isMethod || target == nil || target.obj == nil {
		return nil
	}
	fn, ok := target.obj.(*types.Func)
	if !ok || fn == nil {
		return nil
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig == nil || sig.Recv() == nil {
		return nil
	}
	rawRecv := sig.Recv().Type()
	effRecv := ensurePointerReceiver(rawRecv)
	return &methodMatcher{
		targetFn:     fn,
		effRecv:      effRecv,
		targetIface:  asInterface(effRecv),
		defCleanFile: defCleanFile,
		defBase:      defBase,
		defLine:      defLine,
		defCol:       defCol,
		memo:         make(map[types.Type]bool),
	}
}

func ensurePointerReceiver(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	u := types.Unalias(t)
	if _, ok := u.(*types.Pointer); ok || isInterfaceType(u) {
		return t
	}
	return types.NewPointer(t)
}

func asInterface(t types.Type) *types.Interface {
	if t == nil {
		return nil
	}
	for {
		if pt, ok := t.(*types.Pointer); ok {
			t = pt.Elem()
			continue
		}
		break
	}
	t = types.Unalias(t)
	if iface, ok := t.Underlying().(*types.Interface); ok {
		return iface
	}
	return nil
}

func matchesTarget(obj types.Object, target targetIdentity, matcher *methodMatcher, curPkg *types.Package, localFset *token.FileSet, loader *lookupLoader, rootPkg *types.Package) bool {
	if obj == nil {
		return false
	}
	if target.isPkgName {
		pkgName, ok := obj.(*types.PkgName)
		return ok && pkgName.Imported() != nil && pkgName.Imported().Path() == target.pkgPath
	}
	if obj.Name() != target.name {
		return false
	}
	if target.isMethod {
		return matchMethod(obj, target, matcher, curPkg, localFset, loader, rootPkg)
	}
	if obj.Pkg() == nil || obj.Pkg().Path() != target.pkgPath {
		return false
	}
	if target.isField {
		v, ok := obj.(*types.Var)
		if !ok || !v.IsField() {
			return false
		}
		pos := resolveObjectPosition(obj, curPkg, localFset, loader, rootPkg)
		if pos.IsValid() {
			if target.fieldClean != "" && filepath.Clean(pos.Filename) == target.fieldClean && pos.Line == target.fieldLine && pos.Column == target.fieldCol {
				return true
			}
			if target.fieldClean == "" && filepath.Base(pos.Filename) == target.fieldBase && pos.Line == target.fieldLine && pos.Column == target.fieldCol {
				return true
			}
		}
		return false
	}
	if target.isLocal {
		return false
	}
	return true
}

func matchMethod(obj types.Object, target targetIdentity, matcher *methodMatcher, curPkg *types.Package, localFset *token.FileSet, loader *lookupLoader, rootPkg *types.Package) bool {
	fn, ok := obj.(*types.Func)
	if !ok || fn == nil {
		return false
	}
	uSig, ok := fn.Type().(*types.Signature)
	if !ok || uSig == nil || uSig.Recv() == nil {
		return false
	}

	// Rule A — Exclude builtin.error.Error from interface correspondence (matches gopls methodsets.go:152)
	if (obj.Pkg() == nil && obj.Name() == "Error") || (matcher != nil && matcher.targetFn != nil && matcher.targetFn.Pkg() == nil && matcher.targetFn.Name() == "Error") {
		return false
	}

	// Rule B — Fast exact receiver match
	if matcher != nil && obj == matcher.targetFn {
		return true
	}
	if obj.Pkg() != nil && obj.Pkg().Path() == target.pkgPath {
		if target.recvType != "" && recvBaseTypeName(uSig.Recv().Type()) == target.recvType {
			return true
		}
		if target.recvType == "" && matcher != nil {
			pos := resolveObjectPosition(obj, curPkg, localFset, loader, rootPkg)
			if pos.IsValid() {
				if matcher.defCleanFile != "" && filepath.Clean(pos.Filename) == matcher.defCleanFile && pos.Line == matcher.defLine && pos.Column == matcher.defCol {
					return true
				}
				if matcher.defCleanFile == "" && filepath.Base(pos.Filename) == matcher.defBase && pos.Line == matcher.defLine && pos.Column == matcher.defCol {
					return true
				}
			}
		}
	}

	if matcher == nil || matcher.targetFn == nil {
		return false
	}

	// Rule C — Interface correspondence (memoized by rawURecv := uSig.Recv().Type())
	rawURecv := uSig.Recv().Type()
	if rawURecv == nil {
		return false
	}

	matcher.mu.Lock()
	cached, found := matcher.memo[rawURecv]
	matcher.mu.Unlock()
	if found {
		return cached
	}

	uEffRecv := ensurePointerReceiver(rawURecv)
	uIface := asInterface(uEffRecv)
	uIsIface := uIface != nil

	// CRITICAL (gopls references.go:465): At least one of (matcher.targetIface != nil, uIsIface) MUST be true!
	// Concrete-to-concrete (A.Close vs B.Close) MUST NEVER match each other!
	if !uIsIface && matcher.targetIface == nil {
		matcher.mu.Lock()
		matcher.memo[rawURecv] = false
		matcher.mu.Unlock()
		return false
	}

	matched := false
	if uIsIface && implementsAcrossPackages(matcher.effRecv, uIface) {
		matched = true
	} else if matcher.targetIface != nil && implementsAcrossPackages(uEffRecv, matcher.targetIface) {
		matched = true
	}

	matcher.mu.Lock()
	matcher.memo[rawURecv] = matched
	matcher.mu.Unlock()
	return matched
}

func implementsAcrossPackages(V types.Type, T *types.Interface) (res bool) {
	defer func() {
		if r := recover(); r != nil {
			res = false
		}
	}()
	if V == nil || T == nil || T.NumMethods() == 0 || !T.IsMethodSet() {
		return false
	}
	if safeImplements(V, T) {
		return true
	}

	instV := instantiateIfGeneric(V)
	mset := safeMethodSet(instV)
	if mset == nil || mset.Len() < T.NumMethods() {
		return false
	}

	for i := 0; i < T.NumMethods(); i++ {
		tm := T.Method(i)
		var matchSel *types.Selection
		for j := 0; j < mset.Len(); j++ {
			sel := mset.At(j)
			obj := sel.Obj()
			if obj.Name() != tm.Name() {
				continue
			}
			if !tm.Exported() {
				if obj.Pkg() == nil || tm.Pkg() == nil || obj.Pkg().Path() != tm.Pkg().Path() {
					continue
				}
			}
			matchSel = sel
			break
		}
		if matchSel == nil {
			return false
		}

		vSig, ok1 := matchSel.Type().(*types.Signature)
		tSig, ok2 := tm.Type().(*types.Signature)
		if !ok1 || !ok2 {
			return false
		}
		if !identicalSignatureAcrossPackages(vSig, tSig, nil) {
			return false
		}
	}
	return true
}

func identicalTypeAcrossPackages(a, b types.Type, seen map[[2]types.Type]bool) bool {
	a = types.Unalias(a)
	b = types.Unalias(b)
	if a == b || types.Identical(a, b) {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	pair := [2]types.Type{a, b}
	if seen != nil && seen[pair] {
		return true
	}

	switch aT := a.(type) {
	case *types.Basic:
		bT, ok := b.(*types.Basic)
		return ok && aT.Kind() == bT.Kind()

	case *types.Pointer:
		bT, ok := b.(*types.Pointer)
		return ok && identicalTypeAcrossPackages(aT.Elem(), bT.Elem(), seen)

	case *types.Slice:
		bT, ok := b.(*types.Slice)
		return ok && identicalTypeAcrossPackages(aT.Elem(), bT.Elem(), seen)

	case *types.Array:
		bT, ok := b.(*types.Array)
		return ok && aT.Len() == bT.Len() && identicalTypeAcrossPackages(aT.Elem(), bT.Elem(), seen)

	case *types.Chan:
		bT, ok := b.(*types.Chan)
		return ok && aT.Dir() == bT.Dir() && identicalTypeAcrossPackages(aT.Elem(), bT.Elem(), seen)

	case *types.Map:
		bT, ok := b.(*types.Map)
		return ok && identicalTypeAcrossPackages(aT.Key(), bT.Key(), seen) && identicalTypeAcrossPackages(aT.Elem(), bT.Elem(), seen)

	case *types.Named:
		bT, ok := b.(*types.Named)
		if !ok {
			return false
		}
		if aT.Obj().Name() != bT.Obj().Name() {
			return false
		}
		aPkg := aT.Obj().Pkg()
		bPkg := bT.Obj().Pkg()
		if (aPkg == nil) != (bPkg == nil) {
			return false
		}
		if aPkg != nil {
			if aPkg.Path() != bPkg.Path() || aPkg.Path() == "" {
				return false
			}
			if aT.Obj().Parent() != aPkg.Scope() || bT.Obj().Parent() != bPkg.Scope() {
				return false
			}
		}
		aArgs := aT.TypeArgs()
		bArgs := bT.TypeArgs()
		aLen := 0
		bLen := 0
		if aArgs != nil {
			aLen = aArgs.Len()
		}
		if bArgs != nil {
			bLen = bArgs.Len()
		}
		if aLen != bLen {
			return false
		}
		for k := 0; k < aLen; k++ {
			if !identicalTypeAcrossPackages(aArgs.At(k), bArgs.At(k), seen) {
				return false
			}
		}
		return true

	case *types.Signature:
		bT, ok := b.(*types.Signature)
		return ok && identicalSignatureAcrossPackages(aT, bT, seen)

	case *types.Struct:
		bT, ok := b.(*types.Struct)
		if !ok || aT.NumFields() != bT.NumFields() {
			return false
		}
		for i := 0; i < aT.NumFields(); i++ {
			af := aT.Field(i)
			bf := bT.Field(i)
			if af.Name() != bf.Name() || af.Embedded() != bf.Embedded() || aT.Tag(i) != bT.Tag(i) {
				return false
			}
			if !af.Exported() {
				if af.Pkg() == nil || bf.Pkg() == nil || af.Pkg().Path() != bf.Pkg().Path() {
					return false
				}
			}
			if !identicalTypeAcrossPackages(af.Type(), bf.Type(), seen) {
				return false
			}
		}
		return true

	case *types.Interface:
		bT, ok := b.(*types.Interface)
		if !ok || aT.NumMethods() != bT.NumMethods() || aT.IsComparable() != bT.IsComparable() {
			return false
		}
		if seen == nil {
			seen = make(map[[2]types.Type]bool)
		}
		seen[pair] = true
		for i := 0; i < aT.NumMethods(); i++ {
			am := aT.Method(i)
			bm := findInterfaceMethod(bT, am.Name())
			if bm == nil {
				return false
			}
			if !am.Exported() {
				if am.Pkg() == nil || bm.Pkg() == nil || am.Pkg().Path() != bm.Pkg().Path() {
					return false
				}
			}
			aSig, ok1 := am.Type().(*types.Signature)
			bSig, ok2 := bm.Type().(*types.Signature)
			if !ok1 || !ok2 || !identicalSignatureAcrossPackages(aSig, bSig, seen) {
				return false
			}
		}
		return true

	case *types.TypeParam:
		bT, ok := b.(*types.TypeParam)
		return ok && aT.Index() == bT.Index()

	default:
		return reflect.TypeOf(a) == reflect.TypeOf(b) && types.TypeString(a, nil) == types.TypeString(b, nil)
	}
}

func findInterfaceMethod(iface *types.Interface, name string) *types.Func {
	if iface == nil {
		return nil
	}
	for i := 0; i < iface.NumMethods(); i++ {
		m := iface.Method(i)
		if m.Name() == name {
			return m
		}
	}
	return nil
}

func identicalSignatureAcrossPackages(a, b *types.Signature, seen map[[2]types.Type]bool) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Variadic() != b.Variadic() {
		return false
	}

	aTP := a.TypeParams()
	bTP := b.TypeParams()
	aTPLen := 0
	bTPLen := 0
	if aTP != nil {
		aTPLen = aTP.Len()
	}
	if bTP != nil {
		bTPLen = bTP.Len()
	}
	if aTPLen != bTPLen {
		return false
	}
	for i := 0; i < aTPLen; i++ {
		if !identicalTypeAcrossPackages(aTP.At(i).Constraint(), bTP.At(i).Constraint(), seen) {
			return false
		}
	}

	aParams := a.Params()
	bParams := b.Params()
	aPLen := 0
	bPLen := 0
	if aParams != nil {
		aPLen = aParams.Len()
	}
	if bParams != nil {
		bPLen = bParams.Len()
	}
	if aPLen != bPLen {
		return false
	}
	for i := 0; i < aPLen; i++ {
		if !identicalTypeAcrossPackages(aParams.At(i).Type(), bParams.At(i).Type(), seen) {
			return false
		}
	}

	aRes := a.Results()
	bRes := b.Results()
	aRLen := 0
	bRLen := 0
	if aRes != nil {
		aRLen = aRes.Len()
	}
	if bRes != nil {
		bRLen = bRes.Len()
	}
	if aRLen != bRLen {
		return false
	}
	for i := 0; i < aRLen; i++ {
		if !identicalTypeAcrossPackages(aRes.At(i).Type(), bRes.At(i).Type(), seen) {
			return false
		}
	}
	return true
}

func containsGoIdent(src []byte, ident string) bool {
	if len(src) == 0 || ident == "" {
		return false
	}
	idBytes := []byte(ident)
	idLen := len(idBytes)
	offset := 0
	for {
		idx := bytes.Index(src[offset:], idBytes)
		if idx == -1 {
			return false
		}
		absIdx := offset + idx
		// Check preceding boundary
		if absIdx > 0 {
			prev := src[absIdx-1]
			if prev >= 0x80 {
				r, _ := utf8.DecodeLastRune(src[:absIdx])
				if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
					offset = absIdx + idLen
					continue
				}
			} else if isIdentByte(prev) {
				offset = absIdx + idLen
				continue
			}
		}
		// Check following boundary
		if absIdx+idLen < len(src) {
			next := src[absIdx+idLen]
			if next >= 0x80 {
				r, _ := utf8.DecodeRune(src[absIdx+idLen:])
				if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
					offset = absIdx + idLen
					continue
				}
			} else if isIdentByte(next) {
				offset = absIdx + idLen
				continue
			}
		}
		return true
	}
}

func isIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

func formatLineHTML(lines []string, lineNum int) string {
	if lineNum < 1 || lineNum > len(lines) {
		return ""
	}
	raw := lines[lineNum-1]
	trimmed := strings.TrimRight(raw, "\r\n")
	trimmed = strings.TrimLeft(trimmed, "\t ")
	if utf8.RuneCountInString(trimmed) > 2048 {
		r := []rune(trimmed)
		trimmed = string(r[:2048]) + "..."
	}
	toks := diff.HighlightLines([]string{trimmed}, diff.LangGo)
	var lineToks []diff.TokenSpan
	if len(toks) > 0 {
		lineToks = toks[0]
	}
	return string(diff.RenderTokensHTMLWS(trimmed, lineToks, nil, false))
}

func groupRank(g RefGroup) int {
	switch g {
	case RefGroupChange:
		return 0
	case RefGroupPackage:
		return 1
	case RefGroupRepo:
		return 2
	default:
		return 3
	}
}

func sortRefs(refs []Reference) {
	slices.SortFunc(refs, func(a, b Reference) int {
		if a.Group != b.Group {
			return groupRank(a.Group) - groupRank(b.Group)
		}
		if a.Display != b.Display {
			return strings.Compare(a.Display, b.Display)
		}
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Col - b.Col
	})
}

// References resolves all reference sites for the Go symbol under the cursor in req.
func (a *Analyzer) References(ctx context.Context, req LookupRequest) (*RefsResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	target, err := a.resolveTarget(ctx, req)
	if err != nil || target == nil || target.obj == nil || target.obj.Pkg() == nil {
		return &RefsResult{Found: false}, nil
	}
	target.obj = unwrapOrigin(target.obj)

	symName := target.obj.Name()
	if pkgName, ok := target.obj.(*types.PkgName); ok && (symName == "" || symName == "_" || symName == ".") {
		if pkgName.Imported() != nil {
			symName = pkgName.Imported().Name()
			if symName == "" {
				symName = guessPkgName(pkgName.Imported().Path())
			}
		}
	}
	sig := formatSignature(target.obj, target.rootPkg)
	if pkgName, ok := target.obj.(*types.PkgName); ok {
		pkgDisplayName := pkgName.Imported().Name()
		if pkgDisplayName == "" {
			pkgDisplayName = symName
		}
		isImportable := pkgName.Imported().Path() != "" && pkgName.Imported().Path() != "." && pkgDisplayName != "main" && !strings.HasSuffix(pkgDisplayName, "_test")
		if isImportable {
			sig = fmt.Sprintf("package %s (%q)", pkgDisplayName, pkgName.Imported().Path())
		} else {
			sig = fmt.Sprintf("package %s", pkgDisplayName)
		}
	}
	sigHTML := highlightSignature(sig)

	res := &RefsResult{
		Found:         true,
		SymbolName:    symName,
		Signature:     sig,
		SignatureHTML: sigHTML,
	}

	defPos := target.loader.positionFor(target.obj, target.rootPkg)
	defCleanFile := ""
	if defPos.Filename != "" {
		defCleanFile = filepath.Clean(defPos.Filename)
	}
	defBase := filepath.Base(defPos.Filename)
	defLine := defPos.Line
	defCol := defPos.Column

	targetID := resolveTargetIdentity(target.obj, target, defCleanFile, defLine, defCol, req.RepoRoot)
	declDir := targetID.declDir
	isUnexported := targetID.isUnexported

	var matcher *methodMatcher
	if targetID.isMethod {
		matcher = newMethodMatcher(target, targetID, defCleanFile, defBase, defLine, defCol)
	}

	var refsCacheK refsCacheKey
	var canCacheRefs bool
	if target.loader.revCache != nil && !targetID.isLocal && isImmutableRev(req.BaseRev) && isImmutableRev(target.rev) {
		canCacheRefs = true
		refsCacheK = refsCacheKey{
			baseRev:      req.BaseRev,
			targetRev:    target.rev,
			targetID:     targetID,
			defCleanFile: defCleanFile,
			defLine:      defLine,
			defCol:       defCol,
			isPkgClause:  target.isPkgClause,
		}
		if cached, ok := target.loader.revCache.refsCache.get(refsCacheK); ok {
			return cached, nil
		}
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

	seen := make(map[refKey]bool)
	const maxRefs = 200

	addRef := func(ref Reference) bool {
		k := refKey{display: ref.Display, line: ref.Line}
		if seen[k] {
			return true
		}
		seen[k] = true
		res.Refs = append(res.Refs, ref)
		if len(res.Refs) >= maxRefs {
			res.Truncated = true
			return false
		}
		return true
	}

	// 0. Sibling package declarations in the same directory (for package clause clicks)
	if target.isPkgClause {
		var dirFiles []string
		if target.isDisk {
			if entries, err := os.ReadDir(target.dir); err == nil {
				for _, e := range entries {
					if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(e.Name(), "_") {
						dirFiles = append(dirFiles, e.Name())
					}
				}
			}
		} else if req.Repo != nil {
			if names, err := req.Repo.ListDir(target.rev, target.dir); err == nil {
				for _, name := range names {
					if strings.HasSuffix(name, ".go") && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_") {
						dirFiles = append(dirFiles, name)
					}
				}
			}
		}
		slices.Sort(dirFiles)

		for _, name := range dirFiles {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if res.Truncated {
				break
			}
			inRepoPath := name
			if target.dir != "" && target.dir != "." {
				inRepoPath = path.Join(target.dir, name)
			}
			var data []byte
			if target.isDisk {
				data, _ = os.ReadFile(filepath.Join(target.dir, name))
			} else if req.Repo != nil {
				data, _ = req.Repo.Content(target.rev, inRepoPath)
			}
			if len(data) == 0 {
				continue
			}

			fset := token.NewFileSet()
			astF, err := parser.ParseFile(fset, name, data, parser.PackageClauseOnly)
			if err != nil || astF == nil || astF.Name == nil || astF.Name.Name != symName {
				continue
			}

			pos := fset.Position(astF.Name.Pos())
			if !pos.IsValid() {
				continue
			}

			if defLine > 0 && pos.Line == defLine && pos.Column == defCol && (filepath.Base(pos.Filename) == defBase || (defCleanFile != "" && filepath.Clean(pos.Filename) == defCleanFile)) {
				continue
			}

			lines := diff.SplitLines(data)
			ref := Reference{
				Line:     pos.Line,
				Col:      pos.Column,
				LineHTML: formatLineHTML(lines, pos.Line),
			}
			if !target.isDisk {
				ref.InRepo = true
				ref.Path = inRepoPath
				ref.Display = inRepoPath
				if changedFiles[inRepoPath] {
					ref.Group = RefGroupChange
				} else {
					ref.Group = RefGroupPackage
				}
			} else {
				ref.InRepo = false
				ref.Pkg = req.PkgImportPath
				ref.PkgFile = name
				ref.Display = name
				ref.Group = RefGroupPackage
			}
			addRef(ref)
		}

		if req.Repo == nil || symName == "main" || strings.HasSuffix(symName, "_test") || targetID.pkgPath == "" || targetID.pkgPath == "." {
			sortRefs(res.Refs)
			return res, nil
		}
	}

	// 1. Function-local object search (local vars, params, named returns, type-switch vars)
	if targetID.isLocal {
		typeSwitchObjects := make(map[types.Object]bool)
		typeSwitchObjects[target.obj] = true
		if target.typeSwitchIdent != nil {
			if defObj := target.info.Defs[target.typeSwitchIdent]; defObj != nil {
				typeSwitchObjects[defObj] = true
			}
		}
		if target.enclosingTypeSwitch != nil && target.enclosingTypeSwitch.Body != nil {
			for _, stmt := range target.enclosingTypeSwitch.Body.List {
				if clause, ok := stmt.(*ast.CaseClause); ok {
					if imp := target.info.Implicits[clause]; imp != nil {
						typeSwitchObjects[imp] = true
					}
				}
			}
		}

		var clickedLines []string
		if target.cleanPath != "" && req.Repo != nil {
			if b, err := req.Repo.Content(target.rev, target.cleanPath); err == nil {
				clickedLines = diff.SplitLines(b)
			}
		} else if target.isDisk && target.dir != "" {
			targetPath := filepath.Join(target.dir, req.PkgFile)
			if b, err := os.ReadFile(targetPath); err == nil {
				clickedLines = diff.SplitLines(b)
			}
		}

		var stack []ast.Node
		ast.Inspect(target.clickedAST, func(n ast.Node) bool {
			if n == nil || res.Truncated {
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				return false
			}
			stack = append(stack, n)
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			pos := target.loader.fset.Position(id.Pos())
			if !pos.IsValid() {
				return true
			}
			if defCleanFile != "" && filepath.Clean(pos.Filename) == defCleanFile && pos.Line == defLine && pos.Column == defCol {
				return true
			}
			var sel *ast.SelectorExpr
			if len(stack) >= 2 {
				if s, ok := stack[len(stack)-2].(*ast.SelectorExpr); ok && s.Sel == id {
					sel = s
				}
			}
			obj := identObject(id, sel, target.info)
			if obj == nil {
				return true
			}
			if obj == target.obj || typeSwitchObjects[obj] {
				cName, cLine, cCol := enclosingDeclInfo(target.clickedAST, target.loader.fset, clickedLines, id.Pos())
				ref := Reference{
					Line:              pos.Line,
					Col:               pos.Column,
					LineHTML:          formatLineHTML(clickedLines, pos.Line),
					Container:         cName,
					ContainerLine:     cLine,
					ContainerUTF16Col: cCol,
				}
				if !target.isChained {
					ref.InRepo = true
					ref.Path = target.cleanPath
					ref.Display = target.cleanPath
					if changedFiles[target.cleanPath] {
						ref.Group = RefGroupChange
					} else {
						ref.Group = RefGroupPackage
					}
				} else {
					ref.InRepo = false
					ref.Pkg = req.PkgImportPath
					ref.PkgFile = req.PkgFile
					ref.Display = req.PkgFile
					ref.Group = RefGroupPackage
				}
				addRef(ref)
			}
			return true
		})

		sortRefs(res.Refs)
		if canCacheRefs && res != nil {
			target.loader.revCache.refsCache.put(refsCacheK, res)
		}
		return cloneRefsResult(res), nil
	}

	// 2. Unexported package symbol (only exists in declaring package directory)
	if isUnexported {
		if !target.isChained {
			var preAST []*ast.File
			var preInfo *types.Info
			if declDir == target.dir {
				preAST = target.pkgASTFiles
				preInfo = target.info
			}
			_ = a.searchRepoDir(ctx, target.loader, target.rootPkg, target.rev, declDir, targetID, matcher, defCleanFile, defLine, defCol, changedFiles, declDir, preAST, preInfo, addRef)
			if target.dir != declDir {
				_ = a.searchRepoDir(ctx, target.loader, target.rootPkg, target.rev, target.dir, targetID, matcher, defCleanFile, defLine, defCol, changedFiles, declDir, target.pkgASTFiles, target.info, addRef)
			}
		} else if target.isDisk && target.dir != "" {
			_ = a.searchDiskDir(ctx, target.loader, target.rootPkg, target.dir, req.PkgImportPath, targetID, matcher, defCleanFile, defLine, defCol, addRef)
		}
		sortRefs(res.Refs)
		if canCacheRefs && res != nil {
			target.loader.revCache.refsCache.put(refsCacheK, res)
		}
		return cloneRefsResult(res), nil
	}

	// 3. Exported symbol or package name across repository
	if target.isChained && target.isDisk && target.dir != "" {
		_ = a.searchDiskDir(ctx, target.loader, target.rootPkg, target.dir, req.PkgImportPath, targetID, matcher, defCleanFile, defLine, defCol, addRef)
	}

	if req.Repo == nil {
		sortRefs(res.Refs)
		return res, nil
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

	requireImportOutsideDeclDir := !targetID.isPkgName && !targetID.isField && !targetID.isMethod && !targetID.isLocal && targetID.pkgPath != "" && targetID.pkgPath != "."

	var candidateFiles []string
	contentMap := make(map[string][]byte)

	var candFilterPaths []string
	var candFilterOIDs []string

	for _, fPath := range goFiles {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		fDir := path.Dir(fPath)
		if fDir == "." {
			fDir = ""
		}
		needImport := targetID.isPkgName || (fDir != declDir && requireImportOutsideDeclDir)

		if fileIdx != nil {
			if needImport && !fileIdx.hasImport(fPath, targetID.pkgPath) {
				continue
			}
			if !targetID.isPkgName && !fileIdx.mayContainIdent(fPath, target.obj.Name()) {
				continue
			}
		}

		candFilterPaths = append(candFilterPaths, fPath)
		var oid string
		if fileIdx != nil {
			if i, ok := fileIdx.byPath[fPath]; ok {
				oid = fileIdx.files[i].oid
			}
		}
		candFilterOIDs = append(candFilterOIDs, oid)
	}

	candContents := readRepoFilesBatch(req.Repo, target.rev, candFilterPaths, candFilterOIDs)

	for _, fPath := range candFilterPaths {
		b := candContents[fPath]
		if len(b) == 0 {
			continue
		}
		fDir := path.Dir(fPath)
		if fDir == "." {
			fDir = ""
		}
		needImport := targetID.isPkgName || (fDir != declDir && requireImportOutsideDeclDir)
		if needImport && (fileIdx == nil || !fileIdx.isImportExact(fPath)) && !containsImportLiteral(b, targetID.pkgPath) {
			continue
		}
		if !targetID.isPkgName && !containsGoIdent(b, target.obj.Name()) {
			continue
		}
		candidateFiles = append(candidateFiles, fPath)
		contentMap[fPath] = b
	}

	// Prioritize candidate files: changed files first, then declDir, then repo
	slices.SortStableFunc(candidateFiles, func(a, b string) int {
		pA := 2
		if changedFiles[a] {
			pA = 0
		} else if path.Dir(a) == declDir || (declDir == "" && path.Dir(a) == ".") {
			pA = 1
		}
		pB := 2
		if changedFiles[b] {
			pB = 0
		} else if path.Dir(b) == declDir || (declDir == "" && path.Dir(b) == ".") {
			pB = 1
		}
		return pA - pB
	})

	// Group candidates by directory
	dirGroups := make(map[string][]string)
	var orderedDirs []string
	for _, fPath := range candidateFiles {
		d := path.Dir(fPath)
		if d == "." {
			d = ""
		}
		if _, ok := dirGroups[d]; !ok {
			orderedDirs = append(orderedDirs, d)
		}
		dirGroups[d] = append(dirGroups[d], fPath)
	}

	// Search candidate directories concurrently with bounded parallelism
	perDirRefs := make([][]Reference, len(orderedDirs))
	if len(orderedDirs) > 0 {
		loaders := make([]*lookupLoader, len(orderedDirs))
		for idx, d := range orderedDirs {
			if d == target.dir && !target.isChained {
				loaders[idx] = target.loader
			} else {
				loaders[idx] = target.loader.clone()
			}
		}

		workers := min(len(orderedDirs), max(1, runtime.NumCPU()), 12)
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup

		for idx, d := range orderedDirs {
			if ctx.Err() != nil {
				break
			}
			idx, d := idx, d
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				if ctx.Err() != nil {
					return
				}

				ldr := loaders[idx]
				var preParsedASTs []*ast.File
				var preParsedInfo *types.Info
				if d == target.dir && !target.isChained {
					preParsedASTs = target.pkgASTFiles
					preParsedInfo = target.info
				}

				dirContentMap := make(map[string][]byte)
				for _, f := range dirGroups[d] {
					if b, ok := contentMap[f]; ok {
						dirContentMap[f] = b
					}
				}

				var dirRefs []Reference
				dirAddRef := func(ref Reference) bool {
					dirRefs = append(dirRefs, ref)
					return len(dirRefs) < maxRefs
				}

				_ = a.searchRepoDirWithCandidates(ctx, ldr, target.rootPkg, target.rev, d, dirGroups[d], dirContentMap, targetID, matcher, defCleanFile, defLine, defCol, changedFiles, declDir, preParsedASTs, preParsedInfo, dirAddRef)
				perDirRefs[idx] = dirRefs
			}()
		}
		wg.Wait()

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		for _, dirRefs := range perDirRefs {
			for _, ref := range dirRefs {
				if !addRef(ref) {
					break
				}
			}
			if res.Truncated {
				break
			}
		}
	}

	sortRefs(res.Refs)
	if canCacheRefs && res != nil {
		target.loader.revCache.refsCache.put(refsCacheK, res)
	}
	return cloneRefsResult(res), nil
}

func (a *Analyzer) searchRepoDir(
	ctx context.Context,
	loader *lookupLoader,
	rootPkg *types.Package,
	rev string,
	dirInRepo string,
	targetID targetIdentity,
	matcher *methodMatcher,
	defCleanFile string,
	defLine, defCol int,
	changedFiles map[string]bool,
	declDir string,
	preParsedASTs []*ast.File,
	preParsedInfo *types.Info,
	addRef func(ref Reference) bool,
) error {
	if loader.revCache != nil {
		if fileIdx, _ := loader.revCache.getRepoFileIndex(ctx, loader.req.Repo, rev); fileIdx != nil && len(fileIdx.dirPkgFiles[dirInRepo]) > 0 {
			uniqueFiles := make(map[string]bool)
			for _, files := range fileIdx.dirPkgFiles[dirInRepo] {
				for _, fPath := range files {
					uniqueFiles[fPath] = true
				}
			}
			var candFiles []string
			for fPath := range uniqueFiles {
				match := false
				if targetID.isPkgName {
					match = fileIdx.hasImport(fPath, targetID.pkgPath)
				} else {
					match = fileIdx.mayContainIdent(fPath, targetID.name)
				}
				if match {
					candFiles = append(candFiles, fPath)
				}
			}
			if len(candFiles) == 0 {
				return nil
			}
			slices.Sort(candFiles)
			return a.searchRepoDirWithCandidates(ctx, loader, rootPkg, rev, dirInRepo, candFiles, make(map[string][]byte), targetID, matcher, defCleanFile, defLine, defCol, changedFiles, declDir, preParsedASTs, preParsedInfo, addRef)
		}
	}

	names, err := loader.req.Repo.ListDir(rev, dirInRepo)
	if err != nil || len(names) == 0 {
		return nil
	}
	var candFiles []string
	contentMap := make(map[string][]byte)
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		fPath := name
		if dirInRepo != "" && dirInRepo != "." {
			fPath = path.Join(dirInRepo, name)
		}
		b, err := loader.req.Repo.Content(rev, fPath)
		if err != nil || len(b) == 0 {
			continue
		}
		match := false
		if targetID.isPkgName {
			match = containsImportLiteral(b, targetID.pkgPath)
		} else {
			match = containsGoIdent(b, targetID.name)
		}
		if match {
			candFiles = append(candFiles, fPath)
			contentMap[fPath] = b
		}
	}
	if len(candFiles) == 0 {
		return nil
	}
	return a.searchRepoDirWithCandidates(ctx, loader, rootPkg, rev, dirInRepo, candFiles, contentMap, targetID, matcher, defCleanFile, defLine, defCol, changedFiles, declDir, preParsedASTs, preParsedInfo, addRef)
}

type fileLineKey struct {
	fileIdx int
	line    int
}

type refKey struct {
	display string
	line    int
}

func (a *Analyzer) searchRepoDirWithCandidates(
	ctx context.Context,
	loader *lookupLoader,
	rootPkg *types.Package,
	rev string,
	dirInRepo string,
	candFiles []string,
	contentMap map[string][]byte,
	targetID targetIdentity,
	matcher *methodMatcher,
	defCleanFile string,
	defLine, defCol int,
	changedFiles map[string]bool,
	declDir string,
	preParsedASTs []*ast.File,
	preParsedInfo *types.Info,
	addRef func(ref Reference) bool,
) error {
	candSet := make(map[string]bool)
	for _, f := range candFiles {
		candSet[f] = true
	}

	// Optimization: if preParsedASTs has all candidate files and none are missing, inspect directly
	if preParsedASTs != nil && preParsedInfo != nil {
		preParsedNames := make(map[string]*ast.File)
		for _, astF := range preParsedASTs {
			pos := loader.fset.Position(astF.Pos())
			if pos.Filename != "" {
				preParsedNames[filepath.ToSlash(pos.Filename)] = astF
			}
		}

		allPreParsed := true
		for _, c := range candFiles {
			vPath := filepath.ToSlash(filepath.Join(loader.req.RepoRoot, filepath.FromSlash(c)))
			if _, ok := preParsedNames[vPath]; !ok {
				allPreParsed = false
				break
			}
		}

		if allPreParsed {
			for _, c := range candFiles {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				vPath := filepath.ToSlash(filepath.Join(loader.req.RepoRoot, filepath.FromSlash(c)))
				astF := preParsedNames[vPath]
				if astF == nil {
					continue
				}
				var lines []string
				if b, ok := contentMap[c]; ok {
					lines = diff.SplitLines(b)
				} else if b, err := loader.req.Repo.Content(rev, c); err == nil {
					lines = diff.SplitLines(b)
				}
				group := RefGroupRepo
				if changedFiles[c] {
					group = RefGroupChange
				} else if dirInRepo == declDir {
					group = RefGroupPackage
				}
				inspectASTForRefs(astF, preParsedInfo, rootPkg, loader.fset, loader, rootPkg, targetID, matcher, defCleanFile, defLine, defCol, c, lines, group, addRef)
			}
			return nil
		}
	}

	extractFromCheckedEntry := func(checkedEntry *checkedPkgEntry) bool {
		linesWithRef := make(map[fileLineKey]bool)

		if targetID.isPkgName {
			for _, u := range checkedEntry.pkgUsesByPath[targetID.pkgPath] {
				if u.fileIdx < 0 || u.fileIdx >= len(checkedEntry.files) {
					continue
				}
				f := checkedEntry.files[u.fileIdx]
				if !candSet[f.path] {
					continue
				}
				var pos token.Position
				var idPos token.Pos
				if u.importSpec != nil {
					pos = checkedEntry.fset.Position(u.importSpec.Pos())
					idPos = u.importSpec.Pos()
				} else if u.id != nil {
					pos = checkedEntry.fset.Position(u.id.Pos())
					idPos = u.id.Pos()
				}
				if !pos.IsValid() {
					continue
				}
				if defCleanFile != "" && filepath.Clean(pos.Filename) == defCleanFile && pos.Line == defLine && pos.Column == defCol {
					continue
				}
				flKey := fileLineKey{fileIdx: u.fileIdx, line: pos.Line}
				if !linesWithRef[flKey] {
					linesWithRef[flKey] = true
					lines := f.lines
					cName, cLine, cCol := enclosingDeclInfo(f.ast, checkedEntry.fset, lines, idPos)
					group := RefGroupRepo
					if changedFiles[f.path] {
						group = RefGroupChange
					} else if dirInRepo == declDir {
						group = RefGroupPackage
					}
					ref := Reference{
						Path:              f.path,
						Display:           f.path,
						InRepo:            true,
						Group:             group,
						Line:              pos.Line,
						Col:               pos.Column,
						LineHTML:          formatLineHTML(lines, pos.Line),
						Container:         cName,
						ContainerLine:     cLine,
						ContainerUTF16Col: cCol,
					}
					if !addRef(ref) {
						return false
					}
				}
			}
		} else {
			for _, u := range checkedEntry.usesByName[targetID.name] {
				if u.fileIdx < 0 || u.fileIdx >= len(checkedEntry.files) {
					continue
				}
				f := checkedEntry.files[u.fileIdx]
				if !candSet[f.path] {
					continue
				}
				if !matchesTarget(u.obj, targetID, matcher, checkedEntry.pkg, checkedEntry.fset, loader, rootPkg) {
					continue
				}
				pos := checkedEntry.fset.Position(u.id.Pos())
				if !pos.IsValid() {
					continue
				}
				if defCleanFile != "" && filepath.Clean(pos.Filename) == defCleanFile && pos.Line == defLine && pos.Column == defCol {
					continue
				}
				flKey := fileLineKey{fileIdx: u.fileIdx, line: pos.Line}
				if !linesWithRef[flKey] {
					linesWithRef[flKey] = true
					lines := f.lines
					cName, cLine, cCol := enclosingDeclInfo(f.ast, checkedEntry.fset, lines, u.id.Pos())
					group := RefGroupRepo
					if changedFiles[f.path] {
						group = RefGroupChange
					} else if dirInRepo == declDir {
						group = RefGroupPackage
					}
					ref := Reference{
						Path:              f.path,
						Display:           f.path,
						InRepo:            true,
						Group:             group,
						Line:              pos.Line,
						Col:               pos.Column,
						LineHTML:          formatLineHTML(lines, pos.Line),
						Container:         cName,
						ContainerLine:     cLine,
						ContainerUTF16Col: cCol,
					}
					if !addRef(ref) {
						return false
					}
				}
			}
		}
		return true
	}

	if loader.revCache != nil {
		fileIdx, _ := loader.revCache.getRepoFileIndex(ctx, loader.req.Repo, rev)
		if fileIdx != nil && len(fileIdx.dirPkgFiles[dirInRepo]) > 0 {
			allOk := true
			var pkgNames []string
			for pName := range fileIdx.dirPkgFiles[dirInRepo] {
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
				pkgFilePaths := fileIdx.dirPkgFiles[dirInRepo][pkgName]
				var hasCand, hasTestCand bool
				for _, p := range pkgFilePaths {
					if candSet[p] {
						hasCand = true
						if strings.HasSuffix(p, "_test.go") {
							hasTestCand = true
						}
					}
				}
				if !hasCand {
					continue
				}
				isExtTest := strings.HasSuffix(pkgName, "_test")
				includeTests := isExtTest || hasTestCand
				checkedEntry := loader.loadCheckedPackage(dirInRepo, pkgName, includeTests)
				if checkedEntry != nil {
					if !extractFromCheckedEntry(checkedEntry) {
						return nil
					}
				} else {
					allOk = false
					break
				}
			}
			if allOk {
				return nil
			}
		}
	}

	// Read and parse all files in dirInRepo to type check packages
	names, err := loader.req.Repo.ListDir(rev, dirInRepo)
	if err != nil || len(names) == 0 {
		return nil
	}

	bctx, fc := loader.makeBuildContextWithCache()
	fset := token.NewFileSet()

	type fileEntry struct {
		name    string
		fPath   string
		astFile *ast.File
		content []byte
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
		data, ok := contentMap[fPath]
		if !ok {
			data, ok = loader.matchAndReadInRepo(bctx, fc, dirInRepo, name)
			if !ok || len(data) == 0 {
				continue
			}
			contentMap[fPath] = data
		}

		vPath := filepath.Join(loader.req.RepoRoot, filepath.FromSlash(fPath))
		astF, err := parser.ParseFile(fset, vPath, data, parser.SkipObjectResolution)
		if err != nil || astF == nil {
			continue
		}
		pkgName := astF.Name.Name
		pkgFiles[pkgName] = append(pkgFiles[pkgName], fileEntry{
			name:    name,
			fPath:   fPath,
			astFile: astF,
			content: data,
		})
	}

	rootImportPath := cleanPathToImport(loader.mod, dirInRepo)

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
		entries := pkgFiles[pkgName]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hasCand := false
		hasTestCand := false
		for _, e := range entries {
			if candSet[e.fPath] {
				hasCand = true
				if strings.HasSuffix(e.fPath, "_test.go") {
					hasTestCand = true
				}
			}
		}
		if !hasCand {
			continue
		}

		isExtTest := strings.HasSuffix(pkgName, "_test")
		includeTests := isExtTest || hasTestCand
		if loader.revCache != nil {
			if checkedEntry := loader.loadCheckedPackage(dirInRepo, pkgName, includeTests); checkedEntry != nil {
				if !extractFromCheckedEntry(checkedEntry) {
					return nil
				}
				continue
			}
		}

		var astFiles []*ast.File
		for _, e := range entries {
			astFiles = append(astFiles, e.astFile)
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

		isExtTest = strings.HasSuffix(pkgName, "_test")
		impPath := rootImportPath
		if isExtTest {
			impPath += "_test"
			if basePkg, err := loader.loadInRepoTestBasePackage(rootImportPath, dirInRepo); err == nil && basePkg != nil {
				loader.inRepoPkgs[rootImportPath] = basePkg
			}
		}
		pkg, _ := conf.Check(impPath, fset, astFiles, info)

		for _, e := range entries {
			if !candSet[e.fPath] {
				continue
			}
			group := RefGroupRepo
			if changedFiles[e.fPath] {
				group = RefGroupChange
			} else if dirInRepo == declDir {
				group = RefGroupPackage
			}
			lines := diff.SplitLines(e.content)
			inspectASTForRefs(e.astFile, info, pkg, fset, loader, rootPkg, targetID, matcher, defCleanFile, defLine, defCol, e.fPath, lines, group, addRef)
		}
	}

	return nil
}

func (a *Analyzer) searchDiskDir(
	ctx context.Context,
	loader *lookupLoader,
	rootPkg *types.Package,
	absDir string,
	pkgImportPath string,
	targetID targetIdentity,
	matcher *methodMatcher,
	defCleanFile string,
	defLine, defCol int,
	addRef func(ref Reference) bool,
) error {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil
	}

	bctx, fc := loader.makeBuildContextWithCache()

	type fileEntry struct {
		name    string
		content []byte
		isCand  bool
	}
	pkgFiles := make(map[string][]fileEntry)

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		data, ok := loader.matchAndReadDisk(bctx, fc, absDir, name)
		if !ok || len(data) == 0 {
			continue
		}

		var pkgName string
		if _, _, p := scanGoHeaderImports(data); p != "" {
			pkgName = p
		} else if pAST, err := parser.ParseFile(token.NewFileSet(), name, data, parser.PackageClauseOnly); err == nil && pAST != nil && pAST.Name != nil {
			pkgName = pAST.Name.Name
		}
		if pkgName == "" {
			continue
		}

		isCand := false
		if targetID.isPkgName {
			isCand = containsImportLiteral(data, targetID.pkgPath)
		} else {
			isCand = containsGoIdent(data, targetID.name)
		}

		pkgFiles[pkgName] = append(pkgFiles[pkgName], fileEntry{
			name:    name,
			content: data,
			isCand:  isCand,
		})
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
		list := pkgFiles[pkgName]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hasCand := false
		hasTestCand := false
		candFileSet := make(map[string]bool)
		for _, e := range list {
			if e.isCand {
				hasCand = true
				candFileSet[e.name] = true
				if strings.HasSuffix(e.name, "_test.go") {
					hasTestCand = true
				}
			}
		}
		if !hasCand {
			continue
		}

		isExtTest := strings.HasSuffix(pkgName, "_test")
		includeTests := isExtTest || hasTestCand
		impPath := pkgImportPath
		if isExtTest {
			impPath += "_test"
		}

		if loader.diskCache != nil {
			if checkedEntry := loader.loadDiskCheckedPackage(impPath, absDir, pkgName, includeTests); checkedEntry != nil {
				linesWithRef := make(map[fileLineKey]bool)

				if targetID.isPkgName {
					for _, u := range checkedEntry.pkgUsesByPath[targetID.pkgPath] {
						if u.fileIdx < 0 || u.fileIdx >= len(checkedEntry.files) {
							continue
						}
						f := checkedEntry.files[u.fileIdx]
						baseName := filepath.Base(f.path)
						if !candFileSet[baseName] {
							continue
						}
						var pos token.Position
						var idPos token.Pos
						if u.importSpec != nil {
							pos = checkedEntry.fset.Position(u.importSpec.Pos())
							idPos = u.importSpec.Pos()
						} else if u.id != nil {
							pos = checkedEntry.fset.Position(u.id.Pos())
							idPos = u.id.Pos()
						}
						if !pos.IsValid() {
							continue
						}
						if defCleanFile != "" && filepath.Clean(pos.Filename) == defCleanFile && pos.Line == defLine && pos.Column == defCol {
							continue
						}
						flKey := fileLineKey{fileIdx: u.fileIdx, line: pos.Line}
						if !linesWithRef[flKey] {
							linesWithRef[flKey] = true
							lines := f.lines
							cName, cLine, cCol := enclosingDeclInfo(f.ast, checkedEntry.fset, lines, idPos)
							ref := Reference{
								Display:           baseName,
								Pkg:               pkgImportPath,
								PkgFile:           baseName,
								InRepo:            false,
								Group:             RefGroupPackage,
								Line:              pos.Line,
								Col:               pos.Column,
								LineHTML:          formatLineHTML(lines, pos.Line),
								Container:         cName,
								ContainerLine:     cLine,
								ContainerUTF16Col: cCol,
							}
							if !addRef(ref) {
								return nil
							}
						}
					}
				} else {
					for _, u := range checkedEntry.usesByName[targetID.name] {
						if u.fileIdx < 0 || u.fileIdx >= len(checkedEntry.files) {
							continue
						}
						f := checkedEntry.files[u.fileIdx]
						baseName := filepath.Base(f.path)
						if !candFileSet[baseName] {
							continue
						}
						if !matchesTarget(u.obj, targetID, matcher, checkedEntry.pkg, checkedEntry.fset, loader, rootPkg) {
							continue
						}
						pos := checkedEntry.fset.Position(u.id.Pos())
						if !pos.IsValid() {
							continue
						}
						if defCleanFile != "" && filepath.Clean(pos.Filename) == defCleanFile && pos.Line == defLine && pos.Column == defCol {
							continue
						}
						flKey := fileLineKey{fileIdx: u.fileIdx, line: pos.Line}
						if !linesWithRef[flKey] {
							linesWithRef[flKey] = true
							lines := f.lines
							cName, cLine, cCol := enclosingDeclInfo(f.ast, checkedEntry.fset, lines, u.id.Pos())
							ref := Reference{
								Display:           baseName,
								Pkg:               pkgImportPath,
								PkgFile:           baseName,
								InRepo:            false,
								Group:             RefGroupPackage,
								Line:              pos.Line,
								Col:               pos.Column,
								LineHTML:          formatLineHTML(lines, pos.Line),
								Container:         cName,
								ContainerLine:     cLine,
								ContainerUTF16Col: cCol,
							}
							if !addRef(ref) {
								return nil
							}
						}
					}
				}
				continue
			}
		}

		fset := token.NewFileSet()
		var astFiles []*ast.File
		astFileMap := make(map[string]*ast.File)
		for _, e := range list {
			p := filepath.Join(absDir, e.name)
			if astF, err := parser.ParseFile(fset, p, e.content, parser.SkipObjectResolution); err == nil && astF != nil {
				astFiles = append(astFiles, astF)
				astFileMap[e.name] = astF
			}
		}
		if len(astFiles) == 0 {
			continue
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
		isExtTest = strings.HasSuffix(pkgName, "_test")
		impPath = pkgImportPath
		if isExtTest {
			impPath += "_test"
			if basePkg, err := loader.loadDiskTestBasePackage(pkgImportPath, absDir); err == nil && basePkg != nil {
				loader.inRepoPkgs[pkgImportPath] = basePkg
			}
		}
		pkg, _ := conf.Check(impPath, fset, astFiles, info)

		for _, e := range list {
			if !e.isCand || astFileMap[e.name] == nil {
				continue
			}
			lines := diff.SplitLines(e.content)
			inspectDiskASTForRefs(astFileMap[e.name], info, pkg, fset, loader, rootPkg, targetID, matcher, defCleanFile, defLine, defCol, pkgImportPath, e.name, lines, addRef)
		}
	}

	return nil
}

func inspectASTForRefs(
	fileAST *ast.File,
	info *types.Info,
	curPkg *types.Package,
	fset *token.FileSet,
	loader *lookupLoader,
	rootPkg *types.Package,
	targetID targetIdentity,
	matcher *methodMatcher,
	defCleanFile string,
	defLine, defCol int,
	fPath string,
	lines []string,
	group RefGroup,
	addRef func(ref Reference) bool,
) {
	linesWithRef := make(map[int]bool)
	var stack []ast.Node
	ast.Inspect(fileAST, func(n ast.Node) bool {
		if n == nil {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			return false
		}
		stack = append(stack, n)

		if targetID.isPkgName {
			if spec, ok := n.(*ast.ImportSpec); ok && spec.Path != nil {
				unquoted, err := strconv.Unquote(spec.Path.Value)
				if err == nil && unquoted == targetID.pkgPath {
					pos := fset.Position(spec.Pos())
					if pos.IsValid() && !(defCleanFile != "" && filepath.Clean(pos.Filename) == defCleanFile && pos.Line == defLine && pos.Column == defCol) {
						if !linesWithRef[pos.Line] {
							linesWithRef[pos.Line] = true
							cName, cLine, cCol := enclosingDeclInfo(fileAST, fset, lines, spec.Pos())
							ref := Reference{
								Path:              fPath,
								Display:           fPath,
								InRepo:            true,
								Group:             group,
								Line:              pos.Line,
								Col:               pos.Column,
								LineHTML:          formatLineHTML(lines, pos.Line),
								Container:         cName,
								ContainerLine:     cLine,
								ContainerUTF16Col: cCol,
							}
							addRef(ref)
						}
					}
				}
			}
		}

		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		pos := fset.Position(id.Pos())
		if !pos.IsValid() {
			return true
		}
		// Exclude definition itself
		if defCleanFile != "" && filepath.Clean(pos.Filename) == defCleanFile && pos.Line == defLine && pos.Column == defCol {
			return true
		}
		var sel *ast.SelectorExpr
		if len(stack) >= 2 {
			if s, ok := stack[len(stack)-2].(*ast.SelectorExpr); ok && s.Sel == id {
				sel = s
			}
		}
		obj := identObject(id, sel, info)
		if matchesTarget(obj, targetID, matcher, curPkg, fset, loader, rootPkg) {
			if !linesWithRef[pos.Line] {
				linesWithRef[pos.Line] = true
				cName, cLine, cCol := enclosingDeclInfo(fileAST, fset, lines, id.Pos())
				ref := Reference{
					Path:              fPath,
					Display:           fPath,
					InRepo:            true,
					Group:             group,
					Line:              pos.Line,
					Col:               pos.Column,
					LineHTML:          formatLineHTML(lines, pos.Line),
					Container:         cName,
					ContainerLine:     cLine,
					ContainerUTF16Col: cCol,
				}
				addRef(ref)
			}
		}
		return true
	})
}

func inspectDiskASTForRefs(
	fileAST *ast.File,
	info *types.Info,
	curPkg *types.Package,
	fset *token.FileSet,
	loader *lookupLoader,
	rootPkg *types.Package,
	targetID targetIdentity,
	matcher *methodMatcher,
	defCleanFile string,
	defLine, defCol int,
	pkgImportPath, pkgFile string,
	lines []string,
	addRef func(ref Reference) bool,
) {
	linesWithRef := make(map[int]bool)
	var stack []ast.Node
	ast.Inspect(fileAST, func(n ast.Node) bool {
		if n == nil {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			return false
		}
		stack = append(stack, n)

		if targetID.isPkgName {
			if spec, ok := n.(*ast.ImportSpec); ok && spec.Path != nil {
				unquoted, err := strconv.Unquote(spec.Path.Value)
				if err == nil && unquoted == targetID.pkgPath {
					pos := fset.Position(spec.Pos())
					if pos.IsValid() && !(defCleanFile != "" && filepath.Clean(pos.Filename) == defCleanFile && pos.Line == defLine && pos.Column == defCol) {
						if !linesWithRef[pos.Line] {
							linesWithRef[pos.Line] = true
							cName, cLine, cCol := enclosingDeclInfo(fileAST, fset, lines, spec.Pos())
							ref := Reference{
								Display:           pkgFile,
								Pkg:               pkgImportPath,
								PkgFile:           pkgFile,
								InRepo:            false,
								Group:             RefGroupPackage,
								Line:              pos.Line,
								Col:               pos.Column,
								LineHTML:          formatLineHTML(lines, pos.Line),
								Container:         cName,
								ContainerLine:     cLine,
								ContainerUTF16Col: cCol,
							}
							addRef(ref)
						}
					}
				}
			}
		}

		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		pos := fset.Position(id.Pos())
		if !pos.IsValid() {
			return true
		}
		// Exclude definition itself
		if defCleanFile != "" && filepath.Clean(pos.Filename) == defCleanFile && pos.Line == defLine && pos.Column == defCol {
			return true
		}
		var sel *ast.SelectorExpr
		if len(stack) >= 2 {
			if s, ok := stack[len(stack)-2].(*ast.SelectorExpr); ok && s.Sel == id {
				sel = s
			}
		}
		obj := identObject(id, sel, info)
		if matchesTarget(obj, targetID, matcher, curPkg, fset, loader, rootPkg) {
			if !linesWithRef[pos.Line] {
				linesWithRef[pos.Line] = true
				cName, cLine, cCol := enclosingDeclInfo(fileAST, fset, lines, id.Pos())
				ref := Reference{
					Display:           pkgFile,
					Pkg:               pkgImportPath,
					PkgFile:           pkgFile,
					InRepo:            false,
					Group:             RefGroupPackage,
					Line:              pos.Line,
					Col:               pos.Column,
					LineHTML:          formatLineHTML(lines, pos.Line),
					Container:         cName,
					ContainerLine:     cLine,
					ContainerUTF16Col: cCol,
				}
				addRef(ref)
			}
		}
		return true
	})
}

func containsImportLiteral(b []byte, pkgPath string) bool {
	return bytes.Contains(b, []byte(`"`+pkgPath+`"`)) || bytes.Contains(b, []byte("`"+pkgPath+"`"))
}

func enclosingDeclInfo(fileAST *ast.File, fset *token.FileSet, lines []string, refPos token.Pos) (name string, line int, utf16Col int) {
	if fileAST == nil || !refPos.IsValid() || fset == nil {
		return "", 0, 0
	}
	for _, decl := range fileAST.Decls {
		if decl == nil || refPos < decl.Pos() || refPos > decl.End() {
			continue
		}
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				return "", 0, 0
			}
			fnName := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				isPtr := false
				t := d.Recv.List[0].Type
				if star, ok := t.(*ast.StarExpr); ok {
					isPtr = true
					t = star.X
				}
				if idx, ok := t.(*ast.IndexExpr); ok {
					t = idx.X
				} else if idxList, ok := t.(*ast.IndexListExpr); ok {
					t = idxList.X
				}
				if id, ok := t.(*ast.Ident); ok {
					if isPtr {
						name = fmt.Sprintf("(*%s).%s", id.Name, fnName)
					} else {
						name = fmt.Sprintf("(%s).%s", id.Name, fnName)
					}
				} else {
					name = fnName
				}
			} else {
				name = fnName
			}
			if fnName != "_" && !(d.Recv == nil && fnName == "init") {
				p := fset.Position(d.Name.Pos())
				if p.IsValid() && p.Line > 0 {
					line = p.Line
					utf16Col = byteColToUTF16Col(lines, p.Line, p.Column)
				}
			}
			return name, line, utf16Col

		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				return "", 0, 0
			}
			for _, spec := range d.Specs {
				if spec == nil || refPos < spec.Pos() || refPos > spec.End() {
					continue
				}
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name != nil && s.Name.Name != "_" {
						name = s.Name.Name
						p := fset.Position(s.Name.Pos())
						if p.IsValid() && p.Line > 0 {
							line = p.Line
							utf16Col = byteColToUTF16Col(lines, p.Line, p.Column)
						}
					}
					return name, line, utf16Col
				case *ast.ValueSpec:
					var targetID *ast.Ident
					if len(s.Values) == len(s.Names) {
						for i, val := range s.Values {
							if val != nil && refPos >= val.Pos() && refPos <= val.End() {
								if i < len(s.Names) && s.Names[i] != nil && s.Names[i].Name != "_" {
									targetID = s.Names[i]
									break
								}
							}
						}
					}
					if targetID == nil {
						for _, id := range s.Names {
							if id != nil && id.Name != "_" {
								targetID = id
								break
							}
						}
					}
					if targetID != nil {
						name = targetID.Name
						p := fset.Position(targetID.Pos())
						if p.IsValid() && p.Line > 0 {
							line = p.Line
							utf16Col = byteColToUTF16Col(lines, p.Line, p.Column)
						}
					}
					return name, line, utf16Col
				}
			}
			return "", 0, 0
		}
	}
	return "", 0, 0
}

// ReferencesBatch resolves symbol references for multiple requests in parallel,
// leveraging shared package caches and single-pass repository scanning.
func (a *Analyzer) ReferencesBatch(ctx context.Context, reqs []LookupRequest) ([]*RefsResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	results := make([]*RefsResult, len(reqs))
	if len(reqs) == 1 {
		r, err := a.References(ctx, reqs[0])
		if err != nil {
			return nil, err
		}
		results[0] = r
		return results, nil
	}

	type resolvedItem struct {
		target       *resolvedTarget
		targetID     targetIdentity
		matcher      *methodMatcher
		symName      string
		declDir      string
		defBase      string
		defCleanFile string
		defLine      int
		defCol       int
		isUnexported bool
		refsCacheK   refsCacheKey
		canCacheRefs bool
		err          error
	}

	resolved := make([]resolvedItem, len(reqs))

	// Step 1: Parallel target resolution (min(8, len(reqs)))
	resWorkers := min(8, len(reqs))
	ch := make(chan int, len(reqs))
	for i := range reqs {
		ch <- i
	}
	close(ch)

	var resWg sync.WaitGroup
	for w := 0; w < resWorkers; w++ {
		resWg.Add(1)
		go func() {
			defer resWg.Done()
			for i := range ch {
				if ctx.Err() != nil {
					return
				}
				req := reqs[i]
				target, err := a.resolveTarget(ctx, req)
				if err != nil {
					resolved[i].err = err
					continue
				}
				if target == nil || target.obj == nil || target.obj.Pkg() == nil {
					continue
				}
				target.obj = unwrapOrigin(target.obj)
				symName := target.obj.Name()
				if pkgName, ok := target.obj.(*types.PkgName); ok && (symName == "" || symName == "_" || symName == ".") {
					if pkgName.Imported() != nil {
						symName = pkgName.Imported().Name()
						if symName == "" {
							symName = guessPkgName(pkgName.Imported().Path())
						}
					}
				}

				defPos := target.loader.positionFor(target.obj, target.rootPkg)
				defCleanFile := ""
				if defPos.Filename != "" {
					defCleanFile = filepath.Clean(defPos.Filename)
				}
				defBase := filepath.Base(defPos.Filename)
				defLine := defPos.Line
				defCol := defPos.Column

				targetID := resolveTargetIdentity(target.obj, target, defCleanFile, defLine, defCol, req.RepoRoot)
				declDir := targetID.declDir
				isUnexported := targetID.isUnexported

				var matcher *methodMatcher
				if targetID.isMethod {
					matcher = newMethodMatcher(target, targetID, defCleanFile, defBase, defLine, defCol)
				}

				var refsCacheK refsCacheKey
				var canCacheRefs bool
				if target.loader != nil && target.loader.revCache != nil && !targetID.isLocal && isImmutableRev(req.BaseRev) && isImmutableRev(target.rev) {
					canCacheRefs = true
					refsCacheK = refsCacheKey{
						baseRev:      req.BaseRev,
						targetRev:    target.rev,
						targetID:     targetID,
						defCleanFile: defCleanFile,
						defLine:      defLine,
						defCol:       defCol,
						isPkgClause:  target.isPkgClause,
					}
				}

				resolved[i] = resolvedItem{
					target:       target,
					targetID:     targetID,
					matcher:      matcher,
					symName:      symName,
					declDir:      declDir,
					defBase:      defBase,
					defCleanFile: defCleanFile,
					defLine:      defLine,
					defCol:       defCol,
					isUnexported: isUnexported,
					refsCacheK:   refsCacheK,
					canCacheRefs: canCacheRefs,
				}
			}
		}()
	}
	resWg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	type batchTarget struct {
		reqIdx                      int
		req                         LookupRequest
		target                      *resolvedTarget
		targetID                    targetIdentity
		matcher                     *methodMatcher
		declDir                     string
		defCleanFile                string
		defLine                     int
		defCol                      int
		refsCacheK                  refsCacheKey
		canCacheRefs                bool
		requireImportOutsideDeclDir bool
		res                         *RefsResult
		mu                          sync.Mutex
		seen                        map[refKey]bool
		dirCandidates               map[string][]string
	}

	type batchGroupKey struct {
		repoRoot  string
		rev       string
		baseRev   string
		targetRev string
	}

	groups := make(map[batchGroupKey][]*batchTarget)

	// Process resolved items: handle immediately or collect into in-repo batch groups
	for i, item := range resolved {
		if item.err != nil {
			return nil, item.err
		}
		if item.target == nil {
			results[i] = &RefsResult{Found: false}
			continue
		}
		if item.canCacheRefs {
			if cached, ok := item.target.loader.revCache.refsCache.get(item.refsCacheK); ok {
				results[i] = cached
				continue
			}
		}
		// If target is local, package clause, or on disk, delegate directly to References
		if item.target.isPkgClause || item.targetID.isLocal || item.target.isDisk || reqs[i].Repo == nil {
			r, err := a.References(ctx, reqs[i])
			if err != nil {
				return nil, err
			}
			results[i] = r
			continue
		}

		// Collect in-repo target for unified batch scan
		sig := formatSignature(item.target.obj, item.target.rootPkg)
		sigHTML := highlightSignature(sig)
		t := &batchTarget{
			reqIdx:                      i,
			req:                         reqs[i],
			target:                      item.target,
			targetID:                    item.targetID,
			matcher:                     item.matcher,
			declDir:                     item.declDir,
			defCleanFile:                item.defCleanFile,
			defLine:                     item.defLine,
			defCol:                      item.defCol,
			refsCacheK:                  item.refsCacheK,
			canCacheRefs:                item.canCacheRefs,
			requireImportOutsideDeclDir: !item.targetID.isPkgName && !item.targetID.isField && !item.targetID.isMethod && !item.targetID.isLocal && item.targetID.pkgPath != "" && item.targetID.pkgPath != ".",
			res: &RefsResult{
				Found:         true,
				SymbolName:    item.symName,
				Signature:     sig,
				SignatureHTML: sigHTML,
			},
			seen:          make(map[refKey]bool),
			dirCandidates: make(map[string][]string),
		}

		gk := batchGroupKey{
			repoRoot:  reqs[i].RepoRoot,
			rev:       item.target.rev,
			baseRev:   reqs[i].BaseRev,
			targetRev: reqs[i].TargetRev,
		}
		groups[gk] = append(groups[gk], t)
	}

	if len(groups) == 0 {
		return results, nil
	}

	const maxRefs = 200

	// Step 2: For each group, perform single-pass repo scan & multi-target directory extraction
	for _, groupTargets := range groups {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		first := groupTargets[0]
		repo := first.req.Repo
		rev := first.target.rev
		baseRev := first.req.BaseRev
		targetRev := first.req.TargetRev
		loader := first.target.loader

		changedFiles := make(map[string]bool)
		if repo != nil && baseRev != "" && targetRev != "" {
			if loader != nil && loader.revCache != nil {
				changedFiles = loader.revCache.getChangedFiles(repo, baseRev, targetRev)
			} else if files, err := repo.Files(baseRev, targetRev); err == nil {
				for _, f := range files {
					if f != nil && f.Status != vcs.FileDeleted {
						changedFiles[f.Path] = true
					}
				}
			}
		}

		var goFiles []string
		var fileIdx *repoFileIndex
		if loader != nil && loader.revCache != nil {
			goFiles, _ = loader.revCache.getGoFiles(repo, rev)
			fileIdx, _ = loader.revCache.getRepoFileIndex(ctx, repo, rev)
		} else if repo != nil {
			goFiles, _ = repo.ListGoFiles(rev)
		}

		type dirCandInfo struct {
			dir     string
			files   []string
			targets []*batchTarget
		}
		candDirsMap := make(map[string]*dirCandInfo)
		var orderedCandDirs []string
		sharedContentMap := make(map[string][]byte)

		type fileMatch struct {
			fPath        string
			fDir         string
			matchTargets []*batchTarget
		}
		var fileMatches []fileMatch
		var candBatchPaths []string
		var candBatchOIDs []string

		for _, fPath := range goFiles {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			fDir := path.Dir(fPath)
			if fDir == "." {
				fDir = ""
			}

			var matchTargets []*batchTarget
			for _, t := range groupTargets {
				if t.targetID.isUnexported && fDir != t.declDir {
					continue
				}
				needImport := t.targetID.isPkgName || (fDir != t.declDir && t.requireImportOutsideDeclDir)
				if fileIdx != nil {
					if needImport && !fileIdx.hasImport(fPath, t.targetID.pkgPath) {
						continue
					}
					if !t.targetID.isPkgName && !fileIdx.mayContainIdent(fPath, t.target.obj.Name()) {
						continue
					}
				}
				matchTargets = append(matchTargets, t)
			}
			if len(matchTargets) == 0 {
				continue
			}
			fileMatches = append(fileMatches, fileMatch{fPath: fPath, fDir: fDir, matchTargets: matchTargets})
			candBatchPaths = append(candBatchPaths, fPath)
			var oid string
			if fileIdx != nil {
				if i, ok := fileIdx.byPath[fPath]; ok {
					oid = fileIdx.files[i].oid
				}
			}
			candBatchOIDs = append(candBatchOIDs, oid)
		}

		candContents := readRepoFilesBatch(repo, rev, candBatchPaths, candBatchOIDs)

		for _, fm := range fileMatches {
			fPath := fm.fPath
			fDir := fm.fDir
			content := candContents[fPath]
			if len(content) == 0 {
				continue
			}

			for _, t := range fm.matchTargets {
				needImport := t.targetID.isPkgName || (fDir != t.declDir && t.requireImportOutsideDeclDir)
				if needImport && (fileIdx == nil || !fileIdx.isImportExact(fPath)) {
					if !containsImportLiteral(content, t.targetID.pkgPath) {
						continue
					}
				}
				if !t.targetID.isPkgName {
					if !containsGoIdent(content, t.target.obj.Name()) {
						continue
					}
				}

				t.dirCandidates[fDir] = append(t.dirCandidates[fDir], fPath)
				dInfo, exists := candDirsMap[fDir]
				if !exists {
					dInfo = &dirCandInfo{dir: fDir}
					candDirsMap[fDir] = dInfo
					orderedCandDirs = append(orderedCandDirs, fDir)
				}
				if !slices.Contains(dInfo.targets, t) {
					dInfo.targets = append(dInfo.targets, t)
				}
				if !slices.Contains(dInfo.files, fPath) {
					dInfo.files = append(dInfo.files, fPath)
				}
				sharedContentMap[fPath] = content
			}
		}

		slices.SortStableFunc(orderedCandDirs, func(a, b string) int {
			hasChangedA := false
			hasChangedB := false
			for _, f := range candDirsMap[a].files {
				if changedFiles[f] {
					hasChangedA = true
					break
				}
			}
			for _, f := range candDirsMap[b].files {
				if changedFiles[f] {
					hasChangedB = true
					break
				}
			}
			if hasChangedA != hasChangedB {
				if hasChangedA {
					return -1
				}
				return 1
			}
			return strings.Compare(a, b)
		})

		dirWorkers := min(len(orderedCandDirs), max(1, runtime.NumCPU()), 12)
		if dirWorkers > 0 {
			sem := make(chan struct{}, dirWorkers)
			var dirWg sync.WaitGroup

			for _, d := range orderedCandDirs {
				if ctx.Err() != nil {
					break
				}
				dInfo := candDirsMap[d]
				dirWg.Add(1)
				sem <- struct{}{}
				go func(d string, dInfo *dirCandInfo) {
					defer dirWg.Done()
					defer func() { <-sem }()
					if ctx.Err() != nil {
						return
					}

					dirContentMap := make(map[string][]byte)
					for _, f := range dInfo.files {
						if b, ok := sharedContentMap[f]; ok {
							dirContentMap[f] = b
						}
					}

					for _, t := range dInfo.targets {
						if ctx.Err() != nil {
							return
						}
						candFiles := t.dirCandidates[d]
						if len(candFiles) == 0 {
							continue
						}

						var preParsedASTs []*ast.File
						var preParsedInfo *types.Info
						if d == t.target.dir && !t.target.isChained {
							preParsedASTs = t.target.pkgASTFiles
							preParsedInfo = t.target.info
						}

						ldr := t.target.loader.clone()
						var dirRefs []Reference
						dirAddRef := func(ref Reference) bool {
							dirRefs = append(dirRefs, ref)
							return len(dirRefs) < maxRefs
						}

						_ = a.searchRepoDirWithCandidates(ctx, ldr, t.target.rootPkg, t.target.rev, d, candFiles, dirContentMap, t.targetID, t.matcher, t.defCleanFile, t.defLine, t.defCol, changedFiles, t.declDir, preParsedASTs, preParsedInfo, dirAddRef)

						t.mu.Lock()
						if len(dirRefs) >= maxRefs {
							t.res.Truncated = true
						}
						for _, ref := range dirRefs {
							k := refKey{display: ref.Display, line: ref.Line}
							if !t.seen[k] {
								t.seen[k] = true
								t.res.Refs = append(t.res.Refs, ref)
							}
						}
						t.mu.Unlock()
					}
				}(d, dInfo)
			}
			dirWg.Wait()
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		for _, t := range groupTargets {
			sortRefs(t.res.Refs)
			if len(t.res.Refs) >= maxRefs {
				t.res.Truncated = true
				if len(t.res.Refs) > maxRefs {
					t.res.Refs = t.res.Refs[:maxRefs]
				}
			}
			if t.canCacheRefs && t.res != nil {
				t.target.loader.revCache.refsCache.put(t.refsCacheK, t.res)
			}
			results[t.reqIdx] = cloneRefsResult(t.res)
		}
	}

	return results, nil
}
