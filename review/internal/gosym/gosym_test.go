package gosym

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/vcs"
)

func setupTestGitRepo(tb testing.TB) (vcs.Repo, string) {
	tb.Helper()
	dir := tb.TempDir()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			tb.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goModContent := `module example.com/testmod

go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		tb.Fatal(err)
	}

	calcDir := filepath.Join(dir, "pkg", "calc")
	if err := os.MkdirAll(calcDir, 0755); err != nil {
		tb.Fatal(err)
	}

	calcContent := `package calc

// Add returns the sum of two integers.
func Add(a, b int) int {
	return a + b
}

// Sub subtracts b from a.
func Sub(a, b int) int {
	return a - b
}
`
	if err := os.WriteFile(filepath.Join(calcDir, "calc.go"), []byte(calcContent), 0644); err != nil {
		tb.Fatal(err)
	}

	cmdDir := filepath.Join(dir, "cmd")
	if err := os.MkdirAll(cmdDir, 0755); err != nil {
		tb.Fatal(err)
	}

	mainContent := `package main

import (
	"fmt"
	"net/http"

	"example.com/testmod/pkg/calc"
)

func runAdd() {
	res := calc.Add(10, 20)
	fmt.Println(res)
}

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	r.Header.Set("X-Custom", "review")
}

func main() {
	runAdd()
}
`
	if err := os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte(mainContent), 0644); err != nil {
		tb.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "initial commit")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		tb.Fatalf("vcs.Open failed: %v", err)
	}
	return repo, dir
}

func TestHermeticInRepoLookup(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	// Verify pure stdlib hermetic resolution without any external binary
	t.Setenv("PATH", "")

	analyzer := NewAnalyzer()
	defer analyzer.Close()

	ctx := context.Background()

	// Line 11 in cmd/main.go: "	res := calc.Add(10, 20)"
	// Find column of "Add"
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "working",
		TargetRev: "working",
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      11,
		UTF16Col:  13, // "	res := calc.Add" -> index of 'A' is 13
	}

	res, err := analyzer.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected symbol to be found, got not found")
	}

	if res.SymbolName != "Add" {
		t.Errorf("SymbolName = %q, want %q", res.SymbolName, "Add")
	}
	if !res.InRepo {
		t.Errorf("InRepo = false, want true")
	}
	if res.DefPath != "pkg/calc/calc.go" {
		t.Errorf("DefPath = %q, want %q", res.DefPath, "pkg/calc/calc.go")
	}
	if res.DefLine != 4 {
		t.Errorf("DefLine = %d, want 4", res.DefLine)
	}
	if !strings.Contains(res.Signature, "Add(a int, b int) int") {
		t.Errorf("Signature = %q, want Add(a int, b int) int", res.Signature)
	}
	if len(res.Snippet) == 0 {
		t.Errorf("expected snippet lines, got 0")
	}
}

func TestStdlibLookup(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()

	ctx := context.Background()

	// 1. Test r.Header.Set in cmd/main.go (line 17: "	r.Header.Set(\"X-Custom\", \"review\")")
	// Line 17: "\tr.Header.Set" -> 'S' in Set is at col 10
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      17,
		UTF16Col:  10,
	}

	res, err := analyzer.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("Lookup Header.Set failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected Header.Set to be found, got not found")
	}
	if res.SymbolName != "Set" {
		t.Errorf("SymbolName = %q, want Set", res.SymbolName)
	}
	if res.InRepo {
		t.Errorf("InRepo = true, want false for stdlib")
	}
	if res.DefPkg != "net/http" {
		t.Errorf("DefPkg = %q, want net/http", res.DefPkg)
	}
	if res.DefFile != "header.go" {
		t.Errorf("DefFile = %q, want header.go", res.DefFile)
	}
	if res.PkgGoDevURL != "https://pkg.go.dev/net/http#Header.Set" {
		t.Errorf("PkgGoDevURL = %q, want https://pkg.go.dev/net/http#Header.Set", res.PkgGoDevURL)
	}
	if len(res.Snippet) == 0 {
		t.Errorf("expected snippet lines, got 0")
	}

	// 2. Test w.WriteHeader (interface method on ResponseWriter)
	// Line 16: "	w.WriteHeader(http.StatusOK)" -> 'W' is at col 3
	req2 := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      16,
		UTF16Col:  3,
	}

	res2, err := analyzer.Lookup(ctx, req2)
	if err != nil {
		t.Fatalf("Lookup WriteHeader failed: %v", err)
	}
	if !res2.Found {
		t.Fatalf("expected WriteHeader to be found, got not found")
	}
	if res2.SymbolName != "WriteHeader" {
		t.Errorf("SymbolName = %q, want WriteHeader", res2.SymbolName)
	}
	if res2.PkgGoDevURL != "https://pkg.go.dev/net/http#ResponseWriter.WriteHeader" {
		t.Errorf("PkgGoDevURL = %q, want https://pkg.go.dev/net/http#ResponseWriter.WriteHeader", res2.PkgGoDevURL)
	}
}

func TestBuiltinLookup(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()

	ctx := context.Background()

	// Add a file with builtin calls
	code := `package main

func useBuiltins() error {
	s := make([]int, 0)
	s = append(s, 1)
	return nil
}
`
	if err := os.WriteFile(filepath.Join(repoRoot, "cmd", "builtin.go"), []byte(code), 0644); err != nil {
		t.Fatal(err)
	}

	// Test append on line 5: "	s = append(s, 1)" -> 'a' is at col 5
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "working",
		TargetRev: "working",
		Side:      "R",
		FilePath:  "cmd/builtin.go",
		Line:      5,
		UTF16Col:  5,
	}

	res, err := analyzer.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("Lookup append failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected append to be found")
	}
	if res.SymbolName != "append" {
		t.Errorf("SymbolName = %q, want append", res.SymbolName)
	}
	if res.DefPkg != "builtin" {
		t.Errorf("DefPkg = %q, want builtin", res.DefPkg)
	}
	if res.PkgGoDevURL != "https://pkg.go.dev/builtin#append" {
		t.Errorf("PkgGoDevURL = %q, want https://pkg.go.dev/builtin#append", res.PkgGoDevURL)
	}
	if !strings.Contains(res.Signature, "func append") {
		t.Errorf("Signature = %q, want func append...", res.Signature)
	}

	// Test error on line 3: "func useBuiltins() error {" -> 'e' is at col 19
	req2 := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "working",
		TargetRev: "working",
		Side:      "R",
		FilePath:  "cmd/builtin.go",
		Line:      3,
		UTF16Col:  19,
	}

	res2, err := analyzer.Lookup(ctx, req2)
	if err != nil {
		t.Fatalf("Lookup error failed: %v", err)
	}
	if !res2.Found {
		t.Fatalf("expected error to be found")
	}
	if res2.SymbolName != "error" {
		t.Errorf("SymbolName = %q, want error", res2.SymbolName)
	}
	if res2.DefPkg != "builtin" {
		t.Errorf("DefPkg = %q, want builtin", res2.DefPkg)
	}
}

func TestHistoricalAndSideL(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Create a commit with a function that will be deleted
	legacyFile := filepath.Join(repoRoot, "pkg", "calc", "legacy.go")
	legacyCode := `package calc

// OldFunc is a legacy function slated for removal.
func OldFunc() string {
	return "legacy"
}
`
	if err := os.WriteFile(legacyFile, []byte(legacyCode), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "add legacy")
	c1Rev := runGit("rev-parse", "HEAD")

	// Create commit 2 removing legacy.go
	runGit("rm", "pkg/calc/legacy.go")
	runGit("commit", "-m", "remove legacy")
	c2Rev := runGit("rev-parse", "HEAD")

	ctx := context.Background()

	// Side L lookup on legacy.go at line 4 ("func OldFunc() string {")
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   c1Rev,
		TargetRev: c2Rev,
		Side:      "L",
		FilePath:  "pkg/calc/legacy.go",
		Line:      4,
		UTF16Col:  5, // 'O' in OldFunc
	}

	res, err := analyzer.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("Lookup legacy OldFunc failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected OldFunc to be found on historical Side L")
	}
	if res.SymbolName != "OldFunc" {
		t.Errorf("SymbolName = %q, want OldFunc", res.SymbolName)
	}
}

func TestMultiByteUTF8Offset(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()

	ctx := context.Background()

	// In UTF-16, "🚀" is 2 code units, "🎉" is 2 code units.
	code := `package main

// Multi-byte test: 🚀🎉
const Planet = "Mars"

func check() {
	_ = "🚀🎉" + Planet
}
`
	if err := os.WriteFile(filepath.Join(repoRoot, "cmd", "utf8.go"), []byte(code), 0644); err != nil {
		t.Fatal(err)
	}

	// Line 7: "\t_ = \"🚀🎉\" + Planet"
	// \t (1) + _ (1) + ' ' (1) + = (1) + ' ' (1) + " (1) + 🚀 (2) + 🎉 (2) + " (1) + ' ' (1) + + (1) + ' ' (1) = 14 UTF-16 code units
	// 'P' in Planet is at UTF-16 offset 14.
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "working",
		TargetRev: "working",
		Side:      "R",
		FilePath:  "cmd/utf8.go",
		Line:      7,
		UTF16Col:  14,
	}

	res, err := analyzer.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("Lookup Planet failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected Planet to be found with emoji UTF-16 offset")
	}
	if res.SymbolName != "Planet" {
		t.Errorf("SymbolName = %q, want Planet", res.SymbolName)
	}
	if res.DefLine != 4 {
		t.Errorf("DefLine = %d, want 4", res.DefLine)
	}
}

func TestTestFileLookup(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()

	ctx := context.Background()

	// 1. Internal test file (package calc)
	calcTest := `package calc

import "testing"

func TestCalcAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fail()
	}
}
`
	if err := os.WriteFile(filepath.Join(repoRoot, "pkg", "calc", "calc_test.go"), []byte(calcTest), 0644); err != nil {
		t.Fatal(err)
	}

	// Line 6: "\tif Add(2, 3) != 5 {" -> 'A' is at col 4
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "working",
		TargetRev: "working",
		Side:      "R",
		FilePath:  "pkg/calc/calc_test.go",
		Line:      6,
		UTF16Col:  4,
	}

	res, err := analyzer.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("Lookup Add in internal test failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Add" {
		t.Fatalf("expected Add to be found in internal test, got %+v", res)
	}
	if res.DefPath != "pkg/calc/calc.go" {
		t.Errorf("DefPath = %q, want pkg/calc/calc.go", res.DefPath)
	}

	// 2. External test file (package calc_test)
	calcExtTest := `package calc_test

import (
	"testing"
	"example.com/testmod/pkg/calc"
)

func TestExternalAdd(t *testing.T) {
	if calc.Add(5, 5) != 10 {
		t.Fail()
	}
}
`
	if err := os.WriteFile(filepath.Join(repoRoot, "pkg", "calc", "calc_ext_test.go"), []byte(calcExtTest), 0644); err != nil {
		t.Fatal(err)
	}

	// Line 9: "\tif calc.Add(5, 5) != 10 {" -> 'A' in Add is at col 9
	req2 := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "working",
		TargetRev: "working",
		Side:      "R",
		FilePath:  "pkg/calc/calc_ext_test.go",
		Line:      9,
		UTF16Col:  9,
	}

	res2, err := analyzer.Lookup(ctx, req2)
	if err != nil {
		t.Fatalf("Lookup Add in external test failed: %v", err)
	}
	if !res2.Found || res2.SymbolName != "Add" {
		t.Fatalf("expected Add to be found in external test, got %+v", res2)
	}
	if res2.DefPath != "pkg/calc/calc.go" {
		t.Errorf("DefPath = %q, want pkg/calc/calc.go", res2.DefPath)
	}
}

func TestFullFileSnippet(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()

	ctx := context.Background()

	// Generate a 60-line Go file
	var sb strings.Builder
	sb.WriteString("package calc\n\n")
	sb.WriteString("// BigFileFunction does interesting work.\n")
	sb.WriteString("func BigFileFunction() int {\n")
	sb.WriteString("\treturn 42\n")
	sb.WriteString("}\n")
	for i := 7; i <= 60; i++ {
		sb.WriteString(fmt.Sprintf("// line %d padding comment\n", i))
	}

	if err := os.WriteFile(filepath.Join(repoRoot, "pkg", "calc", "big.go"), []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}

	// Lookup BigFileFunction at line 4
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "working",
		TargetRev: "working",
		Side:      "R",
		FilePath:  "pkg/calc/big.go",
		Line:      4,
		UTF16Col:  5,
	}

	res, err := analyzer.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("Lookup BigFileFunction failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected BigFileFunction to be found")
	}

	if res.SnippetTruncated {
		t.Errorf("SnippetTruncated = true, want false for 60-line file under 5,000 budget")
	}
	if len(res.Snippet) != 60 {
		t.Errorf("len(res.Snippet) = %d, want 60", len(res.Snippet))
	}
	if res.Snippet[0].Line != 1 {
		t.Errorf("first snippet line = %d, want 1", res.Snippet[0].Line)
	}
	if res.Snippet[len(res.Snippet)-1].Line != 60 {
		t.Errorf("last snippet line = %d, want 60", res.Snippet[len(res.Snippet)-1].Line)
	}
	if !res.Snippet[3].Active { // Line 4
		t.Errorf("expected line 4 to be active in snippet")
	}
}

func TestChainedLookup(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()

	ctx := context.Background()

	// In stdlib net/http/header.go, Header.Set calls CanonicalHeaderKey
	// or Header has comments referencing CanonicalHeaderKey or textproto.
	// Let's look up a symbol in net/http/header.go directly via chained lookup!
	// First, let's look up Header.Set to get its exact def_line in header.go
	initReq := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      17, // r.Header.Set
		UTF16Col:  10,
	}
	initRes, err := analyzer.Lookup(ctx, initReq)
	if err != nil || !initRes.Found {
		t.Fatalf("initial lookup failed: %v, %+v", err, initRes)
	}

	headerGoLine := initRes.DefLine
	if headerGoLine <= 0 {
		t.Fatalf("expected DefLine > 0, got %d", headerGoLine)
	}

	// Now chained lookup: directly inside net/http/header.go on line `headerGoLine`
	// at the Set method identifier.
	headerBytes, err := os.ReadFile(filepath.Join(getGoEnv().goroot, "src", "net", "http", "header.go"))
	if err != nil {
		t.Fatalf("read stdlib net/http/header.go: %v", err)
	}
	headerLines := strings.Split(string(headerBytes), "\n")
	if headerGoLine > len(headerLines) {
		t.Fatalf("headerGoLine=%d out of range (%d lines)", headerGoLine, len(headerLines))
	}
	col := strings.Index(headerLines[headerGoLine-1], "Set")
	if col < 0 {
		t.Fatalf("could not find Set on line %d (%q)", headerGoLine, headerLines[headerGoLine-1])
	}
	chainedReq := LookupRequest{
		Repo:          repo,
		RepoRoot:      repoRoot,
		BaseRev:       "HEAD",
		TargetRev:     "HEAD",
		Side:          "R",
		FilePath:      initRes.DefPath,
		Line:          headerGoLine,
		UTF16Col:      col,
		PkgImportPath: "net/http",
		PkgFile:       "header.go",
	}

	chainedRes, err := analyzer.Lookup(ctx, chainedReq)
	if err != nil {
		t.Fatalf("chained Lookup failed: %v", err)
	}
	if !chainedRes.Found {
		t.Fatalf("expected chained symbol to be found")
	}
	if chainedRes.SymbolName != "Set" {
		t.Errorf("SymbolName = %q, want Set", chainedRes.SymbolName)
	}
	if chainedRes.DefPkg != "net/http" {
		t.Errorf("DefPkg = %q, want net/http", chainedRes.DefPkg)
	}
}

func TestMultiLineSelectorAndTypeSwitch(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()

	code := `package main

import "net/http"

func multi(w http.ResponseWriter, v any) {
	w.
		WriteHeader(http.StatusAccepted)
	switch x := v.(type) {
	case string:
		_ = x
	}
}
`
	if err := os.WriteFile(filepath.Join(repoRoot, "cmd", "multi.go"), []byte(code), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. Multi-line interface method selector on line 7: "\t\tWriteHeader(http.StatusAccepted)"
	resSel, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "working",
		TargetRev: "working",
		Side:      "R",
		FilePath:  "cmd/multi.go",
		Line:      7,
		UTF16Col:  2, // 'W' in WriteHeader
	})
	if err != nil || !resSel.Found {
		t.Fatalf("multi-line selector lookup failed: err=%v res=%+v", err, resSel)
	}
	if resSel.PkgGoDevURL != "https://pkg.go.dev/net/http#ResponseWriter.WriteHeader" {
		t.Errorf("PkgGoDevURL = %q, want https://pkg.go.dev/net/http#ResponseWriter.WriteHeader", resSel.PkgGoDevURL)
	}

	// 2. TypeSwitchStmt implicit var on line 8: "\tswitch x := v.(type) {"
	resTS, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "working",
		TargetRev: "working",
		Side:      "R",
		FilePath:  "cmd/multi.go",
		Line:      8,
		UTF16Col:  8, // 'x'
	})
	if err != nil || !resTS.Found {
		t.Fatalf("type switch ident lookup failed: err=%v res=%+v", err, resTS)
	}
	if resTS.SymbolName != "x" {
		t.Errorf("SymbolName = %q, want x", resTS.SymbolName)
	}
}

func TestConcurrentColdDiskLookupAndStdRepo(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	freshAnalyzer := &Analyzer{diskCache: newDiskCache()}

	const workers = 6
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := freshAnalyzer.Lookup(context.Background(), LookupRequest{
				Repo:      repo,
				RepoRoot:  repoRoot,
				BaseRev:   "HEAD",
				TargetRev: "HEAD",
				Side:      "R",
				FilePath:  "cmd/main.go",
				Line:      17, // r.Header.Set
				UTF16Col:  10,
			})
			if err != nil {
				errs <- err
				return
			}
			if !res.Found || res.PkgGoDevURL != "https://pkg.go.dev/net/http#Header.Set" {
				errs <- fmt.Errorf("unexpected concurrent Lookup result: %+v", res)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Also verify Go stdlib repo layout (src/go.mod with module std) resolves in-repo!
	stdDir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = stdDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")
	if err := os.MkdirAll(filepath.Join(stdDir, "src", "foo"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stdDir, "src", "bar"), 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(stdDir, "src", "go.mod"), []byte("module std\n\ngo 1.24\n"), 0644)
	_ = os.WriteFile(filepath.Join(stdDir, "src", "foo", "foo.go"), []byte("package foo\n\n// Greet greets.\nfunc Greet() string { return \"hi\" }\n"), 0644)
	_ = os.WriteFile(filepath.Join(stdDir, "src", "bar", "bar.go"), []byte("package bar\n\nimport \"foo\"\n\nfunc Call() string { return foo.Greet() }\n"), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "std layout")

	stdRepo, err := vcs.Open(stdDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	resStd, err := freshAnalyzer.Lookup(context.Background(), LookupRequest{
		Repo:          stdRepo,
		RepoRoot:      stdDir,
		BaseRev:       "HEAD",
		TargetRev:     "HEAD",
		Side:          "R",
		Line:          5,
		UTF16Col:      32, // 'G' in foo.Greet()
		PkgImportPath: "bar",
		PkgFile:       "bar.go",
	})
	if err != nil || !resStd.Found {
		t.Fatalf("std repo chained lookup failed: err=%v res=%+v", err, resStd)
	}
	if !resStd.InRepo || resStd.DefPath != "src/foo/foo.go" {
		t.Errorf("expected in_repo src/foo/foo.go, got InRepo=%v DefPath=%q", resStd.InRepo, resStd.DefPath)
	}
}

func TestAnalyzerRevCache(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	headSHA := strings.TrimSpace(string(out))
	if len(headSHA) != 40 {
		t.Fatalf("expected 40-char SHA, got %q", headSHA)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()

	// 1. "working" is mutable, so it should not be cached in revCaches.
	reqWorking := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "working",
		TargetRev: "working",
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      11,
		UTF16Col:  13,
	}
	resW, err := analyzer.Lookup(context.Background(), reqWorking)
	if err != nil || !resW.Found {
		t.Fatalf("working lookup failed: %v", err)
	}
	analyzer.mu.Lock()
	numCaches := len(analyzer.revCaches)
	analyzer.mu.Unlock()
	if numCaches != 0 {
		t.Errorf("expected 0 revCaches for 'working', got %d", numCaches)
	}

	// 2. Immutable 40-char commit SHA should be cached in revCaches.
	reqCommit := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   headSHA,
		TargetRev: headSHA,
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      11,
		UTF16Col:  13,
	}
	resC, err := analyzer.Lookup(context.Background(), reqCommit)
	if err != nil || !resC.Found {
		t.Fatalf("commit lookup failed: %v", err)
	}

	key := revCacheKey{repoRoot: filepath.Clean(repoRoot), rev: headSHA}
	analyzer.mu.Lock()
	cached, ok := analyzer.revCaches[key]
	analyzer.mu.Unlock()
	if !ok || cached == nil {
		t.Fatalf("expected revCache to be populated for %v", key)
	}

	// 3. Subsequent commit lookup succeeds.
	resC2, err := analyzer.Lookup(context.Background(), reqCommit)
	if err != nil || !resC2.Found {
		t.Fatalf("second commit lookup failed: %v", err)
	}

	// 4. Evict should clear the cache for this repoRoot.
	analyzer.Evict(repoRoot)
	analyzer.mu.Lock()
	_, okAfter := analyzer.revCaches[key]
	analyzer.mu.Unlock()
	if okAfter {
		t.Errorf("expected revCache entry to be evicted after Evict(%q)", repoRoot)
	}
}

func TestCrossFileSetPosCollision(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/coll\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Small imported in-repo package
	smallDir := filepath.Join(dir, "pkg", "small")
	if err := os.MkdirAll(smallDir, 0755); err != nil {
		t.Fatal(err)
	}
	smallContent := `package small

// TargetIface defines a target interface.
type TargetIface interface {
	Do()
}
`
	if err := os.WriteFile(filepath.Join(smallDir, "small.go"), []byte(smallContent), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Large root package (~100KB) so loader.fset spans token.Pos 1..100000+
	hugeDir := filepath.Join(dir, "pkg", "huge")
	if err := os.MkdirAll(hugeDir, 0755); err != nil {
		t.Fatal(err)
	}
	padContent := "package huge\n\n" + strings.Repeat("// padding comment line to expand fset pos space\n", 1500) + "var X = 1\n"
	if err := os.WriteFile(filepath.Join(hugeDir, "a.go"), []byte(padContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hugeDir, "b.go"), []byte(padContent), 0644); err != nil {
		t.Fatal(err)
	}

	serveContent := `package huge

import (
	"context"

	"example.com/coll/pkg/small"
)

// Serve handles server requests.
func Serve(ctx context.Context, t small.TargetIface) {
	t.Do()
}
`
	if err := os.WriteFile(filepath.Join(hugeDir, "serve.go"), []byte(serveContent), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "initial commit")
	headSHA := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()

	ctx := context.Background()

	// In serve.go:
	// line 10: "func Serve(ctx context.Context, t small.TargetIface) {"
	// Index of 'c' in "context.Context": col 16
	// Index of 'C' in "context.Context": col 24
	// Index of 'T' in "small.TargetIface": col 41

	// Test 1: Resolve stdlib `Context`
	reqContext := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headSHA,
		TargetRev: headSHA,
		Side:      "R",
		FilePath:  "pkg/huge/serve.go",
		Line:      10,
		UTF16Col:  24, // 'C' in Context
	}
	resCtx, err := analyzer.Lookup(ctx, reqContext)
	if err != nil || !resCtx.Found {
		t.Fatalf("Context lookup failed: err=%v res=%+v", err, resCtx)
	}
	if resCtx.InRepo {
		t.Errorf("expected Context to have InRepo=false, got true")
	}
	if !strings.HasSuffix(resCtx.DisplayPath, "context.go") {
		t.Errorf("expected Context DisplayPath ending with context.go, got %q", resCtx.DisplayPath)
	}
	if !strings.Contains(resCtx.Signature, "type Context interface") {
		t.Errorf("expected Context signature 'type Context interface', got %q", resCtx.Signature)
	}

	// Test 2: Resolve in-repo `TargetIface` (cold)
	reqIface := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headSHA,
		TargetRev: headSHA,
		Side:      "R",
		FilePath:  "pkg/huge/serve.go",
		Line:      10,
		UTF16Col:  41, // 'T' in TargetIface
	}
	resIface, err := analyzer.Lookup(ctx, reqIface)
	if err != nil || !resIface.Found {
		t.Fatalf("TargetIface cold lookup failed: err=%v res=%+v", err, resIface)
	}
	if !resIface.InRepo {
		t.Errorf("expected TargetIface to have InRepo=true, got false")
	}
	if resIface.DefPath != "pkg/small/small.go" {
		t.Errorf("expected DefPath 'pkg/small/small.go', got %q", resIface.DefPath)
	}
	if resIface.DefLine != 4 {
		t.Errorf("expected DefLine 4 for TargetIface, got %d", resIface.DefLine)
	}

	// Test 3: Resolve in-repo `TargetIface` (warm, hitting revCache)
	resIfaceWarm, err := analyzer.Lookup(ctx, reqIface)
	if err != nil || !resIfaceWarm.Found {
		t.Fatalf("TargetIface warm lookup failed: err=%v res=%+v", err, resIfaceWarm)
	}
	if resIfaceWarm.DefPath != "pkg/small/small.go" || resIfaceWarm.DefLine != 4 {
		t.Errorf("warm lookup wrong location: %s:%d", resIfaceWarm.DefPath, resIfaceWarm.DefLine)
	}
}

func TestDownloadedModCacheLookup(t *testing.T) {
	tempDir := t.TempDir()
	modCacheDir := filepath.Join(tempDir, "modcache")

	// Pinned extmod with transitive leaf
	extmodDir := filepath.Join(modCacheDir, "example.com", "extmod@v1.2.0")
	if err := os.MkdirAll(extmodDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extmodDir, "go.mod"), []byte("module example.com/extmod\n\ngo 1.22\n\nrequire example.com/leaf v0.3.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	extmodCode := `package extmod

import "example.com/leaf"

// Helper returns a combined string.
func Helper() string {
	return "extmod:" + leaf.LeafValue
}
`
	if err := os.WriteFile(filepath.Join(extmodDir, "ext.go"), []byte(extmodCode), 0644); err != nil {
		t.Fatal(err)
	}

	// Leaf module
	leafDir := filepath.Join(modCacheDir, "example.com", "leaf@v0.3.0")
	if err := os.MkdirAll(leafDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leafDir, "go.mod"), []byte("module example.com/leaf\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	leafCode := `package leaf

// LeafValue is a leaf constant.
const LeafValue = "green"
`
	if err := os.WriteFile(filepath.Join(leafDir, "leaf.go"), []byte(leafCode), 0644); err != nil {
		t.Fatal(err)
	}

	// Untidied module versions: v0.2.0 vs v0.10.0
	untidiedOldDir := filepath.Join(modCacheDir, "example.com", "untidied@v0.2.0", "pkg")
	if err := os.MkdirAll(untidiedOldDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(untidiedOldDir), "go.mod"), []byte("module example.com/untidied\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(untidiedOldDir, "old.go"), []byte("package pkg\nconst Ver = 2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	untidiedNewDir := filepath.Join(modCacheDir, "example.com", "untidied@v0.10.0", "pkg")
	if err := os.MkdirAll(untidiedNewDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(untidiedNewDir), "go.mod"), []byte("module example.com/untidied\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	untidiedNewCode := `package pkg

// UntidiedSymbol is from the higher version.
func UntidiedSymbol() int {
	return 42
}
`
	if err := os.WriteFile(filepath.Join(untidiedNewDir, "new.go"), []byte(untidiedNewCode), 0644); err != nil {
		t.Fatal(err)
	}

	// Local replaced directory (sibling to repo)
	localrepDir := filepath.Join(tempDir, "localrep", "sub")
	if err := os.MkdirAll(localrepDir, 0755); err != nil {
		t.Fatal(err)
	}
	subCode := `package sub

// ReplacedFunc is in local replaced directory.
func ReplacedFunc() string {
	return "from-replaced"
}
`
	if err := os.WriteFile(filepath.Join(localrepDir, "sub.go"), []byte(subCode), 0644); err != nil {
		t.Fatal(err)
	}

	// Test repo
	repoDir := filepath.Join(tempDir, "myrepo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goMod := `module example.com/myrepo

go 1.22

require example.com/extmod v1.2.0

replace example.com/replaced => ../localrep
`
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	mainGo := `package main

import (
	"example.com/extmod"
	"example.com/replaced/sub"
	"example.com/untidied/pkg"
)

func CallExt() {
	_ = extmod.Helper()
	_ = sub.ReplacedFunc()
	_ = pkg.UntidiedSymbol()
}
`
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", "go.mod", "main.go")
	runGit("commit", "-m", "init")
	headSHA := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(repoDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	realEnv := getGoEnv()
	cleanup := setTestGoEnv(&goEnv{
		goroot:     realEnv.goroot,
		gomodcache: modCacheDir,
		gopath:     filepath.Join(tempDir, "gopath"),
		goos:       realEnv.goos,
		goarch:     realEnv.goarch,
	})
	defer cleanup()

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. Lookup Helper from pinned extmod (with transitive leaf)
	req1 := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   headSHA,
		TargetRev: headSHA,
		Side:      "R",
		FilePath:  "main.go",
		Line:      10,
		UTF16Col:  13, // 'H' in Helper
	}
	res1, err := analyzer.Lookup(ctx, req1)
	if err != nil || !res1.Found {
		t.Fatalf("Helper lookup failed: err=%v res=%+v", err, res1)
	}
	if res1.InRepo {
		t.Errorf("expected Helper InRepo=false")
	}
	if res1.DefPkg != "example.com/extmod" {
		t.Errorf("DefPkg = %q, want example.com/extmod", res1.DefPkg)
	}
	if res1.PkgGoDevURL != "https://pkg.go.dev/example.com/extmod#Helper" {
		t.Errorf("PkgGoDevURL = %q, want https://pkg.go.dev/example.com/extmod#Helper", res1.PkgGoDevURL)
	}

	// 2. Subpackage replace lookup: ReplacedFunc
	req2 := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   headSHA,
		TargetRev: headSHA,
		Side:      "R",
		FilePath:  "main.go",
		Line:      11,
		UTF16Col:  10, // 'R' in ReplacedFunc
	}
	res2, err := analyzer.Lookup(ctx, req2)
	if err != nil || !res2.Found {
		t.Fatalf("ReplacedFunc lookup failed: err=%v res=%+v", err, res2)
	}
	if res2.SymbolName != "ReplacedFunc" {
		t.Errorf("SymbolName = %q, want ReplacedFunc", res2.SymbolName)
	}

	// 3. Untidied import lookup: resolved from highest semver (v0.10.0 over v0.2.0)
	req3 := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   headSHA,
		TargetRev: headSHA,
		Side:      "R",
		FilePath:  "main.go",
		Line:      12,
		UTF16Col:  10, // 'U' in UntidiedSymbol
	}
	res3, err := analyzer.Lookup(ctx, req3)
	if err != nil || !res3.Found {
		t.Fatalf("UntidiedSymbol lookup failed: err=%v res=%+v", err, res3)
	}
	if res3.SymbolName != "UntidiedSymbol" {
		t.Errorf("SymbolName = %q, want UntidiedSymbol", res3.SymbolName)
	}
	if !strings.Contains(res3.DisplayPath, "v0.10.0") {
		t.Errorf("expected DisplayPath containing v0.10.0, got %q", res3.DisplayPath)
	}

	// 4. Chained lookup inside extmod resolving leaf
	req4 := LookupRequest{
		Repo:          repo,
		RepoRoot:      repoDir,
		BaseRev:       headSHA,
		TargetRev:     headSHA,
		Side:          "R",
		FilePath:      "main.go",
		PkgImportPath: "example.com/extmod",
		PkgFile:       "ext.go",
		Line:          7,
		UTF16Col:      25, // 'L' in LeafValue
	}
	res4, err := analyzer.Lookup(ctx, req4)
	if err != nil || !res4.Found {
		t.Fatalf("chained LeafValue lookup failed: err=%v res=%+v", err, res4)
	}
	if res4.SymbolName != "LeafValue" {
		t.Errorf("SymbolName = %q, want LeafValue", res4.SymbolName)
	}
	if res4.DefPkg != "example.com/leaf" {
		t.Errorf("DefPkg = %q, want example.com/leaf", res4.DefPkg)
	}

	// 5. Concurrent lookup subtest under -race
	t.Run("Concurrent", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(4)
			go func() {
				defer wg.Done()
				r, err := analyzer.Lookup(ctx, req1)
				if err != nil || !r.Found {
					t.Errorf("concurrent req1 lookup failed: %v", err)
				}
			}()
			go func() {
				defer wg.Done()
				r, err := analyzer.Lookup(ctx, req2)
				if err != nil || !r.Found {
					t.Errorf("concurrent req2 lookup failed: %v", err)
				}
			}()
			go func() {
				defer wg.Done()
				r, err := analyzer.Lookup(ctx, req3)
				if err != nil || !r.Found {
					t.Errorf("concurrent req3 lookup failed: %v", err)
				}
			}()
			go func() {
				defer wg.Done()
				r, err := analyzer.Lookup(ctx, req4)
				if err != nil || !r.Found {
					t.Errorf("concurrent req4 lookup failed: %v", err)
				}
			}()
		}
		wg.Wait()
	})
}

func TestResolveExternalFile(t *testing.T) {
	repo, _ := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. Resolve standard library file: context.go
	disp, content, ok := analyzer.ResolveExternalFile(ctx, ExternalFileRequest{
		PkgImportPath: "context",
		PkgFile:       "context.go",
		Repo:          repo,
		Rev:           "master",
	})
	if !ok {
		t.Fatalf("expected ResolveExternalFile to succeed for context/context.go")
	}
	if disp != "context/context.go" {
		t.Errorf("displayPath = %q, want context/context.go", disp)
	}
	if !strings.Contains(string(content), "type Context interface") {
		t.Errorf("expected content to contain Context interface")
	}

	// 2. Resolve builtin.go
	dispBuiltin, contentBuiltin, ok := analyzer.ResolveExternalFile(ctx, ExternalFileRequest{
		PkgImportPath: "builtin",
		PkgFile:       "builtin.go",
		Repo:          repo,
		Rev:           "master",
	})
	if !ok {
		t.Fatalf("expected ResolveExternalFile to succeed for builtin/builtin.go")
	}
	if dispBuiltin != "builtin/builtin.go" {
		t.Errorf("displayPath = %q, want builtin/builtin.go", dispBuiltin)
	}
	if !strings.Contains(string(contentBuiltin), "type error interface") {
		t.Errorf("expected builtin.go to contain 'type error interface'")
	}

	// 3. Security validation: path traversal, absolute path, non-go suffix
	invalids := []ExternalFileRequest{
		{PkgImportPath: "context", PkgFile: "../context.go"},
		{PkgImportPath: "context", PkgFile: "/context.go"},
		{PkgImportPath: "context", PkgFile: "foo.txt"},
		{PkgImportPath: "context", PkgFile: "sub/context.go"},
		{PkgImportPath: "", PkgFile: "context.go"},
	}
	for _, inv := range invalids {
		if _, _, ok := analyzer.ResolveExternalFile(ctx, inv); ok {
			t.Errorf("expected invalid request %+v to fail, but got ok=true", inv)
		}
	}
}

func TestBackgroundWarmup(t *testing.T) {
	repo, dir := setupTestGitRepo(t)
	_ = repo
	analyzer := NewAnalyzer()
	defer analyzer.Close()

	var idle atomic.Bool
	idleChecked := make(chan struct{}, 1)
	analyzer.SetIdleCheck(func() bool {
		select {
		case idleChecked <- struct{}{}:
		default:
		}
		return idle.Load()
	})

	analyzer.StartBackgroundWarmup()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out))
	}
	headSHA := runGit("rev-parse", "HEAD")

	analyzer.EnqueueWarm(WarmRequest{
		RepoRoot: dir,
		Rev:      headSHA,
		Files:    []string{"cmd/main.go"},
	})

	// Wait for warmup worker to check idleCheck while idle == false
	select {
	case <-idleChecked:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for background warmup idleCheck")
	}

	// Unpause warmup
	idle.Store(true)

	// Wait for background warmup to populate revCache
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	warmed := false
	for !warmed {
		select {
		case <-timeout:
			t.Fatal("timed out waiting for background warmup to populate cache")
		case <-ticker.C:
			rc := analyzer.getRevCache(dir, headSHA)
			if rc != nil {
				rc.mu.RLock()
				if len(rc.pkgs) > 0 {
					warmed = true
				}
				rc.mu.RUnlock()
			}
		}
	}
}

func TestCanRefs(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. User declared func (calc.Add) -> CanRefs = true
	reqAdd := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      11,
		UTF16Col:  13, // 'A' in Add
	}
	resAdd, err := analyzer.Lookup(ctx, reqAdd)
	if err != nil {
		t.Fatalf("Lookup Add failed: %v", err)
	}
	if !resAdd.Found || !resAdd.CanRefs {
		t.Errorf("expected CanRefs=true for user func Add, got found=%v, can_refs=%v", resAdd.Found, resAdd.CanRefs)
	}

	// 2. Local var (res) -> CanRefs = true
	reqLocal := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      11,
		UTF16Col:  2, // 'r' in res
	}
	resLocal, err := analyzer.Lookup(ctx, reqLocal)
	if err != nil {
		t.Fatalf("Lookup res failed: %v", err)
	}
	if !resLocal.Found || !resLocal.CanRefs {
		t.Errorf("expected CanRefs=true for local var res, got found=%v, can_refs=%v", resLocal.Found, resLocal.CanRefs)
	}

	// 3. Built-in (nil or int) in pkg/calc/calc.go -> CanRefs = false
	// func Add(a, b int) int
	reqBuiltin := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/calc/calc.go",
		Line:      4,
		UTF16Col:  16, // 'i' in int
	}
	resBuiltin, err := analyzer.Lookup(ctx, reqBuiltin)
	if err != nil {
		t.Fatalf("Lookup int failed: %v", err)
	}
	if !resBuiltin.Found {
		t.Fatalf("expected int to be found")
	}
	if resBuiltin.CanRefs {
		t.Errorf("expected CanRefs=false for builtin int, got true")
	}
}

func TestReferencesLocalVar(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Line 11: "\tres := calc.Add(10, 20)"
	// Line 12: "\tfmt.Println(res)"
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      11,
		UTF16Col:  2, // 'r' in res
	}

	res, err := analyzer.References(ctx, req)
	if err != nil {
		t.Fatalf("References failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected local var references to be found")
	}
	if res.SymbolName != "res" {
		t.Errorf("SymbolName = %q, want 'res'", res.SymbolName)
	}
	// Exactly 1 reference at line 12 (definition at line 11 excluded)
	if len(res.Refs) != 1 {
		t.Fatalf("expected 1 reference, got %d: %+v", len(res.Refs), res.Refs)
	}
	ref := res.Refs[0]
	if ref.Line != 12 {
		t.Errorf("ref.Line = %d, want 12", ref.Line)
	}
	if !ref.InRepo {
		t.Errorf("ref.InRepo = false, want true")
	}
	if ref.Path != "cmd/main.go" {
		t.Errorf("ref.Path = %q, want 'cmd/main.go'", ref.Path)
	}
	if !strings.Contains(ref.LineHTML, "res") {
		t.Errorf("expected LineHTML to contain 'res', got %q", ref.LineHTML)
	}
}

func TestReferencesExportedCrossPackage(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Looking up references for func Add in pkg/calc/calc.go: line 4 col 5
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/calc/calc.go",
		Line:      4,
		UTF16Col:  5, // 'A' in Add
	}

	res, err := analyzer.References(ctx, req)
	if err != nil {
		t.Fatalf("References failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected references to func Add to be found")
	}
	if res.SymbolName != "Add" {
		t.Errorf("SymbolName = %q, want 'Add'", res.SymbolName)
	}

	// Should find calc.Add call in cmd/main.go at line 11
	foundCall := false
	for _, ref := range res.Refs {
		if ref.Path == "cmd/main.go" && ref.Line == 11 {
			foundCall = true
			if ref.Group != RefGroupRepo && ref.Group != RefGroupChange {
				t.Errorf("ref.Group = %q, want repo or change", ref.Group)
			}
		}
	}
	if !foundCall {
		t.Fatalf("expected reference at cmd/main.go:11, got: %+v", res.Refs)
	}
}

func TestReferencesTypeSwitch(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goMod := "module example.com/ts\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644)

	src := `package main

import "fmt"

func inspectVal(v any) {
	switch x := v.(type) {
	case int:
		fmt.Println(x + 1)
	case string:
		fmt.Println(x + "!")
	}
}

func main() {
	inspectVal(42)
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Line 6: "	switch x := v.(type) {" -> 'x' is at col 8
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      6,
		UTF16Col:  8,
	}

	res, err := analyzer.References(ctx, req)
	if err != nil {
		t.Fatalf("References failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected type switch variable x references to be found")
	}

	// Should find 2 references: line 8 (case int) and line 10 (case string)
	var lines []int
	for _, r := range res.Refs {
		lines = append(lines, r.Line)
	}
	if len(lines) != 2 || lines[0] != 8 || lines[1] != 10 {
		t.Fatalf("expected references at lines [8, 10], got: %v", lines)
	}
}

func TestReferencesUnexportedCollisionSafety(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goMod := "module example.com/coll\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644)

	srcAlpha := `package main

type Alpha struct {
	id int
}

func useAlpha(a Alpha) int {
	return a.id
}
`
	srcBeta := `package main

type Beta struct {
	id int
}

func useBeta(b Beta) int {
	return b.id
}
`
	_ = os.WriteFile(filepath.Join(dir, "alpha.go"), []byte(srcAlpha), 0644)
	_ = os.WriteFile(filepath.Join(dir, "beta.go"), []byte(srcBeta), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// In alpha.go, line 4 col 2: "	id int" (struct field Alpha.id)
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "alpha.go",
		Line:      4,
		UTF16Col:  2,
	}

	res, err := analyzer.References(ctx, req)
	if err != nil {
		t.Fatalf("References failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected references for Alpha.id to be found")
	}

	// Should only find reference in alpha.go line 8 (return a.id), NOT beta.go!
	if len(res.Refs) != 1 {
		t.Fatalf("expected exactly 1 reference for Alpha.id, got %d: %+v", len(res.Refs), res.Refs)
	}
	if res.Refs[0].Path != "alpha.go" || res.Refs[0].Line != 8 {
		t.Errorf("ref = %+v, want alpha.go:8", res.Refs[0])
	}
}

func TestReferencesMultiByteLineTruncation(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	runGit("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0644)

	padding := strings.Repeat("x", 230)
	src := fmt.Sprintf(`package main

func TargetSym() int {
	return 42
}

func caller() int {
	// %s🚀🚀🚀
	return TargetSym()
}
`, padding)

	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      3,
		UTF16Col:  6, // "TargetSym"
	}

	res, err := analyzer.References(ctx, req)
	if err != nil {
		t.Fatalf("References failed: %v", err)
	}
	if !res.Found || len(res.Refs) == 0 {
		t.Fatalf("expected references to be found: %+v", res)
	}

	for _, ref := range res.Refs {
		if !utf8.ValidString(ref.LineHTML) {
			t.Errorf("ref.LineHTML is not valid UTF-8: %q", ref.LineHTML)
		}
	}
}

func TestReferencesAliasedPackageImport(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	runGit("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0644)

	_ = os.MkdirAll(filepath.Join(dir, "pkg", "calc"), 0755)
	calcSrc := `package calc

func Add(a, b int) int {
	return a + b
}
`
	_ = os.WriteFile(filepath.Join(dir, "pkg", "calc", "calc.go"), []byte(calcSrc), 0644)

	fileA := `package main

import mycalc "example.com/test/pkg/calc"

func doA() int {
	return mycalc.Add(1, 2)
}
`
	fileB := `package main

import "example.com/test/pkg/calc"

func doB() int {
	return calc.Add(3, 4)
}
`
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte(fileA), 0644)
	_ = os.WriteFile(filepath.Join(dir, "b.go"), []byte(fileB), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "a.go",
		Line:      3,
		UTF16Col:  8, // "mycalc"
	}

	res, err := analyzer.References(ctx, req)
	if err != nil {
		t.Fatalf("References failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected references for mycalc package to be found")
	}

	pathsFound := make(map[string]bool)
	for _, ref := range res.Refs {
		pathsFound[ref.Path] = true
	}

	if !pathsFound["a.go"] {
		t.Errorf("expected reference in a.go, got: %+v", res.Refs)
	}
	if !pathsFound["b.go"] {
		t.Errorf("expected cross-alias reference in b.go, got: %+v", res.Refs)
	}
}

func TestReferencesCrossPackageGroupingAndCollision(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/collision\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "pkg", "a"), 0755); err != nil {
		t.Fatal(err)
	}
	serverA := `package a

func    Worker() int { return 42 }

func Other() int {
	return Worker()
}
`
	if err := os.WriteFile(filepath.Join(dir, "pkg", "a", "server.go"), []byte(serverA), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "pkg", "b"), 0755); err != nil {
		t.Fatal(err)
	}
	serverB := `package b
import . "example.com/collision/pkg/a"
var _ = Worker()
`
	if err := os.WriteFile(filepath.Join(dir, "pkg", "b", "server.go"), []byte(serverB), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "cmd"), 0755); err != nil {
		t.Fatal(err)
	}
	mainContent := `package main
import "example.com/collision/pkg/a"
func main() {
	_ = a.Worker()
}
`
	if err := os.WriteFile(filepath.Join(dir, "cmd", "main.go"), []byte(mainContent), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "Initial commit")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Click Worker() in cmd/main.go at line 4, col 9 ('W' in Worker)
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      4,
		UTF16Col:  9,
	}

	res, err := analyzer.References(ctx, req)
	if err != nil {
		t.Fatalf("References failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected references to be found")
	}

	// (a) Verify references in pkg/a/ are categorized under RefGroupPackage ("package")
	foundPkgRef := false
	for _, ref := range res.Refs {
		if ref.Path == "pkg/a/server.go" {
			foundPkgRef = true
			if ref.Group != RefGroupPackage {
				t.Errorf("expected reference in pkg/a/server.go to have group 'package', got %q", ref.Group)
			}
		}
	}
	if !foundPkgRef {
		t.Fatalf("expected reference in pkg/a/server.go, got: %+v", res.Refs)
	}

	// (b) Verify reference in pkg/b/server.go at line 3 col 9 is NOT excluded
	foundCollisionRef := false
	for _, ref := range res.Refs {
		if ref.Path == "pkg/b/server.go" && ref.Line == 3 && ref.Col == 9 {
			foundCollisionRef = true
			if ref.Group != RefGroupRepo {
				t.Errorf("expected reference in pkg/b/server.go to have group 'repo', got %q", ref.Group)
			}
		}
	}
	if !foundCollisionRef {
		t.Fatalf("expected reference in pkg/b/server.go at same line/col (line 3 col 9) to not be excluded, got: %+v", res.Refs)
	}
}

func TestReferencesStructFieldCrossPackageAndStdlib(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/fieldtest\n\ngo 1.22\n"), 0644)
	_ = os.MkdirAll(filepath.Join(dir, "pkga"), 0755)
	_ = os.MkdirAll(filepath.Join(dir, "pkgb"), 0755)

	srcA := `package pkga

type Config struct {
	Host string
	Port int
}

func DefaultConfig() Config {
	c := Config{Host: "localhost", Port: 8080}
	return c
}
`
	_ = os.WriteFile(filepath.Join(dir, "pkga", "config.go"), []byte(srcA), 0644)

	srcB := `package pkgb

import (
	"net/url"
	"example.com/fieldtest/pkga"
)

func Connect(cfg pkga.Config, u *url.URL) string {
	h := cfg.Host
	p := u.Path
	return h + p
}
`
	_ = os.WriteFile(filepath.Join(dir, "pkgb", "client.go"), []byte(srcB), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// (a) References on exported struct field pkga.Config.Host (pkga/config.go line 4 col 2)
	reqField := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkga/config.go",
		Line:      4,
		UTF16Col:  2, // 'H' in Host
	}
	resField, err := analyzer.References(ctx, reqField)
	if err != nil {
		t.Fatalf("References on Config.Host failed: %v", err)
	}
	if !resField.Found {
		t.Fatalf("expected references for Config.Host to be found")
	}
	foundInA := false
	foundInB := false
	for _, ref := range resField.Refs {
		if ref.Path == "pkga/config.go" && ref.Line == 9 {
			foundInA = true
			if ref.Group != RefGroupPackage {
				t.Errorf("expected reference in pkga/config.go to have group 'package', got %q", ref.Group)
			}
		}
		if ref.Path == "pkgb/client.go" && ref.Line == 9 {
			foundInB = true
			if ref.Group != RefGroupRepo {
				t.Errorf("expected reference in pkgb/client.go to have group 'repo', got %q", ref.Group)
			}
		}
	}
	if !foundInA {
		t.Errorf("expected reference in pkga/config.go at line 9, got: %+v", resField.Refs)
	}
	if !foundInB {
		t.Errorf("expected cross-package reference in pkgb/client.go at line 9, got: %+v", resField.Refs)
	}

	// (b) References on stdlib struct field url.URL.Path (pkgb/client.go line 10 col 11: 'P' in u.Path)
	reqStdField := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkgb/client.go",
		Line:      10,
		UTF16Col:  11, // 'P' in u.Path
	}
	resStdField, err := analyzer.References(ctx, reqStdField)
	if err != nil {
		t.Fatalf("References on url.URL.Path failed: %v", err)
	}
	if !resStdField.Found {
		t.Fatalf("expected references for url.URL.Path to be found")
	}
	foundStdUse := false
	for _, ref := range resStdField.Refs {
		if ref.Path == "pkgb/client.go" && ref.Line == 10 {
			foundStdUse = true
		}
	}
	if !foundStdUse {
		t.Errorf("expected reference to url.URL.Path in pkgb/client.go line 10, got: %+v", resStdField.Refs)
	}
}

func TestReferencesGenericStructFieldAndMethod(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/generictest\n\ngo 1.22\n"), 0644)

	src := `package main

type Box[T any] struct {
	Val T
}

func (b Box[T]) Get() T {
	return b.Val
}

func main() {
	b := Box[int]{Val: 42}
	_ = b.Val
	_ = b.Get()
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// (a) References on generic struct field Box[T].Val (main.go line 4 col 2)
	reqField := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      4,
		UTF16Col:  2, // 'V' in Val
	}
	resField, err := analyzer.References(ctx, reqField)
	if err != nil {
		t.Fatalf("References on Box[T].Val failed: %v", err)
	}
	if !resField.Found {
		t.Fatalf("expected references for Box[T].Val to be found")
	}
	foundRetVal := false
	foundLitVal := false
	foundUseVal := false
	for _, ref := range resField.Refs {
		if ref.Path == "main.go" {
			switch ref.Line {
			case 8:
				foundRetVal = true
			case 12:
				foundLitVal = true
			case 13:
				foundUseVal = true
			}
		}
	}
	if !foundRetVal || !foundLitVal || !foundUseVal {
		t.Errorf("expected references at lines 8, 12, 13 for Box[T].Val; got: %+v (ret=%v, lit=%v, use=%v)", resField.Refs, foundRetVal, foundLitVal, foundUseVal)
	}

	// (b) References on generic method Box[T].Get (main.go line 7 col 17: 'G' in Get)
	reqMethod := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      7,
		UTF16Col:  17, // 'G' in Get
	}
	resMethod, err := analyzer.References(ctx, reqMethod)
	if err != nil {
		t.Fatalf("References on Box[T].Get failed: %v", err)
	}
	if !resMethod.Found {
		t.Fatalf("expected references for Box[T].Get to be found")
	}
	foundCallGet := false
	for _, ref := range resMethod.Refs {
		if ref.Path == "main.go" && ref.Line == 14 {
			foundCallGet = true
		}
	}
	if !foundCallGet {
		t.Errorf("expected reference to Box[T].Get at line 14, got: %+v", resMethod.Refs)
	}
}

func TestCanImpls(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goMod := "module example.com/canimpls\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644)

	src := `package main

import "io"

type Greeter interface {
	Greet() string
}

type Server struct {
	port int
}

func (s *Server) Greet() string {
	return "hi"
}

type PlainData struct {
	Count int
}

type Constraint interface {
	~int | ~string
}

func StandaloneFunc() string {
	return "test"
}

func Use(r io.Reader) error {
	var num int = 42
	_ = num
	return nil
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	tests := []struct {
		name     string
		line     int
		col      int
		wantImpl bool
		wantDir  string
	}{
		{
			name:     "Greeter interface",
			line:     5,
			col:      5,
			wantImpl: true,
			wantDir:  "implementations",
		},
		{
			name:     "Server struct with methods",
			line:     9,
			col:      5,
			wantImpl: true,
			wantDir:  "interfaces",
		},
		{
			name:     "Server.Greet method",
			line:     13,
			col:      17,
			wantImpl: true,
			wantDir:  "interfaces",
		},
		{
			name:     "PlainData struct without methods",
			line:     17,
			col:      5,
			wantImpl: false,
			wantDir:  "",
		},
		{
			name:     "Constraint interface",
			line:     21,
			col:      5,
			wantImpl: false,
			wantDir:  "",
		},
		{
			name:     "StandaloneFunc",
			line:     25,
			col:      5,
			wantImpl: false,
			wantDir:  "",
		},
		{
			name:     "io.Reader stdlib interface",
			line:     29,
			col:      14,
			wantImpl: true,
			wantDir:  "implementations",
		},
		{
			name:     "built-in error",
			line:     29,
			col:      23,
			wantImpl: true,
			wantDir:  "implementations",
		},
		{
			name:     "local int var",
			line:     30,
			col:      5,
			wantImpl: false,
			wantDir:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := LookupRequest{
				Repo:      repo,
				RepoRoot:  dir,
				BaseRev:   "HEAD",
				TargetRev: "HEAD",
				Side:      "R",
				FilePath:  "main.go",
				Line:      tc.line,
				UTF16Col:  tc.col,
			}
			res, err := analyzer.Lookup(ctx, req)
			if err != nil {
				t.Fatalf("Lookup failed: %v", err)
			}
			if !res.Found {
				t.Fatalf("symbol at line %d col %d not found", tc.line, tc.col)
			}
			if res.CanImpls != tc.wantImpl {
				t.Errorf("CanImpls = %v, want %v (symbol: %s, sig: %s)", res.CanImpls, tc.wantImpl, res.SymbolName, res.Signature)
			}
			if res.ImplsDirection != tc.wantDir {
				t.Errorf("ImplsDirection = %q, want %q (symbol: %s)", res.ImplsDirection, tc.wantDir, res.SymbolName)
			}
		})
	}
}

func TestImplementationsInterfaceToConcrete(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goMod := "module example.com/impltest\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644)

	_ = os.MkdirAll(filepath.Join(dir, "iface"), 0755)
	ifaceSrc := `package iface

type Storage interface {
	Save(data string) error
}
`
	_ = os.WriteFile(filepath.Join(dir, "iface", "storage.go"), []byte(ifaceSrc), 0644)

	_ = os.MkdirAll(filepath.Join(dir, "impl"), 0755)
	implSrc := `package impl

type MemoryStorage struct {
	buf []string
}

func (m *MemoryStorage) Save(data string) error {
	m.buf = append(m.buf, data)
	return nil
}

type DiskStorage struct {
	dir string
}

func (d *DiskStorage) Save(data string) error {
	return nil
}

// Generic box to test generic types do not panic!
type Box[T any] struct {
	val T
}

func (b *Box[T]) Save(data string) error {
	return nil
}
`
	_ = os.WriteFile(filepath.Join(dir, "impl", "storage.go"), []byte(implSrc), 0644)

	_ = os.MkdirAll(filepath.Join(dir, "embed"), 0755)
	embedSrc := `package embed

import "example.com/impltest/impl"

type Wrapper struct {
	*impl.DiskStorage
}
`
	_ = os.WriteFile(filepath.Join(dir, "embed", "wrapper.go"), []byte(embedSrc), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. Click interface type: Storage at line 3, col 5 in iface/storage.go
	reqType := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "iface/storage.go",
		Line:      3,
		UTF16Col:  5,
	}

	resType, err := analyzer.Implementations(ctx, reqType)
	if err != nil {
		t.Fatalf("Implementations for Storage failed: %v", err)
	}
	if !resType.Found {
		t.Fatalf("expected Storage implementations to be found")
	}
	if resType.Direction != ImplDirImplementations {
		t.Errorf("Direction = %v, want %v", resType.Direction, ImplDirImplementations)
	}

	var foundNames []string
	for _, impl := range resType.Implementations {
		foundNames = append(foundNames, impl.Name)
		if !impl.InRepo {
			t.Errorf("expected InRepo = true for %s", impl.Name)
		}
		if impl.Line <= 0 {
			t.Errorf("expected Line > 0 for %s, got %d", impl.Name, impl.Line)
		}
		if impl.LineHTML == "" {
			t.Errorf("expected LineHTML not empty for %s", impl.Name)
		}
	}

	expectedTypes := []string{"*MemoryStorage", "*DiskStorage", "*Box", "Wrapper"}
	for _, exp := range expectedTypes {
		if !slices.Contains(foundNames, exp) {
			t.Errorf("expected implementations to contain %q, got: %v", exp, foundNames)
		}
	}

	// 2. Click interface method: Save at line 4, col 1 in iface/storage.go
	reqMethod := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "iface/storage.go",
		Line:      4,
		UTF16Col:  1, // "\tSave(data string) error" -> index 1 is 'S'
	}

	resMethod, err := analyzer.Implementations(ctx, reqMethod)
	if err != nil {
		t.Fatalf("Implementations for Save method failed: %v", err)
	}
	if !resMethod.Found {
		t.Fatalf("expected Save method implementations to be found")
	}
	var methodNames []string
	for _, impl := range resMethod.Implementations {
		methodNames = append(methodNames, impl.Name)
	}
	expectedMethods := []string{"(*MemoryStorage).Save", "(*DiskStorage).Save", "(*Box).Save"}
	for _, exp := range expectedMethods {
		if !slices.Contains(methodNames, exp) {
			t.Errorf("expected method implementations to contain %q, got: %v", exp, methodNames)
		}
	}
}

func TestImplementationsConcreteToInterface(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goMod := "module example.com/implconcrete\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644)

	_ = os.MkdirAll(filepath.Join(dir, "iface"), 0755)
	ifaceSrc := `package iface

type CustomReader interface {
	Read(p []byte) (n int, err error)
}
`
	_ = os.WriteFile(filepath.Join(dir, "iface", "reader.go"), []byte(ifaceSrc), 0644)

	_ = os.MkdirAll(filepath.Join(dir, "types"), 0755)
	typesSrc := `package types

type MyReader struct{}

func (m *MyReader) Read(p []byte) (n int, err error) {
	return 0, nil
}

type MyError struct {
	msg string
}

func (e *MyError) Error() string {
	return e.msg
}

type Stringable struct{}

func (s *Stringable) String() string {
	return "hello"
}
`
	_ = os.WriteFile(filepath.Join(dir, "types", "types.go"), []byte(typesSrc), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. MyReader type -> should implement in-repo CustomReader AND stdlib io.Reader
	reqReader := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "types/types.go",
		Line:      3,
		UTF16Col:  5, // "type MyReader struct{}"
	}

	resReader, err := analyzer.Implementations(ctx, reqReader)
	if err != nil {
		t.Fatalf("Implementations for MyReader failed: %v", err)
	}
	if !resReader.Found {
		t.Fatalf("expected MyReader implementations to be found")
	}
	if resReader.Direction != ImplDirImplements {
		t.Errorf("Direction = %v, want %v", resReader.Direction, ImplDirImplements)
	}

	var readerNames []string
	for _, impl := range resReader.Implementations {
		readerNames = append(readerNames, impl.Name)
	}
	if !slices.Contains(readerNames, "CustomReader") {
		t.Errorf("expected implemented interfaces to contain CustomReader, got: %v", readerNames)
	}
	if !slices.Contains(readerNames, "Reader") {
		t.Errorf("expected implemented interfaces to contain Reader, got: %v", readerNames)
	}

	// 2. MyReader.Read method -> should implement CustomReader.Read AND io.Reader.Read
	reqReadMethod := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "types/types.go",
		Line:      5,
		UTF16Col:  19, // "func (m *MyReader) Read(p []byte)..." -> 'R' in Read is at col 19
	}

	resReadMethod, err := analyzer.Implementations(ctx, reqReadMethod)
	if err != nil {
		t.Fatalf("Implementations for MyReader.Read failed: %v", err)
	}
	if !resReadMethod.Found {
		t.Fatalf("expected MyReader.Read implementations to be found")
	}
	var readMethodNames []string
	for _, impl := range resReadMethod.Implementations {
		readMethodNames = append(readMethodNames, impl.Name)
	}
	if !slices.Contains(readMethodNames, "CustomReader.Read") {
		t.Errorf("expected CustomReader.Read, got: %v", readMethodNames)
	}
	if !slices.Contains(readMethodNames, "Reader.Read") {
		t.Errorf("expected Reader.Read, got: %v", readMethodNames)
	}

	// 3. MyError -> should implement built-in error
	reqError := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "types/types.go",
		Line:      9,
		UTF16Col:  5, // "type MyError struct {"
	}

	resError, err := analyzer.Implementations(ctx, reqError)
	if err != nil {
		t.Fatalf("Implementations for MyError failed: %v", err)
	}
	if !resError.Found {
		t.Fatalf("expected MyError implementations to be found")
	}
	var errorNames []string
	var foundBuiltinErr *Implementation
	for _, impl := range resError.Implementations {
		errorNames = append(errorNames, impl.Name)
		if impl.Name == "error" {
			cp := impl
			foundBuiltinErr = &cp
		}
	}
	if !slices.Contains(errorNames, "error") {
		t.Errorf("expected MyError to implement error, got: %v", errorNames)
	}
	if foundBuiltinErr == nil {
		t.Fatalf("expected built-in error implementation, got: %v", errorNames)
	}
	if foundBuiltinErr.Pkg != "builtin" || foundBuiltinErr.PkgFile != "builtin.go" || foundBuiltinErr.Line <= 0 {
		t.Errorf("expected builtin error to have Pkg=builtin, PkgFile=builtin.go, Line>0, got: %+v", *foundBuiltinErr)
	}

	// Verify chained Lookup into builtin.go error interface
	resBuiltinLookup, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:          repo,
		RepoRoot:      dir,
		BaseRev:       "HEAD",
		TargetRev:     "HEAD",
		Side:          "R",
		FilePath:      "types/types.go",
		PkgImportPath: "builtin",
		PkgFile:       "builtin.go",
		Line:          foundBuiltinErr.Line,
		UTF16Col:      foundBuiltinErr.UTF16Col,
	})
	if err != nil {
		t.Fatalf("Lookup chained builtin.go failed: %v", err)
	}
	if !resBuiltinLookup.Found || resBuiltinLookup.SymbolName != "error" {
		t.Errorf("expected builtin.go error lookup found, got: %+v", resBuiltinLookup)
	}

	// 4. Stringable -> should implement fmt.Stringer
	reqStringable := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "types/types.go",
		Line:      17,
		UTF16Col:  5, // "type Stringable struct{}"
	}

	resStringable, err := analyzer.Implementations(ctx, reqStringable)
	if err != nil {
		t.Fatalf("Implementations for Stringable failed: %v", err)
	}
	if !resStringable.Found {
		t.Fatalf("expected Stringable implementations to be found")
	}
	var stringableNames []string
	for _, impl := range resStringable.Implementations {
		stringableNames = append(stringableNames, impl.Name)
	}
	if !slices.Contains(stringableNames, "Stringer") {
		t.Errorf("expected Stringable to implement Stringer, got: %v", stringableNames)
	}
}

func TestImplementationsUnexportedMethod(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	runGit("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/unexported\n\ngo 1.22\n"), 0644)

	src := `package main

type runner interface {
	runTask(id int) bool
}

type TaskWorker struct{}

func (w *TaskWorker) runTask(id int) bool {
	return id > 0
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Direction 1: Unexported interface method runner.runTask (line 4 col 2) -> should find (*TaskWorker).runTask
	reqIfaceMethod := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      4,
		UTF16Col:  2, // "runTask(id int) bool"
	}

	resIfaceMethod, err := analyzer.Implementations(ctx, reqIfaceMethod)
	if err != nil {
		t.Fatalf("Implementations for runner.runTask failed: %v", err)
	}
	if !resIfaceMethod.Found {
		t.Fatalf("expected implementations for runner.runTask to be found")
	}
	var ifaceImplNames []string
	for _, impl := range resIfaceMethod.Implementations {
		ifaceImplNames = append(ifaceImplNames, impl.Name)
	}
	if !slices.Contains(ifaceImplNames, "(*TaskWorker).runTask") {
		t.Errorf("expected (*TaskWorker).runTask in implementations, got: %v", ifaceImplNames)
	}

	// Direction 2: Concrete type unexported method TaskWorker.runTask (line 9 col 22) -> should find runner.runTask
	reqConcreteMethod := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      9,
		UTF16Col:  22, // "func (w *TaskWorker) runTask(id int) bool" -> 'r' in runTask
	}

	resConcreteMethod, err := analyzer.Implementations(ctx, reqConcreteMethod)
	if err != nil {
		t.Fatalf("Implementations for TaskWorker.runTask failed: %v", err)
	}
	if !resConcreteMethod.Found {
		t.Fatalf("expected implementations for TaskWorker.runTask to be found")
	}
	var concreteImplNames []string
	for _, impl := range resConcreteMethod.Implementations {
		concreteImplNames = append(concreteImplNames, impl.Name)
	}
	if !slices.Contains(concreteImplNames, "runner.runTask") {
		t.Errorf("expected runner.runTask in implementations, got: %v", concreteImplNames)
	}
}

func TestImplementationsGenericAndRootPackageGrouping(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	runGit("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/genericimpl\n\ngo 1.22\n"), 0644)

	src := `package main

import "fmt"

type Formatter interface {
	FormatString() string
}

type Box[T comparable] struct {
	val T
}

func (b Box[T]) FormatString() string {
	return fmt.Sprint(b.val)
}

func (b Box[T]) String() string {
	return fmt.Sprint(b.val)
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. CanImpls on generic type Box
	lookupBox, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      9,
		UTF16Col:  6, // "type Box[T..."
	})
	if err != nil {
		t.Fatalf("Lookup Box failed: %v", err)
	}
	if !lookupBox.CanImpls {
		t.Errorf("expected CanImpls=true for generic Box struct with methods, got false")
	}

	// 2. Direction 1: Interface Formatter -> concrete Box (in root package)
	reqFormatter := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      5,
		UTF16Col:  6, // "type Formatter interface"
	}
	resFormatter, err := analyzer.Implementations(ctx, reqFormatter)
	if err != nil {
		t.Fatalf("Implementations for Formatter failed: %v", err)
	}
	if !resFormatter.Found || len(resFormatter.Implementations) == 0 {
		t.Fatalf("expected implementations for Formatter to be found, got %v", resFormatter)
	}
	var boxImpl *Implementation
	for i := range resFormatter.Implementations {
		if strings.Contains(resFormatter.Implementations[i].Name, "Box") {
			boxImpl = &resFormatter.Implementations[i]
			break
		}
	}
	if boxImpl == nil {
		t.Fatalf("expected Box in Formatter implementations, got %v", resFormatter.Implementations)
	}
	if boxImpl.Group != ImplGroupPackage {
		t.Errorf("expected Box to have ImplGroupPackage (%v) in root package, got %v", ImplGroupPackage, boxImpl.Group)
	}

	// 3. Direction 2: Concrete generic type Box -> implements Formatter (root package) and fmt.Stringer
	reqBox := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      9,
		UTF16Col:  6, // "type Box[T..."
	}
	resBox, err := analyzer.Implementations(ctx, reqBox)
	if err != nil {
		t.Fatalf("Implementations for Box failed: %v", err)
	}
	if !resBox.Found {
		t.Fatalf("expected implementations for Box to be found")
	}
	var formatterImpl *Implementation
	var stringerImpl *Implementation
	for i := range resBox.Implementations {
		impl := &resBox.Implementations[i]
		if impl.Name == "Formatter" {
			formatterImpl = impl
		} else if strings.Contains(impl.Name, "Stringer") {
			stringerImpl = impl
		}
	}
	if formatterImpl == nil {
		t.Fatalf("expected Formatter in Box implementations, got %v", resBox.Implementations)
	}
	if formatterImpl.Group != ImplGroupPackage {
		t.Errorf("expected Formatter to have ImplGroupPackage in root package, got %v", formatterImpl.Group)
	}
	if stringerImpl == nil {
		t.Errorf("expected fmt.Stringer in Box implementations, got %v", resBox.Implementations)
	}
}

func TestImplementationsPromotedMethodCrossPackage(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/promotetest\n\ngo 1.22\n"), 0644)
	_ = os.MkdirAll(filepath.Join(dir, "pkga"), 0755)
	_ = os.MkdirAll(filepath.Join(dir, "pkgb"), 0755)

	srcA := `package pkga

type Base struct{}

func (b *Base) Run() error {
	return nil
}
`
	_ = os.WriteFile(filepath.Join(dir, "pkga", "base.go"), []byte(srcA), 0644)

	srcB := `package pkgb

import "example.com/promotetest/pkga"

type Runner interface {
	Run() error
}

type Sub struct {
	pkga.Base
}
`
	_ = os.WriteFile(filepath.Join(dir, "pkgb", "sub.go"), []byte(srcB), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Direction 1: Interface method Runner.Run in pkgb/sub.go at line 6, col 2 ('R' in Run)
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkgb/sub.go",
		Line:      6,
		UTF16Col:  2, // 'R' in Run
	}

	res, err := analyzer.Implementations(ctx, req)
	if err != nil {
		t.Fatalf("Implementations on Runner.Run failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected implementations for Runner.Run to be found")
	}

	var subRunImpl *Implementation
	for i := range res.Implementations {
		if strings.Contains(res.Implementations[i].Name, "Sub") {
			subRunImpl = &res.Implementations[i]
			break
		}
	}
	if subRunImpl == nil {
		t.Fatalf("expected (*Sub).Run in implementations, got: %+v", res.Implementations)
	}

	// Verify path points to pkga/base.go where Run() is defined, and line is 5
	if subRunImpl.Path != "pkga/base.go" {
		t.Errorf("expected promoted method implementation path to be pkga/base.go, got: %q", subRunImpl.Path)
	}
	if subRunImpl.Line != 5 {
		t.Errorf("expected promoted method line number to be 5, got: %d", subRunImpl.Line)
	}
}

func TestImplementationsExternalInterfaceAndRanking(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/roundtripper\n\ngo 1.22\n"), 0644)

	src := `package main

import "net/http"

type LocalTransport struct{}

func (t *LocalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, nil
}

type Client struct {
	Transport http.RoundTripper
}

type CustomTripper interface {
	RoundTrip(req *http.Request) (*http.Response, error)
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Direction 1 on http.RoundTripper: line 12, col 17 ('R' in RoundTripper)
	res, err := analyzer.Implementations(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      12,
		UTF16Col:  17,
	})
	if err != nil {
		t.Fatalf("Implementations on http.RoundTripper failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected implementations for http.RoundTripper to be found")
	}

	localIdx := -1
	transportIdx := -1
	for i, impl := range res.Implementations {
		if impl.Name == "*LocalTransport" && impl.InRepo && impl.Group == ImplGroupPackage {
			localIdx = i
		}
		if impl.Name == "*Transport" && !impl.InRepo && impl.Group == ImplGroupExternal {
			transportIdx = i
		}
	}

	if localIdx == -1 {
		t.Fatalf("expected *LocalTransport in implementations, got: %+v", res.Implementations)
	}
	if transportIdx == -1 {
		t.Fatalf("expected *Transport in implementations, got: %+v", res.Implementations)
	}
	if localIdx >= transportIdx {
		t.Errorf("expected in-repo implementation (*LocalTransport at %d) to be ranked before external (*Transport at %d)", localIdx, transportIdx)
	}

	// Direction 1 on method CustomTripper.RoundTrip: line 16, col 2 ('R' in RoundTrip)
	resMethod, err := analyzer.Implementations(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      16,
		UTF16Col:  2,
	})
	if err != nil {
		t.Fatalf("Implementations on CustomTripper.RoundTrip failed: %v", err)
	}
	if !resMethod.Found {
		t.Fatalf("expected implementations for CustomTripper.RoundTrip to be found")
	}

	localMethodIdx := -1
	transportMethodIdx := -1
	for i, impl := range resMethod.Implementations {
		if impl.Name == "(*LocalTransport).RoundTrip" && impl.InRepo {
			localMethodIdx = i
		}
		if impl.Name == "(*Transport).RoundTrip" && !impl.InRepo && impl.Group == ImplGroupExternal {
			transportMethodIdx = i
		}
	}

	if localMethodIdx == -1 {
		t.Fatalf("expected (*LocalTransport).RoundTrip in implementations, got: %+v", resMethod.Implementations)
	}
	if transportMethodIdx == -1 {
		t.Fatalf("expected (*Transport).RoundTrip in implementations, got: %+v", resMethod.Implementations)
	}
	if localMethodIdx >= transportMethodIdx {
		t.Errorf("expected in-repo method (*LocalTransport).RoundTrip at %d before external at %d", localMethodIdx, transportMethodIdx)
	}
}

func TestImplementationsCrossPackageDuckTyping(t *testing.T) {
	parentDir := t.TempDir()
	extDir := filepath.Join(parentDir, "extiface")
	repoDir := filepath.Join(parentDir, "repo")

	if err := os.MkdirAll(extDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}

	extMod := "module example.com/extiface\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(extDir, "go.mod"), []byte(extMod), 0644)
	extSrc := `package extiface

type Doer interface {
	DoThing() string
}
`
	_ = os.WriteFile(filepath.Join(extDir, "doer.go"), []byte(extSrc), 0644)

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	repoMod := `module example.com/ducktest

go 1.22

require example.com/extiface v0.0.0
replace example.com/extiface => ../extiface
`
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(repoMod), 0644)

	consumerSrc := `package main

import "example.com/extiface"

var _ = extiface.Doer(nil)
`
	_ = os.WriteFile(filepath.Join(repoDir, "consumer.go"), []byte(consumerSrc), 0644)

	serviceSrc := `package main

type MyService struct{}

func (s *MyService) DoThing() string {
	return "done"
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "service.go"), []byte(serviceSrc), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(repoDir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Direction 2 on MyService: line 3, col 6 ('M' in MyService)
	res, err := analyzer.Implementations(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "service.go",
		Line:      3,
		UTF16Col:  6,
	})
	if err != nil {
		t.Fatalf("Implementations on MyService failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected implementations for MyService to be found")
	}
	if res.Direction != ImplDirImplements {
		t.Fatalf("expected direction %s, got %s", ImplDirImplements, res.Direction)
	}

	var doerImpl *Implementation
	for i := range res.Implementations {
		if res.Implementations[i].Name == "Doer" {
			doerImpl = &res.Implementations[i]
			break
		}
	}
	if doerImpl == nil {
		t.Fatalf("expected Doer in implementations, got: %+v", res.Implementations)
	}
	if doerImpl.InRepo {
		t.Errorf("expected extiface.Doer to have InRepo = false")
	}
	if doerImpl.Group != ImplGroupExternal {
		t.Errorf("expected extiface.Doer to have Group = ImplGroupExternal, got %s", doerImpl.Group)
	}
	if doerImpl.Pkg != "example.com/extiface" {
		t.Errorf("expected extiface.Doer to have Pkg = example.com/extiface, got %s", doerImpl.Pkg)
	}

	// Direction 2 on method MyService.DoThing: line 5, col 21 ('D' in DoThing)
	resMethod, err := analyzer.Implementations(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "service.go",
		Line:      5,
		UTF16Col:  21,
	})
	if err != nil {
		t.Fatalf("Implementations on MyService.DoThing failed: %v", err)
	}
	if !resMethod.Found {
		t.Fatalf("expected implementations for MyService.DoThing to be found")
	}

	var doerMethodImpl *Implementation
	for i := range resMethod.Implementations {
		if resMethod.Implementations[i].Name == "Doer.DoThing" {
			doerMethodImpl = &resMethod.Implementations[i]
			break
		}
	}
	if doerMethodImpl == nil {
		t.Fatalf("expected Doer.DoThing in implementations, got: %+v", resMethod.Implementations)
	}
	if doerMethodImpl.InRepo {
		t.Errorf("expected extiface.Doer.DoThing to have InRepo = false")
	}
	if doerMethodImpl.Group != ImplGroupExternal {
		t.Errorf("expected extiface.Doer.DoThing to have Group = ImplGroupExternal, got %s", doerMethodImpl.Group)
	}
}

func TestImplementationsBuildTagIgnore(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/buildtagtest\n\ngo 1.22\n"), 0644)

	mainSrc := `package main

type Worker interface {
	Work() string
}

type RealWorker struct{}

func (r *RealWorker) Work() string {
	return "real"
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSrc), 0644)

	ignoredSrc := `//go:build ignore

package main

type BogusWorker struct{}

func (b *BogusWorker) Work() string {
	return "bogus"
}
`
	_ = os.WriteFile(filepath.Join(dir, "ignored.go"), []byte(ignoredSrc), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Direction 1 on Worker interface: line 3, col 6 ('W' in Worker)
	res, err := analyzer.Implementations(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      3,
		UTF16Col:  6,
	})
	if err != nil {
		t.Fatalf("Implementations on Worker failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected implementations for Worker to be found")
	}

	hasReal := false
	for _, impl := range res.Implementations {
		if impl.Name == "*RealWorker" {
			hasReal = true
		}
		if strings.Contains(impl.Name, "BogusWorker") {
			t.Errorf("found BogusWorker from //go:build ignore file in implementations: %+v", impl)
		}
	}
	if !hasReal {
		t.Errorf("expected *RealWorker in implementations, got: %+v", res.Implementations)
	}
}

func TestLookupImportPath(t *testing.T) {
	dir := t.TempDir()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goModContent := `module example.com/testmod

go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	subpkgDir := filepath.Join(dir, "pkg", "subpkg")
	if err := os.MkdirAll(subpkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	docGo := `// Package subpkg provides test utilities.
package subpkg
`
	if err := os.WriteFile(filepath.Join(subpkgDir, "doc.go"), []byte(docGo), 0644); err != nil {
		t.Fatal(err)
	}
	subGo := `package subpkg

func Hello() string {
	return "hello"
}
`
	if err := os.WriteFile(filepath.Join(subpkgDir, "sub.go"), []byte(subGo), 0644); err != nil {
		t.Fatal(err)
	}

	mainGo := `package main

import (
	"C"
	"net/http"
	"go/types"
	_ "embed"
	` + "`" + `fmt` + "`" + `
	"example.com/testmod/pkg/subpkg"
)

func main() {
	s := "net/http"
	_ = s
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. Stdlib import: "net/http" (line 5)
	resHTTP, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      5,
		UTF16Col:  2,
	})
	if err != nil {
		t.Fatalf("Lookup net/http failed: %v", err)
	}
	if !resHTTP.Found {
		t.Fatalf("expected Found=true for net/http")
	}
	if resHTTP.SymbolName != "http" {
		t.Errorf("SymbolName = %q, want http", resHTTP.SymbolName)
	}
	if resHTTP.InRepo {
		t.Errorf("InRepo = true, want false for stdlib")
	}
	if !resHTTP.CanRefs {
		t.Errorf("CanRefs = false, want true for net/http")
	}
	if resHTTP.CanImpls {
		t.Errorf("CanImpls = true, want false for net/http package")
	}
	if len(resHTTP.Snippet) == 0 {
		t.Errorf("expected non-empty snippet for net/http")
	}

	// 2. Stdlib import: "go/types" (line 6)
	resTypes, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      6,
		UTF16Col:  2,
	})
	if err != nil {
		t.Fatalf("Lookup go/types failed: %v", err)
	}
	if !resTypes.Found || resTypes.SymbolName != "types" {
		t.Fatalf("expected Found=true, SymbolName=types for go/types, got %+v", resTypes)
	}

	// 3. In-repo import: "example.com/testmod/pkg/subpkg" (line 9)
	resSubpkg, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      9,
		UTF16Col:  2,
	})
	if err != nil {
		t.Fatalf("Lookup subpkg failed: %v", err)
	}
	if !resSubpkg.Found {
		t.Fatalf("expected Found=true for subpkg")
	}
	if !resSubpkg.InRepo {
		t.Errorf("InRepo = false, want true for subpkg")
	}
	if resSubpkg.DefPath != "pkg/subpkg/doc.go" {
		t.Errorf("DefPath = %q, want pkg/subpkg/doc.go (doc.go preference)", resSubpkg.DefPath)
	}
	if resSubpkg.DefLine != 2 {
		t.Errorf("DefLine = %d, want 2 (package clause line)", resSubpkg.DefLine)
	}
	if !resSubpkg.CanRefs {
		t.Errorf("CanRefs = false, want true for subpkg")
	}
	if resSubpkg.CanImpls {
		t.Errorf("CanImpls = true, want false for subpkg package")
	}

	// 4. Blank import: _ "embed" (line 7)
	resEmbed, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      7,
		UTF16Col:  5,
	})
	if err != nil {
		t.Fatalf("Lookup embed failed: %v", err)
	}
	if !resEmbed.Found || resEmbed.SymbolName != "embed" {
		t.Fatalf("expected Found=true, SymbolName=embed for _ \"embed\", got %+v", resEmbed)
	}

	// 5. Raw backtick import: `fmt` (line 8)
	resFmt, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      8,
		UTF16Col:  2,
	})
	if err != nil {
		t.Fatalf("Lookup fmt failed: %v", err)
	}
	if !resFmt.Found || resFmt.SymbolName != "fmt" {
		t.Fatalf("expected Found=true, SymbolName=fmt for `fmt`, got %+v", resFmt)
	}

	// 6. Cgo import: "C" (line 4)
	resC, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      4,
		UTF16Col:  2,
	})
	if err != nil {
		t.Fatalf("Lookup C failed: %v", err)
	}
	if resC.Found {
		t.Errorf("expected Found=false for \"C\", got Found=true")
	}

	// 7. Normal string literal inside function body: "net/http" (line 13)
	resStr, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      13,
		UTF16Col:  8,
	})
	if err != nil {
		t.Fatalf("Lookup string failed: %v", err)
	}
	if resStr.Found {
		t.Errorf("expected Found=false for string literal outside import, got Found=true")
	}
}

func TestReferencesImportPath(t *testing.T) {
	dir := t.TempDir()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goModContent := `module example.com/testmod

go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	clientGo := `package main

import "net/http"

func Fetch() {
	_, _ = http.Get("https://example.com")
}
`
	if err := os.WriteFile(filepath.Join(dir, "client.go"), []byte(clientGo), 0644); err != nil {
		t.Fatal(err)
	}

	aliasedGo := `package main

import h ` + "`" + `net/http` + "`" + `

func FetchAlias() {
	_, _ = h.Get("https://example.com/alias")
}
`
	if err := os.WriteFile(filepath.Join(dir, "aliased.go"), []byte(aliasedGo), 0644); err != nil {
		t.Fatal(err)
	}

	rootLibDir := filepath.Join(dir, "cmd", "app")
	if err := os.MkdirAll(rootLibDir, 0755); err != nil {
		t.Fatal(err)
	}
	rootPkgGo := `package rootlib

func Version() string { return "v1" }
`
	if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte(rootPkgGo), 0644); err != nil {
		t.Fatal(err)
	}
	// Put client.go/aliased.go in sub-directory so root.go is the root module package
	if err := os.Rename(filepath.Join(dir, "client.go"), filepath.Join(rootLibDir, "client.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "aliased.go"), filepath.Join(rootLibDir, "aliased.go")); err != nil {
		t.Fatal(err)
	}
	useRootGo := `package main

import "example.com/testmod"

var _ = rootlib.Version()
`
	if err := os.WriteFile(filepath.Join(rootLibDir, "useroot.go"), []byte(useRootGo), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Root-module package import lookup (dir == "")
	resRoot, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "cmd/app/useroot.go",
		Line:      3,
		UTF16Col:  8,
	})
	if err != nil {
		t.Fatalf("Lookup root module import failed: %v", err)
	}
	if !resRoot.Found || !resRoot.InRepo || resRoot.DefPath != "root.go" {
		t.Fatalf("expected root module import to resolve to root.go, got %+v", resRoot)
	}

	// References on "net/http" in cmd/app/client.go line 3, col 8
	res, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "cmd/app/client.go",
		Line:      3,
		UTF16Col:  8,
	})
	if err != nil {
		t.Fatalf("References failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected References to find results for net/http")
	}
	if res.SymbolName != "http" {
		t.Errorf("SymbolName = %q, want http", res.SymbolName)
	}

	foundClientCall := false
	foundAliasedImport := false
	foundAliasedCall := false

	for _, ref := range res.Refs {
		if ref.Path == "cmd/app/client.go" && ref.Line == 6 {
			foundClientCall = true
		}
		if ref.Path == "cmd/app/aliased.go" && ref.Line == 3 {
			foundAliasedImport = true
		}
		if ref.Path == "cmd/app/aliased.go" && ref.Line == 6 {
			foundAliasedCall = true
		}
	}

	if !foundClientCall {
		t.Errorf("expected reference at client.go:6 (http.Get call), got: %+v", res.Refs)
	}
	if !foundAliasedImport {
		t.Errorf("expected reference at aliased.go:3 (aliased import spec), got: %+v", res.Refs)
	}
	if !foundAliasedCall {
		t.Errorf("expected reference at aliased.go:6 (h.Get call), got: %+v", res.Refs)
	}
}

func TestLookupPackageClause(t *testing.T) {
	dir := t.TempDir()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goModContent := `module example.com/testmod

go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	subpkgDir := filepath.Join(dir, "pkg", "subpkg")
	if err := os.MkdirAll(subpkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	docGo := `// Package subpkg provides test utilities.
package subpkg
`
	if err := os.WriteFile(filepath.Join(subpkgDir, "doc.go"), []byte(docGo), 0644); err != nil {
		t.Fatal(err)
	}
	subGo := `package subpkg

func Hello() string {
	return "hello"
}
`
	if err := os.WriteFile(filepath.Join(subpkgDir, "sub.go"), []byte(subGo), 0644); err != nil {
		t.Fatal(err)
	}

	subWinGo := `//go:build windows

package subpkg

func WindowsOnly() {}
`
	if err := os.WriteFile(filepath.Join(subpkgDir, "sub_windows.go"), []byte(subWinGo), 0644); err != nil {
		t.Fatal(err)
	}

	subTestGo := `package subpkg_test

import "testing"

func TestSub(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(subpkgDir, "sub_test.go"), []byte(subTestGo), 0644); err != nil {
		t.Fatal(err)
	}

	mainGo := `package main

import "example.com/testmod/pkg/subpkg"

func main() {
	_ = subpkg.Hello()
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. In-repo library package clause in sub.go: package subpkg (line 1, col 9)
	resSub, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkg/subpkg/sub.go",
		Line:      1,
		UTF16Col:  9,
	})
	if err != nil {
		t.Fatalf("Lookup subpkg failed: %v", err)
	}
	if !resSub.Found {
		t.Fatalf("expected Found=true for package subpkg clause")
	}
	if resSub.SymbolName != "subpkg" {
		t.Errorf("SymbolName = %q, want subpkg", resSub.SymbolName)
	}
	if !resSub.InRepo {
		t.Errorf("InRepo = false, want true for in-repo package clause")
	}
	if resSub.DefPath != "pkg/subpkg/doc.go" {
		t.Errorf("DefPath = %q, want pkg/subpkg/doc.go (doc.go preference)", resSub.DefPath)
	}
	if resSub.DefLine != 2 {
		t.Errorf("DefLine = %d, want 2 (package clause in doc.go)", resSub.DefLine)
	}
	if !resSub.CanRefs {
		t.Errorf("CanRefs = false, want true for package clause")
	}
	if resSub.CanImpls {
		t.Errorf("CanImpls = true, want false for package clause")
	}

	// 2. In-repo external test package clause in sub_test.go: package subpkg_test (line 1, col 9)
	resTest, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkg/subpkg/sub_test.go",
		Line:      1,
		UTF16Col:  9,
	})
	if err != nil {
		t.Fatalf("Lookup subpkg_test failed: %v", err)
	}
	if !resTest.Found {
		t.Fatalf("expected Found=true for package subpkg_test clause")
	}
	if resTest.SymbolName != "subpkg_test" {
		t.Errorf("SymbolName = %q, want subpkg_test", resTest.SymbolName)
	}
	if resTest.DefPath != "pkg/subpkg/sub_test.go" {
		t.Errorf("DefPath = %q, want pkg/subpkg/sub_test.go", resTest.DefPath)
	}
	if resTest.Signature != "package subpkg_test" {
		t.Errorf("Signature = %q, want %q", resTest.Signature, "package subpkg_test")
	}
	if resTest.PkgGoDevURL != "" {
		t.Errorf("PkgGoDevURL = %q, want empty", resTest.PkgGoDevURL)
	}

	// 3. Lookup on main.go package main (line 1, col 9)
	resMain, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      1,
		UTF16Col:  9,
	})
	if err != nil {
		t.Fatalf("Lookup main failed: %v", err)
	}
	if !resMain.Found {
		t.Fatalf("expected Found=true for package main clause")
	}
	if resMain.Signature != "package main" {
		t.Errorf("Signature = %q, want %q", resMain.Signature, "package main")
	}
	if resMain.PkgGoDevURL != "" {
		t.Errorf("PkgGoDevURL = %q, want empty for package main", resMain.PkgGoDevURL)
	}

	// 4. Chained stdlib lookup on package http in net/http/header.go
	resHTTP, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:          repo,
		RepoRoot:      dir,
		BaseRev:       headHash,
		TargetRev:     headHash,
		Side:          "R",
		PkgImportPath: "net/http",
		PkgFile:       "header.go",
		Line:          5,
		UTF16Col:      9,
	})
	if err != nil {
		t.Fatalf("Lookup chained net/http failed: %v", err)
	}
	if !resHTTP.Found {
		t.Fatalf("expected Found=true for chained package http clause")
	}
	if resHTTP.SymbolName != "http" {
		t.Errorf("SymbolName = %q, want http", resHTTP.SymbolName)
	}
	if resHTTP.InRepo {
		t.Errorf("InRepo = true, want false for stdlib")
	}
}

func TestReferencesPackageClause(t *testing.T) {
	dir := t.TempDir()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goModContent := `module example.com/testmod

go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	subpkgDir := filepath.Join(dir, "pkg", "subpkg")
	if err := os.MkdirAll(subpkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	docGo := `// Package subpkg provides test utilities.
package subpkg
`
	if err := os.WriteFile(filepath.Join(subpkgDir, "doc.go"), []byte(docGo), 0644); err != nil {
		t.Fatal(err)
	}
	subGo := `package subpkg

func Hello() string {
	return "hello"
}
`
	if err := os.WriteFile(filepath.Join(subpkgDir, "sub.go"), []byte(subGo), 0644); err != nil {
		t.Fatal(err)
	}

	subWinGo := `//go:build windows

package subpkg

func WindowsOnly() {}
`
	if err := os.WriteFile(filepath.Join(subpkgDir, "sub_windows.go"), []byte(subWinGo), 0644); err != nil {
		t.Fatal(err)
	}

	// Multi-file main package: main.go and helper.go
	mainGo := `package main

import "example.com/testmod/pkg/subpkg"

func main() {
	_ = subpkg.Hello()
	Help()
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}

	helperGo := `package main

func Help() {}
`
	if err := os.WriteFile(filepath.Join(dir, "helper.go"), []byte(helperGo), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. References on package main in main.go (line 1, col 9)
	// Should find helper.go line 1 (package main) and NOT search cross-repo
	resMain, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "main.go",
		Line:      1,
		UTF16Col:  9,
	})
	if err != nil {
		t.Fatalf("References package main failed: %v", err)
	}
	if !resMain.Found {
		t.Fatalf("expected Found=true for package main refs")
	}
	foundHelper := false
	for _, ref := range resMain.Refs {
		if ref.Path == "helper.go" && ref.Line == 1 {
			foundHelper = true
		}
	}
	if !foundHelper {
		t.Errorf("expected sibling reference in helper.go:1, got: %+v", resMain.Refs)
	}

	// 2. References on package subpkg in sub.go (line 1, col 9)
	// Should find sibling doc.go:2 (package subpkg) AND cross-repo in main.go:3 (import) and main.go:6 (subpkg.Hello)
	resSub, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkg/subpkg/sub.go",
		Line:      1,
		UTF16Col:  9,
	})
	if err != nil {
		t.Fatalf("References package subpkg failed: %v", err)
	}
	if !resSub.Found {
		t.Fatalf("expected Found=true for package subpkg refs")
	}
	foundDoc := false
	foundWin := false
	foundMainImport := false
	foundMainCall := false
	for _, ref := range resSub.Refs {
		if ref.Path == "pkg/subpkg/doc.go" && ref.Line == 2 {
			foundDoc = true
		}
		if ref.Path == "pkg/subpkg/sub_windows.go" && ref.Line == 3 {
			foundWin = true
		}
		if ref.Path == "main.go" && ref.Line == 3 {
			foundMainImport = true
		}
		if ref.Path == "main.go" && ref.Line == 6 {
			foundMainCall = true
		}
	}
	if !foundDoc {
		t.Errorf("expected sibling ref in doc.go:2, got: %+v", resSub.Refs)
	}
	if !foundWin {
		t.Errorf("expected sibling ref in sub_windows.go:3, got: %+v", resSub.Refs)
	}
	if !foundMainImport {
		t.Errorf("expected import ref in main.go:3, got: %+v", resSub.Refs)
	}
	if !foundMainCall {
		t.Errorf("expected call ref in main.go:6, got: %+v", resSub.Refs)
	}
}

func TestRepoFileIndexBloom(t *testing.T) {
	src := []byte(`package testpkg

// Comment with identifier IgnoredCommentIdent
/* Multi-line comment
   AnotherCommentIdent */
import (
	"fmt"
	"net/http"
	custom "example.com/custom/pkg"
)

type MyType struct {
	Field int
}

func Helloπ(über string) string {
	_ = custom.Value
	_ = "testing StringBodyIdent"
	return fmt.Sprintf("hello %s %v", über, 3.14)
}
`)

	var entry repoFileEntry
	importSet := make(map[string]bool)
	scanFileBloomAndImports(src, &entry, true, importSet)

	if !entry.hasTypeDecl {
		t.Errorf("expected hasTypeDecl to be true")
	}
	if entry.hasInterfaceKw {
		t.Errorf("expected hasInterfaceKw to be false")
	}
	if !entry.importsExact {
		t.Errorf("expected importsExact to be true")
	}

	// Test real identifiers present in code
	expectedIdents := []string{
		"testpkg", "package", "import", "fmt", "custom", "type", "MyType",
		"struct", "Field", "int", "func", "Helloπ", "über", "string", "Value", "Sprintf",
	}
	for _, id := range expectedIdents {
		if !testIdentBloom(&entry.bits, id) {
			t.Errorf("testIdentBloom failed for present ident %q", id)
		}
	}

	// Comments and strings should not be indexed as identifiers
	commentOrStringIdents := []string{
		"IgnoredCommentIdent",
		"AnotherCommentIdent",
		"StringBodyIdent",
	}
	for _, id := range commentOrStringIdents {
		if testIdentBloom(&entry.bits, id) {
			t.Errorf("testIdentBloom unexpectedly found comment/string token %q", id)
		}
	}

	// Test imports
	expectedImports := []string{
		"fmt",
		"net/http",
		"example.com/custom/pkg",
	}
	for _, imp := range expectedImports {
		if !testImportBloom(&entry.bits, imp) {
			t.Errorf("testImportBloom failed for present import %q", imp)
		}
		if !importSet[imp] {
			t.Errorf("importSet missing present import %q", imp)
		}
	}
	if importSet["testing"] {
		t.Errorf("importSet polluted by non-import string literal %q", "testing")
	}
}

func TestRepoFileIndexLexicalScanner(t *testing.T) {
	src := []byte(`package sample

import "os"
import (
	"strings"
	alias "io"
)

// Single line with HiddenCommentIdent
/* Multi line with
   HiddenMultiIdent */
var raw = ` + "`" + `raw string with HiddenRawIdent \n \t` + "`" + `
var charA = 'a'
var charEsc = '\''
var charBackslash = '\\'

type Greeter interface {
	Greet(name string) string
}

type GreeterImpl struct{}

func (g GreeterImpl) Greet(name string) string {
	return "hello " + name
}
`)

	var entry repoFileEntry
	importSet := make(map[string]bool)
	scanFileBloomAndImports(src, &entry, true, importSet)

	if !entry.hasTypeDecl {
		t.Errorf("expected hasTypeDecl to be true")
	}
	if !entry.hasInterfaceKw {
		t.Errorf("expected hasInterfaceKw to be true")
	}
	if !entry.importsExact {
		t.Errorf("expected importsExact to be true")
	}

	for _, imp := range []string{"os", "strings", "io"} {
		if !testImportBloom(&entry.bits, imp) {
			t.Errorf("expected import %q in bloom", imp)
		}
		if !importSet[imp] {
			t.Errorf("expected import %q in importSet", imp)
		}
	}

	for _, id := range []string{"sample", "Greeter", "interface", "Greet", "GreeterImpl", "struct"} {
		if !testIdentBloom(&entry.bits, id) {
			t.Errorf("expected identifier %q in bloom", id)
		}
	}

	for _, hidden := range []string{"HiddenCommentIdent", "HiddenMultiIdent", "HiddenRawIdent"} {
		if testIdentBloom(&entry.bits, hidden) {
			t.Errorf("unexpected token %q in bloom", hidden)
		}
	}
}

func TestParallelRefsAndImplsDeterminism(t *testing.T) {
	dir := t.TempDir()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goModContent := `module example.com/partest

go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{"pkg1", "pkg2", "pkg3", "pkg4", "pkg5", "pkgdot", "pkgsplit"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// pkg1: interface Greeter and struct Container
	pkg1Content := `package pkg1

type Greeter interface {
	Greet() string
}

type Container struct {
	Payload string
}
`
	if err := os.WriteFile(filepath.Join(dir, "pkg1", "iface.go"), []byte(pkg1Content), 0644); err != nil {
		t.Fatal(err)
	}

	// pkg2: struct implementing Greeter and returning Container
	pkg2Content := `package pkg2

import "example.com/partest/pkg1"

type FriendlyGreeter struct{}

func (FriendlyGreeter) Greet() string {
	return "friendly hello"
}

func GetContainer() pkg1.Container {
	return pkg1.Container{Payload: "secret"}
}
`
	if err := os.WriteFile(filepath.Join(dir, "pkg2", "greeter.go"), []byte(pkg2Content), 0644); err != nil {
		t.Fatal(err)
	}

	// pkg3: another struct implementing Greeter
	pkg3Content := `package pkg3

type FormalGreeter struct{}

func (FormalGreeter) Greet() string {
	return "good day"
}
`
	if err := os.WriteFile(filepath.Join(dir, "pkg3", "formal.go"), []byte(pkg3Content), 0644); err != nil {
		t.Fatal(err)
	}

	// pkg4: calls Greeter.Greet
	pkg4Content := `package pkg4

import "example.com/partest/pkg1"

func Run(g pkg1.Greeter) string {
	return g.Greet()
}
`
	if err := os.WriteFile(filepath.Join(dir, "pkg4", "run.go"), []byte(pkg4Content), 0644); err != nil {
		t.Fatal(err)
	}

	// pkg5: accesses Container.Payload WITHOUT directly importing pkg1!
	pkg5Content := `package pkg5

import "example.com/partest/pkg2"

func Extract() string {
	c := pkg2.GetContainer()
	return c.Payload
}
`
	if err := os.WriteFile(filepath.Join(dir, "pkg5", "extract.go"), []byte(pkg5Content), 0644); err != nil {
		t.Fatal(err)
	}

	// pkgdot: dot.go uses dot-import of pkg1 to reference Container unqualified
	pkgdotContent := `package pkgdot

import . "example.com/partest/pkg1"

func MakeDotContainer() Container {
	return Container{Payload: "dot"}
}
`
	if err := os.WriteFile(filepath.Join(dir, "pkgdot", "dot.go"), []byte(pkgdotContent), 0644); err != nil {
		t.Fatal(err)
	}

	// pkgsplit: type declaration in types.go and method implementation in methods.go
	pkgsplitTypes := `package pkgsplit

type SplitGreeter struct{}
`
	if err := os.WriteFile(filepath.Join(dir, "pkgsplit", "types.go"), []byte(pkgsplitTypes), 0644); err != nil {
		t.Fatal(err)
	}
	pkgsplitMethods := `package pkgsplit

func (SplitGreeter) Greet() string {
	return "split hello"
}
`
	if err := os.WriteFile(filepath.Join(dir, "pkgsplit", "methods.go"), []byte(pkgsplitMethods), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headHash := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. Concurrent References determinism test: 8 parallel goroutines querying pkg1.Greeter
	type refsOutcome struct {
		found int
		refs  []string
	}
	refsResults := make([]refsOutcome, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := analyzer.References(ctx, LookupRequest{
				Repo:      repo,
				RepoRoot:  dir,
				BaseRev:   headHash,
				TargetRev: headHash,
				Side:      "R",
				FilePath:  "pkg1/iface.go",
				Line:      3,
				UTF16Col:  6, // Greeter
			})
			if err != nil {
				t.Errorf("worker %d References failed: %v", i, err)
				return
			}
			out := refsOutcome{found: len(res.Refs)}
			for _, r := range res.Refs {
				out.refs = append(out.refs, fmt.Sprintf("%s:%d:%d", r.Path, r.Line, r.Col))
			}
			refsResults[i] = out
		}()
	}
	wg.Wait()

	for i := 1; i < len(refsResults); i++ {
		if !slices.Equal(refsResults[0].refs, refsResults[i].refs) {
			t.Errorf("References determinism failed between worker 0 and %d:\nw0: %+v\nw%d: %+v", i, refsResults[0].refs, i, refsResults[i].refs)
		}
	}

	// 2. Concurrent Implementations determinism test: 8 parallel goroutines querying pkg1.Greeter
	type implsOutcome struct {
		found int
		impls []string
	}
	implsResults := make([]implsOutcome, 8)
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := analyzer.Implementations(ctx, LookupRequest{
				Repo:      repo,
				RepoRoot:  dir,
				BaseRev:   headHash,
				TargetRev: headHash,
				Side:      "R",
				FilePath:  "pkg1/iface.go",
				Line:      3,
				UTF16Col:  6, // Greeter
			})
			if err != nil {
				t.Errorf("worker %d Implementations failed: %v", i, err)
				return
			}
			out := implsOutcome{found: len(res.Implementations)}
			for _, imp := range res.Implementations {
				out.impls = append(out.impls, fmt.Sprintf("%s:%s:%d", imp.Name, imp.Display, imp.Line))
			}
			implsResults[i] = out
		}()
	}
	wg.Wait()

	for i := 1; i < len(implsResults); i++ {
		if !slices.Equal(implsResults[0].impls, implsResults[i].impls) {
			t.Errorf("Implementations determinism failed between worker 0 and %d:\nw0: %+v\nw%d: %+v", i, implsResults[0].impls, i, implsResults[i].impls)
		}
	}

	// 3. Verify cross-package field ref without direct import: pkg5 references Container.Payload
	resPayload, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkg1/iface.go",
		Line:      8,
		UTF16Col:  2, // Payload
	})
	if err != nil {
		t.Fatalf("References Container.Payload failed: %v", err)
	}
	if !resPayload.Found {
		t.Fatalf("expected Found=true for Container.Payload")
	}
	foundPkg5Ref := false
	for _, r := range resPayload.Refs {
		if r.Path == "pkg5/extract.go" && r.Line == 7 {
			foundPkg5Ref = true
		}
	}
	if !foundPkg5Ref {
		t.Errorf("expected reference in pkg5/extract.go:7 without direct import, got: %+v", resPayload.Refs)
	}

	// 4. Repeated cached Implementations call on immutable hash
	resCached, err := analyzer.Implementations(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkg1/iface.go",
		Line:      3,
		UTF16Col:  6,
	})
	if err != nil {
		t.Fatalf("cached Implementations failed: %v", err)
	}
	if len(resCached.Implementations) != len(implsResults[0].impls) {
		t.Errorf("cached Implementations count = %d, want %d", len(resCached.Implementations), len(implsResults[0].impls))
	}

	foundSplitImpl := false
	for _, s := range implsResults[0].impls {
		if strings.HasPrefix(s, "SplitGreeter:") {
			foundSplitImpl = true
			break
		}
	}
	if !foundSplitImpl {
		t.Errorf("expected SplitGreeter (multi-file type+method split) in Implementations, got: %+v", implsResults[0].impls)
	}

	// 5. Verify dot-import resolution: pkgdot/dot.go references Container unqualified via dot-import
	resContainer, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkg1/iface.go",
		Line:      7,
		UTF16Col:  6, // Container
	})
	if err != nil {
		t.Fatalf("References Container failed: %v", err)
	}
	if !resContainer.Found {
		t.Fatalf("expected Found=true for Container")
	}
	foundDotRef := false
	for _, r := range resContainer.Refs {
		if r.Path == "pkgdot/dot.go" {
			foundDotRef = true
			break
		}
	}
	if !foundDotRef {
		t.Errorf("expected reference to Container in pkgdot/dot.go via dot-import, got: %+v", resContainer.Refs)
	}

	// 6. Canceled context safety: test that a canceled context does not poison caches
	freshAnalyzer := NewAnalyzer()
	defer freshAnalyzer.Close()

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	_, _ = freshAnalyzer.Implementations(canceledCtx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkg1/iface.go",
		Line:      3,
		UTF16Col:  6,
	})

	resAfterCancel, err := freshAnalyzer.Implementations(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "pkg1/iface.go",
		Line:      3,
		UTF16Col:  6,
	})
	if err != nil {
		t.Fatalf("Implementations after canceled context failed: %v", err)
	}
	if len(resAfterCancel.Implementations) != len(implsResults[0].impls) {
		t.Errorf("Implementations count after canceled context = %d, want %d", len(resAfterCancel.Implementations), len(implsResults[0].impls))
	}
}

func TestLookupSiteMode(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.com")

	goMod := `module example.com/sitepkg

go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	code := `package sitepkg

import (
	"fmt"
)

var _ = fmt.Println

type MyConfig struct {
	Timeout int
	Retries int
}

func init() {
	x := 42
	_ = x
}

func ProcessData(c *MyConfig) int {
	res := c.Timeout + c.Retries
	return res
}

func Run() int {
	return ProcessData(&MyConfig{Timeout: 1, Retries: 2})
}
`
	if err := os.WriteFile(filepath.Join(dir, "site.go"), []byte(code), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "initial")
	headHash := runGit("rev-parse", "HEAD")

	cache := vcs.NewCache()
	repo, err := vcs.Open(dir, cache)
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	ctx := context.Background()

	// 1. Site lookup inside ProcessData on line 20 ("res := c.Timeout + c.Retries")
	resFunc, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "site.go",
		Line:      20,
		Site:      true,
	})
	if err != nil {
		t.Fatalf("Lookup ProcessData site failed: %v", err)
	}
	if !resFunc.Found {
		t.Fatalf("expected ProcessData site lookup to be found")
	}
	if resFunc.SymbolName != "ProcessData" {
		t.Errorf("SymbolName = %q, want ProcessData", resFunc.SymbolName)
	}
	if resFunc.DefLine != 20 {
		t.Errorf("DefLine = %d, want 20 (siteLine)", resFunc.DefLine)
	}
	if resFunc.SymLine != 19 {
		t.Errorf("SymLine = %d, want 19 (func ProcessData)", resFunc.SymLine)
	}
	if resFunc.SymCol != 5 { // "func ProcessData" -> 'P' is at col 5
		t.Errorf("SymCol = %d, want 5", resFunc.SymCol)
	}
	if !resFunc.CanRefs {
		t.Errorf("expected CanRefs=true for ProcessData")
	}
	// Verify snippet centers on line 20
	foundActive := false
	for _, snip := range resFunc.Snippet {
		if snip.Line == 20 {
			foundActive = snip.Active
		}
	}
	if !foundActive {
		t.Errorf("expected line 20 to be active in snippet")
	}

	// Follow-up: References using SymLine & SymCol
	resRefs, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "site.go",
		Line:      resFunc.SymLine,
		UTF16Col:  resFunc.SymCol,
	})
	if err != nil {
		t.Fatalf("References using SymLine/SymCol failed: %v", err)
	}
	if !resRefs.Found || len(resRefs.Refs) == 0 {
		t.Errorf("expected references for ProcessData to be found")
	}

	// 2. Site lookup inside init() on line 15 ("x := 42")
	resInit, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "site.go",
		Line:      15,
		Site:      true,
	})
	if err != nil {
		t.Fatalf("Lookup init site failed: %v", err)
	}
	if !resInit.Found {
		t.Fatalf("expected init site lookup to be found")
	}
	if resInit.SymbolName != "init" {
		t.Errorf("SymbolName = %q, want init", resInit.SymbolName)
	}
	if resInit.CanRefs {
		t.Errorf("expected CanRefs=false for func init()")
	}

	// 3. Site lookup inside struct field on line 11 ("Retries int")
	resField, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "site.go",
		Line:      11,
		Site:      true,
	})
	if err != nil {
		t.Fatalf("Lookup struct field site failed: %v", err)
	}
	if !resField.Found {
		t.Fatalf("expected struct field site lookup to be found")
	}
	if resField.SymbolName != "Retries" {
		t.Errorf("SymbolName = %q, want Retries", resField.SymbolName)
	}
	if resField.SymLine != 11 {
		t.Errorf("SymLine = %d, want 11", resField.SymLine)
	}

	// 4. Site lookup on import line 4 ('"fmt"') -> falls back to package clause
	resPkg, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   headHash,
		TargetRev: headHash,
		Side:      "R",
		FilePath:  "site.go",
		Line:      4,
		Site:      true,
	})
	if err != nil {
		t.Fatalf("Lookup import line site failed: %v", err)
	}
	if !resPkg.Found {
		t.Fatalf("expected import line fallback site lookup to be found")
	}
	if resPkg.SymbolName != "sitepkg" {
		t.Errorf("SymbolName = %q, want sitepkg", resPkg.SymbolName)
	}
	if resPkg.DefLine != 4 {
		t.Errorf("DefLine = %d, want 4 (import line)", resPkg.DefLine)
	}
	if resPkg.SymLine != 1 {
		t.Errorf("SymLine = %d, want 1 (package clause)", resPkg.SymLine)
	}
}

func TestCrossPackageSymbolSignatures(t *testing.T) {
	dir := t.TempDir()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goMod := `module example.com/testmod

go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	svcDir := filepath.Join(dir, "pkg", "service")
	if err := os.MkdirAll(svcDir, 0755); err != nil {
		t.Fatal(err)
	}
	svcContent := `package service

import "net/url"

const MaxRetries = 3

var DefaultTimeout = 60

type Client struct {
	BaseURL *url.URL
}

func (c *Client) Fetch(u *url.URL) error {
	return nil
}

func NewClient(rawURL string) (*Client, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	return &Client{BaseURL: parsed}, nil
}
`
	if err := os.WriteFile(filepath.Join(svcDir, "service.go"), []byte(svcContent), 0644); err != nil {
		t.Fatal(err)
	}

	appDir := filepath.Join(dir, "cmd", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}
	appContent := `package main

import (
	"net/url"

	"example.com/testmod/pkg/service"
)

func run() {
	client, _ := service.NewClient("https://example.com")
	_ = service.MaxRetries
	_ = service.DefaultTimeout
	_ = client.Fetch(&url.URL{})
	localURL := &url.URL{Path: "/test"}
	_ = localURL
}

func main() {
	run()
}
`
	if err := os.WriteFile(filepath.Join(appDir, "main.go"), []byte(appContent), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "initial commit")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. Exported function: NewClient
	// Looked up from cmd/app/main.go (cross-package)
	resFuncCross, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/app/main.go",
		Line:      10,
		UTF16Col:  23, // NewClient
	})
	if err != nil || !resFuncCross.Found {
		t.Fatalf("lookup NewClient cross-package failed: %v, %+v", err, resFuncCross)
	}
	// Looked up from pkg/service/service.go (defining package)
	resFuncDef, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/service/service.go",
		Line:      17,
		UTF16Col:  6, // func NewClient
	})
	if err != nil || !resFuncDef.Found {
		t.Fatalf("lookup NewClient def failed: %v, %+v", err, resFuncDef)
	}
	if strings.Contains(resFuncCross.Signature, "service.") {
		t.Errorf("cross-package func signature should omit defining package prefix, got %q", resFuncCross.Signature)
	}
	if resFuncCross.Signature != resFuncDef.Signature {
		t.Errorf("cross-package signature %q != def signature %q", resFuncCross.Signature, resFuncDef.Signature)
	}

	// 2. Method: Fetch
	// Looked up from cmd/app/main.go (cross-package)
	resMethodCross, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/app/main.go",
		Line:      13,
		UTF16Col:  14, // Fetch
	})
	if err != nil || !resMethodCross.Found {
		t.Fatalf("lookup Fetch cross-package failed: %v, %+v", err, resMethodCross)
	}
	resMethodDef, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/service/service.go",
		Line:      13,
		UTF16Col:  18, // func (c *Client) Fetch
	})
	if err != nil || !resMethodDef.Found {
		t.Fatalf("lookup Fetch def failed: %v, %+v", err, resMethodDef)
	}
	if strings.Contains(resMethodCross.Signature, "service.") {
		t.Errorf("cross-package method signature should omit defining package prefix, got %q", resMethodCross.Signature)
	}
	if !strings.Contains(resMethodCross.Signature, "url.URL") {
		t.Errorf("method signature should retain external package qualifier url., got %q", resMethodCross.Signature)
	}
	if resMethodCross.Signature != resMethodDef.Signature {
		t.Errorf("cross-package method signature %q != def signature %q", resMethodCross.Signature, resMethodDef.Signature)
	}

	// 3. Exported const: MaxRetries
	resConstCross, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/app/main.go",
		Line:      11,
		UTF16Col:  15, // MaxRetries
	})
	if err != nil || !resConstCross.Found {
		t.Fatalf("lookup MaxRetries cross-package failed: %v, %+v", err, resConstCross)
	}
	resConstDef, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/service/service.go",
		Line:      5,
		UTF16Col:  7, // MaxRetries
	})
	if err != nil || !resConstDef.Found {
		t.Fatalf("lookup MaxRetries def failed: %v, %+v", err, resConstDef)
	}
	if strings.Contains(resConstCross.Signature, "service.") {
		t.Errorf("cross-package const signature should omit defining package prefix, got %q", resConstCross.Signature)
	}
	if resConstCross.Signature != resConstDef.Signature {
		t.Errorf("cross-package const signature %q != def signature %q", resConstCross.Signature, resConstDef.Signature)
	}

	// 4. Exported var: DefaultTimeout
	resVarCross, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/app/main.go",
		Line:      12,
		UTF16Col:  15, // DefaultTimeout
	})
	if err != nil || !resVarCross.Found {
		t.Fatalf("lookup DefaultTimeout cross-package failed: %v, %+v", err, resVarCross)
	}
	resVarDef, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/service/service.go",
		Line:      7,
		UTF16Col:  5, // DefaultTimeout
	})
	if err != nil || !resVarDef.Found {
		t.Fatalf("lookup DefaultTimeout def failed: %v, %+v", err, resVarDef)
	}
	if strings.Contains(resVarCross.Signature, "service.") {
		t.Errorf("cross-package var signature should omit defining package prefix, got %q", resVarCross.Signature)
	}
	if resVarCross.Signature != resVarDef.Signature {
		t.Errorf("cross-package var signature %q != def signature %q", resVarCross.Signature, resVarDef.Signature)
	}

	// 5. Local variable: localURL
	resLocal, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/app/main.go",
		Line:      15,
		UTF16Col:  6, // localURL
	})
	if err != nil || !resLocal.Found {
		t.Fatalf("lookup localURL failed: %v, %+v", err, resLocal)
	}
	if !strings.Contains(resLocal.Signature, "*url.URL") {
		t.Errorf("local var signature should retain external package qualifier url., got %q", resLocal.Signature)
	}
}

func TestExportTestAndBuildConstraints(t *testing.T) {
	dir := t.TempDir()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goModContent := `module example.com/testmod

go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Case 1: export_test.go in external test package with custom build tags
	h2Dir := filepath.Join(dir, "http2")
	if err := os.MkdirAll(h2Dir, 0755); err != nil {
		t.Fatal(err)
	}

	transportGo := `//go:build !(go1.27 && !http2legacy)

package http2

type ClientConn struct {
	disableExtendedConnect bool
}
`
	if err := os.WriteFile(filepath.Join(h2Dir, "transport.go"), []byte(transportGo), 0644); err != nil {
		t.Fatal(err)
	}

	exportTestGo := `//go:build !(go1.27 && !http2legacy)

package http2

// SetDisableExtendedConnectProtocol configures extended CONNECT.
func SetDisableExtendedConnectProtocol(cc *ClientConn, v bool) {
	cc.disableExtendedConnect = v
}
`
	if err := os.WriteFile(filepath.Join(h2Dir, "export_test.go"), []byte(exportTestGo), 0644); err != nil {
		t.Fatal(err)
	}

	exportWrapTestGo := `//go:build go1.27 && !http2legacy

package http2

// Conflicting build tag: should be excluded by build constraint
func SetDisableExtendedConnectProtocol(cc *ClientConn, v bool) {
	panic("wrong export")
}
`
	if err := os.WriteFile(filepath.Join(h2Dir, "export_wrap_test.go"), []byte(exportWrapTestGo), 0644); err != nil {
		t.Fatal(err)
	}

	transportTestGo := `//go:build !(go1.27 && !http2legacy)

package http2_test

import (
	"testing"

	. "example.com/testmod/http2"
)

func TestExtendedConnect(t *testing.T) {
	cc := &ClientConn{}
	SetDisableExtendedConnectProtocol(cc, true)
}
`
	if err := os.WriteFile(filepath.Join(h2Dir, "transport_test.go"), []byte(transportTestGo), 0644); err != nil {
		t.Fatal(err)
	}

	// Case 2: //go:build windows non-host GOOS files
	winDir := filepath.Join(dir, "winpkg")
	if err := os.MkdirAll(winDir, 0755); err != nil {
		t.Fatal(err)
	}

	winAGo := `//go:build windows

package winpkg

func TriggerWindowsAction() string {
	return WindowsInternalHelper()
}
`
	if err := os.WriteFile(filepath.Join(winDir, "win_a.go"), []byte(winAGo), 0644); err != nil {
		t.Fatal(err)
	}

	winBGo := `//go:build windows

package winpkg

// WindowsInternalHelper returns a platform string.
func WindowsInternalHelper() string {
	return "windows-ok"
}
`
	if err := os.WriteFile(filepath.Join(winDir, "win_b.go"), []byte(winBGo), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "Initial test commit")

	cache := vcs.NewCache()
	repo, err := vcs.Open(dir, cache)
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	ctx := context.Background()

	// 1. Lookup SetDisableExtendedConnectProtocol from transport_test.go
	resH2, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "http2/transport_test.go",
		Line:      13,
		UTF16Col:  3, // SetDisableExtendedConnectProtocol
	})
	if err != nil || !resH2.Found {
		t.Fatalf("Lookup SetDisableExtendedConnectProtocol failed: %v, %+v", err, resH2)
	}
	if resH2.DefFile != "export_test.go" {
		t.Errorf("expected DefFile export_test.go, got %q", resH2.DefFile)
	}
	if resH2.DefPath != "http2/export_test.go" {
		t.Errorf("expected DefPath http2/export_test.go, got %q", resH2.DefPath)
	}
	if !strings.Contains(resH2.Signature, "SetDisableExtendedConnectProtocol(cc *ClientConn, v bool)") {
		t.Errorf("expected signature to contain SetDisableExtendedConnectProtocol, got %q", resH2.Signature)
	}
	if len(resH2.Snippet) == 0 {
		t.Errorf("expected non-empty snippet from export_test.go")
	}

	// 2. References for SetDisableExtendedConnectProtocol finds transport_test.go call site
	refsH2, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "http2/transport_test.go",
		Line:      13,
		UTF16Col:  3,
	})
	if err != nil || !refsH2.Found {
		t.Fatalf("References SetDisableExtendedConnectProtocol failed: %v, %+v", err, refsH2)
	}
	foundCallSite := false
	for _, r := range refsH2.Refs {
		if r.Path == "http2/transport_test.go" && r.Line == 13 {
			foundCallSite = true
			break
		}
	}
	if !foundCallSite {
		t.Errorf("expected references to include http2/transport_test.go:13, got: %+v", refsH2.Refs)
	}

	// 3. Lookup non-host GOOS symbol: WindowsInternalHelper in win_a.go -> win_b.go
	resWin, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "winpkg/win_a.go",
		Line:      6,
		UTF16Col:  9, // WindowsInternalHelper
	})
	if err != nil || !resWin.Found {
		t.Fatalf("Lookup WindowsInternalHelper failed: %v, %+v", err, resWin)
	}
	if resWin.DefFile != "win_b.go" {
		t.Errorf("expected DefFile win_b.go, got %q", resWin.DefFile)
	}
	if !strings.Contains(resWin.Signature, "WindowsInternalHelper() string") {
		t.Errorf("expected signature WindowsInternalHelper() string, got %q", resWin.Signature)
	}

	// 4. Chained lookup & site lookup for dot-imported export_test.go symbol in test package
	resChained, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:          repo,
		RepoRoot:      dir,
		BaseRev:       "HEAD",
		TargetRev:     "HEAD",
		Side:          "R",
		PkgFile:       "transport_test.go",
		PkgImportPath: "example.com/testmod/http2_test",
		Line:          13,
		UTF16Col:      3,
	})
	if err != nil || !resChained.Found {
		t.Fatalf("Chained lookup SetDisableExtendedConnectProtocol failed: %v, %+v", err, resChained)
	}
	if resChained.DefFile != "export_test.go" {
		t.Errorf("expected DefFile export_test.go, got %q", resChained.DefFile)
	}

	resSite, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:          repo,
		RepoRoot:      dir,
		BaseRev:       "HEAD",
		TargetRev:     "HEAD",
		Side:          "R",
		PkgFile:       "transport_test.go",
		PkgImportPath: "example.com/testmod/http2_test",
		Site:          true,
		Line:          13,
	})
	if err != nil || !resSite.Found {
		t.Fatalf("Site lookup TestExtendedConnect failed: %v, %+v", err, resSite)
	}
	if resSite.SymbolName != "TestExtendedConnect" {
		t.Errorf("expected SymbolName TestExtendedConnect, got %q", resSite.SymbolName)
	}
	if resSite.DefPath != "http2/transport_test.go" && resSite.DefFile != "transport_test.go" {
		t.Errorf("expected DefPath http2/transport_test.go, got %q (file %q)", resSite.DefPath, resSite.DefFile)
	}

	// 5. Test parseBuildConstraints with multi-line block comments and non-space package declaration
	blockCommentContent := []byte(`/*
 * License header
 */
//go:build linux && !arm
package	foo
`)
	expr := parseBuildConstraints(blockCommentContent)
	if expr == nil || expr.String() != "linux && !arm" {
		t.Errorf("expected build constraint 'linux && !arm', got %v", expr)
	}
}

func TestReferencesEnclosingSymbolAndCallers(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goModContent := `module example.com/testmod

go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	targetDir := filepath.Join(dir, "pkg", "target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	targetSrc := `package target

type Item int

const MaxLimit Item = 100
`
	if err := os.WriteFile(filepath.Join(targetDir, "target.go"), []byte(targetSrc), 0644); err != nil {
		t.Fatal(err)
	}

	mainSrc := `package main

import (
	"example.com/testmod/pkg/target"
)

var _ = target.Item(0)

var GlobalConfig = target.Item(1)

const DefaultLimit = target.MaxLimit

type CustomItem target.Item

func init() {
	_ = target.Item(2)
}

func ProcessItem() {
	_ = target.Item(3)
}

type Server struct{}

func (s *Server) Serve() {
	ProcessItem()
	_ = target.Item(4)
}

type Config struct{}

func (c Config) Validate() {
	_ = target.Item(5)
}

type Cache[T any] struct{}

func (c *Cache[T]) Get() {
	_ = target.Item(6)
}

func main() {
	var s Server
	s.Serve()
}

var (
	VarA = target.Item(7)
	VarB = target.Item(8)
	_ = target.Item(9)
	VarC = target.Item(10)
	MultiA, MultiB = target.Item(11),
		target.Item(12)
)
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSrc), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. References for target.Item (line 3, col 6 in pkg/target/target.go)
	reqItem := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/target/target.go",
		Line:      3,
		UTF16Col:  6,
	}

	resItem, err := analyzer.References(ctx, reqItem)
	if err != nil || !resItem.Found {
		t.Fatalf("References for target.Item failed: %v, %+v", err, resItem)
	}

	containerByLine := make(map[int]Reference)
	for _, ref := range resItem.Refs {
		if ref.Path == "main.go" {
			containerByLine[ref.Line] = ref
		}
	}

	// Line 7: var _ = target.Item(0) -> Container should be empty
	if r, ok := containerByLine[7]; !ok || r.Container != "" {
		t.Errorf("line 7 blank var: expected empty Container, got %q", r.Container)
	}

	// Line 9: var GlobalConfig = target.Item(1) -> Container "GlobalConfig"
	if r, ok := containerByLine[9]; !ok || r.Container != "GlobalConfig" || r.ContainerLine != 9 {
		t.Errorf("line 9 var: expected Container GlobalConfig at line 9, got %+v", r)
	}

	// Line 13: type CustomItem target.Item -> Container "CustomItem"
	if r, ok := containerByLine[13]; !ok || r.Container != "CustomItem" || r.ContainerLine != 13 {
		t.Errorf("line 13 type: expected Container CustomItem at line 13, got %+v", r)
	}

	// Line 16: inside func init() -> Container "init", ContainerLine == 0
	if r, ok := containerByLine[16]; !ok || r.Container != "init" || r.ContainerLine != 0 {
		t.Errorf("line 16 init: expected Container init with line 0, got %+v", r)
	}

	// Line 20: inside ProcessItem -> Container "ProcessItem", ContainerLine == 19
	if r, ok := containerByLine[20]; !ok || r.Container != "ProcessItem" || r.ContainerLine != 19 {
		t.Errorf("line 20 func: expected Container ProcessItem at line 19, got %+v", r)
	}

	// Line 27: inside (*Server).Serve -> Container "(*Server).Serve", ContainerLine == 25
	if r, ok := containerByLine[27]; !ok || r.Container != "(*Server).Serve" || r.ContainerLine != 25 {
		t.Errorf("line 27 ptr method: expected Container (*Server).Serve at line 25, got %+v", r)
	}

	// Line 33: inside (Config).Validate -> Container "(Config).Validate", ContainerLine == 32
	if r, ok := containerByLine[33]; !ok || r.Container != "(Config).Validate" || r.ContainerLine != 32 {
		t.Errorf("line 33 value method: expected Container (Config).Validate at line 32, got %+v", r)
	}

	// Line 39: inside (*Cache).Get -> Container "(*Cache).Get", ContainerLine == 38
	if r, ok := containerByLine[39]; !ok || r.Container != "(*Cache).Get" || r.ContainerLine != 38 {
		t.Errorf("line 39 generic method: expected Container (*Cache).Get at line 38, got %+v", r)
	}

	// Line 48: VarA = target.Item(7)
	if r, ok := containerByLine[48]; !ok || r.Container != "VarA" || r.ContainerLine != 48 {
		t.Errorf("line 48 VarA: expected Container VarA at line 48, got %+v", r)
	}
	// Line 49: VarB = target.Item(8)
	if r, ok := containerByLine[49]; !ok || r.Container != "VarB" || r.ContainerLine != 49 {
		t.Errorf("line 49 VarB: expected Container VarB at line 49, got %+v", r)
	}
	// Line 50: _ = target.Item(9)
	if r, ok := containerByLine[50]; !ok || r.Container != "" {
		t.Errorf("line 50 blank: expected empty Container, got %+v", r)
	}
	// Line 51: VarC = target.Item(10)
	if r, ok := containerByLine[51]; !ok || r.Container != "VarC" || r.ContainerLine != 51 {
		t.Errorf("line 51 VarC: expected Container VarC at line 51, got %+v", r)
	}
	// Line 52: MultiA = target.Item(11)
	if r, ok := containerByLine[52]; !ok || r.Container != "MultiA" || r.ContainerLine != 52 {
		t.Errorf("line 52 MultiA: expected Container MultiA at line 52, got %+v", r)
	}
	// Line 53: MultiB = target.Item(12)
	if r, ok := containerByLine[53]; !ok || r.Container != "MultiB" || r.ContainerLine != 52 {
		t.Errorf("line 53 MultiB: expected Container MultiB at line 52, got %+v", r)
	}

	// Site lookup on line 51 resolves VarC
	resSiteVarC, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      51,
		Site:      true,
	})
	if err != nil || !resSiteVarC.Found || resSiteVarC.SymbolName != "VarC" {
		t.Errorf("expected Site lookup on line 51 to resolve VarC, got err=%v, res=%+v", err, resSiteVarC)
	}

	// Site lookup on line 53 resolves MultiB
	resSiteMultiB, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      53,
		Site:      true,
	})
	if err != nil || !resSiteMultiB.Found || resSiteMultiB.SymbolName != "MultiB" {
		t.Errorf("expected Site lookup on line 53 to resolve MultiB, got err=%v, res=%+v", err, resSiteMultiB)
	}

	// 2. References for target.MaxLimit -> line 11: const DefaultLimit = target.MaxLimit
	resLimit, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/target/target.go",
		Line:      5,
		UTF16Col:  7,
	})
	if err != nil || !resLimit.Found {
		t.Fatalf("References for MaxLimit failed: %v", err)
	}
	var foundConst bool
	for _, ref := range resLimit.Refs {
		if ref.Path == "main.go" && ref.Line == 11 {
			foundConst = true
			if ref.Container != "DefaultLimit" || ref.ContainerLine != 11 {
				t.Errorf("const ref: expected Container DefaultLimit at line 11, got %+v", ref)
			}
		}
	}
	if !foundConst {
		t.Errorf("expected reference to MaxLimit at line 11")
	}

	// 3. Caller finding: ProcessItem (line 19, col 6) is called in Serve (line 26)
	resProcess, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      19,
		UTF16Col:  6,
	})
	if err != nil || !resProcess.Found {
		t.Fatalf("References for ProcessItem failed: %v", err)
	}
	var serveRef *Reference
	for i := range resProcess.Refs {
		if resProcess.Refs[i].Path == "main.go" && resProcess.Refs[i].Line == 26 {
			serveRef = &resProcess.Refs[i]
			break
		}
	}
	if serveRef == nil {
		t.Fatalf("expected caller reference in Serve at line 26, got %+v", resProcess.Refs)
	}
	if serveRef.Container != "(*Server).Serve" || serveRef.ContainerLine != 25 {
		t.Errorf("expected caller container (*Server).Serve at line 25, got %+v", serveRef)
	}

	// 4. Multi-hop caller: query references for Serve using ContainerLine and ContainerUTF16Col
	resServe, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "main.go",
		Line:      serveRef.ContainerLine,
		UTF16Col:  serveRef.ContainerUTF16Col,
	})
	if err != nil || !resServe.Found {
		t.Fatalf("References for Serve failed: %v", err)
	}
	var mainRef *Reference
	for i := range resServe.Refs {
		if resServe.Refs[i].Path == "main.go" && resServe.Refs[i].Line == 44 {
			mainRef = &resServe.Refs[i]
			break
		}
	}
	if mainRef == nil {
		t.Fatalf("expected caller reference in main at line 44, got %+v", resServe.Refs)
	}
	if mainRef.Container != "main" || mainRef.ContainerLine != 42 {
		t.Errorf("expected caller container main at line 42, got %+v", mainRef)
	}
}

func TestCheckedPkgCacheReuse(t *testing.T) {
	// 1. Verify checkedPkgLRU eviction and capacity semantics
	lru := newCheckedPkgLRU(2)
	k1 := checkedPkgKey{importPath: "pkg1"}
	k2 := checkedPkgKey{importPath: "pkg2"}
	k3 := checkedPkgKey{importPath: "pkg3"}

	e1 := &checkedPkgEntry{}
	e2 := &checkedPkgEntry{}
	e3 := &checkedPkgEntry{}

	lru.put(k1, e1)
	lru.put(k2, e2)
	if lru.get(k1) != e1 || lru.get(k2) != e2 {
		t.Fatalf("expected k1 and k2 in LRU")
	}

	// Insert k3 -> should evict k1 (since k2 was accessed most recently by get)
	lru.put(k3, e3)
	if lru.get(k1) != nil {
		t.Errorf("expected k1 to be evicted")
	}
	if lru.get(k2) != e2 || lru.get(k3) != e3 {
		t.Errorf("expected k2 and k3 in LRU")
	}

	// 2. Integration: verify package caching across multiple references queries and concurrent callers
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	reqAdd := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/calc/calc.go",
		Line:      4,
		UTF16Col:  6,
	}

	res1, err := analyzer.References(ctx, reqAdd)
	if err != nil || !res1.Found {
		t.Fatalf("first References call failed: %v", err)
	}

	// Second call should hit the checked package and references caches
	res2, err := analyzer.References(ctx, reqAdd)
	if err != nil || !res2.Found {
		t.Fatalf("second References call failed: %v", err)
	}
	if len(res1.Refs) != len(res2.Refs) {
		t.Fatalf("cached refs count mismatch: %d vs %d", len(res1.Refs), len(res2.Refs))
	}

	// Concurrent queries on the same revision should safely reuse caches
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := analyzer.References(ctx, reqAdd)
			if err != nil || !res.Found {
				t.Errorf("concurrent References failed: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestReferencesBatch(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	reqAdd := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/calc/calc.go",
		Line:      4,
		UTF16Col:  6,
	}
	reqSub := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/calc/calc.go",
		Line:      9,
		UTF16Col:  6,
	}
	reqRunAdd := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      10,
		UTF16Col:  6,
	}

	// 1. Empty batch
	emptyBatch, err := analyzer.ReferencesBatch(ctx, nil)
	if err != nil {
		t.Fatalf("ReferencesBatch(nil) failed: %v", err)
	}
	if len(emptyBatch) != 0 {
		t.Fatalf("expected empty batch result, got %d", len(emptyBatch))
	}

	// 2. Individual reference queries
	resAdd, err := analyzer.References(ctx, reqAdd)
	if err != nil || !resAdd.Found {
		t.Fatalf("References for Add failed: %v", err)
	}
	resSub, err := analyzer.References(ctx, reqSub)
	if err != nil || !resSub.Found {
		t.Fatalf("References for Sub failed: %v", err)
	}
	resRunAdd, err := analyzer.References(ctx, reqRunAdd)
	if err != nil || !resRunAdd.Found {
		t.Fatalf("References for runAdd failed: %v", err)
	}

	// 3. Batch reference query
	batch, err := analyzer.ReferencesBatch(ctx, []LookupRequest{reqAdd, reqSub, reqRunAdd})
	if err != nil {
		t.Fatalf("ReferencesBatch failed: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("expected 3 batch results, got %d", len(batch))
	}

	expected := []*RefsResult{resAdd, resSub, resRunAdd}
	for i, exp := range expected {
		got := batch[i]
		if got == nil {
			t.Fatalf("batch result %d is nil", i)
		}
		if got.Found != exp.Found {
			t.Errorf("batch %d: Found mismatch: %v vs %v", i, got.Found, exp.Found)
		}
		if got.SymbolName != exp.SymbolName {
			t.Errorf("batch %d: SymbolName mismatch: %q vs %q", i, got.SymbolName, exp.SymbolName)
		}
		if len(got.Refs) != len(exp.Refs) {
			t.Fatalf("batch %d: Refs len mismatch: %d vs %d", i, len(got.Refs), len(exp.Refs))
		}
		for j := range exp.Refs {
			if got.Refs[j].Path != exp.Refs[j].Path || got.Refs[j].Line != exp.Refs[j].Line ||
				got.Refs[j].Container != exp.Refs[j].Container {
				t.Errorf("batch %d ref %d mismatch: got %+v, exp %+v", i, j, got.Refs[j], exp.Refs[j])
			}
		}
	}
}

func TestScanGoHeaderImports(t *testing.T) {
	tests := []struct {
		name        string
		src         string
		wantPkg     string
		wantExact   bool
		wantImports []string
	}{
		{
			name:        "simple single import",
			src:         "package mypkg\n\nimport \"fmt\"\n\nfunc Main() {}\n",
			wantPkg:     "mypkg",
			wantExact:   true,
			wantImports: []string{"fmt"},
		},
		{
			name:        "grouped imports with aliases, dot, blank, and raw string",
			src:         "package demo\n\nimport (\n\t\"fmt\"\n\tmypath `net/http`\n\t. \"strings\"\n\t_ \"sync/atomic\"\n)\n\ntype Foo struct{}\n",
			wantPkg:     "demo",
			wantExact:   true,
			wantImports: []string{"fmt", "net/http", "strings", "sync/atomic"},
		},
		{
			name:        "comments containing fake imports and raw strings",
			src:         "// Package comment with \"fake/pkg\"\n/* multi-line comment\n   import \"ignored\"\n*/\npackage com\n\n// comment\nimport (\n\t// inline comment\n\t\"os\" // another comment\n)\n\nvar X = 1\n",
			wantPkg:     "com",
			wantExact:   true,
			wantImports: []string{"os"},
		},
		{
			name:        "no imports",
			src:         "package noimp\n\nconst A = 1\n",
			wantPkg:     "noimp",
			wantExact:   true,
			wantImports: nil,
		},
		{
			name:        "semicolon-separated header",
			src:         "package semi; import \"math\"; func F() {}\n",
			wantPkg:     "semi",
			wantExact:   true,
			wantImports: []string{"math"},
		},
		{
			name:      "malformed header unclosed quote",
			src:       "package malformed\nimport \"unclosed\nfunc F() {}\n",
			wantPkg:   "malformed",
			wantExact: false,
		},
		{
			name:      "malformed header unclosed paren",
			src:       "package malformed\nimport (\n\"fmt\"\n",
			wantPkg:   "malformed",
			wantExact: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotImports, gotExact, gotPkg := scanGoHeaderImports([]byte(tc.src))
			if gotPkg != tc.wantPkg {
				t.Errorf("got pkg %q, want %q", gotPkg, tc.wantPkg)
			}
			if gotExact != tc.wantExact {
				t.Errorf("got exact %v, want %v", gotExact, tc.wantExact)
			}
			if tc.wantExact {
				if !slices.Equal(gotImports, tc.wantImports) {
					t.Errorf("got imports %v, want %v", gotImports, tc.wantImports)
				}
			}
		})
	}
}

func TestSameLineMultipleFilesReference(t *testing.T) {
	// Tests Bug #1: two files in the same package referencing target symbol on the exact same line number.
	repoDir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")

	goMod := "module example.com/sameline\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	pkgDir := filepath.Join(repoDir, "pkg", "service")
	_ = os.MkdirAll(pkgDir, 0755)

	targetGo := `package service

// SharedTarget is the target symbol.
func SharedTarget() int {
	return 42
}
`
	_ = os.WriteFile(filepath.Join(pkgDir, "target.go"), []byte(targetGo), 0644)

	// In file_a.go, SharedTarget() is called on line 5
	fileAGo := `package service

func CallerA() {
	_ = 1
	SharedTarget()
}
`
	_ = os.WriteFile(filepath.Join(pkgDir, "file_a.go"), []byte(fileAGo), 0644)

	// In file_b.go, SharedTarget() is ALSO called on line 5
	fileBGo := `package service

func CallerB() {
	_ = 2
	SharedTarget()
}
`
	_ = os.WriteFile(filepath.Join(pkgDir, "file_b.go"), []byte(fileBGo), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(repoDir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	res, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/service/target.go",
		Line:      4,
		UTF16Col:  6,
	})
	if err != nil {
		t.Fatalf("References failed: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected target to be found, got %+v", res)
	}

	foundA := false
	foundB := false
	for _, ref := range res.Refs {
		if ref.Path == "pkg/service/file_a.go" && ref.Line == 5 {
			foundA = true
		}
		if ref.Path == "pkg/service/file_b.go" && ref.Line == 5 {
			foundB = true
		}
	}

	if !foundA || !foundB {
		t.Fatalf("expected references on line 5 in BOTH file_a.go (found=%v) and file_b.go (found=%v); all refs: %+v", foundA, foundB, res.Refs)
	}
}

func TestInternalTestReferencesWarmCache(t *testing.T) {
	// Tests Bug #2: internal *_test.go references across packages when cache is warm.
	repoDir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")

	goMod := "module example.com/testwarm\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	coreDir := filepath.Join(repoDir, "pkg", "core")
	_ = os.MkdirAll(coreDir, 0755)
	coreGo := `package core

func CoreFunc() string {
	return "core"
}
`
	_ = os.WriteFile(filepath.Join(coreDir, "core.go"), []byte(coreGo), 0644)

	consumerDir := filepath.Join(repoDir, "pkg", "consumer")
	_ = os.MkdirAll(consumerDir, 0755)
	consumerGo := `package consumer

import "example.com/testwarm/pkg/core"

func NormalFunc() string {
	return core.CoreFunc()
}
`
	_ = os.WriteFile(filepath.Join(consumerDir, "consumer.go"), []byte(consumerGo), 0644)

	// Internal test: package consumer (NOT consumer_test)
	consumerTestGo := `package consumer

import (
	"testing"
	"example.com/testwarm/pkg/core"
)

func TestInternal(t *testing.T) {
	_ = core.CoreFunc()
}
`
	_ = os.WriteFile(filepath.Join(consumerDir, "consumer_test.go"), []byte(consumerTestGo), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")

	repo, err := vcs.Open(repoDir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// 1. Warm revCache by looking up NormalFunc in consumer.go (populates checkedPkgs with package consumer WITHOUT tests)
	lookupRes, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/consumer/consumer.go",
		Line:      5,
		UTF16Col:  6,
	})
	if err != nil || !lookupRes.Found {
		t.Fatalf("warm Lookup failed: err=%v, res=%+v", err, lookupRes)
	}

	// 2. Query references for CoreFunc in core.go
	refsRes, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/core/core.go",
		Line:      3,
		UTF16Col:  6,
	})
	if err != nil || !refsRes.Found {
		t.Fatalf("References failed: err=%v, res=%+v", err, refsRes)
	}

	foundTest := false
	foundProd := false
	for _, ref := range refsRes.Refs {
		if ref.Path == "pkg/consumer/consumer_test.go" {
			foundTest = true
		}
		if ref.Path == "pkg/consumer/consumer.go" {
			foundProd = true
		}
	}

	if !foundProd {
		t.Errorf("expected to find reference in consumer.go, got: %+v", refsRes.Refs)
	}
	if !foundTest {
		t.Errorf("expected to find reference in internal test file consumer_test.go with warm cache, got: %+v", refsRes.Refs)
	}
}

func TestBuildIgnoredSiblingPackageNoDuplicates(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")

	goMod := "module example.com/ignoretest\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	pkgDir := filepath.Join(repoDir, "pkg", "service")
	_ = os.MkdirAll(pkgDir, 0755)

	svcGo := `package service

func TargetFunc() int {
	return 42
}

func Helper() int {
	return TargetFunc()
}
`
	_ = os.WriteFile(filepath.Join(pkgDir, "service.go"), []byte(svcGo), 0644)

	// Sibling file in same directory with package main and //go:build ignore
	ignoreGo := `//go:build ignore

package main

import "example.com/ignoretest/pkg/service"

func IgnoredLocalHelper() int {
	return 100
}

func main() {
	_ = service.TargetFunc()
	_ = IgnoredLocalHelper()
}
`
	_ = os.WriteFile(filepath.Join(pkgDir, "ignore.go"), []byte(ignoreGo), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headRev := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(repoDir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   headRev,
		TargetRev: headRev,
		Side:      "R",
		FilePath:  "pkg/service/service.go",
		Line:      3,
		UTF16Col:  6,
	}

	// 1. Cold run
	resCold, err := analyzer.References(ctx, req)
	if err != nil {
		t.Fatalf("cold References failed: %v", err)
	}
	if !resCold.Found {
		t.Fatalf("expected target to be found on cold run, got %+v", resCold)
	}
	checkNoDuplicates := func(name string, refs []Reference) {
		if len(refs) == 0 {
			t.Fatalf("%s: expected at least one reference", name)
		}
		seen := make(map[string]bool)
		for _, ref := range refs {
			key := fmt.Sprintf("%s:%d", ref.Path, ref.Line)
			if seen[key] {
				t.Fatalf("%s: duplicate reference found for %s", name, key)
			}
			seen[key] = true
		}
	}
	checkNoDuplicates("cold", resCold.Refs)

	// 2. Warm run
	resWarm, err := analyzer.References(ctx, req)
	if err != nil {
		t.Fatalf("warm References failed: %v", err)
	}
	if !resWarm.Found {
		t.Fatalf("expected target to be found on warm run, got %+v", resWarm)
	}
	checkNoDuplicates("warm", resWarm.Refs)

	if len(resCold.Refs) != len(resWarm.Refs) {
		t.Fatalf("cold vs warm mismatch: cold has %d refs, warm has %d refs", len(resCold.Refs), len(resWarm.Refs))
	}

	// 3. Click function declared and called inside //go:build ignore file itself with revCache != nil
	reqIgnore := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   headRev,
		TargetRev: headRev,
		Side:      "R",
		FilePath:  "pkg/service/ignore.go",
		Line:      7,
		UTF16Col:  6,
	}
	resIgnore, err := analyzer.References(ctx, reqIgnore)
	if err != nil {
		t.Fatalf("References on ignored file function failed: %v", err)
	}
	if !resIgnore.Found {
		t.Fatalf("expected function in build-ignored file to be found, got %+v", resIgnore)
	}
	if resIgnore.SymbolName != "IgnoredLocalHelper" {
		t.Errorf("expected SymbolName 'IgnoredLocalHelper', got %q", resIgnore.SymbolName)
	}
	if len(resIgnore.Refs) == 0 {
		t.Fatalf("expected references for IgnoredLocalHelper in build-ignored file, got 0")
	}
	checkNoDuplicates("ignore", resIgnore.Refs)
}

func TestReferencesBatchSingleLineMultiVar(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")

	goMod := "module example.com/multivar\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	pkgDir := filepath.Join(repoDir, "pkg")
	_ = os.MkdirAll(pkgDir, 0755)

	src := `package pkg

func Foo() int { return 1 }
func Bar() int { return 2 }

var a, b = Foo(), Bar()

func Use() int {
	return a + b
}
`
	_ = os.WriteFile(filepath.Join(pkgDir, "code.go"), []byte(src), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")
	headRev := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(repoDir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Line 6 is "var a, b = Foo(), Bar()"
	// col of 'a' is 5, col of 'b' is 8.
	reqA := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   headRev,
		TargetRev: headRev,
		Side:      "R",
		FilePath:  "pkg/code.go",
		Line:      6,
		UTF16Col:  5,
		Site:      false,
	}
	reqB := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoDir,
		BaseRev:   headRev,
		TargetRev: headRev,
		Side:      "R",
		FilePath:  "pkg/code.go",
		Line:      6,
		UTF16Col:  8,
		Site:      false,
	}

	batchRes, err := analyzer.ReferencesBatch(ctx, []LookupRequest{reqA, reqB})
	if err != nil {
		t.Fatalf("ReferencesBatch failed: %v", err)
	}
	if len(batchRes) != 2 {
		t.Fatalf("expected 2 results, got %d", len(batchRes))
	}
	if batchRes[0] == nil || !batchRes[0].Found || batchRes[0].SymbolName != "a" {
		t.Fatalf("expected target 'a', got %+v", batchRes[0])
	}
	if batchRes[1] == nil || !batchRes[1].Found || batchRes[1].SymbolName != "b" {
		t.Fatalf("expected target 'b', got %+v", batchRes[1])
	}
}

type largeRepoMock struct {
	vcs.Repo
	syntheticFiles map[string][]byte
	contentCalls   atomic.Int64
	trackDummy     atomic.Bool
}

func (m *largeRepoMock) ListGoFiles(rev string) ([]string, error) {
	files, err := m.Repo.ListGoFiles(rev)
	if err != nil {
		return nil, err
	}
	var res []string
	res = append(res, files...)
	for p := range m.syntheticFiles {
		res = append(res, p)
	}
	slices.Sort(res)
	return res, nil
}

func (m *largeRepoMock) Content(rev, path string) ([]byte, error) {
	if b, ok := m.syntheticFiles[path]; ok {
		if m.trackDummy.Load() {
			m.contentCalls.Add(1)
		}
		return b, nil
	}
	return m.Repo.Content(rev, path)
}

func TestLargeRepoImplsPrunesUnrelatedFiles(t *testing.T) {
	// Tests that when repo has >1500 files and fileIdx is present,
	// Repo.Content is NOT called on dummy files whose Bloom filters lack the method/interface names.
	repoDir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")

	goMod := "module example.com/largerepo\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	ifaceDir := filepath.Join(repoDir, "pkg", "iface")
	_ = os.MkdirAll(ifaceDir, 0755)
	ifaceGo := `package iface

type Worker interface {
	DoWork() string
}
`
	_ = os.WriteFile(filepath.Join(ifaceDir, "iface.go"), []byte(ifaceGo), 0644)

	implDir := filepath.Join(repoDir, "pkg", "impl")
	_ = os.MkdirAll(implDir, 0755)
	implGo := `package impl

type RealWorker struct{}

func (r RealWorker) DoWork() string {
	return "done"
}
`
	_ = os.WriteFile(filepath.Join(implDir, "impl.go"), []byte(implGo), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "init")

	realRepo, err := vcs.Open(repoDir, vcs.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	headRev := runGit("rev-parse", "HEAD")

	synthetic := make(map[string][]byte)
	dummyContent := []byte("package unrelated\n\ntype DummyStruct struct{}\ntype DummyIface interface{ Foo() }\n")
	for i := 0; i < 1550; i++ {
		p := fmt.Sprintf("pkg/unrelated/dummy_%04d.go", i)
		synthetic[p] = dummyContent
	}

	mockRepo := &largeRepoMock{
		Repo:           realRepo,
		syntheticFiles: synthetic,
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	// Pre-index files by running an untracked Implementations so revCache repoFileIndex is built
	_, _ = analyzer.Implementations(ctx, LookupRequest{
		Repo:      mockRepo,
		RepoRoot:  repoDir,
		BaseRev:   headRev,
		TargetRev: headRev,
		Side:      "R",
		FilePath:  "pkg/iface/iface.go",
		Line:      3,
		UTF16Col:  6,
	})

	// Direction 1: interface -> concrete implementations
	mockRepo.contentCalls.Store(0)
	mockRepo.trackDummy.Store(true)

	dir1Res, err := analyzer.Implementations(ctx, LookupRequest{
		Repo:      mockRepo,
		RepoRoot:  repoDir,
		BaseRev:   headRev,
		TargetRev: headRev,
		Side:      "R",
		FilePath:  "pkg/iface/iface.go",
		Line:      3,
		UTF16Col:  6,
	})
	mockRepo.trackDummy.Store(false)
	if err != nil || !dir1Res.Found {
		t.Fatalf("Direction 1 Implementations failed: err=%v, res=%+v", err, dir1Res)
	}
	if calls := mockRepo.contentCalls.Load(); calls != 0 {
		t.Errorf("Direction 1: expected 0 Repo.Content calls on dummy files, got %d", calls)
	}

	// Direction 2: concrete -> implemented interfaces
	mockRepo.contentCalls.Store(0)
	mockRepo.trackDummy.Store(true)

	dir2Res, err := analyzer.Implementations(ctx, LookupRequest{
		Repo:      mockRepo,
		RepoRoot:  repoDir,
		BaseRev:   headRev,
		TargetRev: headRev,
		Side:      "R",
		FilePath:  "pkg/impl/impl.go",
		Line:      3,
		UTF16Col:  6,
	})
	mockRepo.trackDummy.Store(false)
	if err != nil || !dir2Res.Found {
		t.Fatalf("Direction 2 Implementations failed: err=%v, res=%+v", err, dir2Res)
	}
	if calls := mockRepo.contentCalls.Load(); calls != 0 {
		t.Errorf("Direction 2: expected 0 Repo.Content calls on dummy files, got %d", calls)
	}
}

func BenchmarkCallerExpansionBatch(b *testing.B) {
	repo, repoRoot := setupTestGitRepo(b)
	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	reqAdd := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/calc/calc.go",
		Line:      4,
		UTF16Col:  6,
	}
	reqSub := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "pkg/calc/calc.go",
		Line:      9,
		UTF16Col:  6,
	}
	reqRunAdd := LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		BaseRev:   "HEAD",
		TargetRev: "HEAD",
		Side:      "R",
		FilePath:  "cmd/main.go",
		Line:      10,
		UTF16Col:  6,
	}
	reqs := []LookupRequest{reqAdd, reqSub, reqRunAdd}

	// Warm caches
	if _, err := analyzer.ReferencesBatch(ctx, reqs); err != nil {
		b.Fatal(err)
	}

	b.Run("Sequential", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, req := range reqs {
				if _, err := analyzer.References(ctx, req); err != nil {
					b.Fatal(err)
				}
			}
		}
	})

	b.Run("Batch", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := analyzer.ReferencesBatch(ctx, reqs); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func setupInterfaceMethodTestRepo(tb testing.TB) (vcs.Repo, string, string) {
	tb.Helper()
	dir := tb.TempDir()

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			tb.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	goMod := "module example.com/testmod\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		tb.Fatal(err)
	}

	writeSource := func(relPath, content string) {
		fullPath := filepath.Join(dir, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			tb.Fatal(err)
		}
	}

	writeSource("pkg/h3/server.go", `package h3

import "context"

type netHTTPServer struct{}

func (s netHTTPServer) Shutdown(ctx context.Context) error {
	return nil
}

func (s netHTTPServer) DirectCall(ctx context.Context) {
	_ = s.Shutdown(ctx)
}
`)

	writeSource("pkg/server/server.go", `package server

import "context"

type http3Server interface {
	Shutdown(ctx context.Context) error
}

type Server struct {
	h3Server http3Server
}

func (s *Server) Close(ctx context.Context) error {
	if s.h3Server != nil {
		return s.h3Server.Shutdown(ctx)
	}
	return nil
}

func AnonCall(x interface{ Shutdown(context.Context) error }, ctx context.Context) {
	_ = x.Shutdown(ctx)
}

func GenericCall[T interface{ Shutdown(context.Context) error }](x T, ctx context.Context) {
	_ = x.Shutdown(ctx)
}

func BadSignatureCall(x interface{ Shutdown(int) }, ctx context.Context) {
	x.Shutdown(1)
}
`)

	writeSource("pkg/other/other.go", `package other

import "context"

type otherServer struct{}

func (o otherServer) Shutdown(ctx context.Context) error {
	return nil
}

func (o otherServer) LocalCall(ctx context.Context) {
	_ = o.Shutdown(ctx)
}
`)

	writeSource("pkg/service/service.go", `package service

type Worker struct{}

func (w Worker) Work() {}

func (w *Worker) Stop() {}

type Service interface {
	Work()
	Stop()
}

func RunService(s Service) {
	s.Work()
	s.Stop()
}
`)

	writeSource("pkg/errpkg/err.go", `package errpkg

type MyErr struct{}

func (e *MyErr) Error() string {
	return "custom"
}

func CallMyErr(e *MyErr) {
	_ = e.Error()
}

func CallStdErr(err error) {
	_ = err.Error()
}
`)

	writeSource("pkg/unexported/unexp.go", `package unexported

type runner interface {
	run() error
}

type worker struct{}

func (w worker) run() error {
	return nil
}

type Dispatcher struct {
	r runner
}

func (d *Dispatcher) Execute() error {
	if d.r != nil {
		return d.r.run()
	}
	return nil
}
`)

	writeSource("pkg/disambig/disambig.go", `package disambig

func CallAnonInt(x interface{ Shutdown(int) }) {
	x.Shutdown(1)
}

func CallAnonStr(x interface{ Shutdown(string) }) {
	x.Shutdown("x")
}

func CallLocalInt() {
	type localIface interface {
		Ping(int)
	}
	var l localIface
	if l != nil {
		l.Ping(1)
	}
}

func CallLocalStr() {
	type localIface interface {
		Ping(string)
	}
	var l localIface
	if l != nil {
		l.Ping("x")
	}
}
`)

	runGit("add", ".")
	runGit("commit", "-m", "initial commit")

	headSHA := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		tb.Fatalf("vcs.Open failed: %v", err)
	}
	return repo, dir, headSHA
}

func TestReferencesInterfaceMethodCalls(t *testing.T) {
	repo, repoRoot, headSHA := setupInterfaceMethodTestRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()
	ctx := context.Background()

	t.Run("ConcreteToInterfaceCrossPackage", func(t *testing.T) {
		for _, mode := range []string{"immutable", "workingCopy"} {
			t.Run(mode, func(t *testing.T) {
				baseRev := headSHA
				targetRev := headSHA
				if mode == "workingCopy" {
					baseRev = ""
					targetRev = ""
				}
				req := LookupRequest{
					Repo:      repo,
					RepoRoot:  repoRoot,
					BaseRev:   baseRev,
					TargetRev: targetRev,
					Side:      "R",
					FilePath:  "pkg/h3/server.go",
					Line:      7,
					UTF16Col:  24, // 'S' in Shutdown
				}

				res, err := analyzer.References(ctx, req)
				if err != nil {
					t.Fatalf("References failed: %v", err)
				}
				if !res.Found {
					t.Fatalf("expected Shutdown to be found")
				}
				if res.SymbolName != "Shutdown" {
					t.Errorf("SymbolName = %q, want Shutdown", res.SymbolName)
				}

				hasH3Direct := false
				hasServerCall := false
				hasAnonCall := false
				hasGenericCall := false
				hasOtherServer := false
				hasBadSig := false

				for _, r := range res.Refs {
					if r.Path == "pkg/h3/server.go" && r.Line == 12 {
						hasH3Direct = true
					}
					if r.Path == "pkg/server/server.go" && r.Line == 15 {
						hasServerCall = true
					}
					if r.Path == "pkg/server/server.go" && r.Line == 21 {
						hasAnonCall = true
					}
					if r.Path == "pkg/server/server.go" && r.Line == 25 {
						hasGenericCall = true
					}
					if r.Path == "pkg/other/other.go" {
						hasOtherServer = true
					}
					if r.Path == "pkg/server/server.go" && r.Line == 29 {
						hasBadSig = true
					}
				}

				if !hasH3Direct {
					t.Errorf("missing direct call in pkg/h3/server.go:12")
				}
				if !hasServerCall {
					t.Errorf("missing interface method call in pkg/server/server.go:15")
				}
				if !hasAnonCall {
					t.Errorf("missing anonymous interface call in pkg/server/server.go:21")
				}
				if !hasGenericCall {
					t.Errorf("missing generic constraint call in pkg/server/server.go:25")
				}
				if hasOtherServer {
					t.Errorf("unexpected concrete-to-concrete match in pkg/other/other.go")
				}
				if hasBadSig {
					t.Errorf("unexpected match with different signature in pkg/server/server.go:29")
				}
			})
		}
	})

	t.Run("PointerReceiverMultiMethodInterface", func(t *testing.T) {
		req := LookupRequest{
			Repo:      repo,
			RepoRoot:  repoRoot,
			BaseRev:   headSHA,
			TargetRev: headSHA,
			Side:      "R",
			FilePath:  "pkg/service/service.go",
			Line:      5,
			UTF16Col:  17, // 'W' in Work
		}
		res, err := analyzer.References(ctx, req)
		if err != nil {
			t.Fatalf("References failed: %v", err)
		}
		if !res.Found {
			t.Fatalf("expected Work to be found")
		}
		hasInterfaceCall := false
		for _, r := range res.Refs {
			if r.Path == "pkg/service/service.go" && r.Line == 15 {
				hasInterfaceCall = true
				break
			}
		}
		if !hasInterfaceCall {
			t.Errorf("missing Service.Work() call at line 15")
		}
	})

	t.Run("InterfaceTargetToImplementationsAndCallers", func(t *testing.T) {
		req := LookupRequest{
			Repo:      repo,
			RepoRoot:  repoRoot,
			BaseRev:   headSHA,
			TargetRev: headSHA,
			Side:      "R",
			FilePath:  "pkg/server/server.go",
			Line:      6,
			UTF16Col:  2, // 'S' in Shutdown of http3Server
		}
		res, err := analyzer.References(ctx, req)
		if err != nil {
			t.Fatalf("References failed: %v", err)
		}
		if !res.Found {
			t.Fatalf("expected http3Server.Shutdown to be found")
		}

		hasH3Direct := false
		hasServerCall := false
		hasOtherCall := false
		hasAnonCall := false
		hasGenericCall := false
		hasBadSig := false

		for _, r := range res.Refs {
			if r.Path == "pkg/h3/server.go" && r.Line == 12 {
				hasH3Direct = true
			}
			if r.Path == "pkg/server/server.go" && r.Line == 15 {
				hasServerCall = true
			}
			if r.Path == "pkg/other/other.go" && r.Line == 12 {
				hasOtherCall = true
			}
			if r.Path == "pkg/server/server.go" && r.Line == 21 {
				hasAnonCall = true
			}
			if r.Path == "pkg/server/server.go" && r.Line == 25 {
				hasGenericCall = true
			}
			if r.Path == "pkg/server/server.go" && r.Line == 29 {
				hasBadSig = true
			}
		}

		if !hasH3Direct {
			t.Errorf("missing direct call on netHTTPServer at pkg/h3/server.go:12")
		}
		if !hasServerCall {
			t.Errorf("missing interface call at pkg/server/server.go:15")
		}
		if !hasOtherCall {
			t.Errorf("missing otherServer.Shutdown call at pkg/other/other.go:12")
		}
		if !hasAnonCall {
			t.Errorf("missing anonymous interface call at pkg/server/server.go:21")
		}
		if !hasGenericCall {
			t.Errorf("missing generic constraint call at pkg/server/server.go:25")
		}
		if hasBadSig {
			t.Errorf("unexpected match with different signature at pkg/server/server.go:29")
		}
	})

	t.Run("NegativeBuiltinErrorExclusion", func(t *testing.T) {
		req := LookupRequest{
			Repo:      repo,
			RepoRoot:  repoRoot,
			BaseRev:   headSHA,
			TargetRev: headSHA,
			Side:      "R",
			FilePath:  "pkg/errpkg/err.go",
			Line:      5,
			UTF16Col:  17, // 'E' in Error
		}
		res, err := analyzer.References(ctx, req)
		if err != nil {
			t.Fatalf("References failed: %v", err)
		}
		if !res.Found {
			t.Fatalf("expected MyErr.Error to be found")
		}
		hasMyErrCall := false
		hasStdErrCall := false
		for _, r := range res.Refs {
			if r.Path == "pkg/errpkg/err.go" && r.Line == 10 {
				hasMyErrCall = true
			}
			if r.Path == "pkg/errpkg/err.go" && r.Line == 14 {
				hasStdErrCall = true
			}
		}
		if !hasMyErrCall {
			t.Errorf("missing MyErr.Error call at line 10")
		}
		if hasStdErrCall {
			t.Errorf("unexpected match of builtin error.Error at line 14")
		}
	})

	t.Run("UnexportedInterfaceMethod", func(t *testing.T) {
		req := LookupRequest{
			Repo:      repo,
			RepoRoot:  repoRoot,
			BaseRev:   headSHA,
			TargetRev: headSHA,
			Side:      "R",
			FilePath:  "pkg/unexported/unexp.go",
			Line:      9,
			UTF16Col:  17, // 'r' in (w worker) run()
		}
		res, err := analyzer.References(ctx, req)
		if err != nil {
			t.Fatalf("References failed: %v", err)
		}
		if !res.Found {
			t.Fatalf("expected worker.run to be found")
		}
		hasInterfaceCall := false
		for _, r := range res.Refs {
			if r.Path == "pkg/unexported/unexp.go" && r.Line == 19 {
				hasInterfaceCall = true
				break
			}
		}
		if !hasInterfaceCall {
			t.Errorf("missing d.r.run() call at line 19")
		}
	})

	t.Run("AnonymousAndLocalInterfaceDisambiguation", func(t *testing.T) {
		// 1. Anonymous interface call Shutdown(int) should only find CallAnonInt (line 4), not CallAnonStr (line 8)
		reqAnon := LookupRequest{
			Repo:      repo,
			RepoRoot:  repoRoot,
			BaseRev:   headSHA,
			TargetRev: headSHA,
			Side:      "R",
			FilePath:  "pkg/disambig/disambig.go",
			Line:      4,
			UTF16Col:  4, // 'S' in x.Shutdown(1)
		}
		resAnon, err := analyzer.References(ctx, reqAnon)
		if err != nil {
			t.Fatalf("References failed for anon Shutdown(int): %v", err)
		}
		if !resAnon.Found {
			t.Fatalf("expected anon Shutdown(int) to be found")
		}
		hasAnonIntCall := false
		hasAnonStrCall := false
		for _, r := range resAnon.Refs {
			if r.Path == "pkg/disambig/disambig.go" && r.Line == 4 {
				hasAnonIntCall = true
			}
			if r.Path == "pkg/disambig/disambig.go" && r.Line == 8 {
				hasAnonStrCall = true
			}
		}
		if !hasAnonIntCall {
			t.Errorf("missing x.Shutdown(1) call at line 4")
		}
		if hasAnonStrCall {
			t.Errorf("unexpected cross-match with x.Shutdown(\"x\") at line 8")
		}

		// 2. Function-local interface Ping(int) should only find Ping(1) (line 16), not Ping("x") (line 26)
		reqLocal := LookupRequest{
			Repo:      repo,
			RepoRoot:  repoRoot,
			BaseRev:   headSHA,
			TargetRev: headSHA,
			Side:      "R",
			FilePath:  "pkg/disambig/disambig.go",
			Line:      16,
			UTF16Col:  5, // 'P' in l.Ping(1)
		}
		resLocal, err := analyzer.References(ctx, reqLocal)
		if err != nil {
			t.Fatalf("References failed for local Ping(int): %v", err)
		}
		if !resLocal.Found {
			t.Fatalf("expected local Ping(int) to be found")
		}
		hasLocalIntCall := false
		hasLocalStrCall := false
		for _, r := range resLocal.Refs {
			if r.Path == "pkg/disambig/disambig.go" && r.Line == 16 {
				hasLocalIntCall = true
			}
			if r.Path == "pkg/disambig/disambig.go" && r.Line == 26 {
				hasLocalStrCall = true
			}
		}
		if !hasLocalIntCall {
			t.Errorf("missing l.Ping(1) call at line 16")
		}
		if hasLocalStrCall {
			t.Errorf("unexpected cross-match with l.Ping(\"x\") at line 26")
		}
	})

	t.Run("ReferencesBatchParity", func(t *testing.T) {
		req1 := LookupRequest{
			Repo:      repo,
			RepoRoot:  repoRoot,
			BaseRev:   headSHA,
			TargetRev: headSHA,
			Side:      "R",
			FilePath:  "pkg/h3/server.go",
			Line:      7,
			UTF16Col:  24,
		}
		req2 := LookupRequest{
			Repo:      repo,
			RepoRoot:  repoRoot,
			BaseRev:   headSHA,
			TargetRev: headSHA,
			Side:      "R",
			FilePath:  "pkg/server/server.go",
			Line:      6,
			UTF16Col:  2,
		}

		// Fresh analyzers to ensure batch scanning and caching are tested cleanly without stale hits
		analyzerBatch := NewAnalyzer()
		defer analyzerBatch.Close()
		batchRes, err := analyzerBatch.ReferencesBatch(ctx, []LookupRequest{req1, req2})
		if err != nil {
			t.Fatalf("ReferencesBatch failed: %v", err)
		}
		if len(batchRes) != 2 {
			t.Fatalf("got %d batch results, want 2", len(batchRes))
		}

		analyzerSeq := NewAnalyzer()
		defer analyzerSeq.Close()
		seq1, err1 := analyzerSeq.References(ctx, req1)
		if err1 != nil {
			t.Fatalf("References 1 failed: %v", err1)
		}
		seq2, err2 := analyzerSeq.References(ctx, req2)
		if err2 != nil {
			t.Fatalf("References 2 failed: %v", err2)
		}

		if len(batchRes[0].Refs) != len(seq1.Refs) {
			t.Errorf("batchRes[0] refs count %d != sequential %d", len(batchRes[0].Refs), len(seq1.Refs))
		}
		if len(batchRes[1].Refs) != len(seq2.Refs) {
			t.Errorf("batchRes[1] refs count %d != sequential %d", len(batchRes[1].Refs), len(seq2.Refs))
		}
		for i, ref := range batchRes[0].Refs {
			if i < len(seq1.Refs) && (ref.Path != seq1.Refs[i].Path || ref.Line != seq1.Refs[i].Line) {
				t.Errorf("batchRes[0].Refs[%d] (%s:%d) != seq (%s:%d)", i, ref.Path, ref.Line, seq1.Refs[i].Path, seq1.Refs[i].Line)
			}
		}
		for i, ref := range batchRes[1].Refs {
			if i < len(seq2.Refs) && (ref.Path != seq2.Refs[i].Path || ref.Line != seq2.Refs[i].Line) {
				t.Errorf("batchRes[1].Refs[%d] (%s:%d) != seq (%s:%d)", i, ref.Path, ref.Line, seq2.Refs[i].Path, seq2.Refs[i].Line)
			}
		}
	})
}

func TestColdStartAcrossCommitsBlobCache(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	writeFile := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("go.mod", "module example.com/demo\n\ngo 1.22\n")
	writeFile("pkg/iface/worker.go", `package iface

type Worker interface {
	Work() string
}
`)
	writeFile("pkg/impl/worker.go", `package impl

type SimpleWorker struct{}

func (s SimpleWorker) Work() string {
	return "done"
}

func (s SimpleWorker) WorkAgain() string {
	return s.Work()
}
`)
	writeFile("pkg/caller/main.go", `package caller

import (
	"example.com/demo/pkg/iface"
	"example.com/demo/pkg/impl"
)

func CallWork() string {
	var w iface.Worker = impl.SimpleWorker{}
	return w.Work()
}
`)
	for i := 1; i <= 27; i++ {
		writeFile(fmt.Sprintf("pkg/dummy%d/file.go", i), fmt.Sprintf("package dummy%d\n\nfunc F%d() int { return %d }\n", i, i, i))
	}

	runGit("add", ".")
	runGit("commit", "-m", "commit 1")
	c1 := runGit("rev-parse", "HEAD")

	// Commit 2: modify 1 file, add 1 file
	writeFile("pkg/caller/main.go", `package caller

import (
	"example.com/demo/pkg/iface"
	"example.com/demo/pkg/impl"
)

func CallWork() string {
	var w iface.Worker = impl.SimpleWorker{}
	_ = "modified"
	return w.Work()
}
`)
	writeFile("pkg/added/extra.go", "package added\n\nfunc Extra() string { return \"extra\" }\n")

	runGit("add", "-A")
	runGit("commit", "-m", "commit 2")
	c2 := runGit("rev-parse", "HEAD")

	vc := vcs.NewCache()
	defer vc.Close()

	repo, err := vcs.Open(dir, vc)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	analyzer.SetVCSCache(vc)
	defer analyzer.Close()

	ctx := context.Background()

	// Query References on Commit 1
	req1 := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   c1,
		TargetRev: c1,
		Side:      "R",
		FilePath:  "pkg/impl/worker.go",
		Line:      5,
		UTF16Col:  23,
	}
	res1, err := analyzer.References(ctx, req1)
	if err != nil {
		t.Fatalf("References(c1) failed: %v", err)
	}
	if !res1.Found || len(res1.Refs) < 2 {
		t.Fatalf("expected at least 2 refs on c1, got %d (found=%v)", len(res1.Refs), res1.Found)
	}

	// Query Implementations on Commit 1
	implReq1 := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   c1,
		TargetRev: c1,
		Side:      "R",
		FilePath:  "pkg/iface/worker.go",
		Line:      3,
		UTF16Col:  6,
	}
	implRes1, err := analyzer.Implementations(ctx, implReq1)
	if err != nil {
		t.Fatalf("Implementations(c1) failed: %v", err)
	}
	if !implRes1.Found || len(implRes1.Implementations) != 1 {
		t.Fatalf("expected 1 impl on c1, got %d (found=%v)", len(implRes1.Implementations), implRes1.Found)
	}

	// Query References on Commit 2
	req2 := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   c2,
		TargetRev: c2,
		Side:      "R",
		FilePath:  "pkg/impl/worker.go",
		Line:      5,
		UTF16Col:  23,
	}
	res2, err := analyzer.References(ctx, req2)
	if err != nil {
		t.Fatalf("References(c2) failed: %v", err)
	}
	if !res2.Found || len(res2.Refs) < 2 {
		t.Fatalf("expected at least 2 refs on c2, got %d (found=%v)", len(res2.Refs), res2.Found)
	}

	// Query Implementations on Commit 2
	implReq2 := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   c2,
		TargetRev: c2,
		Side:      "R",
		FilePath:  "pkg/iface/worker.go",
		Line:      3,
		UTF16Col:  6,
	}
	implRes2, err := analyzer.Implementations(ctx, implReq2)
	if err != nil {
		t.Fatalf("Implementations(c2) failed: %v", err)
	}
	if !implRes2.Found || len(implRes2.Implementations) != 1 {
		t.Fatalf("expected 1 impl on c2, got %d (found=%v)", len(implRes2.Implementations), implRes2.Found)
	}

	// Verify Commit 2's blobIndexCache hits for all 29 unchanged files!
	blobRepo, ok := repo.(vcs.GoBlobRepo)
	if !ok {
		t.Fatalf("expected GoBlobRepo")
	}
	blobs1, err := blobRepo.ListGoFileBlobs(c1)
	if err != nil {
		t.Fatalf("ListGoFileBlobs(c1) failed: %v", err)
	}
	blobs2, err := blobRepo.ListGoFileBlobs(c2)
	if err != nil {
		t.Fatalf("ListGoFileBlobs(c2) failed: %v", err)
	}

	c1BlobMap := make(map[string]string)
	for _, b := range blobs1 {
		c1BlobMap[b.Path] = b.OID
	}

	unchangedCount := 0
	for _, b := range blobs2 {
		if oid1, ok := c1BlobMap[b.Path]; ok && oid1 == b.OID {
			unchangedCount++
			if _, hit := analyzer.blobCache.get(b.OID); !hit {
				t.Errorf("expected blob cache hit for unchanged file %s (oid %s)", b.Path, b.OID)
			}
		}
	}
	if unchangedCount != 29 {
		t.Fatalf("expected 29 unchanged files between c1 and c2, found %d", unchangedCount)
	}

	t.Run("TransitiveDependencyInvalidation", func(t *testing.T) {
		// Commit 3: modify pkg/impl/worker.go to add another method calling Work(),
		// leaving pkg/caller untouched.
		writeFile("pkg/impl/worker.go", `package impl

type SimpleWorker struct{}

func (s SimpleWorker) Work() string {
	return "done"
}

func (s SimpleWorker) WorkAgain() string {
	return s.Work()
}

func (s SimpleWorker) WorkThird() string {
	return s.Work()
}
`)
		runGit("add", "-A")
		runGit("commit", "-m", "commit 3 - modify impl only")
		c3 := runGit("rev-parse", "HEAD")

		req3 := LookupRequest{
			Repo:      repo,
			RepoRoot:  dir,
			BaseRev:   c3,
			TargetRev: c3,
			Side:      "R",
			FilePath:  "pkg/impl/worker.go",
			Line:      5,
			UTF16Col:  23,
		}
		res3, err := analyzer.References(ctx, req3)
		if err != nil {
			t.Fatalf("References(c3) failed: %v", err)
		}
		if !res3.Found {
			t.Fatalf("expected refs on c3")
		}

		implRefs := 0
		callerRefs := 0
		for _, r := range res3.Refs {
			if strings.HasPrefix(r.Path, "pkg/impl/") {
				implRefs++
			}
			if strings.HasPrefix(r.Path, "pkg/caller/") {
				callerRefs++
			}
		}
		if implRefs < 2 {
			t.Errorf("expected >= 2 refs in pkg/impl on c3, got %d", implRefs)
		}
		if callerRefs < 1 {
			t.Errorf("expected >= 1 ref in pkg/caller on c3, got %d", callerRefs)
		}
	})
}

type mockCountingRepo struct {
	mu           sync.Mutex
	root         string
	contentCalls map[string]int
	revFiles     map[string][]string
	revContent   map[string]map[string]string
	diffFiles    map[string][]*vcs.File
}

func (m *mockCountingRepo) Kind() string                    { return "git" }
func (m *mockCountingRepo) Root() string                    { return m.root }
func (m *mockCountingRepo) User() string                    { return "mock" }
func (m *mockCountingRepo) Changes() ([]*vcs.Change, error) { return nil, nil }
func (m *mockCountingRepo) Commit(rev string) (*vcs.Change, error) {
	return &vcs.Change{Rev: rev, Key: rev}, nil
}
func (m *mockCountingRepo) Files(baseRev, targetRev string) ([]*vcs.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.diffFiles[baseRev+".."+targetRev], nil
}
func (m *mockCountingRepo) Content(rev, path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := rev + ":" + path
	m.contentCalls[key]++
	if revMap, ok := m.revContent[rev]; ok {
		if c, ok := revMap[path]; ok {
			return []byte(c), nil
		}
	}
	return nil, os.ErrNotExist
}
func (m *mockCountingRepo) ListDir(rev, dirPath string) ([]string, error) {
	return nil, nil
}
func (m *mockCountingRepo) ListGoFiles(rev string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revFiles[rev], nil
}
func (m *mockCountingRepo) Pin(refSuffix, rev string) error { return nil }
func (m *mockCountingRepo) ListPins() ([]string, error)     { return nil, nil }
func (m *mockCountingRepo) DeletePin(ref string) error      { return nil }

func TestColdStartAcrossCommitsIncrementalMockRepo(t *testing.T) {
	rev1 := strings.Repeat("1", 40)
	rev2 := strings.Repeat("2", 40)
	rev3 := strings.Repeat("3", 40)
	mockRoot := "/mock/repo"

	rev1Content := make(map[string]string)
	rev2Content := make(map[string]string)
	rev3Content := make(map[string]string)
	var rev1Files []string
	var rev2Files []string
	var rev3Files []string

	rev1Files = append(rev1Files, "pkg/mod.go")
	rev1Content["pkg/mod.go"] = "package pkg\nfunc Mod() int { return 1 }\n"

	rev2Files = append(rev2Files, "pkg/mod.go")
	rev2Content["pkg/mod.go"] = "package pkg\nfunc Mod() int { return 2 }\n"

	rev3Files = append(rev3Files, "pkg/mod.go")
	rev3Content["pkg/mod.go"] = "package pkg\nfunc Mod() int { return 2 }\n"

	for i := 1; i <= 19; i++ {
		p := fmt.Sprintf("pkg/f%d.go", i)
		rev1Files = append(rev1Files, p)
		rev2Files = append(rev2Files, p)
		rev3Files = append(rev3Files, p)
		c := fmt.Sprintf("package pkg\nfunc F%d() int { return %d }\n", i, i)
		rev1Content[p] = c
		rev2Content[p] = c
		rev3Content[p] = c
	}

	diffMap := make(map[string][]*vcs.File)
	diffMap[rev1+".."+rev2] = []*vcs.File{
		{
			Path:   "pkg/mod.go",
			Status: vcs.FileModified,
		},
	}
	// rev2 -> rev3 has 0 Go files changed (only non-Go file README.md modified)
	diffMap[rev2+".."+rev3] = []*vcs.File{
		{
			Path:   "README.md",
			Status: vcs.FileModified,
		},
	}

	mock := &mockCountingRepo{
		root:         mockRoot,
		contentCalls: make(map[string]int),
		revFiles: map[string][]string{
			rev1: rev1Files,
			rev2: rev2Files,
			rev3: rev3Files,
		},
		revContent: map[string]map[string]string{
			rev1: rev1Content,
			rev2: rev2Content,
			rev3: rev3Content,
		},
		diffFiles: diffMap,
	}

	analyzer := NewAnalyzer()
	defer analyzer.Close()

	ctx := context.Background()

	// 1. Index Commit 1
	rc1 := analyzer.getRevCache(mockRoot, rev1)
	if rc1 == nil {
		t.Fatalf("expected non-nil revCache for rev1")
	}
	idx1, err := rc1.getRepoFileIndex(ctx, mock, rev1)
	if err != nil {
		t.Fatalf("getRepoFileIndex(rev1) failed: %v", err)
	}
	if len(idx1.files) != 20 {
		t.Fatalf("idx1 files = %d, want 20", len(idx1.files))
	}

	// 2. Index Commit 2 (modified pkg/mod.go)
	rc2 := analyzer.getRevCache(mockRoot, rev2)
	if rc2 == nil {
		t.Fatalf("expected non-nil revCache for rev2")
	}
	idx2, err := rc2.getRepoFileIndex(ctx, mock, rev2)
	if err != nil {
		t.Fatalf("getRepoFileIndex(rev2) failed: %v", err)
	}
	if len(idx2.files) != 20 {
		t.Fatalf("idx2 files = %d, want 20", len(idx2.files))
	}

	// Assert that Content(rev2, ...) was called for pkg/mod.go, but NEVER for the 19 unchanged files!
	mock.mu.Lock()
	if mock.contentCalls[rev2+":pkg/mod.go"] == 0 {
		t.Errorf("expected Content to be called on modified file pkg/mod.go for rev2")
	}
	for i := 1; i <= 19; i++ {
		p := fmt.Sprintf("pkg/f%d.go", i)
		if count := mock.contentCalls[rev2+":"+p]; count > 0 {
			t.Errorf("Content called %d times for unchanged file %s on rev2", count, p)
		}
	}
	mock.mu.Unlock()

	// 3. Index Commit 3 (modified ONLY README.md - 0 Go files changed)
	rc3 := analyzer.getRevCache(mockRoot, rev3)
	if rc3 == nil {
		t.Fatalf("expected non-nil revCache for rev3")
	}
	idx3, err := rc3.getRepoFileIndex(ctx, mock, rev3)
	if err != nil {
		t.Fatalf("getRepoFileIndex(rev3) failed: %v", err)
	}
	if len(idx3.files) != 20 {
		t.Fatalf("idx3 files = %d, want 20", len(idx3.files))
	}

	// Assert that Content(rev3, ...) was NEVER called for ANY Go file on rev3!
	mock.mu.Lock()
	for _, p := range rev3Files {
		if count := mock.contentCalls[rev3+":"+p]; count > 0 {
			t.Errorf("Content called %d times for file %s on rev3 (which had 0 Go files changed)", count, p)
		}
	}
	mock.mu.Unlock()
}

func TestBackgroundWarmupPrewarmsIndexAndCheckedPkg(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=WarmTester",
			"GIT_AUTHOR_EMAIL=warmtester@example.com",
			"GIT_COMMITTER_NAME=WarmTester",
			"GIT_COMMITTER_EMAIL=warmtester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "WarmTester")
	runGit("config", "user.email", "warmtester@example.com")

	writeFile := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("go.mod", "module example.com/warmdemo\n\ngo 1.22\n")
	writeFile("pkg/hello/hello.go", `package hello

func Greeting() string {
	return "hello world"
}
`)
	writeFile("pkg/hello/hello_test.go", `package hello

import "testing"

func TestGreeting(t *testing.T) {
	_ = Greeting()
}
`)

	runGit("add", ".")
	runGit("commit", "-m", "warmup commit")
	commitSHA := runGit("rev-parse", "HEAD")

	vc := vcs.NewCache()
	defer vc.Close()

	repo, err := vcs.Open(dir, vc)
	if err != nil {
		t.Fatalf("vcs.Open failed: %v", err)
	}

	analyzer := NewAnalyzer()
	analyzer.SetVCSCache(vc)
	analyzer.SetIdleCheck(func() bool { return true })
	analyzer.StartBackgroundWarmup()
	defer analyzer.Close()

	analyzer.EnqueueWarm(WarmRequest{
		RepoRoot: dir,
		Rev:      commitSHA,
		Files:    []string{"pkg/hello/hello.go"},
	})

	deadline := time.Now().Add(5 * time.Second)
	var warmedIdx *repoFileIndex
	var checkedCount int
	for time.Now().Before(deadline) {
		rc := analyzer.getRevCache(dir, commitSHA)
		if rc != nil {
			rc.fileIndexMu.Lock()
			warmedIdx = rc.fileIndex
			rc.fileIndexMu.Unlock()
			if rc.checkedPkgs != nil {
				rc.checkedPkgs.mu.Lock()
				checkedCount = len(rc.checkedPkgs.items)
				rc.checkedPkgs.mu.Unlock()
			}
			if warmedIdx != nil && checkedCount > 0 {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	if warmedIdx == nil {
		t.Fatalf("background warmup failed to populate fileIndex within deadline")
	}
	if checkedCount == 0 {
		t.Fatalf("background warmup failed to populate checkedPkgs within deadline")
	}

	// Verify immediate Lookup succeeds on warm cache
	ctx := context.Background()
	req := LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   commitSHA,
		TargetRev: commitSHA,
		Side:      "R",
		FilePath:  "pkg/hello/hello.go",
		Line:      3,
		UTF16Col:  6,
	}
	res, err := analyzer.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Greeting" {
		t.Fatalf("Lookup on warmed cache returned %+v", res)
	}
}

func TestConstraintHeaderWithCommentPackage(t *testing.T) {
	data := []byte("// This package is ignored.\n//go:build ignore\n\npackage main\n\nfunc ignored() {}\n")
	header := extractConstraintHeader(data)
	if !strings.Contains(header, "//go:build ignore") {
		t.Fatalf("expected header to contain build tag, got: %q", header)
	}

	bctx := &build.Context{
		GOOS:   "linux",
		GOARCH: "amd64",
	}
	e := &repoFileEntry{
		path:             "ignored.go",
		pkgName:          "main",
		constraintHeader: header,
	}
	if matchIndexedGoFile(bctx, "/tmp", e) {
		t.Fatalf("expected matchIndexedGoFile to return false for //go:build ignore")
	}

	validData := []byte("// Package foo does wonderful things.\npackage foo\n\nfunc valid() {}\n")
	validHeader := extractConstraintHeader(validData)
	if validHeader != "" {
		t.Fatalf("expected empty constraint header without build tags, got: %q", validHeader)
	}
	validEntry := &repoFileEntry{
		path:             "foo.go",
		pkgName:          "foo",
		constraintHeader: validHeader,
	}
	if !matchIndexedGoFile(bctx, "/tmp", validEntry) {
		t.Fatalf("expected matchIndexedGoFile to return true for valid file")
	}

	afterPkgData := []byte("package foo\n\nvar s = \"//go:build ignore\"\n")
	if got := extractConstraintHeader(afterPkgData); got != "" {
		t.Fatalf("expected empty constraint header when build directive appears only after package clause, got: %q", got)
	}
}

func TestPositionForEvictedCrossRevPkg(t *testing.T) {
	// Parse dependency package into fsetDep
	fsetDep := token.NewFileSet()
	depSrc := "package dep\n\nfunc TargetFunc() {}\n"
	depAst, err := parser.ParseFile(fsetDep, "/repo/dep/dep.go", depSrc, 0)
	if err != nil {
		t.Fatalf("ParseFile dep failed: %v", err)
	}
	confDep := types.Config{}
	depPkg, err := confDep.Check("repo/dep", fsetDep, []*ast.File{depAst}, nil)
	if err != nil {
		t.Fatalf("Check dep failed: %v", err)
	}
	targetObj := depPkg.Scope().Lookup("TargetFunc")
	if targetObj == nil {
		t.Fatalf("TargetFunc not found in depPkg")
	}
	posBefore := fsetDep.Position(targetObj.Pos())
	if posBefore.Line != 3 || posBefore.Filename != "/repo/dep/dep.go" {
		t.Fatalf("unexpected posBefore: %+v", posBefore)
	}

	// Create a revCache and register depPkg's fset
	rc := newDiskCache()
	rc.registerPkg(depPkg)
	rc.registerPkgFset(depPkg, fsetDep)

	// Build a lookup loader with a different file set (fsetLoader)
	fsetLoader := token.NewFileSet()
	loader := &lookupLoader{
		fset:     fsetLoader,
		revCache: rc,
		// Notice crossRevPkgs is nil (simulating eviction)
		crossRevPkgs: nil,
	}

	// Calling positionFor on targetObj must find the position via revCache.positionForPkg
	posResolved := loader.positionFor(targetObj, nil)
	if posResolved.Line != 3 || posResolved.Filename != "/repo/dep/dep.go" {
		t.Fatalf("positionFor returned wrong position: %+v, want %+v", posResolved, posBefore)
	}
}

func FuzzGoHeaderAndBloomScanners(f *testing.F) {
	seeds := [][]byte{
		[]byte("package p\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"),
		[]byte("\xef\xbb\xbfpackage p\n\nimport \"fmt\"\n\nfunc main() {}\n"),
		[]byte("/* comment */ package p\n"),
		[]byte("package p\nimport `a/b`\n"),
		[]byte("package p\nimport ( _ \"x\"; . \"y\" )\n"),
		[]byte("/* pre */ package /* mid */ p /* post */\nimport /* imp */ \"fmt\"\nfunc f() {}\n"),
		[]byte("package p\r\nimport \"os\"\r\nfunc f() {}\r\n"),
		[]byte("package p\nimport ()\nfunc f() {}\n"),
		[]byte("package p\nimport \"a\"\nimport \"b\"\nfunc f() {}\n"),
		[]byte("package p; import \"fmt\"; func f() {}\n"),
		[]byte("//go:build linux\npackage p\n\nfunc LinuxOnly() {}\n"),
		[]byte("// +build !windows\npackage p\n\nfunc NonWindows() {}\n"),
		[]byte("package p\n\nfunc \u03b1\u03b2() {}\n"),
		[]byte("package p\n\ntype \u03a9 struct {\n\tField int\n}\n"),
		[]byte(""),
		[]byte("package"),
		[]byte("package A0\nimport \"\")0"),
		[]byte("invalid corrupt go file \x00\xff"),
	}

	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 8192 {
			return
		}

		// 1. Zero panic check on arbitrary bytes
		_ = extractConstraintHeader(src)
		hdrImports, exact, pkgName := scanGoHeaderImports(src)
		var entry repoFileEntry
		importSet := make(map[string]bool)
		scanFileBloomAndImports(src, &entry, true, importSet)

		// 2. Differential check #1 against go/parser (Imports)
		fset := token.NewFileSet()
		fParsed, err := parser.ParseFile(fset, "p.go", src, parser.ImportsOnly)
		if err == nil && fParsed != nil {
			var expectedImports []string
			for _, imp := range fParsed.Imports {
				if imp != nil && imp.Path != nil {
					if u, uerr := strconv.Unquote(imp.Path.Value); uerr == nil {
						expectedImports = append(expectedImports, u)
					}
				}
			}

			// scanFileBloomAndImports imports must match 100%
			if !slices.Equal(entry.imports, expectedImports) {
				t.Fatalf("scanFileBloomAndImports mismatch: got %v, want %v\nsrc: %s", entry.imports, expectedImports, string(src))
			}

			// If scanGoHeaderImports indicated exact match, it must match 100%
			if exact {
				if !slices.Equal(hdrImports, expectedImports) {
					t.Fatalf("scanGoHeaderImports exact mismatch: got %v, want %v\nsrc: %s", hdrImports, expectedImports, string(src))
				}
				if fParsed.Name != nil && pkgName != fParsed.Name.Name {
					t.Fatalf("scanGoHeaderImports pkgName mismatch: got %q, want %q\nsrc: %s", pkgName, fParsed.Name.Name, string(src))
				}
			}
		}

		// 3. Differential check #2 (Bloom filter zero false negatives)
		fFull, errFull := parser.ParseFile(fset, "p.go", src, 0)
		if errFull == nil && fFull != nil {
			ast.Inspect(fFull, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					if id.Name != "" && id.Name != "_" && id.Name != "." {
						if !testIdentBloom(&entry.bits, id.Name) {
							t.Fatalf("Bloom filter FALSE NEGATIVE for identifier %q in source:\n%s", id.Name, string(src))
						}
					}
				}
				return true
			})
		}
	})
}

func FuzzUTF16Columns(f *testing.F) {
	seeds := []struct {
		src      string
		line     int
		utf16Col int
	}{
		{"func Add(a, b int) int { return a + b }", 1, 6},
		{"var Rocket = \"🚀 Rocket Ship\"", 1, 14},
		{"var Family = \"👨‍👩‍👧‍👦 Emoji\"", 1, 5},
		{"\t\tx := calc.Add(1, 2)", 1, 13},
		{"func \u03b1\u03b2\u03b3() {}", 1, 6},
		{"line 1\nline 2\nline 3\n", 2, 3},
		{"invalid \xff utf8 byte", 1, 8},
		{"foo" + strings.Repeat("\x80", 20), 1, 10},
		{"foo\x80 foo", 1, 7},
		{"", 0, 0},
		{"short", -5, -10},
		{"short", 100, 1000},
	}

	for _, s := range seeds {
		f.Add(s.src, s.line, s.utf16Col)
	}

	f.Fuzz(func(t *testing.T, src string, line, utf16Col int) {
		if len(src) > 8192 {
			return
		}

		lines := strings.Split(src, "\n")

		// 1. Zero panic check on byteColToUTF16Col
		col16 := byteColToUTF16Col(lines, line, utf16Col+1)
		if col16 < 0 {
			t.Fatalf("byteColToUTF16Col returned negative column: %d", col16)
		}

		// 2. Zero panic check on utf16ColToByteIndex
		var lineStr string
		if line >= 1 && line <= len(lines) {
			lineStr = lines[line-1]
		}
		bIdx := utf16ColToByteIndex(lineStr, utf16Col)
		if bIdx < 0 || bIdx > len(lineStr) {
			t.Fatalf("utf16ColToByteIndex returned out of bounds byte index %d (len=%d)", bIdx, len(lineStr))
		}
		if utf8.ValidString(lineStr) && bIdx < len(lineStr) && !utf8.RuneStart(lineStr[bIdx]) {
			t.Fatalf("utf16ColToByteIndex split a UTF-8 rune: bIdx=%d in %q", bIdx, lineStr)
		}

		// 3. Zero panic check on extractSymbolAtByteIndex
		sym := extractSymbolAtByteIndex(lineStr, bIdx)
		if utf8.ValidString(lineStr) && !utf8.ValidString(sym) {
			t.Fatalf("extractSymbolAtByteIndex produced invalid UTF-8 for valid UTF-8 line: %q in %q", sym, lineStr)
		}
		if strings.HasPrefix(lineStr, "foo") && bIdx >= 3 {
			all80 := true
			for i := 3; i <= bIdx && i < len(lineStr); i++ {
				if lineStr[i] != 0x80 {
					all80 = false
					break
				}
			}
			if all80 && sym == "foo" {
				t.Fatalf("extractSymbolAtByteIndex unexpectedly snapped back to foo across invalid bytes: %q", sym)
			}
		}

		// 4. Roundtrip consistency for valid UTF-8 lines
		if utf8.ValidString(lineStr) && line >= 1 && line <= len(lines) {
			currU16 := 0
			for byteOff, r := range lineStr {
				expectedByte := byteOff
				gotByte := utf16ColToByteIndex(lineStr, currU16)
				if gotByte != expectedByte {
					t.Fatalf("utf16ColToByteIndex mismatch at u16=%d: got byte %d, want %d in %q", currU16, gotByte, expectedByte, lineStr)
				}
				gotU16 := byteColToUTF16Col(lines, line, byteOff+1)
				if gotU16 != currU16 {
					t.Fatalf("byteColToUTF16Col mismatch at byteOff=%d: got u16 %d, want %d in %q", byteOff, gotU16, currU16, lineStr)
				}
				if r >= 0x10000 {
					currU16 += 2
				} else {
					currU16++
				}
			}
		}
	})
}

func TestStripGoFuncBodies(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "basic function",
			src:  "package p\nfunc Foo() int {\n\tx := 1\n\treturn x\n}\n",
		},
		{
			name: "method with receiver",
			src:  "package p\ntype S struct{}\nfunc (s *S) Bar(y string) (string, error) {\n\treturn y, nil\n}\n",
		},
		{
			name: "struct return type",
			src:  "package p\nfunc New() struct{ x int } {\n\treturn struct{ x int }{ x: 1 }\n}\n",
		},
		{
			name: "interface return type",
			src:  "package p\nfunc NewI() interface{ Run() } {\n\treturn nil\n}\n",
		},
		{
			name: "generic function",
			src:  "package p\nfunc Identity[T any](val T) T {\n\treturn val\n}\n",
		},
		{
			name: "bodyless function declaration",
			src:  "package p\nfunc AsmFunc(x int) int\nfunc NextFunc() {}\n",
		},
		{
			name: "multiline comment with newlines in signature",
			src:  "package p\nfunc Multiline(/* comment \n with \n newlines */) int {\n\treturn 42\n}\n",
		},
		{
			name: "line directives preserved unchanged",
			src:  "package p\n//line foo.go:10\nfunc Directed() {\n\tprintln(1)\n}\n",
		},
		{
			name: "utf8 bom",
			src:  "\xef\xbb\xbfpackage p\nfunc WithBOM() {\n\treturn\n}\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := []byte(tc.src)
			stripped := stripGoFuncBodies(orig)

			if bytes.Count(stripped, []byte("\n")) != bytes.Count(orig, []byte("\n")) {
				t.Fatalf("line count mismatch:\norig:\n%s\nstripped:\n%s", orig, stripped)
			}

			fsetOrig := token.NewFileSet()
			fOrig, errOrig := parser.ParseFile(fsetOrig, "src.go", orig, 0)
			if errOrig != nil {
				t.Fatalf("original failed to parse: %v", errOrig)
			}

			fsetStrip := token.NewFileSet()
			fStrip, errStrip := parser.ParseFile(fsetStrip, "src.go", stripped, 0)
			if errStrip != nil {
				t.Fatalf("stripped failed to parse: %v\nstripped code:\n%s", errStrip, stripped)
			}

			conf := types.Config{
				IgnoreFuncBodies: true,
				Error:            func(err error) {},
			}
			pkgOrig, _ := conf.Check("p", fsetOrig, []*ast.File{fOrig}, nil)
			pkgStrip, _ := conf.Check("p", fsetStrip, []*ast.File{fStrip}, nil)

			if pkgOrig != nil && pkgStrip != nil {
				scopeOrig := pkgOrig.Scope()
				scopeStrip := pkgStrip.Scope()
				if !slices.Equal(scopeOrig.Names(), scopeStrip.Names()) {
					t.Fatalf("scope names mismatch: got %v, want %v", scopeStrip.Names(), scopeOrig.Names())
				}
				for _, name := range scopeOrig.Names() {
					objOrig := scopeOrig.Lookup(name)
					objStrip := scopeStrip.Lookup(name)
					if objOrig.Type().String() != objStrip.Type().String() {
						t.Fatalf("type mismatch for %s: got %s, want %s", name, objStrip.Type(), objOrig.Type())
					}
				}
			}
		})
	}
}

func FuzzStripGoFuncBodies(f *testing.F) {
	seeds := [][]byte{
		[]byte("package p\nfunc F() {}\n"),
		[]byte("package p\nfunc F() int { return 1 }\n"),
		[]byte("package p\ntype T struct{}\nfunc (t T) M() {}\n"),
		[]byte("package p\nfunc New() struct{ A int } { return struct{ A int }{ 1 } }\n"),
		[]byte("package p\nfunc NewI() interface{ Foo() } { return nil }\n"),
		[]byte("package p\nfunc Generic[T any](x T) T { return x }\n"),
		[]byte("package p\nfunc Asm(x int) int\nfunc Next() {}\n"),
		[]byte("package p\nfunc Commented() /* block */ { /* body */ }\n"),
		[]byte("\xef\xbb\xbfpackage p\nfunc BOM() {}\n"),
		[]byte("package p\nfunc f() { /* unclosed }"),
		[]byte("package p\nfunc f() { \"\n}\" }"),
		[]byte("package p\nfunc f() { \"\\\n\" }"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 8192 {
			return
		}
		fsetOrig := token.NewFileSet()
		fOrig, err := parser.ParseFile(fsetOrig, "test.go", src, 0)
		if err != nil || fOrig == nil {
			stripped := stripGoFuncBodies(src)
			fsetStrip := token.NewFileSet()
			_, err2 := parser.ParseFile(fsetStrip, "test.go", stripped, 0)
			errStr := err.Error()
			if strings.Contains(errStr, "not terminated") ||
				strings.Contains(errStr, "expected '}', found 'EOF'") ||
				strings.Contains(errStr, "newline in string") ||
				strings.Contains(errStr, "escape sequence") {
				if err2 == nil {
					t.Fatalf("stripped code turned syntax error into valid Go:\nsrc:\n%s\nstripped:\n%s", src, stripped)
				}
			}
			return
		}

		stripped := stripGoFuncBodies(src)
		if bytes.Count(stripped, []byte("\n")) != bytes.Count(src, []byte("\n")) {
			t.Fatalf("newline count altered: orig %d, stripped %d",
				bytes.Count(src, []byte("\n")), bytes.Count(stripped, []byte("\n")))
		}

		fsetStrip := token.NewFileSet()
		fStrip, err := parser.ParseFile(fsetStrip, "test.go", stripped, 0)
		if err != nil {
			t.Fatalf("stripped failed to parse: %v\nsrc:\n%s\nstripped:\n%s", err, src, stripped)
		}

		conf := types.Config{
			IgnoreFuncBodies: true,
			Error:            func(error) {},
		}
		pkgOrig, errOrig := conf.Check("p", fsetOrig, []*ast.File{fOrig}, nil)
		if errOrig == nil && pkgOrig != nil {
			pkgStrip, errStrip := conf.Check("p", fsetStrip, []*ast.File{fStrip}, nil)
			if errStrip != nil || pkgStrip == nil {
				t.Fatalf("stripped failed to typecheck when orig succeeded: %v", errStrip)
			}
			if len(pkgStrip.Scope().Names()) != len(pkgOrig.Scope().Names()) {
				t.Fatalf("scope count mismatch: stripped %d, orig %d",
					len(pkgStrip.Scope().Names()), len(pkgOrig.Scope().Names()))
			}
		}
	})
}

func TestImplementationsCacheIdentity(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("go.mod", "module example.com/testmod\n\ngo 1.22\n")
	writeFile("iface/iface.go", "package iface\n\ntype Reader interface {\n\tRead(p []byte) (n int, err error)\n}\n")
	writeFile("impl/impl.go", "package impl\n\ntype MyReader struct{}\n\nfunc (MyReader) Read(p []byte) (int, error) {\n\treturn 0, nil\n}\n")
	writeFile("callA/a.go", "package calla\n\nimport \"example.com/testmod/iface\"\n\nvar _ iface.Reader\n")
	writeFile("callB/b.go", "package callb\n\nimport \"example.com/testmod/iface\"\n\nvar _ iface.Reader\n")

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")
	runGit("add", ".")
	runGit("commit", "-m", "init")
	head := runGit("rev-parse", "HEAD")

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	a := NewAnalyzer()
	res1, err := a.Implementations(context.Background(), LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   head,
		TargetRev: head,
		Side:      "right",
		FilePath:  "callA/a.go",
		Line:      5,
		UTF16Col:  15,
	})
	if err != nil {
		t.Fatalf("query 1 failed: %v", err)
	}
	if !res1.Found || len(res1.Implementations) == 0 {
		t.Fatalf("query 1 found no implementations: %+v", res1)
	}

	res2, err := a.Implementations(context.Background(), LookupRequest{
		Repo:      repo,
		RepoRoot:  dir,
		BaseRev:   head,
		TargetRev: head,
		Side:      "right",
		FilePath:  "callB/b.go",
		Line:      5,
		UTF16Col:  15,
	})
	if err != nil {
		t.Fatalf("query 2 failed: %v", err)
	}
	if !res2.Found || len(res2.Implementations) == 0 {
		t.Fatalf("query 2 found no implementations: %+v", res2)
	}

	if len(res1.Implementations) != len(res2.Implementations) {
		t.Fatalf("query results differ in count: %d vs %d", len(res1.Implementations), len(res2.Implementations))
	}
	if res1.Implementations[0].Display != res2.Implementations[0].Display {
		t.Fatalf("query results mismatch: %v vs %v", res1.Implementations[0], res2.Implementations[0])
	}
}

func TestGoDocCommentLinkLookup(t *testing.T) {
	repo, repoRoot := setupTestGitRepo(t)
	analyzer := NewAnalyzer()
	defer analyzer.Close()

	ctx := context.Background()

	yamlDir := filepath.Join(repoRoot, "pkg", "yaml.v3")
	if err := os.MkdirAll(yamlDir, 0755); err != nil {
		t.Fatal(err)
	}
	yamlSrc := `package yaml

// Node represents a decoded node.
type Node struct{}

// Decode decodes the node.
func (n *Node) Decode() error { return nil }
`
	if err := os.WriteFile(filepath.Join(yamlDir, "yaml.go"), []byte(yamlSrc), 0644); err != nil {
		t.Fatal(err)
	}

	widgetSrc := `package widget

import (
	aliasjson "encoding/json"
	myfmt "fmt"
	"net/http"
)

var _ = aliasjson.Marshal

// helperFunc does internal work.
func helperFunc() int { return 42 }

type Box[T any] struct {
	Val T
}

func (b *Box[T]) Get() T { return b.Val }

type IntBox = Box[int]

// Widget represents a UI component.
//
// Line 24: See [Widget] or [Widget.Run] or [*Widget.Run].
// Line 25: Also calls [helperFunc].
// Line 26: Formats via [myfmt.Println].
// Line 27: Configured with [*http.Server] or [*http.Server.Shutdown].
// Line 28: Implements [io.Reader] from stdlib without importing io.
// Line 29: Returns [error] builtin.
// Line 30: Package [net/http] and [encoding/json].
// Line 31: Method with import path [net/http.Server.Shutdown].
// Line 32: Dotted pkg [example.com/testmod/pkg/yaml.v3], [example.com/testmod/pkg/yaml.v3.Node], [example.com/testmod/pkg/yaml.v3.Node.Decode].
// Line 33: Generic alias [IntBox.Get] and [IntBox.Val].
// Line 34: [Widget]: closes idle connections.
//
// Negative cases:
// [Docs](https://example.com)
// [Docs]: https://example.com
// xs[idx]
// [*net/http] [*http] [*helperFunc] [http.*Server]
type Widget struct{}

func (w *Widget) Run() error {
	return nil
}

const rawStr = ` + "`" + `// [Widget]` + "`" + `
const fakeURL = "https://[Widget]" // [Widget]
/* a */ var _ = 1 /* [Widget]*/
/*
 * Multi-line [Widget] before closing delimiter.
 */
`

	docSrc := `package widget

// Package docs: see [aliasjson.Marshal].
`

	widgetDir := filepath.Join(repoRoot, "pkg", "widget")
	if err := os.MkdirAll(widgetDir, 0755); err != nil {
		t.Fatal(err)
	}
	widgetPath := filepath.Join(widgetDir, "widget.go")
	if err := os.WriteFile(widgetPath, []byte(widgetSrc), 0644); err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(widgetDir, "doc.go")
	if err := os.WriteFile(docPath, []byte(docSrc), 0644); err != nil {
		t.Fatal(err)
	}

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("add", ".")
	runGit("commit", "-m", "add widget")
	commitRev := runGit("rev-parse", "HEAD")

	findCol := func(lineStr, substr string) int {
		idx := strings.Index(lineStr, substr)
		if idx == -1 {
			t.Fatalf("substr %q not found in line %q", substr, lineStr)
		}
		return utf8.RuneCountInString(lineStr[:idx]) + 1
	}
	findLine := func(lines []string, needle string) int {
		for i, l := range lines {
			if strings.Contains(l, needle) {
				return i + 1
			}
		}
		t.Fatalf("line containing %q not found", needle)
		return 0
	}

	lines := strings.Split(widgetSrc, "\n")
	docLines := strings.Split(docSrc, "\n")

	// 1. Same-package [Widget]
	lWidget := findLine(lines, "Line 24:")
	colWidget := findCol(lines[lWidget-1], "[Widget]") + 1
	res, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lWidget,
		UTF16Col:  colWidget,
	})
	if err != nil {
		t.Fatalf("Lookup [Widget] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Widget" || !res.InRepo {
		t.Fatalf("Lookup [Widget] unexpected result: %+v", res)
	}
	// References on [Widget]
	refRes, err := analyzer.References(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lWidget,
		UTF16Col:  colWidget,
	})
	if err != nil {
		t.Fatalf("References [Widget] failed: %v", err)
	}
	if !refRes.Found || len(refRes.Refs) == 0 {
		t.Fatalf("References [Widget] found no refs: %+v", refRes)
	}

	// 2. Same-package [Widget.Run]
	colWidgetRun := findCol(lines[lWidget-1], "[Widget.Run]") + 8
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lWidget,
		UTF16Col:  colWidgetRun,
	})
	if err != nil {
		t.Fatalf("Lookup [Widget.Run] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Run" || !res.InRepo {
		t.Fatalf("Lookup [Widget.Run] unexpected result: %+v", res)
	}

	// 3. Pointer method [*Widget.Run]
	colStarWidgetRun := findCol(lines[lWidget-1], "[*Widget.Run]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lWidget,
		UTF16Col:  colStarWidgetRun,
	})
	if err != nil {
		t.Fatalf("Lookup [*Widget.Run] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Run" || !res.InRepo {
		t.Fatalf("Lookup [*Widget.Run] unexpected result: %+v", res)
	}

	// 4. Same-package unexported [helperFunc]
	lHelper := findLine(lines, "Line 25:")
	colHelper := findCol(lines[lHelper-1], "[helperFunc]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lHelper,
		UTF16Col:  colHelper,
	})
	if err != nil {
		t.Fatalf("Lookup [helperFunc] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "helperFunc" || !res.InRepo {
		t.Fatalf("Lookup [helperFunc] unexpected result: %+v", res)
	}

	// 5. Aliased import [myfmt.Println]
	lMyfmt := findLine(lines, "Line 26:")
	colMyfmt := findCol(lines[lMyfmt-1], "[myfmt.Println]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lMyfmt,
		UTF16Col:  colMyfmt,
	})
	if err != nil {
		t.Fatalf("Lookup [myfmt.Println] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Println" || res.DefPkg != "fmt" {
		t.Fatalf("Lookup [myfmt.Println] unexpected result: %+v", res)
	}

	// 6. Pointer type [*http.Server] and 3-part imported method [*http.Server.Shutdown]
	lHTTP := findLine(lines, "Line 27:")
	colHTTPServer := findCol(lines[lHTTP-1], "[*http.Server]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lHTTP,
		UTF16Col:  colHTTPServer,
	})
	if err != nil {
		t.Fatalf("Lookup [*http.Server] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Server" || res.DefPkg != "net/http" {
		t.Fatalf("Lookup [*http.Server] unexpected result: %+v", res)
	}

	colShutdown := findCol(lines[lHTTP-1], "[*http.Server.Shutdown]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lHTTP,
		UTF16Col:  colShutdown,
	})
	if err != nil {
		t.Fatalf("Lookup [*http.Server.Shutdown] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Shutdown" || res.DefPkg != "net/http" {
		t.Fatalf("Lookup [*http.Server.Shutdown] unexpected result: %+v", res)
	}

	// 7. Unimported stdlib [io.Reader] + Implementations
	lReader := findLine(lines, "Line 28:")
	colReader := findCol(lines[lReader-1], "[io.Reader]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lReader,
		UTF16Col:  colReader,
	})
	if err != nil {
		t.Fatalf("Lookup [io.Reader] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Reader" || res.DefPkg != "io" {
		t.Fatalf("Lookup [io.Reader] unexpected result: %+v", res)
	}
	if !res.CanImpls {
		t.Fatalf("expected CanImpls=true for [io.Reader]")
	}
	implRes, err := analyzer.Implementations(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lReader,
		UTF16Col:  colReader,
	})
	if err != nil {
		t.Fatalf("Implementations [io.Reader] failed: %v", err)
	}
	if !implRes.Found || len(implRes.Implementations) == 0 {
		t.Fatalf("expected implementations for [io.Reader], got %+v", implRes)
	}

	// 8. Builtin [error]
	lErr := findLine(lines, "Line 29:")
	colError := findCol(lines[lErr-1], "[error]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lErr,
		UTF16Col:  colError,
	})
	if err != nil {
		t.Fatalf("Lookup [error] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "error" || !strings.Contains(res.PkgGoDevURL, "builtin#error") {
		t.Fatalf("Lookup [error] unexpected result: %+v", res)
	}
	if !res.CanImpls {
		t.Fatalf("expected CanImpls=true for [error]")
	}

	// 9. Package doc link [encoding/json]
	lPkg := findLine(lines, "Line 30:")
	colJson := findCol(lines[lPkg-1], "[encoding/json]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lPkg,
		UTF16Col:  colJson,
	})
	if err != nil {
		t.Fatalf("Lookup [encoding/json] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "json" || !strings.Contains(res.PkgGoDevURL, "encoding/json") {
		t.Fatalf("Lookup [encoding/json] unexpected result: %+v", res)
	}

	// 10. Full import path with method [net/http.Server.Shutdown]
	lFull := findLine(lines, "Line 31:")
	colFullMethod := findCol(lines[lFull-1], "[net/http.Server.Shutdown]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lFull,
		UTF16Col:  colFullMethod,
	})
	if err != nil {
		t.Fatalf("Lookup [net/http.Server.Shutdown] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Shutdown" || res.DefPkg != "net/http" {
		t.Fatalf("Lookup [net/http.Server.Shutdown] unexpected result: %+v", res)
	}

	// 11. Dotted import path package, type, and method ([yaml.v3], [yaml.v3.Node], [yaml.v3.Node.Decode])
	lYaml := findLine(lines, "Line 32:")
	for _, tc := range []struct {
		substr  string
		wantSym string
		wantPkg string
	}{
		{"[example.com/testmod/pkg/yaml.v3]", "yaml", "example.com/testmod/pkg/yaml.v3"},
		{"[example.com/testmod/pkg/yaml.v3.Node]", "Node", "example.com/testmod/pkg/yaml.v3"},
		{"[example.com/testmod/pkg/yaml.v3.Node.Decode]", "Decode", "example.com/testmod/pkg/yaml.v3"},
	} {
		col := findCol(lines[lYaml-1], tc.substr) + 1
		res, err = analyzer.Lookup(ctx, LookupRequest{
			Repo:      repo,
			RepoRoot:  repoRoot,
			TargetRev: commitRev,
			FilePath:  "pkg/widget/widget.go",
			Line:      lYaml,
			UTF16Col:  col,
		})
		if err != nil {
			t.Fatalf("Lookup %s failed: %v", tc.substr, err)
		}
		if !res.Found || res.SymbolName != tc.wantSym || res.DefPkg != tc.wantPkg {
			t.Fatalf("Lookup %s unexpected result: %+v", tc.substr, res)
		}
	}

	// 12. Generic type alias method & field ([IntBox.Get], [IntBox.Val])
	lGen := findLine(lines, "Line 33:")
	colGenGet := findCol(lines[lGen-1], "[IntBox.Get]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lGen,
		UTF16Col:  colGenGet,
	})
	if err != nil {
		t.Fatalf("Lookup [IntBox.Get] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Get" {
		t.Fatalf("Lookup [IntBox.Get] unexpected result: %+v", res)
	}
	colGenVal := findCol(lines[lGen-1], "[IntBox.Val]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lGen,
		UTF16Col:  colGenVal,
	})
	if err != nil {
		t.Fatalf("Lookup [IntBox.Val] failed: %v", err)
	}
	if !res.Found || res.SymbolName != "Val" || !strings.Contains(res.Signature, "field (Box).Val") {
		t.Fatalf("Lookup [IntBox.Val] unexpected result: %+v", res)
	}

	// 13. Prose colon after doc link at start of comment line: [Widget]: closes idle connections.
	lColon := findLine(lines, "Line 34:")
	colColon := findCol(lines[lColon-1], "[Widget]") + 1
	res, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lColon,
		UTF16Col:  colColon,
	})
	if err != nil || !res.Found || res.SymbolName != "Widget" {
		t.Fatalf("Lookup [Widget]: prose expected Widget, got %+v err=%v", res, err)
	}

	// 14. Chained lookup inside external file (e.g. net/http server.go)
	resChained, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:          repo,
		RepoRoot:      repoRoot,
		TargetRev:     commitRev,
		PkgImportPath: "net/http",
		PkgFile:       "server.go",
		Line:          29,
		UTF16Col:      15,
	})
	_ = resChained

	// 15. Cached second lookup + sibling-file import alias ([aliasjson.Marshal] in doc.go)
	resCached, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lWidget,
		UTF16Col:  colWidget,
	})
	if err != nil {
		t.Fatalf("Cached lookup failed: %v", err)
	}
	if !resCached.Found || resCached.SymbolName != "Widget" {
		t.Fatalf("Cached lookup unexpected result: %+v", resCached)
	}
	lDocAlias := findLine(docLines, "[aliasjson.Marshal]")
	colDocAlias := findCol(docLines[lDocAlias-1], "[aliasjson.Marshal]") + 1
	resDocAlias, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/doc.go",
		Line:      lDocAlias,
		UTF16Col:  colDocAlias,
	})
	if err != nil || !resDocAlias.Found || resDocAlias.SymbolName != "Marshal" || resDocAlias.DefPkg != "encoding/json" {
		t.Fatalf("Lookup doc.go sibling import alias on warm cache failed: %+v err=%v", resDocAlias, err)
	}

	// 16. Comment bounds: string before //, inline block comment tight closer, multi-line block comment ending later
	lFakeURL := findLine(lines, `const fakeURL`)
	colInString := findCol(lines[lFakeURL-1], `https://[Widget]`) + len(`https://`) + 1
	negStrRes, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lFakeURL,
		UTF16Col:  colInString,
	})
	if err != nil || negStrRes.Found {
		t.Fatalf("expected Found=false inside string literal on fakeURL line, got %+v err=%v", negStrRes, err)
	}
	colAfterFakeURL := strings.LastIndex(lines[lFakeURL-1], "[Widget]")
	colAfterFakeUTF16 := utf8.RuneCountInString(lines[lFakeURL-1][:colAfterFakeURL]) + 2
	resAfterFake, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lFakeURL,
		UTF16Col:  colAfterFakeUTF16,
	})
	if err != nil || !resAfterFake.Found || resAfterFake.SymbolName != "Widget" {
		t.Fatalf("expected // [Widget] after string literal to resolve, got %+v err=%v", resAfterFake, err)
	}

	lTightBlock := findLine(lines, `/* [Widget]*/`)
	colTightBlock := findCol(lines[lTightBlock-1], `[Widget]*/`) + 1
	resTight, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lTightBlock,
		UTF16Col:  colTightBlock,
	})
	if err != nil || !resTight.Found || resTight.SymbolName != "Widget" {
		t.Fatalf("expected /* [Widget]*/ to resolve, got %+v err=%v", resTight, err)
	}

	lMultiBlock := findLine(lines, `Multi-line [Widget]`)
	colMultiBlock := findCol(lines[lMultiBlock-1], `[Widget]`) + 1
	resMulti, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lMultiBlock,
		UTF16Col:  colMultiBlock,
	})
	if err != nil || !resMulti.Found || resMulti.SymbolName != "Widget" {
		t.Fatalf("expected multi-line block comment [Widget] to resolve, got %+v err=%v", resMulti, err)
	}

	// 17. Negative cases:
	// Markdown link [Docs](https://example.com)
	lMdURL := findLine(lines, `[Docs](https://example.com)`)
	colMdUrl := findCol(lines[lMdURL-1], "[Docs]") + 1
	negRes, err := analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lMdURL,
		UTF16Col:  colMdUrl,
	})
	if err != nil || negRes.Found {
		t.Fatalf("expected Found=false for markdown link, got Found=%v err=%v", negRes.Found, err)
	}

	// Markdown link definition line [Docs]: https://example.com
	lMdDef := findLine(lines, `[Docs]: https://example.com`)
	colMdDef := findCol(lines[lMdDef-1], "[Docs]") + 1
	negRes, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lMdDef,
		UTF16Col:  colMdDef,
	})
	if err != nil || negRes.Found {
		t.Fatalf("expected Found=false for markdown def, got Found=%v (%+v) err=%v", negRes.Found, negRes, err)
	}

	// Slice index xs[idx]
	lSlice := findLine(lines, `xs[idx]`)
	colIdx := findCol(lines[lSlice-1], "[idx]") + 1
	negRes, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lSlice,
		UTF16Col:  colIdx,
	})
	if err != nil || negRes.Found {
		t.Fatalf("expected Found=false for slice index, got Found=%v err=%v", negRes.Found, err)
	}

	// Disallowed pointer targets: [*net/http] [*http] [*helperFunc] [http.*Server]
	lBadPtr := findLine(lines, `[*net/http]`)
	for _, bad := range []string{"[*net/http]", "[*http]", "[*helperFunc]", "[http.*Server]"} {
		colBad := findCol(lines[lBadPtr-1], bad) + 1
		negRes, err = analyzer.Lookup(ctx, LookupRequest{
			Repo:      repo,
			RepoRoot:  repoRoot,
			TargetRev: commitRev,
			FilePath:  "pkg/widget/widget.go",
			Line:      lBadPtr,
			UTF16Col:  colBad,
		})
		if err != nil || negRes.Found {
			t.Fatalf("expected Found=false for %s, got Found=%v (%+v) err=%v", bad, negRes.Found, negRes, err)
		}
	}

	// Raw string literal `// [Widget]`
	lRaw := findLine(lines, `const rawStr`)
	colRaw := findCol(lines[lRaw-1], "[Widget]") + 1
	negRes, err = analyzer.Lookup(ctx, LookupRequest{
		Repo:      repo,
		RepoRoot:  repoRoot,
		TargetRev: commitRev,
		FilePath:  "pkg/widget/widget.go",
		Line:      lRaw,
		UTF16Col:  colRaw,
	})
	if err != nil || negRes.Found {
		t.Fatalf("expected Found=false for doc link inside raw string literal, got Found=%v err=%v", negRes.Found, err)
	}
}

func FuzzGoDocLinkExtraction(f *testing.F) {
	seeds := []string{
		"// [Server]",
		"// [*Server.Shutdown]",
		"// [io.Reader]",
		"// [net/http.Server.Shutdown]",
		"// [encoding/json]",
		"// [gopkg.in/yaml.v3.Node]",
		"/* see [Reader] */",
		"// [text](https://example.com)",
		"// [ref]: https://example.com",
		"// [Server]: closes connections",
		"// a[i] and s[0][j]",
		"// [README.md]",
		"// [TODO]",
	}
	for _, s := range seeds {
		f.Add(s, 5)
	}

	f.Fuzz(func(t *testing.T, lineStr string, byteIdx int) {
		lines := []string{lineStr}
		bodyStart, bodyEnd, ok := findCommentBodyAtByteIndex(lines, 1, byteIdx)
		if !ok {
			return
		}
		link, ok := diff.ExtractGoDocLinkInRange(lineStr, bodyStart, bodyEnd, byteIdx)
		if ok {
			if len(link) == 0 || len(link) > 256 {
				t.Fatalf("extracted invalid link length %d: %q", len(link), link)
			}
			if !diff.IsValidGoDocLinkTarget(link) {
				t.Fatalf("extracted link is not valid doc link target: %q", link)
			}
		}
		_ = hasPotentialDocLinkAtByteIndex(lineStr, byteIdx)
	})
}

func TestFormatLineHTMLPreservesLongLines(t *testing.T) {
	// 1. Lines longer than the old 240-rune limit up to 2048 runes must be preserved in full.
	pad350 := strings.Repeat("x", 320)
	line350 := fmt.Sprintf("\tcall(%q, END_MARKER_350)", pad350)
	got350 := formatLineHTML([]string{line350}, 1)
	if strings.Contains(got350, "...") {
		t.Errorf("expected 350+ rune line not to be truncated with ..., got: %s", got350)
	}
	if !strings.Contains(got350, "END_MARKER_350") {
		t.Errorf("expected 350+ rune line to preserve tail identifier END_MARKER_350, got: %s", got350)
	}

	// 2. Exact 2048-rune boundary after leading whitespace trim must not be truncated.
	prefix := "val := "
	tail := "END2048"
	exact2048 := prefix + strings.Repeat("a", 2048-len(prefix)-len(tail)) + tail
	if utf8.RuneCountInString(exact2048) != 2048 {
		t.Fatalf("bad test string length: %d", utf8.RuneCountInString(exact2048))
	}
	got2048 := formatLineHTML([]string{"\t" + exact2048}, 1)
	if strings.Contains(got2048, "...") {
		t.Errorf("expected exact 2048-rune line not to be truncated with ..., got tail %q", got2048[max(0, len(got2048)-40):])
	}
	if !strings.Contains(got2048, tail) {
		t.Errorf("expected exact 2048-rune line to preserve %q", tail)
	}

	// 3. Lines longer than 2048 runes (including multi-byte runes) truncate at 2048 runes with "...".
	multiPrefix := "// " + strings.Repeat("π", 2044)
	overLine := multiPrefix + "12345_DROPPED_TAIL"
	gotOver := formatLineHTML([]string{overLine}, 1)
	if !utf8.ValidString(gotOver) {
		t.Fatalf("expected valid UTF-8 output for truncated multi-byte line")
	}
	if !strings.HasSuffix(gotOver, "...</span>") && !strings.Contains(gotOver, "...") {
		t.Errorf("expected truncated >2048-rune line to contain ..., got tail %q", gotOver[max(0, len(gotOver)-40):])
	}
	if strings.Contains(gotOver, "DROPPED_TAIL") {
		t.Errorf("expected >2048-rune tail to be truncated, got: %s", gotOver[max(0, len(gotOver)-60):])
	}
}

func TestIdenticalTypeAcrossPackagesTupleAndUnion(t *testing.T) {
	pkgA := types.NewPackage("example.com/shared", "shared")
	pkgB := types.NewPackage("example.com/shared", "shared")
	pkgOther := types.NewPackage("example.com/other", "other")

	nameA := types.NewTypeName(token.NoPos, pkgA, "Item", nil)
	namedA := types.NewNamed(nameA, types.Typ[types.Int], nil)
	nameB := types.NewTypeName(token.NoPos, pkgB, "Item", nil)
	namedB := types.NewNamed(nameB, types.Typ[types.Int], nil)
	nameOther := types.NewTypeName(token.NoPos, pkgOther, "Item", nil)
	namedOther := types.NewNamed(nameOther, types.Typ[types.Int], nil)

	tupA := types.NewTuple(types.NewVar(token.NoPos, pkgA, "v", namedA), types.NewVar(token.NoPos, pkgA, "ok", types.Typ[types.Bool]))
	tupB := types.NewTuple(types.NewVar(token.NoPos, pkgB, "v", namedB), types.NewVar(token.NoPos, pkgB, "ok", types.Typ[types.Bool]))
	tupOtherPkg := types.NewTuple(types.NewVar(token.NoPos, pkgOther, "v", namedOther), types.NewVar(token.NoPos, pkgOther, "ok", types.Typ[types.Bool]))
	tupDiffLen := types.NewTuple(types.NewVar(token.NoPos, pkgB, "v", namedB))

	if !identicalTypeAcrossPackages(tupA, tupB, nil) {
		t.Fatalf("expected equivalent cross-package *types.Tuple to match")
	}
	if identicalTypeAcrossPackages(tupA, tupOtherPkg, nil) {
		t.Fatalf("expected *types.Tuple from different package paths not to match")
	}
	if identicalTypeAcrossPackages(tupA, tupDiffLen, nil) {
		t.Fatalf("expected *types.Tuple of different lengths not to match")
	}

	unionA := types.NewUnion([]*types.Term{types.NewTerm(true, namedA), types.NewTerm(false, types.Typ[types.String])})
	unionB := types.NewUnion([]*types.Term{types.NewTerm(true, namedB), types.NewTerm(false, types.Typ[types.String])})
	unionDiffTilde := types.NewUnion([]*types.Term{types.NewTerm(false, namedB), types.NewTerm(false, types.Typ[types.String])})

	if !identicalTypeAcrossPackages(unionA, unionB, nil) {
		t.Fatalf("expected equivalent cross-package *types.Union to match")
	}
	if identicalTypeAcrossPackages(unionA, unionDiffTilde, nil) {
		t.Fatalf("expected *types.Union with differing tilde terms not to match")
	}
	if identicalTypeAcrossPackages(tupA, unionA, nil) {
		t.Fatalf("expected *types.Tuple and *types.Union not to match")
	}
}

func TestStripGoFuncBodiesScannerEdgesAndFallback(t *testing.T) {
	t.Run("raw strings comments forward decls constraints and top vars", func(t *testing.T) {
		src := []byte("\xef\xbb\xbfpackage p\n" +
			"type S struct{ A int }\n" +
			"func Asm(x int) int\n" +
			"var TopLit = S{A: 1}\n" +
			"func F[T interface{ Foo() }, U struct{ B int }](x T) struct{ Y int } {\n" +
			"\t_ = `unbalanced { braces \\\" and quotes`\n" +
			"\t/* comment with } and func */\n" +
			"\treturn struct{ Y int }{Y: 2}\n" +
			"}\n" +
			"func G() interface{ Do() } { return nil }\n" +
			"var TopFn = func() int { return KeepBodyIdent }\n")

		stripped := stripGoFuncBodies(src)
		if bytes.Count(stripped, []byte("\n")) != bytes.Count(src, []byte("\n")) {
			t.Fatalf("newline count changed: got %d, want %d",
				bytes.Count(stripped, []byte("\n")), bytes.Count(src, []byte("\n")))
		}
		s := string(stripped)
		if !strings.Contains(s, "var TopLit = S{A: 1}") {
			t.Fatalf("expected top-level composite literal to remain intact:\n%s", s)
		}
		if !strings.Contains(s, "KeepBodyIdent") {
			t.Fatalf("expected top-level var closure initializer not to be stripped:\n%s", s)
		}
		if strings.Contains(s, "unbalanced { braces") {
			t.Fatalf("expected function F body to be stripped:\n%s", s)
		}
		if !strings.Contains(s, "interface{ Do() }") || !strings.Contains(s, "struct{ Y int }") {
			t.Fatalf("expected inline struct/interface return types in signatures to remain intact:\n%s", s)
		}

		fset := token.NewFileSet()
		f, err := parseGoFile(fset, "edge.go", src, parser.SkipObjectResolution, true)
		if err != nil || f == nil {
			t.Fatalf("parseGoFile failed: %v", err)
		}
	})

	t.Run("parseGoFile falls back to unstripped on parse error", func(t *testing.T) {
		src := []byte("package p\nfunc F() { a := 1 }\nvar x = (\n")
		fset := token.NewFileSet()
		f, err := parseGoFile(fset, "broken.go", src, 0, true)
		if err == nil || f == nil {
			t.Fatalf("expected syntax error with partial AST on broken source, got f=%v err=%v", f, err)
		}
		fileCount := 0
		fset.Iterate(func(tf *token.File) bool {
			fileCount++
			return true
		})
		if fileCount != 1 {
			t.Fatalf("expected exactly 1 token.File in fset after fallback, got %d", fileCount)
		}
		fd := f.Decls[0].(*ast.FuncDecl)
		if len(fd.Body.List) != 1 {
			t.Fatalf("expected unstripped fallback AST body with 1 stmt, got %d", len(fd.Body.List))
		}

		// When IgnoreFuncBodies is false, parseGoFile keeps bodies intact.
		withBody := []byte("package p\nfunc Keep() int { return 42 }\n")
		fsetKeep := token.NewFileSet()
		fKeep, err := parseGoFile(fsetKeep, "keep.go", withBody, 0, false)
		if err != nil || fKeep == nil {
			t.Fatalf("parseGoFile(ignoreFuncBodies=false) failed: %v", err)
		}
		fdKeep := fKeep.Decls[0].(*ast.FuncDecl)
		if len(fdKeep.Body.List) != 1 {
			t.Fatalf("expected unstripped body with 1 stmt, got %d", len(fdKeep.Body.List))
		}
	})
}
