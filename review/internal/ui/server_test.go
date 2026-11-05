package ui

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/engine"
	"nicholas.sh/cmd/review/internal/vcs"
)

func setupTestServer(t *testing.T) (*Server, *httptest.Server, *db.DB, string) {
	repoDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.name", "UI Tester")
	run("config", "user.email", "ui@example.com")

	if err := os.WriteFile(filepath.Join(repoDir, "hello.go"), []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "hello.go")
	run("commit", "-m", "First commit\n\nChange-Id: Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	return srv, ts, d, repoDir
}

func TestServerEndpoints(t *testing.T) {
	srv, ts, d, _ := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	// 1. /healthz
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz failed: code=%d err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	// 2. Static CSS & JS syntax check
	resp, err = http.Get(ts.URL + "/static/style.css")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/static/style.css failed: code=%d err=%v", resp.StatusCode, err)
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("expected immutable cache-control, got: %s", resp.Header.Get("Cache-Control"))
	}
	_ = resp.Body.Close()

	if nodeBin, err := exec.LookPath("node"); err == nil {
		cmd := exec.Command(nodeBin, "--check", filepath.Join("static", "app.js"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("static/app.js failed node --check: %v\n%s", err, string(out))
		}
	}

	// 3. Dashboard GET /
	resp, err = http.Get(ts.URL + "/")
	if err != nil || resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		if resp != nil {
			_, _ = buf.ReadFrom(resp.Body)
		}
		t.Fatalf("GET / failed: code=%d err=%v body=%s", resp.StatusCode, err, buf.String())
	}
	_ = resp.Body.Close()

	// 4. API Notify
	resp, err = http.Post(ts.URL+"/api/notify", "application/json", strings.NewReader(`{}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/notify failed: code=%d err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
}

func TestSSELiveEvents(t *testing.T) {
	srv, ts, d, _ := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	// Connect SSE client
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)

	// Read initial connected event
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "connected") {
		t.Fatalf("expected connected event, got: %s (err=%v)", line, err)
	}

	// Trigger broadcast
	go func() {
		time.Sleep(100 * time.Millisecond)
		srv.broker.Broadcast("reload", map[string]string{"reason": "test"})
	}()

	// Read reload event
	foundReload := false
	for i := 0; i < 5; i++ {
		line, err = reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.Contains(line, "event: reload") {
			foundReload = true
			break
		}
	}
	if !foundReload {
		t.Errorf("expected to receive reload event over SSE")
	}
}

func TestAPIWorkflow(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)

	// Add thread via API
	payload := map[string]any{
		"repo":       rec.Slug,
		"change":     "HEAD",
		"path":       "hello.go",
		"side":       "R",
		"start_line": 3,
		"end_line":   3,
		"author":     "alice",
		"body":       "Please add docs",
	}
	pBytes, _ := json.Marshal(payload)
	resp, err := http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(pBytes))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add failed: code=%d err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	// Publish drafts
	pubPayload, _ := json.Marshal(map[string]any{"repo": rec.Slug, "change": ""})
	resp, err = http.Post(ts.URL+"/api/publish", "application/json", bytes.NewReader(pubPayload))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/publish failed: code=%d err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	// Verify drafts were published
	sums, _ := d.RepoSummaries(rec.ID)
	for _, sum := range sums {
		if sum.DraftCount != 0 {
			t.Errorf("expected 0 drafts after publish, got %d", sum.DraftCount)
		}
	}
}

func TestServerUnifiedDiff(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)

	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	if len(changes) == 0 {
		t.Fatalf("no changes")
	}
	c := changes[0]

	// Request diff with ?view=unified
	url := fmt.Sprintf("%s/r/%s/c/%s/f/hello.go?view=unified", ts.URL, rec.Slug, c.Rev)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET unified diff failed: code=%d err=%v", resp.StatusCode, err)
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	bodyStr := buf.String()

	if !strings.Contains(bodyStr, "line-code-unified") {
		t.Errorf("expected unified diff table row with 'line-code-unified', got:\n%s", bodyStr)
	}
}

func TestServerDraftEdit(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)

	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// Add thread
	payload := map[string]any{
		"repo":       rec.Slug,
		"change":     c.Rev,
		"rev":        c.Rev,
		"path":       "hello.go",
		"side":       "R",
		"start_line": 3,
		"end_line":   3,
		"author":     "alice",
		"body":       "Initial comment body",
	}
	pBytes, _ := json.Marshal(payload)
	resp, err := http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(pBytes))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add failed: code=%d err=%v", resp.StatusCode, err)
	}
	var addRes struct {
		Thread  *db.Thread  `json:"thread"`
		Message *db.Message `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&addRes); err != nil {
		t.Fatalf("decoding add response: %v", err)
	}
	_ = resp.Body.Close()

	if addRes.Message == nil || addRes.Message.ID == 0 {
		t.Fatalf("invalid add response: %+v", addRes)
	}

	// Edit draft message
	editPayload := map[string]any{
		"message_id": addRes.Message.ID,
		"body":       "Revised draft body text",
	}
	eBytes, _ := json.Marshal(editPayload)
	resp, err = http.Post(ts.URL+"/api/threads/edit", "application/json", bytes.NewReader(eBytes))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/edit failed: code=%d err=%v", resp.StatusCode, err)
	}
	var editRes struct {
		Message *db.Message `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&editRes); err != nil {
		t.Fatalf("decoding edit response: %v", err)
	}
	_ = resp.Body.Close()

	if editRes.Message.Body != "Revised draft body text" {
		t.Errorf("expected revised body, got: %q", editRes.Message.Body)
	}
}

func TestServerChangeReviewedBatchLGTM(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)

	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// Call /api/changes/reviewed with lgtm: true
	lgtm := true
	revPayload := map[string]any{
		"repo":   rec.Slug,
		"change": c.Rev,
		"rev":    c.Rev,
		"lgtm":   &lgtm,
	}
	rBytes, _ := json.Marshal(revPayload)
	resp, err := http.Post(ts.URL+"/api/changes/reviewed", "application/json", bytes.NewReader(rBytes))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/changes/reviewed failed: code=%d err=%v", resp.StatusCode, err)
	}
	var resObj struct {
		Status   string `json:"status"`
		Sig      string `json:"sig"`
		Reviewed bool   `json:"reviewed"`
		LGTM     bool   `json:"lgtm"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resObj); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	_ = resp.Body.Close()
	if resObj.Status != "ok" || !resObj.LGTM || !resObj.Reviewed || resObj.Sig == "" {
		t.Errorf("unexpected API response: %+v", resObj)
	}

	// Verify revision mark in DB
	reviewed, isLGTM, err := d.RevMark(rec.ID, c.Key, c.Rev)
	if err != nil {
		t.Fatalf("RevMark failed: %v", err)
	}
	if !reviewed || !isLGTM {
		t.Errorf("expected reviewed=true lgtm=true, got reviewed=%v lgtm=%v", reviewed, isLGTM)
	}

	// Verify batch file mark in DB
	marks, err := d.FileMarks(rec.ID, c.Key, c.Rev)
	if err != nil {
		t.Fatalf("FileMarks failed: %v", err)
	}
	if !marks["hello.go"] {
		t.Errorf("expected hello.go to be marked reviewed after LGTM batch: %+v", marks)
	}
}

func TestServerDiffCellClasses(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)

	// Make an edit to hello.go in repoDir to have both adds and dels
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "hello.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "hello.go")
	run("commit", "-m", "Second commit\n\nChange-Id: Ibbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	if len(changes) == 0 {
		t.Fatalf("no changes")
	}
	c := changes[0] // second commit

	// 1. Split diff
	url := fmt.Sprintf("%s/r/%s/c/%s/f/hello.go?mode=split", ts.URL, rec.Slug, c.Rev)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET split diff failed: code=%d err=%v", resp.StatusCode, err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	if !strings.Contains(body, "cell-add") {
		t.Errorf("expected cell-add in split diff HTML, got:\n%s", body)
	}
	if !strings.Contains(body, "cell-add-num") {
		t.Errorf("expected cell-add-num in split diff HTML")
	}
	if !strings.Contains(body, "cell-empty") {
		t.Errorf("expected cell-empty in split diff HTML")
	}

	// 2. Unified diff
	url = fmt.Sprintf("%s/r/%s/c/%s/f/hello.go?mode=unified", ts.URL, rec.Slug, c.Rev)
	resp, err = http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET unified diff failed: code=%d err=%v", resp.StatusCode, err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body = buf.String()

	if !strings.Contains(body, "cell-add") {
		t.Errorf("expected cell-add in unified diff HTML, got:\n%s", body)
	}

	// Verify that import "fmt" is tagged with tok-imp and normal string "hi" is plain tok-str
	if !strings.Contains(body, `class="tok-str tok-imp"`) {
		t.Errorf("expected class=\"tok-str tok-imp\" on import string in diff HTML")
	}
	if !strings.Contains(body, `<span class="tok-str">&#34;hi&#34;</span>`) && !strings.Contains(body, `<span class="tok-str">"hi"</span>`) {
		t.Errorf("expected plain tok-str for string \"hi\" outside import in diff HTML, got:\n%s", body)
	}
}

func TestServerChangePageAllDiffs(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)

	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// 1. DiffsMode = all
	url := fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, c.Rev)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET change diffs=all failed: code=%d err=%v", resp.StatusCode, err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	if !strings.Contains(body, "commit-msg-card") {
		t.Errorf("expected commit-msg-card in change page")
	}
	if !strings.Contains(body, "relation-chain-card") {
		t.Errorf("expected relation-chain-card in change page")
	}
	if !strings.Contains(body, "diff-file-section") {
		t.Errorf("expected diff-file-section in diffs=all mode")
	}
	if strings.Contains(body, "<span>Files (") {
		t.Errorf("did not expect files summary card in diffs=all mode")
	}

	// 2. DiffsMode = list
	url = fmt.Sprintf("%s/r/%s/c/%s?diffs=list", ts.URL, rec.Slug, c.Rev)
	resp, err = http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET change diffs=list failed: code=%d err=%v", resp.StatusCode, err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body = buf.String()

	if strings.Contains(body, "diff-file-section") {
		t.Errorf("did not expect diff-file-section in diffs=list mode")
	}
	if !strings.Contains(body, "<span>Files (") {
		t.Errorf("expected files summary card in diffs=list mode")
	}
}

func TestServerMarkdownInThreads(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)

	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// Add thread with markdown body
	payload := map[string]any{
		"repo":       rec.Slug,
		"change":     c.Rev,
		"rev":        c.Rev,
		"path":       "hello.go",
		"side":       "R",
		"start_line": 3,
		"end_line":   3,
		"author":     "bob",
		"body":       "Please check `fmt.Sprintf` and **bold note**",
	}
	pBytes, _ := json.Marshal(payload)
	resp, err := http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(pBytes))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add failed: code=%d err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	// Inspect rendered diff
	url := fmt.Sprintf("%s/r/%s/c/%s/f/hello.go", ts.URL, rec.Slug, c.Rev)
	resp, err = http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET diff failed: code=%d err=%v", resp.StatusCode, err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	if !strings.Contains(body, "<code class=\"md-inline-code\">fmt.Sprintf</code>") {
		t.Errorf("expected rendered inline code in markdown comment, got:\n%s", body)
	}
	if !strings.Contains(body, "<strong>bold note</strong>") {
		t.Errorf("expected rendered strong tag in markdown comment, got:\n%s", body)
	}
}

func TestServerAPIDiffExpand(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)
	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// 1. Split mode test
	url := fmt.Sprintf("%s/api/diff/expand?repo=%s&left_rev=%s&right_rev=%s&path=hello.go&left_start=1&left_end=3&right_start=1&right_end=3&mode=split",
		ts.URL, rec.Slug, c.Parent, c.Rev)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/diff/expand split failed: code=%d err=%v", resp.StatusCode, err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	if !strings.Contains(body, `class="diff-row row-equal"`) {
		t.Errorf("expected diff-row row-equal in expand output, got:\n%s", body)
	}
	if !strings.Contains(body, `data-line-l="1"`) || !strings.Contains(body, `data-line-r="1"`) {
		t.Errorf("expected line 1 in expand output, got:\n%s", body)
	}
	if !strings.Contains(body, `data-side="L"`) || !strings.Contains(body, `data-side="R"`) {
		t.Errorf("expected split data-side in output, got:\n%s", body)
	}
	if !strings.Contains(body, `openCommentForm`) {
		t.Errorf("expected comment triggers in output, got:\n%s", body)
	}

	// 2. Unified mode test
	url = fmt.Sprintf("%s/api/diff/expand?repo=%s&left_rev=%s&right_rev=%s&path=hello.go&left_start=1&left_end=3&right_start=1&right_end=3&mode=unified",
		ts.URL, rec.Slug, c.Parent, c.Rev)
	resp, err = http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/diff/expand unified failed: code=%d err=%v", resp.StatusCode, err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body = buf.String()

	if !strings.Contains(body, `line-code-unified`) {
		t.Errorf("expected line-code-unified in unified output, got:\n%s", body)
	}

	// 3. Not found repo
	resp, err = http.Get(ts.URL + "/api/diff/expand?repo=nonexistent&left_start=1&left_end=3")
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent repo, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestServerResolvedUnresolvedThreads(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)
	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// 1. Add unresolved thread
	pUnresolved, _ := json.Marshal(map[string]any{
		"repo":       rec.Slug,
		"change":     c.Rev,
		"rev":        c.Rev,
		"path":       "hello.go",
		"side":       "R",
		"start_line": 3,
		"end_line":   3,
		"author":     "alice",
		"body":       "Please fix this unresolved issue",
		"resolved":   false,
	})
	resp, err := http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(pUnresolved))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add (unresolved) failed: %v", err)
	}
	var res1 struct {
		Thread  *db.Thread  `json:"thread"`
		Message *db.Message `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res1); err != nil {
		t.Fatalf("decoding thread 1: %v", err)
	}
	_ = resp.Body.Close()
	if res1.Thread == nil || res1.Thread.Resolved {
		t.Errorf("expected thread 1 to be unresolved, got: %+v", res1.Thread)
	}

	// 2. Add resolved thread
	pResolved, _ := json.Marshal(map[string]any{
		"repo":       rec.Slug,
		"change":     c.Rev,
		"rev":        c.Rev,
		"path":       "hello.go",
		"side":       "R",
		"start_line": 1,
		"end_line":   1,
		"author":     "bob",
		"body":       "Already good here",
		"resolved":   true,
	})
	resp, err = http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(pResolved))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add (resolved) failed: %v", err)
	}
	var res2 struct {
		Thread  *db.Thread  `json:"thread"`
		Message *db.Message `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res2); err != nil {
		t.Fatalf("decoding thread 2: %v", err)
	}
	_ = resp.Body.Close()
	if res2.Thread == nil || !res2.Thread.Resolved {
		t.Errorf("expected thread 2 to be resolved, got: %+v", res2.Thread)
	}

	// 3. Inspect rendered diff HTML
	url := fmt.Sprintf("%s/r/%s/c/%s/f/hello.go", ts.URL, rec.Slug, c.Rev)
	resp, err = http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET diff failed: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	// Unresolved thread card must have thread-unresolved class
	needle1 := fmt.Sprintf(`id="thread-%d" class="thread-card thread-unresolved`, res1.Thread.ID)
	if !strings.Contains(body, needle1) {
		t.Errorf("expected unresolved thread card to have thread-unresolved class, needle: %s", needle1)
	}

	// Resolved thread card must NOT have thread-unresolved class
	needle2 := fmt.Sprintf(`id="thread-%d"`, res2.Thread.ID)
	idx := strings.Index(body, needle2)
	if idx == -1 {
		t.Errorf("thread 2 not found in diff body")
	} else {
		cardSnippet := body[idx : idx+80]
		if strings.Contains(cardSnippet, "thread-unresolved") {
			t.Errorf("expected resolved thread to NOT have thread-unresolved: %s", cardSnippet)
		}
	}

	// 4. Reply to thread 1 with resolved: true
	pReply, _ := json.Marshal(map[string]any{
		"thread_id": res1.Thread.ID,
		"author":    "charlie",
		"body":      "Done, resolved now",
		"resolved":  true,
	})
	resp, err = http.Post(ts.URL+"/api/threads/reply", "application/json", bytes.NewReader(pReply))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/reply failed: %v", err)
	}
	_ = resp.Body.Close()

	// Verify in DB that thread 1 is now resolved
	th1, err := d.ThreadByID(res1.Thread.ID)
	if err != nil || !th1.Resolved {
		t.Errorf("expected thread 1 to be resolved after reply, got resolved=%v (err=%v)", th1.Resolved, err)
	}
}

func TestServerChangePageStatusBadgesAndButtons(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)
	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// 1. Initial active state
	url := fmt.Sprintf("%s/r/%s/c/%s", ts.URL, rec.Slug, c.Rev)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET change failed: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	if !strings.Contains(body, `header-status-active`) {
		t.Errorf("expected header-status-active in header")
	}
	if !strings.Contains(body, `id="commitStatusBadge" class="badge badge-status-active">Active</span>`) {
		t.Errorf("expected Active commitStatusBadge in body")
	}
	if !strings.Contains(body, `id="lgtmBtn" class="btn btn-sm btn-green"`) || !strings.Contains(body, `>LGTM</button>`) {
		t.Errorf("expected LGTM green button")
	}

	// 2. Mark LGTM
	lgtm := true
	lgtmPayload, _ := json.Marshal(map[string]any{
		"repo":   rec.Slug,
		"change": c.Rev,
		"rev":    c.Rev,
		"lgtm":   &lgtm,
	})
	resp, err = http.Post(ts.URL+"/api/changes/reviewed", "application/json", bytes.NewReader(lgtmPayload))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/changes/reviewed failed: %v", err)
	}
	_ = resp.Body.Close()

	// Verify change page updated to LGTM
	resp, err = http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET change failed after LGTM: %v", err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body = buf.String()

	if !strings.Contains(body, `header-status-lgtm`) {
		t.Errorf("expected header-status-lgtm in header")
	}
	if !strings.Contains(body, `id="commitStatusBadge" class="badge badge-status-lgtm">LGTM</span>`) {
		t.Errorf("expected LGTM commitStatusBadge in body")
	}
	if !strings.Contains(body, `id="lgtmBtn" class="btn btn-sm btn-blue"`) || !strings.Contains(body, `>Revert LGTM</button>`) {
		t.Errorf("expected Revert LGTM blue button")
	}
}

func TestServerAllDiffsCollapsedAndFileStatus(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)
	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	url := fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, c.Rev)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET change diffs=all failed: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	if !strings.Contains(body, `class="diff-file-section collapsed"`) {
		t.Errorf("expected diff-file-section collapsed")
	}
	if !strings.Contains(body, `file-collapse-btn`) || !strings.Contains(body, `>▸</button>`) {
		t.Errorf("expected chevron button ▸ with file-collapse-btn")
	}
	if !strings.Contains(body, `id="file-body-0" style="display: none;"`) {
		t.Errorf("expected diff body to have display: none")
	}
	if !strings.Contains(body, `id="toggleAllFilesBtn"`) || !strings.Contains(body, `Expand All`) {
		t.Errorf("expected toggleAllFilesBtn with Expand All")
	}
	if !strings.Contains(body, `file-status status-`) {
		t.Errorf("expected file-status badge in section header")
	}
}

func TestServerDiffSkipWidget(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)

	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	_ = os.WriteFile(filepath.Join(repoDir, "long.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0644)

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}
	run("add", "long.txt")
	run("commit", "-m", "Add long file\n\nChange-Id: Icccccccccccccccccccccccccccccccccccccccc")

	lines[24] = "line 25 modified"
	_ = os.WriteFile(filepath.Join(repoDir, "long.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0644)
	run("add", "long.txt")
	run("commit", "-m", "Edit long file\n\nChange-Id: Idddddddddddddddddddddddddddddddddddddddd")

	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	url := fmt.Sprintf("%s/r/%s/c/%s/f/long.txt?mode=split", ts.URL, rec.Slug, c.Rev)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET diff failed: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	for _, want := range []string{
		`row-skip`,
		`skip-at-top`,
		`skip-at-bottom`,
		`skip-widget`,
		`skip-controls`,
		`<span>+<span class="skip-count">`,
		`</span> common lines</span>`,
		`skip-tabs`,
		`>+10</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in diff HTML", want)
		}
	}
	if strings.Contains(body, "+10 ↓") || strings.Contains(body, "+10 ↑") {
		t.Errorf("unexpected arrow in +10 button text")
	}

	// Verify that when a file is entirely common (AtTop && AtBottom true),
	// skip-no-tabs is emitted and no empty skip-tabs container is rendered.
	for _, mode := range []string{"split", "unified"} {
		var testBuf bytes.Buffer
		testData := &FileDiffData{
			DiffMode: mode,
			Rows: []diffRowView{
				{
					Row: diff.Row{
						Type:       diff.RowSkip,
						SkipCount:  50,
						SkipGroup:  1,
						AtTop:      true,
						AtBottom:   true,
						LeftStart:  1,
						LeftEnd:    50,
						RightStart: 1,
						RightEnd:   50,
					},
				},
			},
		}
		if err := srv.templates["diff.html"].ExecuteTemplate(&testBuf, "diff_block", testData); err != nil {
			t.Fatalf("ExecuteTemplate diff_block failed for mode %s: %v", mode, err)
		}
		rendered := testBuf.String()
		if !strings.Contains(rendered, "skip-no-tabs") {
			t.Errorf("mode %s: expected skip-no-tabs on unchanged whole-file skip row", mode)
		}
		if strings.Contains(rendered, `class="skip-tabs"`) {
			t.Errorf("mode %s: expected no skip-tabs container when AtTop and AtBottom are both true", mode)
		}
		if !strings.Contains(rendered, `class="skip-count">50</span> common lines`) || !strings.Contains(rendered, `class="skip-btn skip-all"`) {
			t.Errorf("mode %s: expected skip-all button with 50 common lines", mode)
		}
	}

	// Verify skip row column count and structure across split, unified, and single-column modes:
	// Verify that tr.row-skip contains no colspan, and matches <colgroup> column counts:
	// - split mode: 4 cells (2 skip-gutter + 2 skip-code-cell)
	// - unified mode: 3 cells (2 skip-gutter + 1 skip-code-cell)
	// - single-column mode: 2 cells (1 skip-gutter + 1 skip-code-cell)
	type skipStructureCase struct {
		mode         string
		singleColumn bool
		wantGutters  int
		wantCode     int
		wantTotal    int
	}
	structTests := []skipStructureCase{
		{mode: "split", singleColumn: false, wantGutters: 2, wantCode: 2, wantTotal: 4},
		{mode: "unified", singleColumn: false, wantGutters: 2, wantCode: 1, wantTotal: 3},
		{mode: "unified", singleColumn: true, wantGutters: 1, wantCode: 1, wantTotal: 2},
	}
	for _, tc := range structTests {
		var buf bytes.Buffer
		data := &FileDiffData{
			DiffMode:     tc.mode,
			SingleColumn: tc.singleColumn,
			Rows: []diffRowView{
				{
					Row: diff.Row{
						Type:       diff.RowSkip,
						SkipCount:  15,
						SkipGroup:  1,
						AtTop:      false,
						AtBottom:   false,
						LeftStart:  10,
						LeftEnd:    25,
						RightStart: 10,
						RightEnd:   25,
					},
				},
			},
		}
		if err := srv.templates["diff.html"].ExecuteTemplate(&buf, "diff_block", data); err != nil {
			t.Fatalf("ExecuteTemplate diff_block failed for %+v: %v", tc, err)
		}
		rendered := buf.String()
		if strings.Contains(rendered, "colspan") {
			t.Errorf("%s (singleCol=%v): expected no colspan in row-skip", tc.mode, tc.singleColumn)
		}
		numGutters := strings.Count(rendered, "skip-gutter")
		if numGutters != tc.wantGutters {
			t.Errorf("%s (singleCol=%v): expected %d skip-gutter cells, got %d", tc.mode, tc.singleColumn, tc.wantGutters, numGutters)
		}
		numCode := strings.Count(rendered, "skip-code-cell")
		if numCode != tc.wantCode {
			t.Errorf("%s (singleCol=%v): expected %d skip-code-cell cells, got %d", tc.mode, tc.singleColumn, tc.wantCode, numCode)
		}
		totalCells := numGutters + numCode
		if totalCells != tc.wantTotal {
			t.Errorf("%s (singleCol=%v): expected %d total cells, got %d", tc.mode, tc.singleColumn, tc.wantTotal, totalCells)
		}
	}
}

func TestServerPrefsAPI(t *testing.T) {
	srv, ts, d, _ := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	// Set syntax to off
	pPayload, _ := json.Marshal(map[string]any{
		"key": "syntax",
		"val": "off",
	})
	resp, err := http.Post(ts.URL+"/api/prefs", "application/json", bytes.NewReader(pPayload))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/prefs failed: %v", err)
	}
	_ = resp.Body.Close()

	// Verify DB pref
	if d.Pref("syntax", "") != "off" {
		t.Errorf("expected syntax pref 'off', got %q", d.Pref("syntax", ""))
	}

	// Verify rendered page reflects syntax off
	resp, err = http.Get(ts.URL + "/")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / failed: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	if !strings.Contains(body, `data-syntax="off"`) {
		t.Errorf("expected data-syntax=\"off\" in html root")
	}
}

func TestServerAPIStateSignature(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)
	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	getStateSig := func() string {
		resp, err := http.Get(ts.URL + "/api/state")
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/state failed: code=%d err=%v", resp.StatusCode, err)
		}
		defer resp.Body.Close()
		var res struct {
			Sig string `json:"sig"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatalf("decode /api/state: %v", err)
		}
		return res.Sig
	}

	sig0 := getStateSig()
	if sig0 == "" {
		t.Fatalf("expected non-empty sig from /api/state")
	}

	// Add a draft thread
	pAdd, _ := json.Marshal(map[string]any{
		"repo":       rec.Slug,
		"change":     c.Rev,
		"rev":        c.Rev,
		"path":       "hello.go",
		"side":       "R",
		"start_line": 2,
		"end_line":   2,
		"author":     "test",
		"body":       "State test comment",
		"resolved":   false,
	})
	resp, err := http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(pAdd))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add failed: %v", err)
	}
	var addRes struct {
		Thread  *db.Thread  `json:"thread"`
		Message *db.Message `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&addRes)
	_ = resp.Body.Close()

	sig1 := getStateSig()
	if sig1 == sig0 {
		t.Errorf("expected state signature to change after adding thread, got unchanged %s", sig1)
	}

	// Delete the draft message
	pDel, _ := json.Marshal(map[string]any{
		"message_id": addRes.Message.ID,
	})
	resp, err = http.Post(ts.URL+"/api/threads/delete", "application/json", bytes.NewReader(pDel))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/delete failed: %v", err)
	}
	_ = resp.Body.Close()

	sig2 := getStateSig()
	if sig2 != sig0 {
		t.Errorf("expected state signature to return to original %s after deleting draft, got %s", sig0, sig2)
	}
}

func TestServerChangePageDefaultSnapshotSelection(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)
	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// Take Snapshot 1 so latestSnap.Rev == c.Rev
	pSnap, _ := json.Marshal(map[string]any{
		"repo":   rec.Slug,
		"change": c.Key,
	})
	resp, err := http.Post(ts.URL+"/api/snapshot", "application/json", bytes.NewReader(pSnap))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/snapshot failed: %v", err)
	}
	_ = resp.Body.Close()

	// 1. Default request to /r/<repo>/c/<key>
	url := fmt.Sprintf("%s/r/%s/c/%s", ts.URL, rec.Slug, c.Rev)
	resp, err = http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s failed: %v", url, err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	// Verify Parent and Live Snapshot are selected
	if !strings.Contains(body, `<option value="parent" selected>Parent</option>`) {
		t.Errorf("expected <option value=\"parent\" selected>Parent</option> in body")
	}
	if !strings.Contains(body, `<option value="live" selected>Live Snapshot</option>`) {
		t.Errorf("expected <option value=\"live\" selected>Live Snapshot</option> in body")
	}
	if strings.Contains(body, `<option value="snap:1" selected>`) {
		t.Errorf("did NOT expect snap:1 to be selected by default")
	}
	// Verify header badge shows Snapshot 1
	if !strings.Contains(body, `<span class="badge">Snapshot 1</span>`) {
		t.Errorf("expected header badge to show Snapshot 1")
	}

	// 2. Request with ?target=snap:1
	resp, err = http.Get(url + "?target=snap:1")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with ?target=snap:1 failed: %v", err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body = buf.String()

	if !strings.Contains(body, `<option value="snap:1" selected>Snapshot 1</option>`) {
		t.Errorf("expected <option value=\"snap:1\" selected>Snapshot 1</option> when ?target=snap:1")
	}
	if strings.Contains(body, `<option value="live" selected>`) {
		t.Errorf("did NOT expect live to be selected when ?target=snap:1")
	}

	// 3. Request with ?target=live
	resp, err = http.Get(url + "?target=live")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with ?target=live failed: %v", err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body = buf.String()

	if !strings.Contains(body, `<option value="live" selected>Live Snapshot</option>`) {
		t.Errorf("expected <option value=\"live\" selected>Live Snapshot</option> when ?target=live")
	}
	if strings.Contains(body, `<option value="snap:1" selected>`) {
		t.Errorf("did NOT expect snap:1 to be selected when ?target=live")
	}

	// 4. Amend commit and take Snapshot 2 -> verify Base is ascending (Parent, 1, 2) and Target is descending (Live, 2, 1)
	if err := os.WriteFile(filepath.Join(repoDir, "hello.go"), []byte("package main\n\nfunc main() { println(2) }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git amend failed: %s (%v)", string(out), err)
	}
	srv.cache = vcs.NewCache()
	resp, err = http.Post(ts.URL+"/api/snapshot", "application/json", bytes.NewReader(pSnap))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/snapshot (2) failed: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = http.Get(fmt.Sprintf("%s/r/%s/c/%s", ts.URL, rec.Slug, c.Key))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET change by key failed: %v", err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body = buf.String()

	baseSelectStart := strings.Index(body, `<select name="base"`)
	baseSelectEnd := strings.Index(body[baseSelectStart:], `</select>`) + baseSelectStart
	baseHTML := body[baseSelectStart:baseSelectEnd]
	if !(strings.Index(baseHTML, `value="parent"`) < strings.Index(baseHTML, `value="snap:1"`) &&
		strings.Index(baseHTML, `value="snap:1"`) < strings.Index(baseHTML, `value="snap:2"`)) {
		t.Errorf("expected Base options in ascending order (parent, snap:1, snap:2), got:\n%s", baseHTML)
	}

	targetSelectStart := strings.Index(body, `<select name="target"`)
	targetSelectEnd := strings.Index(body[targetSelectStart:], `</select>`) + targetSelectStart
	targetHTML := body[targetSelectStart:targetSelectEnd]
	if !(strings.Index(targetHTML, `value="live"`) < strings.Index(targetHTML, `value="snap:2"`) &&
		strings.Index(targetHTML, `value="snap:2"`) < strings.Index(targetHTML, `value="snap:1"`)) {
		t.Errorf("expected Target options in descending order (live, snap:2, snap:1), got:\n%s", targetHTML)
	}
}

func TestBreadcrumbsWithoutRepos(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	repo, err := vcs.Open(repoDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repo.Changes()
	if err != nil || len(changes) == 0 {
		t.Fatalf("no changes: %v", err)
	}
	c := changes[0]

	// 1. Repo page /r/<slug>
	resp, err := http.Get(fmt.Sprintf("%s/r/%s", ts.URL, rec.Slug))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /r/%s failed: %v", rec.Slug, err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	repoBody := buf.String()
	if strings.Contains(repoBody, ">repos<") {
		t.Errorf("repo page should not contain breadcrumb 'repos', got:\n%s", repoBody)
	}
	if !strings.Contains(repoBody, `<span class="sep">/</span>`) {
		t.Errorf("repo page should contain separator '/', got:\n%s", repoBody)
	}

	// 2. Change page /r/<slug>/c/<key>
	resp, err = http.Get(fmt.Sprintf("%s/r/%s/c/%s", ts.URL, rec.Slug, c.Key))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /r/%s/c/%s failed: %v", rec.Slug, c.Key, err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	changeBody := buf.String()
	if strings.Contains(changeBody, ">repos<") {
		t.Errorf("change page should not contain breadcrumb 'repos', got:\n%s", changeBody)
	}
	if !strings.Contains(changeBody, `<span class="sep">/</span>`) || !strings.Contains(changeBody, fmt.Sprintf(`<a href="/r/%s" id="upLink">%s</a>`, rec.Slug, rec.Slug)) {
		t.Errorf("change page should contain separator and repo link, got:\n%s", changeBody)
	}

	// 3. Diff page /r/<slug>/c/<key>/f/<file>
	resp, err = http.Get(fmt.Sprintf("%s/r/%s/c/%s/f/hello.go", ts.URL, rec.Slug, c.Key))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /r/%s/c/%s/f/hello.go failed: %v", rec.Slug, c.Key, err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	diffBody := buf.String()
	if strings.Contains(diffBody, ">repos<") {
		t.Errorf("diff page should not contain breadcrumb 'repos', got:\n%s", diffBody)
	}
}

func TestThreadRangeAndHighlight(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	repo, err := vcs.Open(repoDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repo.Changes()
	if err != nil || len(changes) == 0 {
		t.Fatalf("no changes: %v", err)
	}
	c := changes[0]

	// 1. Verify CSS styles for range highlight exist
	resp, err := http.Get(ts.URL + "/static/style.css")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/style.css failed: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	css := buf.String()
	if !strings.Contains(css, "--comment-range-bg: #a8d1ff;") {
		t.Errorf("style.css missing --comment-range-bg light theme variable")
	}
	if !strings.Contains(css, ".comment-range-hl") {
		t.Errorf("style.css missing .comment-range-hl rule")
	}
	if !strings.Contains(css, "td.comment-range-line") {
		t.Errorf("style.css missing td.comment-range-line rule")
	}
	if !strings.Contains(css, `html[data-syntax="off"] #symbolPopup [class^="tok-"]`) {
		t.Errorf("style.css missing html[data-syntax=\"off\"] #symbolPopup rule")
	}

	// 2. Add a character-range comment (single line, start_col=2, end_col=8)
	req1 := map[string]any{
		"repo":       rec.Slug,
		"change":     c.Key,
		"rev":        c.Rev,
		"path":       "hello.go",
		"side":       "R",
		"start_line": 1,
		"end_line":   1,
		"start_col":  2,
		"end_col":    8,
		"body":       "word range comment",
		"resolved":   false,
	}
	b1, _ := json.Marshal(req1)
	resp, err = http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(b1))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add (1) failed: code=%d err=%v", resp.StatusCode, err)
	}
	var res1 struct {
		Thread *db.Thread `json:"thread"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res1)
	_ = resp.Body.Close()
	if res1.Thread == nil || res1.Thread.StartCol != 2 || res1.Thread.EndCol != 8 {
		t.Fatalf("unexpected thread response: %+v", res1.Thread)
	}

	// 3. Add a multi-line comment (start_line=1, end_line=2, start_col=0, end_col=0)
	req2 := map[string]any{
		"repo":       rec.Slug,
		"change":     c.Key,
		"rev":        c.Rev,
		"path":       "hello.go",
		"side":       "R",
		"start_line": 1,
		"end_line":   2,
		"body":       "multiline comment",
		"resolved":   false,
	}
	b2, _ := json.Marshal(req2)
	resp, err = http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(b2))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add (2) failed: %v", err)
	}
	var res2 struct {
		Thread *db.Thread `json:"thread"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res2)
	_ = resp.Body.Close()

	// 4. Add a plain single whole-line comment (start_line=1, end_line=1, start_col=0, end_col=0)
	req3 := map[string]any{
		"repo":       rec.Slug,
		"change":     c.Key,
		"rev":        c.Rev,
		"path":       "hello.go",
		"side":       "R",
		"start_line": 1,
		"end_line":   1,
		"body":       "single line comment",
		"resolved":   false,
	}
	b3, _ := json.Marshal(req3)
	resp, err = http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(b3))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add (3) failed: %v", err)
	}
	var res3 struct {
		Thread *db.Thread `json:"thread"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res3)
	_ = resp.Body.Close()

	// 5. Fetch diff page and verify data-has-range attributes
	resp, err = http.Get(fmt.Sprintf("%s/r/%s/c/%s/f/hello.go", ts.URL, rec.Slug, c.Key))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET diff page failed: %v", err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	diffHTML := buf.String()

	// Thread 1 (word range) must have data-has-range="true", start-col="2", end-col="8"
	card1 := fmt.Sprintf(`id="thread-%d"`, res1.Thread.ID)
	idx1 := strings.Index(diffHTML, card1)
	if idx1 == -1 {
		t.Fatalf("thread %d not found in HTML", res1.Thread.ID)
	}
	chunk1 := diffHTML[idx1 : idx1+250]
	if !strings.Contains(chunk1, `data-has-range="true"`) {
		t.Errorf("thread 1 should have data-has-range, got:\n%s", chunk1)
	}
	if !strings.Contains(chunk1, `data-start-col="2"`) || !strings.Contains(chunk1, `data-end-col="8"`) {
		t.Errorf("thread 1 should have data-start-col=2 and data-end-col=8, got:\n%s", chunk1)
	}

	// Thread 2 (multi-line) must have data-has-range="true", start-line="1", end-line="2"
	card2 := fmt.Sprintf(`id="thread-%d"`, res2.Thread.ID)
	idx2 := strings.Index(diffHTML, card2)
	if idx2 == -1 {
		t.Fatalf("thread %d not found in HTML", res2.Thread.ID)
	}
	chunk2 := diffHTML[idx2 : idx2+250]
	if !strings.Contains(chunk2, `data-has-range="true"`) {
		t.Errorf("thread 2 should have data-has-range, got:\n%s", chunk2)
	}
	if !strings.Contains(chunk2, `data-start-line="1"`) || !strings.Contains(chunk2, `data-end-line="2"`) {
		t.Errorf("thread 2 should have data-start-line=1 and data-end-line=2, got:\n%s", chunk2)
	}

	// Thread 3 (plain single line) must NOT have data-has-range
	card3 := fmt.Sprintf(`id="thread-%d"`, res3.Thread.ID)
	idx3 := strings.Index(diffHTML, card3)
	if idx3 == -1 {
		t.Fatalf("thread %d not found in HTML", res3.Thread.ID)
	}
	chunk3 := diffHTML[idx3 : idx3+250]
	if strings.Contains(chunk3, `data-has-range="true"`) {
		t.Errorf("thread 3 (plain single line) should NOT have data-has-range, got:\n%s", chunk3)
	}

	// 6. Fetch change page and verify comments-summary-card is GONE, while comment count badges are present
	resp, err = http.Get(fmt.Sprintf("%s/r/%s/c/%s", ts.URL, rec.Slug, c.Key))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET change page failed: %v", err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	changeHTML := buf.String()
	if strings.Contains(changeHTML, "comments-summary-card") {
		t.Errorf("change page should NOT contain comments-summary-card")
	}
	if !strings.Contains(changeHTML, `id="changeCommentCounts"`) {
		t.Errorf("change page should contain #changeCommentCounts, got:\n%s", changeHTML)
	}
	if !strings.Contains(changeHTML, `data-file-counts="hello.go"`) {
		t.Errorf("change page should contain data-file-counts for hello.go, got:\n%s", changeHTML)
	}
}

func TestEmptyCommitsExcludedFromDashboardAndRepo(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	// Create an empty commit with a description
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "WIP: empty commit with desc\n\nChange-Id: I2222222222222222222222222222222222222222")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to create empty commit: %s (%v)", string(out), err)
	}

	// 1. Dashboard
	resp, err := http.Get(ts.URL + "/")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / failed: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	dashHTML := buf.String()
	if strings.Contains(dashHTML, "empty commit with desc") {
		t.Errorf("dashboard should exclude content-empty commits, got:\n%s", dashHTML)
	}

	// 2. Repo page
	resp, err = http.Get(fmt.Sprintf("%s/r/%s", ts.URL, rec.Slug))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /r/%s failed: %v", rec.Slug, err)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	repoHTML := buf.String()
	if strings.Contains(repoHTML, "empty commit with desc") {
		t.Errorf("repo page should exclude content-empty commits, got:\n%s", repoHTML)
	}
}

func TestAPIThreadActionsReturnPayloads(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	repo, err := vcs.Open(repoDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repo.Changes()
	if err != nil || len(changes) == 0 {
		t.Fatalf("no changes: %v", err)
	}
	c := changes[0]

	// 1. Add comment
	addReq := map[string]any{
		"repo":       rec.Slug,
		"change":     c.Key,
		"rev":        c.Rev,
		"path":       "hello.go",
		"side":       "R",
		"start_line": 1,
		"end_line":   1,
		"body":       "initial comment",
		"resolved":   false,
	}
	bAdd, _ := json.Marshal(addReq)
	resp, err := http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(bAdd))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add failed: %v", err)
	}
	var addRes struct {
		Thread       *db.Thread           `json:"thread"`
		Message      *db.Message          `json:"message"`
		HTML         string               `json:"html"`
		Sig          string               `json:"sig"`
		FilePath     string               `json:"file_path"`
		FileCounts   engine.CommentCounts `json:"file_counts"`
		ChangeCounts engine.CommentCounts `json:"change_counts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&addRes)
	_ = resp.Body.Close()

	if addRes.HTML == "" || addRes.Sig == "" || addRes.FilePath != "hello.go" {
		t.Fatalf("unexpected add response: %+v", addRes)
	}
	if addRes.FileCounts.Unresolved != 1 || addRes.FileCounts.Draft != 1 {
		t.Errorf("unexpected file counts after add: %+v", addRes.FileCounts)
	}
	if addRes.ChangeCounts.Unresolved != 1 || addRes.ChangeCounts.Draft != 1 {
		t.Errorf("unexpected change counts after add: %+v", addRes.ChangeCounts)
	}

	// 2. Reply to thread
	replyReq := map[string]any{
		"thread_id": addRes.Thread.ID,
		"author":    "bob",
		"body":      "reply message",
	}
	bReply, _ := json.Marshal(replyReq)
	resp, err = http.Post(ts.URL+"/api/threads/reply", "application/json", bytes.NewReader(bReply))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/reply failed: %v", err)
	}
	var replyRes struct {
		Message      *db.Message          `json:"message"`
		HTML         string               `json:"html"`
		Sig          string               `json:"sig"`
		FilePath     string               `json:"file_path"`
		FileCounts   engine.CommentCounts `json:"file_counts"`
		ChangeCounts engine.CommentCounts `json:"change_counts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&replyRes)
	_ = resp.Body.Close()

	if replyRes.HTML == "" || replyRes.Sig == "" || replyRes.FilePath != "hello.go" {
		t.Fatalf("unexpected reply response: %+v", replyRes)
	}
	if replyRes.FileCounts.Draft != 2 {
		t.Errorf("expected 2 drafts after reply, got %d", replyRes.FileCounts.Draft)
	}

	// 3. Edit message
	editReq := map[string]any{
		"message_id": replyRes.Message.ID,
		"body":       "edited reply message",
	}
	bEdit, _ := json.Marshal(editReq)
	resp, err = http.Post(ts.URL+"/api/threads/edit", "application/json", bytes.NewReader(bEdit))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/edit failed: %v", err)
	}
	var editRes struct {
		HTML       string               `json:"html"`
		Sig        string               `json:"sig"`
		FilePath   string               `json:"file_path"`
		FileCounts engine.CommentCounts `json:"file_counts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&editRes)
	_ = resp.Body.Close()
	if editRes.HTML == "" || editRes.Sig == "" || !strings.Contains(editRes.HTML, "edited reply message") {
		t.Fatalf("unexpected edit response: %+v", editRes)
	}

	// 4. Resolve thread
	resolveReq := map[string]any{
		"thread_id": addRes.Thread.ID,
		"resolved":  true,
	}
	bRes, _ := json.Marshal(resolveReq)
	resp, err = http.Post(ts.URL+"/api/threads/resolve", "application/json", bytes.NewReader(bRes))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/resolve failed: %v", err)
	}
	var resolveRes struct {
		HTML         string               `json:"html"`
		Sig          string               `json:"sig"`
		FileCounts   engine.CommentCounts `json:"file_counts"`
		ChangeCounts engine.CommentCounts `json:"change_counts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&resolveRes)
	_ = resp.Body.Close()
	if resolveRes.FileCounts.Resolved != 1 || resolveRes.FileCounts.Unresolved != 0 {
		t.Errorf("expected 1 resolved 0 unresolved, got: %+v", resolveRes.FileCounts)
	}

	// 5. Delete draft message (first delete reply message: thread still exists)
	del1Req := map[string]any{"message_id": replyRes.Message.ID}
	bDel1, _ := json.Marshal(del1Req)
	resp, err = http.Post(ts.URL+"/api/threads/delete", "application/json", bytes.NewReader(bDel1))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/delete (1) failed: %v", err)
	}
	var del1Res struct {
		ThreadID      int64  `json:"thread_id"`
		ThreadDeleted bool   `json:"thread_deleted"`
		HTML          string `json:"html"`
		Sig           string `json:"sig"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&del1Res)
	_ = resp.Body.Close()
	if del1Res.ThreadDeleted {
		t.Errorf("thread should not be deleted after deleting only reply")
	}
	if del1Res.HTML == "" {
		t.Errorf("expected remaining thread HTML, got empty")
	}

	// 6. Delete root message: thread deleted completely
	del2Req := map[string]any{"message_id": addRes.Message.ID}
	bDel2, _ := json.Marshal(del2Req)
	resp, err = http.Post(ts.URL+"/api/threads/delete", "application/json", bytes.NewReader(bDel2))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/delete (2) failed: %v", err)
	}
	var del2Res struct {
		ThreadID      int64                `json:"thread_id"`
		ThreadDeleted bool                 `json:"thread_deleted"`
		HTML          string               `json:"html"`
		FileCounts    engine.CommentCounts `json:"file_counts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&del2Res)
	_ = resp.Body.Close()
	if !del2Res.ThreadDeleted {
		t.Errorf("expected thread to be deleted when all messages are deleted")
	}
	if del2Res.FileCounts.Resolved != 0 || del2Res.FileCounts.Unresolved != 0 {
		t.Errorf("expected 0 comments after all deleted, got: %+v", del2Res.FileCounts)
	}

	t.Run("RelationChainLiveBadge", func(t *testing.T) {
		nodeBin, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node not available")
		}
		script := `
const fs = require('fs');
const vm = require('vm');
const code = fs.readFileSync('static/app.js', 'utf8');

let relBadge = null;
const relInfo = {
  children: [],
  querySelector(sel) {
    if (sel === '.badge-count-unresolved') return relBadge;
    return null;
  },
  appendChild(el) {
    el.remove = () => {
      if (relBadge === el) relBadge = null;
    };
    relBadge = el;
    this.children.push(el);
  }
};
const changeCountsEl = { innerHTML: '' };
const fileCountsEl = { innerHTML: '' };
let nextFetchData = null;
let lastQueryAllSel = null;

const doc = {
  readyState: 'complete',
  documentElement: {
    getAttribute() { return ''; },
    setAttribute() {},
    removeAttribute() {},
  },
  addEventListener() {},
  getElementById(id) {
    if (id === 'changeCommentCounts') return changeCountsEl;
    return null;
  },
  querySelector(sel) {
    if (sel === '.relation-chain-card li.current-change-row .commit-info') return relInfo;
    return null;
  },
  querySelectorAll(sel) {
    lastQueryAllSel = sel;
    if (sel.startsWith('span.comment-counts[data-file-counts=')) return [fileCountsEl];
    return [];
  },
  createElement(tag) {
    return {
      tagName: tag.toUpperCase(),
      className: '',
      style: { cssText: '' },
      textContent: '',
      remove() {}
    };
  }
};
const win = {
  document: doc,
  location: { pathname: '/r/test/c/abc', search: '', hash: '', reload() {} },
  addEventListener() {},
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  fetch() {
    const d = nextFetchData;
    return Promise.resolve({ ok: true, json: () => Promise.resolve(d) });
  }
};
const ctx = vm.createContext({
  window: win,
  document: doc,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  fetch: win.fetch,
  URLSearchParams,
  Promise,
  setTimeout,
  clearTimeout,
  console,
});
vm.runInContext(code, ctx);

(async () => {
  // 1. Transition 0 -> 1 unresolved (e.g. reopen / add comment)
  nextFetchData = {
    sig: 's1',
    html: '<div></div>',
    file_path: 'hello.go',
    file_counts: { unresolved: 1, resolved: 0, draft: 1 },
    change_counts: { unresolved: 1, resolved: 0, draft: 1 },
  };
  win.resolveThread(1, false);
  await new Promise(r => setImmediate(r));
  if (!relBadge || relBadge.textContent !== '1' || !relBadge.className.includes('badge-count-unresolved')) {
    throw new Error('expected relation chain badge "1", got: ' + JSON.stringify(relBadge));
  }

  // 2. Transition 1 -> 2 unresolved
  nextFetchData = {
    sig: 's2',
    html: '<div></div>',
    file_path: 'hello.go',
    file_counts: { unresolved: 2, resolved: 0, draft: 2 },
    change_counts: { unresolved: 2, resolved: 0, draft: 2 },
  };
  win.resolveThread(2, false);
  await new Promise(r => setImmediate(r));
  if (!relBadge || relBadge.textContent !== '2') {
    throw new Error('expected relation chain badge "2", got: ' + JSON.stringify(relBadge));
  }

  // 3. Transition 2 -> 0 unresolved (delete / resolve all)
  nextFetchData = {
    sig: 's3',
    thread_id: 2,
    thread_deleted: true,
    html: '',
    file_path: 'hello.go',
    file_counts: { unresolved: 0, resolved: 0, draft: 0 },
    change_counts: { unresolved: 0, resolved: 0, draft: 0 },
  };
  win.deleteMessage(10);
  await new Promise(r => setImmediate(r));
  if (relBadge !== null) {
    throw new Error('expected relation chain badge to be removed at 0 unresolved, got: ' + JSON.stringify(relBadge));
  }

  // 4. Test numeric-leading and nested file path (e.g. 1a/test.go)
  nextFetchData = {
    sig: 's4',
    html: '<div></div>',
    file_path: '1a/test.go',
    file_counts: { unresolved: 1, resolved: 0, draft: 0 },
    change_counts: { unresolved: 1, resolved: 0, draft: 0 },
  };
  win.resolveThread(3, false);
  await new Promise(r => setImmediate(r));
  const expectedSel = 'span.comment-counts[data-file-counts="1a/test.go"]';
  if (lastQueryAllSel !== expectedSel) {
    throw new Error('expected selector ' + expectedSel + ', got: ' + lastQueryAllSel);
  }
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
		cmd := exec.Command(nodeBin, "-e", script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("RelationChainLiveBadge JS check failed: %v\n%s", err, string(out))
		}
	})
}

func TestCommitMsgReviewedAndThreads(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	repo, err := vcs.Open(repoDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repo.Changes()
	if err != nil || len(changes) == 0 {
		t.Fatalf("no changes: %v", err)
	}
	c := changes[0]

	fetchHTML := func(rawURL string) string {
		t.Helper()
		resp, err := http.Get(rawURL)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s failed: resp=%v err=%v", rawURL, resp, err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return buf.String()
	}

	// 1. Initial change page check (diffs=list):
	html := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s?diffs=list", ts.URL, rec.Slug, c.Key))

	if !strings.Contains(html, `id="copyLinkBtn"`) {
		t.Errorf("expected #copyLinkBtn in layout header")
	}

	// Commit Message summary card must NOT carry duplicate reviewed buttons/icons or Single file link.
	cardStart := strings.Index(html, "commit-msg-card")
	cardEnd := strings.Index(html, "relation-chain-card")
	if cardStart < 0 || cardEnd <= cardStart {
		t.Fatalf("could not locate commit-msg-card slice in HTML")
	}
	commitMsgCardHTML := html[cardStart:cardEnd]
	if !strings.Contains(commitMsgCardHTML, "commit-msg-box") {
		t.Errorf("expected commit-msg-box inside commit-msg-card")
	}
	for _, forbidden := range []string{"Mark Reviewed", "Mark Unreviewed", "Single file", "data-reviewed-btn", "data-reviewed-icon"} {
		if strings.Contains(commitMsgCardHTML, forbidden) {
			t.Errorf("commit-msg-card should not contain %q, got:\n%s", forbidden, commitMsgCardHTML)
		}
	}

	// File List shows /COMMIT_MSG first with regular file-row controls.
	if !strings.Contains(html, "<span>Files (2)</span>") {
		t.Errorf("expected Files (2) in file list")
	}
	idxCommitMsg := strings.Index(html, "<span>/COMMIT_MSG</span>")
	idxHelloGo := strings.Index(html, "<span>hello.go</span>")
	if idxCommitMsg < 0 || idxHelloGo < 0 || idxCommitMsg >= idxHelloGo {
		t.Errorf("expected /COMMIT_MSG before hello.go in file list (idxCommitMsg=%d, idxHelloGo=%d)", idxCommitMsg, idxHelloGo)
	}
	if !strings.Contains(html, `data-reviewed-btn="COMMIT_MSG"`) {
		t.Errorf("File list should contain data-reviewed-btn=\"COMMIT_MSG\"")
	}
	if !strings.Contains(html, `data-reviewed-icon="COMMIT_MSG"`) {
		t.Errorf("File list should contain data-reviewed-icon=\"COMMIT_MSG\"")
	}

	// 1b. All Diffs renders /COMMIT_MSG first as file-sec-0 with commit metadata headers and controls.
	htmlAll := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, c.Key))
	secCommitIdx := strings.Index(htmlAll, `id="file-sec-0" data-file-path="COMMIT_MSG"`)
	secHelloIdx := strings.Index(htmlAll, `id="file-sec-1" data-file-path="hello.go"`)
	if secCommitIdx < 0 || secHelloIdx < 0 || secCommitIdx >= secHelloIdx {
		t.Errorf("expected file-sec-0 COMMIT_MSG before file-sec-1 hello.go in diffs=all")
	}
	commitSecHTML := htmlAll[secCommitIdx:secHelloIdx]
	for _, want := range []string{
		`data-reviewed-btn="COMMIT_MSG"`,
		"/f/COMMIT_MSG",
		"Author:",
		"AuthorDate:",
		"Commit:",
		"CommitDate:",
	} {
		if !strings.Contains(commitSecHTML, want) {
			t.Errorf("diffs=all /COMMIT_MSG section missing %q", want)
		}
	}

	// 1c. Single-file /f/COMMIT_MSG and /f/hello.go navigation and NextUnreviewedFile.
	singleCommitHTML := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s/f/COMMIT_MSG", ts.URL, rec.Slug, c.Key))
	if !strings.Contains(singleCommitHTML, `id="toggleReviewedBtn"`) || !strings.Contains(singleCommitHTML, "Mark Reviewed (m)") {
		t.Errorf("expected Mark Reviewed (m) button on /f/COMMIT_MSG")
	}
	if !strings.Contains(singleCommitHTML, `id="nextFileLink"`) || !strings.Contains(singleCommitHTML, "/f/hello.go") {
		t.Errorf("expected nextFileLink to /f/hello.go on /f/COMMIT_MSG")
	}
	if !strings.Contains(singleCommitHTML, `data-next-unreviewed="hello.go"`) {
		t.Errorf("expected data-next-unreviewed=\"hello.go\" on /f/COMMIT_MSG")
	}
	if strings.Contains(singleCommitHTML, ">unchanged<") {
		t.Errorf("did not expect unchanged badge on /f/COMMIT_MSG when base=parent")
	}
	for _, want := range []string{"Author:", "AuthorDate:", "Commit:", "CommitDate:"} {
		if !strings.Contains(singleCommitHTML, want) {
			t.Errorf("/f/COMMIT_MSG missing formatted header %q", want)
		}
	}

	singleHelloHTML := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s/f/hello.go", ts.URL, rec.Slug, c.Key))
	if !strings.Contains(singleHelloHTML, `id="prevFileLink"`) || !strings.Contains(singleHelloHTML, "/f/COMMIT_MSG") {
		t.Errorf("expected prevFileLink to /f/COMMIT_MSG on /f/hello.go")
	}
	if !strings.Contains(singleHelloHTML, `data-next-unreviewed="COMMIT_MSG"`) {
		t.Errorf("expected /f/hello.go to point data-next-unreviewed to COMMIT_MSG when unreviewed")
	}

	// 2. Mark /COMMIT_MSG as reviewed via API
	markReq := map[string]any{
		"repo":     rec.Slug,
		"change":   c.Key,
		"rev":      c.Rev,
		"path":     "/COMMIT_MSG",
		"reviewed": true,
	}
	bMark, _ := json.Marshal(markReq)
	resp, err := http.Post(ts.URL+"/api/files/reviewed", "application/json", bytes.NewReader(bMark))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/files/reviewed failed: %v", err)
	}
	_ = resp.Body.Close()

	// 3. Re-fetch change page & /f/COMMIT_MSG & /f/hello.go
	htmlAfter := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s?diffs=list", ts.URL, rec.Slug, c.Key))
	if !strings.Contains(htmlAfter, `data-reviewed-btn="COMMIT_MSG"`) || !strings.Contains(htmlAfter, `Mark Unreviewed`) {
		t.Errorf("expected /COMMIT_MSG file row reviewed button to show Mark Unreviewed, got:\n%s", htmlAfter)
	}
	singleCommitReviewed := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s/f/COMMIT_MSG", ts.URL, rec.Slug, c.Key))
	if !strings.Contains(singleCommitReviewed, "Reviewed ✓") {
		t.Errorf("expected /f/COMMIT_MSG to show Reviewed ✓ after marking reviewed")
	}
	singleHelloAfter := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s/f/hello.go", ts.URL, rec.Slug, c.Key))
	if strings.Contains(singleHelloAfter, "data-next-unreviewed") {
		t.Errorf("did not expect data-next-unreviewed on /f/hello.go once /COMMIT_MSG is reviewed")
	}

	// 4. Unmark with COMMIT_MSG (without leading slash)
	markReq2 := map[string]any{
		"repo":     rec.Slug,
		"change":   c.Key,
		"rev":      c.Rev,
		"path":     "COMMIT_MSG",
		"reviewed": false,
	}
	bMark2, _ := json.Marshal(markReq2)
	resp, err = http.Post(ts.URL+"/api/files/reviewed", "application/json", bytes.NewReader(bMark2))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/files/reviewed (unmark) failed: %v", err)
	}
	_ = resp.Body.Close()

	marks, err := d.FileMarks(rec.ID, c.Key, c.Rev)
	if err != nil {
		t.Fatal(err)
	}
	if marks[vcs.CommitMsgPath] {
		t.Errorf("expected /COMMIT_MSG mark to be false after unmarking")
	}

	// 5. LGTM marks /COMMIT_MSG reviewed alongside code files in DB
	lgtmBody, _ := json.Marshal(map[string]any{
		"repo":   rec.Slug,
		"change": c.Key,
		"rev":    c.Rev,
		"lgtm":   true,
	})
	resp, err = http.Post(ts.URL+"/api/changes/reviewed", "application/json", bytes.NewReader(lgtmBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/changes/reviewed lgtm failed: %v", err)
	}
	_ = resp.Body.Close()
	marksAfterLGTM, err := d.FileMarks(rec.ID, c.Key, c.Rev)
	if err != nil {
		t.Fatal(err)
	}
	if !marksAfterLGTM[vcs.CommitMsgPath] {
		t.Errorf("expected /COMMIT_MSG mark to be true after LGTM, got %+v", marksAfterLGTM)
	}
	htmlAfterLGTM := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s", ts.URL, rec.Slug, c.Key))
	if !strings.Contains(htmlAfterLGTM, `data-reviewed-btn="COMMIT_MSG"`) || !strings.Contains(htmlAfterLGTM, `Mark Unreviewed`) {
		t.Errorf("expected /COMMIT_MSG to remain marked reviewed on reload after LGTM")
	}
	singleHelloAfterLGTM := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s/f/hello.go", ts.URL, rec.Slug, c.Key))
	if strings.Contains(singleHelloAfterLGTM, "data-next-unreviewed") {
		t.Errorf("did not expect data-next-unreviewed on /f/hello.go after LGTM")
	}

	t.Run("CommentOnCommitMsgWithoutLeadingSlash", func(t *testing.T) {
		addPayload, _ := json.Marshal(map[string]any{
			"repo":       rec.Slug,
			"change":     c.Key,
			"rev":        c.Rev,
			"path":       "COMMIT_MSG",
			"side":       "R",
			"start_line": 2,
			"end_line":   2,
			"body":       "Check author date",
		})
		resp, err := http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(addPayload))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/threads/add on COMMIT_MSG failed: resp=%v err=%v", resp, err)
		}
		defer resp.Body.Close()
		var addRes struct {
			Thread struct {
				ID          int64  `json:"id"`
				Path        string `json:"path"`
				AnchorLines string `json:"anchor_lines"`
			} `json:"thread"`
			HTML string `json:"html"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&addRes); err != nil {
			t.Fatalf("decode /api/threads/add response: %v", err)
		}
		if addRes.Thread.Path != vcs.CommitMsgPath {
			t.Errorf("expected stored thread.Path == %q, got %q", vcs.CommitMsgPath, addRes.Thread.Path)
		}
		if !strings.HasPrefix(addRes.Thread.AnchorLines, "AuthorDate:") {
			t.Errorf("expected non-empty AnchorLines for line 2 of /COMMIT_MSG, got %q", addRes.Thread.AnchorLines)
		}
		if strings.Contains(addRes.HTML, "thread-stale") || strings.Contains(addRes.HTML, "stale @") {
			t.Errorf("expected /api/threads/add returned HTML not to be marked stale, got:\n%s", addRes.HTML)
		}

		unresJSON := fetchHTML(fmt.Sprintf("%s/api/threads/list?repo=%s&change=%s&base=parent&target=live", ts.URL, rec.Slug, c.Key))
		var unresRes struct {
			Threads []struct {
				ID    int64  `json:"id"`
				Path  string `json:"path"`
				Line  int    `json:"line"`
				Stale bool   `json:"stale"`
			} `json:"threads"`
		}
		if err := json.Unmarshal([]byte(unresJSON), &unresRes); err != nil {
			t.Fatalf("decode /api/threads/list: %v", err)
		}
		if len(unresRes.Threads) != 1 || unresRes.Threads[0].Stale || unresRes.Threads[0].Path != "COMMIT_MSG" || unresRes.Threads[0].Line != 2 {
			t.Errorf("expected unresolved thread {Path: COMMIT_MSG, Line: 2, Stale: false}, got %+v", unresRes.Threads)
		}

		allHTML := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, c.Key))
		sec0 := strings.Index(allHTML, `id="file-sec-0" data-file-path="COMMIT_MSG"`)
		sec1 := strings.Index(allHTML, `id="file-sec-1" data-file-path="hello.go"`)
		if sec0 < 0 || sec1 <= sec0 {
			t.Fatalf("could not locate /COMMIT_MSG section in diffs=all HTML")
		}
		secHTML := allHTML[sec0:sec1]
		threadAttr := fmt.Sprintf(`id="thread-%d"`, addRes.Thread.ID)
		if !strings.Contains(secHTML, threadAttr) || !strings.Contains(secHTML, `class="thread-row"`) {
			t.Errorf("expected thread %s anchored in thread-row inside /COMMIT_MSG section", threadAttr)
		}
		if strings.Contains(secHTML, "file-threads-container") || strings.Contains(secHTML, "thread-stale") || strings.Contains(secHTML, "stale @") {
			t.Errorf("expected /COMMIT_MSG thread on line 2 to be anchored inline without stale markers, got:\n%s", secHTML)
		}
	})

	t.Run("ChildCommitParentHeaderAndExpand", func(t *testing.T) {
		runGit := func(args ...string) {
			cmd := exec.Command("git", args...)
			cmd.Dir = repoDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
			}
		}
		if err := os.WriteFile(filepath.Join(repoDir, "hello.go"), []byte("package main\n\nfunc main() { println(1) }\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "Second child commit\n\nChange-Id: Ibbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

		srv.cache.Evict(repoDir)
		repo2, err := vcs.Open(repoDir, srv.cache)
		if err != nil {
			t.Fatal(err)
		}
		changes2, err := repo2.Changes()
		if err != nil || len(changes2) == 0 {
			t.Fatalf("Changes failed: %v", err)
		}
		c2 := changes2[0]
		if len(c2.Parent) < 8 {
			t.Fatalf("expected c2 to have a commit parent, got %q", c2.Parent)
		}

		childSingleHTML := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s/f/COMMIT_MSG", ts.URL, rec.Slug, c2.Key))
		wantParentLine := "Parent:     " + c2.Parent[:8] + " (First commit)"
		if !strings.Contains(childSingleHTML, wantParentLine) {
			t.Errorf("expected /f/COMMIT_MSG for child commit to contain %q", wantParentLine)
		}

		expandURL := fmt.Sprintf("%s/api/diff/expand?repo=%s&change=%s&path=COMMIT_MSG&left_rev=%s&right_rev=%s&left_start=6&left_end=6&right_start=7&right_end=7&mode=split",
			ts.URL, rec.Slug, c2.Key, c2.Parent, c2.Rev)
		expandHTML := fetchHTML(expandURL)
		if !strings.Contains(expandHTML, "Second child commit") {
			t.Errorf("expected /api/diff/expand right side to contain %q, got:\n%s", "Second child commit", expandHTML)
		}
		if strings.Contains(expandHTML, "First commit") {
			t.Errorf("did not expect /api/diff/expand to leak parent commit message body %q into left side, got:\n%s", "First commit", expandHTML)
		}

		// Side=L comment on /COMMIT_MSG created while comparing snapshots:
		// must anchor on snap:1..snap:2, but become stale against parent without matching parent's Author line.
		snap1, _, err := d.TakeSnapshot(rec.ID, c2)
		if err != nil {
			t.Fatalf("TakeSnapshot 1 on c2: %v", err)
		}
		thLeft, _, err := engine.AddComment(d, repo2, rec.ID, c2, snap1.Rev, vcs.CommitMsgPath, "L", 2, 2, "Alice", "check snap1 author", false, false)
		if err != nil {
			t.Fatalf("AddComment Side=L on snap1: %v", err)
		}
		runGit("commit", "--amend", "-m", "Second child commit amended\n\nChange-Id: Ibbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		srv.cache.Evict(repoDir)
		repo3, err := vcs.Open(repoDir, srv.cache)
		if err != nil {
			t.Fatal(err)
		}
		changes3, err := repo3.Changes()
		if err != nil || len(changes3) == 0 {
			t.Fatalf("Changes after amend failed: %v", err)
		}
		c3 := changes3[0]
		if _, _, err := d.TakeSnapshot(rec.ID, c3); err != nil {
			t.Fatalf("TakeSnapshot 2 on c3: %v", err)
		}

		unresSnapsJSON := fetchHTML(fmt.Sprintf("%s/api/threads/list?repo=%s&change=%s&base=snap:1&target=snap:2", ts.URL, rec.Slug, c3.Key))
		var unresSnaps struct {
			Threads []struct {
				ID    int64 `json:"id"`
				Stale bool  `json:"stale"`
			} `json:"threads"`
		}
		if err := json.Unmarshal([]byte(unresSnapsJSON), &unresSnaps); err != nil {
			t.Fatalf("decode unresolved snap:1..snap:2: %v", err)
		}
		var foundNotStale bool
		for _, item := range unresSnaps.Threads {
			if item.ID == thLeft.ID {
				foundNotStale = true
				if item.Stale {
					t.Errorf("expected Side=L thread #%d to be non-stale in snap:1..snap:2", thLeft.ID)
				}
			}
		}
		if !foundNotStale {
			t.Errorf("expected Side=L thread #%d in unresolved threads for snap:1..snap:2", thLeft.ID)
		}

		unresParentJSON := fetchHTML(fmt.Sprintf("%s/api/threads/list?repo=%s&change=%s&base=parent&target=live", ts.URL, rec.Slug, c3.Key))
		var unresParent struct {
			Threads []struct {
				ID    int64 `json:"id"`
				Stale bool  `json:"stale"`
			} `json:"threads"`
		}
		if err := json.Unmarshal([]byte(unresParentJSON), &unresParent); err != nil {
			t.Fatalf("decode unresolved parent..live: %v", err)
		}
		var foundStale bool
		for _, item := range unresParent.Threads {
			if item.ID == thLeft.ID {
				foundStale = true
				if !item.Stale {
					t.Errorf("expected Side=L thread #%d on /COMMIT_MSG to be stale when base=parent", thLeft.ID)
				}
			}
		}
		if !foundStale {
			t.Errorf("expected Side=L thread #%d in unresolved threads for parent..live", thLeft.ID)
		}

		replyPayload, _ := json.Marshal(map[string]any{
			"thread_id": thLeft.ID,
			"body":      "reply checking renderThreadCard",
			"left_rev":  c3.Parent,
			"right_rev": c3.Rev,
		})
		replyResp, err := http.Post(ts.URL+"/api/threads/reply", "application/json", bytes.NewReader(replyPayload))
		if err != nil || replyResp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/threads/reply failed: resp=%v err=%v", replyResp, err)
		}
		var replyRes struct {
			HTML string `json:"html"`
		}
		if err := json.NewDecoder(replyResp.Body).Decode(&replyRes); err != nil {
			_ = replyResp.Body.Close()
			t.Fatalf("decode reply response: %v", err)
		}
		_ = replyResp.Body.Close()
		if !strings.Contains(replyRes.HTML, "thread-stale") && !strings.Contains(replyRes.HTML, "stale @") {
			t.Errorf("expected renderThreadCard HTML to mark Side=L /COMMIT_MSG stale against c3.Parent, got:\n%s", replyRes.HTML)
		}

		diffParentHTML := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s/f/COMMIT_MSG", ts.URL, rec.Slug, c3.Key))
		if strings.Contains(diffParentHTML, fmt.Sprintf(`id="thread-%d"`, thLeft.ID)) {
			t.Errorf("expected Snapshot 1 /COMMIT_MSG thread #%d hidden inline in parent..live when Snapshot 2 is live", thLeft.ID)
		}

		diffSnap1HTML := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s/f/COMMIT_MSG?base=parent&target=snap:1", ts.URL, rec.Slug, c3.Key))
		if !strings.Contains(diffSnap1HTML, fmt.Sprintf(`id="thread-%d"`, thLeft.ID)) || strings.Contains(diffSnap1HTML, "stale @") {
			t.Errorf("expected Snapshot 1 /COMMIT_MSG thread #%d non-stale inline in parent..snap:1", thLeft.ID)
		}

		diffInterHTML := fetchHTML(fmt.Sprintf("%s/r/%s/c/%s/f/COMMIT_MSG?base=snap:1&target=snap:2", ts.URL, rec.Slug, c3.Key))
		if !strings.Contains(diffInterHTML, fmt.Sprintf(`id="thread-%d"`, thLeft.ID)) || strings.Contains(diffInterHTML, "stale @") {
			t.Errorf("expected Snapshot 1 Side=L /COMMIT_MSG thread #%d non-stale inline in snap:1..snap:2", thLeft.ID)
		}
	})

	t.Run("JS_LGTMAndCommitMsgThreadLoc", func(t *testing.T) {
		nodeBin, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node not available")
		}
		script := `
const fs = require("fs");
const vm = require("vm");

function makeEl(tag, cls) {
  const attrs = {};
  const el = {
    tagName: tag.toUpperCase(),
    className: cls || "",
    textContent: "",
    innerHTML: "",
    style: {},
    children: [],
    childNodes: [],
    classList: {
      add(...cs) { cs.forEach(c => { if (!el.className.split(/\s+/).includes(c)) el.className = (el.className + " " + c).trim(); }); },
      remove(...cs) { el.className = el.className.split(/\s+/).filter(x => x && !cs.includes(x)).join(" "); },
      contains(c) { return el.className.split(/\s+/).includes(c); }
    },
    setAttribute(k, v) { attrs[k] = String(v); },
    getAttribute(k) { return Object.prototype.hasOwnProperty.call(attrs, k) ? attrs[k] : null; },
    removeAttribute(k) { delete attrs[k]; },
    appendChild(c) { c.parentNode = el; el.children.push(c); el.childNodes.push(c); return c; },
    contains(target) { return target === el || el.children.some(c => c.contains && c.contains(target)); },
    getBoundingClientRect() { return { left: 10, right: 100, top: 10, bottom: 30, width: 90, height: 20 }; },
    closest(sel) {
      let cur = el;
      while (cur) {
        if (sel.startsWith("#") && cur.id === sel.slice(1)) return cur;
        if (sel.startsWith(".") && cur.classList && cur.classList.contains(sel.slice(1))) return cur;
        cur = cur.parentNode;
      }
      return null;
    },
    addEventListener() {},
    removeEventListener() {},
    querySelectorAll() { return []; },
    querySelector() { return null; }
  };
  return el;
}

const body = makeEl("body");
const btnListCommit = makeEl("button", "btn btn-sm file-mark-reviewed-btn");
btnListCommit.setAttribute("data-reviewed-btn", "COMMIT_MSG");
btnListCommit.textContent = "Mark Reviewed";
const btnAllCommit = makeEl("button", "btn btn-sm file-reviewed-btn-0");
btnAllCommit.setAttribute("data-reviewed-btn", "COMMIT_MSG");
btnAllCommit.textContent = "Mark Reviewed";
const iconCommit = makeEl("span", "file-reviewed-icon");
iconCommit.setAttribute("data-reviewed-icon", "COMMIT_MSG");
iconCommit.style.display = "none";

const lgtmBtn = makeEl("button", "btn btn-sm btn-green");
lgtmBtn.id = "lgtmBtn";
const commitStatusBadge = makeEl("span", "badge badge-status-active");
commitStatusBadge.id = "commitStatusBadge";

const countsContainer = makeEl("span", "comment-counts");
countsContainer.id = "changeCommentCounts";
countsContainer.setAttribute("data-repo", "r1");
countsContainer.setAttribute("data-change", "c1");
countsContainer.setAttribute("data-base", "parent");
countsContainer.setAttribute("data-target", "live");
const unresTrigger = makeEl("span", "badge badge-count-unresolved threads-menu-trigger");
unresTrigger.setAttribute("data-status", "unresolved");
countsContainer.appendChild(unresTrigger);

const doc = {
  body: body,
  documentElement: {
    clientWidth: 1200,
    clientHeight: 800,
    classList: { contains() { return false; }, add() {}, remove() {} },
    style: { setProperty() {}, getPropertyValue() { return ""; } },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {}
  },
  createElement(tag) {
    return makeEl(tag);
  },
  createTextNode(txt) {
    return { nodeType: 3, textContent: txt, parentNode: null };
  },
  getElementById(id) {
    if (id === "lgtmBtn") return lgtmBtn;
    if (id === "commitStatusBadge") return commitStatusBadge;
    if (id === "changeCommentCounts") return countsContainer;
    if (id === "unresolvedThreadsMenu") return body.children.find(c => c.id === "unresolvedThreadsMenu") || null;
    return null;
  },
  querySelector(sel) {
    if (sel.includes("#changeCommentCounts")) return unresTrigger;
    return null;
  },
  querySelectorAll(sel) {
    if (sel === "button[data-reviewed-btn]") return [btnListCommit, btnAllCommit];
    if (sel === "[data-reviewed-icon]") return [iconCommit];
    return [];
  },
  addEventListener() {},
  removeEventListener() {}
};

const win = {
  document: doc,
  location: { search: "", hash: "", href: "" },
  addEventListener() {},
  removeEventListener() {},
  scrollX: 0, scrollY: 0, innerWidth: 1200, innerHeight: 800,
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; },
  fetch(url) {
    if (url.includes("/api/changes/reviewed")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ status: "ok", reviewed: true, lgtm: true })
      });
    }
    if (url.includes("/api/threads/list")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          threads: [
            { id: 1, path: "COMMIT_MSG", line: 0, author: "Alice", preview: "file comment" },
            { id: 2, path: "COMMIT_MSG", line: 4, end_line: 6, author: "Bob", preview: "line comment" }
          ]
        })
      });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  fetch: win.fetch,
  NodeFilter: { SHOW_TEXT: 4 },
  Node: { TEXT_NODE: 3, ELEMENT_NODE: 1 }
});
vm.runInContext(fs.readFileSync("static/app.js", "utf8"), ctx);

(async () => {
  win.toggleChangeRevMark("r1", "c1", "rev1", "lgtm", true);
  await new Promise(r => setTimeout(r, 20));
  if (!btnListCommit.classList.contains("btn-active") || btnListCommit.textContent !== "Mark Unreviewed") {
    throw new Error("expected File List COMMIT_MSG button updated on LGTM, got: " + btnListCommit.textContent);
  }
  if (!btnAllCommit.classList.contains("btn-active") || btnAllCommit.textContent !== "Reviewed ✓") {
    throw new Error("expected All Diffs COMMIT_MSG button updated on LGTM, got: " + btnAllCommit.textContent);
  }
  if (iconCommit.style.display !== "inline-flex") {
    throw new Error("expected COMMIT_MSG reviewed icon visible on LGTM, got: " + iconCommit.style.display);
  }

  win.toggleUnresolvedMenu(unresTrigger);
  await new Promise(r => setTimeout(r, 20));
  const menu = body.children.find(c => c.className && c.className.includes("unresolved-threads-menu"));
  if (!menu) throw new Error("expected unresolved-threads-menu");
  if (!menu.innerHTML.includes(">COMMIT_MSG<")) {
    throw new Error("expected file-level COMMIT_MSG thread loc without line number, got: " + menu.innerHTML);
  }
  if (!menu.innerHTML.includes(">COMMIT_MSG:L4–6<")) {
    throw new Error("expected line comment on COMMIT_MSG to format as COMMIT_MSG:L4–6, got: " + menu.innerHTML);
  }
})().catch(e => { console.error(e); process.exit(1); });
`
		cmd := exec.Command(nodeBin, "-e", script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("JS check failed: %v\n%s", err, string(out))
		}
	})
}

func TestCopyHostHeaderLink(t *testing.T) {
	testCases := []struct {
		name       string
		listenAddr string
		expected   string
	}{
		{
			name:       "hostname specified",
			listenAddr: "devbox.example.com:8080",
			expected:   `data-copy-host="devbox.example.com:8080"`,
		},
		{
			name:       "wildcard port only",
			listenAddr: ":8080",
			expected:   `data-copy-host=""`,
		},
		{
			name:       "empty listen address",
			listenAddr: "",
			expected:   `data-copy-host=""`,
		},
		{
			name:       "bind all IPv4",
			listenAddr: "0.0.0.0:8080",
			expected:   `data-copy-host=""`,
		},
		{
			name:       "bind all IPv6",
			listenAddr: "[::]:8080",
			expected:   `data-copy-host=""`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "review.db")
			d, err := db.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			cache := vcs.NewCache()
			srv, err := NewServer(d, cache, tc.listenAddr)
			if err != nil {
				t.Fatal(err)
			}

			ts := httptest.NewServer(srv.httpServer.Handler)
			defer ts.Close()

			resp, err := http.Get(ts.URL + "/")
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("GET / failed: %v", err)
			}
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(resp.Body)
			_ = resp.Body.Close()

			body := buf.String()
			if !strings.Contains(body, tc.expected) {
				t.Errorf("expected dashboard HTML to contain %q, but it was not found:\n%s", tc.expected, body)
			}
			if !strings.Contains(body, `id="copyLinkBtn"`) {
				t.Errorf("expected dashboard HTML to contain id=\"copyLinkBtn\"")
			}
		})
	}
}

func TestServerLazyDiffFileAndEagerBudget(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.name", "UI Tester")
	run("config", "user.email", "ui@example.com")

	if err := os.WriteFile(filepath.Join(repoDir, "init.txt"), []byte("init\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "init.txt")
	run("commit", "-m", "Initial commit")

	// Create 20 files with 80 lines each (total 1600 lines)
	for i := 0; i < 20; i++ {
		var content bytes.Buffer
		for l := 1; l <= 80; l++ {
			fmt.Fprintf(&content, "file %d line %d\n", i, l)
		}
		if err := os.WriteFile(filepath.Join(repoDir, fmt.Sprintf("file_%02d.txt", i)), content.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
		run("add", fmt.Sprintf("file_%02d.txt", i))
	}
	run("commit", "-m", "Add 20 large files")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.broker.Close()

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	repo, err := vcs.Open(rec.RootPath, cache)
	if err != nil {
		t.Fatal(err)
	}
	changes, _ := repo.Changes()
	c := changes[0]

	url := fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, c.Rev)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET change diffs=all failed: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	body := buf.String()

	// Later files should be lazy-loaded with data-lazy-url
	if !strings.Contains(body, `data-lazy-url="/api/diff/file?repo=`) {
		t.Fatalf("expected page to contain lazy-loaded files with data-lazy-url")
	}

	// Fetch /api/diff/file for one of the lazy files
	apiURL := fmt.Sprintf("%s/api/diff/file?repo=%s&change=%s&path=file_18.txt&index=18&base=%s&target=%s&mode=split&ctx=3",
		ts.URL, rec.Slug, c.Key, c.Parent, c.Rev)
	apiResp, err := http.Get(apiURL)
	if err != nil || apiResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/diff/file failed: %v", err)
	}
	var apiBuf bytes.Buffer
	_, _ = apiBuf.ReadFrom(apiResp.Body)
	_ = apiResp.Body.Close()
	apiBody := apiBuf.String()

	if !strings.Contains(apiBody, `class="diff-container"`) {
		t.Errorf("expected /api/diff/file to return rendered diff-container HTML, got:\n%s", apiBody)
	}
	if !strings.Contains(apiBody, `file 18 line 1`) {
		t.Errorf("expected /api/diff/file to contain diff lines from file 18")
	}
}

func TestServerGzipCompression(t *testing.T) {
	srv, ts, d, _ := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	// 1. Request with Accept-Encoding: gzip on standard page
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()

	if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Errorf("expected Content-Encoding: gzip, got %q", ce)
	}

	gzReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader failed: %v", err)
	}
	defer gzReader.Close()
	uncompressed, err := io.ReadAll(gzReader)
	if err != nil {
		t.Fatalf("reading uncompressed body failed: %v", err)
	}
	if !strings.Contains(string(uncompressed), `Code Review`) {
		t.Errorf("expected decompressed HTML to contain 'Code Review'")
	}

	// 2. Request without Accept-Encoding
	reqNoGz, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	respNoGz, err := http.DefaultClient.Do(reqNoGz)
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer respNoGz.Body.Close()
	if ce := respNoGz.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("expected no Content-Encoding without Accept-Encoding, got %q", ce)
	}

	// 3. Request /api/events with Accept-Encoding: gzip (must NOT be gzipped)
	reqSSE, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	reqSSE.Header.Set("Accept-Encoding", "gzip")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	reqSSE = reqSSE.WithContext(ctx)

	respSSE, err := http.DefaultClient.Do(reqSSE)
	if err == nil {
		defer respSSE.Body.Close()
		if ce := respSSE.Header.Get("Content-Encoding"); ce == "gzip" {
			t.Errorf("SSE /api/events must NEVER be compressed with gzip")
		}
		if ct := respSSE.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			t.Errorf("expected text/event-stream, got %q", ct)
		}
	}
}

func TestServerUnresolvedBadges(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "hello2.go"), []byte("package main\n\nfunc foo() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "hello2.go")
	run("commit", "-m", "Second commit\n\nChange-Id: Ibbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	repo, _ := vcs.Open(repoDir, srv.cache)
	changes, _ := repo.Changes()
	c := changes[0]

	// Add unresolved thread
	_, _, err := engine.AddComment(d, repo, rec.ID, c, c.Rev, "hello2.go", "right", 1, 1, "Alice", "Needs fix", true, false)
	if err != nil {
		t.Fatalf("AddComment failed: %v", err)
	}

	// 1. Dashboard
	respDash, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	dashBody, _ := io.ReadAll(respDash.Body)
	respDash.Body.Close()
	if !strings.Contains(string(dashBody), `badge badge-count-unresolved">1 unresolved</span>`) {
		t.Errorf("expected dashboard to contain unresolved badge with badge-count-unresolved class")
	}

	// 2. Repo page
	respRepo, err := http.Get(fmt.Sprintf("%s/r/%s", ts.URL, rec.Slug))
	if err != nil {
		t.Fatal(err)
	}
	repoBody, _ := io.ReadAll(respRepo.Body)
	respRepo.Body.Close()
	if !strings.Contains(string(repoBody), `badge badge-count-unresolved">1 unresolved</span>`) {
		t.Errorf("expected repo page to contain unresolved badge with badge-count-unresolved class")
	}

	// 3. Change page (relation chain badge)
	respChange, err := http.Get(fmt.Sprintf("%s/r/%s/c/%s", ts.URL, rec.Slug, c.Rev))
	if err != nil {
		t.Fatal(err)
	}
	changeBody, _ := io.ReadAll(respChange.Body)
	respChange.Body.Close()
	if !strings.Contains(string(changeBody), `badge badge-count-unresolved" style="font-size: 10px; padding: 1px 4px;">1</span>`) {
		t.Errorf("expected relation chain in change view to contain badge-count-unresolved, body:\n%s", string(changeBody))
	}
}

func TestSSEBrokerClientCap(t *testing.T) {
	dPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	cache := vcs.NewCache()
	broker := NewSSEBroker(d, cache)
	defer broker.Close()

	// Spin up 70 SSE requests sequentially
	var cancels []context.CancelFunc
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	for i := 0; i < 70; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/api/events", nil)
		rr := httptest.NewRecorder()
		go broker.ServeHTTP(rr, req)
	}

	// Wait briefly for broker registration
	time.Sleep(100 * time.Millisecond)

	broker.mu.Lock()
	numClients := len(broker.clients)
	orderLen := len(broker.clientOrder)
	broker.mu.Unlock()

	if numClients > 64 {
		t.Errorf("expected at most 64 clients connected, got %d", numClients)
	}
	if orderLen > 64 {
		t.Errorf("expected clientOrder <= 64, got %d", orderLen)
	}
}

func TestServerIdenticalSnapshotComparisons(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)
	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// Take Snapshot 1
	pSnap, _ := json.Marshal(map[string]any{
		"repo":   rec.Slug,
		"change": c.Key,
	})
	resp, err := http.Post(ts.URL+"/api/snapshot", "application/json", bytes.NewReader(pSnap))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/snapshot failed: %v", err)
	}
	_ = resp.Body.Close()

	// 1. Compare identical snapshot: base=snap:1&target=live (since no edits occurred, target == base)
	// Test diffs=list
	urlList := fmt.Sprintf("%s/r/%s/c/%s?diffs=list&base=snap:1&target=live", ts.URL, rec.Slug, c.Rev)
	resp, err = http.Get(urlList)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s failed: %v", urlList, err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	bodyList := string(bodyBytes)

	if strings.Contains(bodyList, "class=\"file-row\"") || strings.Contains(bodyList, "<span>/COMMIT_MSG</span>") {
		t.Errorf("expected no COMMIT_MSG file row in file list when snapshots are identical, body:\n%s", bodyList)
	}
	if !strings.Contains(bodyList, "No diffs found!") {
		t.Errorf("expected 'No diffs found!' in file list view when snapshots are identical")
	}

	// Test diffs=all
	urlAll := fmt.Sprintf("%s/r/%s/c/%s?diffs=all&base=snap:1&target=live", ts.URL, rec.Slug, c.Rev)
	resp, err = http.Get(urlAll)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s failed: %v", urlAll, err)
	}
	bodyBytes, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	bodyAll := string(bodyBytes)

	if strings.Contains(bodyAll, "class=\"diff-file-section\"") || strings.Contains(bodyAll, "diff-file-name\">/COMMIT_MSG<") {
		t.Errorf("expected no COMMIT_MSG diff section in all diffs when snapshots are identical, body:\n%s", bodyAll)
	}
	if !strings.Contains(bodyAll, "No diffs found!") {
		t.Errorf("expected 'No diffs found!' in all diffs view when snapshots are identical")
	}

	// 2. Amend commit message only (no file changes) and take Snapshot 2
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}
	runGit("commit", "--amend", "-m", "Updated commit message\n\nChange-Id: Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	resp, err = http.Post(ts.URL+"/api/snapshot", "application/json", bytes.NewReader(pSnap))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST second /api/snapshot failed: %v", err)
	}
	_ = resp.Body.Close()

	// Compare base=snap:1&target=snap:2 where commit message DID change
	urlChanged := fmt.Sprintf("%s/r/%s/c/%s?diffs=list&base=snap:1&target=snap:2", ts.URL, rec.Slug, c.Key)
	resp, err = http.Get(urlChanged)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s failed: %v", urlChanged, err)
	}
	bodyBytes, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	bodyChanged := string(bodyBytes)

	if !strings.Contains(bodyChanged, "<span>/COMMIT_MSG</span>") {
		t.Errorf("expected <span>/COMMIT_MSG</span> in file list when commit message changed between snapshots, body:\n%s", bodyChanged)
	}
}

func TestFallbackRouting(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, err := d.RepoBySlug(repoDir)
	if err != nil || rec == nil {
		t.Fatalf("RepoBySlug failed: %v", err)
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 1. /r/<slug>/ should redirect to /r/<slug>
	resp, err := client.Get(ts.URL + "/r/" + rec.Slug + "/")
	if err != nil {
		t.Fatalf("GET /r/%s/ failed: %v", rec.Slug, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("expected 302 redirect for trailing slash, got %d", resp.StatusCode)
	}
	expectedLoc := "/r/" + rec.Slug
	if loc := resp.Header.Get("Location"); loc != expectedLoc {
		t.Errorf("expected Location %q, got %q", expectedLoc, loc)
	}

	// 2. Unknown route should return 404 Not Found
	resp, err = client.Get(ts.URL + "/unknown-invalid-route-xyz")
	if err != nil {
		t.Fatalf("GET /unknown-invalid-route-xyz failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 Not Found for unknown route, got %d", resp.StatusCode)
	}
}

func TestUnchangedFileDiffNotCollapsed(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")
	run("config", "user.name", "Tester")
	run("config", "user.email", "test@example.com")

	// Commit 1: add unchanged.go and other.go
	unchangedCode := "package main\n\nfunc UnchangedFunction() int {\n\treturn 42\n}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "unchanged.go"), []byte(unchangedCode), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "other.go"), []byte("package main\n\nfunc Other() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "Initial commit\n\nChange-Id: I1111111111111111111111111111111111111111")

	// Commit 2: modify other.go ONLY, leaving unchanged.go completely untouched
	if err := os.WriteFile(filepath.Join(repoDir, "other.go"), []byte("package main\n\nfunc Other() string { return \"changed\" }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "other.go")
	run("commit", "-m", "Modify other.go\n\nChange-Id: I2222222222222222222222222222222222222222")
	c2Hash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	// 1. GET without ?ctx=all
	resp, err := http.Get(fmt.Sprintf("%s/r/%s/c/%s/f/unchanged.go", ts.URL, rec.Slug, c2Hash))
	if err != nil {
		t.Fatalf("GET unchanged.go failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	bodyStr := string(body)
	if strings.Contains(bodyStr, "row-skip") || strings.Contains(bodyStr, "common lines") {
		t.Errorf("unchanged file without ?ctx=all should not be collapsed into row-skip: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "UnchangedFunction") {
		t.Errorf("expected code content UnchangedFunction in body: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "data-line-r=\"1\"") && !strings.Contains(bodyStr, "data-line-l=\"1\"") {
		t.Errorf("expected line number rows in body: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "unchanged") {
		t.Errorf("expected 'unchanged' badge in breadcrumbs: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "diff-table-single") {
		t.Errorf("expected HTML to contain diff-table-single class for unchanged file")
	}
	if !strings.Contains(bodyStr, `<span class="badge" style="margin-left: 6px;">unchanged</span>`) {
		t.Errorf("expected HTML to contain unchanged badge")
	}
	if strings.Contains(bodyStr, "Split View") {
		t.Errorf("expected Split View button NOT to be rendered for unchanged file")
	}

	// 2. GET with ?ctx=all
	respAll, err := http.Get(fmt.Sprintf("%s/r/%s/c/%s/f/unchanged.go?ctx=all", ts.URL, rec.Slug, c2Hash))
	if err != nil {
		t.Fatalf("GET unchanged.go?ctx=all failed: %v", err)
	}
	bodyAll, _ := io.ReadAll(respAll.Body)
	_ = respAll.Body.Close()

	if respAll.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for ?ctx=all, got %d", respAll.StatusCode)
	}
	bodyAllStr := string(bodyAll)
	if strings.Contains(bodyAllStr, "row-skip") || strings.Contains(bodyAllStr, "common lines") {
		t.Errorf("unchanged file with ?ctx=all should not be collapsed into row-skip: %s", bodyAllStr)
	}
	if !strings.Contains(bodyAllStr, "UnchangedFunction") {
		t.Errorf("expected code content UnchangedFunction in ?ctx=all body: %s", bodyAllStr)
	}
}

func TestAPISymbol(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")
	run("config", "user.name", "Symbol Tester")
	run("config", "user.email", "symbol@example.com")

	goMod := "module testmod\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	utilGo := `package pkg

// HelperFunction performs a critical calculation.
func HelperFunction(x int) int {
	return x * 2
}
`
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "util.go"), []byte(utilGo), 0644); err != nil {
		t.Fatal(err)
	}

	mainGo := `package main

import (
	"fmt"
	"testmod/pkg"
)

func main() {
	res := pkg.HelperFunction(21)
	fmt.Println(res)
}
`
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}

	run("add", ".")
	run("commit", "-m", "Initial commit\n\nChange-Id: I1111111111111111111111111111111111111111")
	commitHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	// 1. Valid Go symbol lookup on main.go line 9 (HelperFunction)
	url := fmt.Sprintf("%s/api/symbol?repo=%s&change=%s&path=main.go&side=R&line=9&col=15", ts.URL, rec.Slug, commitHash)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /api/symbol failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	var data struct {
		Found            bool   `json:"found"`
		SymbolName       string `json:"symbol_name"`
		Signature        string `json:"signature"`
		SignatureHTML    string `json:"signature_html"`
		InRepo           bool   `json:"in_repo"`
		DefPath          string `json:"def_path"`
		DisplayPath      string `json:"display_path"`
		DefLine          int    `json:"def_line"`
		SnippetTruncated bool   `json:"snippet_truncated"`
		Snippet          []struct {
			Line   int    `json:"line"`
			HTML   string `json:"html"`
			Active bool   `json:"active"`
		} `json:"snippet"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if !data.Found {
		t.Fatalf("expected found=true for HelperFunction")
	}
	if !strings.Contains(data.SymbolName, "HelperFunction") {
		t.Errorf("expected SymbolName to contain HelperFunction, got %q", data.SymbolName)
	}
	if !strings.Contains(data.Signature, "HelperFunction") {
		t.Errorf("expected Signature to contain HelperFunction, got %q", data.Signature)
	}
	if !data.InRepo {
		t.Errorf("expected in_repo=true")
	}
	if data.DefPath != "pkg/util.go" {
		t.Errorf("expected def_path pkg/util.go, got %q", data.DefPath)
	}
	if data.DefLine != 4 {
		t.Errorf("expected def_line 4, got %d", data.DefLine)
	}
	if len(data.Snippet) < 4 || data.Snippet[3].Line != 4 || !data.Snippet[3].Active || !strings.Contains(data.Snippet[3].HTML, "HelperFunction") {
		t.Errorf("expected active snippet line 4 containing HelperFunction, got %+v", data.Snippet)
	}

	// 2. Go import symbol lookup on main.go line 4 col 2 ("fmt")
	urlFmt := fmt.Sprintf("%s/api/symbol?repo=%s&change=%s&path=main.go&side=R&line=4&col=2", ts.URL, rec.Slug, commitHash)
	respFmt, err := http.Get(urlFmt)
	if err != nil {
		t.Fatalf("GET /api/symbol import fmt failed: %v", err)
	}
	defer respFmt.Body.Close()
	if respFmt.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for import fmt, got %d", respFmt.StatusCode)
	}
	var fmtData struct {
		Found      bool   `json:"found"`
		SymbolName string `json:"symbol_name"`
		CanRefs    bool   `json:"can_refs"`
		CanImpls   bool   `json:"can_impls"`
	}
	if err := json.NewDecoder(respFmt.Body).Decode(&fmtData); err != nil {
		t.Fatalf("failed to decode JSON response for import fmt: %v", err)
	}
	if !fmtData.Found || fmtData.SymbolName != "fmt" || !fmtData.CanRefs || fmtData.CanImpls {
		t.Errorf("expected Found=true, SymbolName=fmt, CanRefs=true, CanImpls=false, got %+v", fmtData)
	}

	// 3. Non-Go file should return found=false
	respNonGo, err := http.Get(fmt.Sprintf("%s/api/symbol?repo=%s&path=README.md", ts.URL, rec.Slug))
	if err != nil {
		t.Fatalf("GET /api/symbol non-go failed: %v", err)
	}
	defer respNonGo.Body.Close()
	if respNonGo.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for non-go path, got %d", respNonGo.StatusCode)
	}
	var nonGoData struct {
		Found bool `json:"found"`
	}
	if err := json.NewDecoder(respNonGo.Body).Decode(&nonGoData); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if nonGoData.Found {
		t.Errorf("expected found=false for non-go file")
	}

	// 3. Directory traversal path should return found=false
	respTraversal, err := http.Get(fmt.Sprintf("%s/api/symbol?repo=%s&path=../outside.go", ts.URL, rec.Slug))
	if err != nil {
		t.Fatalf("GET /api/symbol traversal failed: %v", err)
	}
	defer respTraversal.Body.Close()
	var travData struct {
		Found bool `json:"found"`
	}
	if err := json.NewDecoder(respTraversal.Body).Decode(&travData); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if travData.Found {
		t.Errorf("expected found=false for traversal path")
	}

	// 4. Unknown repo should return 404
	respUnknown, err := http.Get(fmt.Sprintf("%s/api/symbol?repo=unknown-repo&path=main.go", ts.URL))
	if err != nil {
		t.Fatalf("GET /api/symbol unknown repo failed: %v", err)
	}
	defer respUnknown.Body.Close()
	if respUnknown.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown repo, got %d", respUnknown.StatusCode)
	}

	// 5. Chained lookup: looking up a symbol inside stdlib net/http/header.go
	respChained, err := http.Get(fmt.Sprintf("%s/api/symbol?repo=%s&pkg=net/http&pkg_file=header.go&line=38&col=13", ts.URL, rec.Slug))
	if err != nil {
		t.Fatalf("GET /api/symbol chained failed: %v", err)
	}
	defer respChained.Body.Close()
	if respChained.StatusCode == http.StatusOK {
		var chainedData struct {
			Found       bool   `json:"found"`
			SymbolName  string `json:"symbol_name"`
			PkgGoDevURL string `json:"pkg_go_dev_url"`
			DefPkg      string `json:"def_pkg"`
			DefFile     string `json:"def_file"`
		}
		if err := json.NewDecoder(respChained.Body).Decode(&chainedData); err == nil && chainedData.Found {
			if !strings.Contains(chainedData.SymbolName, "MIMEHeader") {
				t.Errorf("expected MIMEHeader, got %q", chainedData.SymbolName)
			}
			if !strings.Contains(chainedData.PkgGoDevURL, "net/textproto#MIMEHeader") {
				t.Errorf("expected textproto#MIMEHeader pkg.go.dev URL, got %q", chainedData.PkgGoDevURL)
			}
		}
	}

	// 6. Path-traversal in pkg_file should return found=false
	respTravPkg, err := http.Get(fmt.Sprintf("%s/api/symbol?repo=%s&pkg=net/http&pkg_file=../header.go&line=38&col=13", ts.URL, rec.Slug))
	if err != nil {
		t.Fatalf("GET /api/symbol traversal pkg failed: %v", err)
	}
	defer respTravPkg.Body.Close()
	var travPkgData struct {
		Found bool `json:"found"`
	}
	if err := json.NewDecoder(respTravPkg.Body).Decode(&travPkgData); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if travPkgData.Found {
		t.Errorf("expected found=false for path traversal in pkg_file")
	}

	// 7. Package clause lookup on main.go line 1 col 9 (package main)
	respPkgClause, err := http.Get(fmt.Sprintf("%s/api/symbol?repo=%s&change=%s&path=main.go&side=R&line=1&col=9", ts.URL, rec.Slug, commitHash))
	if err != nil {
		t.Fatalf("GET /api/symbol package clause failed: %v", err)
	}
	defer respPkgClause.Body.Close()
	var pkgClauseData struct {
		Found      bool   `json:"found"`
		SymbolName string `json:"symbol_name"`
		CanRefs    bool   `json:"can_refs"`
		CanImpls   bool   `json:"can_impls"`
	}
	if err := json.NewDecoder(respPkgClause.Body).Decode(&pkgClauseData); err != nil {
		t.Fatalf("decode package clause failed: %v", err)
	}
	if !pkgClauseData.Found {
		t.Errorf("expected found=true for package clause")
	}
	if pkgClauseData.SymbolName != "main" {
		t.Errorf("expected SymbolName main, got %q", pkgClauseData.SymbolName)
	}
	if !pkgClauseData.CanRefs {
		t.Errorf("expected can_refs=true for package clause")
	}
	if pkgClauseData.CanImpls {
		t.Errorf("expected can_impls=false for package clause")
	}
}

func TestAPISymbolRefs(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
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
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")

	goMod := "module example.com/refstest\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	pkgDir := filepath.Join(repoDir, "pkg")
	_ = os.MkdirAll(pkgDir, 0755)
	utilGo := `package pkg

// HelperFunction provides utility.
func HelperFunction() string {
	return "ok"
}
`
	_ = os.WriteFile(filepath.Join(pkgDir, "util.go"), []byte(utilGo), 0644)

	mainGo := `package main

import (
	"fmt"
	"example.com/refstest/pkg"
)

func main() {
	msg := pkg.HelperFunction()
	fmt.Println(msg)
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(mainGo), 0644)

	run("add", ".")
	run("commit", "-m", "init")
	commitHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	// 1. Valid Go symbol reference lookup on main.go line 9 col 15 (HelperFunction)
	url := fmt.Sprintf("%s/api/symbol/refs?repo=%s&change=%s&path=main.go&side=R&line=9&col=15", ts.URL, rec.Slug, commitHash)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /api/symbol/refs failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	var data struct {
		Found      bool   `json:"found"`
		SymbolName string `json:"symbol_name"`
		Signature  string `json:"signature"`
		Refs       []struct {
			Path     string `json:"path"`
			Display  string `json:"display"`
			InRepo   bool   `json:"in_repo"`
			Group    string `json:"group"`
			Line     int    `json:"line"`
			Col      int    `json:"col"`
			LineHTML string `json:"line_html"`
		} `json:"refs"`
		Truncated bool `json:"truncated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if !data.Found {
		t.Fatalf("expected found=true for HelperFunction refs")
	}
	if !strings.Contains(data.SymbolName, "HelperFunction") {
		t.Errorf("expected SymbolName to contain HelperFunction, got %q", data.SymbolName)
	}
	if len(data.Refs) == 0 {
		t.Fatalf("expected at least 1 reference site, got 0")
	}
	ref := data.Refs[0]
	if ref.Path != "main.go" {
		t.Errorf("ref.Path = %q, want main.go", ref.Path)
	}
	if ref.Line != 9 {
		t.Errorf("ref.Line = %d, want 9", ref.Line)
	}
	if !strings.Contains(ref.LineHTML, "HelperFunction") {
		t.Errorf("expected line_html to contain HelperFunction, got %q", ref.LineHTML)
	}

	// Verify that querying with only right_rev still resolves missing baseRev
	urlOnlyRight := fmt.Sprintf("%s/api/symbol/refs?repo=%s&change=%s&right_rev=%s&path=main.go&side=R&line=9&col=15", ts.URL, rec.Slug, commitHash, commitHash)
	respOnlyRight, err := http.Get(urlOnlyRight)
	if err != nil || respOnlyRight.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/symbol/refs with only right_rev failed: %v", err)
	}
	var dataOnlyRight struct {
		Found bool `json:"found"`
	}
	_ = json.NewDecoder(respOnlyRight.Body).Decode(&dataOnlyRight)
	respOnlyRight.Body.Close()
	if !dataOnlyRight.Found {
		t.Errorf("expected Found=true when querying with only right_rev")
	}

	// 2. Go import symbol references lookup on main.go line 4 col 2 ("fmt")
	urlFmtRefs := fmt.Sprintf("%s/api/symbol/refs?repo=%s&change=%s&path=main.go&side=R&line=4&col=2", ts.URL, rec.Slug, commitHash)
	respFmtRefs, err := http.Get(urlFmtRefs)
	if err != nil {
		t.Fatalf("GET /api/symbol/refs for fmt failed: %v", err)
	}
	defer respFmtRefs.Body.Close()
	if respFmtRefs.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for fmt refs, got %d", respFmtRefs.StatusCode)
	}
	var fmtRefsData struct {
		Found      bool   `json:"found"`
		SymbolName string `json:"symbol_name"`
		Refs       []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
		} `json:"refs"`
	}
	if err := json.NewDecoder(respFmtRefs.Body).Decode(&fmtRefsData); err != nil {
		t.Fatalf("failed to decode JSON response for fmt refs: %v", err)
	}
	if !fmtRefsData.Found || fmtRefsData.SymbolName != "fmt" {
		t.Errorf("expected Found=true, SymbolName=fmt for fmt refs, got %+v", fmtRefsData)
	}
	hasFmtUsage := false
	for _, r := range fmtRefsData.Refs {
		if r.Path == "main.go" && r.Line == 10 {
			hasFmtUsage = true
		}
	}
	if !hasFmtUsage {
		t.Errorf("expected fmt reference at main.go:10, got: %+v", fmtRefsData.Refs)
	}

	// 3. Non-Go file should return found=false
	respNonGo, err := http.Get(fmt.Sprintf("%s/api/symbol/refs?repo=%s&path=README.md", ts.URL, rec.Slug))
	if err != nil {
		t.Fatalf("GET /api/symbol/refs non-go failed: %v", err)
	}
	defer respNonGo.Body.Close()
	var nonGoData struct {
		Found bool `json:"found"`
	}
	_ = json.NewDecoder(respNonGo.Body).Decode(&nonGoData)
	if nonGoData.Found {
		t.Errorf("expected found=false for non-go file")
	}

	// 3. Unknown repo should return 404
	respUnknown, err := http.Get(fmt.Sprintf("%s/api/symbol/refs?repo=unknown-repo&path=main.go", ts.URL))
	if err != nil {
		t.Fatalf("GET /api/symbol/refs unknown repo failed: %v", err)
	}
	defer respUnknown.Body.Close()
	if respUnknown.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown repo, got %d", respUnknown.StatusCode)
	}

	// 4. Package clause refs on pkg/util.go line 1 col 9 (package pkg)
	urlPkgRefs := fmt.Sprintf("%s/api/symbol/refs?repo=%s&change=%s&path=pkg/util.go&side=R&line=1&col=9", ts.URL, rec.Slug, commitHash)
	respPkgRefs, err := http.Get(urlPkgRefs)
	if err != nil {
		t.Fatalf("GET /api/symbol/refs package clause failed: %v", err)
	}
	defer respPkgRefs.Body.Close()
	var pkgRefsData struct {
		Found bool `json:"found"`
		Refs  []struct {
			Path string `json:"path"`
		} `json:"refs"`
	}
	if err := json.NewDecoder(respPkgRefs.Body).Decode(&pkgRefsData); err != nil {
		t.Fatalf("decode package clause refs failed: %v", err)
	}
	if !pkgRefsData.Found {
		t.Errorf("expected found=true for package clause refs")
	}
	hasMainRef := false
	for _, r := range pkgRefsData.Refs {
		if r.Path == "main.go" {
			hasMainRef = true
			break
		}
	}
	if !hasMainRef {
		t.Errorf("expected main.go reference in package clause refs, got: %+v", pkgRefsData.Refs)
	}
}

func TestAPISymbolRefsBatch(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
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
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")

	goMod := "module example.com/refstest\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	pkgDir := filepath.Join(repoDir, "pkg")
	_ = os.MkdirAll(pkgDir, 0755)
	utilGo := `package pkg

// HelperFunction provides utility.
func HelperFunction() string {
	return "ok"
}

// AnotherHelper provides another utility.
func AnotherHelper() string {
	return "another"
}
`
	_ = os.WriteFile(filepath.Join(pkgDir, "util.go"), []byte(utilGo), 0644)

	mainGo := `package main

import (
	"fmt"
	"example.com/refstest/pkg"
)

func main() {
	msg := pkg.HelperFunction()
	msg2 := pkg.AnotherHelper()
	fmt.Println(msg, msg2)
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(mainGo), 0644)

	run("add", ".")
	run("commit", "-m", "init")
	commitHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	url := fmt.Sprintf("%s/api/symbol/refs/batch?repo=%s&change=%s", ts.URL, rec.Slug, commitHash)

	// 1. Valid batch with 2 requests: HelperFunction and AnotherHelper
	batchPayload := map[string]any{
		"repo":   rec.Slug,
		"change": commitHash,
		"side":   "R",
		"requests": []map[string]any{
			{
				"file": "main.go",
				"line": 9,
				"col":  15,
			},
			{
				"file": "main.go",
				"line": 10,
				"col":  15,
			},
		},
	}
	payloadBytes, _ := json.Marshal(batchPayload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(payloadBytes))
	if err != nil {
		t.Fatalf("POST /api/symbol/refs/batch failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(b))
	}

	var batchResp struct {
		Results []struct {
			Found      bool   `json:"found"`
			SymbolName string `json:"symbol_name"`
			Refs       []struct {
				Path string `json:"path"`
				Line int    `json:"line"`
			} `json:"refs"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if len(batchResp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(batchResp.Results))
	}
	if !batchResp.Results[0].Found || !strings.Contains(batchResp.Results[0].SymbolName, "HelperFunction") {
		t.Errorf("result 0: expected HelperFunction, got %+v", batchResp.Results[0])
	}
	if !batchResp.Results[1].Found || !strings.Contains(batchResp.Results[1].SymbolName, "AnotherHelper") {
		t.Errorf("result 1: expected AnotherHelper, got %+v", batchResp.Results[1])
	}

	// 2. Empty batch
	emptyPayload := map[string]any{
		"repo":     rec.Slug,
		"change":   commitHash,
		"requests": []any{},
	}
	emptyBytes, _ := json.Marshal(emptyPayload)
	emptyResp, err := http.Post(url, "application/json", bytes.NewReader(emptyBytes))
	if err != nil {
		t.Fatalf("POST empty batch failed: %v", err)
	}
	defer emptyResp.Body.Close()
	if emptyResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for empty batch, got %d", emptyResp.StatusCode)
	}
	var emptyData struct {
		Results []any `json:"results"`
	}
	if err := json.NewDecoder(emptyResp.Body).Decode(&emptyData); err != nil {
		t.Fatalf("decode empty results failed: %v", err)
	}
	if len(emptyData.Results) != 0 {
		t.Errorf("expected 0 empty results, got %d", len(emptyData.Results))
	}

	// 3. Fallback to query params when repo/change are omitted in JSON body
	queryOnlyPayload := map[string]any{
		"requests": []map[string]any{
			{
				"file": "main.go",
				"line": 9,
				"col":  15,
			},
		},
	}
	queryBytes, _ := json.Marshal(queryOnlyPayload)
	queryResp, err := http.Post(url, "application/json", bytes.NewReader(queryBytes))
	if err != nil {
		t.Fatalf("POST query-fallback batch failed: %v", err)
	}
	defer queryResp.Body.Close()
	if queryResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(queryResp.Body)
		t.Fatalf("expected 200 OK for query params fallback, got %d: %s", queryResp.StatusCode, string(b))
	}

	// 4. Missing repo/change
	noParamUrl := fmt.Sprintf("%s/api/symbol/refs/batch", ts.URL)
	noParamPayload := map[string]any{
		"requests": []map[string]any{
			{
				"file": "main.go",
				"line": 9,
				"col":  15,
			},
		},
	}
	noParamBytes, _ := json.Marshal(noParamPayload)
	noParamResp, err := http.Post(noParamUrl, "application/json", bytes.NewReader(noParamBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer noParamResp.Body.Close()
	if noParamResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 Not Found for missing repo, got %d", noParamResp.StatusCode)
	}

	// 5. Invalid JSON body
	invalidResp, err := http.Post(url, "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer invalidResp.Body.Close()
	if invalidResp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid JSON, got %d", invalidResp.StatusCode)
	}

	// 6. Batch cap exceeded: >256 requests returns 400 Bad Request
	largeRequests := make([]map[string]any, 257)
	for i := range largeRequests {
		largeRequests[i] = map[string]any{
			"file": "main.go",
			"line": 9,
			"col":  15,
		}
	}
	largePayload := map[string]any{
		"repo":     rec.Slug,
		"change":   commitHash,
		"requests": largeRequests,
	}
	largeBytes, _ := json.Marshal(largePayload)
	largeResp, err := http.Post(url, "application/json", bytes.NewReader(largeBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer largeResp.Body.Close()
	if largeResp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for batch > 256 items, got %d", largeResp.StatusCode)
	}

	// 7. Site as bool or int cleanly handled
	sitePayload := map[string]any{
		"repo":   rec.Slug,
		"change": commitHash,
		"requests": []map[string]any{
			{"file": "main.go", "line": 9, "col": 15, "site": true},
			{"file": "main.go", "line": 9, "col": 15, "site": 1},
			{"file": "main.go", "line": 9, "col": 15, "site": "true"},
		},
	}
	siteBytes, _ := json.Marshal(sitePayload)
	siteResp, err := http.Post(url, "application/json", bytes.NewReader(siteBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer siteResp.Body.Close()
	if siteResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for site bool/int payload, got %d", siteResp.StatusCode)
	}
}

func TestAPISymbolImpls(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
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
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")

	goMod := "module example.com/implstest\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	mainGo := `package main

type Greeter interface {
	Greet() string
}

type ConsoleGreeter struct{}

func (c *ConsoleGreeter) Greet() string {
	return "hello"
}

func main() {
	var g Greeter = &ConsoleGreeter{}
	_ = g.Greet()
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(mainGo), 0644)

	run("add", ".")
	run("commit", "-m", "init")
	commitHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	// 1. Valid implementation lookup on main.go line 3 col 5 (Greeter interface)
	symUrl := fmt.Sprintf("%s/api/symbol?repo=%s&change=%s&path=main.go&side=R&line=3&col=5", ts.URL, rec.Slug, commitHash)
	symResp, err := http.Get(symUrl)
	if err != nil {
		t.Fatalf("GET /api/symbol failed: %v", err)
	}
	defer symResp.Body.Close()
	var symData struct {
		Found          bool   `json:"found"`
		CanImpls       bool   `json:"can_impls"`
		ImplsDirection string `json:"impls_direction"`
	}
	_ = json.NewDecoder(symResp.Body).Decode(&symData)
	if !symData.Found || !symData.CanImpls {
		t.Errorf("expected found=true, can_impls=true for Greeter")
	}
	if symData.ImplsDirection != "implementations" {
		t.Errorf("expected impls_direction=implementations, got %q", symData.ImplsDirection)
	}

	url := fmt.Sprintf("%s/api/symbol/impls?repo=%s&change=%s&path=main.go&side=R&line=3&col=5", ts.URL, rec.Slug, commitHash)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /api/symbol/impls failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	var data struct {
		Found           bool   `json:"found"`
		SymbolName      string `json:"symbol_name"`
		Direction       string `json:"direction"`
		Implementations []struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			Display  string `json:"display"`
			InRepo   bool   `json:"in_repo"`
			Group    string `json:"group"`
			Line     int    `json:"line"`
			Col      int    `json:"col"`
			LineHTML string `json:"line_html"`
		} `json:"implementations"`
		Truncated bool `json:"truncated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if !data.Found {
		t.Fatalf("expected found=true for Greeter impls")
	}
	if data.Direction != "implementations" {
		t.Errorf("expected direction=implementations, got %q", data.Direction)
	}
	if len(data.Implementations) == 0 {
		t.Fatalf("expected at least 1 implementation, got 0")
	}
	impl := data.Implementations[0]
	if impl.Name != "*ConsoleGreeter" {
		t.Errorf("impl.Name = %q, want *ConsoleGreeter", impl.Name)
	}
	if impl.Path != "main.go" {
		t.Errorf("impl.Path = %q, want main.go", impl.Path)
	}
	if impl.Line != 7 {
		t.Errorf("impl.Line = %d, want 7", impl.Line)
	}
	if !impl.InRepo {
		t.Errorf("expected in_repo=true")
	}

	// 2. Non-Go file should return found=false
	respNonGo, err := http.Get(fmt.Sprintf("%s/api/symbol/impls?repo=%s&path=README.md", ts.URL, rec.Slug))
	if err != nil {
		t.Fatalf("GET /api/symbol/impls non-go failed: %v", err)
	}
	defer respNonGo.Body.Close()
	var nonGoData struct {
		Found bool `json:"found"`
	}
	_ = json.NewDecoder(respNonGo.Body).Decode(&nonGoData)
	if nonGoData.Found {
		t.Errorf("expected found=false for non-go file")
	}

	// 3. Unknown repo should return 404
	respUnknown, err := http.Get(fmt.Sprintf("%s/api/symbol/impls?repo=unknown-repo&path=main.go", ts.URL))
	if err != nil {
		t.Fatalf("GET /api/symbol/impls unknown repo failed: %v", err)
	}
	defer respUnknown.Body.Close()
	if respUnknown.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown repo, got %d", respUnknown.StatusCode)
	}
}

func TestEnclosingScopeContextUI(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
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
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")

	goMod := "module example.com/scopetest\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	serverGoInitial := `package main

type Server struct {
	port int
}

func (s *Server) ProcessRequest(id int) string {
	// padding line 8
	// padding line 9
	// padding line 10
	// padding line 11
	// padding line 12
	// padding line 13
	// padding line 14
	// padding line 15
	// padding line 16
	// padding line 17
	// padding line 18
	// padding line 19
	// padding line 20
	// padding line 21
	return "initial"
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "server.go"), []byte(serverGoInitial), 0644)
	run("add", ".")
	run("commit", "-m", "initial")

	serverGoModified := `package main

type Server struct {
	port int
}

func (s *Server) ProcessRequest(id int) string {
	// padding line 8
	// padding line 9
	// padding line 10
	// padding line 11
	// padding line 12
	// padding line 13
	// padding line 14
	// padding line 15
	// padding line 16
	// padding line 17
	// padding line 18
	// padding line 19
	// padding line 20
	// padding line 21
	return "modified"
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "server.go"), []byte(serverGoModified), 0644)
	run("add", "server.go")
	run("commit", "-m", "modify request processing")
	commitHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	// 1. Change view with diffs=all
	changeURL := fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, commitHash)
	resp, err := http.Get(changeURL)
	if err != nil {
		t.Fatalf("GET change view failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(bodyBytes))
	}
	html := string(bodyBytes)

	// Verify enclosing scope is in data-scopes JSON
	if !strings.Contains(html, "data-scopes=") {
		t.Fatalf("expected HTML to contain data-scopes attribute, got:\n%s", html)
	}
	if !strings.Contains(html, "ProcessRequest") {
		t.Errorf("expected data-scopes to contain ProcessRequest, got:\n%s", html)
	}

	// Verify diff-header-scope button exists in file header
	if !strings.Contains(html, "diff-header-scope") {
		t.Errorf("expected HTML to contain diff-header-scope button")
	}
	if !strings.Contains(html, `class="diff-file-name" title="server.go">server.go<`) {
		t.Errorf("expected change view to contain diff-file-name with title for server.go")
	}

	// 2. Single-file diff view
	fileURL := fmt.Sprintf("%s/r/%s/c/%s/f/server.go", ts.URL, rec.Slug, commitHash)
	respFile, err := http.Get(fileURL)
	if err != nil {
		t.Fatalf("GET file view failed: %v", err)
	}
	bodyFileBytes, _ := io.ReadAll(respFile.Body)
	respFile.Body.Close()

	if respFile.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for file view, got %d", respFile.StatusCode)
	}
	fileHtml := string(bodyFileBytes)
	if !strings.Contains(fileHtml, "data-scopes=") || !strings.Contains(fileHtml, "ProcessRequest") {
		t.Errorf("expected file view to contain data-scopes with ProcessRequest")
	}
	if !strings.Contains(fileHtml, "appHeaderScope") || !strings.Contains(fileHtml, "diff-header-scope") {
		t.Errorf("expected file view to contain appHeaderScope diff-header-scope")
	}
	if !strings.Contains(fileHtml, `class="breadcrumb-file" title="server.go">server.go<`) {
		t.Errorf("expected file view to contain breadcrumb-file with title for server.go")
	}

	// 3. /ext stdlib/external Go file view
	extURL := fmt.Sprintf("%s/r/%s/c/%s/ext?pkg=context&file=context.go", ts.URL, rec.Slug, commitHash)
	respExt, err := http.Get(extURL)
	if err != nil {
		t.Fatalf("GET ext view failed: %v", err)
	}
	bodyExtBytes, _ := io.ReadAll(respExt.Body)
	respExt.Body.Close()

	if respExt.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /ext, got %d: %s", respExt.StatusCode, string(bodyExtBytes))
	}
	extHtml := string(bodyExtBytes)
	if !strings.Contains(extHtml, "appHeaderScope") {
		t.Errorf("expected /ext HTML to contain appHeaderScope")
	}

	// 4. Verify style.css rules for non-overlapping headers
	respCSS, err := http.Get(ts.URL + "/static/style.css")
	if err != nil || respCSS.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/style.css failed: %v", err)
	}
	bodyCSS, _ := io.ReadAll(respCSS.Body)
	respCSS.Body.Close()
	cssStr := string(bodyCSS)
	if !strings.Contains(cssStr, ".breadcrumb-file") {
		t.Errorf("expected style.css to contain .breadcrumb-file")
	}
	if !strings.Contains(cssStr, "flex: 0 99999 auto;") {
		t.Errorf("expected style.css to contain flex: 0 99999 auto for .diff-header-scope")
	}
	if !strings.Contains(cssStr, ".diff-file-header.has-scope > .diff-header-scope") {
		t.Errorf("expected style.css to contain .diff-file-header.has-scope > .diff-header-scope")
	}
	if !strings.Contains(cssStr, "header.app-header.has-scope > .diff-header-scope") {
		t.Errorf("expected style.css to contain header.app-header.has-scope > .diff-header-scope")
	}
	if !strings.Contains(cssStr, "--skip-btn-bg") {
		t.Errorf("expected style.css to contain --skip-btn-bg")
	}
	if !strings.Contains(cssStr, "td.skip-gutter") || !strings.Contains(cssStr, ".diff-table-split .row-skip .skip-controls") {
		t.Errorf("expected style.css to contain td.skip-gutter and .diff-table-split .row-skip .skip-controls")
	}
	if !strings.Contains(cssStr, "color-scheme: dark") {
		t.Errorf("expected style.css to contain color-scheme: dark")
	}
	if !strings.Contains(cssStr, "--scrollbar-thumb") {
		t.Errorf("expected style.css to contain --scrollbar-thumb")
	}
	if !strings.Contains(cssStr, "#symbolPopup *::-webkit-scrollbar") {
		t.Errorf("expected style.css to contain #symbolPopup *::-webkit-scrollbar")
	}

	// 5. Node VM test verifying static/app.js behavior
	if nodeBin, err := exec.LookPath("node"); err == nil {
		script := `
const fs = require('fs');
const vm = require('vm');
const code = fs.readFileSync('static/app.js', 'utf8');

const barEl = {
  style: { display: 'none' },
  offsetWidth: 200,
  textContent: '',
  title: '',
  attributes: {},
  setAttribute(k, v) { this.attributes[k] = String(v); },
  getAttribute(k) { return this.attributes[k] || null; },
};

const fileHeaderEl = {
  classList: {
    classes: new Set(),
    add(c) { this.classes.add(c); },
    remove(c) { this.classes.delete(c); },
    contains(c) { return this.classes.has(c); },
  },
  getBoundingClientRect() { return { top: 48, bottom: 78 }; },
};

const fileSecEl = {
  classList: {
    contains(c) { return false; },
  },
  querySelector(sel) {
    if (sel === '.diff-file-header') return fileHeaderEl;
    if (sel === '.diff-header-scope') return barEl;
    return null;
  },
};

const row0 = {
  tagName: 'TR',
  classList: { contains(c) { return c === 'diff-row'; } },
  attributes: { 'data-line-r': '8' },
  getAttribute(a) { return this.attributes[a] || null; },
  getBoundingClientRect() { return { top: 80, bottom: 98 }; },
};

const tableEl = {
  getBoundingClientRect() { return { top: 80, bottom: 600 }; },
  querySelector(sel) { return null; },
  querySelectorAll(sel) {
    if (sel === 'tr.diff-row') return [row0];
    return [];
  },
};

const scopesJSON = JSON.stringify([{s: 7, e: 31, h: "func (s *Server) ProcessRequest(id int) string"}]);

const containerEl = {
  offsetParent: {},
  getAttribute(a) {
    if (a === 'data-scopes') return scopesJSON;
    if (a === 'data-scopes-side') return 'R';
    return null;
  },
  closest(sel) {
    if (sel === '.diff-file-section') return fileSecEl;
    return null;
  },
  querySelector(sel) {
    if (sel === '.diff-table') return tableEl;
    return null;
  },
};

const doc = {
  readyState: 'complete',
  documentElement: { getAttribute() { return ''; }, setAttribute() {}, removeAttribute() {} },
  addEventListener() {},
  getElementById(id) { return null; },
  querySelector(sel) {
    if (sel === 'header.app-header') return {
      classList: {
        classes: new Set(),
        add(c) { this.classes.add(c); },
        remove(c) { this.classes.delete(c); },
        contains(c) { return this.classes.has(c); },
      },
      getBoundingClientRect() { return { top: 0, bottom: 48 }; },
    };
    return null;
  },
  querySelectorAll(sel) {
    if (sel === '.diff-container[data-scopes]') return [containerEl];
    return [];
  },
};

const ctx = {
  document: doc,
  window: {
    location: { search: '', pathname: '/r/test/c/123' },
    addEventListener() {},
    setTimeout(cb) { cb(); },
    requestAnimationFrame(cb) { cb(); },
  },
  setTimeout(cb) { cb(); },
  clearTimeout() {},
  console: console,
};
ctx.window.document = doc;
vm.createContext(ctx);
vm.runInContext(code, ctx);

// Trigger sticky scopes update
ctx.window.updateStickyScopes ? ctx.window.updateStickyScopes() : null;

// Bar should now be visible and contain the function header
if (barEl.style.display !== 'inline-block') {
  throw new Error("expected barEl.style.display to be inline-block, got: " + barEl.style.display);
}
if (!barEl.textContent.includes("ProcessRequest")) {
  throw new Error("expected barEl.textContent to include ProcessRequest, got: " + barEl.textContent);
}
if (barEl.getAttribute("data-decl-line") !== "7") {
  throw new Error("expected data-decl-line 7, got: " + barEl.getAttribute("data-decl-line"));
}
if (!fileHeaderEl.classList.contains("has-scope")) {
  throw new Error("expected fileHeaderEl to have class has-scope when bar is visible");
}

// Test < 72px suppression
barEl.offsetWidth = 50;
ctx.window.updateStickyScopes();
if (barEl.style.display !== 'none') {
  throw new Error("expected barEl.style.display to be hidden when offsetWidth < 72, got: " + barEl.style.display);
}
if (fileHeaderEl.classList.contains("has-scope")) {
  throw new Error("expected fileHeaderEl to NOT have class has-scope when bar is suppressed");
}

console.log("NODE_OK");
`
		cmd := exec.Command(nodeBin, "-e", script)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Node VM test failed: %v\nOutput: %s", err, string(out))
		}
		if !strings.Contains(string(out), "NODE_OK") {
			t.Fatalf("Node VM test did not output NODE_OK: %s", string(out))
		}
	}
}

func TestStickyScopeSplitSideToggle(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
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
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")

	goMod := "module example.com/scopetest\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	processGoInitial := `package main

func OldProcess() {
	// line 4
	// line 5
	// line 6
	// line 7
	// line 8
	// line 9
}

func RemovedHelper() {
	// line 14
	// line 15
	// line 16
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "process.go"), []byte(processGoInitial), 0644)
	run("add", ".")
	run("commit", "-m", "initial")
	baseCommit := run("rev-parse", "HEAD")

	processGoModified := `package main

func NewProcess() {
	// line 4
	// line 5
	// line 6
	// line 7
	// line 8
	// line 9
}

func AddedHelper() {
	// line 14
	// line 15
	// line 16
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "process.go"), []byte(processGoModified), 0644)
	run("add", "process.go")
	run("commit", "-m", "modify processes")
	targetCommit := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	// 1. All-diffs view with mode=split
	changeURL := fmt.Sprintf("%s/r/%s/c/%s?diffs=all&mode=split&base=%s&target=%s", ts.URL, rec.Slug, targetCommit, baseCommit, targetCommit)
	resp, err := http.Get(changeURL)
	if err != nil {
		t.Fatalf("GET split change view failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for split change view, got %d", resp.StatusCode)
	}
	html := string(bodyBytes)

	if !strings.Contains(html, `data-diff-mode="split"`) {
		t.Errorf("expected HTML to contain data-diff-mode=\"split\"")
	}
	if !strings.Contains(html, "data-scopes-r=") || !strings.Contains(html, "NewProcess") || !strings.Contains(html, "AddedHelper") {
		t.Errorf("expected HTML to contain data-scopes-r with NewProcess and AddedHelper")
	}
	if !strings.Contains(html, "data-scopes-l=") || !strings.Contains(html, "OldProcess") || !strings.Contains(html, "RemovedHelper") {
		t.Errorf("expected HTML to contain data-scopes-l with OldProcess and RemovedHelper")
	}
	if !strings.Contains(html, "diff-header-scope-side-btn") {
		t.Errorf("expected HTML to contain diff-header-scope-side-btn")
	}
	if !strings.Contains(html, "diff-scope-side-left") || !strings.Contains(html, "diff-scope-side-right") {
		t.Errorf("expected HTML to contain diff-scope-side-left and diff-scope-side-right")
	}
	if !strings.Contains(html, "diff-header-scope-group") {
		t.Errorf("expected HTML to contain diff-header-scope-group")
	}
	if !strings.Contains(html, "diff-scope-side-icon") {
		t.Errorf("expected HTML to contain diff-scope-side-icon")
	}
	if !strings.Contains(html, "M3 11.8V6a1.5 1.5 0 0 1 1.5-1.5h6.7") {
		t.Errorf("expected HTML to contain orthogonal up-then-right arrow path")
	}
	if !strings.Contains(html, "M11 11.8V6A1.5 1.5 0 0 0 9.5 4.5H2.8") {
		t.Errorf("expected HTML to contain orthogonal up-then-left arrow path")
	}
	if strings.Contains(html, ">↗<") || strings.Contains(html, ">↖<") {
		t.Errorf("expected HTML not to contain >↗< or >↖< text arrows")
	}

	// 2. Single-file view with mode=split
	fileURL := fmt.Sprintf("%s/r/%s/c/%s/f/process.go?mode=split&base=%s&target=%s", ts.URL, rec.Slug, targetCommit, baseCommit, targetCommit)
	respFile, err := http.Get(fileURL)
	if err != nil {
		t.Fatalf("GET split file view failed: %v", err)
	}
	bodyFileBytes, _ := io.ReadAll(respFile.Body)
	respFile.Body.Close()
	if respFile.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for split file view, got %d", respFile.StatusCode)
	}
	fileHtml := string(bodyFileBytes)
	if !strings.Contains(fileHtml, `data-diff-mode="split"`) {
		t.Errorf("expected file view to contain data-diff-mode=\"split\"")
	}
	if !strings.Contains(fileHtml, "data-scopes-r=") || !strings.Contains(fileHtml, "NewProcess") {
		t.Errorf("expected file view to contain data-scopes-r with NewProcess")
	}
	if !strings.Contains(fileHtml, "data-scopes-l=") || !strings.Contains(fileHtml, "OldProcess") {
		t.Errorf("expected file view to contain data-scopes-l with OldProcess")
	}
	if !strings.Contains(fileHtml, "appHeaderScopeGroup") || !strings.Contains(fileHtml, "appHeaderScopeSideLeft") || !strings.Contains(fileHtml, "appHeaderScopeSideRight") {
		t.Errorf("expected file view to contain appHeaderScopeGroup, appHeaderScopeSideLeft, and appHeaderScopeSideRight")
	}
	if !strings.Contains(fileHtml, "diff-scope-side-icon") {
		t.Errorf("expected file view to contain diff-scope-side-icon")
	}
	if !strings.Contains(fileHtml, "M3 11.8V6a1.5 1.5 0 0 1 1.5-1.5h6.7") {
		t.Errorf("expected file view to contain orthogonal up-then-right arrow path")
	}
	if !strings.Contains(fileHtml, "M11 11.8V6A1.5 1.5 0 0 0 9.5 4.5H2.8") {
		t.Errorf("expected file view to contain orthogonal up-then-left arrow path")
	}
	if strings.Contains(fileHtml, ">↗<") || strings.Contains(fileHtml, ">↖<") {
		t.Errorf("expected file view not to contain >↗< or >↖< text arrows")
	}

	// Verify split scope side persistence:
	// Initial page load has no data-scope-side="L" (defaults to "R")
	if strings.Contains(fileHtml, `data-scope-side="L"`) {
		t.Errorf("expected initial load not to have data-scope-side=\"L\"")
	}

	// Update scope_side pref to "L" via /api/prefs
	prefBody := bytes.NewBufferString(`{"key":"scope_side","val":"L"}`)
	respPref, err := http.Post(ts.URL+"/api/prefs", "application/json", prefBody)
	if err != nil {
		t.Fatalf("POST /api/prefs failed: %v", err)
	}
	respPref.Body.Close()
	if respPref.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /api/prefs, got %d", respPref.StatusCode)
	}
	if srv.d.Pref("scope_side", "") != "L" {
		t.Errorf("expected DB pref scope_side to be 'L', got %q", srv.d.Pref("scope_side", ""))
	}

	// Subsequent file view load should render data-scope-side="L" on <html
	respFileL, err := http.Get(fileURL)
	if err != nil {
		t.Fatalf("GET file view after pref change failed: %v", err)
	}
	bodyFileLBytes, _ := io.ReadAll(respFileL.Body)
	respFileL.Body.Close()
	fileLHtml := string(bodyFileLBytes)
	if !strings.Contains(fileLHtml, `data-scope-side="L"`) {
		t.Errorf("expected file view to contain data-scope-side=\"L\" after setting pref")
	}

	// Normalization test: setting invalid value should normalize to "R"
	prefNormBody := bytes.NewBufferString(`{"key":"scope_side","val":"invalid"}`)
	respNorm, err := http.Post(ts.URL+"/api/prefs", "application/json", prefNormBody)
	if err != nil {
		t.Fatalf("POST /api/prefs with invalid value failed: %v", err)
	}
	respNorm.Body.Close()
	if srv.d.Pref("scope_side", "") != "R" {
		t.Errorf("expected normalized DB pref to be 'R', got %q", srv.d.Pref("scope_side", ""))
	}

	// Reset back to default "R" for subsequent tests
	srv.d.SetPref("scope_side", "R")

	// 3. /api/diff/file lazy-load endpoint with mode=split
	apiURL := fmt.Sprintf("%s/api/diff/file?repo=%s&change=%s&path=process.go&index=0&mode=split&base=%s&target=%s", ts.URL, rec.Slug, targetCommit, baseCommit, targetCommit)
	respAPI, err := http.Get(apiURL)
	if err != nil {
		t.Fatalf("GET /api/diff/file failed: %v", err)
	}
	bodyAPIBytes, _ := io.ReadAll(respAPI.Body)
	respAPI.Body.Close()
	if respAPI.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /api/diff/file, got %d", respAPI.StatusCode)
	}
	apiHtml := string(bodyAPIBytes)
	if !strings.Contains(apiHtml, "data-scopes-r=") || !strings.Contains(apiHtml, "NewProcess") {
		t.Errorf("expected /api/diff/file to contain data-scopes-r with NewProcess")
	}
	if !strings.Contains(apiHtml, "data-scopes-l=") || !strings.Contains(apiHtml, "OldProcess") {
		t.Errorf("expected /api/diff/file to contain data-scopes-l with OldProcess")
	}

	// Verify CSS accessibility rule for empty-side focus-visible
	cssData, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read style.css: %v", err)
	}
	cssText := string(cssData)
	if !strings.Contains(cssText, ".diff-header-scope-group.is-split.is-empty-side[data-active-side=\"L\"] .diff-scope-side-left:focus-visible") {
		t.Errorf("expected style.css to include :focus-visible rule for left empty-side toggle button")
	}
	if !strings.Contains(cssText, ".diff-header-scope-group.is-split.is-empty-side[data-active-side=\"R\"] .diff-scope-side-right:focus-visible") {
		t.Errorf("expected style.css to include :focus-visible rule for right empty-side toggle button")
	}
	if strings.Contains(cssText, "\n.move-popup-btn {") {
		t.Errorf("expected dead standalone .move-popup-btn CSS rule to be removed")
	}

	// 4. Node VM test verifying static/app.js behavior
	if nodeBin, err := exec.LookPath("node"); err == nil {
		script := `
const fs = require('fs');
const vm = require('vm');
const code = fs.readFileSync('static/app.js', 'utf8');

const barEl = {
  style: { display: 'none' },
  offsetWidth: 200,
  textContent: '',
  title: '',
  attributes: {},
  setAttribute(k, v) { this.attributes[k] = String(v); },
  getAttribute(k) { return this.attributes[k] || null; },
  removeAttribute(k) { delete this.attributes[k]; },
};

const sideBtnLeftEl = {
  style: { display: '' },
  tabIndex: -1,
  offsetWidth: 20,
  innerHTML: '<svg class="diff-scope-side-icon" width="13" height="13" viewBox="0 0 14 14" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 11.8V6a1.5 1.5 0 0 1 1.5-1.5h6.7"/><polyline points="8.8 2.2 11.5 4.5 8.8 6.8"/></svg>',
  title: 'Based on left diff — click to base on right diff',
  attributes: {},
  setAttribute(k, v) { this.attributes[k] = String(v); },
  getAttribute(k) { return this.attributes[k] || null; },
  removeAttribute(k) { delete this.attributes[k]; },
  focus() { doc.activeElement = this; },
  closest(sel) {
    if (sel === '.diff-header-scope-group') return groupEl;
    if (sel === '.diff-file-section') return fileSecEl;
    if (sel === '.diff-container') return containerEl;
    return null;
  },
};

const sideBtnRightEl = {
  style: { display: '' },
  tabIndex: 0,
  offsetWidth: 20,
  innerHTML: '<svg class="diff-scope-side-icon" width="13" height="13" viewBox="0 0 14 14" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M11 11.8V6A1.5 1.5 0 0 0 9.5 4.5H2.8"/><polyline points="5.2 2.2 2.5 4.5 5.2 6.8"/></svg>',
  title: 'Based on right diff — click to base on left diff',
  attributes: {},
  setAttribute(k, v) { this.attributes[k] = String(v); },
  getAttribute(k) { return this.attributes[k] || null; },
  removeAttribute(k) { delete this.attributes[k]; },
  focus() { doc.activeElement = this; },
  closest(sel) {
    if (sel === '.diff-header-scope-group') return groupEl;
    if (sel === '.diff-file-section') return fileSecEl;
    if (sel === '.diff-container') return containerEl;
    return null;
  },
};

const groupEl = {
  style: { display: 'none' },
  attributes: {},
  classList: {
    classes: new Set(),
    add(c) { this.classes.add(c); },
    remove(c) { this.classes.delete(c); },
    contains(c) { return this.classes.has(c); },
  },
  setAttribute(k, v) { this.attributes[k] = String(v); },
  getAttribute(k) { return this.attributes[k] || null; },
  removeAttribute(k) { delete this.attributes[k]; },
  querySelector(sel) {
    if (sel === '.diff-scope-side-left') return sideBtnLeftEl;
    if (sel === '.diff-scope-side-right') return sideBtnRightEl;
    if (sel === '.diff-header-scope') return barEl;
    return null;
  },
};

const fileHeaderEl = {
  classList: {
    classes: new Set(),
    add(c) { this.classes.add(c); },
    remove(c) { this.classes.delete(c); },
    contains(c) { return this.classes.has(c); },
  },
  getBoundingClientRect() { return { top: 48, bottom: 78 }; },
};

const fileSecEl = {
  classList: {
    classes: new Set(),
    add(c) { this.classes.add(c); },
    remove(c) { this.classes.delete(c); },
    contains(c) { return this.classes.has(c); },
  },
  querySelector(sel) {
    if (sel === '.diff-file-header') return fileHeaderEl;
    if (sel === '.diff-header-scope-group') return groupEl;
    if (sel === '.diff-header-scope') return barEl;
    if (sel === '.diff-scope-side-left') return sideBtnLeftEl;
    if (sel === '.diff-scope-side-right') return sideBtnRightEl;
    return null;
  },
  querySelectorAll(sel) {
    if (sel === '.diff-header-scope-side-btn') return [sideBtnLeftEl, sideBtnRightEl];
    return [];
  },
};

// Second container in multi-file commit view to verify simultaneous global update
const bar2El = {
  style: { display: 'none' },
  offsetWidth: 200,
  textContent: '',
  attributes: {},
  setAttribute(k, v) { this.attributes[k] = String(v); },
  getAttribute(k) { return this.attributes[k] || null; },
  removeAttribute(k) { delete this.attributes[k]; },
};
const sideBtn2LeftEl = { style: { display: '' }, offsetWidth: 20, attributes: {}, setAttribute(k, v) { this.attributes[k] = String(v); }, getAttribute(k) { return this.attributes[k] || null; }, removeAttribute(k) { delete this.attributes[k]; } };
const sideBtn2RightEl = { style: { display: '' }, offsetWidth: 20, attributes: {}, setAttribute(k, v) { this.attributes[k] = String(v); }, getAttribute(k) { return this.attributes[k] || null; }, removeAttribute(k) { delete this.attributes[k]; } };
const group2El = {
  style: { display: 'none' },
  attributes: {},
  classList: {
    classes: new Set(),
    add(c) { this.classes.add(c); },
    remove(c) { this.classes.delete(c); },
    contains(c) { return this.classes.has(c); },
  },
  setAttribute(k, v) { this.attributes[k] = String(v); },
  getAttribute(k) { return this.attributes[k] || null; },
  removeAttribute(k) { delete this.attributes[k]; },
  querySelector(sel) {
    if (sel === '.diff-scope-side-left') return sideBtn2LeftEl;
    if (sel === '.diff-scope-side-right') return sideBtn2RightEl;
    if (sel === '.diff-header-scope') return bar2El;
    return null;
  },
};
const fileSec2El = {
  classList: { classes: new Set(), add(c) { this.classes.add(c); }, remove(c) { this.classes.delete(c); }, contains(c) { return this.classes.has(c); } },
  querySelector(sel) {
    if (sel === '.diff-file-header') return fileHeaderEl;
    if (sel === '.diff-header-scope-group') return group2El;
    if (sel === '.diff-header-scope') return bar2El;
    if (sel === '.diff-scope-side-left') return sideBtn2LeftEl;
    if (sel === '.diff-scope-side-right') return sideBtn2RightEl;
    return null;
  },
  querySelectorAll(sel) {
    if (sel === '.diff-header-scope-side-btn') return [sideBtn2LeftEl, sideBtn2RightEl];
    return [];
  },
};

function makeRow(attrs) {
  return {
    tagName: 'TR',
    classList: { contains(c) { return c === 'diff-row'; } },
    attributes: attrs || {},
    getAttribute(a) { return this.attributes[a] || null; },
    getBoundingClientRect() { return { top: 80, bottom: 98 }; },
  };
}

const row0 = makeRow({ 'data-line-l': '5', 'data-line-r': '5' });
const row1 = makeRow({ 'data-line-r': '14' });
const row2Prev = makeRow({ 'data-line-l': '10', 'data-line-r': '10' });
const row2Gap = makeRow({ 'data-line-r': '11' });
const row3 = makeRow({ 'data-line-l': '22' });

let currentRows = [row0];

const tableEl = {
  classList: {
    classes: new Set(),
    add(c) { this.classes.add(c); },
    remove(c) { this.classes.delete(c); },
    contains(c) { return this.classes.has(c); },
  },
  getBoundingClientRect() { return { top: 80, bottom: 600 }; },
  querySelector(sel) { return null; },
  querySelectorAll(sel) {
    if (sel === 'tr.diff-row') return currentRows;
    return [];
  },
};

const table2El = {
  classList: { classes: new Set(), add(c) {}, remove(c) {}, contains(c) { return false; } },
  getBoundingClientRect() { return { top: 80, bottom: 600 }; },
  querySelector() { return null; },
  querySelectorAll(sel) { if (sel === 'tr.diff-row') return currentRows; return []; },
};

const scopesRJSON = JSON.stringify([
  { s: 3, e: 10, h: "func NewProcess()", nl: 3, nc: 6 },
  { s: 12, e: 18, h: "func AddedHelper()", nl: 12, nc: 6 }
]);

const scopesLJSON = JSON.stringify([
  { s: 3, e: 10, h: "func OldProcess()", nl: 3, nc: 6 },
  { s: 20, e: 26, h: "func RemovedHelper()", nl: 20, nc: 6 }
]);

const containerEl = {
  offsetParent: {},
  attributes: {
    'data-scopes': scopesRJSON,
    'data-scopes-side': 'R',
    'data-scopes-r': scopesRJSON,
    'data-scopes-l': scopesLJSON,
    'data-diff-mode': 'split',
    'data-file-path': 'process.go',
  },
  getAttribute(a) { return this.attributes[a] || null; },
  setAttribute(a, v) { this.attributes[a] = String(v); },
  removeAttribute(a) { delete this.attributes[a]; },
  hasAttribute(a) { return this.attributes[a] !== undefined && this.attributes[a] !== null; },
  closest(sel) {
    if (sel === '.diff-file-section') return fileSecEl;
    return null;
  },
  querySelector(sel) {
    if (sel === '.diff-table') return tableEl;
    return null;
  },
};

const container2El = {
  offsetParent: {},
  attributes: {
    'data-scopes': scopesRJSON,
    'data-scopes-side': 'R',
    'data-scopes-r': scopesRJSON,
    'data-scopes-l': scopesLJSON,
    'data-diff-mode': 'split',
    'data-file-path': 'other.go',
  },
  getAttribute(a) { return this.attributes[a] || null; },
  setAttribute(a, v) { this.attributes[a] = String(v); },
  removeAttribute(a) { delete this.attributes[a]; },
  hasAttribute(a) { return this.attributes[a] !== undefined && this.attributes[a] !== null; },
  closest(sel) {
    if (sel === '.diff-file-section') return fileSec2El;
    return null;
  },
  querySelector(sel) {
    if (sel === '.diff-table') return table2El;
    return null;
  },
};

const fileBodyEl = {
  style: { display: 'block' },
  getAttribute() { return null; },
  closest(sel) {
    if (sel === '.diff-file-section') return fileSecEl;
    return null;
  },
};

const doc = {
  readyState: 'complete',
  activeElement: null,
  documentElement: {
    attributes: {},
    getAttribute(k) { return this.attributes[k] || null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    removeAttribute(k) { delete this.attributes[k]; },
  },
  addEventListener() {},
  getElementById(id) {
    if (id === 'appHeaderScopeGroup') return null;
    if (id === 'appHeaderScope') return null;
    if (id === 'appHeaderScopeSideLeft') return null;
    if (id === 'appHeaderScopeSideRight') return null;
    if (id === 'file-body-0') return fileBodyEl;
    if (id === 'file-sec-0') return fileSecEl;
    return null;
  },
  querySelector(sel) {
    if (sel === 'header.app-header') return {
      classList: {
        classes: new Set(),
        add(c) { this.classes.add(c); },
        remove(c) { this.classes.delete(c); },
        contains(c) { return this.classes.has(c); },
      },
      getBoundingClientRect() { return { top: 0, bottom: 48 }; },
    };
    return null;
  },
  querySelectorAll(sel) {
    if (sel === '.diff-container[data-scopes]') return [containerEl, container2El];
    if (sel === '.diff-file-body') return [fileBodyEl];
    return [];
  },
};

const ctx = {
  document: doc,
  window: {
    location: { search: '', pathname: '/r/test/c/123' },
    addEventListener() {},
    setTimeout(cb) { cb(); },
    requestAnimationFrame(cb) { cb(); },
  },
  setTimeout(cb) { cb(); },
  clearTimeout() {},
  console: console,
};
ctx.window.document = doc;
vm.createContext(ctx);
vm.runInContext(code, ctx);

(async () => {
// 1. Initial state in split view: defaults to 'R'
ctx.window.updateStickyScopes();
if (barEl.style.display !== 'inline-block') {
  throw new Error("expected barEl.style.display to be inline-block, got: " + barEl.style.display);
}
if (!barEl.textContent.includes("NewProcess")) {
  throw new Error("expected barEl.textContent to include NewProcess, got: " + barEl.textContent);
}
if (barEl.getAttribute("data-active-side") !== "R") {
  throw new Error("expected data-active-side R, got: " + barEl.getAttribute("data-active-side"));
}
if (!groupEl.classList.contains("is-split")) {
  throw new Error("expected groupEl to have class is-split");
}
if (groupEl.getAttribute("data-active-side") !== "R") {
  throw new Error("expected groupEl data-active-side R, got: " + groupEl.getAttribute("data-active-side"));
}
if (groupEl.classList.contains("is-empty-side")) {
  throw new Error("expected groupEl to not have class is-empty-side when sActive is present");
}
if (sideBtnLeftEl.getAttribute("data-scope-side") !== "L") {
  throw new Error("expected data-scope-side L on sideBtnLeftEl, got: " + sideBtnLeftEl.getAttribute("data-scope-side"));
}
if (sideBtnRightEl.getAttribute("data-scope-side") !== "R") {
  throw new Error("expected data-scope-side R on sideBtnRightEl, got: " + sideBtnRightEl.getAttribute("data-scope-side"));
}
if (sideBtnRightEl.tabIndex !== 0 || sideBtnRightEl.getAttribute("aria-hidden") !== null) {
  throw new Error("expected active sideBtnRightEl to be focusable and visible to AT");
}
if (sideBtnLeftEl.tabIndex !== -1 || sideBtnLeftEl.getAttribute("aria-hidden") !== "true") {
  throw new Error("expected inactive sideBtnLeftEl to have tabIndex -1 and aria-hidden true");
}
if (!sideBtnLeftEl.innerHTML.includes("diff-scope-side-icon") || !sideBtnLeftEl.innerHTML.includes("M3 11.8V6a1.5 1.5 0 0 1 1.5-1.5h6.7")) {
  throw new Error("expected sideBtnLeftEl to contain orthogonal up-then-right arrow SVG");
}
if (!sideBtnRightEl.innerHTML.includes("diff-scope-side-icon") || !sideBtnRightEl.innerHTML.includes("M11 11.8V6A1.5 1.5 0 0 0 9.5 4.5H2.8")) {
  throw new Error("expected sideBtnRightEl to contain orthogonal up-then-left arrow SVG");
}
// Multi-container check: container2El was also initialized to R
if (bar2El.style.display !== 'inline-block' || !bar2El.textContent.includes("NewProcess")) {
  throw new Error("expected bar2El in multi-file commit to also show NewProcess on R");
}

// 2. Click handleToggleStickyScopeSide on focused right button: switches globally to 'L' and transfers focus to sideBtnLeftEl
doc.activeElement = sideBtnRightEl;
ctx.window.handleToggleStickyScopeSide(null, sideBtnRightEl);
if (doc.documentElement.getAttribute("data-scope-side") !== "L") {
  throw new Error("expected doc.documentElement data-scope-side L after toggle");
}
if (doc.activeElement !== sideBtnLeftEl) {
  throw new Error("expected focus to transfer from sideBtnRightEl to newly active sideBtnLeftEl");
}
if (sideBtnLeftEl.tabIndex !== 0 || sideBtnLeftEl.getAttribute("aria-hidden") !== null) {
  throw new Error("expected active sideBtnLeftEl to have tabIndex 0 and no aria-hidden");
}
if (sideBtnRightEl.tabIndex !== -1 || sideBtnRightEl.getAttribute("aria-hidden") !== "true") {
  throw new Error("expected inactive sideBtnRightEl to have tabIndex -1 and aria-hidden true");
}
if (groupEl.getAttribute("data-active-side") !== "L") {
  throw new Error("expected groupEl data-active-side L after toggle, got: " + groupEl.getAttribute("data-active-side"));
}
if (barEl.style.display !== 'inline-block') {
  throw new Error("expected barEl.style.display inline-block after toggle to L");
}
if (!barEl.textContent.includes("OldProcess")) {
  throw new Error("expected barEl.textContent to include OldProcess after toggle, got: " + barEl.textContent);
}
if (barEl.getAttribute("data-active-side") !== "L") {
  throw new Error("expected data-active-side L, got: " + barEl.getAttribute("data-active-side"));
}
// Verify simultaneous update of ALL split containers in multi-file commit
if (bar2El.style.display !== 'inline-block' || !bar2El.textContent.includes("OldProcess")) {
  throw new Error("expected bar2El in multi-file commit to update simultaneously to OldProcess on L");
}
if (group2El.getAttribute("data-active-side") !== "L") {
  throw new Error("expected group2El in multi-file commit to have data-active-side L");
}
if (fileSecEl.classList.contains("collapsed")) {
  throw new Error("expected fileSecEl to not be collapsed on side button click");
}

// 3. Test handleStickyScopeClick while toggled to 'L': passes side 'L'
let symbolPopupSide = null;
ctx.window.openSymbolPopupAt = async function(container, side, nl, nc, targetEl, pinned) {
  symbolPopupSide = side;
  return true;
};
await ctx.window.handleStickyScopeClick(null, barEl);
if (symbolPopupSide !== "L") {
  throw new Error("expected openSymbolPopupAt to receive side 'L', got: " + symbolPopupSide);
}

// 4. Click handleToggleStickyScopeSide again (on left button): toggles back to 'R'
ctx.window.handleToggleStickyScopeSide(null, sideBtnLeftEl);
if (doc.documentElement.getAttribute("data-scope-side") !== null) {
  throw new Error("expected doc.documentElement data-scope-side removed on toggle back to R");
}
if (groupEl.getAttribute("data-active-side") !== "R") {
  throw new Error("expected groupEl data-active-side R, got: " + groupEl.getAttribute("data-active-side"));
}
if (!barEl.textContent.includes("NewProcess")) {
  throw new Error("expected barEl to switch back to NewProcess, got: " + barEl.textContent);
}
if (barEl.getAttribute("data-active-side") !== "R") {
  throw new Error("expected data-active-side R, got: " + barEl.getAttribute("data-active-side"));
}
if (!bar2El.textContent.includes("NewProcess")) {
  throw new Error("expected bar2El to also switch back to NewProcess simultaneously");
}

// 5. Edge case A: added function on R, no enclosing scope on L
currentRows = [row1];
tableEl._diffRows = null;
table2El._diffRows = null;
ctx.window.setGlobalScopeSide('R');
if (!barEl.textContent.includes("AddedHelper")) {
  throw new Error("expected barEl to show AddedHelper on R, got: " + barEl.textContent);
}
if (groupEl.classList.contains("is-empty-side")) {
  throw new Error("expected groupEl to not be empty-side when AddedHelper is on R");
}
// Toggle to L: no scope on L, so barEl hidden, groupEl gains is-empty-side, left button title updated!
ctx.window.setGlobalScopeSide('L');
if (barEl.style.display !== 'none') {
  throw new Error("expected barEl.style.display to be 'none' on L with no scope, got: " + barEl.style.display);
}
if (!groupEl.classList.contains("is-empty-side")) {
  throw new Error("expected groupEl to have is-empty-side on L when no scope on L");
}
if (!sideBtnLeftEl.title.includes("No enclosing declaration on left diff")) {
  throw new Error("expected sideBtnLeftEl title to indicate no enclosing declaration on left diff, got: " + sideBtnLeftEl.title);
}
// Toggle back to R: barEl reappears with AddedHelper, is-empty-side removed!
ctx.window.setGlobalScopeSide('R');
if (barEl.style.display !== 'inline-block' || !barEl.textContent.includes("AddedHelper")) {
  throw new Error("expected barEl to reappear with AddedHelper when switching back to R");
}
if (groupEl.classList.contains("is-empty-side")) {
  throw new Error("expected groupEl to not have is-empty-side after returning to R");
}

// 6. Edge case B: deleted function on L, no enclosing scope on R
currentRows = [row3];
tableEl._diffRows = null;
table2El._diffRows = null;
ctx.window.setGlobalScopeSide('R');
// Starts on R: no scope on R, so barEl hidden, groupEl gains is-empty-side, right button title updated!
if (barEl.style.display !== 'none') {
  throw new Error("expected barEl to be hidden on R for deleted func, got: " + barEl.style.display);
}
if (!groupEl.classList.contains("is-empty-side")) {
  throw new Error("expected groupEl to have is-empty-side on R for deleted func");
}
if (!sideBtnRightEl.title.includes("No enclosing declaration on right diff")) {
  throw new Error("expected sideBtnRightEl title to indicate no enclosing declaration on right diff, got: " + sideBtnRightEl.title);
}
// Click side button: switches to L and shows RemovedHelper!
ctx.window.setGlobalScopeSide('L');
if (barEl.style.display !== 'inline-block' || !barEl.textContent.includes("RemovedHelper")) {
  throw new Error("expected barEl to show RemovedHelper on L, got display=" + barEl.style.display + ", text=" + barEl.textContent);
}
if (groupEl.classList.contains("is-empty-side")) {
  throw new Error("expected groupEl to not have is-empty-side on L with RemovedHelper");
}

// 7. Edge case C: gap after closing }
currentRows = [row2Prev, row2Gap];
tableEl._diffRows = null;
ctx.window.setGlobalScopeSide('L');
// At row2Gap (startIdx 1): row2Prev was line 10 on L. Line 10 is OldProcess's s.e.
// resolveSideAnchorLine gives 10.5 on L, which must NOT match OldProcess.
const sGap = ctx.window.findScopeAtLine ? ctx.window.findScopeAtLine(JSON.parse(scopesLJSON), 10.5) : null;
if (sGap !== null) {
  throw new Error("expected line 10.5 not to match scope ending at 10, got: " + JSON.stringify(sGap));
}

// Collapsed row-skip DOM traversal test:
// diff-row at L5 inside [3, 9] -> row-skip with data-left-start="6" data-left-end="20" -> pure-R diff-row at R25
const skipRowTest = {
  tagName: 'TR',
  classList: {
    classes: new Set(['row-skip']),
    contains(c) { return this.classes.has(c); },
  },
  attributes: { 'data-left-start': '6', 'data-left-end': '20' },
  getAttribute(a) { return this.attributes[a] || null; },
  previousElementSibling: null,
  nextElementSibling: null,
};
const prevDiffRow = makeRow({ 'data-line-l': '5' });
prevDiffRow.previousElementSibling = null;
prevDiffRow.nextElementSibling = skipRowTest;
skipRowTest.previousElementSibling = prevDiffRow;

const pureRRow = makeRow({ 'data-line-r': '25' });
pureRRow.previousElementSibling = skipRowTest;
pureRRow.nextElementSibling = null;
skipRowTest.nextElementSibling = pureRRow;

const anchorLine = ctx.window.resolveSideAnchorLine ? ctx.window.resolveSideAnchorLine([prevDiffRow, pureRRow], 1, 'data-line-l') : 0;
if (anchorLine !== 20.5) {
  throw new Error("expected resolveSideAnchorLine with row-skip to return 20.5, got: " + anchorLine);
}

// 8. Unified view: side buttons MUST ALWAYS be hidden and is-split removed
currentRows = [row0];
tableEl._diffRows = null;
containerEl.attributes['data-diff-mode'] = 'unified';
container2El.attributes['data-diff-mode'] = 'unified';
ctx.window.updateStickyScopes();
if (groupEl.classList.contains('is-split')) {
  throw new Error("expected groupEl not to have is-split in unified view");
}
if (sideBtnLeftEl.style.display !== 'none' || sideBtnRightEl.style.display !== 'none') {
  throw new Error("expected side buttons to be hidden in unified view");
}

// 9. /ext view: side buttons MUST ALWAYS be hidden
containerEl.attributes['data-diff-mode'] = 'split';
containerEl.attributes['data-ext-pkg'] = 'fmt';
ctx.window.updateStickyScopes();
if (groupEl.classList.contains('is-split')) {
  throw new Error("expected groupEl not to have is-split in /ext view");
}
if (sideBtnLeftEl.style.display !== 'none' || sideBtnRightEl.style.display !== 'none') {
  throw new Error("expected side buttons to be hidden in /ext view");
}

// 10. Single column view: side buttons MUST ALWAYS be hidden
delete containerEl.attributes['data-ext-pkg'];
tableEl.classList.add('diff-table-single');
ctx.window.updateStickyScopes();
if (groupEl.classList.contains('is-split')) {
  throw new Error("expected groupEl not to have is-split in single column view");
}
if (sideBtnLeftEl.style.display !== 'none' || sideBtnRightEl.style.display !== 'none') {
  throw new Error("expected side buttons to be hidden in single column view");
}

// 11. Short untruncated vs truncated header visibility
tableEl.classList.remove('diff-table-single');
currentRows = [row0];
tableEl._diffRows = null;
ctx.window.setGlobalScopeSide('R');

// Short untruncated header (e.g. func f())
barEl.offsetWidth = 56;
barEl.clientWidth = 54;
barEl.scrollWidth = 54;
ctx.window.updateStickyScopes();
if (barEl.style.display !== 'inline-block') {
  throw new Error("expected short untruncated barEl to stay visible (inline-block), got: " + barEl.style.display);
}
if (groupEl.style.display === 'none') {
  throw new Error("expected groupEl to stay visible when untruncated");
}

// Short truncated header (overflowing container) -> hides both bar and group cleanly and strips split attributes
barEl.offsetWidth = 56;
barEl.clientWidth = 54;
barEl.scrollWidth = 220;
ctx.window.updateStickyScopes();
if (barEl.style.display !== 'none') {
  throw new Error("expected short truncated barEl to be hidden (none), got: " + barEl.style.display);
}
if (groupEl.style.display !== 'none') {
  throw new Error("expected groupEl to be hidden when barEl is truncated to prevent orphan empty slots");
}
if (groupEl.getAttribute("data-active-side") !== null || groupEl.classList.contains("is-split") || groupEl.classList.contains("is-empty-side")) {
  throw new Error("expected truncated bar cleanup to remove data-active-side, is-split, and is-empty-side");
}
if (sideBtnRightEl.tabIndex !== -1 || sideBtnRightEl.getAttribute("aria-hidden") !== "true") {
  throw new Error("expected truncated bar cleanup to reset side button accessibility state");
}

// Reset barEl dimensions and re-show scope group
barEl.offsetWidth = 200;
delete barEl.clientWidth;
delete barEl.scrollWidth;
ctx.window.updateStickyScopes();
if (groupEl.style.display !== 'inline-flex' || groupEl.getAttribute("data-active-side") !== "R") {
  throw new Error("expected groupEl to become visible again on R after resetting bar width");
}

// 12. offsetParent === null resets stale visible scope group
containerEl.offsetParent = null;
ctx.window.updateStickyScopes();
if (groupEl.style.display !== 'none' || groupEl.getAttribute("data-active-side") !== null || groupEl.classList.contains("is-split")) {
  throw new Error("expected offsetParent === null to reset hidden scope group state");
}
containerEl.offsetParent = {};
ctx.window.updateStickyScopes();

// 13. toggleFileSection & toggleAllFiles collapse cleanly resets groupEl attributes
ctx.window.toggleFileSection(0);
if (groupEl.style.display !== 'none' || groupEl.getAttribute("data-active-side") !== null || groupEl.classList.contains("is-split") || fileHeaderEl.classList.contains("has-scope")) {
  throw new Error("expected toggleFileSection collapse to clear groupEl attributes and has-scope");
}
// Expand and test toggleAllFiles collapse
ctx.window.toggleFileSection(0);
ctx.window.updateStickyScopes();
ctx.window.toggleAllFiles();
if (groupEl.style.display !== 'none' || groupEl.getAttribute("data-active-side") !== null || groupEl.classList.contains("is-split") || fileHeaderEl.classList.contains("has-scope")) {
  throw new Error("expected toggleAllFiles collapse to clear groupEl attributes and has-scope");
}

console.log("NODE_SPLIT_OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
		cmd := exec.Command(nodeBin, "-e", script)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Node VM test failed: %v\nOutput: %s", err, string(out))
		}
		if !strings.Contains(string(out), "NODE_SPLIT_OK") {
			t.Fatalf("Node VM test did not output NODE_SPLIT_OK: %s", string(out))
		}
	}
}

func setupTestServerForUI(t *testing.T) (*httptest.Server, *db.RepoRecord, string) {
	t.Helper()
	repoDir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")
	run("config", "user.name", "UI Tester")
	run("config", "user.email", "ui@example.com")

	goMod := "module testmod\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	utilGo := `package pkg

// HelperFunction performs a critical calculation.
func HelperFunction(x int) int {
	return x * 2
}
`
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "util.go"), []byte(utilGo), 0644); err != nil {
		t.Fatal(err)
	}

	mainGo := `package main

import (
	"fmt"
	"testmod/pkg"
)

func main() {
	res := pkg.HelperFunction(21)
	fmt.Println(res)
}
`
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}

	run("add", ".")
	run("commit", "-m", "Initial commit\n\nChange-Id: I1111111111111111111111111111111111111111")

	// Second commit modifies pkg/util.go, leaving main.go unchanged
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "util.go"), []byte(utilGo+"\n// Extra\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-a", "-m", "Second commit\n\nChange-Id: I2222222222222222222222222222222222222222")
	secondHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(func() { ts.Close() })

	return ts, rec, secondHash
}

func TestWebReplyAuthorFromRepoUser(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	cmd := exec.Command("git", "config", "user.name", "Test User")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config user.name failed: %v: %s", err, string(out))
	}

	rec, _ := d.EnsureRepo(repoDir)
	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// 1. Add comment without author
	payloadAdd, _ := json.Marshal(map[string]any{
		"repo":       rec.Slug,
		"change":     c.Key,
		"rev":        c.Rev,
		"path":       "hello.go",
		"side":       "R",
		"start_line": 3,
		"end_line":   3,
		"body":       "Initial comment without explicit author",
	})
	resp, err := http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(payloadAdd))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add failed: %v", err)
	}
	var resAdd struct {
		Thread  db.Thread  `json:"thread"`
		Message db.Message `json:"message"`
		HTML    string     `json:"html"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&resAdd)
	resp.Body.Close()

	if resAdd.Message.Author != "Test User" {
		t.Errorf("expected initial message author to be 'Test User', got %q", resAdd.Message.Author)
	}
	if !strings.Contains(resAdd.HTML, `<strong class="thread-author">Test User</strong>`) {
		t.Errorf("expected initial HTML to contain '<strong class=\"thread-author\">Test User</strong>', got: %s", resAdd.HTML)
	}

	// 2. Reply to comment without author
	payloadReply, _ := json.Marshal(map[string]any{
		"thread_id": resAdd.Thread.ID,
		"body":      "Reply comment without explicit author",
	})
	resp, err = http.Post(ts.URL+"/api/threads/reply", "application/json", bytes.NewReader(payloadReply))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/reply failed: %v", err)
	}
	var resReply struct {
		Thread  db.Thread  `json:"thread"`
		Message db.Message `json:"message"`
		HTML    string     `json:"html"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&resReply)
	resp.Body.Close()

	if resReply.Message.Author != "Test User" {
		t.Errorf("expected reply message author to be 'Test User', got %q", resReply.Message.Author)
	}
	if !strings.Contains(resReply.HTML, `<strong class="thread-author">Test User</strong>`) {
		t.Errorf("expected reply HTML to contain '<strong class=\"thread-author\">Test User</strong>', got: %s", resReply.HTML)
	}
}

func TestExternalFilePage(t *testing.T) {
	ts, rec, changeKey := setupTestServerForUI(t)

	// 1. Valid external file: context.go
	url := fmt.Sprintf("%s/r/%s/c/%s/ext?pkg=context&file=context.go", ts.URL, rec.Slug, changeKey)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /ext failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "diff-table-single") {
		t.Errorf("expected HTML to contain diff-table-single class")
	}
	if !strings.Contains(bodyStr, "data-readonly=\"true\"") {
		t.Errorf("expected HTML to contain data-readonly=\"true\"")
	}
	if !strings.Contains(bodyStr, `data-ext-pkg="context"`) {
		t.Errorf("expected HTML to contain data-ext-pkg=\"context\"")
	}
	if !strings.Contains(bodyStr, `data-ext-file="context.go"`) {
		t.Errorf("expected HTML to contain data-ext-file=\"context.go\"")
	}
	if !strings.Contains(bodyStr, `<span class="badge" style="margin-left: 6px;">external</span>`) {
		t.Errorf("expected HTML to contain external badge")
	}
	if strings.Contains(bodyStr, "Split View") {
		t.Errorf("expected Split View button NOT to be rendered for external file")
	}

	// 2. Invalid external file: path traversal
	badUrl := fmt.Sprintf("%s/r/%s/c/%s/ext?pkg=context&file=../context.go", ts.URL, rec.Slug, changeKey)
	badResp, err := http.Get(badUrl)
	if err != nil {
		t.Fatalf("GET /ext bad failed: %v", err)
	}
	defer badResp.Body.Close()
	if badResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for path traversal in ext file, got %d", badResp.StatusCode)
	}
}

func TestMoveDetectionUI(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")
	run("config", "user.name", "Tester")
	run("config", "user.email", "tester@example.com")

	sourceGo := `package main

func ComputeWidget(x int, y int) int {
	a := x * 2
	b := y * 3
	sum := a + b
	if sum > 100 {
		return sum - 10
	}
	return sum + 10
}

func OtherFunc() {
	println("other")
}
`
	destGo := `package main

func ExistingDest() {
	println("existing dest")
}
`

	if err := os.WriteFile(filepath.Join(repoDir, "source.go"), []byte(sourceGo), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "dest.go"), []byte(destGo), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "Initial commit\n\nChange-Id: I1111111111111111111111111111111111111111")

	// In second commit: ComputeWidget is moved from source.go to dest.go, with 1 line modified (y * 3 -> y * 5)
	sourceGo2 := `package main

func OtherFunc() {
	println("other")
}
`
	destGo2 := `package main

func ExistingDest() {
	println("existing dest")
}

func ComputeWidget(x int, y int) int {
	a := x * 2
	b := y * 5
	sum := a + b
	if sum > 100 {
		return sum - 10
	}
	return sum + 10
}
`
	if err := os.WriteFile(filepath.Join(repoDir, "source.go"), []byte(sourceGo2), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "dest.go"), []byte(destGo2), 0644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-a", "-m", "Move ComputeWidget to dest.go\n\nChange-Id: I2222222222222222222222222222222222222222")
	secondHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(func() { ts.Close() })

	// 1. Fetch change page with all diffs
	url := fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, secondHash)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /c failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)

	// Check move header row and badge
	if !strings.Contains(body, "move-header-row") {
		t.Errorf("expected HTML to contain move-header-row")
	}
	if !strings.Contains(body, "move-badge") {
		t.Errorf("expected HTML to contain move-badge")
	}
	if strings.Contains(body, `class="move-badge" href="#"`) || strings.Contains(body, `href="#" class="move-badge"`) {
		t.Errorf("expected move-badge to have real destination href, got href='#'")
	}
	if !strings.Contains(body, "/r/"+rec.Slug+"/c/") || (!strings.Contains(body, "/f/dest.go?side=R#L") && !strings.Contains(body, "/f/dest.go?side=R&mode=")) {
		t.Errorf("expected move-badge to contain real destination URL link")
	}
	if !strings.Contains(body, "→ Moved to source.go:") && !strings.Contains(body, "→ Moved to dest.go:") {
		t.Errorf("expected HTML to contain '→ Moved to dest.go:', body had:\n%s", body)
	}
	if !strings.Contains(body, "← Moved from source.go:") && !strings.Contains(body, "← Moved from dest.go:") {
		t.Errorf("expected HTML to contain '← Moved from source.go:', body had:\n%s", body)
	}
	if !strings.Contains(body, "(modified)") {
		t.Errorf("expected HTML to contain '(modified)' badge label")
	}

	// Check cell-moved class for verbatim lines
	if !strings.Contains(body, "cell-moved") {
		t.Errorf("expected HTML to contain cell-moved class")
	}

	// Check cell-add / cell-del + mark.intra for the modified line
	if !strings.Contains(body, `mark class="intra"`) {
		t.Errorf("expected HTML to contain intra-line mark inside modified moved line")
	}

	// Verify invariant: td[data-side] MUST NOT contain move-badge text
	lines := strings.Split(body, "\n")
	for _, l := range lines {
		if strings.Contains(l, `data-side=`) && (strings.Contains(l, "Moved to") || strings.Contains(l, "Moved from")) {
			t.Errorf("invariant violated: td[data-side] contains badge text: %s", l)
		}
	}

	// 2. Fetch single-file diff page for dest.go
	singleUrl := fmt.Sprintf("%s/r/%s/c/%s/f/dest.go", ts.URL, rec.Slug, secondHash)
	singleResp, err := http.Get(singleUrl)
	if err != nil {
		t.Fatalf("GET /f/dest.go failed: %v", err)
	}
	defer singleResp.Body.Close()
	if singleResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for single file, got %d", singleResp.StatusCode)
	}
	singleBodyBytes, _ := io.ReadAll(singleResp.Body)
	singleBody := string(singleBodyBytes)
	if !strings.Contains(singleBody, "move-header-row") {
		t.Errorf("expected single-file HTML to contain move-header-row")
	}
	if !strings.Contains(singleBody, "move-badge") {
		t.Errorf("expected single-file HTML to contain move-badge")
	}
	if !strings.Contains(singleBody, "← Moved from source.go:") {
		t.Errorf("expected single-file HTML to contain '← Moved from source.go:', got:\n%s", singleBody)
	}

	// Verify node --check on static/app.js passes
	if nodeBin, err := exec.LookPath("node"); err == nil {
		cmd := exec.Command(nodeBin, "--check", filepath.Join("static", "app.js"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("static/app.js failed node --check: %v\n%s", err, string(out))
		}
	}
}

func TestDiffMoveDetectionSplitSwappedBlocks(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")
	run("config", "user.name", "Tester")
	run("config", "user.email", "tester@example.com")

	swapGo1 := `package main

func FirstFunc(a, b int) int {
	x := a * 2
	y := b * 3
	sum := x + y
	if sum > 50 {
		return sum - 5
	}
	return sum + 5
}

func SecondFunc(p, q int) int {
	u := p * 10
	v := q * 20
	prod := u + v
	if prod > 500 {
		return prod - 50
	}
	return prod + 50
}
`
	if err := os.WriteFile(filepath.Join(repoDir, "swap.go"), []byte(swapGo1), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "Initial commit\n\nChange-Id: I4444444444444444444444444444444444444444")

	swapGo2 := `package main

func SecondFunc(p, q int) int {
	u := p * 10
	v := q * 20
	prod := u + v
	if prod > 500 {
		return prod - 50
	}
	return prod + 50
}

func FirstFunc(a, b int) int {
	x := a * 2
	y := b * 3
	sum := x + y
	if sum > 50 {
		return sum - 5
	}
	return sum + 5
}
`
	if err := os.WriteFile(filepath.Join(repoDir, "swap.go"), []byte(swapGo2), 0644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-a", "-m", "Swap FirstFunc and SecondFunc\n\nChange-Id: I5555555555555555555555555555555555555555")
	secondHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(func() { ts.Close() })

	url := fmt.Sprintf("%s/r/%s/c/%s?diffs=all&mode=split", ts.URL, rec.Slug, secondHash)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /c failed: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)

	if !strings.Contains(body, "→ Moved to") {
		t.Errorf("expected split diff to contain '→ Moved to'")
	}
	if !strings.Contains(body, "← Moved from") {
		t.Errorf("expected split diff to contain '← Moved from'")
	}

	// Verify real href link (not href="#")
	if strings.Contains(body, `class="move-badge" href="#"`) || strings.Contains(body, `href="#" class="move-badge"`) {
		t.Errorf("expected move-badge to have real destination href, got href='#'")
	}
	if !strings.Contains(body, "/f/swap.go?side=R#L") && !strings.Contains(body, "/f/swap.go?side=R&mode=") {
		t.Errorf("expected move-badge href to point to destination line on right side, got:\n%s", body)
	}
	if !strings.Contains(body, "/f/swap.go?side=L#L") && !strings.Contains(body, "/f/swap.go?side=L&mode=") {
		t.Errorf("expected move-badge href to point to destination line on left side, got:\n%s", body)
	}

	// Verify that if a single row has both MovedL and MovedR with BlockStart, both render in one row
	var testBuf bytes.Buffer
	testData := &FileDiffData{
		RepoSlug:  "test-repo",
		ChangeKey: "test-change",
		FilePath:  "swap.go",
		DiffMode:  "split",
		Rows: []diffRowView{
			{
				Row: diff.Row{
					Type:      diff.RowChange,
					LeftLine:  5,
					RightLine: 12,
					MovedL: &diff.MoveInfo{
						BlockStart: true,
						PeerPath:   "dest.go",
						PeerStart:  20,
						PeerEnd:    25,
					},
					MovedR: &diff.MoveInfo{
						BlockStart: true,
						PeerPath:   "src.go",
						PeerStart:  40,
						PeerEnd:    45,
					},
				},
			},
		},
	}
	if err := srv.templates["diff.html"].ExecuteTemplate(&testBuf, "diff_block", testData); err != nil {
		t.Fatalf("ExecuteTemplate diff_block failed: %v", err)
	}
	rendered := testBuf.String()
	if !strings.Contains(rendered, "move-header-row") {
		t.Fatalf("expected rendered diff_block to contain move-header-row")
	}
	foundBothInSameRow := false
	for _, row := range strings.Split(rendered, "<tr class=\"move-header-row\">") {
		if strings.Contains(row, "→ Moved to dest.go:L20–25") && strings.Contains(row, "← Moved from src.go:L40–45") {
			foundBothInSameRow = true
			break
		}
	}
	if !foundBothInSameRow {
		t.Errorf("expected single move-header-row with both MovedL and MovedR to render both pills, got:\n%s", rendered)
	}
}

func TestSymbolPopupBackAbortsPendingRefs(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

let popupListeners = {};
const popupEl = {
  id: "symbolPopup",
  innerHTML: "",
  style: {},
  remove() {},
  addEventListener(event, fn) {
    if (!popupListeners[event]) popupListeners[event] = [];
    popupListeners[event].push(fn);
  },
  querySelector() { return null; },
  querySelectorAll() { return []; },
  classList: { add() {}, remove() {}, contains() { return false; } },
  getBoundingClientRect() { return { top: 0, left: 0, bottom: 0, right: 0, width: 100, height: 100 }; },
  closest(sel) {
    if (sel.includes("symbolPopup") || sel.includes("#symbolPopup")) return this;
    return null;
  }
};

let docListeners = {};
const doc = {
  readyState: "complete",
  documentElement: {
    clientWidth: 1000,
    clientHeight: 1000,
    style: {
      _props: {},
      setProperty(k, v) { this._props[k] = String(v); },
      getPropertyValue(k) { return this._props[k] || ""; },
    },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  body: {
    appendChild() {},
  },
  getElementById(id) {
    if (id === "symbolPopup") return popupEl;
    return null;
  },
  createElement(tag) {
    if (tag === "div") return popupEl;
    return {
      tagName: tag.toUpperCase(),
      className: "",
      style: {},
      classList: { add() {}, remove() {} },
      appendChild() {},
      setAttribute() {},
      getAttribute() { return null; },
    };
  },
  createTreeWalker(root) {
    return {
      currentNode: root,
      nextNode() { return null; }
    };
  },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  querySelector() { return null; },
  querySelectorAll() { return []; },
};

let abortedSignal = null;
let fetchReject = null;

const NodeFilter = { SHOW_TEXT: 4 };

const win = {
  document: doc,
  NodeFilter: NodeFilter,
  location: { pathname: "/r/test/c/abc", search: "", hash: "", reload() {} },
  addEventListener() {},
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() {
    return { isCollapsed: true };
  },
  window: null,
  fetch(url, opts) {
    if (url.includes("/api/symbol/refs")) {
      abortedSignal = opts.signal;
      return new Promise((resolve, reject) => {
        fetchReject = reject;
        if (opts.signal) {
          opts.signal.addEventListener("abort", () => {
            const err = new Error("The user aborted a request.");
            err.name = "AbortError";
            reject(err);
          });
        }
      });
    }
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        found: true,
        symbol_name: "TestSymbol",
        signature: "func TestSymbol()",
        signature_html: "<span>func TestSymbol()</span>",
        can_refs: true,
      })
    });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  NodeFilter: NodeFilter,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  fetch: win.fetch,
  URLSearchParams,
  Promise,
  setTimeout,
  clearTimeout,
  console,
  AbortController,
});
vm.runInContext(code, ctx);

(async () => {
  const containerEl = {
    getAttribute(k) {
      if (k === "data-repo") return "myrepo";
      if (k === "data-change") return "mychange";
      if (k === "data-file-path") return "foo.go";
      if (k === "data-left-rev") return "revL";
      if (k === "data-right-rev") return "revR";
      return "";
    }
  };
  const trEl = {
    getAttribute(k) {
      if (k === "data-line-r") return "10";
      return "";
    }
  };
  const dummyCodeTd = {
    getAttribute(k) {
      if (k === "data-side") return "R";
      return null;
    },
    closest(sel) {
      if (sel === "td[data-side]") return this;
      if (sel === ".diff-container") return containerEl;
      if (sel === "tr") return trEl;
      return null;
    }
  };
  const dummyTok = {
    textContent: "TestSymbol",
    closest(sel) {
      if (sel.includes("thread-card") || sel.includes("comment-form") || sel.includes("button") || sel.includes("#symbolPopup")) return null;
      if (sel.includes("tok-id") || sel.includes("tok-typ") || sel.includes("tok-imp")) return this;
      if (sel === "td[data-side]") return dummyCodeTd;
      return null;
    },
    classList: { add() {}, remove() {} },
    getBoundingClientRect() { return { top: 50, left: 50, bottom: 60, right: 100, width: 50, height: 10 }; }
  };

  const docClickHandler = docListeners["click"][0];
  await docClickHandler({
    button: 0,
    detail: 1,
    target: dummyTok,
    preventDefault() {},
    stopPropagation() {},
  });

  if (!popupEl.innerHTML.includes("TestSymbol")) {
    throw new Error("expected popup to contain TestSymbol after click");
  }

  const refsClickEvt = {
    preventDefault() {},
    stopPropagation() {},
    target: {
      closest(sel) {
        if (sel.includes("sym-btn-refs") || sel.includes("refs")) {
          return { getAttribute() { return "refs"; } };
        }
        return null;
      }
    }
  };

  const clickHandler = popupListeners["click"][0];
  const refsPromise = clickHandler(refsClickEvt);

  if (!abortedSignal || abortedSignal.aborted) {
    throw new Error("expected un-aborted signal during pending refs fetch");
  }
  if (!popupEl.innerHTML.includes("Finding references…")) {
    throw new Error("expected popup to show Finding references…");
  }

  const backClickEvt = {
    preventDefault() {},
    stopPropagation() {},
    target: {
      closest(sel) {
        if (sel.includes("sym-btn-back")) {
          return {};
        }
        return null;
      }
    }
  };
  await clickHandler(backClickEvt);

  if (!abortedSignal.aborted) {
    throw new Error("expected signal to be aborted immediately on back click");
  }

  await refsPromise;

  if (popupEl.innerHTML.includes("Finding references…")) {
    throw new Error("popup still displays Finding references… after Back");
  }
  if (!popupEl.innerHTML.includes("TestSymbol")) {
    throw new Error("popup lost definition view after Back");
  }
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TestSymbolPopupBackAbortsPendingRefs failed: %v\n%s", err, string(out))
	}
}

func TestSymbolPopupPersistenceAndStacking(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur.textContent = (cur.textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT"].includes(tag)) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    nodeType: 1,
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    style: {},
    children: [],
    parentElement: null,
    get parentNode() { return this.parentElement; },
    get childNodes() { return this.children; },
    _textContent: "",
    get textContent() {
      let t = this._textContent || "";
      for (const ch of this.children) {
        t += ch.textContent;
      }
      return t;
    },
    set textContent(v) {
      this._textContent = String(v);
    },
    listeners: {},
    get offsetWidth() {
      if (this._offsetWidth !== undefined) return this._offsetWidth;
      if (this.classList && this.classList.contains("sym-popup-actions")) return 60;
      if (this.classList && this.classList.contains("sym-btn-back")) return 24;
      if (this.classList && this.classList.contains("sym-popup-loc")) return 100;
      if (this.tagName === "BUTTON" || this.tagName === "A" || this.tagName === "SPAN") return 24;
      return 300;
    },
    set offsetWidth(v) { this._offsetWidth = v; },
    get offsetHeight() {
      if (this._offsetHeight !== undefined) return this._offsetHeight;
      if (this._rect && this._rect.height !== undefined) return this._rect.height;
      return 200;
    },
    set offsetHeight(v) { this._offsetHeight = v; },
    scrollTop: 0,
    scrollLeft: 0,
    _rect: { top: 100, left: 100, bottom: 300, right: 400, width: 300, height: 200 },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    removeAttribute(k) { delete this.attributes[k]; },
    getBoundingClientRect() { return { ...this._rect }; },
    focus() { doc.activeElement = this; this._focused = true; },
    blur() { this._blurred = true; this._focused = false; if (doc.activeElement === this) doc.activeElement = null; },
    addEventListener(event, fn) {
      if (!this.listeners[event]) this.listeners[event] = [];
      this.listeners[event].push(fn);
    },
    removeEventListener(event, fn) {
      if (this.listeners[event]) {
        this.listeners[event] = this.listeners[event].filter(f => f !== fn);
      }
    },
    appendChild(child) {
      if (!child) return child;
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    insertBefore(newChild, refChild) {
      if (!newChild) return newChild;
      newChild.parentElement = this;
      const idx = this.children.indexOf(refChild);
      if (idx !== -1) this.children.splice(idx, 0, newChild);
      else this.children.push(newChild);
      return newChild;
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    get classList() {
      return {
        contains: (c) => (el.className || "").split(/\s+/).includes(c),
        add: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.add(c));
          el.className = Array.from(parts).join(" ");
        },
        remove: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.delete(c));
          el.className = Array.from(parts).join(" ");
        }
      };
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (node.tagName && matchesSel(node, sel)) return node;
        node = node.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const results = [];
      function search(n) {
        for (const ch of n.children) {
          if (matchesSel(ch, sel)) results.push(ch);
          search(ch);
        }
      }
      search(this);
      return results;
    },
    set innerHTML(val) {
      this._innerHTML = val;
      this.children = parseHtml(val, this);
    },
    get innerHTML() {
      return this._innerHTML || "";
    }
  };
  return el;
}

function matchesSel(node, sel) {
  const parts = sel.split(",").map(s => s.trim());
  for (const p of parts) {
    if (p.includes("[")) {
      const bIdx = p.indexOf("[");
      const tagPart = p.slice(0, bIdx);
      const attrPart = p.slice(bIdx + 1, -1);
      if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
        continue;
      }
      if (attrPart.includes("=")) {
        const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
        if (node.getAttribute && node.getAttribute(k) === v) return true;
      } else {
        if (node.hasAttribute && node.hasAttribute(attrPart)) return true;
      }
      continue;
    }
    if (p.includes(".")) {
      const segs = p.split(".");
      const tagPart = segs[0];
      const clsParts = segs.slice(1);
      if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
        continue;
      }
      if (clsParts.every(c => node.classList && node.classList.contains && node.classList.contains(c))) {
        return true;
      }
      continue;
    }
    if (p.startsWith("#")) {
      if (node.id === p.slice(1)) return true;
    } else if (p.toLowerCase() === (node.tagName || "").toLowerCase()) {
      return true;
    }
  }
  return false;
}

const docListeners = {};
const winListeners = {};
const body = createMockElement("BODY");
const header = createMockElement("HEADER");
header.className = "app-header";
header._rect = { top: 0, left: 0, bottom: 48, right: 1000, width: 1000, height: 48 };
body.appendChild(header);

const doc = {
  readyState: "complete",
  documentElement: {
    clientWidth: 1000,
    clientHeight: 1000,
    style: {
      _props: {},
      setProperty(k, v) { this._props[k] = String(v); },
      getPropertyValue(k) { return this._props[k] || ""; },
    },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  body: body,
  getElementById(id) {
    if (id === "shortcutsModal") return null;
    const all = body.querySelectorAll("#" + id);
    return all.length > 0 ? all[0] : null;
  },
  createElement(tag) { return createMockElement(tag); },
  createTreeWalker(root) { return { currentNode: root, nextNode() { return null; } }; },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (docListeners[event]) docListeners[event] = docListeners[event].filter(f => f !== fn);
  },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); }
};

const win = {
  document: doc,
  NodeFilter: { SHOW_TEXT: 4 },
  location: { pathname: "/r/test/c/abc", search: "", hash: "", reload() {} },
  addEventListener(event, fn) {
    if (!winListeners[event]) winListeners[event] = [];
    winListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (winListeners[event]) winListeners[event] = winListeners[event].filter(f => f !== fn);
  },
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; },
  window: null,
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  fetch(url, opts) {
    if (win._customFetch) {
      const res = win._customFetch(url, opts);
      if (res) return res;
    }
    const sp = new URLSearchParams(url.split("?")[1] || "");
    const sym = sp.get("col") === "10" ? "SymA" : "SymB";
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        found: true,
        symbol_name: sym,
        def_path: "foo.go",
        def_line: 42,
        in_repo: true,
        display_path: "foo.go",
        signature: "func " + sym + "()",
        snippet: [
          { line: 40, html: "package main" },
          { line: 41, html: "" },
          { line: 42, active: true, html: "func " + sym + "() {" },
          { line: 43, html: "}" },
        ],
        can_refs: true,
        can_impls: true,
      })
    });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  NodeFilter: win.NodeFilter,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  fetch: win.fetch,
  URLSearchParams,
  Promise,
  setTimeout,
  clearTimeout,
  console,
  AbortController,
});
vm.runInContext(code, ctx);

(async () => {
  const container = createMockElement("DIV");
  container.className = "diff-container";
  container.setAttribute("data-file-path", "foo.go");
  container.setAttribute("data-repo", "testrepo");
  container.setAttribute("data-change", "123");
  body.appendChild(container);

  const tr = createMockElement("TR");
  tr.setAttribute("data-line-r", "10");
  container.appendChild(tr);

  const td = createMockElement("TD");
  td.setAttribute("data-side", "R");
  tr.appendChild(td);

  const tok1 = createMockElement("SPAN");
  tok1.className = "tok-id";
  tok1.textContent = "SymA";
  tok1._rect = { top: 100, left: 100, bottom: 120, right: 150, width: 50, height: 20 };
  td.appendChild(tok1);

  const tok2 = createMockElement("SPAN");
  tok2.className = "tok-id";
  tok2.textContent = "SymB";
  tok2._rect = { top: 200, left: 100, bottom: 220, right: 150, width: 50, height: 20 };
  td.appendChild(tok2);

  const docClick = docListeners["click"][0];

  // 1. Click tok1 -> opens popup 1 (pin via shade toggle so it persists across opening tok2)
  await docClick({ button: 0, detail: 1, target: tok1, preventDefault() {}, stopPropagation() {} });
  const popups1 = body.querySelectorAll(".symbol-popup");
  if (popups1.length !== 1) throw new Error("expected 1 popup, got " + popups1.length);
  const pop1 = popups1[0];
  if (!tok1.classList.contains("sym-active")) throw new Error("expected tok1 to have sym-active");
  const z1 = parseInt(pop1.style.zIndex, 10);
  if (z1 < 200) throw new Error("expected z-index >= 200, got " + z1);
  win.togglePopupShade(win.openSymbolPopups[0]);
  win.togglePopupShade(win.openSymbolPopups[0]);

  // 2. Click tok2 -> opens popup 2 (persistent popup 1 remains open!)
  await docClick({ button: 0, detail: 1, target: tok2, preventDefault() {}, stopPropagation() {} });
  const popups2 = body.querySelectorAll(".symbol-popup");
  if (popups2.length !== 2) throw new Error("expected 2 popups open, got " + popups2.length);
  const pop2 = popups2[1];
  if (!pop1.id || !pop2.id || pop1.id === pop2.id) {
    throw new Error("expected unique popup IDs, got " + pop1.id + " and " + pop2.id);
  }
  const z2 = parseInt(pop2.style.zIndex, 10);
  if (z2 <= z1) throw new Error("expected z2 (" + z2 + ") > z1 (" + z1 + ")");

  // 3. Click pop1 -> raises pop1 z-index above pop2
  const pop1Click = pop1.listeners["pointerdown"][0];
  pop1Click({ button: 0, target: pop1, preventDefault() {}, stopPropagation() {} });
  const z1New = parseInt(pop1.style.zIndex, 10);
  if (z1New <= z2) throw new Error("expected pop1 raised to front (" + z1New + " > " + z2 + ")");

  // 4. Test Escape closes highest Z popup first (pop1, which was raised to front!)
  const keydown = winListeners["keydown"][0];
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  const popupsAfterEsc1 = body.querySelectorAll(".symbol-popup");
  if (popupsAfterEsc1.length !== 1) throw new Error("expected 1 popup after Esc, got " + popupsAfterEsc1.length);
  if (popupsAfterEsc1[0] !== pop2) throw new Error("expected pop1 closed first on Esc (highest Z), leaving pop2 open");

  // Close pop2 on second Escape
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  const popupsAfterEsc2 = body.querySelectorAll(".symbol-popup");
  if (popupsAfterEsc2.length !== 0) throw new Error("expected 0 popups after 2nd Esc, got " + popupsAfterEsc2.length);
  if (tok1.classList.contains("sym-active")) throw new Error("expected sym-active removed after all closed");

  // 5. Test Close '×' button on older pinned popup
  await docClick({ button: 0, detail: 1, target: tok1, preventDefault() {}, stopPropagation() {} });
  win.togglePopupShade(win.openSymbolPopups[0]);
  win.togglePopupShade(win.openSymbolPopups[0]);
  await docClick({ button: 0, detail: 1, target: tok2, preventDefault() {}, stopPropagation() {} });
  const [pA, pB] = body.querySelectorAll(".symbol-popup");
  const closeBtnA = pA.querySelector(".sym-btn-close");
  const pAClickHandler = pA.listeners["click"][0];
  await pAClickHandler({ target: closeBtnA, preventDefault() {}, stopPropagation() {} });

  const remaining = body.querySelectorAll(".symbol-popup");
  if (remaining.length !== 1 || remaining[0] !== pB) {
    throw new Error("expected closing pA via close button leaves pB open");
  }
  // Subsequent Escape closes pB
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (body.querySelectorAll(".symbol-popup").length !== 0) throw new Error("expected pB closed after Esc");

  // 6. Mixed Z-axis priority between comments and popups
  // Order: Popup 1 (pinned) -> Comment -> Popup 2
  await docClick({ button: 0, detail: 1, target: tok1, preventDefault() {}, stopPropagation() {} });
  win.togglePopupShade(win.openSymbolPopups[0]);
  win.togglePopupShade(win.openSymbolPopups[0]);
  const p1Mixed = body.querySelectorAll(".symbol-popup")[0];
  win.openCommentForm(tr, 10, "R", false);
  const commentRow = body.querySelector("#inlineNewComment");
  if (!commentRow) throw new Error("expected #inlineNewComment in DOM");
  const commentCard = commentRow.querySelector(".thread-card");
  if (!commentCard.classList.contains("comment-z-active")) {
    throw new Error("expected commentCard to have comment-z-active class");
  }
  await docClick({ button: 0, detail: 1, target: tok2, preventDefault() {}, stopPropagation() {} });
  const p2Mixed = body.querySelectorAll(".symbol-popup")[1];

  const zP1 = parseInt(p1Mixed.style.zIndex, 10);
  const zCard = parseInt(commentCard.style.zIndex, 10);
  const zP2 = parseInt(p2Mixed.style.zIndex, 10);
  if (!(zP2 > zCard && zCard > zP1)) {
    throw new Error("expected zP2 (" + zP2 + ") > zCard (" + zCard + ") > zP1 (" + zP1 + ")");
  }

  // Active textarea inside comment: Escape blurs without closing anything
  const commentTa = commentRow.querySelector("textarea");
  doc.activeElement = commentTa;
  keydown({ key: "Escape", target: commentTa, preventDefault() {}, stopPropagation() {} });
  if (doc.activeElement === commentTa) throw new Error("expected commentTa blurred on 1st Esc");
  if (!body.querySelector("#inlineNewComment")) throw new Error("comment closed on blur Esc");
  if (body.querySelectorAll(".symbol-popup").length !== 2) throw new Error("popup closed on blur Esc");

  // Next Escape closes p2Mixed (highest z)
  doc.activeElement = body;
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (body.querySelectorAll(".symbol-popup").length !== 1 || body.querySelectorAll(".symbol-popup")[0] !== p1Mixed) {
    throw new Error("expected p2Mixed closed first on Esc (highest z)");
  }
  if (!body.querySelector("#inlineNewComment")) throw new Error("comment closed prematurely");

  // Next Escape closes #inlineNewComment
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (body.querySelector("#inlineNewComment")) throw new Error("expected comment closed on Esc");
  if (body.querySelectorAll(".symbol-popup").length !== 1) throw new Error("p1Mixed closed prematurely");

  // Next Escape closes p1Mixed
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (body.querySelectorAll(".symbol-popup").length !== 0) throw new Error("expected all closed");

  // 7. Dynamic reordering by focusing comment card above newer popup
  await docClick({ button: 0, detail: 1, target: tok1, preventDefault() {}, stopPropagation() {} });
  win.togglePopupShade(win.openSymbolPopups[0]);
  win.togglePopupShade(win.openSymbolPopups[0]);
  const p1Reorder = body.querySelectorAll(".symbol-popup")[0];
  win.openCommentForm(tr, 10, "R", false);
  const cRow2 = body.querySelector("#inlineNewComment");
  const cCard2 = cRow2.querySelector(".thread-card");
  await docClick({ button: 0, detail: 1, target: tok2, preventDefault() {}, stopPropagation() {} });
  const p2Reorder = body.querySelectorAll(".symbol-popup")[1];

  // Focusing comment card elevates it above p2Reorder
  const focusinListeners = docListeners["focusin"] || [];
  focusinListeners.forEach(fn => fn({ target: cCard2 }));
  const zCardElevated = parseInt(cCard2.style.zIndex, 10);
  const zP2Base = parseInt(p2Reorder.style.zIndex, 10);
  if (zCardElevated <= zP2Base) {
    throw new Error("expected elevated comment card zIndex (" + zCardElevated + ") > p2 (" + zP2Base + ")");
  }

  // Escape closes elevated comment card first!
  doc.activeElement = body;
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (body.querySelector("#inlineNewComment")) throw new Error("expected elevated comment closed first on Esc");
  if (body.querySelectorAll(".symbol-popup").length !== 2) throw new Error("popups prematurely closed");

  // Next Escape closes p2Reorder
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (body.querySelectorAll(".symbol-popup").length !== 1 || body.querySelectorAll(".symbol-popup")[0] !== p1Reorder) {
    throw new Error("expected p2Reorder closed after comment");
  }

  // Final Escape closes p1Reorder
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (body.querySelectorAll(".symbol-popup").length !== 0) throw new Error("expected all closed");

  // 8. Symbol popup body remains visible while finding refs or impls (no minimization)
  let refsResolve = null;
  let refsReject = null;
  let refsSignal = null;
  let implsResolve = null;
  let implsSignal = null;

  win._customFetch = (url, opts) => {
    if (url.includes("/api/symbol/refs")) {
      refsSignal = opts.signal;
      return new Promise((resolve, reject) => {
        refsResolve = resolve;
        refsReject = reject;
        if (opts.signal) {
          opts.signal.addEventListener("abort", () => {
            const err = new Error("The user aborted a request.");
            err.name = "AbortError";
            reject(err);
          });
        }
      });
    }
    if (url.includes("/api/symbol/impls")) {
      implsSignal = opts.signal;
      return new Promise((resolve, reject) => {
        implsResolve = resolve;
        if (opts.signal) {
          opts.signal.addEventListener("abort", () => {
            const err = new Error("The user aborted a request.");
            err.name = "AbortError";
            reject(err);
          });
        }
      });
    }
    return null;
  };

  // Open unresized popup
  await docClick({ button: 0, detail: 1, target: tok1, preventDefault() {}, stopPropagation() {} });
  const pUnresized = body.querySelectorAll(".symbol-popup")[0];
  if (!pUnresized) throw new Error("expected popup open for tok1");
  if (pUnresized.style.height) throw new Error("expected unresized popup height to be unset");
  if (pUnresized.classList.contains("sym-resized-h")) throw new Error("expected unresized popup not to have sym-resized-h");
  if (!pUnresized.querySelector(".sym-popup-sig")) throw new Error("expected sym-popup-sig present");
  if (!pUnresized.querySelector(".sym-popup-snippet")) throw new Error("expected sym-popup-snippet present");
  if (!pUnresized.querySelector(".sym-btn-refs")) throw new Error("expected sym-btn-refs present");
  if (!pUnresized.querySelector(".sym-btn-impls")) throw new Error("expected sym-btn-impls present");

  const pClick = pUnresized.listeners["click"][0];

  // Click Refs: while fetch is pending, body (sig & snippet) remains in DOM, header shows finding
  const refsBtn = pUnresized.querySelector(".sym-btn-refs");
  const refsPromise = pClick({ button: 0, detail: 1, target: refsBtn, preventDefault() {}, stopPropagation() {} });

  const locElRefs = pUnresized.querySelector(".sym-popup-loc");
  if (!locElRefs || !locElRefs.textContent.includes("Finding references…")) {
    throw new Error("expected locEl to display Finding references…");
  }
  if (!pUnresized.querySelector(".sym-btn-back")) {
    throw new Error("expected back button during pending refs");
  }
  if (!pUnresized.querySelector(".sym-popup-sig")) {
    throw new Error("expected sym-popup-sig to remain visible while finding refs (prevent minimizing)");
  }
  if (!pUnresized.querySelector(".sym-popup-snippet")) {
    throw new Error("expected sym-popup-snippet to remain visible while finding refs (prevent minimizing)");
  }
  if (pUnresized.style.height) {
    throw new Error("expected popup height still unconstrained (not modified)");
  }

  // Cancel refs fetch via Back button
  const backBtnRefs = pUnresized.querySelector(".sym-btn-back");
  await pClick({ button: 0, detail: 1, target: backBtnRefs, preventDefault() {}, stopPropagation() {} });
  if (!refsSignal || !refsSignal.aborted) {
    throw new Error("expected refs fetch aborted by Back button");
  }
  await refsPromise;

  // Verify restored definition header and body intact
  if (!pUnresized.querySelector(".sym-btn-refs")) {
    throw new Error("expected sym-btn-refs restored after Back");
  }
  if (!pUnresized.querySelector(".sym-popup-sig") || !pUnresized.querySelector(".sym-popup-snippet")) {
    throw new Error("expected definition sig & snippet intact after Back");
  }

  // Click Impls: while fetch is pending, body remains in DOM
  const implsBtn = pUnresized.querySelector(".sym-btn-impls");
  const implsPromise = pClick({ button: 0, detail: 1, target: implsBtn, preventDefault() {}, stopPropagation() {} });

  const locElImpls = pUnresized.querySelector(".sym-popup-loc");
  if (!locElImpls || !locElImpls.textContent.includes("Finding")) {
    throw new Error("expected locEl to display Finding interfaces… / implementations…");
  }
  if (!pUnresized.querySelector(".sym-btn-back")) {
    throw new Error("expected back button during pending impls");
  }
  if (!pUnresized.querySelector(".sym-popup-sig")) {
    throw new Error("expected sym-popup-sig to remain visible while finding impls (prevent minimizing)");
  }
  if (!pUnresized.querySelector(".sym-popup-snippet")) {
    throw new Error("expected sym-popup-snippet to remain visible while finding impls (prevent minimizing)");
  }

  // Resolve impls fetch
  implsResolve({
    ok: true,
    json: () => Promise.resolve({
      found: true,
      symbol_name: "SymA",
      direction: "implements",
      implementations: [{ name: "MyInterface", line: 20, group: "repo" }]
    })
  });
  await implsPromise;

  if (!pUnresized.querySelector(".sym-ref-row")) {
    throw new Error("expected impls rows rendered after fetch resolves");
  }

  // Back button from completed impls restores definition
  const backBtnImpls = pUnresized.querySelector(".sym-btn-back");
  await pClick({ button: 0, detail: 1, target: backBtnImpls, preventDefault() {}, stopPropagation() {} });
  if (!pUnresized.querySelector(".sym-popup-sig") || !pUnresized.querySelector(".sym-popup-snippet")) {
    throw new Error("expected definition restored after Back from impls");
  }

  // Close popup
  const closeBtn = pUnresized.querySelector(".sym-btn-close");
  await pClick({ button: 0, detail: 1, target: closeBtn, preventDefault() {}, stopPropagation() {} });
  if (body.querySelectorAll(".symbol-popup").length !== 0) {
    throw new Error("expected popup closed via close button");
  }

  // 9. Hanging file header z-index, zero gap, and top clearance below hanging bar
  const css = fs.readFileSync(path.join("static", "style.css"), "utf8");
  if (!/\.diff-file-header\s*\{[^}]*top:\s*var\(--app-header-h,\s*42px\)/.test(css)) {
    throw new Error("expected .diff-file-header to use top: var(--app-header-h, 42px) in style.css");
  }
  if (!/\.diff-file-header\.is-hanging\s*\{[^}]*z-index:\s*890/.test(css) ||
      !/\.diff-file-header\.is-hanging\s*\{[^}]*border-radius:\s*0/.test(css)) {
    throw new Error("expected .diff-file-header.is-hanging to set z-index: 890 and border-radius: 0 in style.css");
  }
  if (!/\.diff-file-header\s*\{[^}]*z-index:\s*10/.test(css)) {
    throw new Error("expected .diff-file-header to have default z-index: 10 in style.css");
  }
  if (!/header\.app-header[^{]*\{[^}]*z-index:\s*900/.test(css)) {
    throw new Error("expected header.app-header to have z-index: 900 in style.css");
  }

  // Verify syncAppHeaderHeight dynamically sets --app-header-h to rendered app-header height
  header.offsetHeight = 42.5;
  header._rect = { top: 0, left: 0, bottom: 42.5, right: 1000, width: 1000, height: 42.5 };

  const syncedH = win.syncAppHeaderHeight();
  if (syncedH !== 42.5) {
    throw new Error("expected syncAppHeaderHeight to return 42.5, got " + syncedH);
  }
  if (doc.documentElement.style.getPropertyValue("--app-header-h") !== "42.5px") {
    throw new Error("expected --app-header-h set to 42.5px on documentElement.style, got " + doc.documentElement.style.getPropertyValue("--app-header-h"));
  }

  const secHanging = createMockElement("DIV");
  secHanging.className = "diff-file-section";
  secHanging._rect = { top: 42.5, left: 0, bottom: 500, right: 1000, width: 1000, height: 457.5 };
  body.appendChild(secHanging);

  const hdrHanging = createMockElement("DIV");
  hdrHanging.className = "diff-file-header";
  hdrHanging.offsetHeight = 35;
  hdrHanging._rect = { top: 42.5, left: 0, bottom: 77.5, right: 1000, width: 1000, height: 35 };
  secHanging.appendChild(hdrHanging);

  const secLower = createMockElement("DIV");
  secLower.className = "diff-file-section";
  secLower._rect = { top: 300, left: 0, bottom: 700, right: 1000, width: 1000, height: 400 };
  body.appendChild(secLower);

  const hdrLower = createMockElement("DIV");
  hdrLower.className = "diff-file-header";
  hdrLower.offsetHeight = 35;
  hdrLower._rect = { top: 300, left: 0, bottom: 335, right: 1000, width: 1000, height: 35 };
  secLower.appendChild(hdrLower);

  win.updateHangingFileHeaders(42.5);
  if (!hdrHanging.classList.contains("is-hanging")) {
    throw new Error("expected stuck file header at secRect.top = 42.5 to have is-hanging class");
  }
  if (hdrLower.classList.contains("is-hanging")) {
    throw new Error("expected lower unstuck file header not to have is-hanging class");
  }

  // Collapsed section does not hang
  secHanging.classList.add("collapsed");
  win.updateHangingFileHeaders(42.5);
  if (hdrHanging.classList.contains("is-hanging")) {
    throw new Error("expected collapsed file header not to have is-hanging class");
  }
  secHanging.classList.remove("collapsed");

  // Scrolled completely off top past stickyTop (bottom <= 42.5)
  secHanging._rect = { top: -600, left: 0, bottom: 42.5, right: 1000, width: 1000, height: 642.5 };
  win.updateHangingFileHeaders(42.5);
  if (hdrHanging.classList.contains("is-hanging")) {
    throw new Error("expected off-screen file header not to have is-hanging class");
  }

  secHanging.remove();
  secLower.remove();

  // Y-axis clearance below approaching file header
  header.offsetHeight = 48;
  header._rect = { top: 0, left: 0, bottom: 48, right: 1000, width: 1000, height: 48 };

  const secClearance = createMockElement("DIV");
  secClearance.className = "diff-file-section";
  secClearance._rect = { top: 70, left: 0, bottom: 500, right: 1000, width: 1000, height: 430 };
  body.appendChild(secClearance);

  const hdrClearance = createMockElement("DIV");
  hdrClearance.className = "diff-file-header";
  hdrClearance.offsetHeight = 35;
  hdrClearance._rect = { top: 70, left: 0, bottom: 105, right: 1000, width: 1000, height: 35 };
  secClearance.appendChild(hdrClearance);

  const topClearance = win.getPopupTopClearance();
  if (topClearance < 83) {
    throw new Error("expected top clearance >= 83 (appHeader 48 + hdr 35), got " + topClearance);
  }

  const tokTop = createMockElement("SPAN");
  tokTop.className = "tok-id";
  tokTop.textContent = "SymA";
  tokTop._rect = { top: 20, left: 100, bottom: 30, right: 150, width: 50, height: 10 };
  td.appendChild(tokTop);

  await docClick({ button: 0, detail: 1, target: tokTop, preventDefault() {}, stopPropagation() {} });
  const pTop = body.querySelectorAll(".symbol-popup")[0];
  if (!pTop) throw new Error("expected popup open for tokTop");
  const topVal = parseFloat(pTop.style.top);
  if (topVal < 89) {
    throw new Error("expected popup top clamped >= 89 (48 + 35 + 6), got " + topVal);
  }

  // Drag upward to clientY = 10 -> must clamp at >= 89
  const pointerdownListeners = pTop.listeners["pointerdown"] || [];
  const pointermoveListeners = winListeners["pointermove"] || [];
  const pointerupListeners = winListeners["pointerup"] || [];
  const hdrEl = pTop.querySelector(".sym-popup-header");

  for (const fn of pointerdownListeners) {
    fn({ button: 0, clientX: 120, clientY: topVal + 10, target: hdrEl, preventDefault() {}, stopPropagation() {} });
  }
  for (const fn of pointermoveListeners) {
    fn({ clientX: 120, clientY: 10, preventDefault() {}, stopPropagation() {} });
  }
  for (const fn of pointerupListeners) {
    fn({ clientX: 120, clientY: 10, preventDefault() {}, stopPropagation() {} });
  }
  const draggedTop = parseFloat(pTop.style.top);
  if (draggedTop < 83) {
    throw new Error("expected dragged popup top clamped >= 83, got " + draggedTop);
  }

  // Top-resize ('n' handle) upward to clientY = 10 -> must clamp at >= 83
  const nHandle = pTop.querySelector(".sym-resize-n");
  if (nHandle) {
    const handleDownListeners = nHandle.listeners["pointerdown"] || [];
    for (const fn of handleDownListeners) {
      fn({ button: 0, clientX: 150, clientY: draggedTop, target: nHandle, preventDefault() {}, stopPropagation() {} });
    }
    for (const fn of pointermoveListeners) {
      fn({ clientX: 150, clientY: 10, preventDefault() {}, stopPropagation() {} });
    }
    for (const fn of pointerupListeners) {
      fn({ clientX: 150, clientY: 10, preventDefault() {}, stopPropagation() {} });
    }
    const resizedTop = parseFloat(pTop.style.top);
    if (resizedTop < 83) {
      throw new Error("expected resized popup top clamped >= 83, got " + resizedTop);
    }
  }

  const pClose = pTop.querySelector(".sym-btn-close");
  const pClickTop = pTop.listeners["click"][0];
  await pClickTop({ button: 0, detail: 1, target: pClose, preventDefault() {}, stopPropagation() {} });
  if (body.querySelectorAll(".symbol-popup").length !== 0) {
    throw new Error("expected pTop closed");
  }

  secClearance.remove();
  tokTop.remove();
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TestSymbolPopupPersistenceAndStacking failed: %v\n%s", err, string(out))
	}
}

func TestSymbolPopupDragAndResize(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur.textContent = (cur.textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT"].includes(tag)) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    nodeType: 1,
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    style: {},
    children: [],
    parentElement: null,
    get parentNode() { return this.parentElement; },
    get childNodes() { return this.children; },
    _textContent: "",
    get textContent() {
      let t = this._textContent || "";
      for (const ch of this.children) {
        t += ch.textContent;
      }
      return t;
    },
    set textContent(v) {
      this._textContent = String(v);
    },
    listeners: {},
    get offsetWidth() {
      if (this._offsetWidth !== undefined) return this._offsetWidth;
      if (this.classList && this.classList.contains("sym-popup-actions")) return 60;
      if (this.classList && this.classList.contains("sym-btn-back")) return 24;
      if (this.classList && this.classList.contains("sym-popup-loc")) return 100;
      if (this.tagName === "BUTTON" || this.tagName === "A" || this.tagName === "SPAN") return 24;
      return 300;
    },
    set offsetWidth(v) { this._offsetWidth = v; },
    get offsetHeight() {
      if (this._offsetHeight !== undefined) return this._offsetHeight;
      if (this._rect && this._rect.height !== undefined) return this._rect.height;
      return 200;
    },
    set offsetHeight(v) { this._offsetHeight = v; },
    scrollTop: 0,
    scrollLeft: 0,
    _rect: { top: 100, left: 100, bottom: 300, right: 400, width: 300, height: 200 },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    removeAttribute(k) { delete this.attributes[k]; },
    getBoundingClientRect() { return { ...this._rect }; },
    focus() { doc.activeElement = this; this._focused = true; },
    blur() { this._blurred = true; this._focused = false; if (doc.activeElement === this) doc.activeElement = null; },
    addEventListener(event, fn) {
      if (!this.listeners[event]) this.listeners[event] = [];
      this.listeners[event].push(fn);
    },
    removeEventListener(event, fn) {
      if (this.listeners[event]) {
        this.listeners[event] = this.listeners[event].filter(f => f !== fn);
      }
    },
    appendChild(child) {
      if (!child) return child;
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    insertBefore(newChild, refChild) {
      if (!newChild) return newChild;
      newChild.parentElement = this;
      const idx = this.children.indexOf(refChild);
      if (idx !== -1) this.children.splice(idx, 0, newChild);
      else this.children.push(newChild);
      return newChild;
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    get classList() {
      return {
        contains: (c) => (el.className || "").split(/\s+/).includes(c),
        add: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.add(c));
          el.className = Array.from(parts).join(" ");
        },
        remove: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.delete(c));
          el.className = Array.from(parts).join(" ");
        }
      };
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (node.tagName && matchesSel(node, sel)) return node;
        node = node.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const results = [];
      function search(n) {
        for (const ch of n.children) {
          if (matchesSel(ch, sel)) results.push(ch);
          search(ch);
        }
      }
      search(this);
      return results;
    },
    set innerHTML(val) {
      this._innerHTML = val;
      this.children = parseHtml(val, this);
    },
    get innerHTML() {
      return this._innerHTML || "";
    }
  };
  return el;
}

function matchesSel(node, sel) {
  const parts = sel.split(",").map(s => s.trim());
  for (const p of parts) {
    if (p.includes("[")) {
      const bIdx = p.indexOf("[");
      const tagPart = p.slice(0, bIdx);
      const attrPart = p.slice(bIdx + 1, -1);
      if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
        continue;
      }
      if (attrPart.includes("=")) {
        const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
        if (node.getAttribute && node.getAttribute(k) === v) return true;
      } else {
        if (node.hasAttribute && node.hasAttribute(attrPart)) return true;
      }
      continue;
    }
    if (p.includes(".")) {
      const segs = p.split(".");
      const tagPart = segs[0];
      const clsParts = segs.slice(1);
      if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
        continue;
      }
      if (clsParts.every(c => node.classList && node.classList.contains && node.classList.contains(c))) {
        return true;
      }
      continue;
    }
    if (p.startsWith("#")) {
      if (node.id === p.slice(1)) return true;
    } else if (p.toLowerCase() === (node.tagName || "").toLowerCase()) {
      return true;
    }
  }
  return false;
}

const docListeners = {};
const winListeners = {};
const body = createMockElement("BODY");
const header = createMockElement("HEADER");
header.className = "app-header";
header._rect = { top: 0, left: 0, bottom: 48, right: 1000, width: 1000, height: 48 };
body.appendChild(header);

const doc = {
  readyState: "complete",
  documentElement: {
    clientWidth: 1000,
    clientHeight: 1000,
    style: {
      _props: {},
      setProperty(k, v) { this._props[k] = String(v); },
      getPropertyValue(k) { return this._props[k] || ""; },
    },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  body: body,
  getElementById(id) {
    if (id === "shortcutsModal") return null;
    const all = body.querySelectorAll("#" + id);
    return all.length > 0 ? all[0] : null;
  },
  createElement(tag) { return createMockElement(tag); },
  createTreeWalker(root) { return { currentNode: root, nextNode() { return null; } }; },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (docListeners[event]) docListeners[event] = docListeners[event].filter(f => f !== fn);
  },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); }
};

const win = {
  document: doc,
  NodeFilter: { SHOW_TEXT: 4 },
  location: { pathname: "/r/test/c/abc", search: "", hash: "", reload() {} },
  addEventListener(event, fn) {
    if (!winListeners[event]) winListeners[event] = [];
    winListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (winListeners[event]) winListeners[event] = winListeners[event].filter(f => f !== fn);
  },
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; },
  window: null,
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  fetch(url, opts) {
    if (url.includes("/api/symbol/refs")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "TestSym",
          refs: [
            { group: "change", path: "foo.go", line: 42, in_repo: true, display: "foo.go", line_html: "foo()" },
            { group: "package", path: "pkg/other.go", line: 88, in_repo: true, display: "pkg/other.go", line_html: "TestSym()" }
          ]
        })
      });
    }
    if (url.includes("/api/symbol/impls")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "TestSym",
          direction: "interfaces",
          implementations: [
            { group: "change", name: "LocalIface", path: "foo.go", line: 12, utf16_col: 5, in_repo: true, display: "foo.go", line_html: "type LocalIface interface" },
            { group: "repo", name: "RepoIface", path: "pkg/iface.go", line: 30, utf16_col: 5, in_repo: true, display: "pkg/iface.go", line_html: "type RepoIface interface" },
            { group: "external", name: "Reader", pkg: "io", pkg_file: "io.go", line: 86, utf16_col: 5, in_repo: false, display: "io/io.go", line_html: "type Reader interface" }
          ]
        })
      });
    }
    if (url.includes("site=1")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "RefResolvedSym",
          def_path: "foo.go",
          def_line: 42,
          sym_line: 40,
          sym_col: 5,
          in_repo: true,
          display_path: "foo.go",
          signature: "func RefResolvedSym()",
          can_refs: true,
          can_impls: false,
          snippet: [
            { line: 40, html: "func RefResolvedSym()", active: false },
            { line: 42, html: "  foo()", active: true }
          ]
        })
      });
    }
    if (url.includes("line=12")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "LocalIface",
          def_path: "foo.go",
          def_line: 12,
          in_repo: true,
          display_path: "foo.go",
          signature: "type LocalIface interface",
          can_refs: true,
          can_impls: false,
          snippet: [
            { line: 12, html: "type LocalIface interface", active: true }
          ]
        })
      });
    }
    if (url.includes("line=77")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "ChainedSym",
          def_path: "foo.go",
          def_line: 77,
          in_repo: true,
          display_path: "foo.go",
          signature: "func ChainedSym()",
          can_refs: false,
          can_impls: false,
          snippet: [
            { line: 75, html: "func Before()", active: false },
            { line: 77, html: "func ChainedSym()", active: true },
            { line: 79, html: "func After()", active: false }
          ]
        })
      });
    }
    if (url.includes("line=55")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "ExternalSym",
          def_path: "pkg/outside.go",
          def_line: 99,
          in_repo: true,
          display_path: "pkg/outside.go",
          signature: "func ExternalSym()",
          can_refs: false,
          can_impls: false,
        })
      });
    }
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        found: true,
        symbol_name: "TestSym",
        def_path: "foo.go",
        def_line: 42,
        in_repo: true,
        display_path: "foo.go",
        signature: "func TestSym()",
        can_refs: true,
        can_impls: true,
        impls_direction: "interfaces",
        snippet: [
          { line: 40, html: "func Before()", active: false },
          { line: 42, html: "func TestSym() {", active: true },
          { line: 43, html: '<span class="tok-id">TargetCall</span>()', active: false },
          { line: 45, html: "}", active: false }
        ]
      })
    });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  NodeFilter: win.NodeFilter,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  fetch: win.fetch,
  URLSearchParams,
  Promise,
  setTimeout,
  clearTimeout,
  console,
  AbortController,
});
vm.runInContext(code, ctx);

(async () => {
  const container = createMockElement("DIV");
  container.className = "diff-container";
  container.setAttribute("data-file-path", "foo.go");
  container.setAttribute("data-repo", "testrepo");
  container.setAttribute("data-change", "123");
  body.appendChild(container);

  const tr = createMockElement("TR");
  tr.setAttribute("data-line-r", "10");
  container.appendChild(tr);

  const td = createMockElement("TD");
  td.setAttribute("data-side", "R");
  tr.appendChild(td);

  const tok = createMockElement("SPAN");
  tok.className = "tok-id";
  tok.textContent = "TestSym";
  tok._rect = { top: 100, left: 100, bottom: 120, right: 150, width: 50, height: 20 };
  td.appendChild(tok);

  const docClick = docListeners["click"][0];
  await docClick({ button: 0, detail: 1, target: tok, preventDefault() {}, stopPropagation() {} });

  const pDrag = body.querySelectorAll(".symbol-popup")[0];
  const headerEl = pDrag.querySelector(".sym-popup-header");
  const pPointerDown = pDrag.listeners["pointerdown"][0];

  // Drag header by dx=50, dy=30
  pPointerDown({ button: 0, clientX: 100, clientY: 100, target: headerEl, preventDefault() {}, stopPropagation() {} });
  const pMove = winListeners["pointermove"][0];
  const pUp = winListeners["pointerup"][0];
  pMove({ clientX: 150, clientY: 130 });
  pUp({});
  const leftAfterDrag = parseFloat(pDrag.style.left);
  const topAfterDrag = parseFloat(pDrag.style.top);
  if (isNaN(leftAfterDrag) || isNaN(topAfterDrag)) throw new Error("drag failed to update left/top");

  // Resize Handle E (+50px width)
  const handleE = pDrag.querySelector(".sym-resize-e");
  pPointerDown({ button: 0, clientX: 200, clientY: 200, target: handleE, preventDefault() {}, stopPropagation() {} });
  const resizeMove = winListeners["pointermove"][0];
  const resizeUp = winListeners["pointerup"][0];
  resizeMove({ clientX: 250, clientY: 200 });
  resizeUp({});
  if (pDrag.style.width !== "350px") throw new Error("expected width 350px, got " + pDrag.style.width);

  // Resize Handle S (+80px height) -> sets sym-resized-h
  const handleS = pDrag.querySelector(".sym-resize-s");
  pPointerDown({ button: 0, clientX: 200, clientY: 200, target: handleS, preventDefault() {}, stopPropagation() {} });
  const sMove = winListeners["pointermove"][0];
  const sUp = winListeners["pointerup"][0];
  sMove({ clientX: 200, clientY: 280 });
  sUp({});
  if (pDrag.style.height !== "280px") throw new Error("expected height 280px, got " + pDrag.style.height);
  if (!pDrag.classList.contains("sym-resized-h")) throw new Error("expected sym-resized-h class added on vertical resize");

  // Test min width clamping (260px)
  const handleW = pDrag.querySelector(".sym-resize-w");
  pPointerDown({ button: 0, clientX: 100, clientY: 100, target: handleW, preventDefault() {}, stopPropagation() {} });
  const wMove = winListeners["pointermove"][0];
  const wUp = winListeners["pointerup"][0];
  wMove({ clientX: 400, clientY: 100 });
  wUp({});
  if (pDrag.style.width !== "260px") throw new Error("expected min width 260px, got " + pDrag.style.width);

  // Test dynamic min width with wide actions
  const actionsEl = pDrag.querySelector(".sym-popup-actions");
  actionsEl.offsetWidth = 180;
  const dynMinW = win.getPopupMinWidth(pDrag);
  if (dynMinW < 290) throw new Error("expected dynMinW >= 290, got " + dynMinW);
  pPointerDown({ button: 0, clientX: 100, clientY: 100, target: handleW, preventDefault() {}, stopPropagation() {} });
  const dynWMove = winListeners["pointermove"][0];
  const dynWUp = winListeners["pointerup"][0];
  dynWMove({ clientX: 400, clientY: 100 });
  dynWUp({});
  if (pDrag.style.width !== dynMinW + "px") {
    throw new Error("expected width clamped to dynamic min width " + dynMinW + "px, got " + pDrag.style.width);
  }
  actionsEl.offsetWidth = 60; // restore
  pPointerDown({ button: 0, clientX: 100, clientY: 100, target: handleW, preventDefault() {}, stopPropagation() {} });
  const restoreWMove = winListeners["pointermove"][0];
  const restoreWUp = winListeners["pointerup"][0];
  restoreWMove({ clientX: 400, clientY: 100 });
  restoreWUp({});
  if (pDrag.style.width !== "260px") throw new Error("expected restored width 260px, got " + pDrag.style.width);

  // Test Refs view preserves dimensions and sym-resized-h, and clicking a ref row resolves inside popup
  const refsBtn = pDrag.querySelector(".sym-btn-refs");
  const pDragClick = pDrag.listeners["click"][0];
  await pDragClick({ target: refsBtn, preventDefault() {}, stopPropagation() {} });
  if (pDrag.style.width !== "260px") throw new Error("width lost on Refs");
  if (pDrag.style.height !== "280px") throw new Error("height lost on Refs");
  if (!pDrag.classList.contains("sym-resized-h")) throw new Error("sym-resized-h lost on Refs");
  const refRows = pDrag.querySelectorAll(".sym-ref-row");
  if (refRows.length !== 2) throw new Error("expected 2 ref rows, got " + refRows.length);
  if (!refRows[0].hasAttribute("data-resolve-sym") || refRows[0].getAttribute("data-site") !== "1") {
    throw new Error("expected ref row to have data-resolve-sym and data-site='1'");
  }
  const refListBody = pDrag.querySelector(".sym-list-body");
  if (!refListBody) throw new Error("expected .sym-list-body in Refs view");
  const refListInner = refListBody.querySelector(".sym-list-inner");
  if (!refListInner) throw new Error("expected .sym-list-inner inside .sym-list-body in Refs view");
  if (!refListInner.querySelector(".sym-group-header") || refListInner.querySelectorAll(".sym-ref-row").length !== 2) {
    throw new Error("expected Refs group header and rows wrapped inside .sym-list-inner");
  }
  refListBody.scrollTop = 45;

  // Clicking ref row resolves site=1 inside the popup without navigation
  await pDragClick({ target: refRows[0], preventDefault() {}, stopPropagation() {} });
  const refLoc = pDrag.querySelector(".sym-popup-loc");
  if (!refLoc || !refLoc.textContent.includes("foo.go:42")) {
    throw new Error("expected ref row to resolve in-popup to foo.go:42, got: " + (refLoc && refLoc.textContent));
  }

  // Test Back button restores Refs view and its scrollTop
  const backBtn = pDrag.querySelector(".sym-btn-back");
  await pDragClick({ target: backBtn, preventDefault() {}, stopPropagation() {} });
  const restoredRefBody = pDrag.querySelector(".sym-list-body");
  if (!restoredRefBody || restoredRefBody.scrollTop !== 45) {
    throw new Error("expected Refs scrollTop 45 restored, got " + (restoredRefBody && restoredRefBody.scrollTop));
  }
  // Back to definition
  await pDragClick({ target: pDrag.querySelector(".sym-btn-back"), preventDefault() {}, stopPropagation() {} });
  if (pDrag.style.width !== "260px") throw new Error("width lost after Back");
  if (pDrag.style.height !== "280px") throw new Error("height lost after Back");
  if (!pDrag.classList.contains("sym-resized-h")) throw new Error("sym-resized-h lost after Back");

  // In-page loc link must NOT show '→'
  const inPageLocLink = pDrag.querySelector("a.sym-popup-loc-link");
  if (!inPageLocLink || inPageLocLink.classList.contains("sym-will-nav") || (inPageLocLink.innerHTML || "").includes("→")) {
    throw new Error("expected in-page definition NOT to show '→'");
  }

  // Test Impls / Interfaces view: rows wrapped in .sym-list-inner, clicking resolves inside popup, Back restores scrollTop and scrollLeft
  const implsBtn = pDrag.querySelector(".sym-btn-impls");
  await pDragClick({ target: implsBtn, preventDefault() {}, stopPropagation() {} });
  const implRows = pDrag.querySelectorAll(".sym-ref-row");
  if (implRows.length !== 3) throw new Error("expected 3 impl/interface rows, got " + implRows.length);
  if (!implRows[0].hasAttribute("data-resolve-sym")) {
    throw new Error("expected impl row to have data-resolve-sym");
  }
  const implListBody = pDrag.querySelector(".sym-list-body");
  if (!implListBody) throw new Error("expected .sym-list-body in Impls view");
  const implListInner = implListBody.querySelector(".sym-list-inner");
  if (!implListInner) throw new Error("expected .sym-list-inner inside .sym-list-body in Impls view");
  if (!implListInner.querySelector(".sym-group-header") || implListInner.querySelectorAll(".sym-ref-row").length !== 3) {
    throw new Error("expected Impls group header and rows wrapped inside .sym-list-inner");
  }
  implListBody.scrollTop = 70;
  implListBody.scrollLeft = 64;

  // Clicking impl row resolves inside popup
  await pDragClick({ target: implRows[0], preventDefault() {}, stopPropagation() {} });
  const implLoc = pDrag.querySelector(".sym-popup-loc");
  if (!implLoc || !implLoc.textContent.includes("foo.go:12")) {
    throw new Error("expected impl row to resolve in-popup to foo.go:12, got: " + (implLoc && implLoc.textContent));
  }

  // Back restores Impls view and both scrollTop and scrollLeft
  await pDragClick({ target: pDrag.querySelector(".sym-btn-back"), preventDefault() {}, stopPropagation() {} });
  const restoredImplBody = pDrag.querySelector(".sym-list-body");
  if (!restoredImplBody || restoredImplBody.scrollTop !== 70) {
    throw new Error("expected Impls scrollTop 70 restored, got " + (restoredImplBody && restoredImplBody.scrollTop));
  }
  if (restoredImplBody.scrollLeft !== 64) {
    throw new Error("expected Impls scrollLeft 64 restored, got " + restoredImplBody.scrollLeft);
  }
  await pDragClick({ target: pDrag.querySelector(".sym-btn-back"), preventDefault() {}, stopPropagation() {} });

  // Test Loc Link Click vs Drag
  const locLink = pDrag.querySelector("a.sym-popup-loc-link");
  if (!locLink) throw new Error("expected a.sym-popup-loc-link in popup header");

  // < 4px movement -> jump click not prevented
  pPointerDown({ button: 0, clientX: 100, clientY: 100, target: locLink, preventDefault() {}, stopPropagation() {} });
  const pMoveSmall = winListeners["pointermove"][0];
  const pUpSmall = winListeners["pointerup"][0];
  pMoveSmall({ clientX: 101, clientY: 101 });
  pUpSmall({});
  let jumpEventPrevented = false;
  await pDragClick({
    target: locLink,
    preventDefault() { jumpEventPrevented = true; },
    stopPropagation() {}
  });
  if (jumpEventPrevented) throw new Error("expected jump click NOT prevented for < 4px movement");

  // >= 4px movement -> moves popup and suppresses jump click
  pPointerDown({ button: 0, clientX: 100, clientY: 100, target: locLink, preventDefault() {}, stopPropagation() {} });
  const pMoveLarge = winListeners["pointermove"][0];
  const pUpLarge = winListeners["pointerup"][0];
  pMoveLarge({ clientX: 120, clientY: 115 });
  pUpLarge({});
  let dragLinkClickPrevented = false;
  await pDragClick({
    target: locLink,
    preventDefault() { dragLinkClickPrevented = true; },
    stopPropagation() {}
  });
  if (!dragLinkClickPrevented) throw new Error("expected jump click suppressed after >= 4px drag on loc link");

  // Test sticky enclosing-func click opens symbol popup, and outside-diff def shows '→' on header
  const scopeBar = createMockElement("BUTTON");
  scopeBar.className = "diff-header-scope";
  scopeBar.setAttribute("data-decl-line", "55");
  scopeBar.setAttribute("data-name-line", "55");
  scopeBar.setAttribute("data-name-col", "5");
  scopeBar._scopeContainer = container;
  scopeBar._rect = { top: 54, left: 300, bottom: 76, right: 500, width: 200, height: 22 };
  body.appendChild(scopeBar);

  await win.handleStickyScopeClick({ preventDefault() {}, stopPropagation() {} }, scopeBar);
  if (scopeBar.classList.contains("sym-active")) {
    throw new Error("expected sticky scope button NOT to keep sym-active class");
  }
  const allPops = body.querySelectorAll(".symbol-popup");
  if (allPops.length !== 2) throw new Error("expected sticky scope click to open 2nd symbol popup, got " + allPops.length);
  const pExtDef = allPops[1];
  const extLocLink = pExtDef.querySelector("a.sym-popup-loc-link");
  if (!extLocLink || !extLocLink.classList.contains("sym-will-nav") || !extLocLink.querySelector(".sym-nav-arrow")) {
    throw new Error("expected outside-diff header location link to show '→'");
  }

  // Test chained token click inside snippet scrolls to active line
  const snipSnippet = pDrag.querySelector(".sym-popup-snippet");
  if (!snipSnippet) throw new Error("expected snippet in pDrag");
  const tokCall = pDrag.querySelector(".tok-id");
  if (tokCall) {
    await pDragClick({
      target: tokCall,
      button: 0,
      detail: 1,
      preventDefault() {},
      stopPropagation() {}
    });
    const chainedActiveRow = pDrag.querySelector(".sym-snippet-row.sym-snippet-active");
    if (!chainedActiveRow) throw new Error("expected active row in chained snippet");
    if (pDrag.querySelector(".sym-btn-back")) {
      await pDragClick({ target: pDrag.querySelector(".sym-btn-back"), preventDefault() {}, stopPropagation() {} });
    }
  }

  // Test bottom-of-viewport popup opening overflows naturally without maxHeight clamp
  const bottomTok = createMockElement("SPAN");
  bottomTok.className = "tok-id";
  bottomTok.textContent = "BottomSym";
  bottomTok._rect = { top: 750, left: 100, bottom: 770, right: 150, width: 50, height: 20 };
  td.appendChild(bottomTok);
  await docClick({ button: 0, detail: 1, target: bottomTok, preventDefault() {}, stopPropagation() {} });
  const pops = body.querySelectorAll(".symbol-popup");
  const pBottom = pops[pops.length - 1];
  if (pBottom.style.maxHeight) {
    throw new Error("expected bottom popup NOT to have inline maxHeight clamping, got " + pBottom.style.maxHeight);
  }
  if (parseFloat(pBottom.style.top) < 770) {
    throw new Error("expected bottom popup top >= 770, got " + pBottom.style.top);
  }

  // Test dragging from blank space in .sym-snippet-code (past the code) and from .sym-snippet-num
  const snipCode = pDrag.querySelector(".sym-snippet-code");
  if (snipCode) {
    snipCode._rect = { top: 150, left: 100, bottom: 170, right: 350, width: 250, height: 20 };
    const snipTok = createMockElement("SPAN");
    snipTok.className = "tok-kw";
    snipTok.textContent = "func";
    snipTok._rect = { top: 150, left: 100, bottom: 170, right: 180, width: 80, height: 20 };
    snipCode.appendChild(snipTok);

    const curL = parseFloat(pDrag.style.left);
    const curT = parseFloat(pDrag.style.top);
    // Pointerdown over actual code (clientX: 120, inside snipTok 100..180) -> does NOT start drag
    pPointerDown({ button: 0, clientX: 120, clientY: 160, target: snipCode, preventDefault() {}, stopPropagation() {} });
    if (winListeners["pointermove"].length > 0) {
      throw new Error("expected pointerdown over code NOT to start drag");
    }

    // Pointerdown on blank space past code (clientX: 250, right of 180) -> DOES start drag!
    pPointerDown({ button: 0, clientX: 250, clientY: 160, target: snipCode, preventDefault() {}, stopPropagation() {} });
    if (winListeners["pointermove"].length === 0) {
      throw new Error("expected pointerdown on blank snippet space to start drag");
    }
    winListeners["pointermove"][0]({ clientX: 290, clientY: 190 });
    winListeners["pointerup"][0]({});
    if (parseFloat(pDrag.style.left) <= curL) {
      throw new Error("expected popup left to increase after dragging from blank code space");
    }

    // Pointerdown on .sym-snippet-num line number -> DOES start drag!
    const snipNum = pDrag.querySelector(".sym-snippet-num");
    if (snipNum) {
      const curL2 = parseFloat(pDrag.style.left);
      pPointerDown({ button: 0, clientX: 70, clientY: 160, target: snipNum, preventDefault() {}, stopPropagation() {} });
      if (winListeners["pointermove"].length === 0) {
        throw new Error("expected pointerdown on .sym-snippet-num to start drag");
      }
      winListeners["pointermove"][0]({ clientX: 100, clientY: 160 });
      winListeners["pointerup"][0]({});
      if (parseFloat(pDrag.style.left) <= curL2) {
        throw new Error("expected popup left to increase after dragging from .sym-snippet-num");
      }
    }
  }
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TestSymbolPopupDragAndResize failed: %v\n%s", err, string(out))
	}
}

func TestSymbolPopupEscapeIsolation(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur.textContent = (cur.textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT"].includes(tag)) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    nodeType: 1,
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    style: {},
    children: [],
    parentElement: null,
    get parentNode() { return this.parentElement; },
    get childNodes() { return this.children; },
    textContent: "",
    value: "",
    defaultValue: "",
    listeners: {},
    get offsetWidth() {
      if (this._offsetWidth !== undefined) return this._offsetWidth;
      if (this.classList && this.classList.contains("sym-popup-actions")) return 60;
      if (this.classList && this.classList.contains("sym-btn-back")) return 24;
      if (this.classList && this.classList.contains("sym-popup-loc")) return 100;
      if (this.tagName === "BUTTON" || this.tagName === "A" || this.tagName === "SPAN") return 24;
      return 300;
    },
    set offsetWidth(v) { this._offsetWidth = v; },
    get offsetHeight() {
      if (this._offsetHeight !== undefined) return this._offsetHeight;
      if (this._rect && this._rect.height !== undefined) return this._rect.height;
      return 200;
    },
    set offsetHeight(v) { this._offsetHeight = v; },
    scrollTop: 0,
    scrollLeft: 0,
    _rect: { top: 100, left: 100, bottom: 300, right: 400, width: 300, height: 200 },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    removeAttribute(k) { delete this.attributes[k]; },
    getBoundingClientRect() { return { ...this._rect }; },
    focus() { doc.activeElement = this; this._focused = true; },
    blur() { this._blurred = true; this._focused = false; if (doc.activeElement === this) doc.activeElement = null; },
    addEventListener(event, fn) {
      if (!this.listeners[event]) this.listeners[event] = [];
      this.listeners[event].push(fn);
    },
    removeEventListener(event, fn) {
      if (this.listeners[event]) {
        this.listeners[event] = this.listeners[event].filter(f => f !== fn);
      }
    },
    appendChild(child) {
      if (!child) return child;
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    insertBefore(newChild, refChild) {
      if (!newChild) return newChild;
      newChild.parentElement = this;
      const idx = this.children.indexOf(refChild);
      if (idx !== -1) this.children.splice(idx, 0, newChild);
      else this.children.push(newChild);
      return newChild;
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    blur() { this._blurred = true; },
    focus() { this._focused = true; },
    get classList() {
      return {
        contains: (c) => (el.className || "").split(/\s+/).includes(c),
        add: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.add(c));
          el.className = Array.from(parts).join(" ");
        },
        remove: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.delete(c));
          el.className = Array.from(parts).join(" ");
        }
      };
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (node.tagName && matchesSel(node, sel)) return node;
        node = node.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const results = [];
      function search(n) {
        for (const ch of n.children) {
          if (matchesSel(ch, sel)) results.push(ch);
          search(ch);
        }
      }
      search(this);
      return results;
    },
    set innerHTML(val) {
      this._innerHTML = val;
      this.children = parseHtml(val, this);
    },
    get innerHTML() {
      return this._innerHTML || "";
    }
  };
  return el;
}

function matchesSel(node, sel) {
  const parts = sel.split(",").map(s => s.trim());
  for (const p of parts) {
    if (p.includes("[")) {
      const bIdx = p.indexOf("[");
      const tagPart = p.slice(0, bIdx);
      const attrPart = p.slice(bIdx + 1, -1);
      if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
        continue;
      }
      if (attrPart.includes("=")) {
        const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
        if (node.getAttribute && node.getAttribute(k) === v) return true;
      } else {
        if (node.hasAttribute && node.hasAttribute(attrPart)) return true;
      }
      continue;
    }
    if (p.includes(".")) {
      const segs = p.split(".");
      const tagPart = segs[0];
      const clsParts = segs.slice(1);
      if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
        continue;
      }
      if (clsParts.every(c => node.classList && node.classList.contains && node.classList.contains(c))) {
        return true;
      }
      continue;
    }
    if (p.startsWith("#")) {
      if (node.id === p.slice(1)) return true;
    } else if (p.toLowerCase() === (node.tagName || "").toLowerCase()) {
      return true;
    }
  }
  return false;
}

const docListeners = {};
const winListeners = {};
const body = createMockElement("BODY");
body.offsetHeight = 3000;
const header = createMockElement("HEADER");
header.className = "app-header";
header.offsetHeight = 48;
header._rect = { top: 0, left: 0, bottom: 48, right: 1000, width: 1000, height: 48 };
body.appendChild(header);

const doc = {
  readyState: "complete",
  documentElement: {
    clientWidth: 1000,
    clientHeight: 1000,
    style: {
      _props: {},
      setProperty(k, v) { this._props[k] = String(v); },
      getPropertyValue(k) { return this._props[k] || ""; },
    },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  body: body,
  getElementById(id) {
    if (id === "shortcutsModal") return null;
    const all = body.querySelectorAll("#" + id);
    return all.length > 0 ? all[0] : null;
  },
  createElement(tag) { return createMockElement(tag); },
  createTreeWalker(root) { return { currentNode: root, nextNode() { return null; } }; },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (docListeners[event]) docListeners[event] = docListeners[event].filter(f => f !== fn);
  },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); }
};

let currentSelection = { isCollapsed: true, removeAllRanges() { this.isCollapsed = true; } };

const win = {
  document: doc,
  NodeFilter: { SHOW_TEXT: 4 },
  location: { pathname: "/r/test/c/abc", search: "", hash: "", reload() {} },
  addEventListener(event, fn) {
    if (!winListeners[event]) winListeners[event] = [];
    winListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (winListeners[event]) winListeners[event] = winListeners[event].filter(f => f !== fn);
  },
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return currentSelection; },
  window: null,
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  fetch(url, opts) {
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        found: true,
        symbol_name: "TestSym",
        def_path: "foo.go",
        def_line: 42,
        in_repo: true,
        display_path: "foo.go",
        signature: "func TestSym()",
        can_refs: false,
        can_impls: false,
      })
    });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  NodeFilter: win.NodeFilter,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  fetch: win.fetch,
  URLSearchParams,
  Promise,
  setTimeout,
  clearTimeout,
  console,
  AbortController,
});
vm.runInContext(code, ctx);

(async () => {
  const container = createMockElement("DIV");
  container.className = "diff-container";
  container.setAttribute("data-file-path", "foo.go");
  container.setAttribute("data-repo", "testrepo");
  container.setAttribute("data-change", "123");
  body.appendChild(container);

  const tr = createMockElement("TR");
  tr.setAttribute("data-line-r", "10");
  container.appendChild(tr);

  const td = createMockElement("TD");
  td.setAttribute("data-side", "R");
  tr.appendChild(td);

  const tok = createMockElement("SPAN");
  tok.className = "tok-id";
  tok.textContent = "TestSym";
  tok._rect = { top: 100, left: 100, bottom: 120, right: 150, width: 50, height: 20 };
  td.appendChild(tok);

  const docClick = docListeners["click"][0];
  await docClick({ button: 0, detail: 1, target: tok, preventDefault() {}, stopPropagation() {} });

  const popups = body.querySelectorAll(".symbol-popup");
  if (popups.length !== 1) throw new Error("expected 1 popup open");

  const keydown = winListeners["keydown"][0];

  // A. Focus on comment box with draft text -> Escape blurs, does NOT close popup
  const commentTa = createMockElement("TEXTAREA");
  commentTa.id = "inlineCommentText";
  commentTa.value = "my draft comment";
  const inlineCommentForm = createMockElement("DIV");
  inlineCommentForm.id = "inlineNewComment";
  inlineCommentForm.appendChild(commentTa);
  body.appendChild(inlineCommentForm);

  keydown({ key: "Escape", target: commentTa, preventDefault() {}, stopPropagation() {} });
  if (!commentTa._blurred) throw new Error("expected textarea blurred on Esc");
  if (body.querySelectorAll(".symbol-popup").length !== 1) throw new Error("popup closed when Esc pressed in textarea with text");
  if (!body.querySelector("#inlineNewComment")) throw new Error("inlineNewComment removed prematurely");

  // B. Press Escape when unfocused while #inlineNewComment exists -> closes inline comment, NOT popup
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (body.querySelector("#inlineNewComment")) throw new Error("expected inlineNewComment closed after unfocused Esc");
  if (body.querySelectorAll(".symbol-popup").length !== 1) throw new Error("popup closed when Esc closed inline comment");

  // C. Reply form with empty text -> Escape in textarea hides reply form, does NOT close popup
  const replyForm = createMockElement("DIV");
  replyForm.className = "reply-form";
  replyForm.style.display = "block";
  const replyTa = createMockElement("TEXTAREA");
  replyTa.value = "";
  replyForm.appendChild(replyTa);
  body.appendChild(replyForm);

  // C. Reply form -> 1st Escape in textarea blurs, 2nd unfocused Escape hides form, NEITHER closes popup
  keydown({ key: "Escape", target: replyTa, preventDefault() {}, stopPropagation() {} });
  if (!replyTa._blurred) throw new Error("expected replyTa blurred on 1st Esc");
  if (replyForm.style.display !== "block") throw new Error("expected reply form kept open on 1st Esc");
  if (body.querySelectorAll(".symbol-popup").length !== 1) throw new Error("popup closed when Esc blurred reply textarea");

  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (replyForm.style.display !== "none") throw new Error("expected reply form hidden on 2nd unfocused Esc");
  if (body.querySelectorAll(".symbol-popup").length !== 1) throw new Error("popup closed when Esc closed reply form");

  // C2. Reply form with draft text -> 1st Escape blurs, 2nd unfocused Escape cancels form, NEITHER closes popup
  replyForm.style.display = "block";
  replyTa.value = "draft reply";
  replyTa._blurred = false;
  keydown({ key: "Escape", target: replyTa, preventDefault() {}, stopPropagation() {} });
  if (!replyTa._blurred) throw new Error("expected non-empty replyTa blurred on 1st Esc");
  if (replyForm.style.display !== "block") throw new Error("expected non-empty reply form kept open on 1st Esc");
  if (body.querySelectorAll(".symbol-popup").length !== 1) throw new Error("popup closed on 1st Esc in non-empty reply form");

  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (replyForm.style.display !== "none") throw new Error("expected open reply form canceled on 2nd unfocused Esc");
  if (replyTa.value !== "") throw new Error("expected reply draft cleared on cancel");
  if (body.querySelectorAll(".symbol-popup").length !== 1) throw new Error("popup closed on 2nd Esc canceling reply form");

  // C3. Hidden reply form with empty style.display ("") must NOT swallow Escape
  const cssHiddenReply = createMockElement("DIV");
  cssHiddenReply.className = "reply-form";
  cssHiddenReply.style.display = "";
  body.appendChild(cssHiddenReply);

  // D. Search input focused -> Escape blurs search, does NOT close popup
  const searchInput = createMockElement("INPUT");
  searchInput.id = "changeSearchInput";
  body.appendChild(searchInput);

  keydown({ key: "Escape", target: searchInput, preventDefault() {}, stopPropagation() {} });
  if (!searchInput._blurred) throw new Error("expected searchInput blurred on Esc");
  if (body.querySelectorAll(".symbol-popup").length !== 1) throw new Error("popup closed when Esc blurred search");

  // E. Non-collapsed selection active -> Escape clears selection, does NOT close popup
  currentSelection = { isCollapsed: false, removeAllRanges() { this.isCollapsed = true; } };
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (!currentSelection.isCollapsed) throw new Error("expected selection collapsed on Esc");
  if (body.querySelectorAll(".symbol-popup").length !== 1) throw new Error("popup closed when Esc cleared selection");

  // F. Clean Escape -> now closes popup
  keydown({ key: "Escape", target: body, preventDefault() {}, stopPropagation() {} });
  if (body.querySelectorAll(".symbol-popup").length !== 0) throw new Error("expected popup closed on clean Esc");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TestSymbolPopupEscapeIsolation failed: %v\n%s", err, string(out))
	}
}

func TestSymbolPopupPageGrowShrink(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur.textContent = (cur.textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT"].includes(tag)) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    nodeType: 1,
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    style: {},
    children: [],
    parentElement: null,
    get parentNode() { return this.parentElement; },
    get childNodes() { return this.children; },
    _textContent: "",
    get textContent() {
      let t = this._textContent || "";
      for (const ch of this.children) {
        t += ch.textContent;
      }
      return t;
    },
    set textContent(v) {
      this._textContent = String(v);
    },
    listeners: {},
    get offsetWidth() {
      if (this._offsetWidth !== undefined) return this._offsetWidth;
      if (this.classList && this.classList.contains("sym-popup-actions")) return 60;
      if (this.classList && this.classList.contains("sym-btn-back")) return 24;
      if (this.classList && this.classList.contains("sym-popup-loc")) return 100;
      if (this.tagName === "BUTTON" || this.tagName === "A" || this.tagName === "SPAN") return 24;
      return 300;
    },
    set offsetWidth(v) { this._offsetWidth = v; },
    get offsetHeight() {
      if (this._offsetHeight !== undefined) return this._offsetHeight;
      if (this._rect && this._rect.height !== undefined) return this._rect.height;
      return 200;
    },
    set offsetHeight(v) { this._offsetHeight = v; },
    scrollTop: 0,
    scrollLeft: 0,
    _rect: { top: 100, left: 100, bottom: 300, right: 400, width: 300, height: 200 },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    removeAttribute(k) { delete this.attributes[k]; },
    getBoundingClientRect() { return { ...this._rect }; },
    focus() { doc.activeElement = this; this._focused = true; },
    blur() { this._blurred = true; this._focused = false; if (doc.activeElement === this) doc.activeElement = null; },
    addEventListener(event, fn) {
      if (!this.listeners[event]) this.listeners[event] = [];
      this.listeners[event].push(fn);
    },
    removeEventListener(event, fn) {
      if (this.listeners[event]) {
        this.listeners[event] = this.listeners[event].filter(f => f !== fn);
      }
    },
    appendChild(child) {
      if (!child) return child;
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    insertBefore(newChild, refChild) {
      if (!newChild) return newChild;
      newChild.parentElement = this;
      const idx = this.children.indexOf(refChild);
      if (idx !== -1) this.children.splice(idx, 0, newChild);
      else this.children.push(newChild);
      return newChild;
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    get classList() {
      return {
        contains: (c) => (el.className || "").split(/\s+/).includes(c),
        add: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.add(c));
          el.className = Array.from(parts).join(" ");
        },
        remove: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.delete(c));
          el.className = Array.from(parts).join(" ");
        }
      };
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (node.tagName && matchesSel(node, sel)) return node;
        node = node.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const results = [];
      function search(n) {
        for (const ch of n.children) {
          if (matchesSel(ch, sel)) results.push(ch);
          search(ch);
        }
      }
      search(this);
      return results;
    },
    set innerHTML(val) {
      this._innerHTML = val;
      this.children = parseHtml(val, this);
    },
    get innerHTML() {
      return this._innerHTML || "";
    }
  };
  return el;
}

function matchesSel(node, sel) {
  const parts = sel.split(",").map(s => s.trim());
  for (const p of parts) {
    if (p.includes("[")) {
      const bIdx = p.indexOf("[");
      const tagPart = p.slice(0, bIdx);
      const attrPart = p.slice(bIdx + 1, -1);
      if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
        continue;
      }
      if (attrPart.includes("=")) {
        const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
        if (node.getAttribute && node.getAttribute(k) === v) return true;
      } else {
        if (node.hasAttribute && node.hasAttribute(attrPart)) return true;
      }
      continue;
    }
    if (p.includes(".")) {
      const segs = p.split(".");
      const tagPart = segs[0];
      const clsParts = segs.slice(1);
      if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
        continue;
      }
      if (clsParts.every(c => node.classList && node.classList.contains && node.classList.contains(c))) {
        return true;
      }
      continue;
    }
    if (p.startsWith("#")) {
      if (node.id === p.slice(1)) return true;
    } else if (p.toLowerCase() === (node.tagName || "").toLowerCase()) {
      return true;
    }
  }
  return false;
}

const docListeners = {};
const winListeners = {};
const body = createMockElement("BODY");
body.offsetHeight = 3000;
const header = createMockElement("HEADER");
header.className = "app-header";
header.offsetHeight = 48;
header._rect = { top: 0, left: 0, bottom: 48, right: 1000, width: 1000, height: 48 };
body.appendChild(header);

const doc = {
  readyState: "complete",
  documentElement: {
    clientWidth: 1000,
    clientHeight: 1000,
    style: {
      _props: {},
      setProperty(k, v) { this._props[k] = String(v); },
      getPropertyValue(k) { return this._props[k] || ""; },
    },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  body: body,
  getElementById(id) {
    if (id === "shortcutsModal") return null;
    const all = body.querySelectorAll("#" + id);
    return all.length > 0 ? all[0] : null;
  },
  createElement(tag) { return createMockElement(tag); },
  createTreeWalker(root) { return { currentNode: root, nextNode() { return null; } }; },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (docListeners[event]) docListeners[event] = docListeners[event].filter(f => f !== fn);
  },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); }
};

const win = {
  document: doc,
  NodeFilter: { SHOW_TEXT: 4 },
  location: { pathname: "/r/test/c/abc", search: "", hash: "", reload() {} },
  addEventListener(event, fn) {
    if (!winListeners[event]) winListeners[event] = [];
    winListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (winListeners[event]) winListeners[event] = winListeners[event].filter(f => f !== fn);
  },
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; },
  window: null,
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  fetch(url, opts) {
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        found: true,
        symbol_name: "TestSym",
        def_path: "foo.go",
        def_line: 42,
        in_repo: true,
        display_path: "foo.go",
        signature: "func TestSym()",
        can_refs: false,
        can_impls: false,
      })
    });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  NodeFilter: win.NodeFilter,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  fetch: win.fetch,
  URLSearchParams,
  Promise,
  setTimeout,
  clearTimeout,
  console,
  AbortController,
});
vm.runInContext(code, ctx);

(async () => {
  const container = createMockElement("DIV");
  container.className = "diff-container";
  container.setAttribute("data-file-path", "foo.go");
  container.setAttribute("data-repo", "testrepo");
  container.setAttribute("data-change", "123");
  body.appendChild(container);

  const tr = createMockElement("TR");
  tr.setAttribute("data-line-r", "10");
  container.appendChild(tr);

  const td = createMockElement("TD");
  td.setAttribute("data-side", "R");
  tr.appendChild(td);

  const tok = createMockElement("SPAN");
  tok.className = "tok-id";
  tok.textContent = "TestSym";
  tok._rect = { top: 100, left: 100, bottom: 120, right: 150, width: 50, height: 20 };
  td.appendChild(tok);

  const docClick = docListeners["click"][0];

  // Setup File 1 section [secTop=100, oldBottom=1000]
  const sec1 = createMockElement("DIV");
  sec1.id = "file-sec-0";
  sec1.className = "diff-file-section";
  sec1._rect = { top: 100, bottom: 1000, left: 0, right: 1000, width: 1000, height: 900 };
  const sec1Body = createMockElement("DIV");
  sec1Body.id = "file-body-0";
  sec1Body.className = "diff-file-body";
  sec1.appendChild(sec1Body);
  body.appendChild(sec1);

  // Setup File 2 section [secTop=1100, bottom=2000]
  const sec2 = createMockElement("DIV");
  sec2.id = "file-sec-1";
  sec2.className = "diff-file-section";
  sec2._rect = { top: 1100, bottom: 2000, left: 0, right: 1000, width: 1000, height: 900 };
  body.appendChild(sec2);

  // Popup 1: inside File 1 at top=500 (pin p1 and p2 so all 3 stack for collapse relocation)
  await docClick({ button: 0, detail: 1, target: tok, preventDefault() {}, stopPropagation() {} });
  win.togglePopupShade(win.openSymbolPopups[0]);
  win.togglePopupShade(win.openSymbolPopups[0]);
  // Popup 2: in File 2 at top=1400
  await docClick({ button: 0, detail: 1, target: tok, preventDefault() {}, stopPropagation() {} });
  win.togglePopupShade(win.openSymbolPopups[1]);
  win.togglePopupShade(win.openSymbolPopups[1]);
  // Popup 3: near bottom of page at top=2600
  await docClick({ button: 0, detail: 1, target: tok, preventDefault() {}, stopPropagation() {} });
  win.togglePopupShade(win.openSymbolPopups[2]);
  win.togglePopupShade(win.openSymbolPopups[2]);

  const allPops = body.querySelectorAll(".symbol-popup");
  if (allPops.length !== 3) throw new Error("expected 3 popups");
  const [p1, p2, p3] = allPops;
  p1.style.top = "500px"; p1.offsetHeight = 250;
  p2.style.top = "1400px"; p2.offsetHeight = 250;
  p3.style.top = "2600px"; p3.offsetHeight = 250;

  // A. Page grows (3000 -> 4500): all popups MUST STAY AT EXACT SAME POSITION!
  body.offsetHeight = 4500;
  win.handleNaturalPageHeightChange();
  if (p1.style.top !== "500px") throw new Error("p1 moved on page grow");
  if (p2.style.top !== "1400px") throw new Error("p2 moved on page grow");
  if (p3.style.top !== "2600px") throw new Error("p3 moved on page grow");

  // B. File 1 collapses: sec1 collapses to bottom=136. Page height shortens to 2136.
  sec1._rect.bottom = 1000;
  Object.defineProperty(sec1Body.style, "display", {
    set(v) {
      this._display = v;
      if (v === "none") {
        sec1._rect.bottom = 136;
      }
    },
    get() {
      return this._display || "";
    }
  });

  body.offsetHeight = 2136;
  win.toggleFileSection(0);

  // Check p1 (was at top=500 inside File 1): moved to 136px
  if (p1.style.top !== "136px") throw new Error("expected p1 moved to 136px, got " + p1.style.top);

  // Check p2 (was at top=1400 in File 2, 1400+250 <= 2124): UNTOUCHED at 1400px
  if (p2.style.top !== "1400px") throw new Error("expected p2 untouched at 1400px, got " + p2.style.top);

  // Check p3 (was at top=2600, page now 2136, 2600+250 > 2124): moved to bottom of page (2136 - 250 - 12 = 1874px)
  if (p3.style.top !== "1874px") throw new Error("expected p3 moved to 1874px, got " + p3.style.top);
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TestSymbolPopupPageGrowShrink failed: %v\n%s", err, string(out))
	}
}

func TestMoveDetectionLargeFileHandling(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")
	run("config", "user.name", "Tester")
	run("config", "user.email", "tester@example.com")

	var buf bytes.Buffer
	for i := 1; i <= 25005; i++ {
		fmt.Fprintf(&buf, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "large.txt"), buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "Initial 25005-line file\n\nChange-Id: I3333333333333333333333333333333333333333")

	buf.Reset()
	for i := 1; i <= 25005; i++ {
		if i == 500 {
			buf.WriteString("modified line 500\n")
		} else {
			fmt.Fprintf(&buf, "line %d\n", i)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "large.txt"), buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-a", "-m", "Modify line 500\n\nChange-Id: I4444444444444444444444444444444444444444")
	changeHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(func() { ts.Close() })

	// 1. ?diffs=all renders large diff notice
	url := fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, changeHash)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /c failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)
	if !strings.Contains(body, "Large diff (exceeds 25,000 lines)") {
		t.Errorf("expected ?diffs=all to contain 'Large diff (exceeds 25,000 lines)'")
	}

	// 2. Single-file view renders actual diff rows
	singleUrl := fmt.Sprintf("%s/r/%s/c/%s/f/large.txt", ts.URL, rec.Slug, changeHash)
	singleResp, err := http.Get(singleUrl)
	if err != nil {
		t.Fatalf("GET /f failed: %v", err)
	}
	defer singleResp.Body.Close()
	if singleResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for single-file, got %d", singleResp.StatusCode)
	}
	singleBodyBytes, _ := io.ReadAll(singleResp.Body)
	singleBody := string(singleBodyBytes)
	if strings.Contains(singleBody, "Large diff (exceeds 25,000 lines)") {
		t.Errorf("expected single-file view NOT to contain 'Large diff (exceeds 25,000 lines)'")
	}
	if !strings.Contains(singleBody, "diff-row") {
		t.Errorf("expected single-file view to contain diff-row")
	}
	if !strings.Contains(singleBody, `<mark class="intra">modified </mark>line 500`) && !strings.Contains(singleBody, "modified") {
		t.Errorf("expected single-file view to contain modified line 500 diff")
	}
}

func TestDiffGutterBordersOnCommentAndMoveRows(t *testing.T) {
	// 1. Verify static/style.css contains center divider and gutter rules, and no superseded rules.
	styleBytes, err := os.ReadFile(filepath.Join("static", "style.css"))
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	styleCSS := string(styleBytes)
	if !strings.Contains(styleCSS, ".diff-table-split tr > td:nth-child(3)") {
		t.Errorf("expected style.css to contain '.diff-table-split tr > td:nth-child(3)'")
	}
	if !strings.Contains(styleCSS, "border-left: 1px solid var(--border-muted);") {
		t.Errorf("expected style.css to set border-left on center divider")
	}
	if strings.Contains(styleCSS, ".diff-table-split .thread-row > td:first-child") {
		t.Errorf("expected style.css NOT to contain superseded '.diff-table-split .thread-row > td:first-child'")
	}
	if strings.Contains(styleCSS, ".diff-table-split td.line-code + td.line-num") {
		t.Errorf("expected style.css NOT to contain superseded '.diff-table-split td.line-code + td.line-num'")
	}
	if strings.Contains(styleCSS, ".diff-table-split .row-skip td.skip-code-cell + td.line-num") {
		t.Errorf("expected style.css NOT to contain superseded '.diff-table-split .row-skip td.skip-code-cell + td.line-num'")
	}
	if !strings.Contains(styleCSS, ".move-header-row td.thread-gutter") {
		t.Errorf("expected style.css to style '.move-header-row td.thread-gutter'")
	}

	// 2. Setup repo with moved code and comment threads.
	repoDir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("config", "user.name", "Tester")
	run("config", "user.email", "tester@example.com")

	sourceGo := `package main

func ComputeBlock(x int) int {
	a := x * 2
	b := a + 1
	c := b + 2
	d := c + 3
	return d
}

func Other() {}
`
	destGo := `package main

func Existing() {}
`
	if err := os.WriteFile(filepath.Join(repoDir, "source.go"), []byte(sourceGo), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "dest.go"), []byte(destGo), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "Initial commit\n\nChange-Id: I1111111111111111111111111111111111111111")

	sourceGo2 := `package main

func Other() {}
`
	destGo2 := `package main

func Existing() {}

func ComputeBlock(x int) int {
	a := x * 2
	b := a + 1
	c := b + 2
	d := c + 3
	return d
}
`
	if err := os.WriteFile(filepath.Join(repoDir, "source.go"), []byte(sourceGo2), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "dest.go"), []byte(destGo2), 0644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-a", "-m", "Move ComputeBlock to dest.go\n\nChange-Id: I2222222222222222222222222222222222222222")
	secondHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(func() { ts.Close() })

	// Add a comment on dest.go L7
	payload, _ := json.Marshal(map[string]any{
		"repo":       rec.Slug,
		"change":     secondHash,
		"rev":        secondHash,
		"path":       "dest.go",
		"side":       "R",
		"start_line": 7,
		"end_line":   7,
		"author":     "reviewer",
		"body":       "Great moved function",
		"resolved":   false,
	})
	resp, err := http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(payload))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add failed: %v", err)
	}

	// 3. Check split diff rendering
	url := fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, secondHash)
	resp, err = http.Get(url)
	if err != nil {
		t.Fatalf("GET /c failed: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)

	// Ensure move-header-row in split table does NOT have colspan="2"
	if strings.Contains(body, `<td colspan="2" class="move-header-cell">`) {
		t.Errorf("found colspan=2 in move-header-cell in split diff")
	}
	if strings.Contains(body, `<tr class="move-header-row">`) && !strings.Contains(body, `class="line-num thread-gutter cell-moved-num"`) {
		t.Errorf("expected move-header-row to contain 'line-num thread-gutter cell-moved-num'")
	}

	// Ensure thread-row in split table does NOT have colspan="2"
	if strings.Contains(body, `<td colspan="2" class="thread-cell">`) {
		t.Errorf("found colspan=2 in thread-cell in split diff")
	}
	if strings.Contains(body, `<tr class="thread-row">`) && !strings.Contains(body, `class="line-num thread-gutter`) {
		t.Errorf("expected thread-row to contain 'line-num thread-gutter'")
	}

	// 4. Test openCommentForm with Node
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	nodeScript := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur.textContent = (cur.textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT"].includes(tag)) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    children: [],
    parentElement: null,
    style: {},
    listeners: {},
    focus() {},
    blur() {},
    setAttribute(k, v) { this.attributes[k] = String(v); },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    removeAttribute(k) { delete this.attributes[k]; },
    addEventListener(evt, fn) { (this.listeners[evt] = this.listeners[evt] || []).push(fn); },
    removeEventListener(evt, fn) {
      if (this.listeners[evt]) this.listeners[evt] = this.listeners[evt].filter(f => f !== fn);
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    closest(selector) {
      let cur = this;
      while (cur) {
        if (matches(cur, selector)) return cur;
        cur = cur.parentElement;
      }
      return null;
    },
    querySelector(selector) {
      return querySelectorAllMock(this, selector)[0] || null;
    },
    querySelectorAll(selector) {
      return querySelectorAllMock(this, selector);
    },
    insertBefore(newChild, refChild) {
      const idx = refChild ? this.children.indexOf(refChild) : this.children.length;
      if (idx === -1) this.children.push(newChild);
      else this.children.splice(idx, 0, newChild);
      newChild.parentElement = this;
      return newChild;
    },
    appendChild(child) {
      this.children.push(child);
      child.parentElement = this;
      return child;
    },
    get nextSibling() {
      if (!this.parentElement) return null;
      const idx = this.parentElement.children.indexOf(this);
      return (idx !== -1 && idx + 1 < this.parentElement.children.length) ? this.parentElement.children[idx + 1] : null;
    },
    get parentNode() { return this.parentElement; },
    get classList() {
      return {
        add: (cls) => {
          const parts = (el.className || "").split(/\s+/).filter(Boolean);
          if (!parts.includes(cls)) parts.push(cls);
          el.className = parts.join(" ");
        },
        remove: (cls) => {
          el.className = (el.className || "").split(/\s+/).filter(c => c && c !== cls).join(" ");
        },
        contains: (cls) => (el.className || "").split(/\s+/).includes(cls)
      };
    },
    get innerHTML() {
      return serializeHtml(this);
    },
    set innerHTML(val) {
      this.children = parseHtml(val, this);
    }
  };
  return el;
}

function matches(el, selector) {
  if (selector.startsWith(".")) {
    const cls = selector.slice(1);
    return el.className && el.className.split(/\s+/).includes(cls);
  }
  if (selector.startsWith("#")) {
    return el.id === selector.slice(1);
  }
  return el.tagName === selector.toUpperCase();
}

function querySelectorAllMock(root, selector) {
  const results = [];
  function walk(node) {
    for (const child of node.children) {
      if (matches(child, selector)) results.push(child);
      walk(child);
    }
  }
  walk(root);
  return results;
}

function serializeHtml(el) {
  let s = "";
  for (const c of el.children) {
    s += "<" + c.tagName.toLowerCase();
    if (c.id) s += ' id="' + c.id + '"';
    if (c.className) s += ' class="' + c.className + '"';
    s += ">";
    if (c.children.length > 0) s += serializeHtml(c);
    else s += c.textContent || "";
    s += "</" + c.tagName.toLowerCase() + ">";
  }
  return s;
}

const table = createMockElement("TABLE");
table.className = "diff-table diff-table-split";
const tbody = createMockElement("TBODY");
table.appendChild(tbody);

const tr = createMockElement("TR");
tr.className = "diff-row";
tr.setAttribute("data-line-l", "10");
tr.setAttribute("data-line-r", "12");
tbody.appendChild(tr);

const body = createMockElement("BODY");
body.appendChild(table);

const doc = {
  body: body,
  documentElement: {
    clientWidth: 1000,
    clientHeight: 1000,
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  activeElement: null,
  getElementById(id) {
    if (id === "inlineNewComment") {
      return querySelectorAllMock(tbody, "#inlineNewComment")[0] || null;
    }
    return null;
  },
  querySelector(selector) {
    return querySelectorAllMock(this.body, selector)[0] || null;
  },
  querySelectorAll(selector) {
    return querySelectorAllMock(this.body, selector);
  },
  createElement(tag) {
    return createMockElement(tag);
  },
  addEventListener() {},
  removeEventListener() {}
};

const sandbox = {
  window: {
    addEventListener() {},
    removeEventListener() {},
    location: { pathname: "/r/test/c/123", hash: "", search: "" },
    document: doc
  },
  document: doc,
  navigator: { platform: "Linux" },
  setTimeout() {},
  clearTimeout() {},
  console
};
sandbox.window.window = sandbox.window;
vm.createContext(sandbox);
vm.runInContext(code, sandbox);

// 1. Open comment form on R side
sandbox.window.openCommentForm(tr, 12, 'R');
const formTr = tbody.querySelector("#inlineNewComment");
if (!formTr) throw new Error("expected #inlineNewComment to be created");
const cellsR = formTr.children.filter(c => c.tagName === "TD");
if (cellsR.length !== 4) throw new Error("expected 4 TD cells in split mode inlineNewComment, got " + cellsR.length);
if (!cellsR[0].classList.contains("thread-gutter")) throw new Error("expected cell 0 to have thread-gutter");
if (!cellsR[1].classList.contains("line-code")) throw new Error("expected cell 1 to have line-code");
if (!cellsR[2].classList.contains("thread-gutter")) throw new Error("expected cell 2 to have thread-gutter");
if (!cellsR[3].classList.contains("thread-cell")) throw new Error("expected cell 3 to have thread-cell");

// 2. Open comment form on L side
sandbox.window.openCommentForm(tr, 10, 'L');
const formTrL = tbody.querySelector("#inlineNewComment");
if (!formTrL) throw new Error("expected #inlineNewComment to be recreated for L");
const cellsL = formTrL.children.filter(c => c.tagName === "TD");
if (cellsL.length !== 4) throw new Error("expected 4 TD cells for L side, got " + cellsL.length);
if (!cellsL[0].classList.contains("thread-gutter")) throw new Error("expected cell 0 to have thread-gutter");
if (!cellsL[1].classList.contains("thread-cell")) throw new Error("expected cell 1 to have thread-cell");
if (!cellsL[2].classList.contains("thread-gutter")) throw new Error("expected cell 2 to have thread-gutter");
if (!cellsL[3].classList.contains("line-code")) throw new Error("expected cell 3 to have line-code");

console.log("OK");
`
	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("openCommentForm node test failed: %v\n%s", err, string(out))
	}
}

func TestRightClickBlankAreaAsEscape(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	nodeScript := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur.textContent = (cur.textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT"].includes(tag)) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    children: [],
    parentElement: null,
    style: {},
    listeners: {},
    focus() {
      doc.activeElement = el;
    },
    blur() {
      if (doc.activeElement === el) doc.activeElement = null;
    },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    removeAttribute(k) { delete this.attributes[k]; },
    addEventListener(evt, fn) { (this.listeners[evt] = this.listeners[evt] || []).push(fn); },
    removeEventListener(evt, fn) {
      if (this.listeners[evt]) this.listeners[evt] = this.listeners[evt].filter(f => f !== fn);
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    closest(selector) {
      let cur = this;
      while (cur) {
        if (matches(cur, selector)) return cur;
        cur = cur.parentElement;
      }
      return null;
    },
    querySelector(selector) {
      return querySelectorAllMock(this, selector)[0] || null;
    },
    querySelectorAll(selector) {
      return querySelectorAllMock(this, selector);
    },
    insertBefore(newChild, refChild) {
      const idx = refChild ? this.children.indexOf(refChild) : this.children.length;
      if (idx === -1) this.children.push(newChild);
      else this.children.splice(idx, 0, newChild);
      newChild.parentElement = this;
      return newChild;
    },
    appendChild(child) {
      this.children.push(child);
      child.parentElement = this;
      return child;
    },
    get nextSibling() {
      if (!this.parentElement) return null;
      const idx = this.parentElement.children.indexOf(this);
      return (idx !== -1 && idx + 1 < this.parentElement.children.length) ? this.parentElement.children[idx + 1] : null;
    },
    get parentNode() { return this.parentElement; },
    get classList() {
      return {
        add: (cls) => {
          const parts = (el.className || "").split(/\s+/).filter(Boolean);
          if (!parts.includes(cls)) parts.push(cls);
          el.className = parts.join(" ");
        },
        remove: (cls) => {
          el.className = (el.className || "").split(/\s+/).filter(c => c && c !== cls).join(" ");
        },
        contains: (cls) => (el.className || "").split(/\s+/).includes(cls)
      };
    },
    get innerHTML() {
      return serializeHtml(this);
    },
    set innerHTML(val) {
      this.children = parseHtml(val, this);
    }
  };
  return el;
}

function matches(el, selector) {
  const parts = selector.split(",").map(s => s.trim());
  for (const part of parts) {
    if (part.startsWith(".")) {
      const cls = part.slice(1);
      if (el.className && el.className.split(/\s+/).includes(cls)) return true;
    } else if (part.startsWith("#")) {
      if (el.id === part.slice(1)) return true;
    } else if (part.includes("[")) {
      const bIdx = part.indexOf("[");
      const tagPart = part.slice(0, bIdx);
      const attrPart = part.slice(bIdx + 1, -1);
      if (!tagPart || el.tagName === tagPart.toUpperCase()) {
        if (attrPart.includes("*=")) {
          const [k, v] = attrPart.split("*=").map(s => s.replace(/["']/g, "").trim());
          const val = (k === "class") ? (el.className || "") : (el.getAttribute && el.getAttribute(k) || "");
          if (val.includes(v)) return true;
        } else if (attrPart.includes("=")) {
          const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
          const val = (k === "class") ? (el.className || "") : (el.getAttribute && el.getAttribute(k) || "");
          if (val === v) return true;
        } else {
          if (el.attributes && el.attributes[attrPart] !== undefined) return true;
        }
      }
    } else if (el.tagName === part.toUpperCase()) {
      return true;
    }
  }
  return false;
}

function querySelectorAllMock(root, selector) {
  const results = [];
  function walk(node) {
    for (const child of node.children) {
      if (matches(child, selector)) results.push(child);
      walk(child);
    }
  }
  walk(root);
  return results;
}

function serializeHtml(el) {
  let s = "";
  for (const c of el.children) {
    s += "<" + c.tagName.toLowerCase();
    if (c.id) s += ' id="' + c.id + '"';
    if (c.className) s += ' class="' + c.className + '"';
    s += ">";
    if (c.children.length > 0) s += serializeHtml(c);
    else s += c.textContent || "";
    s += "</" + c.tagName.toLowerCase() + ">";
  }
  return s;
}

const table = createMockElement("TABLE");
table.className = "diff-table diff-table-split";
const tbody = createMockElement("TBODY");
table.appendChild(tbody);

const tr = createMockElement("TR");
tr.className = "diff-row";
tr.setAttribute("data-line-l", "10");
tr.setAttribute("data-line-r", "12");
const tdNum = createMockElement("TD");
tdNum.className = "line-num";
tdNum.textContent = "12";
const tdCode = createMockElement("TD");
tdCode.className = "line-code";
const tokSpan = createMockElement("SPAN");
tokSpan.className = "tok-kw";
tokSpan.textContent = "func";
tdCode.appendChild(tokSpan);
tr.appendChild(tdNum);
tr.appendChild(tdCode);
tbody.appendChild(tr);

const body = createMockElement("BODY");
body.appendChild(table);

const docListeners = {};
const doc = {
  body: body,
  documentElement: {
    clientWidth: 1000,
    clientHeight: 1000,
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  activeElement: null,
  getElementById(id) {
    const all = querySelectorAllMock(body, "#" + id);
    return all.length > 0 ? all[0] : null;
  },
  querySelector(selector) {
    return querySelectorAllMock(this.body, selector)[0] || null;
  },
  querySelectorAll(selector) {
    return querySelectorAllMock(this.body, selector);
  },
  createElement(tag) {
    return createMockElement(tag);
  },
  addEventListener(evt, fn) {
    (docListeners[evt] = docListeners[evt] || []).push(fn);
  },
  removeEventListener(evt, fn) {
    if (docListeners[evt]) docListeners[evt] = docListeners[evt].filter(f => f !== fn);
  }
};

let currentSelection = { isCollapsed: true, toString: () => "" };

const sandbox = {
  window: {
    addEventListener() {},
    removeEventListener() {},
    location: { pathname: "/r/test/c/123", hash: "", search: "" },
    document: doc,
    getSelection: () => currentSelection
  },
  document: doc,
  navigator: { platform: "Linux" },
  setTimeout() {},
  clearTimeout() {},
  console
};
sandbox.window.window = sandbox.window;
vm.createContext(sandbox);
vm.runInContext(code, sandbox);

function fireContextMenu(target, opts = {}) {
  let prevented = false;
  const evt = Object.assign({
    target: target,
    button: 2,
    clientX: 100,
    clientY: 100,
    shiftKey: false,
    defaultPrevented: false,
    preventDefault: () => { prevented = true; }
  }, opts);
  const listeners = docListeners['contextmenu'] || [];
  for (const fn of listeners) {
    fn(evt);
  }
  return prevented;
}

// Case 1: Nothing open, right click on blank area -> no preventDefault
let prevented = fireContextMenu(body);
if (prevented) throw new Error("expected no preventDefault on body when nothing open");

// Case 2: Open comment form and focus textarea
sandbox.window.openCommentForm(tr, 12, 'R');
const commentTr = tbody.querySelector("#inlineNewComment");
if (!commentTr) throw new Error("comment form not opened");
const textarea = commentTr.querySelector("#inlineCommentText");
if (!textarea) throw new Error("textarea not found");
textarea.focus();
if (doc.activeElement !== textarea) throw new Error("textarea should be activeElement");

// Right-click inside textarea itself -> native menu preserved (no preventDefault)
prevented = fireContextMenu(textarea);
if (prevented) throw new Error("expected no preventDefault inside textarea");
if (doc.activeElement !== textarea) throw new Error("textarea should still be active");

// Right-click with Shift key -> native menu preserved (no preventDefault)
prevented = fireContextMenu(body, { shiftKey: true });
if (prevented) throw new Error("expected no preventDefault with Shift key");

// Right-click with active selection -> native menu preserved (no preventDefault)
currentSelection = { isCollapsed: false, toString: () => "some selected text" };
prevented = fireContextMenu(body);
if (prevented) throw new Error("expected no preventDefault when selection active");
currentSelection = { isCollapsed: true, toString: () => "" };

// Right-click over syntax token -> native menu preserved (no preventDefault)
prevented = fireContextMenu(tokSpan);
if (prevented) throw new Error("expected no preventDefault over syntax token");

// Right-click over line number with text -> native menu preserved (no preventDefault)
prevented = fireContextMenu(tdNum);
if (prevented) throw new Error("expected no preventDefault over line number with text");

// Right-click on blank area while textarea focused -> blurs textarea & calls preventDefault
prevented = fireContextMenu(body);
if (!prevented) throw new Error("expected preventDefault when blurring textarea on blank right-click");
if (doc.activeElement === textarea) throw new Error("expected textarea to be blurred");
if (!tbody.querySelector("#inlineNewComment")) throw new Error("comment form should still be open after 1st right-click blur");

// Right-click on blank area while unfocused comment form is open -> closes form & calls preventDefault
prevented = fireContextMenu(body);
if (!prevented) throw new Error("expected preventDefault when closing unfocused form on blank right-click");
if (tbody.querySelector("#inlineNewComment")) throw new Error("comment form should be closed after 2nd right-click");

// Right-click on blank area now that everything is closed -> native menu preserved
prevented = fireContextMenu(body);
if (prevented) throw new Error("expected no preventDefault after form is closed");

// Case 3: Keyboard context menu (clientX=0, clientY=0, button=0) -> no preventDefault
const popup = createMockElement("DIV");
popup.className = "symbol-popup";
body.appendChild(popup);
prevented = fireContextMenu(body, { button: 0, clientX: 0, clientY: 0 });
if (prevented) throw new Error("expected no preventDefault for keyboard context menu");
if (!body.querySelector(".symbol-popup")) throw new Error("popup should still exist after keyboard context menu");

// Case 4: Right-click on blank area with open popup -> closes popup & calls preventDefault
prevented = fireContextMenu(body);
if (!prevented) throw new Error("expected preventDefault when closing popup on blank right-click");
if (body.querySelector(".symbol-popup")) throw new Error("expected popup to be closed");

console.log("OK");
`
	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TestRightClickBlankAreaAsEscape failed: %v\n%s", err, string(out))
	}
}

func TestKeyboardCommentAndChunkNavigation(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	nodeScript := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function matchCompound(el, sel) {
  if (sel === "*") return true;
  const parts = sel.split(".");
  const tag = parts[0];
  if (tag && el.tagName !== tag.toUpperCase()) return false;
  for (let i = 1; i < parts.length; i++) {
    const cls = parts[i];
    if (!el.className || !el.className.split(/\s+/).includes(cls)) return false;
  }
  return true;
}

function matches(el, selector) {
  const parts = selector.split(",").map(s => s.trim());
  for (const part of parts) {
    if (part.includes(":not(")) {
      const main = part.slice(0, part.indexOf(":not("));
      const notSelector = part.slice(part.indexOf(":not(") + 5, -1);
      if ((!main || matches(el, main)) && !matches(el, notSelector)) return true;
      continue;
    }
    if (part.includes(" ")) {
      const sub = part.split(/\s+/);
      let cur = el;
      if (!matchCompound(cur, sub[sub.length - 1])) continue;
      let matched = true;
      cur = cur.parentElement;
      for (let i = sub.length - 2; i >= 0; i--) {
        while (cur && !matchCompound(cur, sub[i])) {
          cur = cur.parentElement;
        }
        if (!cur) { matched = false; break; }
        cur = cur.parentElement;
      }
      if (matched) return true;
      continue;
    }
    if (part.startsWith("#")) {
      if (el.id === part.slice(1)) return true;
      continue;
    }
    if (matchCompound(el, part)) return true;
  }
  return false;
}

function querySelectorAllMock(root, selector) {
  const results = [];
  function walk(node) {
    for (const child of node.children) {
      if (matches(child, selector)) results.push(child);
      walk(child);
    }
  }
  walk(root);
  return results;
}

function createMockElement(tag) {
  const el = {
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    children: [],
    parentElement: null,
    style: {},
    listeners: {},
    offsetTop: 0,
    offsetHeight: 40,
    offsetWidth: 200,
    offsetParent: {},
    focus() { doc.activeElement = el; },
    blur() { if (doc.activeElement === el) doc.activeElement = null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    removeAttribute(k) { delete this.attributes[k]; },
    addEventListener(evt, fn) { (this.listeners[evt] = this.listeners[evt] || []).push(fn); },
    removeEventListener(evt, fn) {
      if (this.listeners[evt]) this.listeners[evt] = this.listeners[evt].filter(f => f !== fn);
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    closest(selector) {
      let cur = this;
      while (cur) {
        if (matches(cur, selector)) return cur;
        cur = cur.parentElement;
      }
      return null;
    },
    querySelector(selector) { return querySelectorAllMock(this, selector)[0] || null; },
    querySelectorAll(selector) { return querySelectorAllMock(this, selector); },
    appendChild(child) {
      this.children.push(child);
      child.parentElement = this;
      return child;
    },
    get previousElementSibling() {
      if (!this.parentElement) return null;
      const idx = this.parentElement.children.indexOf(this);
      return idx > 0 ? this.parentElement.children[idx - 1] : null;
    },
    get classList() {
      return {
        add: (cls) => {
          const parts = (el.className || "").split(/\s+/).filter(Boolean);
          if (!parts.includes(cls)) parts.push(cls);
          el.className = parts.join(" ");
        },
        remove: (cls) => {
          el.className = (el.className || "").split(/\s+/).filter(c => c && c !== cls).join(" ");
        },
        contains: (cls) => (el.className || "").split(/\s+/).includes(cls)
      };
    },
    getBoundingClientRect() {
      const top = el.offsetTop - (win.scrollY || 0);
      return {
        top: top,
        bottom: top + (el.offsetHeight || 40),
        left: 0,
        right: el.offsetWidth || 200,
        width: el.offsetWidth || 200,
        height: el.offsetHeight || 40
      };
    },
    scrollIntoView(opts) {
      win.scrollY = Math.max(0, el.offsetTop - 200);
    }
  };
  return el;
}

const body = createMockElement("BODY");
const docListeners = {};
const winListeners = {};

const doc = {
  body: body,
  documentElement: {
    clientWidth: 1000,
    clientHeight: 800,
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  activeElement: null,
  getElementById(id) {
    const all = querySelectorAllMock(body, "#" + id);
    return all.length > 0 ? all[0] : null;
  },
  querySelector(selector) { return querySelectorAllMock(this.body, selector)[0] || null; },
  querySelectorAll(selector) { return querySelectorAllMock(this.body, selector); },
  createElement(tag) { return createMockElement(tag); },
  addEventListener(evt, fn) { (docListeners[evt] = docListeners[evt] || []).push(fn); },
  removeEventListener(evt, fn) {
    if (docListeners[evt]) docListeners[evt] = docListeners[evt].filter(f => f !== fn);
  }
};

const win = {
  scrollY: 0,
  innerHeight: 800,
  document: doc,
  addEventListener(evt, fn) { (winListeners[evt] = winListeners[evt] || []).push(fn); },
  removeEventListener(evt, fn) {
    if (winListeners[evt]) winListeners[evt] = winListeners[evt].filter(f => f !== fn);
  },
  location: { pathname: "/r/test/c/123", hash: "", search: "" },
  getSelection: () => ({ isCollapsed: true, toString: () => "" })
};
win.window = win;

const sandbox = {
  window: win,
  document: doc,
  navigator: { platform: "Linux" },
  setTimeout() {},
  clearTimeout() {},
  console
};
vm.createContext(sandbox);
vm.runInContext(code, sandbox);

function fireKey(key) {
  const evt = {
    key: key,
    target: doc.body,
    defaultPrevented: false,
    preventDefault: () => {}
  };
  const listeners = winListeners["keydown"] || [];
  for (const fn of listeners) {
    fn(evt);
  }
}

// 1. Thread card navigation test
const card0 = createMockElement("DIV");
card0.className = "thread-card";
card0.offsetTop = 100;
card0.offsetHeight = 50;

// Side-by-side comments with identical top
const card1a = createMockElement("DIV");
card1a.className = "thread-card";
card1a.offsetTop = 300;
card1a.offsetHeight = 50;

const card1b = createMockElement("DIV");
card1b.className = "thread-card";
card1b.offsetTop = 300;
card1b.offsetHeight = 50;

// Bottom of page comment at maxScrollY
const card2 = createMockElement("DIV");
card2.className = "thread-card";
card2.offsetTop = 1200;
card2.offsetHeight = 60;
card2.scrollIntoView = function() {
  // simulate hitting maxScrollY so card2 stays near the bottom of viewport
  win.scrollY = 800; // max scroll
};

body.appendChild(card0);
body.appendChild(card1a);
body.appendChild(card1b);
body.appendChild(card2);

const threadCards = win.getThreadCards();
if (threadCards.length !== 4) throw new Error("expected 4 thread cards, got " + threadCards.length);

// Test consecutive N: 0 -> 1a -> 1b -> 2 -> 0
win.scrollY = 0;
fireKey("N");
if (win.threadNavState.el !== card0) throw new Error("1st N should be card0");

fireKey("N");
if (win.threadNavState.el !== card1a) throw new Error("2nd N should be card1a");

fireKey("N");
if (win.threadNavState.el !== card1b) throw new Error("3rd N should advance to side-by-side card1b");

fireKey("N");
if (win.threadNavState.el !== card2) throw new Error("4th N should be card2 at bottom");

fireKey("N");
if (win.threadNavState.el !== card0) throw new Error("5th N should wrap to card0");

// Test consecutive P (including comments close together): 0 -> 2 -> 1b -> 1a -> 0
fireKey("P");
if (win.threadNavState.el !== card2) throw new Error("1st P should wrap backward to card2");

fireKey("P");
if (win.threadNavState.el !== card1b) throw new Error("2nd P should step back to card1b");

fireKey("P");
if (win.threadNavState.el !== card1a) throw new Error("3rd P should step back to card1a");

fireKey("P");
if (win.threadNavState.el !== card0) throw new Error("4th P should step back to card0");

// Test pointerdown / focusin updates threadNavState
const pointerdownListeners = docListeners["pointerdown"] || [];
for (const fn of pointerdownListeners) {
  fn({ button: 0, target: card1a });
}
if (win.threadNavState.el !== card1a) throw new Error("pointerdown on card1a should set threadNavState.el");

fireKey("N");
if (win.threadNavState.el !== card1b) throw new Error("N after clicking card1a should advance to card1b");

// Test manual scroll reset
win.scrollY = 5000; // scrolled far away so active card is out of view
card1b.offsetTop = 300; // top is far above viewport
fireKey("N");
// Should reset to card0 (or first card below header)
if (win.threadNavState.el !== card0) throw new Error("N after manual scroll should reset to card0");

// 2. Diff chunk navigation test
const table = createMockElement("TABLE");
table.className = "diff-table";
const tbody = createMockElement("TBODY");
table.appendChild(tbody);

// Chunk 1: Single modified row with both cell-del and cell-add (split row)
const trSplit = createMockElement("TR");
trSplit.className = "row-change";
const tdDel = createMockElement("TD");
tdDel.className = "cell-del";
const tdAdd = createMockElement("TD");
tdAdd.className = "cell-add";
trSplit.appendChild(tdDel);
trSplit.appendChild(tdAdd);
tbody.appendChild(trSplit);

// Context row separating chunk 1 and chunk 2
const trCtx0 = createMockElement("TR");
tbody.appendChild(trCtx0);

// Chunk 2: Contiguous addition rows
const trAdd1 = createMockElement("TR");
trAdd1.className = "row-add";
const trAdd2 = createMockElement("TR");
trAdd2.className = "row-add";
tbody.appendChild(trAdd1);
tbody.appendChild(trAdd2);

// Hidden row: should be excluded
const trHidden = createMockElement("TR");
trHidden.className = "row-add";
trHidden.offsetParent = null;
tbody.appendChild(trHidden);

// Chunk 3: Row separated by normal context
const trContext = createMockElement("TR");
tbody.appendChild(trContext);

const trDel2 = createMockElement("TR");
trDel2.className = "row-del";
tbody.appendChild(trDel2);

body.appendChild(table);

const chunks = win.getDiffChunks();
// trSplit (1 chunk, not 2!), trAdd1 (start of 2-row chunk), trDel2 (separated chunk)
if (chunks.length !== 3) {
  throw new Error("expected exactly 3 chunks, got " + chunks.length);
}
if (chunks[0] !== trSplit) throw new Error("chunk[0] should be trSplit");
if (chunks[1] !== trAdd1) throw new Error("chunk[1] should be trAdd1");
if (chunks[2] !== trDel2) throw new Error("chunk[2] should be trDel2");

// Test chunk stepping
win.chunkNavState.el = null;
fireKey("n");
if (win.chunkNavState.el !== trSplit) throw new Error("1st n should be chunk[0]");
fireKey("n");
if (win.chunkNavState.el !== trAdd1) throw new Error("2nd n should be chunk[1]");
fireKey("n");
if (win.chunkNavState.el !== trDel2) throw new Error("3rd n should be chunk[2]");
fireKey("n");
if (win.chunkNavState.el !== trSplit) throw new Error("4th n should wrap to chunk[0]");

fireKey("p");
if (win.chunkNavState.el !== trDel2) throw new Error("1st p should wrap backward to chunk[2]");

console.log("OK");
`
	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TestKeyboardCommentAndChunkNavigation failed: %v\n%s", err, string(out))
	}
}

func TestIntralineTokenNestingAndStyle(t *testing.T) {
	// Verify style.css rules for mark.intra inside hovered tokens and sym-active
	cssBytes, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	css := string(cssBytes)

	reqRules := []string{
		".tok-id:hover mark.intra",
		".tok-typ:hover mark.intra",
		".tok-imp:hover mark.intra",
		".sym-active mark.intra",
		"background-color: transparent !important",
	}
	for _, rule := range reqRules {
		if !strings.Contains(css, rule) {
			t.Errorf("style.css missing required rule %q", rule)
		}
	}

	// Verify rendered token HTML with intraline diff
	line := "func EscapePath(path string) string {"
	tokens := []diff.TokenSpan{
		{Start: 0, End: 4, Class: diff.TokenKeyword},
		{Start: 5, End: 15, Class: diff.TokenIdent},  // EscapePath
		{Start: 16, End: 20, Class: diff.TokenIdent}, // path
		{Start: 21, End: 27, Class: diff.TokenIdent}, // string
		{Start: 29, End: 35, Class: diff.TokenIdent}, // string
	}
	intra := []diff.Span{
		{Start: 11, End: 15}, // Path (sub-interval of EscapePath)
	}
	rendered := string(diff.RenderTokensHTMLWS(line, tokens, intra, false))
	wantToken := `<span class="tok-id">Escape<mark class="intra">Path</mark></span>`
	if !strings.Contains(rendered, wantToken) {
		t.Errorf("rendered HTML missing expected nested token %q, got: %s", wantToken, rendered)
	}
}

func TestSymbolPopupEscapeAndRightClickHistoryBack(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	nodeScript := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function createMockElement(tag) {
  const el = {
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    children: [],
    parentElement: null,
    style: {},
    listeners: {},
    focus() { doc.activeElement = el; },
    blur() { if (doc.activeElement === el) doc.activeElement = null; },
    getBoundingClientRect() {
      return this._rect || { left: 0, top: 0, right: 100, bottom: 100, width: 100, height: 100 };
    },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    removeAttribute(k) { delete this.attributes[k]; },
    addEventListener(evt, fn) { (this.listeners[evt] = this.listeners[evt] || []).push(fn); },
    removeEventListener(evt, fn) {
      if (this.listeners[evt]) this.listeners[evt] = this.listeners[evt].filter(f => f !== fn);
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    closest(selector) {
      let cur = this;
      while (cur) {
        if (cur.className && cur.className.split(/\s+/).includes(selector.replace(/^\./, ""))) return cur;
        cur = cur.parentElement;
      }
      return null;
    },
    querySelector(selector) {
      if (selector === ".sym-popup-content") {
        for (const child of el.children) {
          if (child.className && child.className.includes("sym-popup-content")) return child;
        }
      }
      return null;
    },
    querySelectorAll() { return []; },
    appendChild(child) {
      this.children.push(child);
      child.parentElement = this;
      return child;
    },
    get classList() {
      return {
        add: (cls) => {
          const parts = (el.className || "").split(/\s+/).filter(Boolean);
          if (!parts.includes(cls)) parts.push(cls);
          el.className = parts.join(" ");
        },
        remove: (cls) => {
          el.className = (el.className || "").split(/\s+/).filter(c => c && c !== cls).join(" ");
        },
        contains: (cls) => (el.className || "").split(/\s+/).includes(cls)
      };
    }
  };
  return el;
}

const body = createMockElement("BODY");
const docListeners = {};
const winListeners = {};

const doc = {
  body: body,
  documentElement: {
    clientWidth: 1000,
    clientHeight: 800,
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  activeElement: null,
  getElementById(id) { return null; },
  querySelector(selector) {
    if (selector === ".symbol-popup") {
      for (const child of body.children) {
        if (child.className && child.className.includes("symbol-popup")) return child;
      }
    }
    return null;
  },
  querySelectorAll(selector) {
    if (selector === ".symbol-popup") {
      return body.children.filter(c => c.className && c.className.includes("symbol-popup"));
    }
    return [];
  },
  createElement(tag) { return createMockElement(tag); },
  addEventListener(evt, fn) { (docListeners[evt] = docListeners[evt] || []).push(fn); },
  removeEventListener(evt, fn) {
    if (docListeners[evt]) docListeners[evt] = docListeners[evt].filter(f => f !== fn);
  }
};

const win = {
  scrollY: 0,
  innerHeight: 800,
  document: doc,
  addEventListener(evt, fn) { (winListeners[evt] = winListeners[evt] || []).push(fn); },
  removeEventListener(evt, fn) {
    if (winListeners[evt]) winListeners[evt] = winListeners[evt].filter(f => f !== fn);
  },
  location: { pathname: "/r/test/c/123", hash: "", search: "" },
  getSelection: () => ({ isCollapsed: true, toString: () => "" })
};
win.window = win;

const sandbox = {
  window: win,
  document: doc,
  navigator: { platform: "Linux" },
  setTimeout() {},
  clearTimeout() {},
  console
};
vm.createContext(sandbox);
vm.runInContext(code, sandbox);

// Case 1: Focus/interaction INSIDE popup navigates back on Escape or blank right-click
const popupEl = createMockElement("DIV");
popupEl.className = "symbol-popup";
popupEl.style.zIndex = "205";
popupEl._rect = { left: 10, top: 10, right: 200, bottom: 200, width: 190, height: 190 };
const contentEl = createMockElement("DIV");
contentEl.className = "sym-popup-content";
popupEl.appendChild(contentEl);
body.appendChild(popupEl);

const inst = {
  el: popupEl,
  _zOrder: 205,
  history: [
    { kind: "symbol", data: { symbol_name: "RootSymbol" }, ctx: {}, scrollTop: 0 },
    { kind: "refs", data: { symbol_name: "RootSymbol", refs: [] }, ctx: {}, scrollTop: 0 }
  ],
  currentState: { kind: "symbol", data: { symbol_name: "ChainedSymbol" }, ctx: {}, scrollTop: 0 }
};
win.openSymbolPopups.push(inst);
win.setActivePopupInst(inst);
doc.activeElement = popupEl;

// 1st Escape inside -> should step back to refs list, history length becomes 1, popup stays open
let handled = win.triggerEscapeAction(popupEl);
if (!handled) throw new Error("1st Escape inside should be handled");
if (inst.history.length !== 1) throw new Error("history.length should be 1, got " + inst.history.length);
if (inst.currentState.kind !== "refs") throw new Error("currentState should be refs, got " + inst.currentState.kind);
if (win.openSymbolPopups.length !== 1) throw new Error("popup should still be open after 1st Escape");

// 2nd step back (via blank right-click inside popup) -> should step back to root symbol
const ctxListeners = docListeners["contextmenu"] || [];
let prevented = false;
for (const fn of ctxListeners) {
  fn({ button: 2, clientX: 50, clientY: 50, defaultPrevented: false, preventDefault: () => { prevented = true; }, target: popupEl });
}
if (!prevented) throw new Error("blank right click inside popup should call preventDefault");
if (inst.history.length !== 0) throw new Error("history.length should be 0, got " + inst.history.length);
if (inst.currentState.kind !== "symbol") throw new Error("currentState should be symbol, got " + inst.currentState.kind);
if (win.openSymbolPopups.length !== 1) throw new Error("popup should still be open after right-click inside");

// 3rd Escape inside -> history is 0, so should close popup
handled = win.triggerEscapeAction(popupEl);
if (!handled) throw new Error("3rd Escape inside should be handled");
if (win.openSymbolPopups.length !== 0) throw new Error("popup should be closed after 3rd Escape inside");

// Case 2: Escape or blank right-click OUTSIDE popup closes immediately without navigating back
const popupElOutside1 = createMockElement("DIV");
popupElOutside1.className = "symbol-popup";
popupElOutside1.style.zIndex = "205";
popupElOutside1._rect = { left: 10, top: 10, right: 200, bottom: 200, width: 190, height: 190 };
body.appendChild(popupElOutside1);
const instOutside1 = {
  el: popupElOutside1,
  _zOrder: 205,
  history: [
    { kind: "symbol", data: { symbol_name: "RootSymbol" }, ctx: {} },
    { kind: "refs", data: { symbol_name: "RootSymbol" }, ctx: {} }
  ],
  currentState: { kind: "symbol", data: { symbol_name: "ChainedSymbol" }, ctx: {} }
};
win.openSymbolPopups.push(instOutside1);
win.setActivePopupInst(null);
doc.activeElement = doc.body;

// Blank right-click OUTSIDE popup -> must close immediately
prevented = false;
for (const fn of ctxListeners) {
  fn({ button: 2, clientX: 500, clientY: 500, defaultPrevented: false, preventDefault: () => { prevented = true; }, target: doc.body });
}
if (!prevented) throw new Error("blank right click outside should call preventDefault");
if (win.openSymbolPopups.length !== 0) throw new Error("blank right click outside should close popup immediately");

// Escape OUTSIDE popup -> must close immediately
const popupElOutside2 = createMockElement("DIV");
popupElOutside2.className = "symbol-popup";
popupElOutside2.style.zIndex = "205";
popupElOutside2._rect = { left: 10, top: 10, right: 200, bottom: 200, width: 190, height: 190 };
body.appendChild(popupElOutside2);
const instOutside2 = {
  el: popupElOutside2,
  _zOrder: 205,
  history: [
    { kind: "symbol", data: { symbol_name: "RootSymbol" }, ctx: {} },
    { kind: "refs", data: { symbol_name: "RootSymbol" }, ctx: {} }
  ],
  currentState: { kind: "symbol", data: { symbol_name: "ChainedSymbol" }, ctx: {} }
};
win.openSymbolPopups.push(instOutside2);
win.setActivePopupInst(null);
doc.activeElement = doc.body;

handled = win.triggerEscapeAction(doc.body);
if (!handled) throw new Error("Escape outside should be handled");
if (win.openSymbolPopups.length !== 0) throw new Error("Escape outside should close popup immediately despite history");

// Case 3: Explicit close button closes immediately even when history > 0
const popupEl2 = createMockElement("DIV");
popupEl2.className = "symbol-popup";
body.appendChild(popupEl2);
const inst2 = {
  el: popupEl2,
  _zOrder: 205,
  history: [
    { kind: "symbol", data: { symbol_name: "RootSymbol" }, ctx: {} }
  ],
  currentState: { kind: "refs", data: { symbol_name: "RootSymbol" }, ctx: {} }
};
win.openSymbolPopups.push(inst2);

// Simulate closeSpecificSymbolPopup (which .sym-btn-close calls)
win.closeSpecificSymbolPopup(inst2);
if (win.openSymbolPopups.length !== 0) throw new Error("explicit close should close popup immediately despite history");

// Case 4a: Pointerdown inside popup sets activeElement, then pointermove outside popup -> Escape must close popup immediately
const popupEl4a = createMockElement("DIV");
popupEl4a.className = "symbol-popup";
popupEl4a.style.zIndex = "205";
popupEl4a._rect = { left: 50, top: 50, right: 300, bottom: 300, width: 250, height: 250 };
body.appendChild(popupEl4a);
const inst4a = {
  el: popupEl4a,
  _zOrder: 205,
  history: [
    { kind: "symbol", data: { symbol_name: "RootSymbol" }, ctx: {} }
  ],
  currentState: { kind: "refs", data: { symbol_name: "RootSymbol" }, ctx: {} }
};
win.openSymbolPopups.push(inst4a);

// Pointerdown inside popup
const pdListeners = docListeners["pointerdown"] || [];
for (const fn of pdListeners) {
  fn({ clientX: 100, clientY: 100, target: popupEl4a });
}
if (doc.activeElement !== popupEl4a) throw new Error("pointerdown inside popup should focus popup");

// Pointermove outside popup
const pmListeners = docListeners["pointermove"] || [];
for (const fn of pmListeners) {
  fn({ clientX: 10, clientY: 10 });
}

// Escape with doc.activeElement passed -> must close immediately because cursor is outside
handled = win.triggerEscapeAction(doc.activeElement);
if (!handled) throw new Error("Escape outside should be handled");
if (win.openSymbolPopups.length !== 0) throw new Error("Escape after pointer moved outside must close popup immediately");

// Case 4b: Pointerdown near bottom of popup, popup shrinks under stationary cursor -> Escape must navigate back
const popupEl4b = createMockElement("DIV");
popupEl4b.className = "symbol-popup";
popupEl4b.style.zIndex = "205";
popupEl4b._rect = { left: 50, top: 50, right: 300, bottom: 400, width: 250, height: 350 };
body.appendChild(popupEl4b);
const inst4b = {
  el: popupEl4b,
  _zOrder: 205,
  history: [
    { kind: "symbol", data: { symbol_name: "RootSymbol" }, ctx: {} }
  ],
  currentState: { kind: "refs", data: { symbol_name: "RootSymbol" }, ctx: {} }
};
win.openSymbolPopups.push(inst4b);

// Pointerdown inside popup at clientY: 350
for (const fn of pdListeners) {
  fn({ clientX: 100, clientY: 350, target: popupEl4b });
}
// Popup shrinks vertically to bottom: 200
popupEl4b._rect = { left: 50, top: 50, right: 300, bottom: 200, width: 250, height: 150 };

// 0-delta pointermove (Chromium synthetic move after layout shift)
for (const fn of pmListeners) {
  fn({ clientX: 100, clientY: 350 });
}

// Escape -> stationary cursor was inside when clicked, must navigate back, not close!
handled = win.triggerEscapeAction(doc.activeElement);
if (!handled) throw new Error("Escape after popup shrink should be handled");
if (inst4b.history.length !== 0) throw new Error("Escape after popup shrink should navigate back (history 0)");
if (win.openSymbolPopups.length !== 1) throw new Error("popup should remain open after navigating back");

win.closeSpecificSymbolPopup(inst4b);

console.log("OK");
`
	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TestSymbolPopupEscapeAndRightClickHistoryBack failed: %v\n%s", err, string(out))
	}
}

func TestPreserveDiffViewMode(t *testing.T) {
	ts, rec, changeKey := setupTestServerForUI(t)

	// 1. Single-file changed diff with mode=split
	urlSplit := fmt.Sprintf("%s/r/%s/c/%s/f/pkg/util.go?mode=split", ts.URL, rec.Slug, changeKey)
	resp, err := http.Get(urlSplit)
	if err != nil {
		t.Fatalf("GET /f split failed: %v", err)
	}
	bodySplit, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	strSplit := string(bodySplit)
	if !strings.Contains(strSplit, `id="upLink"`) || !strings.Contains(strSplit, `mode=split`) {
		t.Errorf("expected upLink to preserve mode=split, got body without it")
	}

	// 2. Single-file changed diff with mode=unified
	urlUnified := fmt.Sprintf("%s/r/%s/c/%s/f/pkg/util.go?mode=unified", ts.URL, rec.Slug, changeKey)
	resp, err = http.Get(urlUnified)
	if err != nil {
		t.Fatalf("GET /f unified failed: %v", err)
	}
	bodyUnified, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	strUnified := string(bodyUnified)
	if !strings.Contains(strUnified, `id="upLink"`) || !strings.Contains(strUnified, `mode=unified`) {
		t.Errorf("expected upLink to preserve mode=unified, got body without it")
	}

	// 3. Unchanged file in repo with mode=split (renders single-column unified diff table, but upLink has mode=split)
	urlUnchanged := fmt.Sprintf("%s/r/%s/c/%s/f/main.go?mode=split", ts.URL, rec.Slug, changeKey)
	resp, err = http.Get(urlUnchanged)
	if err != nil {
		t.Fatalf("GET /f main.go split failed: %v", err)
	}
	bodyUnchanged, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	strUnchanged := string(bodyUnchanged)
	if !strings.Contains(strUnchanged, "diff-table-single") {
		t.Errorf("expected unchanged file to render diff-table-single")
	}
	if !strings.Contains(strUnchanged, `id="upLink"`) || !strings.Contains(strUnchanged, `mode=split`) {
		t.Errorf("expected upLink on unchanged file to preserve requested mode=split")
	}

	// 4. External file with mode=split
	urlExt := fmt.Sprintf("%s/r/%s/c/%s/ext?pkg=context&file=context.go&mode=split", ts.URL, rec.Slug, changeKey)
	resp, err = http.Get(urlExt)
	if err != nil {
		t.Fatalf("GET /ext split failed: %v", err)
	}
	bodyExt, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	strExt := string(bodyExt)
	if !strings.Contains(strExt, "diff-table-single") {
		t.Errorf("expected external file to render diff-table-single")
	}
	if !strings.Contains(strExt, `id="upLink"`) || !strings.Contains(strExt, `mode=split`) {
		t.Errorf("expected upLink on external file to preserve requested mode=split")
	}

	// 5. Change overview page with mode=split (test diffs=list and diffs=all)
	urlOverviewList := fmt.Sprintf("%s/r/%s/c/%s?diffs=list&mode=split", ts.URL, rec.Slug, changeKey)
	resp, err = http.Get(urlOverviewList)
	if err != nil {
		t.Fatalf("GET change overview diffs=list split failed: %v", err)
	}
	bodyOverviewList, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	strOverviewList := string(bodyOverviewList)
	if !strings.Contains(strOverviewList, `class="file-row"`) || !strings.Contains(strOverviewList, `mode=split`) {
		t.Errorf("expected file-row data-href to contain mode=split in diffs=list")
	}
	if !strings.Contains(strOverviewList, `class="file-link"`) || !strings.Contains(strOverviewList, `mode=split`) {
		t.Errorf("expected file-link href to contain mode=split in diffs=list")
	}

	urlOverviewAll := fmt.Sprintf("%s/r/%s/c/%s?diffs=all&mode=split", ts.URL, rec.Slug, changeKey)
	resp, err = http.Get(urlOverviewAll)
	if err != nil {
		t.Fatalf("GET change overview diffs=all split failed: %v", err)
	}
	bodyOverviewAll, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	strOverviewAll := string(bodyOverviewAll)
	if !strings.Contains(strOverviewAll, "Single file &rarr;") || !strings.Contains(strOverviewAll, "mode=split") {
		t.Errorf("expected Single file link to contain mode=split in diffs=all")
	}

	// 6. Test app.js jump helpers via node
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		return
	}
	nodeScript := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

let urlSearch = "?mode=split";
const win = {
  URLSearchParams: URLSearchParams,
  get location() {
    return {
      search: urlSearch,
      pathname: "/r/test/c/123",
      href: "http://localhost/r/test/c/123" + urlSearch,
    };
  },
  navigator: { userAgent: "test", clipboard: { writeText: async () => {} } },
  addEventListener: () => {},
  removeEventListener: () => {},
  requestAnimationFrame: (cb) => setTimeout(cb, 0),
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  openSymbolPopups: [],
  document: {
    readyState: "complete",
    documentElement: { getAttribute() { return ""; }, setAttribute() {}, removeAttribute() {} },
    getElementById: () => null,
    querySelector: () => null,
    querySelectorAll: () => [],
    addEventListener: () => {},
    removeEventListener: () => {},
    body: { appendChild: () => {}, addEventListener: () => {}, removeEventListener: () => {} },
  }
};
win.window = win;
const doc = win.document;
const ctx = vm.createContext(win);
vm.runInContext(code, ctx);

// Case 6a: mode from URL search param
if (win.getCurrentPageDiffMode() !== "split") {
  throw new Error("expected getCurrentPageDiffMode to return split, got " + win.getCurrentPageDiffMode());
}

const inRepoHref = win.handleJumpToDefinition ? null : null;
const testCtx = { repo: "test", change: "123", base: "abc", target: "def", side: "R", mode: "split" };
// Test URL generation directly or through exported helpers
urlSearch = "?view=unified";
if (win.getCurrentPageDiffMode() !== "unified") {
  throw new Error("expected getCurrentPageDiffMode to return unified for view=unified, got " + win.getCurrentPageDiffMode());
}

// Case 6b: mode fallback from DOM
urlSearch = "";
doc.querySelector = (sel) => {
  if (sel === ".diff-table-split") return {};
  return null;
};
if (win.getCurrentPageDiffMode() !== "split") {
  throw new Error("expected getCurrentPageDiffMode to detect split from DOM, got " + win.getCurrentPageDiffMode());
}

doc.querySelector = (sel) => {
  if (sel === ".diff-table:not(.diff-table-single)") return {};
  return null;
};
if (win.getCurrentPageDiffMode() !== "unified") {
  throw new Error("expected getCurrentPageDiffMode to detect unified from DOM, got " + win.getCurrentPageDiffMode());
}

console.log("OK");
`
	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test in TestPreserveDiffViewMode failed: %v\n%s", err, string(out))
	}
}

func TestCodeDiffAndCommitMsgWidthInvariants(t *testing.T) {
	cssBytes, err := os.ReadFile(filepath.Join("static", "style.css"))
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	css := string(cssBytes)

	// Check container rules
	if !strings.Contains(css, ".container {") ||
		!strings.Contains(css, "max-width: calc(200ch + 164px);") {
		t.Errorf("expected calc(200ch + 164px) constraint on .container in style.css")
	}

	if !strings.Contains(css, ".container > * {") ||
		!strings.Contains(css, "font-family: var(--font-sans);") ||
		!strings.Contains(css, "font-size: 14px;") {
		t.Errorf("expected 14px sans font restoration on .container > * in style.css")
	}

	// Check commit message grid 90ch + 26px
	if !strings.Contains(css, "grid-template-columns: minmax(0, calc(90ch + 26px)) minmax(0, 1fr);") {
		t.Errorf("expected 90ch + 26px column sizing for commit message in style.css")
	}
}

func TestSymbolPopupEdgeAndCornerSnap(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	nodeScript := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function createMockElement(tag) {
  const el = {
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    children: [],
    parentElement: null,
    style: {},
    listeners: {},
    get offsetHeight() { return (this._rect && this._rect.height) || 0; },
    get offsetWidth() { return (this._rect && this._rect.width) || 0; },
    focus() { doc.activeElement = el; },
    blur() { if (doc.activeElement === el) doc.activeElement = null; },
    getBoundingClientRect() {
      return this._rect || { left: 0, top: 0, right: 100, bottom: 100, width: 100, height: 100 };
    },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    removeAttribute(k) { delete this.attributes[k]; },
    addEventListener(evt, fn) { (this.listeners[evt] = this.listeners[evt] || []).push(fn); },
    removeEventListener(evt, fn) {
      if (this.listeners[evt]) this.listeners[evt] = this.listeners[evt].filter(f => f !== fn);
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    closest(selector) {
      let cur = this;
      while (cur) {
        if (cur.className && cur.className.split(/\s+/).includes(selector.replace(/^\./, ""))) return cur;
        cur = cur.parentElement;
      }
      return null;
    },
    querySelector(selector) {
      let found = null;
      function walk(n) {
        if (found) return;
        for (const child of n.children) {
          if (selector.startsWith(".") && child.className && child.className.split(/\s+/).includes(selector.slice(1))) {
            found = child;
            return;
          }
          if (child.tagName === selector.toUpperCase()) {
            found = child;
            return;
          }
          walk(child);
          if (found) return;
        }
      }
      walk(this);
      return found;
    },
    querySelectorAll(selector) {
      const res = [];
      function walk(n) {
        for (const c of n.children) {
          if (selector === ".diff-file-section:not(.collapsed)") {
            if (c.className && c.className.includes("diff-file-section") && !c.className.includes("collapsed")) {
              res.push(c);
            }
          } else if (selector.startsWith(".") && c.className && c.className.includes(selector.slice(1))) {
            res.push(c);
          }
          walk(c);
        }
      }
      walk(this);
      return res;
    },
    appendChild(child) {
      this.children.push(child);
      child.parentElement = this;
      child.parentNode = this;
      return child;
    },
    removeChild(child) {
      const idx = this.children.indexOf(child);
      if (idx !== -1) {
        this.children.splice(idx, 1);
        child.parentElement = null;
        child.parentNode = null;
      }
      return child;
    },
    get classList() {
      return {
        add: (cls) => {
          const parts = (el.className || "").split(/\s+/).filter(Boolean);
          if (!parts.includes(cls)) parts.push(cls);
          el.className = parts.join(" ");
        },
        remove: (cls) => {
          el.className = (el.className || "").split(/\s+/).filter(c => c && c !== cls).join(" ");
        },
        contains: (cls) => (el.className || "").split(/\s+/).includes(cls)
      };
    }
  };
  return el;
}

const body = createMockElement("BODY");
const docListeners = {};
const winListeners = {};

const appHeader = createMockElement("HEADER");
appHeader.className = "app-header";
appHeader._rect = { left: 0, top: 0, right: 1000, bottom: 42.5, width: 1000, height: 42.5 };
body.appendChild(appHeader);

const doc = {
  body: body,
  documentElement: {
    clientWidth: 1000,
    clientHeight: 800,
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  activeElement: null,
  getElementById(id) { return null; },
  querySelector(selector) {
    if (selector === "header.app-header" || selector === ".app-header") return appHeader;
    if (selector === ".symbol-popup") {
      for (const child of body.children) {
        if (child.className && child.className.includes("symbol-popup")) return child;
      }
    }
    if (selector.includes(".diff-file-header.is-hanging") || selector === ".diff-file-header.is-hanging") {
      for (const s of body.children) {
        if (s.className && s.className.includes("diff-file-section") && !s.className.includes("collapsed")) {
          for (const h of s.children) {
            if (h.className && h.className.includes("diff-file-header") && h.className.includes("is-hanging")) return h;
          }
        }
      }
    }
    return body.querySelector(selector);
  },
  querySelectorAll(selector) {
    if (selector === ".symbol-popup") {
      return body.children.filter(c => c.className && c.className.includes("symbol-popup"));
    }
    return body.querySelectorAll(selector);
  },
  createElement(tag) { return createMockElement(tag); },
  addEventListener(evt, fn) { (docListeners[evt] = docListeners[evt] || []).push(fn); },
  removeEventListener(evt, fn) {
    if (docListeners[evt]) docListeners[evt] = docListeners[evt].filter(f => f !== fn);
  }
};

const win = {
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  document: doc,
  addEventListener(evt, fn) { (winListeners[evt] = winListeners[evt] || []).push(fn); },
  removeEventListener(evt, fn) {
    if (winListeners[evt]) winListeners[evt] = winListeners[evt].filter(f => f !== fn);
  },
  location: { pathname: "/r/test/c/123", hash: "", search: "" },
  getSelection: () => ({ isCollapsed: true, toString: () => "" })
};
win.window = win;

const sandbox = {
  window: win,
  document: doc,
  navigator: { platform: "Linux" },
  setTimeout() {},
  clearTimeout() {},
  console
};
vm.createContext(sandbox);
vm.runInContext(code, sandbox);

// 1. Test getFloatingTopClearance() and getSnapTopClearance() with appHeader alone
const clearance1 = win.getFloatingTopClearance();
if (clearance1 !== 42.5) {
  throw new Error("expected base appHeader clearance 42.5, got " + clearance1);
}
const snapClearance1 = win.getSnapTopClearance();
if (snapClearance1 !== 42.5) {
  throw new Error("expected base appHeader snap clearance 42.5, got " + snapClearance1);
}

// 2. Test getFloatingTopClearance() and getSnapTopClearance() with a sticky diff file header
const sec = createMockElement("DIV");
sec.className = "diff-file-section";
sec._rect = { left: 0, top: 20, right: 1000, bottom: 600, width: 1000, height: 580 };
const fileHdr = createMockElement("DIV");
fileHdr.className = "diff-file-header";
fileHdr.offsetHeight = 35;
fileHdr._rect = { left: 0, top: 42.5, right: 1000, bottom: 77.5, width: 1000, height: 35 };
sec.appendChild(fileHdr);
body.appendChild(sec);

const clearance2 = win.getFloatingTopClearance();
if (clearance2 !== 77.5) {
  throw new Error("expected combined sticky file header clearance 77.5, got " + clearance2);
}
const snapClearance2 = win.getSnapTopClearance();
if (snapClearance2 !== 77.5) {
  throw new Error("expected combined sticky file header snap clearance 77.5, got " + snapClearance2);
}

// Test partially scrolled-off hanging header falls back to appH + hdrH (77.5) instead of 55
fileHdr.className = "diff-file-header is-hanging";
const origHdrRect = fileHdr._rect;
const origSecRect = sec._rect;
fileHdr._rect = { left: 0, top: 20, right: 1000, bottom: 55, width: 1000, height: 35 };
sec._rect = { left: 0, top: -500, right: 1000, bottom: 55, width: 1000, height: 555 };
const snapClearanceScrolledOff = win.getSnapTopClearance();
if (snapClearanceScrolledOff !== 77.5) {
  throw new Error("expected partially scrolled-off header to fall back to 77.5, got " + snapClearanceScrolledOff);
}

// When hanging header is properly anchored below appHeader, getSnapTopClearance returns its bottom
fileHdr._rect = origHdrRect;
sec._rect = origSecRect;
const snapClearanceAnchored = win.getSnapTopClearance();
if (snapClearanceAnchored !== 77.5) {
  throw new Error("expected anchored hanging header clearance 77.5, got " + snapClearanceAnchored);
}
fileHdr.className = "diff-file-header";

// When diff file header is present, computeSnapRect docks flush below 77.5 with 0px gap
const snapRectWithHdr = win.computeSnapRect("left");
if (snapRectWithHdr.top !== 77.5) {
  throw new Error("expected computeSnapRect to dock below diff file header at 77.5, got " + snapRectWithHdr.top);
}

// Remove the section to test standard clearance 42.5 for snap zone detection
body.removeChild(sec);
const snapClearanceRemoved = win.getSnapTopClearance();
if (snapClearanceRemoved !== 42.5) {
  throw new Error("expected snap clearance 42.5 after section removed, got " + snapClearanceRemoved);
}
const snapRectWithoutHdr = win.computeSnapRect("left");
if (snapRectWithoutHdr.top !== 42.5) {
  throw new Error("expected computeSnapRect to dock at 42.5 without diff file header, got " + snapRectWithoutHdr.top);
}

// 3. Test detectSnapZone across all 8 zones
const testCases = [
  { x: 10, y: 55, expected: "top-left" },
  { x: 990, y: 55, expected: "top-right" },
  { x: 10, y: 790, expected: "bottom-left" },
  { x: 990, y: 790, expected: "bottom-right" },
  { x: 10, y: 400, expected: "left" },
  { x: 990, y: 400, expected: "right" },
  { x: 500, y: 55, expected: "top" },
  { x: 500, y: 790, expected: "bottom" },
  { x: 500, y: 400, expected: null },
];

for (const tc of testCases) {
  const actual = win.detectSnapZone(tc.x, tc.y);
  if (actual !== tc.expected) {
    throw new Error("detectSnapZone(" + tc.x + ", " + tc.y + ") expected " + tc.expected + ", got " + actual);
  }
}

// 4. Test computeSnapRect for all 8 zones
const zones = ["left", "right", "top", "bottom", "top-left", "top-right", "bottom-left", "bottom-right"];
const rects = {};
for (const z of zones) {
  const r = win.computeSnapRect(z);
  if (!r) throw new Error("computeSnapRect returned null for " + z);
  rects[z] = r;
}

// Check left/right partition
if (rects.left.left !== 6) throw new Error("rects.left.left expected 6, got " + rects.left.left);
if (rects.right.left !== rects.left.left + rects.left.width + 6) {
  throw new Error("expected 6px gap between left and right zones");
}
if (rects.right.left + rects.right.width !== 1000 - 6) {
  throw new Error("expected right zone to reach vw - 6");
}

// Check top/bottom partition
if (rects.top.top !== 42.5) throw new Error("rects.top.top expected 42.5, got " + rects.top.top);
if (rects.bottom.top !== rects.top.top + rects.top.height + 6) {
  throw new Error("expected 6px gap between top and bottom zones");
}
if (rects.bottom.top + rects.bottom.height !== 800 - 6) {
  throw new Error("expected bottom zone to reach vh - 6");
}

// Check quarter corners match half boundaries
if (rects["top-left"].width !== rects.left.width || rects["top-left"].height !== rects.top.height) {
  throw new Error("top-left dimensions mismatch with halves");
}
if (rects["top-right"].width !== rects.right.width || rects["top-right"].height !== rects.top.height) {
  throw new Error("top-right dimensions mismatch with halves");
}
if (rects["bottom-left"].width !== rects.left.width || rects["bottom-left"].height !== rects.bottom.height) {
  throw new Error("bottom-left dimensions mismatch with halves");
}
if (rects["bottom-right"].width !== rects.right.width || rects["bottom-right"].height !== rects.bottom.height) {
  throw new Error("bottom-right dimensions mismatch with halves");
}

// 5. Test applyPopupSnap and unsnap
const popupEl = createMockElement("DIV");
popupEl.className = "symbol-popup";
popupEl._rect = { left: 100, top: 120, right: 780, bottom: 480, width: 680, height: 360 };
body.appendChild(popupEl);

const inst = {
  el: popupEl,
  snapped: false,
  resizedW: false,
  resizedH: false,
};
win.openSymbolPopups.push(inst);

win.applyPopupSnap(inst, "left");
if (!inst.snapped || inst.snapZone !== "left") {
  throw new Error("expected inst.snapped=true and snapZone=left");
}
if (!popupEl.classList.contains("sym-snapped") || popupEl.style.position !== "fixed") {
  throw new Error("expected sym-snapped class and position: fixed on snapped popup");
}
if (!popupEl.classList.contains("sym-snapped-top")) {
  throw new Error("expected sym-snapped-top class on top-docked snapped popup");
}
if (popupEl.style.width !== rects.left.width + "px" || popupEl.style.height !== rects.left.height + "px") {
  throw new Error("popup dimensions did not match snapped left rect");
}

// Test bottom snap does NOT have sym-snapped-top
win.applyPopupSnap(inst, "bottom");
if (popupEl.classList.contains("sym-snapped-top")) {
  throw new Error("bottom snap must NOT have sym-snapped-top");
}
win.applyPopupSnap(inst, "left"); // restore

// 6. Test dynamic clearance update for snapped popup when file header becomes sticky
body.appendChild(sec); // adds sec with header bottom 77.5
for (const fn of (winListeners["resize"] || [])) {
  fn();
}

if (parseFloat(popupEl.style.top) !== 77.5) {
  throw new Error("expected snapped popup top to dynamically adjust to 77.5 on resize below floating file header, got " + popupEl.style.top);
}

// 7. Test live preview creation and cleanup
if (body.querySelector(".sym-snap-preview")) {
  throw new Error(".sym-snap-preview should not exist before showSnapPreview");
}
const previewRect = { left: 6, top: 77.5, width: 491, height: 716.5 };
win.showSnapPreview(previewRect, 205);
const previewEl = body.querySelector(".sym-snap-preview");
if (!previewEl) {
  throw new Error("expected .sym-snap-preview to be created in body");
}
if (previewEl.style.left !== "6px" || previewEl.style.zIndex !== "204") {
  throw new Error("preview styles incorrect: left=" + previewEl.style.left + " zIndex=" + previewEl.style.zIndex);
}
if (!previewEl.classList.contains("sym-snapped-top")) {
  throw new Error("expected sym-snapped-top class on top-docked preview");
}
win.hideSnapPreview();
if (body.querySelector(".sym-snap-preview")) {
  throw new Error("expected .sym-snap-preview to be removed after hideSnapPreview");
}

// 7b. Test south resize of snapped popup clamps at vh - 6
const handleS = createMockElement("DIV");
handleS.className = "sym-resize-handle sym-resize-s";
handleS.setAttribute("data-resize-dir", "s");
popupEl.appendChild(handleS);

win.setupDragAndResize(inst);

for (const fn of (popupEl.listeners["pointerdown"] || [])) {
  fn({
    button: 0,
    clientX: 100,
    clientY: 100,
    target: handleS,
    preventDefault() {},
    stopPropagation() {}
  });
}

for (const fn of (winListeners["pointermove"] || [])) {
  fn({
    clientX: 100,
    clientY: 2000
  });
}

const expectedMaxH = (800 - 6) - parseFloat(popupEl.style.top);
if (Math.abs(parseFloat(popupEl.style.height) - expectedMaxH) > 0.01) {
  throw new Error("expected snapped south resize to clamp to " + expectedMaxH + "px, got " + popupEl.style.height);
}

for (const fn of (winListeners["pointerup"] || [])) {
  fn({});
}
handleS.remove();

// 8. Test KDE-style unsnap on drag
const hdrEl = createMockElement("DIV");
hdrEl.className = "sym-popup-header";
popupEl.appendChild(hdrEl);

win.setupDragAndResize(inst);

if (!inst.snapped) throw new Error("inst should still be snapped before drag");

// Pointer down on header at (100, 60)
for (const fn of (popupEl.listeners["pointerdown"] || [])) {
  fn({
    button: 0,
    clientX: 100,
    clientY: 60,
    target: hdrEl,
    preventDefault() {},
    stopPropagation() {}
  });
}

// Pointer moves past threshold to (250, 180) (center, not near any edge)
for (const fn of (winListeners["pointermove"] || [])) {
  fn({
    clientX: 250,
    clientY: 180
  });
}

if (inst.snapped) {
  throw new Error("expected inst.snapped to be false after dragging away from edge");
}
if (popupEl.classList.contains("sym-snapped") || popupEl.classList.contains("sym-snapped-top")) {
  throw new Error("expected sym-snapped and sym-snapped-top classes to be removed after unsnap");
}
if (popupEl.style.width !== "") {
  throw new Error("expected restored width empty string for responsive CSS, got " + popupEl.style.width);
}

// Pointer up releases drag
for (const fn of (winListeners["pointerup"] || [])) {
  fn({});
}

console.log("OK");
`
	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test in TestSymbolPopupEdgeAndCornerSnap failed: %v\n%s", err, string(out))
	}
}

func TestIdentOccurrenceHighlighting(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	nodeScript := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

const Node = { ELEMENT_NODE: 1, TEXT_NODE: 3 };
const NodeFilter = { SHOW_TEXT: 4 };

function createTextNode(text) {
  const tn = {
    nodeType: Node.TEXT_NODE,
    textContent: text,
    parentElement: null,
    parentNode: null,
    nextSibling: null,
    previousSibling: null,
    splitText(offset) {
      const rest = this.textContent.slice(offset);
      this.textContent = this.textContent.slice(0, offset);
      const newNode = createTextNode(rest);
      newNode.parentElement = this.parentElement;
      newNode.parentNode = this.parentNode;
      if (this.parentNode && this.parentNode.childNodes) {
        const idx = this.parentNode.childNodes.indexOf(this);
        this.parentNode.childNodes.splice(idx + 1, 0, newNode);
      }
      return newNode;
    }
  };
  return tn;
}

function createElement(tag) {
  const el = {
    nodeType: Node.ELEMENT_NODE,
    tagName: tag.toUpperCase(),
    className: "",
    style: {},
    attributes: {},
    childNodes: [],
    _rect: null,
    getBoundingClientRect() {
      return this._rect || { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 };
    },
    get children() { return this.childNodes.filter(n => n.nodeType === Node.ELEMENT_NODE); },
    get firstElementChild() { return this.children[0] || null; },
    get nextSibling() {
      if (!this.parentNode) return null;
      const idx = this.parentNode.childNodes.indexOf(this);
      return idx >= 0 && idx + 1 < this.parentNode.childNodes.length ? this.parentNode.childNodes[idx + 1] : null;
    },
    get previousSibling() {
      if (!this.parentNode) return null;
      const idx = this.parentNode.childNodes.indexOf(this);
      return idx > 0 ? this.parentNode.childNodes[idx - 1] : null;
    },
    parentElement: null,
    parentNode: null,
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    removeAttribute(k) { delete this.attributes[k]; },
    classList: {
      add: (cls) => {
        const set = new Set((el.className || "").split(/\s+/).filter(Boolean));
        set.add(cls);
        el.className = Array.from(set).join(" ");
      },
      remove: (cls) => {
        const set = new Set((el.className || "").split(/\s+/).filter(Boolean));
        set.delete(cls);
        el.className = Array.from(set).join(" ");
      },
      contains: (cls) => (el.className || "").split(/\s+/).includes(cls)
    },
    get textContent() {
      return this.childNodes.map(n => n.textContent).join("");
    },
    set textContent(val) {
      this.childNodes = [createTextNode(val)];
      this.childNodes[0].parentNode = this;
      this.childNodes[0].parentElement = this;
    },
    appendChild(node) {
      node.parentNode = this;
      node.parentElement = this;
      this.childNodes.push(node);
      return node;
    },
    replaceChild(newChild, oldChild) {
      const idx = this.childNodes.indexOf(oldChild);
      if (idx !== -1) {
        newChild.parentNode = this;
        newChild.parentElement = this;
        oldChild.parentNode = null;
        oldChild.parentElement = null;
        this.childNodes[idx] = newChild;
      }
      return oldChild;
    },
    normalize() {
      const newNodes = [];
      let pendingText = null;
      for (const n of this.childNodes) {
        if (n.nodeType === Node.TEXT_NODE) {
          if (pendingText) {
            pendingText.textContent += n.textContent;
          } else {
            pendingText = createTextNode(n.textContent);
            pendingText.parentNode = this;
            pendingText.parentElement = this;
            newNodes.push(pendingText);
          }
        } else {
          pendingText = null;
          newNodes.push(n);
          if (n.normalize) n.normalize();
        }
      }
      this.childNodes = newNodes;
    },
    closest(selector) {
      let cur = this;
      while (cur) {
        if (selector.startsWith(".") && cur.classList.contains(selector.slice(1))) return cur;
        if (selector.startsWith("td.") && cur.tagName === "TD" && cur.classList.contains(selector.slice(3))) return cur;
        if (selector === "td.line-code" && cur.tagName === "TD" && cur.classList.contains("line-code")) return cur;
        if (selector.includes(",")) {
          const parts = selector.split(",").map(s => s.trim());
          for (const p of parts) {
            const found = cur.closest(p);
            if (found) return found;
          }
        }
        cur = cur.parentElement;
      }
      return null;
    },
    querySelectorAll(selector) {
      const res = [];
      const parts = selector.split(",").map(s => s.trim());
      function match(c) {
        for (const p of parts) {
          if (p.startsWith(".") && c.classList.contains(p.slice(1))) return true;
          if (p.includes(".")) {
            const [tag, cls] = p.split(".");
            if (c.tagName === tag.toUpperCase() && c.classList.contains(cls)) return true;
          } else if (c.tagName === p.toUpperCase()) {
            return true;
          }
        }
        return false;
      }
      function walk(n) {
        for (const c of n.childNodes) {
          if (c.nodeType === Node.ELEMENT_NODE) {
            if (match(c)) res.push(c);
            walk(c);
          }
        }
      }
      walk(this);
      return res;
    },
    querySelector(selector) {
      return this.querySelectorAll(selector)[0] || null;
    }
  };
  return el;
}

const doc = createElement("DOCUMENT");
doc.body = createElement("BODY");
doc.body.parentNode = doc;
doc.documentElement = createElement("HTML");
doc.createElement = createElement;
doc.createTextNode = createTextNode;
doc.getElementById = () => null;
doc.createTreeWalker = function(root, filter) {
  const nodes = [];
  function walk(n) {
    if (n.nodeType === Node.TEXT_NODE) nodes.push(n);
    if (n.childNodes) {
      for (const c of n.childNodes) walk(c);
    }
  }
  walk(root);
  return {
    currentNode: root,
    nextNode() {
      let idx = nodes.indexOf(this.currentNode);
      if (idx === -1) {
        for (let i = 0; i < nodes.length; i++) {
          if (nodes[i] !== this.currentNode) { idx = i - 1; break; }
        }
      }
      if (idx + 1 < nodes.length) {
        this.currentNode = nodes[idx + 1];
        return this.currentNode;
      }
      return null;
    },
    previousNode() {
      let idx = nodes.indexOf(this.currentNode);
      if (idx > 0) {
        this.currentNode = nodes[idx - 1];
        return this.currentNode;
      }
      return null;
    }
  };
};

doc.querySelectorAll = function(selector) { return doc.body.querySelectorAll(selector); };
doc.querySelector = function(selector) { return doc.body.querySelector(selector); };
doc.listeners = {};
doc.addEventListener = function(evt, fn) {
  (this.listeners[evt] = this.listeners[evt] || []).push(fn);
};
doc.removeEventListener = function(evt, fn) {
  if (this.listeners[evt]) this.listeners[evt] = this.listeners[evt].filter(f => f !== fn);
};

let currentSel = null;
const winListeners = {};
const win = {
  Node: Node,
  NodeFilter: NodeFilter,
  getSelection: () => currentSel,
  addEventListener: (evt, fn) => { (winListeners[evt] = winListeners[evt] || []).push(fn); },
  removeEventListener: (evt, fn) => {
    if (winListeners[evt]) winListeners[evt] = winListeners[evt].filter(f => f !== fn);
  },
  requestAnimationFrame: (cb) => { cb(); },
  cancelAnimationFrame: () => {},
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  URLSearchParams: URLSearchParams,
  location: { search: "" },
  openSymbolPopups: []
};
win.window = win;
win.document = doc;
win.global = win;
win.self = win;

const ctx = vm.createContext(win);
vm.runInContext(code, ctx);

(async () => {
  const sec = createElement("DIV");
  sec.className = "diff-file-section";
  doc.body.appendChild(sec);

  // Left cell: "func Serve(r *Server) { return r }"
  const tdL = createElement("TD");
  tdL.className = "line-code";
  tdL._rect = { left: 0, top: 100, right: 800, bottom: 120, width: 800, height: 20 };
  sec.appendChild(tdL);

  const kw1 = createElement("SPAN"); kw1.className = "tok-kw"; kw1.textContent = "func";
  kw1._rect = { left: 0, top: 100, right: 40, bottom: 120, width: 40, height: 20 };
  const sp1 = createTextNode(" ");
  const idServeL = createElement("SPAN"); idServeL.className = "tok-id"; idServeL.textContent = "Serve";
  idServeL._rect = { left: 45, top: 100, right: 90, bottom: 120, width: 45, height: 20 };
  const sp2 = createTextNode("(");
  const idRL = createElement("SPAN"); idRL.className = "tok-id"; idRL.textContent = "r";
  idRL._rect = { left: 95, top: 100, right: 105, bottom: 120, width: 10, height: 20 };
  const sp3 = createTextNode(" *");
  const typServerL = createElement("SPAN"); typServerL.className = "tok-typ"; typServerL.textContent = "Server";
  typServerL._rect = { left: 115, top: 100, right: 165, bottom: 120, width: 50, height: 20 };
  const sp4 = createTextNode(") { ");
  const kwRetL = createElement("SPAN"); kwRetL.className = "tok-kw"; kwRetL.textContent = "return";
  kwRetL._rect = { left: 180, top: 100, right: 230, bottom: 120, width: 50, height: 20 };
  const sp5 = createTextNode(" ");
  const idRetRL = createElement("SPAN"); idRetRL.className = "tok-id"; idRetRL.textContent = "r";
  idRetRL._rect = { left: 235, top: 100, right: 245, bottom: 120, width: 10, height: 20 };
  const sp6 = createTextNode(" }");
  tdL.appendChild(kw1); tdL.appendChild(sp1); tdL.appendChild(idServeL); tdL.appendChild(sp2);
  tdL.appendChild(idRL); tdL.appendChild(sp3); tdL.appendChild(typServerL); tdL.appendChild(sp4);
  tdL.appendChild(kwRetL); tdL.appendChild(sp5); tdL.appendChild(idRetRL); tdL.appendChild(sp6);

  // Right cell: "func Serve(r *Server)" + string + comment
  const tdR = createElement("TD");
  tdR.className = "line-code";
  tdR._rect = { left: 0, top: 120, right: 800, bottom: 140, width: 800, height: 20 };
  sec.appendChild(tdR);

  const idServeR = createElement("SPAN"); idServeR.className = "tok-id"; idServeR.textContent = "Serve";
  idServeR._rect = { left: 0, top: 120, right: 45, bottom: 140, width: 45, height: 20 };
  const spR2 = createTextNode("(");
  const idRR = createElement("SPAN"); idRR.className = "tok-id"; idRR.textContent = "r";
  idRR._rect = { left: 50, top: 120, right: 60, bottom: 140, width: 10, height: 20 };
  const spR3 = createTextNode(" *");
  const typServerR = createElement("SPAN"); typServerR.className = "tok-typ"; typServerR.textContent = "Server";
  typServerR._rect = { left: 70, top: 120, right: 120, bottom: 140, width: 50, height: 20 };
  const spR4 = createTextNode(") ");
  const strR = createElement("SPAN"); strR.className = "tok-str"; strR.textContent = "\"Server\"";
  strR._rect = { left: 125, top: 120, right: 175, bottom: 140, width: 50, height: 20 };
  const comR = createElement("SPAN"); comR.className = "tok-com"; comR.textContent = "// Server helper";
  comR._rect = { left: 180, top: 120, right: 280, bottom: 140, width: 100, height: 20 };
  tdR.appendChild(idServeR); tdR.appendChild(spR2); tdR.appendChild(idRR); tdR.appendChild(spR3);
  tdR.appendChild(typServerR); tdR.appendChild(spR4); tdR.appendChild(strR); tdR.appendChild(comR);

  // Symbol popup with signature
  const popupEl = createElement("DIV");
  popupEl.className = "symbol-popup";
  doc.body.appendChild(popupEl);
  const popSig = createElement("DIV");
  popSig.className = "sym-popup-sig";
  popupEl.appendChild(popSig);
  const popTypServer = createElement("SPAN");
  popTypServer.className = "tok-typ";
  popTypServer.textContent = "Server";
  popTypServer._rect = { left: 10, top: 200, right: 60, bottom: 220, width: 50, height: 20 };
  popSig.appendChild(popTypServer);
  win.openSymbolPopups.push({ el: popupEl });

  // Commit message box
  const commitBox = createElement("PRE");
  commitBox.className = "commit-msg-box";
  commitBox.textContent = "Serve starts the Server.";
  commitBox._rect = { left: 0, top: 300, right: 600, bottom: 340, width: 600, height: 40 };
  doc.body.appendChild(commitBox);

  // 1. Hovering identifier token "Server" at (120, 110)
  win.updateIdentOccurrencesAt(typServerL, 120, 110);
  if (!typServerL.classList.contains("ident-occ-hl")) throw new Error("typServerL missing ident-occ-hl on hover");
  if (!typServerR.classList.contains("ident-occ-hl")) throw new Error("typServerR missing ident-occ-hl on hover");
  if (!popTypServer.classList.contains("ident-occ-hl")) throw new Error("popTypServer in popup missing ident-occ-hl on hover");
  const marks = commitBox.querySelectorAll("mark.ident-occ-mark");
  if (marks.length === 0 || marks[0].textContent !== "Server") {
    throw new Error("commit-msg-box missing ident-occ-mark for Server");
  }

  // 2. Hovering blank padding space to the right of code at (600, 110) -> clears occurrences
  win.updateIdentOccurrencesAt(tdL, 600, 110);
  if (typServerL.classList.contains("ident-occ-hl") || typServerR.classList.contains("ident-occ-hl")) {
    throw new Error("occurrences not cleared when hovering blank code cell padding");
  }

  // 3. Hovering keyword "func" or "return" -> does NOT highlight anything
  win.updateIdentOccurrencesAt(kw1, 20, 110);
  if (kw1.classList.contains("ident-occ-hl") || typServerL.classList.contains("ident-occ-hl")) {
    throw new Error("hovering keyword func incorrectly triggered occurrence highlight");
  }
  win.updateIdentOccurrencesAt(kwRetL, 190, 110);
  if (kwRetL.classList.contains("ident-occ-hl")) {
    throw new Error("hovering keyword return incorrectly triggered occurrence highlight");
  }

  // 4. Hovering string or comment -> does NOT highlight anything
  win.updateIdentOccurrencesAt(strR, 140, 130);
  if (strR.classList.contains("ident-occ-hl")) {
    throw new Error("hovering string incorrectly triggered occurrence highlight");
  }
  win.updateIdentOccurrencesAt(comR, 200, 130);
  if (comR.classList.contains("ident-occ-hl")) {
    throw new Error("hovering comment incorrectly triggered occurrence highlight");
  }

  // 5. 1-char identifier "r" -> highlights only "r", NOT "return"
  win.updateIdentOccurrencesAt(idRL, 100, 110);
  if (!idRL.classList.contains("ident-occ-hl")) throw new Error("idRL missing ident-occ-hl");
  if (!idRetRL.classList.contains("ident-occ-hl")) throw new Error("idRetRL missing ident-occ-hl");
  if (!idRR.classList.contains("ident-occ-hl")) throw new Error("idRR missing ident-occ-hl");
  if (kwRetL.classList.contains("ident-occ-hl")) throw new Error("return keyword incorrectly highlighted for r!");

  // 6. Leaving document clears occurrences
  const mouseLeaveHandlers = doc.listeners["mouseleave"] || [];
  for (const fn of mouseLeaveHandlers) fn({});
  if (idRL.classList.contains("ident-occ-hl") || idRetRL.classList.contains("ident-occ-hl")) {
    throw new Error("ident-occ-hl not cleared after document mouseleave");
  }

  // 7. Debounced pointer events (120ms hover intent delay)
  const hoverDelay = win.IDENT_HOVER_DELAY_MS || 120;
  if (hoverDelay !== 120) throw new Error("expected IDENT_HOVER_DELAY_MS to be 120, got " + hoverDelay);
  const pointerMoveHandlers = doc.listeners["pointermove"] || [];
  for (const fn of pointerMoveHandlers) fn({ target: idServeL, clientX: 60, clientY: 110, buttons: 0 });
  await new Promise(r => setTimeout(r, 40));
  if (idServeL.classList.contains("ident-occ-hl")) {
    throw new Error("ident-occ-hl triggered too early before 120ms hover intent delay");
  }
  await new Promise(r => setTimeout(r, hoverDelay + 40));
  if (!idServeL.classList.contains("ident-occ-hl") || !idServeR.classList.contains("ident-occ-hl")) {
    throw new Error("debounced pointermove failed to highlight Serve occurrences");
  }

  // Moving pointer outside code containers immediately clears without debounce delay
  for (const fn of pointerMoveHandlers) fn({ target: doc.body, clientX: 0, clientY: 0, buttons: 0 });
  if (idServeL.classList.contains("ident-occ-hl") || idServeR.classList.contains("ident-occ-hl")) {
    throw new Error("pointermove outside code container failed to clear occurrences immediately");
  }

  // 8. Active drag / selection (buttons !== 0) ignores hover
  for (const fn of pointerMoveHandlers) fn({ target: typServerL, clientX: 120, clientY: 110, buttons: 1 });
  await new Promise(r => setTimeout(r, hoverDelay + 40));
  if (typServerL.classList.contains("ident-occ-hl")) {
    throw new Error("pointermove while buttons !== 0 should ignore hover occurrences");
  }

  // 9. refreshIdentOccurrences / updateIdentOccurrences re-applies to dynamically added cells
  win.updateIdentOccurrencesAt(typServerL, 120, 110);
  const tdNew = createElement("TD");
  tdNew.className = "line-code";
  const typServerNew = createElement("SPAN");
  typServerNew.className = "tok-typ";
  typServerNew.textContent = "Server";
  tdNew.appendChild(typServerNew);
  sec.appendChild(tdNew);
  win.updateIdentOccurrences();
  if (!typServerNew.classList.contains("ident-occ-hl")) {
    throw new Error("refreshIdentOccurrences failed to highlight dynamically added cell");
  }

  console.log("OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test in TestIdentOccurrenceHighlighting failed: %v\n%s", err, string(out))
	}
}

func TestAPIPublishInPlaceResponse(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := vcs.Open(repoDir, srv.cache)
	changes, _ := repo.Changes()
	c := changes[0]

	// 1. Publish when no drafts exist -> published == 0, sig returned
	payload0, _ := json.Marshal(map[string]any{
		"repo":   rec.Slug,
		"change": c.Key,
	})
	resp, err := http.Post(ts.URL+"/api/publish", "application/json", bytes.NewReader(payload0))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/publish failed: status=%v err=%v", resp.StatusCode, err)
	}
	var res0 struct {
		Published int64  `json:"published"`
		Sig       string `json:"sig"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res0); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if res0.Published != 0 || res0.Sig == "" {
		t.Errorf("expected published=0 and non-empty sig, got %+v", res0)
	}

	// 2. Add draft comment
	th, _, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "hello.go", "R", 1, 1, "anchor", "author", "my draft", true)
	if err != nil {
		t.Fatal(err)
	}

	// 3. Publish with drafts -> published > 0, sig, change_counts, file_counts, thread_html
	payload1, _ := json.Marshal(map[string]any{
		"repo":      rec.Slug,
		"change":    c.Key,
		"left_rev":  c.Parent,
		"right_rev": c.Rev,
	})
	resp, err = http.Post(ts.URL+"/api/publish", "application/json", bytes.NewReader(payload1))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/publish failed: status=%v err=%v", resp.StatusCode, err)
	}
	var res1 struct {
		Published    int64                           `json:"published"`
		Sig          string                          `json:"sig"`
		ChangeCounts engine.CommentCounts            `json:"change_counts"`
		FileCounts   map[string]engine.CommentCounts `json:"file_counts"`
		ThreadHTML   map[string]string               `json:"thread_html"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res1); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if res1.Published != 1 {
		t.Errorf("expected published=1, got %d", res1.Published)
	}
	if res1.Sig == "" {
		t.Errorf("expected non-empty sig in publish response")
	}
	if res1.ChangeCounts.Unresolved != 1 || res1.ChangeCounts.Draft != 0 {
		t.Errorf("unexpected ChangeCounts: %+v", res1.ChangeCounts)
	}
	if res1.FileCounts["hello.go"].Unresolved != 1 {
		t.Errorf("unexpected FileCounts for hello.go: %+v", res1.FileCounts)
	}
	thKey := strconv.FormatInt(th.ID, 10)
	if html, ok := res1.ThreadHTML[thKey]; !ok || !strings.Contains(html, "my draft") {
		t.Errorf("expected ThreadHTML[%s] to contain 'my draft', got %q", thKey, html)
	}

	// 4. Verify auto-snapshot created Snapshot 1 and publishing again does NOT take a redundant snapshot.
	snaps, err := d.Snapshots(rec.ID, c.Key)
	if err != nil || len(snaps) != 1 || snaps[0].Rev != c.Rev {
		t.Fatalf("expected auto-snapshot 1 for %s, got %+v (err=%v)", c.Rev, snaps, err)
	}
	thUpdated, err := d.ThreadByID(th.ID)
	if err != nil || thUpdated.SnapshotNum != 1 {
		t.Fatalf("expected thread promoted to snapshot 1, got %+v (err=%v)", thUpdated, err)
	}

	// Empty publish takes no redundant snapshot
	resp2, err := http.Post(ts.URL+"/api/publish", "application/json", bytes.NewReader(payload1))
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("second POST /api/publish failed: %v", err)
	}
	var res2 struct {
		Published       int64 `json:"published"`
		SnapshotCreated bool  `json:"snapshot_created"`
		SnapshotPending bool  `json:"snapshot_pending"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&res2)
	_ = resp2.Body.Close()
	if res2.Published != 0 || res2.SnapshotCreated || res2.SnapshotPending {
		t.Errorf("unexpected second publish result: %+v", res2)
	}

	// New draft on already-snapshotted c.Rev publishes without creating a redundant Snapshot 2
	thSameRev, _, err := engine.AddCommentRange(d, repo, rec.ID, c, c.Rev, "hello.go", "R", 3, 3, 0, 4, "author", "second draft same rev", true, false)
	if err != nil {
		t.Fatal(err)
	}
	resp3, err := http.Post(ts.URL+"/api/publish", "application/json", bytes.NewReader(payload1))
	if err != nil || resp3.StatusCode != http.StatusOK {
		t.Fatalf("third POST /api/publish failed: %v", err)
	}
	var res3 struct {
		Published       int64 `json:"published"`
		SnapshotCreated bool  `json:"snapshot_created"`
		SnapshotNum     int   `json:"snapshot_num"`
		SnapshotPending bool  `json:"snapshot_pending"`
	}
	_ = json.NewDecoder(resp3.Body).Decode(&res3)
	_ = resp3.Body.Close()
	if res3.Published != 1 || res3.SnapshotCreated || res3.SnapshotNum != 0 || res3.SnapshotPending {
		t.Errorf("expected no redundant snapshot on same rev, got %+v", res3)
	}
	if thSameRev.SnapshotNum != 1 {
		t.Errorf("expected AddCommentRange on c.Rev to assign SnapshotNum=1, got %d", thSameRev.SnapshotNum)
	}
	snapsAfter3, _ := d.Snapshots(rec.ID, c.Key)
	if len(snapsAfter3) != 1 {
		t.Errorf("expected still 1 snapshot, got %d", len(snapsAfter3))
	}

	// Non-canonical changeKey ("HEAD") is canonicalized to c.Key inside getCommentCountsForChangeView
	// both when c is passed directly and when resolved internally.
	if fc, cc, err := srv.getCommentCountsForChangeView(repo, c, snapsAfter3, true, rec.ID, "HEAD", "hello.go", c.Parent, c.Rev); err != nil || fc.Unresolved != 2 || cc.Unresolved != 2 {
		t.Errorf("getCommentCountsForChangeView(c!=nil, HEAD) = (%+v, %+v, %v), want 2/2", fc, cc, err)
	}
	if fc, cc, err := srv.getCommentCountsForChangeView(repo, nil, nil, false, rec.ID, "HEAD", "hello.go", c.Parent, c.Rev); err != nil || fc.Unresolved != 2 || cc.Unresolved != 2 {
		t.Errorf("getCommentCountsForChangeView(c==nil, HEAD) = (%+v, %+v, %v), want 2/2", fc, cc, err)
	}

	// 5. Amend commit to create pending v2: Snapshot 1 threads hidden on parent..live,
	//    visible on Left pane in snap:1..live (A1), listed with snapshot_num & in_current_view in /api/threads/list.
	if err := os.WriteFile(filepath.Join(repoDir, "hello.go"), []byte("package main\n\nfunc main() { println(\"v2\") }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmdAmend := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
	cmdAmend.Dir = repoDir
	if out, err := cmdAmend.CombinedOutput(); err != nil {
		t.Fatalf("git commit --amend: %s (%v)", string(out), err)
	}
	srv.cache.Evict(repoDir)

	respDiffLive, err := http.Get(fmt.Sprintf("%s/r/%s/c/%s/f/hello.go?base=parent&target=live", ts.URL, rec.Slug, c.Key))
	if err != nil || respDiffLive.StatusCode != http.StatusOK {
		t.Fatalf("GET parent..live diff: %v", err)
	}
	liveBodyBytes, _ := io.ReadAll(respDiffLive.Body)
	_ = respDiffLive.Body.Close()
	liveBody := string(liveBodyBytes)
	if strings.Contains(liveBody, "my draft") || strings.Contains(liveBody, thKey+`" class="thread-card`) {
		t.Errorf("expected Snapshot 1 thread hidden inline in parent..live diff")
	}

	respChangeLive, err := http.Get(fmt.Sprintf("%s/r/%s/c/%s?diffs=all&base=parent&target=live", ts.URL, rec.Slug, c.Key))
	if err != nil || respChangeLive.StatusCode != http.StatusOK {
		t.Fatalf("GET change parent..live: %v", err)
	}
	changeLiveBytes, _ := io.ReadAll(respChangeLive.Body)
	_ = respChangeLive.Body.Close()
	changeLiveBody := string(changeLiveBytes)
	if !strings.Contains(changeLiveBody, `>2 unresolved</span>`) {
		t.Errorf("expected change header unresolved badge to still count Snapshot 1 threads (2 unresolved)")
	}
	if strings.Contains(changeLiveBody, fmt.Sprintf(`id="thread-%d"`, th.ID)) {
		t.Errorf("expected Snapshot 1 thread not rendered inline in ?diffs=all parent..live")
	}
	fileRowBadgeRe := regexp.MustCompile(`data-file-counts="hello\.go">[^<]*badge-count-unresolved`)
	if fileRowBadgeRe.MatchString(changeLiveBody) {
		t.Errorf("expected hello.go per-file badge not to show Snapshot 1 threads in parent..live")
	}

	respInterdiff, err := http.Get(fmt.Sprintf("%s/r/%s/c/%s/f/hello.go?base=snap:1&target=live", ts.URL, rec.Slug, c.Key))
	if err != nil || respInterdiff.StatusCode != http.StatusOK {
		t.Fatalf("GET snap:1..live diff: %v", err)
	}
	interBytes, _ := io.ReadAll(respInterdiff.Body)
	_ = respInterdiff.Body.Close()
	interBody := string(interBytes)
	if !strings.Contains(interBody, fmt.Sprintf(`id="thread-%d"`, thSameRev.ID)) || !strings.Contains(interBody, `data-range-side="L"`) {
		t.Errorf("expected Snapshot 1 thread rendered on Left pane (data-range-side=L) in snap:1..live, body:\n%s", interBody)
	}

	respList, err := http.Get(fmt.Sprintf("%s/api/threads/list?repo=%s&change=%s&base=parent&target=live", ts.URL, rec.Slug, c.Key))
	if err != nil || respList.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/threads/list: %v", err)
	}
	var listRes struct {
		Threads []struct {
			ID            int64 `json:"id"`
			SnapshotNum   int   `json:"snapshot_num"`
			InCurrentView bool  `json:"in_current_view"`
		} `json:"threads"`
	}
	if err := json.NewDecoder(respList.Body).Decode(&listRes); err != nil {
		t.Fatal(err)
	}
	_ = respList.Body.Close()
	if len(listRes.Threads) != 2 {
		t.Fatalf("expected 2 threads across snapshots in /api/threads/list, got %+v", listRes.Threads)
	}
	for _, item := range listRes.Threads {
		if item.SnapshotNum != 1 || item.InCurrentView {
			t.Errorf("expected snapshot_num=1, in_current_view=false in parent..live list, got %+v", item)
		}
	}

	// 6. Interdiff Side=L draft on snap1.Rev flips symmetrically to Side=R when viewed in parent..snap:1
	//    (both in /api/threads/list and in /api/threads/reply HTML).
	snapsCur, _ := d.Snapshots(rec.ID, c.Key)
	snap1 := snapsCur[0]
	thInterLeft, _, err := engine.AddCommentRangeWithTarget(d, repo, rec.ID, c, snap1.Rev, snap1.Rev, "hello.go", "L", 3, 3, 0, 4, "alice", "left pane interdiff note", true, false)
	if err != nil {
		t.Fatalf("AddCommentRangeWithTarget snap1 Side=L: %v", err)
	}
	respListSnap1, err := http.Get(fmt.Sprintf("%s/api/threads/list?repo=%s&change=%s&base=parent&target=snap:1", ts.URL, rec.Slug, c.Key))
	if err != nil || respListSnap1.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/threads/list parent..snap:1: %v", err)
	}
	var listSnap1 struct {
		Threads []struct {
			ID            int64  `json:"id"`
			Side          string `json:"side"`
			InCurrentView bool   `json:"in_current_view"`
		} `json:"threads"`
	}
	if err := json.NewDecoder(respListSnap1.Body).Decode(&listSnap1); err != nil {
		t.Fatal(err)
	}
	_ = respListSnap1.Body.Close()
	var foundInter bool
	for _, it := range listSnap1.Threads {
		if it.ID == thInterLeft.ID {
			foundInter = true
			if !it.InCurrentView || it.Side != "R" {
				t.Errorf("expected interdiff Side=L thread #%d to report in_current_view=true side=R in parent..snap:1, got %+v", thInterLeft.ID, it)
			}
		}
	}
	if !foundInter {
		t.Errorf("expected thread #%d in /api/threads/list for parent..snap:1", thInterLeft.ID)
	}

	replyInterPayload, _ := json.Marshal(map[string]any{
		"thread_id": thInterLeft.ID,
		"body":      "reply in parent..snap:1",
		"left_rev":  c.Parent,
		"right_rev": snap1.Rev,
	})
	respReplyInter, err := http.Post(ts.URL+"/api/threads/reply", "application/json", bytes.NewReader(replyInterPayload))
	if err != nil || respReplyInter.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/reply parent..snap:1: %v", err)
	}
	var replyInterRes struct {
		HTML string `json:"html"`
	}
	if err := json.NewDecoder(respReplyInter.Body).Decode(&replyInterRes); err != nil {
		t.Fatal(err)
	}
	_ = respReplyInter.Body.Close()
	if !strings.Contains(replyInterRes.HTML, `data-range-side="R"`) {
		t.Errorf("expected /api/threads/reply in parent..snap:1 to render data-range-side=R, got:\n%s", replyInterRes.HTML)
	}

	// Publish the draft on snap1 cleanly before testing v2->v3 auto-snapshot hiding on live.
	_, _ = engine.PublishChange(d, repo, rec.ID, c.Key, c, true)

	// 7. Draft created on v2 (snapshot_num=0), amended to pending v3, then /api/publish from parent..live(v3):
	//    auto-snapshots v2 as Snapshot 2, excludes v2's now-hidden thread from thread_html, reports it in
	//    hidden_thread_ids, and zeroes hello.go per-file counts for v3.
	repoV2, _ := vcs.Open(repoDir, nil)
	changesV2, _ := repoV2.Changes()
	cV2 := changesV2[0]
	thV2Draft, _, err := engine.AddCommentRange(d, repoV2, rec.ID, cV2, cV2.Rev, "hello.go", "R", 3, 3, 0, 4, "alice", "v2 draft hidden on v3 publish", true, false)
	if err != nil || thV2Draft.SnapshotNum != 0 {
		t.Fatalf("AddCommentRange v2: th=%+v err=%v", thV2Draft, err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "hello.go"), []byte("package main\n\nfunc main() { println(\"v3\") }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmdAmendV3 := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
	cmdAmendV3.Dir = repoDir
	if out, err := cmdAmendV3.CombinedOutput(); err != nil {
		t.Fatalf("git commit --amend v3: %s (%v)", string(out), err)
	}
	srv.cache.Evict(repoDir)
	repoV3, _ := vcs.Open(repoDir, srv.cache)
	changesV3, _ := repoV3.Changes()
	cV3 := changesV3[0]

	pubV3Payload, _ := json.Marshal(map[string]any{
		"repo":      rec.Slug,
		"change":    cV3.Key,
		"left_rev":  cV3.Parent,
		"right_rev": cV3.Rev,
	})
	respPubV3, err := http.Post(ts.URL+"/api/publish", "application/json", bytes.NewReader(pubV3Payload))
	if err != nil || respPubV3.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/publish on v3: %v", err)
	}
	var pubV3Res struct {
		Published       int64                           `json:"published"`
		LiveRev         string                          `json:"live_rev"`
		SnapshotCreated bool                            `json:"snapshot_created"`
		SnapshotNum     int                             `json:"snapshot_num"`
		SnapshotPending bool                            `json:"snapshot_pending"`
		ThreadHTML      map[string]string               `json:"thread_html"`
		HiddenThreadIDs []int64                         `json:"hidden_thread_ids"`
		FileCounts      map[string]engine.CommentCounts `json:"file_counts"`
	}
	if err := json.NewDecoder(respPubV3.Body).Decode(&pubV3Res); err != nil {
		t.Fatal(err)
	}
	_ = respPubV3.Body.Close()
	if pubV3Res.Published != 1 || pubV3Res.LiveRev != cV3.Rev || !pubV3Res.SnapshotCreated || pubV3Res.SnapshotNum != 2 || !pubV3Res.SnapshotPending {
		t.Fatalf("unexpected v3 publish response: %+v (want live_rev=%s)", pubV3Res, cV3.Rev)
	}
	if _, renderedHidden := pubV3Res.ThreadHTML[strconv.FormatInt(thV2Draft.ID, 10)]; renderedHidden {
		t.Errorf("expected hidden Snapshot 2 thread #%d omitted from thread_html when viewing v3", thV2Draft.ID)
	}
	var sawHiddenV2 bool
	for _, id := range pubV3Res.HiddenThreadIDs {
		if id == thV2Draft.ID {
			sawHiddenV2 = true
			break
		}
	}
	if !sawHiddenV2 {
		t.Errorf("expected hidden_thread_ids to include #%d, got %+v", thV2Draft.ID, pubV3Res.HiddenThreadIDs)
	}
	if fc := pubV3Res.FileCounts["hello.go"]; fc.Unresolved != 0 || fc.Resolved != 0 || fc.Draft != 0 {
		t.Errorf("expected hello.go per-file counts zeroed on v3 after v2 snapshot, got %+v", fc)
	}
}

func TestUnchangedFileDiffSkipMoveBundleAndLazyPeek(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "other.go"), []byte("package main\nvar x = 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "other.go")
	run("commit", "-m", "Commit with other.go\n\nChange-Id: I1111111111111111111111111111111111111111")

	if err := os.WriteFile(filepath.Join(repoDir, "hello.go"), []byte("package main\n\nfunc main() {}\nvar y = 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "hello.go")
	run("commit", "-m", "Commit touching hello.go\n\nChange-Id: I2222222222222222222222222222222222222222")

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := vcs.Open(repoDir, srv.cache)
	changes, _ := repo.Changes()
	c := changes[0]

	// 1. Viewing an unchanged file in handleDiff succeeds without moveBundle error
	resp, err := http.Get(ts.URL + "/r/" + rec.Slug + "/c/" + c.Key + "/f/other.go?base=parent&target=live")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /r/.../f/other.go failed: status=%v err=%v", resp.StatusCode, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "other.go") {
		t.Errorf("expected body to contain other.go")
	}

	// 2. Peek move bundle when cached vs not cached
	if b, ok := srv.peekMoveBundle(rec.RootPath, c.Parent, c.Rev); ok || b != nil {
		t.Errorf("expected peekMoveBundle miss before caching, got %v", b)
	}

	// Pre-populate cache via getMoveBundle
	view, err := engine.BuildChangeView(d, repo, rec, c, "parent", "live")
	if err != nil {
		t.Fatal(err)
	}
	b := srv.getMoveBundle(repo, view)
	if b == nil {
		t.Fatal("getMoveBundle returned nil")
	}

	// Now peek should hit
	if b2, ok := srv.peekMoveBundle(rec.RootPath, view.BaseRev, view.TargetRev); !ok || b2 != b {
		t.Errorf("expected peekMoveBundle hit, got ok=%v, b2=%v", ok, b2)
	}

	// 3. Lazy diff file endpoint hits peekMoveBundle and returns diff HTML
	lazyURL := fmt.Sprintf("%s/api/diff/file?repo=%s&change=%s&path=hello.go&index=0&base=parent&target=live&mode=split",
		ts.URL, rec.Slug, c.Key)
	resp, err = http.Get(lazyURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/diff/file failed: status=%v err=%v", resp.StatusCode, err)
	}
	lazyHTML, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(lazyHTML), "diff-table") {
		t.Errorf("expected lazy diff HTML to contain diff-table, got %s", string(lazyHTML))
	}
}

func TestAPIThreadsList(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nfunc main() {\n\tprintln(1)\n\tprintln(2)\n\tprintln(3)\n\tprintln(4)\n\tprintln(5)\n\tprintln(6)\n\tprintln(7)\n\tprintln(8)\n\tprintln(9)\n\tprintln(10)\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "util.go"), []byte("package pkg\nfunc Util() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "main.go", "pkg/util.go")
	run("commit", "-m", "Commit 1\n\nChange-Id: I1111111111111111111111111111111111111111")

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := vcs.Open(repoDir, srv.cache)
	changes, _ := repo.Changes()
	c := changes[0]

	// Add threads:
	// 1. COMMIT_MSG unresolved
	th1, _, _ := d.AddThread(rec.ID, c.Key, c.Rev, 0, vcs.CommitMsgPath, "R", 0, 0, "", "Alice", "Fix the commit message typo", false)
	_ = th1

	// 2. main.go:L10 unresolved, with draft message
	th2, _, _ := d.AddThread(rec.ID, c.Key, c.Rev, 0, "/main.go", "R", 10, 10, "", "Bob", "Check nil ptr here", false)
	_, _ = d.AddMessage(th2.ID, "Charlie", "I agree", true) // Draft

	// 3. main.go:L5 unresolved
	th3, _, _ := d.AddThread(rec.ID, c.Key, c.Rev, 0, "main.go", "R", 5, 5, "", "Dave", "Refactor this loop", false)
	_ = th3

	// 4. main.go:L12 resolved (should be excluded)
	th4, _, _ := d.AddThread(rec.ID, c.Key, c.Rev, 0, "main.go", "R", 12, 12, "", "Eve", "Resolved issue", false)
	_ = d.SetResolved(th4.ID, true)

	// 5. pkg/util.go:L1 unresolved
	th5, _, _ := d.AddThread(rec.ID, c.Key, c.Rev, 0, "pkg/util.go", "R", 1, 1, "", "Frank", "Add package doc", false)
	_ = th5

	// Test GET /api/threads/list
	url := fmt.Sprintf("%s/api/threads/list?repo=%s&change=%s&base=parent&target=live", ts.URL, rec.Slug, c.Key)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/threads/list failed: status=%v err=%v", resp.StatusCode, err)
	}
	defer resp.Body.Close()

	var result struct {
		Threads []struct {
			ID          int64  `json:"id"`
			Path        string `json:"path"`
			Side        string `json:"side"`
			Line        int    `json:"line"`
			EndLine     int    `json:"end_line"`
			Stale       bool   `json:"stale"`
			HasDraft    bool   `json:"has_draft"`
			Author      string `json:"author"`
			Preview     string `json:"preview"`
			PreviewHTML string `json:"preview_html"`
		} `json:"threads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(result.Threads) != 4 {
		t.Fatalf("expected 4 unresolved threads (excluding resolved), got %d: %+v", len(result.Threads), result.Threads)
	}

	// Ordering: COMMIT_MSG first, then main.go (line 5, line 10), then pkg/util.go (line 1)
	if result.Threads[0].Path != "COMMIT_MSG" || result.Threads[0].Author != "Alice" || result.Threads[0].Preview != "Fix the commit message typo" || result.Threads[0].HasDraft {
		t.Errorf("unexpected thread 0: %+v", result.Threads[0])
	}
	if !strings.Contains(result.Threads[0].PreviewHTML, "Fix the commit message typo") {
		t.Errorf("expected preview_html on thread 0 to contain rendered markdown, got: %q", result.Threads[0].PreviewHTML)
	}
	if result.Threads[1].Path != "main.go" || result.Threads[1].Line != 5 || result.Threads[1].Author != "Dave" {
		t.Errorf("unexpected thread 1: %+v", result.Threads[1])
	}
	if result.Threads[2].Path != "main.go" || result.Threads[2].Line != 10 || result.Threads[2].Author != "Bob" || !result.Threads[2].HasDraft {
		t.Errorf("unexpected thread 2: %+v", result.Threads[2])
	}
	if result.Threads[3].Path != "pkg/util.go" || result.Threads[3].Line != 1 || result.Threads[3].Author != "Frank" {
		t.Errorf("unexpected thread 3: %+v", result.Threads[3])
	}

	// Test 404 for unknown repo
	badURL := fmt.Sprintf("%s/api/threads/list?repo=nonexistent&change=%s", ts.URL, c.Key)
	resp2, err := http.Get(badURL)
	if err != nil || resp2.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent repo, got %v", resp2.StatusCode)
	}
}

func TestAPIFormDrafts(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)
	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// 1. Initial GET returns empty drafts
	getURL := fmt.Sprintf("%s/api/form-drafts?repo=%s&change=%s", ts.URL, rec.Slug, c.Key)
	resp, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("GET /api/form-drafts failed: %v", err)
	}
	defer resp.Body.Close()

	var getRes struct {
		Drafts []struct {
			DraftKey string `json:"draft_key"`
			Content  string `json:"content"`
		} `json:"drafts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(getRes.Drafts) != 0 {
		t.Fatalf("expected 0 drafts initially, got %d", len(getRes.Drafts))
	}

	// 2. POST save draft
	postURL := fmt.Sprintf("%s/api/form-drafts", ts.URL)
	unresFalse := false
	savePayload := map[string]any{
		"repo":        rec.Slug,
		"change":      c.Key,
		"action":      "save",
		"draft_key":   "new:main.go:R:15",
		"target_path": "main.go",
		"side":        "R",
		"line":        15,
		"end_line":    25,
		"start_col":   3,
		"end_col":     10,
		"unresolved":  unresFalse,
		"content":     "My unsaved draft",
	}
	b, _ := json.Marshal(savePayload)
	respPost, err := http.Post(postURL, "application/json", bytes.NewReader(b))
	if err != nil || respPost.StatusCode != http.StatusOK {
		t.Fatalf("POST save failed: status=%v, err=%v", respPost.StatusCode, err)
	}
	respPost.Body.Close()

	// 3. GET returns saved draft
	resp2, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	var getRes2 struct {
		Drafts []struct {
			DraftKey   string `json:"draft_key"`
			TargetPath string `json:"target_path"`
			Side       string `json:"side"`
			Line       int    `json:"line"`
			EndLine    int    `json:"end_line"`
			StartCol   int    `json:"start_col"`
			EndCol     int    `json:"end_col"`
			Unresolved bool   `json:"unresolved"`
			Content    string `json:"content"`
		} `json:"drafts"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&getRes2); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	resp2.Body.Close()
	if len(getRes2.Drafts) != 1 || getRes2.Drafts[0].Content != "My unsaved draft" || getRes2.Drafts[0].Line != 15 || getRes2.Drafts[0].EndLine != 25 || getRes2.Drafts[0].StartCol != 3 || getRes2.Drafts[0].EndCol != 10 || getRes2.Drafts[0].Unresolved != false {
		t.Fatalf("unexpected drafts: %+v", getRes2.Drafts)
	}

	// 4. POST delete draft
	delPayload := map[string]any{
		"repo":      rec.Slug,
		"change":    c.Key,
		"action":    "delete",
		"draft_key": "new:main.go:R:15",
	}
	bDel, _ := json.Marshal(delPayload)
	respDel, err := http.Post(postURL, "application/json", bytes.NewReader(bDel))
	if err != nil || respDel.StatusCode != http.StatusOK {
		t.Fatalf("POST delete failed: status=%v, err=%v", respDel.StatusCode, err)
	}
	respDel.Body.Close()

	// 5. GET confirms deleted
	resp3, _ := http.Get(getURL)
	var getRes3 struct {
		Drafts []any `json:"drafts"`
	}
	_ = json.NewDecoder(resp3.Body).Decode(&getRes3)
	resp3.Body.Close()
	if len(getRes3.Drafts) != 0 {
		t.Fatalf("expected 0 drafts after delete, got %d", len(getRes3.Drafts))
	}
}

func TestThreadDeleteCleansUpEditDraft(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, _ := d.EnsureRepo(repoDir)
	repo, _ := vcs.Open(repoDir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// Add thread with 1 message
	pAdd, _ := json.Marshal(map[string]any{
		"repo":       rec.Slug,
		"change":     c.Key,
		"path":       "main.go",
		"side":       "R",
		"start_line": 10,
		"end_line":   10,
		"start_col":  0,
		"end_col":    0,
		"body":       "Initial message",
		"unresolved": true,
	})
	resp, err := http.Post(ts.URL+"/api/threads/add", "application/json", bytes.NewReader(pAdd))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/add failed: %v", err)
	}
	var addRes struct {
		Thread struct {
			ID int64 `json:"id"`
		} `json:"thread"`
		Message struct {
			ID int64 `json:"id"`
		} `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&addRes)
	resp.Body.Close()

	// Save an edit form draft for that message
	msgID := addRes.Message.ID
	editDraftKey := fmt.Sprintf("review_draft:edit:%s:%s:%d", rec.Slug, c.Key, msgID)
	savePayload := map[string]any{
		"repo":       rec.Slug,
		"change":     c.Key,
		"action":     "save",
		"draft_key":  editDraftKey,
		"message_id": strconv.FormatInt(msgID, 10),
		"content":    "In-progress edit text",
	}
	bSave, _ := json.Marshal(savePayload)
	respSave, err := http.Post(ts.URL+"/api/form-drafts", "application/json", bytes.NewReader(bSave))
	if err != nil || respSave.StatusCode != http.StatusOK {
		t.Fatalf("POST save edit draft failed: %v", err)
	}
	respSave.Body.Close()

	// Verify draft exists
	drafts, err := d.FormDraftsForChange(rec.ID, c.Key)
	if err != nil || len(drafts) != 1 {
		t.Fatalf("expected 1 draft saved, got %d, err=%v", len(drafts), err)
	}

	// Delete the draft message (which is the only message, so thread is also deleted)
	pDel, _ := json.Marshal(map[string]any{
		"message_id": msgID,
	})
	respDel, err := http.Post(ts.URL+"/api/threads/delete", "application/json", bytes.NewReader(pDel))
	if err != nil || respDel.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/threads/delete failed: %v", err)
	}
	respDel.Body.Close()

	// Verify the edit draft was cleaned up from SQLite!
	draftsAfter, err := d.FormDraftsForChange(rec.ID, c.Key)
	if err != nil || len(draftsAfter) != 0 {
		t.Fatalf("expected 0 drafts remaining after thread delete, got %d", len(draftsAfter))
	}
}

func TestUnsavedDraftsAndUnresolvedThreadsUI(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	nodeScript := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

const Node = { ELEMENT_NODE: 1, TEXT_NODE: 3 };

function createTextNode(text) {
  return {
    nodeType: Node.TEXT_NODE,
    textContent: text,
    parentElement: null,
    parentNode: null
  };
}

function parseAttrs(el, raw) {
  const attrRe = /([a-z0-9_-]+)(?:=(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?/gi;
  let m;
  while ((m = attrRe.exec(raw)) !== null) {
    const k = m[1];
    const v = m[2] !== undefined ? m[2] : (m[3] !== undefined ? m[3] : (m[4] !== undefined ? m[4] : ""));
    if (k === "id") el.id = v;
    else if (k === "class") el.className = v;
    else if (k === "checked") el.checked = true;
    else el.setAttribute(k, v);
  }
}

function parseHTMLInto(parent, html) {
  if (!html) return;
  const tagRe = /<([a-z0-9]+)([^>]*)>([\s\S]*?)<\/\1>|<([a-z0-9]+)([^>]*)\/?>|([^<]+)/gi;
  let match;
  while ((match = tagRe.exec(html)) !== null) {
    if (match[1]) {
      const el = createElement(match[1]);
      parseAttrs(el, match[2]);
      parseHTMLInto(el, match[3]);
      parent.appendChild(el);
    } else if (match[4]) {
      const el = createElement(match[4]);
      parseAttrs(el, match[5]);
      parent.appendChild(el);
    } else if (match[6]) {
      const text = match[6].trim();
      if (text) parent.appendChild(createTextNode(text));
    }
  }
}

function createElement(tag) {
  const el = {
    nodeType: Node.ELEMENT_NODE,
    tagName: tag.toUpperCase(),
    className: "",
    style: {},
    attributes: {},
    childNodes: [],
    value: "",
    checked: false,
    offsetWidth: 100,
    offsetHeight: 30,
    get children() { return this.childNodes.filter(n => n.nodeType === Node.ELEMENT_NODE); },
    parentElement: null,
    parentNode: null,
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    removeAttribute(k) { delete this.attributes[k]; },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    classList: {
      add: (cls) => {
        const set = new Set((el.className || "").split(/\s+/).filter(Boolean));
        set.add(cls);
        el.className = Array.from(set).join(" ");
      },
      remove: (cls) => {
        const set = new Set((el.className || "").split(/\s+/).filter(Boolean));
        set.delete(cls);
        el.className = Array.from(set).join(" ");
      },
      contains: (cls) => (el.className || "").split(/\s+/).includes(cls)
    },
    get textContent() {
      return this.childNodes.map(n => n.textContent).join("");
    },
    set textContent(val) {
      this.childNodes = [createTextNode(val)];
      this.childNodes[0].parentNode = this;
      this.childNodes[0].parentElement = this;
    },
    set innerHTML(html) {
      this.childNodes = [];
      parseHTMLInto(this, html);
    },
    get innerHTML() {
      return this.childNodes.map(n => n.textContent).join("");
    },
    appendChild(node) {
      node.parentNode = this;
      node.parentElement = this;
      this.childNodes.push(node);
      return node;
    },
    insertBefore(newChild, refChild) {
      const idx = this.childNodes.indexOf(refChild);
      newChild.parentNode = this;
      newChild.parentElement = this;
      if (idx !== -1) {
        this.childNodes.splice(idx, 0, newChild);
      } else {
        this.childNodes.push(newChild);
      }
      return newChild;
    },
    removeChild(node) {
      const idx = this.childNodes.indexOf(node);
      if (idx !== -1) {
        this.childNodes.splice(idx, 1);
        node.parentNode = null;
        node.parentElement = null;
      }
      return node;
    },
    remove() {
      if (this.parentNode && this.parentNode.removeChild) {
        this.parentNode.removeChild(this);
      }
    },
    closest(sel) {
      let cur = this;
      while (cur) {
        if (sel.startsWith(".") && cur.classList.contains(sel.slice(1))) return cur;
        if (sel.startsWith("#") && cur.id === sel.slice(1)) return cur;
        if (sel.toUpperCase() === cur.tagName) return cur;
        if (sel.includes(",")) {
          for (const s of sel.split(",").map(x => x.trim())) {
            const found = cur.closest(s);
            if (found) return found;
          }
        }
        cur = cur.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all[0] || null;
    },
    querySelectorAll(sel) {
      const results = [];
      function walk(n) {
        for (const c of n.children) {
          if (c.matchesSelector && c.matchesSelector(sel)) {
            results.push(c);
          }
          walk(c);
        }
      }
      walk(this);
      return results;
    },
    matchesSelector(sel) {
      if (sel.startsWith("#") && this.id === sel.slice(1)) return true;
      if (sel.startsWith(".") && this.classList.contains(sel.slice(1))) return true;
      if (sel.toUpperCase() === this.tagName) return true;
      if (sel.includes("[") && sel.endsWith("]")) {
        const bracketIdx = sel.indexOf("[");
        const tagPrefix = sel.slice(0, bracketIdx);
        if (tagPrefix && tagPrefix.toUpperCase() !== this.tagName) return false;
        const inside = sel.slice(bracketIdx + 1, -1);
        if (inside.includes("=")) {
          const parts = inside.split("=");
          const k = parts[0];
          const v = parts[1].replace(/['"]/g, "");
          return this.getAttribute(k) === v;
        } else {
          return this.hasAttribute(inside);
        }
      }
      if (sel.includes(",")) {
        return sel.split(",").map(s => s.trim()).some(s => this.matchesSelector(s));
      }
      if (sel.includes(" ")) {
        const parts = sel.split(" ");
        if (parts.length === 2 && this.matchesSelector(parts[1])) {
          let p = this.parentElement;
          while (p) {
            if (p.matchesSelector && p.matchesSelector(parts[0])) return true;
            p = p.parentElement;
          }
        }
      }
      return false;
    },
    getBoundingClientRect() {
      return { top: 100, bottom: 130, left: 200, right: 300, width: 100, height: 30 };
    },
    scrollIntoView(opts) {
      this._scrolled = true;
    },
    focus() {
      this._focused = true;
    },
    blur() {
      this._blurred = true;
    },
    addEventListener(evt, fn) {
      this._listeners = this._listeners || {};
      this._listeners[evt] = this._listeners[evt] || [];
      this._listeners[evt].push(fn);
    },
    removeEventListener(evt, fn) {}
  };
  return el;
}

const mockStorage = () => {
  const store = new Map();
  return {
    getItem(k) { return store.has(k) ? store.get(k) : null; },
    setItem(k, v) { store.set(k, String(v)); },
    removeItem(k) { store.delete(k); },
    get length() { return store.size; },
    key(i) { return Array.from(store.keys())[i] || null; }
  };
};

const localStorage = mockStorage();
const sessionStorage = mockStorage();

const doc = createElement("html");
doc.body = createElement("body");
doc.documentElement = doc;
doc.appendChild(doc.body);
doc.createElement = createElement;
doc.createTextNode = createTextNode;

doc.getElementById = (id) => {
  function find(n) {
    if (n.id === id) return n;
    for (const c of n.children) {
      const res = find(c);
      if (res) return res;
    }
    return null;
  }
  return find(doc);
};

const listeners = {};
doc.addEventListener = (evt, fn) => {
  listeners[evt] = listeners[evt] || [];
  listeners[evt].push(fn);
};

const win = {
  window: null,
  document: doc,
  localStorage: localStorage,
  sessionStorage: sessionStorage,
  Node: Node,
  URLSearchParams: URLSearchParams,
  location: { hash: "", pathname: "/r/my-repo/c/my-change", search: "?diffs=all" },
  innerWidth: 1200,
  innerHeight: 800,
  scrollY: 0,
  scrollX: 0,
  addEventListener: doc.addEventListener,
  requestAnimationFrame: (cb) => { cb(); return 1; },
  cancelAnimationFrame: () => {},
  setTimeout: (cb) => setTimeout(cb, 0),
  clearTimeout: () => {},
  fetch: (url, opts) => {
    if (url.includes("/api/threads/list")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          threads: [
            { id: 101, path: "COMMIT_MSG", line: 0, author: "Alice", preview: "Fix msg" },
            { id: 102, path: "main.go", line: 15, author: "Bob", preview: "Check ptr" }
          ]
        })
      });
    }
    if (url.includes("/api/threads/reply")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ sig: "sig123", html: "<div>reply</div>" })
      });
    }
    if (url.includes("/api/threads/delete")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ sig: "sig123", thread_deleted: true, thread_id: 102 })
      });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  }
};
win.window = win;

const ctx = vm.createContext(win);
vm.runInContext(code, ctx);

// --- Test Section A: Unresolved threads badge and dropdown navigation ---
const countsContainer = createElement("span");
countsContainer.id = "changeCommentCounts";
countsContainer.setAttribute("data-repo", "my-repo");
countsContainer.setAttribute("data-change", "my-change");
countsContainer.setAttribute("data-base", "parent");
countsContainer.setAttribute("data-target", "live");
doc.body.appendChild(countsContainer);

const badge = createElement("span");
badge.className = "badge badge-count-unresolved";
badge.textContent = "2 unresolved";
countsContainer.appendChild(badge);

// 1. Test enhanceChangeUnresolvedBadge
win.enhanceChangeUnresolvedBadge();
if (!badge.classList.contains("unresolved-threads-trigger")) {
  throw new Error("expected badge to have class unresolved-threads-trigger");
}
if (badge.textContent !== "2 unresolved ▾") {
  throw new Error("expected badge text to be '2 unresolved ▾', got " + badge.textContent);
}
if (badge.getAttribute("role") !== "button") {
  throw new Error("expected role='button'");
}

// 2. Setup DOM diff structure with collapsed file section
const diffAll = createElement("div");
diffAll.className = "diff-all-files";
diffAll.setAttribute("data-repo", "my-repo");
diffAll.setAttribute("data-change", "my-change");
doc.body.appendChild(diffAll);

const fileSec = createElement("div");
fileSec.className = "diff-file-section collapsed";
fileSec.setAttribute("data-file-path", "main.go");
diffAll.appendChild(fileSec);

const fileBody = createElement("div");
fileBody.className = "diff-file-body";
fileBody.style.display = "none";
fileSec.appendChild(fileBody);

const chevron = createElement("span");
chevron.className = "file-collapse-btn";
chevron.textContent = "▸";
fileSec.appendChild(chevron);

const thread102 = createElement("div");
thread102.id = "thread-102";
thread102.className = "thread-card";
fileBody.appendChild(thread102);

// 3. Test toggleUnresolvedMenu opens dropdown
win.toggleUnresolvedMenu(badge);
const menu = doc.body.querySelector(".unresolved-threads-menu");
if (!menu || menu.style.display === "none") {
  throw new Error("expected unresolved-threads-menu to be displayed");
}

// 4. Test jump in diffs=all mode auto-expands section and flashes card
win.jumpToUnresolvedThread("102", "main.go").then(() => {
  if (fileSec.classList.contains("collapsed")) {
    throw new Error("expected fileSec to be expanded");
  }
  if (fileBody.style.display !== "block") {
    throw new Error("expected fileBody display to be block");
  }
  if (!thread102._scrolled) {
    throw new Error("expected thread102 to be scrolled into view");
  }
  if (!thread102.classList.contains("jump-target-flash")) {
    throw new Error("expected thread102 to have jump-target-flash class");
  }
  if (win.threadNavState.el !== thread102) {
    throw new Error("expected threadNavState.el to be thread102");
  }

  // 5. Test jump in diffs=list mode navigates to single file URL
  fileSec.remove();
  diffAll.remove();
  win.jumpToUnresolvedThread("102", "main.go");
  if (!win.location.href.includes("/r/my-repo/c/my-change/f/main.go") || !win.location.href.includes("#thread-102")) {
    throw new Error("expected diffs=list jump to navigate, got: " + win.location.href);
  }

  // 6. Test #thread-102 hash navigation via scrollToHashLine
  doc.body.appendChild(diffAll);
  diffAll.appendChild(fileSec);
  fileSec.classList.add("collapsed");
  fileBody.style.display = "none";
  win.location.hash = "#thread-102";
  win.scrollToHashLine().then(async () => {
    if (fileSec.classList.contains("collapsed")) {
      throw new Error("expected scrollToHashLine to expand collapsed section");
    }

    // --- Test Section B: localStorage Draft Persistence ---
    const diffCont = createElement("div");
    diffCont.className = "diff-container";
    diffCont.setAttribute("data-repo", "my-repo");
    diffCont.setAttribute("data-change", "my-change");
    diffCont.setAttribute("data-file-path", "main.go");
    doc.body.appendChild(diffCont);

    const row20 = createElement("tr");
    row20.setAttribute("data-line-r", "20");
    diffCont.appendChild(row20);

    // B1: Open new inline comment and type
    win.openCommentForm(row20, 20, "R");
    const inlineComment = doc.getElementById("inlineNewComment");
    if (!inlineComment) throw new Error("inlineNewComment not found");
    const cardEl = inlineComment.querySelector(".thread-card");
    if (!cardEl || cardEl.getAttribute("data-start-line") !== "20") {
      throw new Error("expected single-line comment card to have data-start-line='20'");
    }
    const ta = inlineComment.querySelector("textarea");
    ta.value = "Unsaved draft text";
    win.handleInlineCommentInput(ta);

    const draftKey = "review_draft:new:my-repo:my-change:main.go:R:20";
    const memDraft = win.formDraftsMap.get(draftKey);
    if (!memDraft || memDraft.content !== "Unsaved draft text") {
      throw new Error("expected draft in formDraftsMap, got: " + JSON.stringify(memDraft));
    }
    if (localStorage.getItem(draftKey) !== null) {
      throw new Error("expected draft NOT stored in localStorage");
    }

    // B2: Escape preserves draft in formDraftsMap
    win.closeInlineComment(false);
    if (doc.getElementById("inlineNewComment")) throw new Error("inlineNewComment should be closed");
    if (!win.formDraftsMap.has(draftKey)) throw new Error("Escape should NOT clear formDraftsMap draft");

    // B3: Re-open restores draft
    win.openCommentForm(row20, 20, "R");
    const ta2 = doc.getElementById("inlineNewComment").querySelector("textarea");
    if (ta2.value !== "Unsaved draft text") {
      throw new Error("expected draft to be restored into textarea, got: " + ta2.value);
    }

    // B4: Cancel clears draft
    win.closeInlineComment(true);
    if (win.formDraftsMap.has(draftKey)) throw new Error("Cancel MUST clear formDraftsMap draft");

    // B5: Reply draft persistence & Cancel
    const replyForm = createElement("div");
    replyForm.id = "reply-form-102";
    replyForm.className = "reply-form";
    replyForm.style.display = "none";
    const replyTa = createElement("textarea");
    replyForm.appendChild(replyTa);
    diffCont.appendChild(replyForm);

    win.toggleReplyForm(102);
    replyTa.value = "Reply draft text";
    win.handleReplyInput(replyTa, 102);
    const replyKey = "review_draft:reply:my-repo:my-change:102";
    if (!win.formDraftsMap.has(replyKey)) throw new Error("expected reply draft in formDraftsMap");
    if (localStorage.getItem(replyKey) !== null) throw new Error("expected reply draft NOT stored in localStorage");

    win.toggleReplyForm(102, false);
    if (!win.formDraftsMap.has(replyKey)) throw new Error("Escape should NOT clear reply draft");

    // Re-open form (restores draft) and submit reply
    win.toggleReplyForm(102);
    const fakeEvt = { preventDefault() {}, target: replyForm };
    win.replyThread(fakeEvt, 102);
    await new Promise(r => setTimeout(r, 20));
    if (win.formDraftsMap.has(replyKey)) throw new Error("replyThread submission MUST clear reply draft");

    win.toggleReplyForm(102);
    replyTa.value = "Another reply draft";
    win.handleReplyInput(replyTa, 102);
    if (!win.formDraftsMap.has(replyKey)) throw new Error("expected fresh reply draft in formDraftsMap");
    win.toggleReplyForm(102, true);
    if (win.formDraftsMap.has(replyKey)) throw new Error("Cancel MUST clear reply draft");

    // B6: Edit draft persistence & Cancel
    const editForm = createElement("div");
    editForm.id = "msg-edit-301";
    editForm.className = "msg-edit-form";
    editForm.setAttribute("data-orig-body", "Orig");
    editForm.style.display = "none";
    const editTa = createElement("textarea");
    editTa.value = "Orig";
    editForm.appendChild(editTa);
    diffCont.appendChild(editForm);

    win.openEditMessageForm(301);
    editTa.value = "Modified draft";
    win.handleEditMsgInput(editTa, 301);
    const editKey = "review_draft:edit:my-repo:my-change:301";
    if (!win.formDraftsMap.has(editKey)) throw new Error("expected edit draft in formDraftsMap");
    if (localStorage.getItem(editKey) !== null) throw new Error("expected edit draft NOT stored in localStorage");

    win.closeEditMessageForm(301, false);
    if (!win.formDraftsMap.has(editKey)) throw new Error("Escape should NOT clear edit draft");

    win.closeEditMessageForm(301, true);
    if (win.formDraftsMap.has(editKey)) throw new Error("Cancel MUST clear edit draft");

    // B6b: deleteMessage on thread deletion clears reply draft
    win.formDraftsMap.set(replyKey, { content: "leftover reply" });
    win.deleteMessage("301");
    await new Promise(r => setTimeout(r, 20));
    if (win.formDraftsMap.has(replyKey)) throw new Error("deleteMessage thread deletion MUST clear thread reply draft");

    // B7: One-time startup sweep removes ALL review_draft:* keys from localStorage
    const oldKey = "review_draft:reply:my-repo:my-change:999";
    const freshKey = "review_draft:new:my-repo:my-change:main.go:R:15";
    localStorage.setItem(oldKey, JSON.stringify({ body: "old" }));
    localStorage.setItem(freshKey, JSON.stringify({ body: "fresh" }));
    localStorage.setItem("review_theme", "dark");

    win.pruneOldDrafts();
    if (localStorage.getItem(oldKey) !== null) throw new Error("legacy draft should be swept from localStorage");
    if (localStorage.getItem(freshKey) !== null) throw new Error("legacy draft should be swept from localStorage");
    if (localStorage.getItem("review_theme") !== "dark") throw new Error("non-draft keys must not be swept");

    // B8: Multi-line and single-line + unresolved=false restore from formDraftsMap without localStorage
    const row25 = createElement("tr");
    row25.setAttribute("data-line-r", "25");
    diffCont.appendChild(row25);

    const multiKey = "review_draft:new:my-repo:my-change:main.go:R:20";
    win.formDraftsMap.set(multiKey, {
      draft_key: multiKey,
      target_path: "main.go",
      side: "R",
      line: 20,
      end_line: 25,
      start_col: 2,
      end_col: 8,
      unresolved: false,
      content: "Multi-line resolved draft",
      body: "Multi-line resolved draft",
    });
    win.restoreSavedDrafts();
    const serverInline = doc.getElementById("inlineNewComment");
    if (!serverInline) throw new Error("expected inline comment restored from formDraftsMap");
    const serverTa = serverInline.querySelector("textarea");
    if (!serverTa || serverTa.value !== "Multi-line resolved draft") {
      throw new Error("expected server draft text restored, got: " + (serverTa ? serverTa.value : "null"));
    }
    const resCheckbox = serverInline.querySelector('input[name="resolved"]');
    if (!resCheckbox || !resCheckbox.checked) {
      throw new Error("expected resolved checkbox to be checked");
    }
    const restoredCard = serverInline.querySelector(".thread-card");
    if (!restoredCard || restoredCard.getAttribute("data-start-line") !== "20" || restoredCard.getAttribute("data-end-line") !== "25") {
      throw new Error("expected multi-line range restored on card, got: " + (restoredCard ? restoredCard.getAttribute("data-end-line") : "null"));
    }
    win.closeInlineComment(true);

    console.log("SUCCESS");
  });
});
`

	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test in TestUnsavedDraftsAndUnresolvedThreadsUI failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "SUCCESS") {
		t.Fatalf("expected node test to print SUCCESS, got: %s", string(out))
	}
}

func TestSymbolReferencesEnclosingSymbolsAndCallerTree(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur.textContent = (cur.textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT", "PATH", "POLYLINE"].includes(tag) && !attrs.trim().endsWith("/")) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    nodeType: 1,
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    style: {},
    children: [],
    parentElement: null,
    get parentNode() { return this.parentElement; },
    get childNodes() { return this.children; },
    get nextElementSibling() {
      if (!this.parentElement) return null;
      const idx = this.parentElement.children.indexOf(this);
      if (idx !== -1 && idx + 1 < this.parentElement.children.length) {
        return this.parentElement.children[idx + 1];
      }
      return null;
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    get disabled() { return !!this.attributes["disabled"]; },
    set disabled(v) {
      if (v) this.attributes["disabled"] = "disabled";
      else delete this.attributes["disabled"];
    },
    _textContent: "",
    get textContent() {
      let t = this._textContent || "";
      for (const ch of this.children) {
        t += ch.textContent;
      }
      return t;
    },
    set textContent(v) {
      this._textContent = String(v);
    },
    listeners: {},
    get offsetWidth() { return 300; },
    get offsetHeight() { return 200; },
    scrollTop: 0,
    scrollLeft: 0,
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    removeAttribute(k) { delete this.attributes[k]; },
    getBoundingClientRect() { return { top: 100, left: 100, bottom: 300, right: 400, width: 300, height: 200 }; },
    focus() { doc.activeElement = this; },
    blur() { if (doc.activeElement === this) doc.activeElement = null; },
    addEventListener(event, fn) {
      if (!this.listeners[event]) this.listeners[event] = [];
      this.listeners[event].push(fn);
    },
    removeEventListener(event, fn) {
      if (this.listeners[event]) {
        this.listeners[event] = this.listeners[event].filter(f => f !== fn);
      }
    },
    appendChild(child) {
      if (!child) return child;
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    insertBefore(newChild, refChild) {
      if (!newChild) return newChild;
      newChild.parentElement = this;
      const idx = this.children.indexOf(refChild);
      if (idx !== -1) this.children.splice(idx, 0, newChild);
      else this.children.push(newChild);
      return newChild;
    },
    after(newChild) {
      if (!this.parentElement || !newChild) return;
      const p = this.parentElement;
      const idx = p.children.indexOf(this);
      if (idx !== -1 && idx + 1 < p.children.length) {
        p.insertBefore(newChild, p.children[idx + 1]);
      } else {
        p.appendChild(newChild);
      }
    },
    get classList() {
      return {
        contains: (c) => (el.className || "").split(/\s+/).includes(c),
        add: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.add(c));
          el.className = Array.from(parts).join(" ");
        },
        remove: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.delete(c));
          el.className = Array.from(parts).join(" ");
        }
      };
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (node.tagName && matchesSel(node, sel)) return node;
        node = node.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const results = [];
      function search(n) {
        for (const ch of n.children) {
          if (matchesSel(ch, sel)) results.push(ch);
          search(ch);
        }
      }
      search(this);
      return results;
    },
    set innerHTML(val) {
      this._innerHTML = val;
      this.children = parseHtml(val, this);
    },
    get innerHTML() {
      return this._innerHTML || "";
    }
  };
  return el;
}

function matchesSel(node, sel) {
  const parts = sel.split(",").map(s => s.trim());
  for (const p of parts) {
    if (p.includes("[")) {
      const bIdx = p.indexOf("[");
      const tagPart = p.slice(0, bIdx);
      const attrPart = p.slice(bIdx + 1, -1);
      if (tagPart) {
        if (tagPart.startsWith(".")) {
          if (!node.classList || !node.classList.contains(tagPart.slice(1))) continue;
        } else if ((node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
          continue;
        }
      }
      if (attrPart.includes("=")) {
        const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
        if (node.getAttribute && node.getAttribute(k) === v) return true;
      } else {
        if (node.hasAttribute && node.hasAttribute(attrPart)) return true;
      }
    } else if (p.startsWith("#")) {
      const id = p.slice(1);
      if (node.id === id) return true;
    } else if (p.includes(".")) {
      const dotIdx = p.indexOf(".");
      const tag = p.slice(0, dotIdx);
      const cls = p.slice(dotIdx + 1);
      if (tag && (node.tagName || "").toLowerCase() !== tag.toLowerCase()) continue;
      if (cls && (!node.classList || !node.classList.contains(cls))) continue;
      return true;
    } else {
      if ((node.tagName || "").toLowerCase() === p.toLowerCase()) return true;
    }
  }
  return false;
}

const body = createMockElement("BODY");
const docListeners = {};
const winListeners = {};
const doc = {
  documentElement: {
    classList: { contains() { return false; }, add() {}, remove() {} },
    style: { setProperty() {}, getPropertyValue() { return ""; } },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  body: body,
  activeElement: null,
  getElementById(id) {
    if (id === "shortcutsModal") return null;
    const all = body.querySelectorAll("#" + id);
    return all.length > 0 ? all[0] : null;
  },
  createElement(tag) { return createMockElement(tag); },
  createTreeWalker(root) { return { currentNode: root, nextNode() { return null; } }; },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (docListeners[event]) docListeners[event] = docListeners[event].filter(f => f !== fn);
  },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); }
};

let refFetchCount = 0;
let fetchCallerACount = 0;
let fetchCallerDCount = 0;
const win = {
  document: doc,
  NodeFilter: { SHOW_TEXT: 4 },
  location: { pathname: "/r/test/c/abc", search: "", hash: "", reload() {} },
  addEventListener(event, fn) {
    if (!winListeners[event]) winListeners[event] = [];
    winListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (winListeners[event]) winListeners[event] = winListeners[event].filter(f => f !== fn);
  },
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; },
  window: null,
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  fetch(url, opts) {
    if (url.includes("/api/symbol/refs/batch")) {
      const parsed = JSON.parse(opts.body || "{}");
      const results = (parsed.requests || []).map(req => {
        if (req.line === 18 && req.col === 6) {
          fetchCallerACount++;
          return {
            found: true,
            symbol_name: "CallerA",
            refs: [
              { group: "change", path: "foo.go", line: 50, col: 4, in_repo: true, display: "foo.go", line_html: "CallerA()", container: "CallerB", container_line: 48, container_utf16_col: 6 },
              { group: "change", path: "foo.go", line: 52, col: 4, in_repo: true, display: "foo.go", line_html: "CallerA()", container: "CallerInit", container_line: 0, container_utf16_col: 0 },
            ]
          };
        }
        if (req.line === 90 && req.col === 6) {
          fetchCallerDCount++;
          return {
            found: true,
            symbol_name: "CallerD",
            refs: [
              { group: "change", path: "foo.go", line: 95, col: 4, in_repo: true, display: "foo.go", line_html: "CallerD()", container: "CallerDChild", container_line: 0, container_utf16_col: 0 },
            ]
          };
        }
        if (req.line >= 100 && req.line < 115) {
          const nextDepth = req.line - 100 + 1;
          return {
            found: true,
            symbol_name: "Deep" + (nextDepth - 1),
            refs: [
              { group: "change", path: "foo.go", line: 200 + nextDepth, col: 4, in_repo: true, display: "foo.go", line_html: "DeepCaller()", container: "Deep" + nextDepth, container_line: 100 + nextDepth, container_utf16_col: 6 }
            ]
          };
        }
        return { found: false, refs: [] };
      });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ results })
      });
    }
    if (url.includes("/api/symbol/refs")) {
      refFetchCount++;
      if (url.includes("line=18") && url.includes("col=6")) {
        fetchCallerACount++;
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            found: true,
            symbol_name: "CallerA",
            refs: [
              { group: "change", path: "foo.go", line: 50, col: 4, in_repo: true, display: "foo.go", line_html: "CallerA()", container: "CallerB", container_line: 48, container_utf16_col: 6 },
              { group: "change", path: "foo.go", line: 52, col: 4, in_repo: true, display: "foo.go", line_html: "CallerA()", container: "CallerInit", container_line: 0, container_utf16_col: 0 },
            ]
          })
        });
      }
      if (url.includes("line=48") && url.includes("col=6")) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            found: true,
            symbol_name: "CallerB",
            refs: [
              { group: "change", path: "foo.go", line: 80, col: 4, in_repo: true, display: "foo.go", line_html: "CallerB()", container: "CallerC", container_line: 78, container_utf16_col: 6 }
            ]
          })
        });
      }
      if (url.includes("line=78") && url.includes("col=6")) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            found: true,
            symbol_name: "CallerC",
            refs: []
          })
        });
      }
      if (url.includes("line=90") && url.includes("col=6")) {
        fetchCallerDCount++;
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            found: true,
            symbol_name: "CallerD",
            refs: [
              { group: "change", path: "foo.go", line: 95, col: 4, in_repo: true, display: "foo.go", line_html: "CallerD()", container: "CallerDChild", container_line: 0, container_utf16_col: 0 },
            ]
          })
        });
      }
      const deepMatch = url.match(/line=(\d+)/);
      if (deepMatch) {
        const lineNum = parseInt(deepMatch[1], 10);
        if (lineNum >= 100 && lineNum < 115) {
          const nextDepth = lineNum - 100 + 1;
          return Promise.resolve({
            ok: true,
            json: () => Promise.resolve({
              found: true,
              symbol_name: "Deep" + (nextDepth - 1),
              refs: [
                { group: "change", path: "foo.go", line: 200 + nextDepth, col: 4, in_repo: true, display: "foo.go", line_html: "DeepCaller()", container: "Deep" + nextDepth, container_line: 100 + nextDepth, container_utf16_col: 6 }
              ]
            })
          });
        }
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "TargetFunc",
          refs: [
            { group: "change", path: "foo.go", line: 20, col: 4, in_repo: true, display: "foo.go", line_html: "TargetFunc()", container: "CallerA", container_line: 18, container_utf16_col: 6 },
            { group: "change", path: "foo.go", line: 25, col: 4, in_repo: true, display: "foo.go", line_html: "TargetFunc()", container: "CallerA", container_line: 18, container_utf16_col: 6 },
            { group: "change", path: "foo.go", line: 26, col: 4, in_repo: true, display: "foo.go", line_html: "TargetFunc()", container: "CallerD", container_line: 90, container_utf16_col: 6 },
            { group: "change", path: "foo.go", line: 27, col: 4, in_repo: true, display: "foo.go", line_html: "TargetFunc()", container: "CallerD", container_line: 90, container_utf16_col: 6 },
            { group: "change", path: "foo.go", line: 30, col: 4, in_repo: true, display: "foo.go", line_html: "TargetFunc()", container: "init", container_line: 0, container_utf16_col: 0 },
            { group: "change", path: "foo.go", line: 35, col: 4, in_repo: true, display: "foo.go", line_html: "DeepRoot()", container: "Deep0", container_line: 100, container_utf16_col: 6 },
            { group: "package", path: "pkg/bar.go", line: 40, col: 4, in_repo: true, display: "pkg/bar.go", line_html: "var _ = TargetFunc()", container: "", container_line: 0, container_utf16_col: 0 }
          ]
        })
      });
    }
    if (url.includes("site=1")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "ResolvedSite",
          def_path: "foo.go",
          def_line: 50,
          in_repo: true,
          display_path: "foo.go",
          signature: "func CallerA()",
          can_refs: true,
          can_impls: false,
          snippet: [
            { line: 50, html: "CallerA()", active: true }
          ]
        })
      });
    }
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        found: true,
        symbol_name: "TargetFunc",
        def_path: "foo.go",
        def_line: 10,
        in_repo: true,
        display_path: "foo.go",
        signature: "func TargetFunc()",
        can_refs: true,
        can_impls: false,
        snippet: [
          { line: 10, html: "func TargetFunc()", active: true }
        ]
      })
    });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  NodeFilter: win.NodeFilter,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  fetch: win.fetch,
  URLSearchParams,
  Promise,
  setTimeout,
  clearTimeout,
  console,
  AbortController,
});
vm.runInContext(code, ctx);

(async () => {
  const container = createMockElement("DIV");
  container.className = "diff-container";
  container.setAttribute("data-file-path", "foo.go");
  container.setAttribute("data-repo", "testrepo");
  container.setAttribute("data-change", "123");
  body.appendChild(container);

  const tr = createMockElement("TR");
  tr.setAttribute("data-line-r", "10");
  container.appendChild(tr);

  const td = createMockElement("TD");
  td.setAttribute("data-side", "R");
  tr.appendChild(td);

  const tok = createMockElement("SPAN");
  tok.className = "tok-id";
  tok.textContent = "TargetFunc";
  tok._rect = { top: 100, left: 100, bottom: 120, right: 150, width: 50, height: 20 };
  td.appendChild(tok);

  const docClick = docListeners["click"][0];
  await docClick({ button: 0, detail: 1, target: tok, preventDefault() {}, stopPropagation() {} });

  const popup = body.querySelectorAll(".symbol-popup")[0];
  const refsBtn = popup.querySelector(".sym-btn-refs");
  const popupClick = popup.listeners["click"][0];
  await popupClick({ target: refsBtn, preventDefault() {}, stopPropagation() {} });

  // 1. Verify reference view structure, prefix, chevron SVGs, and misclick protection
  const rows = popup.querySelectorAll(".sym-ref-row");
  if (rows.length !== 7) throw new Error("expected 7 ref rows, got " + rows.length);

  // Group header buttons
  const header = popup.querySelector('.sym-group-header[data-ref-group="change"]');
  if (!header) throw new Error("expected change group header");
  const expandBtn = header.querySelector('[data-caller-group-action="expand"]');
  const collapseBtn = header.querySelector('[data-caller-group-action="collapse"]');
  if (!expandBtn || !collapseBtn) throw new Error("expected Expand All and Collapse All buttons in group header");
  if (collapseBtn.disabled !== true) throw new Error("expected Collapse All to be initially disabled");
  if (expandBtn.disabled !== false) throw new Error("expected Expand All to be initially enabled");

  // Row 0: CallerA
  const name0 = rows[0].querySelector(".sym-ref-name");
  if (!name0 || !name0.textContent.includes("CallerA")) {
    throw new Error("expected Row 0 to contain .sym-ref-name with CallerA");
  }
  const prefix0 = name0.querySelector(".sym-caller-prefix");
  if (!prefix0) throw new Error("expected Row 0 to have .sym-caller-prefix");
  const toggle0 = prefix0.querySelector(".sym-caller-toggle");
  if (!toggle0 || toggle0.getAttribute("data-container-line") !== "18") {
    throw new Error("expected Row 0 to have .sym-caller-toggle with line 18");
  }
  const poly0 = toggle0.querySelector("polyline");
  if (toggle0.getAttribute("aria-expanded") !== "false" || !poly0 || poly0.getAttribute("points") !== "4 2.2 8.2 6 4 9.8") {
    throw new Error("expected Row 0 toggle button to be collapsed right chevron");
  }
  if (rows[0].querySelector(".sym-caller-up-arrow")) {
    throw new Error("Row 0 (depth 0) must NOT have .sym-caller-up-arrow");
  }

  // Row 4: init (container_line == 0 -> NO toggle button, NO up arrow)
  const name4 = rows[4].querySelector(".sym-ref-name");
  if (!name4 || name4.textContent.trim() !== "init") {
    throw new Error("expected Row 4 to have .sym-ref-name with 'init'");
  }
  if (name4.querySelector(".sym-caller-toggle") || name4.querySelector(".sym-caller-up-arrow")) {
    throw new Error("Row 4 (init) must NOT have toggle or up-arrow");
  }

  // Row 6: in group package, empty container -> NO .sym-ref-name
  if (rows[6].querySelector(".sym-ref-name")) {
    throw new Error("Row 6 (empty container) must NOT have .sym-ref-name");
  }

  // Test misclick protection: click on .sym-caller-prefix (not directly on toggle button)
  const fetchCountBefore = refFetchCount;
  await popupClick({ target: prefix0, preventDefault() {}, stopPropagation() {} });
  const polyExpanded = toggle0.querySelector("polyline");
  if (toggle0.getAttribute("aria-expanded") !== "true" || !polyExpanded || polyExpanded.getAttribute("points") !== "2.2 4.2 6 8.4 9.8 4.2") {
    throw new Error("expected prefix click to expand toggle0 with down chevron");
  }
  if (refFetchCount !== fetchCountBefore + 1) {
    throw new Error("expected 1 fetch call for CallerA refs");
  }

  // Verify child rows inserted inside .sym-list-inner and up-arrow presence
  const listBodyWrap = popup.querySelector(".sym-list-body");
  const listInner = listBodyWrap ? listBodyWrap.querySelector(".sym-list-inner") : null;
  if (!listInner) throw new Error("expected .sym-list-inner inside .sym-list-body");
  if (!listInner.querySelector('.sym-group-header[data-ref-group="change"]')) {
    throw new Error("expected group headers wrapped inside .sym-list-inner");
  }
  const childContainer = listInner.querySelector('.sym-caller-children[data-parent-node-id="change:0"]');
  if (!childContainer) throw new Error("expected .sym-caller-children inserted inside .sym-list-inner after Row 0");
  if (childContainer.parentElement !== listInner) {
    throw new Error("expected depth-1 .sym-caller-children to be direct child of .sym-list-inner");
  }
  const childRows = childContainer.querySelectorAll(".sym-ref-row");
  if (childRows.length !== 2) throw new Error("expected 2 child rows, got " + childRows.length);

  // Child Row 0: CallerB (expandable, depth = 1)
  const childRow0 = childRows[0];
  if (childRow0.getAttribute("data-caller-depth") !== "1") throw new Error("expected depth 1 on childRow0");
  const childPrefix0 = childRow0.querySelector(".sym-caller-prefix");
  if (!childPrefix0 || !childPrefix0.querySelector(".sym-caller-up-arrow")) {
    throw new Error("expected childRow0 to have .sym-caller-up-arrow inside .sym-caller-prefix");
  }
  const childToggle0 = childPrefix0.querySelector(".sym-caller-toggle");
  if (!childToggle0 || childToggle0.getAttribute("data-container-line") !== "48") {
    throw new Error("expected childRow0 to have toggle with line 48");
  }

  // Child Row 1: CallerInit (non-expandable, depth = 1) -> must STILL have .sym-caller-up-arrow!
  const childRow1 = childRows[1];
  const childPrefix1 = childRow1.querySelector(".sym-caller-prefix");
  if (!childPrefix1 || !childPrefix1.querySelector(".sym-caller-up-arrow")) {
    throw new Error("expected non-expandable childRow1 to STILL have .sym-caller-up-arrow");
  }
  if (childPrefix1.querySelector(".sym-caller-toggle")) {
    throw new Error("non-expandable childRow1 must NOT have toggle button");
  }

  // 2. Expand childRow0 (CallerB -> depth 2 CallerC)
  await popupClick({ target: childToggle0, preventDefault() {}, stopPropagation() {} });
  const grandChildContainer = childContainer.querySelector('.sym-caller-children[data-parent-node-id="change:0.0"]');
  if (!grandChildContainer) throw new Error("expected grandChildContainer for CallerB");
  const grandChildRow = grandChildContainer.querySelector(".sym-ref-row");
  if (!grandChildRow || grandChildRow.getAttribute("data-caller-depth") !== "2") {
    throw new Error("expected grandChildRow at depth 2");
  }

  // 3. Preserve expanded caller tree, scrollTop, and scrollLeft across Back navigation
  const listBody = popup.querySelector(".sym-list-body");
  listBody.scrollTop = 145;
  listBody.scrollLeft = 88;
  const fetchCountBeforeNav = refFetchCount;

  // Click childRow0 to navigate to definition
  await popupClick({ target: childRow0, preventDefault() {}, stopPropagation() {} });
  const locEl = popup.querySelector(".sym-popup-loc");
  if (!locEl || !locEl.textContent.includes("foo.go:50")) {
    throw new Error("expected definition navigation to foo.go:50");
  }

  // Press Back
  const backBtn = popup.querySelector(".sym-btn-back");
  if (!backBtn) throw new Error("expected back button");
  await popupClick({ target: backBtn, preventDefault() {}, stopPropagation() {} });

  // Verify tree preserved synchronously without network fetch
  if (refFetchCount !== fetchCountBeforeNav) {
    throw new Error("Back navigation must NOT trigger network refetch");
  }
  const restoredListBody = popup.querySelector(".sym-list-body");
  if (restoredListBody.scrollTop !== 145) {
    throw new Error("expected restored scrollTop 145, got " + restoredListBody.scrollTop);
  }
  if (restoredListBody.scrollLeft !== 88) {
    throw new Error("expected restored scrollLeft 88, got " + restoredListBody.scrollLeft);
  }
  if (!popup.querySelector('.sym-caller-children[data-parent-node-id="change:0"]')) {
    throw new Error("expected Row 0 to remain expanded after Back");
  }
  if (!popup.querySelector('.sym-caller-children[data-parent-node-id="change:0.0"]')) {
    throw new Error("expected Child Row 0 to remain expanded after Back");
  }

  // 4. Test incremental Collapse All and Expand All with declaration deduplication
  const changeHeader = popup.querySelector('.sym-group-header[data-ref-group="change"]') || popup.querySelector('.sym-group-header');
  const curCollapseBtn = changeHeader.querySelector('[data-caller-group-action="collapse"]');
  const curExpandBtn = changeHeader.querySelector('[data-caller-group-action="expand"]');

  // Collapse level 2
  await popupClick({ target: curCollapseBtn, preventDefault() {}, stopPropagation() {} });
  if (popup.querySelector('.sym-caller-children[data-parent-node-id="change:0.0"]')) {
    throw new Error("expected level 2 (CallerB children) to be collapsed");
  }
  if (!popup.querySelector('.sym-caller-children[data-parent-node-id="change:0"]')) {
    throw new Error("expected level 1 (Row 0 children) to remain expanded after first Collapse All click");
  }
  if (curCollapseBtn.disabled !== false) throw new Error("Collapse All should still be enabled");

  // Collapse level 1
  await popupClick({ target: curCollapseBtn, preventDefault() {}, stopPropagation() {} });
  if (popup.querySelector('.sym-caller-children[data-parent-node-id="change:0"]')) {
    throw new Error("expected level 1 to be collapsed after second Collapse All click");
  }
  if (curCollapseBtn.disabled !== true) throw new Error("Collapse All should be disabled when all rows collapsed");

  // Test Expand All:
  // - CallerA was loaded in step 1, so expanding Row 0 and Row 1 hits the declaration cache (0 new fetches for CallerA).
  // - CallerD (Row 2 and Row 3) is a new declaration that was never loaded. Expanding both rows concurrently fetches CallerD ONCE!
  const fetchACountBefore = fetchCallerACount;
  const fetchDCountBefore = fetchCallerDCount;
  await popupClick({ target: curExpandBtn, preventDefault() {}, stopPropagation() {} });

  if (!popup.querySelector('.sym-caller-children[data-parent-node-id="change:0"]')) {
    throw new Error("expected Row 0 expanded after Expand All");
  }
  if (!popup.querySelector('.sym-caller-children[data-parent-node-id="change:1"]')) {
    throw new Error("expected Row 1 expanded after Expand All");
  }
  if (!popup.querySelector('.sym-caller-children[data-parent-node-id="change:2"]')) {
    throw new Error("expected Row 2 (CallerD) expanded after Expand All");
  }
  const expandedInner = popup.querySelector(".sym-list-body").querySelector(".sym-list-inner");
  if (!expandedInner || !expandedInner.querySelector('.sym-caller-children[data-parent-node-id="change:3"]')) {
    throw new Error("expected Row 3 (CallerD) expanded inside .sym-list-inner after Expand All");
  }
  // CallerA was already cached: 0 new fetches
  if (fetchCallerACount !== fetchACountBefore) {
    throw new Error("expected CallerA to be served from cache with 0 fetches");
  }
  // CallerD was uncached: deduplicated across the 2 concurrent nodes, so exactly 1 fetch!
  if (fetchCallerDCount !== fetchDCountBefore + 1) {
    throw new Error("expected CallerD declaration fetch to be deduplicated: " + (fetchCallerDCount - fetchDCountBefore));
  }
  if (curCollapseBtn.disabled !== false) throw new Error("Collapse All should be enabled after Expand All");

  // Collapse after Expand All so all rows are collapsed before uncapped depth test
  await popupClick({ target: curCollapseBtn, preventDefault() {}, stopPropagation() {} });
  if (curCollapseBtn.disabled !== true) throw new Error("Collapse All should be disabled after collapsing expanded level 1");

  // 5. Test uncapped depth past depth 8 (e.g. depth 10+)
  // Find Deep0 row
  const allRefRows = popup.querySelectorAll('.sym-ref-row');
  let deepRow0 = null;
  for (const r of allRefRows) {
    if (r.textContent.includes("Deep0")) {
      deepRow0 = r;
      break;
    }
  }
  if (!deepRow0) throw new Error("expected Deep0 row");
  let deepToggle = deepRow0.querySelector(".sym-caller-toggle");
  if (!deepToggle) throw new Error("expected Deep0 toggle");

  let currentToggle = deepToggle;
  for (let d = 0; d < 10; d++) {
    await popupClick({ target: currentToggle, preventDefault() {}, stopPropagation() {} });
    const allRows = popup.querySelectorAll('.sym-ref-row');
    const nextRow = allRows.find(r => r.textContent.includes("Deep" + (d + 1)));
    if (!nextRow) throw new Error("expected row Deep" + (d + 1));
    currentToggle = nextRow.querySelector(".sym-caller-toggle");
    if (!currentToggle) throw new Error("expected toggle for Deep" + (d + 1));
  }
  // At depth 10:
  const allRowsAt10 = popup.querySelectorAll('.sym-ref-row');
  const row10 = allRowsAt10.find(r => r.textContent.includes("Deep10"));
  if (!row10) throw new Error("expected row at depth 10");
  if (row10.getAttribute("data-caller-depth") !== "10") throw new Error("expected depth 10, got " + row10.getAttribute("data-caller-depth"));
  if (!row10.querySelector(".sym-caller-up-arrow")) throw new Error("expected up-arrow at depth 10");
  const toggle10 = row10.querySelector(".sym-caller-toggle");
  if (!toggle10) throw new Error("expected toggle at depth 10 (uncapped depth!)");

  // Expand depth 10 to depth 11
  await popupClick({ target: toggle10, preventDefault() {}, stopPropagation() {} });
  const allRowsAt11 = popup.querySelectorAll('.sym-ref-row');
  const row11 = allRowsAt11.find(r => r.textContent.includes("Deep11"));
  if (!row11) throw new Error("expected row at depth 11");
  if (row11.getAttribute("data-caller-depth") !== "11") throw new Error("expected depth 11, got " + row11.getAttribute("data-caller-depth"));

  console.log("SUCCESS");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TestSymbolReferencesEnclosingSymbolsAndCallerTree failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "SUCCESS") {
		t.Fatalf("expected node test to print SUCCESS, got: %s", string(out))
	}
}

func TestMoveBlockPopupWindow(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
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
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")

	goMod := "module example.com/movetest\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	sourceGo1 := `package main

func LocalMoved() {
	// line local 1
	// line local 2
	// line local 3
}

func ComputeWidget(x int, y int) int {
	a := x * 2
	b := y * 3
	sum := a + b
	if sum > 100 {
		return sum - 10
	}
	return sum + 10
}

func OtherFunc() {
	println("other")
}
`
	destGo1 := `package main

func ExistingDest() {
	println("existing dest")
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "source.go"), []byte(sourceGo1), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "dest.go"), []byte(destGo1), 0644)
	run("add", ".")
	run("commit", "-m", "Initial commit")
	firstHash := run("rev-parse", "HEAD")

	sourceGo2 := `package main

func OtherFunc() {
	println("other")
}

func LocalMoved() {
	// line local 1
	// line local 2
	// line local 3
}
`
	destGo2 := `package main

func ExistingDest() {
	println("existing dest")
}

func ComputeWidget(x int, y int) int {
	a := x * 2
	b := y * 5
	sum := a + b
	if sum > 100 {
		return sum - 10
	}
	return sum + 10
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "source.go"), []byte(sourceGo2), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "dest.go"), []byte(destGo2), 0644)
	run("commit", "-a", "-m", "Move ComputeWidget to dest.go (modified)")
	secondHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(func() { ts.Close() })

	// 1. Verify split view diff renders move-popup-btn with required attributes
	splitURL := fmt.Sprintf("%s/r/%s/c/%s?diffs=all&mode=split&base=%s&target=%s", ts.URL, rec.Slug, secondHash, firstHash, secondHash)
	respSplit, err := http.Get(splitURL)
	if err != nil {
		t.Fatalf("GET split diff failed: %v", err)
	}
	defer respSplit.Body.Close()
	if respSplit.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for split diff, got %d", respSplit.StatusCode)
	}
	splitBytes, _ := io.ReadAll(respSplit.Body)
	splitHTML := string(splitBytes)

	if !strings.Contains(splitHTML, "move-popup-btn") {
		t.Errorf("expected split diff to contain move-popup-btn")
	}
	if !strings.Contains(splitHTML, "move-header-actions") {
		t.Errorf("expected split diff to contain move-header-actions")
	}
	if !strings.Contains(splitHTML, `data-peer-path="source.go"`) && !strings.Contains(splitHTML, `data-peer-path="dest.go"`) {
		t.Errorf("expected split diff move-popup-btn to contain data-peer-path")
	}
	if !strings.Contains(splitHTML, `<span class="move-header-actions">`) {
		t.Errorf("expected split diff to wrap move badge and popup btn in move-header-actions span")
	}

	// Verify segmented pill styling in static/style.css
	respCSS, err := http.Get(ts.URL + "/static/style.css")
	if err != nil {
		t.Fatalf("GET /static/style.css failed: %v", err)
	}
	cssBytes, _ := io.ReadAll(respCSS.Body)
	respCSS.Body.Close()
	cssContent := string(cssBytes)
	if !strings.Contains(cssContent, ".move-header-actions {") || !strings.Contains(cssContent, "border-radius: 12px;") {
		t.Errorf("expected style.css to contain .move-header-actions with border-radius: 12px")
	}
	if !strings.Contains(cssContent, ".move-header-actions .move-badge") {
		t.Errorf("expected style.css to contain .move-header-actions .move-badge")
	}
	if !strings.Contains(cssContent, ".move-header-actions .move-popup-btn") || !strings.Contains(cssContent, "border-left: 1px solid var(--diff-move-badge-border);") {
		t.Errorf("expected style.css to contain .move-header-actions .move-popup-btn with border-left divider")
	}

	// 2. Verify unified view diff also renders move-popup-btn
	unifiedURL := fmt.Sprintf("%s/r/%s/c/%s?diffs=all&mode=unified&base=%s&target=%s", ts.URL, rec.Slug, secondHash, firstHash, secondHash)
	respUnified, err := http.Get(unifiedURL)
	if err != nil {
		t.Fatalf("GET unified diff failed: %v", err)
	}
	defer respUnified.Body.Close()
	if respUnified.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for unified diff, got %d", respUnified.StatusCode)
	}
	unifiedBytes, _ := io.ReadAll(respUnified.Body)
	unifiedHTML := string(unifiedBytes)
	if !strings.Contains(unifiedHTML, "move-popup-btn") {
		t.Errorf("expected unified diff to contain move-popup-btn")
	}

	// 3. Test GET /api/diff/move-preview
	// 3a. Query destination preview on dest.go (side=R, lines 7..17)
	destAPIURL := fmt.Sprintf("%s/api/diff/move-preview?repo=%s&change=%s&base=%s&target=%s&path=dest.go&side=R&start=7&end=17",
		ts.URL, rec.Slug, secondHash, firstHash, secondHash)
	respDestAPI, err := http.Get(destAPIURL)
	if err != nil {
		t.Fatalf("GET move-preview for dest.go failed: %v", err)
	}
	defer respDestAPI.Body.Close()
	if respDestAPI.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", respDestAPI.StatusCode)
	}
	var destPreview movePreviewResponse
	if err := json.NewDecoder(respDestAPI.Body).Decode(&destPreview); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	if !destPreview.Found {
		t.Errorf("expected dest.go move preview to be found")
	}
	if destPreview.Path != "dest.go" || destPreview.Side != "R" {
		t.Errorf("expected Path=dest.go Side=R, got Path=%s Side=%s", destPreview.Path, destPreview.Side)
	}
	if !destPreview.BlockModified {
		t.Errorf("expected dest.go moved block to be marked modified")
	}
	if len(destPreview.Rows) == 0 {
		t.Errorf("expected dest.go move preview to contain rows")
	}
	var foundIntra bool
	for _, row := range destPreview.Rows {
		if row.Modified && strings.Contains(row.HTML, `mark class="intra"`) {
			foundIntra = true
			break
		}
	}
	if !foundIntra {
		t.Errorf("expected modified line in dest.go preview to have intra diff highlight")
	}

	// 3b. Query source preview on source.go (side=L, lines 10..20)
	srcAPIURL := fmt.Sprintf("%s/api/diff/move-preview?repo=%s&change=%s&base=%s&target=%s&path=source.go&side=L&start=10&end=20",
		ts.URL, rec.Slug, secondHash, firstHash, secondHash)
	respSrcAPI, err := http.Get(srcAPIURL)
	if err != nil {
		t.Fatalf("GET move-preview for source.go failed: %v", err)
	}
	defer respSrcAPI.Body.Close()
	var srcPreview movePreviewResponse
	if err := json.NewDecoder(respSrcAPI.Body).Decode(&srcPreview); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	if !srcPreview.Found {
		t.Errorf("expected source.go move preview to be found")
	}
	if srcPreview.Side != "L" {
		t.Errorf("expected source.go move preview side=L, got %s", srcPreview.Side)
	}

	// 3c. Invalid queries: COMMIT_MSG, nonexistent file, out-of-range start
	checkNotFound := func(url string, desc string) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s failed: %v", desc, err)
		}
		defer resp.Body.Close()
		var p movePreviewResponse
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatalf("failed to decode %s response: %v", desc, err)
		}
		if p.Found {
			t.Errorf("expected %s to return found=false, got true", desc)
		}
	}
	checkNotFound(fmt.Sprintf("%s/api/diff/move-preview?repo=%s&change=%s&path=COMMIT_MSG&side=R&start=1&end=5", ts.URL, rec.Slug, secondHash), "COMMIT_MSG")
	checkNotFound(fmt.Sprintf("%s/api/diff/move-preview?repo=%s&change=%s&path=nonexistent.go&side=R&start=1&end=5", ts.URL, rec.Slug, secondHash), "nonexistent file")
	checkNotFound(fmt.Sprintf("%s/api/diff/move-preview?repo=%s&change=%s&path=dest.go&side=R&start=99999&end=99999", ts.URL, rec.Slug, secondHash), "out of range start")

	// 4. Node VM test testing static/app.js popup interactions
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur.textContent = (cur.textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT", "PATH", "POLYLINE", "RECT", "LINE"].includes(tag) && !attrs.trim().endsWith("/")) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    nodeType: 1,
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    style: {},
    children: [],
    parentElement: null,
    get parentNode() { return this.parentElement; },
    get childNodes() { return this.children; },
    get nextElementSibling() {
      if (!this.parentElement) return null;
      const idx = this.parentElement.children.indexOf(this);
      if (idx !== -1 && idx + 1 < this.parentElement.children.length) {
        return this.parentElement.children[idx + 1];
      }
      return null;
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    contains(node) {
      let cur = node;
      while (cur) {
        if (cur === this) return true;
        cur = cur.parentElement;
      }
      return false;
    },
    get disabled() { return !!this.attributes["disabled"]; },
    set disabled(v) {
      if (v) this.attributes["disabled"] = "disabled";
      else delete this.attributes["disabled"];
    },
    _textContent: "",
    get textContent() {
      let t = this._textContent || "";
      for (const ch of this.children) {
        t += ch.textContent;
      }
      return t;
    },
    set textContent(v) {
      this._textContent = String(v);
    },
    listeners: {},
    get offsetWidth() { return 300; },
    get offsetHeight() { return 200; },
    scrollTop: 0,
    scrollLeft: 0,
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    removeAttribute(k) { delete this.attributes[k]; },
    getBoundingClientRect() { return { top: 100, left: 100, bottom: 300, right: 400, width: 300, height: 200 }; },
    focus() { doc.activeElement = this; },
    blur() { if (doc.activeElement === this) doc.activeElement = null; },
    addEventListener(event, fn) {
      if (!this.listeners[event]) this.listeners[event] = [];
      this.listeners[event].push(fn);
    },
    removeEventListener(event, fn) {
      if (this.listeners[event]) {
        this.listeners[event] = this.listeners[event].filter(f => f !== fn);
      }
    },
    appendChild(child) {
      if (!child) return child;
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    insertBefore(newChild, refChild) {
      if (!newChild) return newChild;
      newChild.parentElement = this;
      const idx = this.children.indexOf(refChild);
      if (idx !== -1) this.children.splice(idx, 0, newChild);
      else this.children.push(newChild);
      return newChild;
    },
    get classList() {
      return {
        contains: (c) => (el.className || "").split(/\s+/).includes(c),
        add: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.add(c));
          el.className = Array.from(parts).join(" ");
        },
        remove: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.delete(c));
          el.className = Array.from(parts).join(" ");
        }
      };
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (node.tagName && matchesSel(node, sel)) return node;
        node = node.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const results = [];
      function search(n) {
        for (const ch of n.children) {
          if (matchesSel(ch, sel)) results.push(ch);
          search(ch);
        }
      }
      search(this);
      return results;
    },
    set innerHTML(val) {
      this._innerHTML = val;
      this.children = parseHtml(val, this);
    },
    get innerHTML() {
      return this._innerHTML || "";
    }
  };
  return el;
}

function matchesSel(node, sel) {
  const parts = sel.split(",").map(s => s.trim());
  for (const p of parts) {
    if (p.includes("[")) {
      const bIdx = p.indexOf("[");
      const tagPart = p.slice(0, bIdx);
      const attrPart = p.slice(bIdx + 1, -1);
      if (tagPart) {
        if (tagPart.startsWith(".")) {
          if (!node.classList || !node.classList.contains(tagPart.slice(1))) continue;
        } else if ((node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
          continue;
        }
      }
      if (attrPart.includes("=")) {
        const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
        if (node.getAttribute && node.getAttribute(k) === v) return true;
      } else {
        if (node.hasAttribute && node.hasAttribute(attrPart)) return true;
      }
    } else if (p.startsWith("#")) {
      const id = p.slice(1);
      if (node.id === id) return true;
    } else if (p.includes(".")) {
      const dotIdx = p.indexOf(".");
      const tag = p.slice(0, dotIdx);
      const cls = p.slice(dotIdx + 1);
      if (tag && (node.tagName || "").toLowerCase() !== tag.toLowerCase()) continue;
      if (cls && (!node.classList || !node.classList.contains(cls))) continue;
      return true;
    } else {
      if ((node.tagName || "").toLowerCase() === p.toLowerCase()) return true;
    }
  }
  return false;
}

const body = createMockElement("BODY");
const docListeners = {};
const winListeners = {};
const doc = {
  documentElement: {
    classList: { contains() { return false; }, add() {}, remove() {} },
    style: { setProperty() {}, getPropertyValue() { return ""; } },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {},
  },
  body: body,
  activeElement: null,
  getElementById(id) {
    const all = body.querySelectorAll("#" + id);
    return all.length > 0 ? all[0] : null;
  },
  createElement(tag) { return createMockElement(tag); },
  createRange() {
    return {
      setStart() {},
      setEnd() {},
      toString() { return ""; }
    };
  },
  createTreeWalker(root) { return { currentNode: root, nextNode() { return null; } }; },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (docListeners[event]) docListeners[event] = docListeners[event].filter(f => f !== fn);
  },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); }
};

let movePreviewRequestedUrl = "";
let symbolLookupRequestedUrl = "";

const win = {
  document: doc,
  NodeFilter: { SHOW_TEXT: 4 },
  location: { pathname: "/r/testrepo/c/123", search: "", hash: "", href: "", reload() {} },
  addEventListener(event, fn) {
    if (!winListeners[event]) winListeners[event] = [];
    winListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (winListeners[event]) winListeners[event] = winListeners[event].filter(f => f !== fn);
  },
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; },
  window: null,
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  fetch(url, opts) {
    if (url.includes("/api/diff/move-preview")) {
      movePreviewRequestedUrl = url;
      if (url.includes("side=R") || url.includes("path=dest.go")) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            found: true,
            path: "dest.go",
            side: "R",
            start_line: 7,
            end_line: 17,
            block_modified: true,
            left_rev: "revL",
            right_rev: "revR",
            rows: [
              { line: 5, html: "func ExistingDest() {", moved: false, modified: false },
              { line: 7, html: "func <span class=\"tok-id\">ComputeWidget</span>(x int, y int) int {", moved: true, modified: false },
              { line: 8, html: "	a := x * 2", moved: true, modified: false },
              { line: 9, html: "	b := y * <mark class=\"intra\">5</mark>", moved: true, modified: true },
              { line: 17, html: "}", moved: true, modified: false }
            ]
          })
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          path: "source.go",
          side: "L",
          start_line: 11,
          end_line: 21,
          block_modified: true,
          left_rev: "revL",
          right_rev: "revR",
          rows: [
            { line: 8, html: "func LocalMoved() {", moved: false, modified: false },
            { line: 11, html: "func <span class=\"tok-id\">ComputeWidget</span>(x int, y int) int {", moved: true, modified: false },
            { line: 12, html: "	a := x * 2", moved: true, modified: false },
            { line: 13, html: "	b := y * <mark class=\"intra\">3</mark>", moved: true, modified: true },
            { line: 21, html: "}", moved: true, modified: false }
          ]
        })
      });
    }
    if (url.includes("/api/symbol")) {
      symbolLookupRequestedUrl = url;
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "ComputeWidget",
          display_path: "source.go",
          def_path: "source.go",
          def_line: 11,
          in_repo: true,
          signature: "func ComputeWidget(x int, y int) int",
          can_refs: true,
          can_impls: false,
          snippet: [
            { line: 11, html: "func ComputeWidget(x int, y int) int {", active: true }
          ]
        })
      });
    }
    return Promise.resolve({ ok: false, status: 404 });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  Node: { ELEMENT_NODE: 1, TEXT_NODE: 3 },
  NodeFilter: win.NodeFilter,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  fetch: win.fetch,
  URLSearchParams,
  Promise,
  setTimeout,
  clearTimeout,
  console,
  AbortController,
});
vm.runInContext(code, ctx);

(async () => {
  const container = createMockElement("DIV");
  container.className = "diff-container";
  container.setAttribute("data-file-path", "dest.go");
  container.setAttribute("data-repo", "testrepo");
  container.setAttribute("data-change", "123");
  body.appendChild(container);

  const btn = createMockElement("BUTTON");
  btn.className = "move-popup-btn";
  btn.setAttribute("data-peer-path", "source.go");
  btn.setAttribute("data-peer-start", "11");
  btn.setAttribute("data-peer-end", "21");
  btn.setAttribute("data-peer-side", "L");
  btn.setAttribute("data-move-modified", "true");
  container.appendChild(btn);

  // 1. Open Move Popup Window
  await win.handleOpenMovePeerPopup({ button: 0, detail: 1, preventDefault() {}, stopPropagation() {} }, btn);

  if (!movePreviewRequestedUrl.includes("path=source.go") || !movePreviewRequestedUrl.includes("side=L")) {
    throw new Error("expected move-preview request for source.go side=L, got: " + movePreviewRequestedUrl);
  }

  const popups = body.querySelectorAll(".symbol-popup");
  if (popups.length !== 1) throw new Error("expected 1 symbol popup, got " + popups.length);
  const popup = popups[0];

  const locEl = popup.querySelector(".sym-popup-loc");
  if (!locEl || !locEl.textContent.includes("← Moved from") || !locEl.textContent.includes("source.go:L11–21")) {
    throw new Error("expected header loc with Moved from source.go:L11–21, got: " + (locEl ? locEl.textContent : "null"));
  }
  const modLabel = locEl.querySelector(".sym-move-mod-label");
  if (!modLabel || modLabel.textContent !== "modified") {
    throw new Error("expected .sym-move-mod-label with text modified");
  }

  const jumpLink = popup.querySelector("[data-jump-move-peer]");
  if (!jumpLink || jumpLink.getAttribute("data-peer-path") !== "source.go") {
    throw new Error("expected jump link with data-peer-path=source.go");
  }
  if (jumpLink.getAttribute("data-peer-end") !== "21") {
    throw new Error("expected jump link data-peer-end=21, got: " + jumpLink.getAttribute("data-peer-end"));
  }
  const jumpHref = jumpLink.getAttribute("href") || "";
  if (!jumpHref.includes("&end=21#L11")) {
    throw new Error("expected jump link href to contain &end=21#L11, got: " + jumpHref);
  }

  const moveRows = popup.querySelectorAll(".sym-move-row");
  if (moveRows.length !== 4) {
    throw new Error("expected 4 sym-move-rows, got " + moveRows.length);
  }
  const modRow = popup.querySelector(".sym-move-row-del");
  if (!modRow) throw new Error("expected sym-move-row-del for modified line");

  // 2. Click jump link
  let jumpedTarget = null;
  win.jumpToMovePeerTarget = (target, e) => { jumpedTarget = target; };
  const popupClick = popup.listeners["click"][0];
  await popupClick({ target: jumpLink, preventDefault() {}, stopPropagation() {} });
  if (!jumpedTarget || jumpedTarget.peerPath !== "source.go" || jumpedTarget.peerStart !== 11 || jumpedTarget.peerEnd !== 21) {
    throw new Error("expected jumpToMovePeerTarget called with source.go:11..21, got: " + JSON.stringify(jumpedTarget));
  }

  // 3. Token click inside moved snippet -> chained symbol lookup
  const tok = popup.querySelector(".tok-id");
  if (!tok) throw new Error("expected .tok-id token in moved rows");
  await popupClick({ button: 0, detail: 1, target: tok, preventDefault() {}, stopPropagation() {} });

  if (!symbolLookupRequestedUrl.includes("path=source.go") || !symbolLookupRequestedUrl.includes("side=L")) {
    throw new Error("expected symbol lookup for source.go side=L, got: " + symbolLookupRequestedUrl);
  }

  // Symbol definition view is rendered, with back button
  const backBtn = popup.querySelector(".sym-btn-back");
  if (!backBtn) throw new Error("expected .sym-btn-back after chaining into symbol definition");

  // 4. Back navigation restores move popup
  await popupClick({ target: backBtn, preventDefault() {}, stopPropagation() {} });
  const restoredLoc = popup.querySelector(".sym-popup-loc");
  if (!restoredLoc || !restoredLoc.textContent.includes("← Moved from") || !restoredLoc.textContent.includes("source.go:L11–21")) {
    throw new Error("expected restored move popup after back, got: " + (restoredLoc ? restoredLoc.textContent : "null"));
  }

  // 5. Close popup
  const closeBtn = popup.querySelector(".sym-btn-close");
  if (!closeBtn) throw new Error("expected close button");
  await popupClick({ target: closeBtn, preventDefault() {}, stopPropagation() {} });
  if (body.querySelectorAll(".symbol-popup").length !== 0) {
    throw new Error("expected popup removed after close");
  }

  // 6. Test opening move popup from "Moved to" header (side=R, e.g. dest.go preview)
  const btnR = createMockElement("BUTTON");
  btnR.className = "move-popup-btn";
  btnR.setAttribute("data-peer-path", "dest.go");
  btnR.setAttribute("data-peer-start", "7");
  btnR.setAttribute("data-peer-end", "17");
  btnR.setAttribute("data-peer-side", "R");
  btnR.setAttribute("data-move-modified", "true");
  container.appendChild(btnR);

  await win.handleOpenMovePeerPopup({ button: 0, detail: 1, preventDefault() {}, stopPropagation() {} }, btnR);

  if (!movePreviewRequestedUrl.includes("path=dest.go") || !movePreviewRequestedUrl.includes("side=R")) {
    throw new Error("expected move-preview request for dest.go side=R, got: " + movePreviewRequestedUrl);
  }

  const popupR = body.querySelectorAll(".symbol-popup")[0];
  const locElR = popupR.querySelector(".sym-popup-loc");
  if (!locElR || !locElR.textContent.includes("→ Moved to") || !locElR.textContent.includes("dest.go:L7–17")) {
    throw new Error("expected header loc with Moved to dest.go:L7–17, got: " + (locElR ? locElR.textContent : "null"));
  }
  if (locElR.textContent.includes(":R")) {
    throw new Error("popup header loc must NEVER contain :R (must use L for line), got: " + locElR.textContent);
  }

  const closeBtnR = popupR.querySelector(".sym-btn-close");
  await popupR.listeners["click"][0]({ target: closeBtnR, preventDefault() {}, stopPropagation() {} });
  if (body.querySelectorAll(".symbol-popup").length !== 0) {
    throw new Error("expected popupR removed after close");
  }

  console.log("SUCCESS");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TestMoveBlockPopupWindow node test failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "SUCCESS") {
		t.Fatalf("expected node test to print SUCCESS, got: %s", string(out))
	}
}

func TestResolvedThreadsAPIAndMenu(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := vcs.Open(repoDir, srv.cache)
	changes, _ := repo.Changes()
	c := changes[0]

	// Add 1 unresolved thread and 1 resolved thread
	thUnres, _, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "main.go", "R", 1, 1, "anchor", "author1", "**open** `question`\n\nsecond line", false)
	if err != nil {
		t.Fatal(err)
	}
	thRes, _, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "main.go", "R", 2, 2, "anchor", "author2", "**fixed** `issue`\n\nresolved note", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetResolved(thRes.ID, true); err != nil {
		t.Fatal(err)
	}

	// 1. GET /api/threads/list returns open threads only
	unresURL := fmt.Sprintf("%s/api/threads/list?repo=%s&change=%s", ts.URL, rec.Slug, c.Key)
	resp, err := http.Get(unresURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/threads/list failed: status=%v err=%v", resp.StatusCode, err)
	}
	var resUnres struct {
		Threads []struct {
			ID          int64  `json:"id"`
			Line        int    `json:"line"`
			Path        string `json:"path"`
			PreviewHTML string `json:"preview_html"`
		} `json:"threads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resUnres); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(resUnres.Threads) != 1 || resUnres.Threads[0].ID != thUnres.ID {
		t.Fatalf("expected 1 unresolved thread (id=%d), got %+v", thUnres.ID, resUnres.Threads)
	}
	if !strings.Contains(resUnres.Threads[0].PreviewHTML, "<strong>open</strong>") || !strings.Contains(resUnres.Threads[0].PreviewHTML, "<code class=\"md-inline-code\">question</code>") {
		t.Errorf("expected unresolved thread preview_html to contain rendered markdown HTML, got: %q", resUnres.Threads[0].PreviewHTML)
	}

	// 2. GET /api/threads/list?status=resolved returns resolved threads
	resURL := fmt.Sprintf("%s/api/threads/list?status=resolved&repo=%s&change=%s", ts.URL, rec.Slug, c.Key)
	resp, err = http.Get(resURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/threads/list?status=resolved failed: status=%v err=%v", resp.StatusCode, err)
	}
	var resRes struct {
		Threads []struct {
			ID          int64  `json:"id"`
			Line        int    `json:"line"`
			Path        string `json:"path"`
			PreviewHTML string `json:"preview_html"`
		} `json:"threads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resRes); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(resRes.Threads) != 1 || resRes.Threads[0].ID != thRes.ID {
		t.Fatalf("expected 1 resolved thread (id=%d), got %+v", thRes.ID, resRes.Threads)
	}
	if !strings.Contains(resRes.Threads[0].PreviewHTML, "<strong>fixed</strong>") || !strings.Contains(resRes.Threads[0].PreviewHTML, "<code class=\"md-inline-code\">issue</code>") {
		t.Errorf("expected resolved thread preview_html to contain rendered markdown HTML, got: %q", resRes.Threads[0].PreviewHTML)
	}

	// 3. Whitespace-only first message is skipped; >12-line unclosed code fence is closed cleanly
	thBlankFirst, _, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "main.go", "R", 3, 3, "anchor", "blankAuthor", "   \n\t  ", false)
	if err != nil {
		t.Fatal(err)
	}
	longCode := "real header\n```go\n" + strings.Repeat("x := 1\n", 15) + "```"
	if _, err := d.AddMessage(thBlankFirst.ID, "realAuthor", longCode, false); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(fmt.Sprintf("%s/api/threads/list?status=unresolved&repo=%s&change=%s", ts.URL, rec.Slug, c.Key))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/threads/list unresolved failed: status=%v err=%v", resp.StatusCode, err)
	}
	var resBlankList struct {
		Threads []struct {
			ID          int64  `json:"id"`
			Author      string `json:"author"`
			Preview     string `json:"preview"`
			PreviewHTML string `json:"preview_html"`
		} `json:"threads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resBlankList); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var foundBlank *struct {
		ID          int64  `json:"id"`
		Author      string `json:"author"`
		Preview     string `json:"preview"`
		PreviewHTML string `json:"preview_html"`
	}
	for i := range resBlankList.Threads {
		if resBlankList.Threads[i].ID == thBlankFirst.ID {
			foundBlank = &resBlankList.Threads[i]
			break
		}
	}
	if foundBlank == nil {
		t.Fatalf("expected thread id=%d in unresolved list", thBlankFirst.ID)
	}
	if foundBlank.Author != "realAuthor" {
		t.Errorf("expected whitespace-only first message skipped -> author=realAuthor, got %q", foundBlank.Author)
	}
	if !strings.Contains(foundBlank.PreviewHTML, "<pre") || !strings.Contains(foundBlank.PreviewHTML, "</pre>") {
		t.Errorf("expected truncated fenced code block to close cleanly in preview_html, got: %q", foundBlank.PreviewHTML)
	}

	// 3c. Direct check of truncateMarkdownPreview UTF-8 boundary and tilde fence closure
	utf8Src := "~~~txt\n" + strings.Repeat("é", 700) + "\n~~~"
	trunc := truncateMarkdownPreview(utf8Src, 12, 100)
	if !utf8.ValidString(trunc) {
		t.Fatalf("expected valid UTF-8 after byte truncation")
	}
	if !strings.HasSuffix(trunc, "\n~~~") {
		t.Fatalf("expected unclosed tilde fence to be closed, got %q", trunc)
	}

	// 4. Test dropdown menu and badge enhancement in Node VM
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	nodeScript := fmt.Sprintf(`
const vm = require("vm");
const fs = require("fs");

let fetchUrl = "";
const winListeners = {};
const docListeners = {};

function createMockElement(tag) {
  const children = [];
  const attrs = {};
  const classes = new Set();
  const listeners = {};

  const el = {
    tagName: tag.toUpperCase(),
    style: {},
    parentNode: null,
    childNodes: children,
    listeners: listeners,
    _rect: { top: 100, bottom: 120, left: 200, right: 300, width: 100, height: 20 },
    getAttribute(name) { return attrs[name] !== undefined ? attrs[name] : null; },
    setAttribute(name, val) { attrs[name] = String(val); },
    hasAttribute(name) { return attrs[name] !== undefined; },
    removeAttribute(name) { delete attrs[name]; },
    appendChild(child) {
      child.parentNode = el;
      children.push(child);
      return child;
    },
    removeChild(child) {
      const idx = children.indexOf(child);
      if (idx !== -1) {
        children.splice(idx, 1);
        child.parentNode = null;
      }
      return child;
    },
    addEventListener(ev, fn) {
      if (!listeners[ev]) listeners[ev] = [];
      listeners[ev].push(fn);
    },
    removeEventListener(ev, fn) {
      if (listeners[ev]) listeners[ev] = listeners[ev].filter(f => f !== fn);
    },
    getBoundingClientRect() { return el._rect; },
    closest(sel) {
      let cur = el;
      while (cur) {
        if (sel === "#changeCommentCounts" && cur.id === "changeCommentCounts") return cur;
        if (sel === ".unres-item-row" && cur.classList && cur.classList.contains("unres-item-row")) return cur;
        if (sel.includes("threads-menu-trigger") && cur.classList && cur.classList.contains("threads-menu-trigger")) return cur;
        cur = cur.parentNode;
      }
      return null;
    }
  };

  Object.defineProperty(el, "className", {
    get() { return Array.from(classes).join(" "); },
    set(v) {
      classes.clear();
      if (v) {
        String(v).trim().split(/\s+/).forEach(c => { if (c) classes.add(c); });
      }
    }
  });

  el.classList = {
    add(...cls) {
      cls.forEach(c => classes.add(c));
    },
    remove(...cls) {
      cls.forEach(c => classes.delete(c));
    },
    contains(c) { return classes.has(c); }
  };

  Object.defineProperty(el, "textContent", {
    get() {
      if (children.length > 0) return children.map(c => c.textContent || "").join("");
      return el._text || "";
    },
    set(v) {
      children.length = 0;
      el._text = v;
    }
  });

  Object.defineProperty(el, "isConnected", {
    get() {
      let cur = el;
      while (cur) {
        if (cur.tagName === "BODY") return true;
        cur = cur.parentNode;
      }
      return false;
    }
  });

  Object.defineProperty(el, "innerHTML", {
    get() { return el._html || el.textContent; },
    set(v) {
      el._html = v;
      while (children.length > 0) {
        const ch = children.pop();
        ch.parentNode = null;
      }
      const spanRe = /<span\s+class="([^"]+)">([^<]*)<\/span>/g;
      let m;
      while ((m = spanRe.exec(String(v || ""))) !== null) {
        const sp = createMockElement("SPAN");
        sp.className = m[1];
        sp.textContent = m[2];
        el.appendChild(sp);
      }
    }
  });

  el.querySelector = (sel) => {
    const cleanSel = sel.replace(/^#changeCommentCounts\s+/, "");
    const attrM = cleanSel.match(/\[data-status="([^"]+)"\]/);
    const attrStatus = attrM ? attrM[1] : "";
    const baseSel = cleanSel.replace(/\[[^\]]+\]/g, "");
    for (const c of children) {
      const classMatch = !baseSel || (baseSel.startsWith(".") && c.classList && c.classList.contains(baseSel.slice(1))) || (baseSel.startsWith("#") && c.id === baseSel.slice(1));
      const attrMatch = !attrStatus || (c.getAttribute && c.getAttribute("data-status") === attrStatus);
      if (classMatch && attrMatch) return c;
      if (c.querySelector) {
        const found = c.querySelector(cleanSel);
        if (found) return found;
      }
    }
    return null;
  };

  el.querySelectorAll = (sel) => {
    const res = [];
    for (const c of children) {
      if (sel.includes("threads-menu-trigger") && c.classList && c.classList.contains("threads-menu-trigger")) {
        if (!res.includes(c)) res.push(c);
      }
      if (sel.includes("unresolved-threads-trigger") && c.classList && c.classList.contains("unresolved-threads-trigger")) {
        if (!res.includes(c)) res.push(c);
      }
      if (sel.includes("resolved-threads-trigger") && c.classList && c.classList.contains("resolved-threads-trigger")) {
        if (!res.includes(c)) res.push(c);
      }
      if (c.querySelectorAll) {
        for (const sub of c.querySelectorAll(sel)) {
          if (!res.includes(sub)) res.push(sub);
        }
      }
    }
    return res;
  };

  return el;
}

const body = createMockElement("BODY");
const countsEl = createMockElement("DIV");
countsEl.id = "changeCommentCounts";
countsEl.setAttribute("data-repo", %q);
countsEl.setAttribute("data-change", %q);

const unresBadge = createMockElement("SPAN");
unresBadge.className = "badge badge-count-unresolved";
unresBadge.textContent = "1 unresolved";
countsEl.appendChild(unresBadge);

const resBadge = createMockElement("SPAN");
resBadge.className = "badge badge-count-resolved";
resBadge.textContent = "1 resolved";
countsEl.appendChild(resBadge);

body.appendChild(countsEl);

const doc = {
  body: body,
  documentElement: {
    classList: { contains() { return false; }, add() {}, remove() {} },
    style: { setProperty() {}, getPropertyValue() { return ""; } },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {}
  },
  getElementById(id) {
    if (id === "changeCommentCounts") return countsEl;
    return null;
  },
  createElement(tag) { return createMockElement(tag); },
  createTextNode(txt) {
    return { nodeType: 3, textContent: txt, parentNode: null };
  },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (docListeners[event]) docListeners[event] = docListeners[event].filter(f => f !== fn);
  },
  querySelector(sel) {
    if (sel === "#changeCommentCounts") return countsEl;
    if (sel === ".unresolved-threads-menu") return body.querySelector(".unresolved-threads-menu");
    return countsEl.querySelector(sel);
  },
  querySelectorAll(sel) {
    if (sel.includes("#changeCommentCounts")) return countsEl.querySelectorAll(sel);
    return [];
  }
};

const win = {
  document: doc,
  location: { search: "", hash: "", href: "" },
  addEventListener(ev, fn) {
    if (!winListeners[ev]) winListeners[ev] = [];
    winListeners[ev].push(fn);
  },
  removeEventListener(ev, fn) {},
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; },
  fetch(url, opts) {
    fetchUrl = url;
    if (win._customFetch) return win._customFetch(url, opts);
    if (url.includes("status=resolved")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          threads: [
            { id: 200, path: "main.go", line: 2, end_line: 2, author: "author2", preview: "fixed issue", preview_html: "<p class=\"md-p\"><strong>fixed</strong> <code class=\"md-inline-code\">issue</code></p><p class=\"md-p\">resolved note</p>" }
          ]
        })
      });
    }
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        threads: [
          { id: 100, path: "main.go", line: 1, end_line: 1, author: "author1", preview: "open question" }
        ]
      })
    });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  fetch: win.fetch,
  NodeFilter: { SHOW_TEXT: 4 },
  Node: { TEXT_NODE: 3, ELEMENT_NODE: 1 }
});

const appCode = fs.readFileSync("static/app.js", "utf8");
vm.runInContext(appCode, ctx);

(async () => {
  // 1. Verify enhanceChangeUnresolvedBadge enhanced both badges
  win.enhanceChangeUnresolvedBadge();

  if (!unresBadge.classList.contains("unresolved-threads-trigger") || !unresBadge.classList.contains("threads-menu-trigger")) {
    throw new Error("unresBadge missing menu trigger classes");
  }
  if (unresBadge.getAttribute("data-status") !== "unresolved" || unresBadge.getAttribute("role") !== "button") {
    throw new Error("unresBadge missing data-status or role attributes");
  }
  if (!unresBadge.textContent.includes("1 unresolved ▾")) {
    throw new Error("unresBadge text not formatted with chevron: " + unresBadge.textContent);
  }

  if (!resBadge.classList.contains("resolved-threads-trigger") || !resBadge.classList.contains("threads-menu-trigger")) {
    throw new Error("resBadge missing menu trigger classes");
  }
  if (resBadge.getAttribute("data-status") !== "resolved" || resBadge.getAttribute("role") !== "button") {
    throw new Error("resBadge missing data-status or role attributes");
  }
  if (!resBadge.textContent.includes("1 resolved ▾")) {
    throw new Error("resBadge text not formatted with chevron: " + resBadge.textContent);
  }

  // 2. Open resolved threads dropdown menu
  win.toggleUnresolvedMenu(resBadge);

  if (resBadge.getAttribute("aria-expanded") !== "true") {
    throw new Error("expected resBadge aria-expanded to be true");
  }
  if (!fetchUrl.includes("status=resolved")) {
    throw new Error("expected fetchUrl to contain status=resolved, got: " + fetchUrl);
  }

  // Allow promise to resolve
  await new Promise(r => setTimeout(r, 20));

  const menu = body.childNodes.find(c => c.className && c.className.includes("unresolved-threads-menu"));
  if (!menu) throw new Error("menu element not added to body");
  if (!menu.innerHTML.includes("data-thread-id=\"200\"") || !menu.innerHTML.includes("fixed")) {
    throw new Error("menu HTML does not contain resolved thread 200: " + menu.innerHTML);
  }
  if (!menu.innerHTML.includes("unres-item-loc-line") || !menu.innerHTML.includes("unres-item-author")) {
    throw new Error("menu HTML missing unres-item-loc-line or unres-item-author");
  }
  if (!menu.innerHTML.includes("unres-item-preview markdown-body") || !menu.innerHTML.includes("<strong>fixed</strong>")) {
    throw new Error("menu HTML missing markdown-body or preview_html rendered content: " + menu.innerHTML);
  }

  // 3. Close menu
  win.closeUnresolvedMenu();
  if (resBadge.getAttribute("aria-expanded") !== "false") {
    throw new Error("expected resBadge aria-expanded to be false after close");
  }
  if (menu.style.display !== "none") {
    throw new Error("expected menu.style.display to be none after close");
  }

  // 4. Stale-response race guard: slow unresolved fetch followed by fast resolved fetch
  let resolveSlowUnres;
  win._customFetch = (url) => {
    if (url.includes("status=unresolved")) {
      return new Promise(resolve => { resolveSlowUnres = resolve; });
    }
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        threads: [{ id: 201, path: "main.go", line: 9, end_line: 9, author: "fastAuthor", preview: "fast resolved" }]
      })
    });
  };
  win.toggleUnresolvedMenu(unresBadge);
  win.toggleUnresolvedMenu(resBadge);
  await new Promise(r => setTimeout(r, 20));
  if (!menu.innerHTML.includes("data-thread-id=\"201\"")) {
    throw new Error("expected fast resolved thread 201 to be shown, got: " + menu.innerHTML);
  }
  // Now resolve the stale slow unresolved request; must NOT overwrite resolved menu
  resolveSlowUnres({
    ok: true,
    json: () => Promise.resolve({
      threads: [{ id: 999, path: "stale.go", line: 1, author: "stale", preview: "stale" }]
    })
  });
  await new Promise(r => setTimeout(r, 20));
  if (menu.innerHTML.includes("data-thread-id=\"999\"") || !menu.innerHTML.includes("data-thread-id=\"201\"")) {
    throw new Error("stale unresolved fetch overwrote resolved menu: " + menu.innerHTML);
  }

  // 5. Close-while-in-flight guard: closing menu cancels pending fetch updates
  win.closeUnresolvedMenu();
  let resolveAbortedLate;
  win._customFetch = () => new Promise(resolve => { resolveAbortedLate = resolve; });
  win.toggleUnresolvedMenu(unresBadge);
  win.closeUnresolvedMenu();
  resolveAbortedLate({
    ok: true,
    json: () => Promise.resolve({
      threads: [{ id: 888, path: "late.go", line: 1, author: "late", preview: "late" }]
    })
  });
  await new Promise(r => setTimeout(r, 20));
  if (menu.style.display !== "none" || menu.innerHTML.includes("data-thread-id=\"888\"")) {
    throw new Error("late fetch reopened or populated closed menu");
  }

  // 6. Viewport clamping (right edge and bottom flip-above)
  win._customFetch = null;
  menu._rect = { top: 0, left: 0, width: 320, height: 150, right: 320, bottom: 150 };
  unresBadge._rect = { top: 100, bottom: 120, left: 900, right: 980, width: 80, height: 20 };
  win.toggleUnresolvedMenu(unresBadge);
  await new Promise(r => setTimeout(r, 20));
  const clampedLeft = parseFloat(menu.style.left);
  if (!(clampedLeft <= 1000 - 320 - 12)) {
    throw new Error("expected right-edge clamped left <= 668, got: " + menu.style.left);
  }

  win.closeUnresolvedMenu();
  unresBadge._rect = { top: 740, bottom: 760, left: 100, right: 180, width: 80, height: 20 };
  menu._rect = { top: 0, left: 0, width: 300, height: 220, right: 300, bottom: 220 };
  win.toggleUnresolvedMenu(unresBadge);
  await new Promise(r => setTimeout(r, 20));
  const flippedTop = parseFloat(menu.style.top);
  if (!(flippedTop < 740)) {
    throw new Error("expected menu near bottom to flip above trigger (top < 740), got: " + menu.style.top);
  }

  // 7. Count badge re-render preserves aria-expanded and re-anchors live trigger while active count > 0
  if (menu.getAttribute("data-active-status") !== "unresolved") {
    throw new Error("expected data-active-status='unresolved', got: " + menu.getAttribute("data-active-status"));
  }
  win.updateCommentCountDOM(null, null, { unresolved: 2, resolved: 1, draft: 0 });
  const liveUnresAfterUpdate = countsEl.querySelector(".badge-count-unresolved");
  if (!liveUnresAfterUpdate || liveUnresAfterUpdate === unresBadge) {
    throw new Error("expected countsEl to have replaced unresBadge");
  }
  if (liveUnresAfterUpdate.getAttribute("aria-expanded") !== "true") {
    throw new Error("expected liveUnresAfterUpdate aria-expanded to remain true, got: " + liveUnresAfterUpdate.getAttribute("aria-expanded"));
  }
  if (menu.style.display === "none") {
    throw new Error("expected menu to remain open while unresolved count > 0");
  }

  // 8. In-flight fetch re-anchors onto replacement live badge when count update occurs mid-request
  win.closeUnresolvedMenu();
  if (menu.getAttribute("data-active-status") !== null) {
    throw new Error("expected closeUnresolvedMenu to remove data-active-status");
  }
  let resolveMidUpdate;
  win._customFetch = () => new Promise(resolve => { resolveMidUpdate = resolve; });
  const triggerBeforeUpdate = countsEl.querySelector(".badge-count-unresolved");
  triggerBeforeUpdate._rect = { top: 100, bottom: 120, left: 40, right: 120, width: 80, height: 20 };
  win.toggleUnresolvedMenu(triggerBeforeUpdate);
  win.updateCommentCountDOM(null, null, { unresolved: 3, resolved: 1, draft: 0 });
  const liveTriggerMid = countsEl.querySelector(".badge-count-unresolved");
  liveTriggerMid._rect = { top: 100, bottom: 120, left: 260, right: 340, width: 80, height: 20 };
  menu._rect = { top: 0, left: 0, width: 200, height: 150, right: 200, bottom: 150 };
  resolveMidUpdate({
    ok: true,
    json: () => Promise.resolve({
      threads: [{ id: 301, path: "mid.go", line: 7, author: "mid", preview: "updated" }]
    })
  });
  await new Promise(r => setTimeout(r, 20));
  if (menu.style.display === "none" || !menu.innerHTML.includes("data-thread-id=\"301\"")) {
    throw new Error("expected in-flight fetch to render onto open menu after live badge replacement");
  }
  if (parseFloat(menu.style.left) !== 260) {
    throw new Error("expected menu to reposition at live trigger left=260, got: " + menu.style.left);
  }

  // 9. Count badge update dropping active status count to 0 closes menu cleanly
  win.updateCommentCountDOM(null, null, { unresolved: 0, resolved: 1, draft: 0 });
  if (menu.style.display !== "none") {
    throw new Error("expected menu to close when unresolved count drops to 0");
  }
  if (menu.getAttribute("data-active-status") !== null) {
    throw new Error("expected data-active-status removed when menu auto-closes");
  }
  const remainingRes = countsEl.querySelector(".badge-count-resolved");
  if (!remainingRes || remainingRes.getAttribute("aria-expanded") !== "false") {
    throw new Error("expected remaining resolved trigger aria-expanded to be false");
  }

  console.log("OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`, rec.Slug, c.Key)

	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test in TestResolvedThreadsAPIAndMenu failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected node test to print OK, got: %s", string(out))
	}

	// Verify style.css rules for unresolved threads menu and preview clamping
	cssBytes, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	cssStr := string(cssBytes)
	if !strings.Contains(cssStr, "-webkit-line-clamp: 5") {
		t.Errorf("expected style.css to contain -webkit-line-clamp: 5")
	}
	if !strings.Contains(cssStr, "84ch") || !strings.Contains(cssStr, "86ch") {
		t.Errorf("expected style.css to contain 84ch and 86ch width limits for comment preview menu")
	}
}

func TestDraftThreadsAPIAndMenu(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := vcs.Open(repoDir, srv.cache)
	changes, _ := repo.Changes()
	c := changes[0]

	// 1. Brand-new draft thread
	thDraftRoot, mDraftRoot, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "main.go", "R", 1, 1, "anchor", "draftAuthor", "**draft** `root`\n\nsecond line", true)
	if err != nil {
		t.Fatal(err)
	}
	// 2. Published resolved thread with two draft replies
	thResolvedWithDraft, _, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "main.go", "R", 2, 2, "anchor", "publishedAuthor", "published root comment", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetResolved(thResolvedWithDraft.ID, true); err != nil {
		t.Fatal(err)
	}
	mReply1, err := d.AddMessage(thResolvedWithDraft.ID, "replyDraftAuthor", "*draft* reply body", true)
	if err != nil {
		t.Fatal(err)
	}
	mReply2, err := d.AddMessage(thResolvedWithDraft.ID, "secondDraftAuthor", "second draft reply", true)
	if err != nil {
		t.Fatal(err)
	}
	// 3. Published unresolved thread with no drafts (must not appear)
	if _, _, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "main.go", "R", 3, 3, "anchor", "pubOnly", "published only", false); err != nil {
		t.Fatal(err)
	}

	allThreads, err := d.ThreadsForChange(rec.ID, c.Key)
	if err != nil {
		t.Fatal(err)
	}
	counts := engine.ComputeCommentCounts(allThreads)
	if counts.Draft != 3 {
		t.Fatalf("expected ComputeCommentCounts draft=3, got %d", counts.Draft)
	}

	listURL := fmt.Sprintf("%s/api/threads/list?status=draft&repo=%s&change=%s", ts.URL, rec.Slug, c.Key)
	resp, err := http.Get(listURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/threads/list?status=draft failed: status=%v err=%v", resp.StatusCode, err)
	}
	var draftRes struct {
		Threads []struct {
			ID          int64  `json:"id"`
			MessageID   int64  `json:"message_id"`
			Line        int    `json:"line"`
			Path        string `json:"path"`
			HasDraft    bool   `json:"has_draft"`
			Author      string `json:"author"`
			Preview     string `json:"preview"`
			PreviewHTML string `json:"preview_html"`
		} `json:"threads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&draftRes); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(draftRes.Threads) != counts.Draft {
		t.Fatalf("expected %d draft items matching counts.Draft, got %d (%+v)", counts.Draft, len(draftRes.Threads), draftRes.Threads)
	}
	if draftRes.Threads[0].ID != thDraftRoot.ID || draftRes.Threads[0].MessageID != mDraftRoot.ID || draftRes.Threads[0].Author != "draftAuthor" || !draftRes.Threads[0].HasDraft {
		t.Fatalf("unexpected first draft item: %+v", draftRes.Threads[0])
	}
	if !strings.Contains(draftRes.Threads[0].PreviewHTML, "<strong>draft</strong>") || !strings.Contains(draftRes.Threads[0].PreviewHTML, `<code class="md-inline-code">root</code>`) {
		t.Errorf("expected brand-new draft preview_html to render Markdown, got %q", draftRes.Threads[0].PreviewHTML)
	}
	if draftRes.Threads[1].ID != thResolvedWithDraft.ID || draftRes.Threads[1].MessageID != mReply1.ID || draftRes.Threads[1].Author != "replyDraftAuthor" {
		t.Errorf("expected second item to preview draft reply on resolved thread (id=%d, msg=%d, author=replyDraftAuthor), got %+v", thResolvedWithDraft.ID, mReply1.ID, draftRes.Threads[1])
	}
	if !strings.Contains(draftRes.Threads[1].PreviewHTML, "<em>draft</em> reply body") || strings.Contains(draftRes.Threads[1].PreviewHTML, "published root comment") {
		t.Errorf("expected draft reply preview rather than published root comment, got %q", draftRes.Threads[1].PreviewHTML)
	}
	if draftRes.Threads[2].ID != thResolvedWithDraft.ID || draftRes.Threads[2].MessageID != mReply2.ID || draftRes.Threads[2].Author != "secondDraftAuthor" || draftRes.Threads[2].Preview != "second draft reply" {
		t.Errorf("expected third item for second draft reply in same thread (msg=%d), got %+v", mReply2.ID, draftRes.Threads[2])
	}

	// Non-draft status filters omit message_id from JSON output
	for _, st := range []string{"unresolved", "resolved"} {
		u := fmt.Sprintf("%s/api/threads/list?status=%s&repo=%s&change=%s", ts.URL, st, rec.Slug, c.Key)
		r, err := http.Get(u)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s failed: status=%v err=%v", u, r.StatusCode, err)
		}
		rawBytes, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(rawBytes), `"message_id"`) {
			t.Errorf("did not expect message_id for status=%s, got: %s", st, string(rawBytes))
		}
	}

	// Publishing drafts clears ?status=draft results
	pubPayload, _ := json.Marshal(map[string]string{"repo": rec.Slug, "change": c.Key})
	resp, err = http.Post(ts.URL+"/api/publish", "application/json", bytes.NewReader(pubPayload))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/publish failed: status=%v err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()

	resp, err = http.Get(listURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/threads/list?status=draft after publish failed: status=%v err=%v", resp.StatusCode, err)
	}
	var afterPub struct {
		Threads []any `json:"threads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&afterPub); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(afterPub.Threads) != 0 {
		t.Fatalf("expected 0 draft items after publish, got %d", len(afterPub.Threads))
	}

	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	nodeScript := fmt.Sprintf(`
const vm = require("vm");
const fs = require("fs");

let fetchUrl = "";
const docListeners = {};

function createMockElement(tag) {
  const children = [];
  const attrs = {};
  const classes = new Set();
  const listeners = {};

  const el = {
    tagName: tag.toUpperCase(),
    style: {},
    parentNode: null,
    childNodes: children,
    _scrolled: false,
    _listeners: listeners,
    _rect: { top: 100, bottom: 120, left: 200, right: 300, width: 100, height: 20 },
    getAttribute(name) { return attrs[name] !== undefined ? attrs[name] : null; },
    setAttribute(name, val) { attrs[name] = String(val); },
    hasAttribute(name) { return attrs[name] !== undefined; },
    removeAttribute(name) { delete attrs[name]; },
    scrollIntoView() { el._scrolled = true; },
    appendChild(child) {
      child.parentNode = el;
      children.push(child);
      return child;
    },
    insertBefore(newChild, refChild) {
      newChild.parentNode = el;
      const idx = refChild ? children.indexOf(refChild) : -1;
      if (idx >= 0) children.splice(idx, 0, newChild);
      else children.push(newChild);
      return newChild;
    },
    removeChild(child) {
      const idx = children.indexOf(child);
      if (idx >= 0) children.splice(idx, 1);
      child.parentNode = null;
      return child;
    },
    remove() {
      if (el.parentNode && el.parentNode.removeChild) {
        el.parentNode.removeChild(el);
      }
    },
    get options() { return children; },
    contains(other) {
      if (other === el) return true;
      for (const c of children) {
        if (c === other || (c.contains && c.contains(other))) return true;
      }
      return false;
    },
    addEventListener(event, fn) {
      if (!listeners[event]) listeners[event] = [];
      listeners[event].push(fn);
    },
    removeEventListener() {},
    dispatchEvent(eventObj) {
      for (const fn of (listeners[eventObj.type] || [])) {
        fn(eventObj);
      }
    },
    getBoundingClientRect() { return el._rect; },
    closest(sel) {
      let cur = el;
      while (cur) {
        if (sel === "#changeCommentCounts" && cur.id === "changeCommentCounts") return cur;
        if (sel === ".diff-file-section" && cur.classList && cur.classList.contains("diff-file-section")) return cur;
        if (sel === ".unres-item-row" && cur.classList && cur.classList.contains("unres-item-row")) return cur;
        if (sel === "tr.thread-row" && cur.tagName === "TR" && cur.classList && cur.classList.contains("thread-row")) return cur;
        if (sel === ".file-threads-container" && cur.classList && cur.classList.contains("file-threads-container")) return cur;
        if (sel.includes("draft-threads-trigger") && cur.classList && cur.classList.contains("draft-threads-trigger")) return cur;
        if (sel.includes("threads-menu-trigger") && cur.classList && cur.classList.contains("threads-menu-trigger")) return cur;
        cur = cur.parentNode;
      }
      return null;
    }
  };

  Object.defineProperty(el, "className", {
    get() { return Array.from(classes).join(" "); },
    set(v) {
      classes.clear();
      if (v) {
        String(v).trim().split(/\s+/).forEach(c => { if (c) classes.add(c); });
      }
    }
  });

  el.classList = {
    add(...cls) { cls.forEach(c => classes.add(c)); },
    remove(...cls) { cls.forEach(c => classes.delete(c)); },
    contains(c) { return classes.has(c); }
  };

  Object.defineProperty(el, "textContent", {
    get() {
      if (children.length > 0) return children.map(c => c.textContent || "").join("");
      return el._text || "";
    },
    set(v) {
      children.length = 0;
      el._text = v;
    }
  });

  Object.defineProperty(el, "isConnected", {
    get() {
      let cur = el;
      while (cur) {
        if (cur.tagName === "BODY") return true;
        cur = cur.parentNode;
      }
      return false;
    }
  });

  Object.defineProperty(el, "innerHTML", {
    get() { return el._html || el.textContent; },
    set(v) {
      el._html = v;
      while (children.length > 0) {
        const ch = children.pop();
        ch.parentNode = null;
      }
      const s = String(v || "");
      const spanRe = /<span\s+class="([^"]+)">([^<]*)<\/span>/g;
      let m;
      while ((m = spanRe.exec(s)) !== null) {
        const sp = createMockElement("SPAN");
        sp.className = m[1];
        sp.textContent = m[2];
        el.appendChild(sp);
      }
      const rowRe = /<div\s+class="([^"]*unres-item-row[^"]*)"([^>]*)>/g;
      while ((m = rowRe.exec(s)) !== null) {
        const row = createMockElement("DIV");
        row.className = m[1];
        const attrRe = /([a-z-]+)="([^"]*)"/g;
        let am;
        while ((am = attrRe.exec(m[2])) !== null) {
          row.setAttribute(am[1], am[2]);
        }
        el.appendChild(row);
      }
      const cardRe = /<div\s+id="(thread-\d+)"\s+class="([^"]*)"[^>]*>/g;
      while ((m = cardRe.exec(s)) !== null) {
        const card = createMockElement("DIV");
        card.id = m[1];
        card.className = m[2];
        const msgRe = /<div\s+id="(msg-\d+)"\s+class="([^"]*)"[^>]*><\/div>/g;
        let mm;
        while ((mm = msgRe.exec(s)) !== null) {
          const msg = createMockElement("DIV");
          msg.id = mm[1];
          msg.className = mm[2];
          card.appendChild(msg);
        }
        el.appendChild(card);
      }
    }
  });

  el.querySelector = (sel) => {
    const cleanSel = sel.replace(/^#changeCommentCounts\s+/, "");
    const attrM = cleanSel.match(/\[data-status="([^"]+)"\]/);
    const attrStatus = attrM ? attrM[1] : "";
    const baseSel = cleanSel.replace(/\[[^\]]+\]/g, "");
    for (const c of children) {
      const classMatch = !baseSel || (baseSel.startsWith(".") && c.classList && c.classList.contains(baseSel.slice(1))) || (baseSel.startsWith("#") && c.id === baseSel.slice(1));
      const attrMatch = !attrStatus || (c.getAttribute && c.getAttribute("data-status") === attrStatus);
      if (classMatch && attrMatch) return c;
      if (c.querySelector) {
        const found = c.querySelector(cleanSel);
        if (found) return found;
      }
    }
    return null;
  };

  el.querySelectorAll = (sel) => {
    const res = [];
    for (const c of children) {
      if ((sel === ".diff-file-section" && c.classList && c.classList.contains("diff-file-section")) ||
          (sel.includes("threads-menu-trigger") && c.classList && c.classList.contains("threads-menu-trigger")) ||
          (sel.includes("draft-threads-trigger") && c.classList && c.classList.contains("draft-threads-trigger"))) {
        if (!res.includes(c)) res.push(c);
      }
      if (c.querySelectorAll) {
        for (const sub of c.querySelectorAll(sel)) {
          if (!res.includes(sub)) res.push(sub);
        }
      }
    }
    return res;
  };

  return el;
}

const body = createMockElement("BODY");
const countsEl = createMockElement("DIV");
countsEl.id = "changeCommentCounts";
countsEl.setAttribute("data-repo", %q);
countsEl.setAttribute("data-change", %q);
countsEl.setAttribute("data-base", "parent");
countsEl.setAttribute("data-target", "live");

const draftBadge = createMockElement("SPAN");
draftBadge.className = "badge badge-draft";
draftBadge.textContent = "2 draft";
countsEl.appendChild(draftBadge);
body.appendChild(countsEl);

const nonHeaderDraftBadge = createMockElement("SPAN");
nonHeaderDraftBadge.className = "badge badge-draft badge-xs";
nonHeaderDraftBadge.textContent = "draft";
body.appendChild(nonHeaderDraftBadge);

const outsideEl = createMockElement("DIV");
body.appendChild(outsideEl);

function findById(node, id) {
  if (!node) return null;
  if (node.id === id) return node;
  for (const c of (node.childNodes || [])) {
    const found = findById(c, id);
    if (found) return found;
  }
  return null;
}

const sessionStore = new Map();
const historyCalls = [];

const doc = {
  body: body,
  documentElement: {
    classList: { contains() { return false; }, add() {}, remove() {} },
    style: { setProperty() {}, getPropertyValue() { return ""; } },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {}
  },
  getElementById(id) { return findById(body, id); },
  createElement(tag) { return createMockElement(tag); },
  createTextNode(txt) { return { nodeType: 3, textContent: txt, parentNode: null }; },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener() {},
  querySelector(sel) {
    if (sel === "#changeCommentCounts") return countsEl;
    if (sel === ".unresolved-threads-menu") return body.querySelector(".unresolved-threads-menu");
    if (sel === ".diff-all-files") return body.querySelector(".diff-all-files");
    if (sel === ".diff-file-section") return body.querySelector(".diff-file-section");
    return countsEl.querySelector(sel);
  },
  querySelectorAll(sel) {
    if (sel.includes("#changeCommentCounts")) return countsEl.querySelectorAll(sel);
    if (sel === ".diff-file-section") return body.querySelectorAll(".diff-file-section");
    return [];
  }
};

let reloadCalls = 0;
const win = {
  document: doc,
  location: { pathname: "/r/" + %q + "/c/" + %q, search: "?diffs=all&mode=unified&ctx=all", hash: "", href: "", reload() { reloadCalls++; } },
  history: {
    replaceState(_state, _title, url) {
      historyCalls.push(url);
    }
  },
  addEventListener() {},
  removeEventListener() {},
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: {
    getItem(k) { return sessionStore.has(k) ? sessionStore.get(k) : null; },
    setItem(k, v) { sessionStore.set(k, String(v)); }
  },
  getSelection() { return { isCollapsed: true }; },
  fetch(url, opts) {
    fetchUrl = url;
    if (win._customFetch) return win._customFetch(url, opts);
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        threads: [
          { id: 501, message_id: 801, path: "main.go", line: 10, end_line: 10, has_draft: true, stale: true, author: "draftAuthor", preview: "draft root", preview_html: "<p><strong>draft</strong></p>" }
        ]
      })
    });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  history: win.history,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  URLSearchParams: URLSearchParams,
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  fetch: win.fetch,
  NodeFilter: { SHOW_TEXT: 4 },
  Node: { TEXT_NODE: 3, ELEMENT_NODE: 1 }
});

vm.runInContext(fs.readFileSync("static/app.js", "utf8"), ctx);

(async () => {
  win.enhanceChangeUnresolvedBadge();

  if (!draftBadge.classList.contains("draft-threads-trigger") || !draftBadge.classList.contains("threads-menu-trigger")) {
    throw new Error("draftBadge missing menu trigger classes");
  }
  if (draftBadge.getAttribute("data-status") !== "draft" || draftBadge.getAttribute("role") !== "button" || draftBadge.getAttribute("tabindex") !== "0") {
    throw new Error("draftBadge missing expected trigger attributes");
  }
  if (draftBadge.textContent !== "2 draft ▾") {
    throw new Error("expected '2 draft ▾', got " + draftBadge.textContent);
  }
  if (nonHeaderDraftBadge.classList.contains("threads-menu-trigger") || nonHeaderDraftBadge.getAttribute("data-status") !== null || nonHeaderDraftBadge.textContent !== "draft") {
    throw new Error("non-header .badge-draft should remain untouched");
  }

  // Test keyboard Enter/Space opens draft menu with loading copy
  let resolveSlowDraft;
  win._customFetch = (url) => {
    fetchUrl = url;
    return new Promise(resolve => { resolveSlowDraft = resolve; });
  };
  for (const fn of (docListeners.keydown || [])) {
    fn({ key: "Enter", target: draftBadge, preventDefault() {} });
  }
  const menu = body.childNodes.find(c => c.className && c.className.includes("unresolved-threads-menu"));
  if (!menu || menu.style.display !== "block" || menu.getAttribute("data-active-status") !== "draft") {
    throw new Error("expected draft menu open via Enter key");
  }
  if (!menu.innerHTML.includes("Loading draft comments…")) {
    throw new Error("expected loading draft comments text, got: " + menu.innerHTML);
  }
  if (!fetchUrl.includes("status=draft")) {
    throw new Error("expected status=draft fetch URL, got: " + fetchUrl);
  }
  resolveSlowDraft({
    ok: true,
    json: () => Promise.resolve({
      threads: [
        { id: 501, message_id: 801, path: "main.go", line: 10, end_line: 10, has_draft: true, stale: true, author: "draftAuthor", preview: "draft root", preview_html: "<p><strong>draft</strong></p>" }
      ]
    })
  });
  await new Promise(r => setTimeout(r, 20));
  if (menu.innerHTML.includes("badge-draft")) {
    throw new Error("did not expect redundant inner draft badge in draft menu: " + menu.innerHTML);
  }
  if (!menu.innerHTML.includes("stale") || !menu.innerHTML.includes("draftAuthor")) {
    throw new Error("expected stale badge and author in draft menu: " + menu.innerHTML);
  }
  if (!menu.innerHTML.includes('data-message-id="801"')) {
    throw new Error("expected data-message-id attribute in draft menu: " + menu.innerHTML);
  }

  // Outside click closes menu, Space reopens it, Escape closes it
  for (const fn of (docListeners.click || [])) {
    fn({ target: outsideEl, preventDefault() {}, stopPropagation() {} });
  }
  if (menu.style.display !== "none" || draftBadge.getAttribute("aria-expanded") !== "false") {
    throw new Error("expected outside click to close draft menu");
  }
  win._customFetch = () => Promise.resolve({ ok: true, json: () => Promise.resolve({ threads: [] }) });
  for (const fn of (docListeners.keydown || [])) {
    fn({ key: " ", target: draftBadge, preventDefault() {} });
  }
  await new Promise(r => setTimeout(r, 20));
  if (!menu.innerHTML.includes("No draft comments")) {
    throw new Error("expected empty draft menu copy, got: " + menu.innerHTML);
  }
  if (!win.triggerEscapeAction(doc.body) || menu.style.display !== "none") {
    throw new Error("expected Escape to close draft menu");
  }

  // Test multi-draft reply targeting via message_id on click, keydown, and scrollToHashLine
  const diffAll = createMockElement("DIV");
  diffAll.className = "diff-all-files";
  diffAll.setAttribute("data-repo", %q);
  diffAll.setAttribute("data-change", %q);
  body.appendChild(diffAll);

  const secMain = createMockElement("DIV");
  secMain.className = "diff-file-section collapsed";
  secMain.setAttribute("data-file-path", "main.go");
  diffAll.appendChild(secMain);

  const collapseBtn = createMockElement("BUTTON");
  collapseBtn.className = "file-collapse-btn";
  collapseBtn.textContent = "▸";
  secMain.appendChild(collapseBtn);

  const secBody = createMockElement("DIV");
  secBody.className = "diff-file-body";
  secBody.style.display = "none";
  secMain.appendChild(secBody);

  const thread502 = createMockElement("DIV");
  thread502.id = "thread-502";
  thread502.className = "thread-card";
  secBody.appendChild(thread502);

  const msg901 = createMockElement("DIV");
  msg901.id = "msg-901";
  msg901.className = "message-body";
  thread502.appendChild(msg901);

  const msg902 = createMockElement("DIV");
  msg902.id = "msg-902";
  msg902.className = "message-body message-reply";
  thread502.appendChild(msg902);

  win._customFetch = () => Promise.resolve({
    ok: true,
    json: () => Promise.resolve({
      threads: [
        { id: 502, message_id: 901, path: "main.go", line: 20, has_draft: true, author: "a1", preview: "first" },
        { id: 502, message_id: 902, path: "main.go", line: 20, has_draft: true, author: "a2", preview: "second" }
      ]
    })
  });
  for (const fn of (docListeners.click || [])) {
    fn({ target: draftBadge, preventDefault() {}, stopPropagation() {} });
  }
  await new Promise(r => setTimeout(r, 20));
  const menuRows = menu.childNodes.filter(c => c.classList && c.classList.contains("unres-item-row"));
  if (menuRows.length !== 2) {
    throw new Error("expected 2 rendered menu rows, got " + menuRows.length);
  }

  // Click second draft reply row -> uncollapses main.go, saves expanded files, centers msg-902
  menu.dispatchEvent({ type: "click", target: menuRows[1] });
  await new Promise(r => setTimeout(r, 20));
  if (secMain.classList.contains("collapsed") || secBody.style.display !== "block" || collapseBtn.textContent !== "▾") {
    throw new Error("expected jumping to draft reply to uncollapse file section");
  }
  const expandedKey = "review_expanded:" + %q + ":" + %q;
  if (sessionStore.get(expandedKey) !== JSON.stringify(["main.go"])) {
    throw new Error("expected saveExpandedFiles to persist ['main.go'], got: " + sessionStore.get(expandedKey));
  }
  if (!msg902._scrolled || msg901._scrolled || thread502._scrolled) {
    throw new Error("expected exact #msg-902 scrolled into view rather than thread root");
  }
  if (!thread502.classList.contains("jump-target-flash")) {
    throw new Error("expected thread card to flash");
  }
  if (historyCalls[historyCalls.length - 1] !== "#thread-502-msg-902") {
    throw new Error("expected history hash #thread-502-msg-902, got: " + historyCalls[historyCalls.length - 1]);
  }

  // Space keydown on first draft reply row targets msg-901
  msg901._scrolled = false;
  msg902._scrolled = false;
  for (const fn of (docListeners.click || [])) {
    fn({ target: draftBadge, preventDefault() {}, stopPropagation() {} });
  }
  await new Promise(r => setTimeout(r, 20));
  const reopenedRows = menu.childNodes.filter(c => c.classList && c.classList.contains("unres-item-row"));
  menu.dispatchEvent({ type: "keydown", key: " ", target: reopenedRows[0], preventDefault() {} });
  await new Promise(r => setTimeout(r, 20));
  if (!msg901._scrolled || msg902._scrolled || historyCalls[historyCalls.length - 1] !== "#thread-502-msg-901") {
    throw new Error("expected Space keydown on first draft item to center msg-901");
  }

  // scrollToHashLine with #thread-502-msg-902 inside collapsed section
  secMain.classList.add("collapsed");
  secBody.style.display = "none";
  msg901._scrolled = false;
  msg902._scrolled = false;
  win.location.hash = "#thread-502-msg-902";
  await win.scrollToHashLine();
  if (secMain.classList.contains("collapsed") || !msg902._scrolled || msg901._scrolled) {
    throw new Error("expected scrollToHashLine('#thread-502-msg-902') to expand section and scroll #msg-902");
  }

  // scrollToHashLine skips collapsed eager/uncommented sections, loads only commented (or counts-less) lazy sections, and uncollapses ONLY target section
  secMain.classList.add("collapsed");
  secBody.style.display = "none";
  collapseBtn.textContent = "▸";
  sessionStore.set(expandedKey, "[]");
  const makeTestSec = (filePath, lazyUrl, hasCountsEl, badgeClass) => {
    const s = createMockElement("DIV");
    s.className = "diff-file-section collapsed";
    s.setAttribute("data-file-path", filePath);
    if (hasCountsEl) {
      const cc = createMockElement("SPAN");
      cc.className = "comment-counts";
      if (badgeClass) {
        const b = createMockElement("SPAN");
        b.className = "badge " + badgeClass;
        b.textContent = "1";
        cc.appendChild(b);
      }
      s.appendChild(cc);
    }
    const cbtn = createMockElement("BUTTON");
    cbtn.className = "file-collapse-btn";
    cbtn.textContent = "▸";
    s.appendChild(cbtn);
    const bdy = createMockElement("DIV");
    bdy.className = "diff-file-body";
    bdy.style.display = "none";
    if (lazyUrl) bdy.setAttribute("data-lazy-url", lazyUrl);
    s.appendChild(bdy);
    diffAll.appendChild(s);
    return { sec: s, body: bdy, btn: cbtn };
  };
  const secEager = makeTestSec("eager.go", "", true, "");
  const secLazyZero = makeTestSec("zero.go", "/api/diff/file?zero", true, "");
  const secLazyNoCounts = makeTestSec("nocounts.go", "/api/diff/file?nocounts", false, "");
  const secLazyOther = makeTestSec("other.go", "/api/diff/file?other", true, "badge-draft");
  const secLazyTarget = makeTestSec("target.go", "/api/diff/file?target", true, "badge-count-unresolved");
  const secLazyAfter = makeTestSec("after.go", "/api/diff/file?after", true, "badge-draft");

  const lazyFetched = [];
  win._customFetch = (url) => {
    lazyFetched.push(url);
    let html = "<div></div>";
    if (url.includes("other")) {
      html = '<div id="thread-599" class="thread-card"></div>';
    } else if (url.includes("target")) {
      html = '<div id="thread-503" class="thread-card"><div id="msg-903" class="message-body"></div></div>';
    }
    return Promise.resolve({ ok: true, text: () => Promise.resolve(html) });
  };
  win.location.hash = "#thread-503-msg-903";
  await win.scrollToHashLine();

  if (JSON.stringify(lazyFetched) !== JSON.stringify(["/api/diff/file?nocounts", "/api/diff/file?other", "/api/diff/file?target"])) {
    throw new Error("expected lazy fetches ONLY for counts-less and commented sections up to target, got: " + JSON.stringify(lazyFetched));
  }
  for (const untouched of [secMain, secEager.sec, secLazyZero.sec, secLazyNoCounts.sec, secLazyOther.sec, secLazyAfter.sec]) {
    const bdy = untouched.querySelector(".diff-file-body");
    if (!untouched.classList.contains("collapsed") || (bdy && bdy.style.display !== "none")) {
      throw new Error("expected non-target section " + untouched.getAttribute("data-file-path") + " to remain collapsed");
    }
  }
  if (secLazyTarget.sec.classList.contains("collapsed") || secLazyTarget.body.style.display !== "block" || secLazyTarget.btn.textContent !== "▾") {
    throw new Error("expected ONLY target lazy section to uncollapse");
  }
  if (sessionStore.get(expandedKey) !== JSON.stringify(["target.go"])) {
    throw new Error("expected expanded files sessionStorage to record only ['target.go'], got: " + sessionStore.get(expandedKey));
  }
  const t503 = doc.getElementById("thread-503");
  const m903 = doc.getElementById("msg-903");
  if (!t503 || !m903 || !m903._scrolled || !t503.classList.contains("jump-target-flash")) {
    throw new Error("expected target lazy thread-503/msg-903 to be scrolled and flashed");
  }
  diffAll.childNodes.length = 1;

  // Fallback navigation when thread's file is absent from .diff-file-section (reverted/interdiff file), preserving ctx
  win.location.href = "";
  await win.jumpToUnresolvedThread("601", "reverted.go", "955");
  if (!win.location.href.includes("/f/reverted.go?") ||
      !win.location.href.includes("mode=unified") ||
      !win.location.href.includes("ctx=all") ||
      !win.location.href.endsWith("#thread-601-msg-955")) {
    throw new Error("expected interdiff/reverted fallback URL preserving ctx and hash, got: " + win.location.href);
  }

  // Already on /f/ single-file route -> should not redirect when card missing
  win.location.pathname = "/r/" + %q + "/c/" + %q + "/f/reverted.go";
  win.location.href = "unchanged";
  await win.jumpToUnresolvedThread("601", "reverted.go", "955");
  if (win.location.href !== "unchanged") {
    throw new Error("expected no redirect when already on /f/ route, got: " + win.location.href);
  }

  // !hasDiffsAll branch (diffs=list) preserves ctx
  diffAll.remove ? diffAll.remove() : body.childNodes.splice(body.childNodes.indexOf(diffAll), 1);
  win.location.pathname = "/r/" + %q + "/c/" + %q;
  win.location.href = "";
  await win.jumpToUnresolvedThread("701", "sub/other.go", "977");
  if (!win.location.href.includes("/f/sub/other.go?") ||
      !win.location.href.includes("mode=unified") ||
      !win.location.href.includes("ctx=all") ||
      !win.location.href.endsWith("#thread-701-msg-977")) {
    throw new Error("expected !hasDiffsAll jump URL preserving mode and ctx, got: " + win.location.href);
  }

  // Reopen via click, keep open when draft count stays > 0, auto-close when draft drops to 0
  win._customFetch = null;
  for (const fn of (docListeners.click || [])) {
    fn({ target: draftBadge, preventDefault() {}, stopPropagation() {} });
  }
  await new Promise(r => setTimeout(r, 20));
  win.updateCommentCountDOM(null, null, { unresolved: 1, resolved: 1, draft: 1 });
  const liveDraft = countsEl.querySelector(".badge-draft");
  if (!liveDraft || liveDraft.textContent !== "1 draft ▾" || liveDraft.getAttribute("aria-expanded") !== "true" || menu.style.display === "none") {
    throw new Error("expected open draft menu to stay open and re-anchor when draft=1");
  }
  win.updateCommentCountDOM(null, null, { unresolved: 1, resolved: 1, draft: 0 });
  if (menu.style.display !== "none" || menu.getAttribute("data-active-status") !== null) {
    throw new Error("expected open draft menu to auto-close when draft drops to 0");
  }

  // Snapshot badge & cross-snapshot click navigation in header menu
  body.appendChild(diffAll);
  win.location.pathname = "/r/" + %q + "/c/" + %q;
  win.updateCommentCountDOM(null, null, { unresolved: 1, resolved: 1, draft: 1 });
  const activeDraftBadge = countsEl.querySelector(".badge-draft");
  win._customFetch = () => Promise.resolve({
    ok: true,
    json: () => Promise.resolve({
      threads: [
        { id: 811, message_id: 911, path: "main.go", line: 10, has_draft: false, snapshot_num: 1, in_current_view: false, author: "alice", preview: "old snap" }
      ]
    })
  });
  for (const fn of (docListeners.click || [])) {
    fn({ target: activeDraftBadge, preventDefault() {}, stopPropagation() {} });
  }
  await new Promise(r => setTimeout(r, 20));
  if (!menu.innerHTML.includes("Snapshot 1")) {
    throw new Error("expected Snapshot 1 badge in threads menu, got: " + menu.innerHTML);
  }
  const snapRow = menu.childNodes.find(c => c.classList && c.classList.contains("unres-item-row"));
  if (!snapRow || snapRow.getAttribute("data-snapshot-num") !== "1" || snapRow.getAttribute("data-in-current-view") !== "false") {
    throw new Error("expected snapshot data attributes on menu row");
  }
  win.location.href = "";
  menu.dispatchEvent({ type: "click", target: snapRow });
  await new Promise(r => setTimeout(r, 20));
  if (!win.location.href.includes("diffs=all") ||
      !win.location.href.includes("base=parent") ||
      !win.location.href.includes("target=snap%%3A1") ||
      !win.location.href.endsWith("#thread-811-msg-911")) {
    throw new Error("expected cross-snapshot navigation to target=snap:1, got: " + win.location.href);
  }

  // publishDrafts updates pending snapshot badge, base/target selects, and summary Target label
  const changeOverview = createMockElement("DIV");
  changeOverview.setAttribute("data-left-rev", "rev0");
  changeOverview.setAttribute("data-right-rev", "rev1");
  body.appendChild(changeOverview);

  const pendingBadge = createMockElement("SPAN");
  pendingBadge.className = "badge badge-pending";
  pendingBadge.textContent = "Snapshot pending";
  body.appendChild(pendingBadge);

  const targetSel = createMockElement("SELECT");
  targetSel.value = "live";
  const optLiveTarget = createMockElement("OPTION");
  optLiveTarget.value = "live";
  optLiveTarget.textContent = "Live Snapshot (pending)";
  targetSel.appendChild(optLiveTarget);
  body.appendChild(targetSel);

  const baseSel = createMockElement("SELECT");
  baseSel.value = "parent";
  const optParentBase = createMockElement("OPTION");
  optParentBase.value = "parent";
  optParentBase.textContent = "Parent";
  baseSel.appendChild(optParentBase);
  body.appendChild(baseSel);

  const summaryHeader = createMockElement("DIV");
  const targetSpan = createMockElement("SPAN");
  targetSpan.textContent = "Target: Live Snapshot";
  summaryHeader.appendChild(targetSpan);
  body.appendChild(summaryHeader);

  const origDocQS = doc.querySelector;
  doc.querySelector = (sel) => {
    if (sel === "[data-left-rev][data-right-rev]") return changeOverview;
    if (sel === ".change-header-bar .badge-pending") return pendingBadge;
    if (sel === '#diffForm select[name="target"]') return targetSel;
    if (sel === '#diffForm select[name="base"]') return baseSel;
    if (sel === ".file-summary-card .files-header") return summaryHeader;
    return origDocQS(sel);
  };
  summaryHeader.querySelectorAll = () => [targetSpan];

  // Also attach hidden thread cards (inline tr.thread-row and file-level .file-threads-container)
  // to verify publishDrafts removes cards/empty wrappers reported in hidden_thread_ids.
  const threadRowWrap = createMockElement("TR");
  threadRowWrap.className = "thread-row";
  const hiddenInlineCard = createMockElement("DIV");
  hiddenInlineCard.id = "thread-999";
  hiddenInlineCard.className = "thread-card";
  threadRowWrap.appendChild(hiddenInlineCard);
  body.appendChild(threadRowWrap);

  const fileThreadsWrap = createMockElement("DIV");
  fileThreadsWrap.className = "file-threads-container";
  const hiddenFileCard = createMockElement("DIV");
  hiddenFileCard.id = "thread-1000";
  hiddenFileCard.className = "thread-card";
  fileThreadsWrap.appendChild(hiddenFileCard);
  body.appendChild(fileThreadsWrap);

  win._customFetch = () => Promise.resolve({
    ok: true,
    json: () => Promise.resolve({
      published: 1,
      sig: "sig-after-pub",
      change_counts: { unresolved: 1, resolved: 0, draft: 0 },
      file_counts: {},
      thread_html: {},
      hidden_thread_ids: [999, 1000],
      snapshot_created: true,
      snapshot_num: 1,
      snapshots: [{ num: 1 }, { num: 2 }],
      snapshot_pending: false,
      live_rev: "rev1",
      target_desc: "Snapshot 1"
    })
  });
  const pubRepo = %q, pubChange = %q;
  win.publishDrafts(pubRepo, pubChange);
  await new Promise(r => setTimeout(r, 30));
  if (reloadCalls !== 0) {
    throw new Error("expected no reload when live_rev matches rightRev, got " + reloadCalls);
  }
  if (doc.getElementById("thread-999") || threadRowWrap.parentNode !== null ||
      doc.getElementById("thread-1000") || fileThreadsWrap.parentNode !== null) {
    throw new Error("expected publishDrafts hidden_thread_ids to remove thread cards and empty wrappers");
  }
  if (pendingBadge.className !== "badge" || pendingBadge.textContent !== "Snapshot 1") {
    throw new Error("expected pendingBadge updated to Snapshot 1, got class=" + pendingBadge.className + " text=" + pendingBadge.textContent);
  }
  if (optLiveTarget.textContent !== "Live Snapshot") {
    throw new Error("expected live option text 'Live Snapshot', got: " + optLiveTarget.textContent);
  }
  const targetVals = targetSel.options.map(o => o.value);
  if (JSON.stringify(targetVals) !== JSON.stringify(["live", "snap:2", "snap:1"])) {
    throw new Error("expected target select options [live, snap:2, snap:1], got: " + JSON.stringify(targetVals));
  }
  const baseVals = baseSel.options.map(o => o.value);
  if (JSON.stringify(baseVals) !== JSON.stringify(["parent", "snap:1", "snap:2"])) {
    throw new Error("expected base select options [parent, snap:1, snap:2], got: " + JSON.stringify(baseVals));
  }
  if (targetSpan.textContent !== "Target: Snapshot 1") {
    throw new Error("expected summary Target span updated to 'Target: Snapshot 1', got: " + targetSpan.textContent);
  }

  // When viewing Live on a stale tab revision (live_rev !== rightRev), publishDrafts reloads cleanly.
  win._customFetch = () => Promise.resolve({
    ok: true,
    json: () => Promise.resolve({
      published: 1,
      sig: "sig-after-pub-stale-live",
      live_rev: "rev2"
    })
  });
  win.publishDrafts(pubRepo, pubChange);
  await new Promise(r => setTimeout(r, 30));
  if (reloadCalls !== 1) {
    throw new Error("expected 1 reload when targetSel is live and live_rev !== rightRev, got " + reloadCalls);
  }

  console.log("OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key)

	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test in TestDraftThreadsAPIAndMenu failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected node test to print OK, got: %s", string(out))
	}

	cssBytes, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	if !strings.Contains(string(cssBytes), ".draft-threads-trigger") {
		t.Errorf("expected style.css to style .draft-threads-trigger")
	}
}

func TestSideAwareDiffFlashAndCommentGlow(t *testing.T) {
	// 1. Verify CSS rules in static/style.css
	cssBytes, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	cssStr := string(cssBytes)

	// Ensure !important is not present on cell-moved / cell-moved-num background colors
	movedIdx := strings.Index(cssStr, "td.cell-moved,")
	if movedIdx == -1 {
		movedIdx = strings.Index(cssStr, "td.cell-moved")
	}
	if movedIdx != -1 {
		blockEnd := strings.Index(cssStr[movedIdx:], "}")
		if blockEnd != -1 {
			movedBlock := cssStr[movedIdx : movedIdx+blockEnd]
			if strings.Contains(movedBlock, "!important") {
				t.Errorf("td.cell-moved style block unexpectedly contains !important: %s", movedBlock)
			}
		}
	}

	// Verify side-aware flash classes, :not(.cell-empty), and moved-block vertical bar glow
	if !strings.Contains(cssStr, "jump-target-flash-l") || !strings.Contains(cssStr, "jump-target-flash-r") {
		t.Errorf("expected style.css to contain jump-target-flash-l and jump-target-flash-r")
	}
	if !strings.Contains(cssStr, ":not(.cell-empty)") {
		t.Errorf("expected style.css to contain :not(.cell-empty)")
	}
	if !strings.Contains(cssStr, "@property --move-glow-color") {
		t.Errorf("expected style.css to register @property --move-glow-color")
	}
	for _, want := range []string{
		"@keyframes moveBarGlow",
		"inset 4px 0 0 var(--move-jump-bar)",
		"inset 3px 0 0 var(--diff-move-border)",
		"@keyframes moveGutterGlow",
		"moveBarGlow 2.5s ease-in-out",
		"moveGutterGlow 2.5s ease-in-out",
		"jumpTargetFlash 1.8s ease-in-out",
	} {
		if !strings.Contains(cssStr, want) {
			t.Errorf("expected style.css to contain %q", want)
		}
	}
	flashIdx := strings.Index(cssStr, "@keyframes jumpTargetFlash")
	if flashIdx == -1 {
		t.Fatalf("expected style.css to contain @keyframes jumpTargetFlash")
	}
	flashEnd := strings.Index(cssStr[flashIdx:], "\n@keyframes moveBarGlow")
	if flashEnd != -1 {
		flashBlock := cssStr[flashIdx : flashIdx+flashEnd]
		if !strings.Contains(flashBlock, "var(--comment-range-bg)") {
			t.Errorf("expected jumpTargetFlash to highlight with var(--comment-range-bg), got:\n%s", flashBlock)
		}
		if strings.Contains(flashBlock, "transparent") {
			t.Errorf("jumpTargetFlash must not animate background-color to transparent:\n%s", flashBlock)
		}
	}

	// Verify threadCardBorderGlow keyframes and variables
	if !strings.Contains(cssStr, "@keyframes threadCardBorderGlow") {
		t.Errorf("expected style.css to contain @keyframes threadCardBorderGlow")
	}
	if !strings.Contains(cssStr, "0%, 50%") {
		t.Errorf("expected style.css to contain 0%%, 50%% keyframe for hold")
	}
	if !strings.Contains(cssStr, "--comment-jump-ring-clear:") || !strings.Contains(cssStr, "--comment-jump-halo-clear:") {
		t.Errorf("expected style.css to define --comment-jump-ring-clear and --comment-jump-halo-clear")
	}
	if !strings.Contains(cssStr, "threadCardBorderGlow 3s ease-in-out") {
		t.Errorf("expected .thread-card.jump-target-flash to use animation: threadCardBorderGlow 3s ease-in-out")
	}
	if !strings.Contains(cssStr, "0 0 0 2.5px var(--comment-jump-ring)") {
		t.Errorf("expected threadCardBorderGlow to use outer box-shadow ring with blur halo so overflow-x does not clip")
	}

	// 2. Node VM test for flashDiffRows, flashThreadCard, and jumpToMovePeerTarget
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found, skipping JS VM test")
	}

	nodeScript := `
const fs = require("fs");
const vm = require("vm");

function createMockElement(tag) {
  const children = [];
  const attrs = {};
  const classes = new Set();

  const el = {
    tagName: tag.toUpperCase(),
    style: {},
    parentNode: null,
    childNodes: children,
    getAttribute(name) { return attrs[name] !== undefined ? attrs[name] : null; },
    setAttribute(name, val) { attrs[name] = String(val); },
    hasAttribute(name) { return attrs[name] !== undefined; },
    removeAttribute(name) { delete attrs[name]; },
    appendChild(child) {
      child.parentNode = el;
      children.push(child);
      return child;
    },
    scrollIntoView() {},
    closest(sel) {
      let cur = el;
      while (cur) {
        if (sel.startsWith(".") && cur.classList && cur.classList.contains(sel.slice(1))) return cur;
        cur = cur.parentNode;
      }
      return null;
    }
  };

  Object.defineProperty(el, "className", {
    get() { return Array.from(classes).join(" "); },
    set(v) {
      classes.clear();
      if (v) {
        String(v).trim().split(/\s+/).forEach(c => { if (c) classes.add(c); });
      }
    }
  });

  el.classList = {
    add(...cls) { cls.forEach(c => classes.add(c)); },
    remove(...cls) { cls.forEach(c => classes.delete(c)); },
    contains(c) { return classes.has(c); }
  };

  el.querySelector = (sel) => {
    for (const c of children) {
      if (sel.startsWith("tr.diff-row") || sel.startsWith("tr[")) {
        const m = sel.match(/\[([^=\]]+)="([^"]+)"\]/);
        if (m && c.getAttribute(m[1]) === m[2]) return c;
      }
      if (sel === ".skip-all" && c.classList.contains("skip-all")) return c;
      if (c.querySelector) {
        const found = c.querySelector(sel);
        if (found) return found;
      }
    }
    return null;
  };

  el.querySelectorAll = (sel) => {
    const res = [];
    for (const c of children) {
      if (sel === "tr.row-skip" && c.classList.contains("row-skip")) res.push(c);
      if (sel === "tr.diff-row" && c.classList.contains("diff-row")) res.push(c);
      if (sel === ".diff-container" && c.classList.contains("diff-container")) res.push(c);
      if (c.querySelectorAll) res.push(...c.querySelectorAll(sel));
    }
    return res;
  };

  return el;
}

const doc = {
  documentElement: {
    classList: { contains() { return false; }, add() {}, remove() {} },
    style: { setProperty() {}, getPropertyValue() { return ""; } },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {}
  },
  body: createMockElement("BODY"),
  createElement(tag) { return createMockElement(tag); },
  getElementById(id) { return null; },
  addEventListener() {},
  removeEventListener() {},
  querySelector(sel) { return doc.body.querySelector(sel); },
  querySelectorAll(sel) {
    if (sel === ".diff-container") return doc.body.querySelectorAll(".diff-container");
    if (sel === ".diff-file-section") return [];
    if (sel === "tr.row-skip") return doc.body.querySelectorAll("tr.row-skip");
    return doc.body.querySelectorAll(sel);
  }
};

const win = {
  document: doc,
  location: { search: "", hash: "", href: "" },
  addEventListener() {},
  removeEventListener() {},
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} }
};
win.window = win;

const container = createMockElement("DIV");
container.className = "diff-container";
container.setAttribute("data-file-path", "pkg/foo.go");

const skipRow = createMockElement("TR");
skipRow.className = "row-skip";
skipRow.setAttribute("data-left-start", "14");
skipRow.setAttribute("data-left-end", "17");
skipRow.setAttribute("data-right-start", "14");
skipRow.setAttribute("data-right-end", "17");
const skipBtn = createMockElement("BUTTON");
skipBtn.className = "skip-all";
skipRow.appendChild(skipBtn);
container.appendChild(skipRow);

const row15 = createMockElement("TR");
row15.className = "diff-row";
row15.setAttribute("data-line-l", "15");
container.appendChild(row15);

const row16 = createMockElement("TR");
row16.className = "diff-row";
row16.setAttribute("data-line-l", "16");
container.appendChild(row16);

doc.body.appendChild(container);

let expandCalled = false;

let lastTimeoutDelay = null;
const ctx = vm.createContext({
  window: win,
  document: doc,
  setTimeout: (fn, ms) => {
    lastTimeoutDelay = ms;
    return setTimeout(fn, ms);
  },
  clearTimeout: clearTimeout,
  URLSearchParams: URLSearchParams
});

const appCode = fs.readFileSync("static/app.js", "utf8");
vm.runInContext(appCode, ctx);

win.expandContextAll = async () => { expandCalled = true; };

(async () => {
  // Test 1: flashDiffRows with side 'L'
  const rowA = createMockElement("TR");
  win.flashDiffRows(rowA, "L");
  if (!rowA.classList.contains("jump-target-flash-l")) {
    throw new Error("expected rowA to have jump-target-flash-l");
  }
  if (rowA.classList.contains("jump-target-flash-r") || rowA.classList.contains("jump-target-flash")) {
    throw new Error("rowA should not have -r or default flash");
  }

  // Test 2: flashDiffRows with side 'R' on multiple rows
  const rowB = createMockElement("TR");
  const rowC = createMockElement("TR");
  win.flashDiffRows([rowB, rowC], "R");
  if (!rowB.classList.contains("jump-target-flash-r") || !rowC.classList.contains("jump-target-flash-r")) {
    throw new Error("expected rowB and rowC to have jump-target-flash-r");
  }

  // Test 3: flashDiffRows with default side
  const rowD = createMockElement("TR");
  win.flashDiffRows(rowD);
  if (!rowD.classList.contains("jump-target-flash")) {
    throw new Error("expected rowD to have jump-target-flash");
  }

  // Test 4: flashThreadCard
  const card = createMockElement("DIV");
  card.className = "thread-card";
  win.flashThreadCard(card);
  if (!card.classList.contains("jump-target-flash")) {
    throw new Error("expected card to have jump-target-flash");
  }
  if (!card._flashTimer) {
    throw new Error("expected card._flashTimer to be set");
  }
  if (lastTimeoutDelay !== 3000) {
    throw new Error("expected lastTimeoutDelay === 3000, got: " + lastTimeoutDelay);
  }

  // Test 5: jumpToMovePeerTarget multiline range on side L with skip expansion
  await win.jumpToMovePeerTarget({
    peerPath: "pkg/foo.go",
    peerStart: 15,
    peerEnd: 16,
    peerSide: "L"
  });

  if (!expandCalled) {
    throw new Error("expected window.expandContextAll to be called for overlapping skip row");
  }
  if (!row15.classList.contains("jump-target-flash-l")) {
    throw new Error("expected row15 to have jump-target-flash-l");
  }
  if (!row16.classList.contains("jump-target-flash-l")) {
    throw new Error("expected row16 to have jump-target-flash-l");
  }

  // Test 6: scrollToHashLine handles huge ?end=99999999 in single pass, unified effectiveSide='L', and isolates skip rows to matched container
  row15.classList.remove("jump-target-flash-l");
  row16.classList.remove("jump-target-flash-l");
  const otherContainer = createMockElement("DIV");
  otherContainer.className = "diff-container";
  otherContainer.setAttribute("data-file-path", "pkg/other.go");
  const otherSkip = createMockElement("TR");
  otherSkip.className = "row-skip";
  otherSkip.setAttribute("data-left-start", "14");
  otherSkip.setAttribute("data-left-end", "20");
  otherSkip.setAttribute("data-right-start", "14");
  otherSkip.setAttribute("data-right-end", "20");
  const otherSkipBtn = createMockElement("BUTTON");
  otherSkipBtn.className = "skip-all";
  otherSkip.appendChild(otherSkipBtn);
  otherContainer.appendChild(otherSkip);
  doc.body.appendChild(otherContainer);

  let expandedBtns = [];
  win.expandContextAll = async (btn) => { expandedBtns.push(btn); };
  win.location.hash = "#L15";
  win.location.search = "?end=99999999";
  await win.scrollToHashLine();
  if (!row15.classList.contains("jump-target-flash-l") || !row16.classList.contains("jump-target-flash-l")) {
    throw new Error("expected unified/unqualified #L15 on data-line-l-only rows to flash jump-target-flash-l for both 15 and 16");
  }
  if (expandedBtns.includes(otherSkipBtn)) {
    throw new Error("scrollToHashLine expanded skip row in unrelated diff-container");
  }

  // Test 7: flashDiffRows includes matching preceding and intervening move-header-row, but ignores opposite-side headers and thread-rows
  const tbody = createMockElement("TBODY");
  const hdrR = createMockElement("TR");
  hdrR.className = "move-header-row";
  for (const cls of ["line-num", "line-code", "line-num cell-moved-num", "move-header-cell"]) {
    const td = createMockElement("TD");
    td.className = cls;
    hdrR.appendChild(td);
  }
  const mRow1 = createMockElement("TR");
  mRow1.className = "diff-row";
  const midHdrL = createMockElement("TR");
  midHdrL.className = "move-header-row";
  const midThread = createMockElement("TR");
  midThread.className = "thread-row";
  const mRow2 = createMockElement("TR");
  mRow2.className = "diff-row";
  tbody.appendChild(hdrR);
  tbody.appendChild(mRow1);
  tbody.appendChild(midHdrL);
  tbody.appendChild(midThread);
  tbody.appendChild(mRow2);

  win.flashDiffRows([mRow1, mRow2], "R");
  if (!hdrR.classList.contains("jump-target-flash-r") || !midHdrL.classList.contains("jump-target-flash-r")) {
    throw new Error("expected preceding and intervening move-header-row to receive jump-target-flash-r");
  }
  if (midThread.classList.contains("jump-target-flash-r")) {
    throw new Error("thread-row inside moved block should not receive jump-target-flash-r");
  }

  hdrR.classList.remove("jump-target-flash-r");
  win.flashDiffRows([mRow1], "L");
  if (hdrR.classList.contains("jump-target-flash-l")) {
    throw new Error("R-only move-header-row should not receive jump-target-flash-l when flashing L");
  }

  console.log("OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`

	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test in TestSideAwareDiffFlashAndCommentGlow failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected node test to print OK, got: %s", string(out))
	}
}

func TestIdentOccurrencesAcrossIntraMark(t *testing.T) {
	// 1. Verify CSS rules in static/style.css
	cssBytes, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	cssStr := string(cssBytes)

	// Verify td.cell-moved-modified mark.intra has background var(--diff-add-intra)
	if !strings.Contains(cssStr, "cell-moved-modified mark.intra") {
		t.Errorf("expected style.css to style cell-moved-modified mark.intra")
	}
	if !strings.Contains(cssStr, ".ident-occ-hl mark.intra") || !strings.Contains(cssStr, "mark.ident-occ-mark mark.intra") {
		t.Errorf("expected style.css to include .ident-occ-hl mark.intra and mark.ident-occ-mark mark.intra")
	}
	if !strings.Contains(cssStr, "mark.ident-occ-mark.ident-occ-frag-start") ||
		!strings.Contains(cssStr, "mark.ident-occ-mark.ident-occ-frag-mid") ||
		!strings.Contains(cssStr, "mark.ident-occ-mark.ident-occ-frag-end") {
		t.Errorf("expected style.css to define mark.ident-occ-mark.ident-occ-frag-start, .ident-occ-frag-mid, and .ident-occ-frag-end with winning specificity")
	}

	// 2. Node VM test
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found, skipping JS VM test")
	}

	nodeScript := `
const fs = require("fs");
const vm = require("vm");

const Node = {
  ELEMENT_NODE: 1,
  TEXT_NODE: 3
};

function createTextNode(text) {
  const tn = {
    nodeType: Node.TEXT_NODE,
    textContent: String(text),
    parentNode: null,
    parentElement: null,
    splitText(offset) {
      const full = this.textContent;
      this.textContent = full.slice(0, offset);
      const newNode = createTextNode(full.slice(offset));
      if (this.parentNode) {
        const idx = this.parentNode.childNodes.indexOf(this);
        this.parentNode.childNodes.splice(idx + 1, 0, newNode);
        newNode.parentNode = this.parentNode;
        newNode.parentElement = this.parentElement;
      }
      return newNode;
    }
  };
  return tn;
}

function createElement(tag) {
  const el = {
    nodeType: Node.ELEMENT_NODE,
    tagName: tag.toUpperCase(),
    className: "",
    style: {},
    attributes: {},
    childNodes: [],
    parentElement: null,
    parentNode: null,
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    removeAttribute(k) { delete this.attributes[k]; },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    classList: {
      add: (cls) => {
        const set = new Set((el.className || "").split(/\s+/).filter(Boolean));
        set.add(cls);
        el.className = Array.from(set).join(" ");
      },
      remove: (cls) => {
        const set = new Set((el.className || "").split(/\s+/).filter(Boolean));
        set.delete(cls);
        el.className = Array.from(set).join(" ");
      },
      contains: (cls) => (el.className || "").split(/\s+/).includes(cls)
    },
    get textContent() {
      return this.childNodes.map(n => n.textContent).join("");
    },
    set textContent(val) {
      this.childNodes = [createTextNode(val)];
      this.childNodes[0].parentNode = this;
      this.childNodes[0].parentElement = this;
    },
    appendChild(node) {
      node.parentNode = this;
      node.parentElement = this;
      this.childNodes.push(node);
      return node;
    },
    replaceChild(newChild, oldChild) {
      const idx = this.childNodes.indexOf(oldChild);
      if (idx !== -1) {
        newChild.parentNode = this;
        newChild.parentElement = this;
        oldChild.parentNode = null;
        oldChild.parentElement = null;
        this.childNodes[idx] = newChild;
      }
      return oldChild;
    },
    normalize() {
      const newNodes = [];
      let pendingText = null;
      for (const n of this.childNodes) {
        if (n.nodeType === Node.TEXT_NODE) {
          if (pendingText) {
            pendingText.textContent += n.textContent;
          } else {
            pendingText = createTextNode(n.textContent);
            pendingText.parentNode = this;
            pendingText.parentElement = this;
            newNodes.push(pendingText);
          }
        } else {
          pendingText = null;
          newNodes.push(n);
          if (n.normalize) n.normalize();
        }
      }
      this.childNodes = newNodes;
    },
    closest(selector) {
      let cur = this;
      while (cur) {
        if (selector.startsWith(".") && cur.classList && cur.classList.contains(selector.slice(1))) return cur;
        if (selector.startsWith("td.") && cur.tagName === "TD" && cur.classList && cur.classList.contains(selector.slice(3))) return cur;
        if (selector.startsWith("mark.") && cur.tagName === "MARK" && cur.classList && cur.classList.contains(selector.slice(5))) return cur;
        if (selector === "td.line-code" && cur.tagName === "TD" && cur.classList && cur.classList.contains("line-code")) return cur;
        if (selector.includes(",")) {
          const parts = selector.split(",").map(s => s.trim());
          for (const p of parts) {
            const found = cur.closest(p);
            if (found) return found;
          }
        }
        cur = cur.parentElement;
      }
      return null;
    },
    querySelectorAll(selector) {
      const res = [];
      const parts = selector.split(",").map(s => s.trim());
      function match(c) {
        for (const p of parts) {
          if (p.startsWith(".") && c.classList && c.classList.contains(p.slice(1))) return true;
          if (p.includes(".")) {
            const [tag, cls] = p.split(".");
            if (c.tagName === tag.toUpperCase() && c.classList && c.classList.contains(cls)) return true;
          } else if (c.tagName === p.toUpperCase()) {
            return true;
          }
        }
        return false;
      }
      function walk(n) {
        for (const c of n.childNodes) {
          if (c.nodeType === Node.ELEMENT_NODE) {
            if (match(c)) res.push(c);
            walk(c);
          }
        }
      }
      walk(this);
      return res;
    },
    querySelector(selector) {
      return this.querySelectorAll(selector)[0] || null;
    }
  };
  return el;
}

const doc = createElement("DOCUMENT");
doc.body = createElement("BODY");
doc.body.parentNode = doc;
doc.documentElement = {
  classList: { contains() { return false; }, add() {}, remove() {} },
  style: { setProperty() {}, getPropertyValue() { return ""; } },
  getAttribute() { return ""; },
  setAttribute() {},
  removeAttribute() {}
};
doc.createElement = createElement;
doc.createTextNode = createTextNode;
doc.getElementById = () => null;
doc.querySelectorAll = function(selector) { return doc.body.querySelectorAll(selector); };
doc.querySelector = function(selector) { return doc.body.querySelector(selector); };
doc.addEventListener = () => {};
doc.removeEventListener = () => {};

doc.createTreeWalker = function(root, filter) {
  const nodes = [];
  function walk(n) {
    if (n.nodeType === Node.TEXT_NODE) nodes.push(n);
    if (n.childNodes) {
      for (const c of n.childNodes) walk(c);
    }
  }
  walk(root);
  return {
    currentNode: root,
    nextNode() {
      let idx = nodes.indexOf(this.currentNode);
      if (idx === -1) {
        for (let i = 0; i < nodes.length; i++) {
          if (nodes[i] !== this.currentNode) { idx = i - 1; break; }
        }
      }
      if (idx + 1 < nodes.length) {
        this.currentNode = nodes[idx + 1];
        return this.currentNode;
      }
      return null;
    },
    previousNode() {
      let idx = nodes.indexOf(this.currentNode);
      if (idx > 0) {
        this.currentNode = nodes[idx - 1];
        return this.currentNode;
      }
      return null;
    }
  };
};

const win = {
  Node: Node,
  document: doc,
  location: { search: "", hash: "", href: "" },
  addEventListener() {},
  removeEventListener() {},
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  NodeFilter: { SHOW_TEXT: 4 },
  Node: Node
});

const appCode = fs.readFileSync("static/app.js", "utf8");
vm.runInContext(appCode, ctx);

(async () => {
  // Setup code container with intra mark: val = prev_<mark class="intra">id</mark> + 1;
  const td = createElement("TD");
  td.className = "line-code";
  const tn1 = createTextNode("val = prev_");
  const markIntra = createElement("MARK");
  markIntra.className = "intra";
  const tn2 = createTextNode("id");
  markIntra.appendChild(tn2);
  const tn3 = createTextNode(" + 1;");
  td.appendChild(tn1);
  td.appendChild(markIntra);
  td.appendChild(tn3);
  doc.body.appendChild(td);

  // Test 1: getAdjacentWordRun across boundary from tn1
  const run1 = win.getAdjacentWordRun(td, tn1, 10);
  if (run1 !== "prev_id") {
    throw new Error("expected run1 to be 'prev_id', got: " + run1);
  }

  // Test 2: getAdjacentWordRun across boundary from tn2 inside mark.intra
  const run2 = win.getAdjacentWordRun(td, tn2, 1);
  if (run2 !== "prev_id") {
    throw new Error("expected run2 to be 'prev_id', got: " + run2);
  }

  // Test 3: applyIdentOccurrences creates multi-node fragment marks
  win.applyIdentOccurrences("prev_id");
  const occMarks = td.querySelectorAll(".ident-occ-mark");
  if (occMarks.length !== 2) {
    throw new Error("expected 2 occMarks across intra boundary, got " + occMarks.length);
  }
  if (!occMarks[0].classList.contains("ident-occ-frag-start")) {
    throw new Error("expected first occMark to have ident-occ-frag-start class");
  }
  if (!occMarks[1].classList.contains("ident-occ-frag-end")) {
    throw new Error("expected second occMark to have ident-occ-frag-end class");
  }
  if (occMarks[0].getAttribute("data-occ-ident") !== "prev_id" || occMarks[1].getAttribute("data-occ-ident") !== "prev_id") {
    throw new Error("expected data-occ-ident attribute 'prev_id' on fragments");
  }

  // Test 4: resolveHoveredIdent zero-flicker stability guard
  const resolved = win.resolveHoveredIdent(occMarks[1], 0, 0);
  if (resolved !== "prev_id") {
    throw new Error("expected resolveHoveredIdent to resolve 'prev_id', got: " + resolved);
  }

  // Test 5: clearIdentOccurrences unwraps cleanly and normalizes
  win.clearIdentOccurrences();
  const remainingMarks = td.querySelectorAll(".ident-occ-mark");
  if (remainingMarks.length !== 0) {
    throw new Error("expected 0 occMarks after clear, got " + remainingMarks.length);
  }
  if (td.textContent !== "val = prev_id + 1;") {
    throw new Error("expected textContent restored to 'val = prev_id + 1;', got: " + td.textContent);
  }

  // Test 6: 3-fragment (start, mid, end) ordering across two intra marks and multiple matches in one cell
  const td3 = createElement("TD");
  td3.className = "line-code";
  const a1 = createTextNode("alpha_");
  const m1 = createElement("MARK");
  m1.className = "intra";
  m1.appendChild(createTextNode("beta"));
  const m2 = createElement("MARK");
  m2.className = "intra";
  m2.appendChild(createTextNode("_gamma"));
  const a2 = createTextNode(" + alpha_beta_gamma");
  td3.appendChild(a1);
  td3.appendChild(m1);
  td3.appendChild(m2);
  td3.appendChild(a2);
  doc.body.appendChild(td3);

  win.applyIdentOccurrences("alpha_beta_gamma");
  const marks3 = td3.querySelectorAll(".ident-occ-mark");
  if (marks3.length !== 4) {
    throw new Error("expected 4 marks (3 fragments + 1 whole), got " + marks3.length);
  }
  if (!marks3[0].classList.contains("ident-occ-frag-start") ||
      !marks3[1].classList.contains("ident-occ-frag-mid") ||
      !marks3[2].classList.contains("ident-occ-frag-end")) {
    throw new Error("expected 3 fragments in left-to-right start, mid, end order");
  }
  if (marks3[3].classList.contains("ident-occ-frag-start") || marks3[3].classList.contains("ident-occ-frag-end")) {
    throw new Error("single-node whole match should not have frag classes");
  }

  // Test 7: plain comments (.tok-com) and strings (.tok-str) return null even if holding ident-occ-mark, whereas .tok-doc works
  const comSpan = createElement("SPAN");
  comSpan.className = "tok-com";
  const comMark = createElement("MARK");
  comMark.className = "ident-occ-mark";
  comMark.setAttribute("data-occ-ident", "alpha_beta_gamma");
  comMark.textContent = "alpha_beta_gamma";
  comSpan.appendChild(comMark);
  td3.appendChild(comSpan);
  if (win.resolveHoveredIdent(comMark, 0, 0) !== null) {
    throw new Error("expected resolveHoveredIdent inside plain .tok-com to return null");
  }

  const strSpan = createElement("SPAN");
  strSpan.className = "tok-str";
  strSpan.textContent = "alpha_beta_gamma";
  td3.appendChild(strSpan);
  if (win.resolveHoveredIdent(strSpan, 0, 0) !== null) {
    throw new Error("expected resolveHoveredIdent inside .tok-str to return null");
  }

  const docSpan = createElement("SPAN");
  docSpan.className = "tok-com tok-doc";
  const docMark = createElement("MARK");
  docMark.className = "ident-occ-mark";
  docMark.setAttribute("data-occ-ident", "alpha_beta_gamma");
  docMark.textContent = "alpha_beta_gamma";
  docSpan.appendChild(docMark);
  td3.appendChild(docSpan);
  if (win.resolveHoveredIdent(docMark, 0, 0) !== "alpha_beta_gamma") {
    throw new Error("expected resolveHoveredIdent inside .tok-doc to resolve 'alpha_beta_gamma'");
  }

  win.clearIdentOccurrences();
  console.log("OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`

	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test in TestIdentOccurrencesAcrossIntraMark failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected node test to print OK, got: %s", string(out))
	}
}

func TestGoDocCommentLinkUI(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) string {
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
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")

	goMod := "module example.com/doclinktest\n\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)

	mainGo1 := `package main

// Target represents the main target.
type Target struct{}

// Run executes the target.
func (t *Target) Run() {}
`
	_ = os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(mainGo1), 0644)
	run("add", ".")
	run("commit", "-m", "Initial commit")
	firstHash := run("rev-parse", "HEAD")

	mainGo2 := `package main

// Target represents the main target.
type Target struct{}

// Run executes the target.
func (t *Target) Run() {}

// UseTarget references [Target] or [*Target.Run].
func UseTarget() {}
`
	_ = os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(mainGo2), 0644)
	run("add", ".")
	run("commit", "-m", "Add UseTarget with doc links")
	secondHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(func() { ts.Close() })

	// 1. Verify split view diff renders .tok-doc spans for Target and *Target.Run
	diffURL := fmt.Sprintf("%s/r/%s/c/%s?diffs=all&mode=split&base=%s&target=%s", ts.URL, rec.Slug, secondHash, firstHash, secondHash)
	resp, err := http.Get(diffURL)
	if err != nil {
		t.Fatalf("GET diff failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for diff, got %d", resp.StatusCode)
	}
	diffBytes, _ := io.ReadAll(resp.Body)
	diffHTML := string(diffBytes)

	if !strings.Contains(diffHTML, `<span class="tok-com tok-doc">Target</span>`) {
		t.Errorf("expected diff to contain <span class=\"tok-com tok-doc\">Target</span>, got:\n%s", diffHTML)
	}
	if !strings.Contains(diffHTML, `<span class="tok-com tok-doc">*Target.Run</span>`) {
		t.Errorf("expected diff to contain <span class=\"tok-com tok-doc\">*Target.Run</span>, got:\n%s", diffHTML)
	}
	// Brackets should remain plain tok-com
	if !strings.Contains(diffHTML, `[</span><span class="tok-com tok-doc">Target</span><span class="tok-com">]`) {
		t.Errorf("expected diff to contain [</span><span class=\"tok-com tok-doc\">Target</span><span class=\"tok-com\">]")
	}

	// 2. Test /api/symbol on the doc link Target
	symURL := fmt.Sprintf("%s/api/symbol?repo=%s&change=%s&side=R&path=main.go&line=9&col=26", ts.URL, rec.Slug, secondHash)
	symResp, err := http.Get(symURL)
	if err != nil {
		t.Fatalf("GET /api/symbol failed: %v", err)
	}
	defer symResp.Body.Close()
	if symResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /api/symbol, got %d", symResp.StatusCode)
	}
	var symResult struct {
		Found      bool   `json:"found"`
		SymbolName string `json:"symbol_name"`
		InRepo     bool   `json:"in_repo"`
	}
	if err := json.NewDecoder(symResp.Body).Decode(&symResult); err != nil {
		t.Fatalf("failed to decode /api/symbol JSON: %v", err)
	}
	if !symResult.Found || symResult.SymbolName != "Target" {
		t.Errorf("expected symbol Target found=true, got %+v", symResult)
	}

	// Test /api/symbol on *Target.Run
	runSymURL := fmt.Sprintf("%s/api/symbol?repo=%s&change=%s&side=R&path=main.go&line=9&col=40", ts.URL, rec.Slug, secondHash)
	runSymResp, err := http.Get(runSymURL)
	if err != nil {
		t.Fatalf("GET /api/symbol for Run failed: %v", err)
	}
	defer runSymResp.Body.Close()
	if runSymResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for Run /api/symbol, got %d", runSymResp.StatusCode)
	}
	var runSymResult struct {
		Found      bool   `json:"found"`
		SymbolName string `json:"symbol_name"`
		InRepo     bool   `json:"in_repo"`
	}
	if err := json.NewDecoder(runSymResp.Body).Decode(&runSymResult); err != nil {
		t.Fatalf("failed to decode /api/symbol Run JSON: %v", err)
	}
	if !runSymResult.Found || runSymResult.SymbolName != "Run" {
		t.Errorf("expected symbol Run found=true, got %+v", runSymResult)
	}

	// 3. Test static/style.css contains .tok-doc pointer hover and dark-mode color rules
	cssResp, err := http.Get(ts.URL + "/static/style.css")
	if err != nil {
		t.Fatalf("GET /static/style.css failed: %v", err)
	}
	defer cssResp.Body.Close()
	cssBytes, _ := io.ReadAll(cssResp.Body)
	cssText := string(cssBytes)
	if strings.Contains(cssText, "underline dotted") {
		t.Errorf("expected style.css not to contain underline dotted")
	}
	if !strings.Contains(cssText, ".tok-doc:hover") || !strings.Contains(cssText, "cursor: pointer;") || !strings.Contains(cssText, "text-decoration: none;") {
		t.Errorf("expected style.css to contain .tok-doc:hover with cursor: pointer and text-decoration: none")
	}

	// 4. Test Node VM symbol popup click wiring & popup chaining
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur.textContent = (cur.textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT", "PATH", "POLYLINE", "RECT", "LINE"].includes(tag) && !attrs.trim().endsWith("/")) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    nodeType: 1,
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    style: {},
    children: [],
    parentElement: null,
    get parentNode() { return this.parentElement; },
    get childNodes() { return this.children; },
    get nextElementSibling() {
      if (!this.parentElement) return null;
      const idx = this.parentElement.children.indexOf(this);
      if (idx !== -1 && idx + 1 < this.parentElement.children.length) {
        return this.parentElement.children[idx + 1];
      }
      return null;
    },
    remove() {
      if (this.parentElement) {
        const idx = this.parentElement.children.indexOf(this);
        if (idx !== -1) this.parentElement.children.splice(idx, 1);
        this.parentElement = null;
      }
    },
    contains(node) {
      let cur = node;
      while (cur) {
        if (cur === this) return true;
        cur = cur.parentElement;
      }
      return false;
    },
    get disabled() { return !!this.attributes["disabled"]; },
    set disabled(v) {
      if (v) this.attributes["disabled"] = "disabled";
      else delete this.attributes["disabled"];
    },
    _textContent: "",
    get textContent() {
      let t = this._textContent || "";
      for (const ch of this.children) {
        t += ch.textContent;
      }
      return t;
    },
    set textContent(v) {
      this._textContent = String(v);
    },
    listeners: {},
    get offsetWidth() { return 300; },
    get offsetHeight() { return 200; },
    scrollTop: 0,
    scrollLeft: 0,
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    removeAttribute(k) { delete this.attributes[k]; },
    getBoundingClientRect() { return { top: 100, left: 100, bottom: 300, right: 400, width: 300, height: 200 }; },
    focus() { doc.activeElement = this; },
    blur() { if (doc.activeElement === this) doc.activeElement = null; },
    addEventListener(event, fn) {
      if (!this.listeners[event]) this.listeners[event] = [];
      this.listeners[event].push(fn);
    },
    removeEventListener(event, fn) {
      if (this.listeners[event]) {
        this.listeners[event] = this.listeners[event].filter(f => f !== fn);
      }
    },
    appendChild(child) {
      if (!child) return child;
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    insertBefore(newChild, refChild) {
      if (!newChild) return newChild;
      newChild.parentElement = this;
      const idx = this.children.indexOf(refChild);
      if (idx !== -1) this.children.splice(idx, 0, newChild);
      else this.children.push(newChild);
      return newChild;
    },
    get classList() {
      return {
        contains: (c) => (el.className || "").split(/\s+/).includes(c),
        add: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.add(c));
          el.className = Array.from(parts).join(" ");
        },
        remove: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.delete(c));
          el.className = Array.from(parts).join(" ");
        }
      };
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (node.tagName && matchesSel(node, sel)) return node;
        node = node.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const results = [];
      function search(n) {
        for (const ch of n.children) {
          if (matchesSel(ch, sel)) results.push(ch);
          search(ch);
        }
      }
      search(this);
      return results;
    },
    set innerHTML(val) {
      this._innerHTML = val;
      this.children = parseHtml(val, this);
    },
    get innerHTML() {
      return this._innerHTML || "";
    }
  };
  return el;
}

function matchesSel(node, sel) {
  const parts = sel.split(",").map(s => s.trim());
  for (const p of parts) {
    if (p.includes("[")) {
      const bIdx = p.indexOf("[");
      const tagPart = p.slice(0, bIdx);
      const attrPart = p.slice(bIdx + 1, -1);
      if (tagPart) {
        if (tagPart.startsWith(".")) {
          if (!node.classList || !node.classList.contains(tagPart.slice(1))) continue;
        } else if ((node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) {
          continue;
        }
      }
      if (attrPart.includes("=")) {
        const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
        if (node.getAttribute && node.getAttribute(k) === v) return true;
      } else {
        if (node.hasAttribute && node.hasAttribute(attrPart)) return true;
      }
    } else if (p.startsWith("#")) {
      const id = p.slice(1);
      if (node.id === id) return true;
    } else if (p.includes(".")) {
      const dotIdx = p.indexOf(".");
      const tag = p.slice(0, dotIdx);
      const cls = p.slice(dotIdx + 1);
      if (tag && (node.tagName || "").toLowerCase() !== tag.toLowerCase()) continue;
      if (cls && (!node.classList || !node.classList.contains(cls))) continue;
      return true;
    } else {
      if ((node.tagName || "").toLowerCase() === p.toLowerCase()) return true;
    }
  }
  return false;
}

const body = createMockElement("BODY");
const htmlEl = createMockElement("HTML");
htmlEl.clientWidth = 1000;
htmlEl.clientHeight = 800;
htmlEl.appendChild(body);

const docListeners = {};
const winListeners = {};
const doc = {
  body: body,
  documentElement: htmlEl,
  activeElement: null,
  getElementById(id) {
    const all = body.querySelectorAll("#" + id);
    return all.length > 0 ? all[0] : null;
  },
  createElement(tag) { return createMockElement(tag); },
  createRange() {
    return {
      setStart() {},
      setEnd() {},
      toString() { return "mock"; }
    };
  },
  createTreeWalker(root) { return { currentNode: root, nextNode() { return null; } }; },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (docListeners[event]) {
      docListeners[event] = docListeners[event].filter(f => f !== fn);
    }
  },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); }
};

let lastSymbolLookupUrl = "";

const win = {
  document: doc,
  NodeFilter: { SHOW_TEXT: 4 },
  location: { pathname: "/r/testrepo/c/123", search: "", hash: "", href: "", reload() {} },
  addEventListener(event, fn) {
    if (!winListeners[event]) winListeners[event] = [];
    winListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (winListeners[event]) winListeners[event] = winListeners[event].filter(f => f !== fn);
  },
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; },
  window: null,
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  fetch(url, opts) {
    if (url.includes("/api/symbol")) {
      lastSymbolLookupUrl = url;
      if (url.includes("line=9")) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            found: true,
            symbol_name: "Target",
            display_path: "main.go",
            def_path: "main.go",
            def_line: 4,
            in_repo: true,
            signature_html: "type <span class=\"tok-typ\">Target</span> struct",
            snippet: [
              { line: 3, html: "// Target represents the main target." },
              { line: 4, html: "type <span class=\"tok-typ\">Target</span> struct{}" },
              { line: 6, html: "// Next link to [InnerSymbol]." },
              { line: 7, html: "func <span class=\"tok-id\">Next</span>() { <span class=\"tok-com tok-doc\">InnerSymbol</span>() }" }
            ]
          })
        });
      }
      if (url.includes("line=7")) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            found: true,
            symbol_name: "InnerSymbol",
            display_path: "main.go",
            def_path: "main.go",
            def_line: 12,
            in_repo: true,
            signature_html: "func <span class=\"tok-id\">InnerSymbol</span>()",
            snippet: [
              { line: 12, html: "func <span class=\"tok-id\">InnerSymbol</span>() {}" }
            ]
          })
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ found: false })
      });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  }
};
win.window = win;

// Set up DOM structure: .diff-container -> .diff-table -> tr -> td -> span.tok-com.tok-doc
const container = createMockElement("DIV");
container.className = "diff-container";
container.setAttribute("data-repo", "testrepo");
container.setAttribute("data-file-path", "main.go");

const table = createMockElement("TABLE");
table.className = "diff-table diff-table-split";

const tr = createMockElement("TR");
tr.className = "diff-row row-add";
tr.setAttribute("data-line-r", "9");

const tdEmpty = createMockElement("TD");
tdEmpty.className = "line-code-split cell-empty";

const tdCode = createMockElement("TD");
tdCode.className = "line-code-split cell-add";
tdCode.setAttribute("data-side", "R");

const spanComment = createMockElement("SPAN");
spanComment.className = "tok-com";
spanComment.textContent = "// UseTarget references ";

const spanOpen = createMockElement("SPAN");
spanOpen.className = "tok-com";
spanOpen.textContent = "[";

const spanDoc = createMockElement("SPAN");
spanDoc.className = "tok-com tok-doc";
spanDoc.textContent = "Target";

const spanClose = createMockElement("SPAN");
spanClose.className = "tok-com";
spanClose.textContent = "]";

tdCode.appendChild(spanComment);
tdCode.appendChild(spanOpen);
tdCode.appendChild(spanDoc);
tdCode.appendChild(spanClose);

tr.appendChild(tdEmpty);
tr.appendChild(tdCode);
table.appendChild(tr);
container.appendChild(table);
body.appendChild(container);

const sandbox = {
  window: win,
  document: doc,
  navigator: { userAgent: "Node" },
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  setInterval: setInterval,
  clearInterval: clearInterval,
  URL: URL,
  URLSearchParams: URLSearchParams,
  console: console,
  NodeFilter: win.NodeFilter,
  Node: { ELEMENT_NODE: 1, TEXT_NODE: 3 },
  AbortController: AbortController,
  fetch: win.fetch.bind(win)
};

vm.createContext(sandbox);
vm.runInContext(code, sandbox);

(async () => {
  // 1. Simulate click on .tok-doc in the diff table
  const docClicks = docListeners["click"] || [];
  if (docClicks.length === 0) {
    throw new Error("expected document click listeners registered by app.js");
  }

  // Click on spanDoc
  for (const handler of docClicks) {
    await handler({
      target: spanDoc,
      button: 0,
      detail: 1,
      preventDefault() {},
      stopPropagation() {}
    });
  }

  // Verify symbol lookup was triggered
  if (!lastSymbolLookupUrl.includes("path=main.go") || !lastSymbolLookupUrl.includes("line=9") || !lastSymbolLookupUrl.includes("side=R")) {
    throw new Error("expected /api/symbol fetch for line=9 side=R, got: " + lastSymbolLookupUrl);
  }

  // Verify symbol popup opened
  const popup = body.querySelector(".symbol-popup");
  if (!popup) {
    throw new Error("expected .symbol-popup in document body");
  }

  const loc = popup.querySelector(".sym-popup-loc");
  if (!loc || !loc.textContent.includes("main.go:4")) {
    throw new Error("expected popup loc to include main.go:4, got: " + (loc ? loc.textContent : "null"));
  }

  // 2. Chained click: find .tok-doc inside popup snippet code
  const innerDoc = popup.querySelectorAll(".tok-doc")[0];
  if (!innerDoc) {
    throw new Error("expected .tok-doc inside popup snippet code");
  }

  // Click innerDoc inside snippet
  const popupClicks = popup.listeners["click"] || [];
  if (popupClicks.length === 0) {
    throw new Error("expected popup click listener attached by attachPopupClickHandler");
  }

  for (const handler of popupClicks) {
    await handler({
      target: innerDoc,
      button: 0,
      detail: 1,
      preventDefault() {},
      stopPropagation() {}
    });
  }

  // Verify chained symbol lookup was triggered for line=7
  if (!lastSymbolLookupUrl.includes("line=7")) {
    throw new Error("expected chained /api/symbol fetch for line=7, got: " + lastSymbolLookupUrl);
  }

  // Verify back button is present in the chained popup
  const backBtn = popup.querySelector(".sym-btn-back");
  if (!backBtn) {
    throw new Error("expected .sym-btn-back in popup header after chaining");
  }

  // 3. Back navigation restores Target definition
  for (const handler of popupClicks) {
    await handler({
      target: backBtn,
      button: 0,
      detail: 1,
      preventDefault() {},
      stopPropagation() {}
    });
  }
  const restoredLoc = popup.querySelector(".sym-popup-loc");
  if (!restoredLoc || !restoredLoc.textContent.includes("main.go:4")) {
    throw new Error("expected restored loc main.go:4 after clicking back, got: " + (restoredLoc ? restoredLoc.textContent : "null"));
  }

  // 4. Close button removes popup
  const closeBtn = popup.querySelector(".sym-btn-close");
  if (!closeBtn) {
    throw new Error("expected .sym-btn-close in popup header");
  }
  for (const handler of popupClicks) {
    await handler({
      target: closeBtn,
      button: 0,
      detail: 1,
      preventDefault() {},
      stopPropagation() {}
    });
  }
  if (body.querySelectorAll(".symbol-popup").length !== 0) {
    throw new Error("expected popup removed after clicking close button");
  }

  console.log("OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`

	cmd := exec.Command(nodeBin, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test in TestGoDocCommentLinkUI failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected node test to print OK, got: %s", string(out))
	}
}

func FuzzDiffExpandAndMovePreview(f *testing.F) {
	repoDir := f.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			f.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")
	run("config", "user.name", "Fuzz Tester")
	run("config", "user.email", "fuzz@example.com")

	f1 := "package main\n\nfunc A() {\n\t// line 4\n\t// line 5\n}\n\nfunc B() {}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(f1), 0644); err != nil {
		f.Fatal(err)
	}
	run("add", "main.go")
	run("commit", "-m", "Base commit\n\nChange-Id: Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	baseHash := run("rev-parse", "HEAD")

	f2 := "package main\n\nfunc NewA() {\n\t// line 4 modified\n\t// line 5\n}\n\nfunc B() {}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(f2), 0644); err != nil {
		f.Fatal(err)
	}
	run("commit", "-a", "-m", "Target commit\n\nChange-Id: Ibbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	targetHash := run("rev-parse", "HEAD")

	dbPath := filepath.Join(f.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { d.Close() })

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		f.Fatal(err)
	}

	cache := vcs.NewCache()
	srv, err := NewServer(d, cache, "127.0.0.1:0")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { srv.Shutdown(context.Background()) })

	handler := srv.httpServer.Handler

	f.Add(1, 3, 1, 3, "split", "main.go", "R", 1, 3)
	f.Add(0, 0, 1, 10, "unified", "main.go", "L", 2, 5)
	f.Add(-1, -5, -1, -5, "split", "/COMMIT_MSG", "R", -1, -1)
	f.Add(100, 200, 100, 200, "invalid", "nonexistent.go", "X", 100, 50)
	f.Add(1, 100000, 1, 100000, "", "", "", 1, 100000)

	f.Fuzz(func(t *testing.T, leftStart, leftEnd, rightStart, rightEnd int, mode, path, side string, start, end int) {
		expandURL := fmt.Sprintf("/api/diff/expand?repo=%s&left_rev=%s&right_rev=%s&path=%s&left_start=%d&left_end=%d&right_start=%d&right_end=%d&mode=%s",
			url.QueryEscape(rec.Slug),
			url.QueryEscape(baseHash),
			url.QueryEscape(targetHash),
			url.QueryEscape(path),
			leftStart, leftEnd, rightStart, rightEnd,
			url.QueryEscape(mode),
		)
		reqExpand := httptest.NewRequest("GET", expandURL, nil)
		recExpand := httptest.NewRecorder()
		handler.ServeHTTP(recExpand, reqExpand)
		if recExpand.Code >= 500 {
			t.Fatalf("GET %s returned server error %d: %s", expandURL, recExpand.Code, recExpand.Body.String())
		}

		moveURL := fmt.Sprintf("/api/diff/move-preview?repo=%s&change=%s&base=%s&target=%s&path=%s&side=%s&start=%d&end=%d",
			url.QueryEscape(rec.Slug),
			url.QueryEscape(targetHash),
			url.QueryEscape(baseHash),
			url.QueryEscape(targetHash),
			url.QueryEscape(path),
			url.QueryEscape(side),
			start, end,
		)
		reqMove := httptest.NewRequest("GET", moveURL, nil)
		recMove := httptest.NewRecorder()
		handler.ServeHTTP(recMove, reqMove)
		if recMove.Code >= 500 {
			t.Fatalf("GET %s returned server error %d: %s", moveURL, recMove.Code, recMove.Body.String())
		}
	})
}

func TestCommentFormControlsAndMenuRowColors(t *testing.T) {
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := vcs.Open(repoDir, srv.cache)
	changes, _ := repo.Changes()
	c := changes[0]

	// 1. Seed:
	// - thUnresDraft: unresolved thread with root draft
	// - thResWithReply: published resolved thread with draft reply
	// - thResRootDraft: resolved thread with root draft
	thUnresDraft, mUnresDraft, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "hello.go", "R", 1, 1, "package main", "alice", "fix package doc", true)
	if err != nil {
		t.Fatal(err)
	}
	thResWithReply, mResPublishedRoot, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "hello.go", "R", 2, 2, "", "bob", "resolved root", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetResolved(thResWithReply.ID, true); err != nil {
		t.Fatal(err)
	}
	mResDraftReply, err := d.AddMessage(thResWithReply.ID, "alice", "draft reply on resolved", true)
	if err != nil {
		t.Fatal(err)
	}
	thResRootDraft, mResRootDraft, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "hello.go", "R", 3, 3, "func main() {}", "carol", "resolved root draft", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetResolved(thResRootDraft.ID, true); err != nil {
		t.Fatal(err)
	}

	// Render change page and verify thread_card template markup for root drafts and draft replies.
	pageURL := fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, c.Key)
	resp, err := http.Get(pageURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s failed: status=%v err=%v", pageURL, resp.StatusCode, err)
	}
	pageBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	pageHTML := string(pageBytes)

	extractEditBlock := func(htmlStr string, msgID int64) string {
		marker := fmt.Sprintf(`id="msg-edit-%d"`, msgID)
		idx := strings.Index(htmlStr, marker)
		if idx == -1 {
			t.Fatalf("missing %s in rendered HTML", marker)
		}
		rest := htmlStr[idx:]
		endIdx := strings.Index(rest, "</form>")
		if endIdx == -1 {
			t.Fatalf("missing </form> inside %s: %q", marker, rest[:min(len(rest), 300)])
		}
		return rest[:endIdx+len("</form>")]
	}

	extractDivBlock := func(htmlStr string, marker string) string {
		idx := strings.Index(htmlStr, marker)
		if idx == -1 {
			t.Fatalf("missing %s in rendered HTML", marker)
		}
		start := strings.LastIndex(htmlStr[:idx+1], "<div")
		if start == -1 {
			t.Fatalf("missing opening <div for %s", marker)
		}
		depth := 0
		pos := start
		for pos < len(htmlStr) {
			nextOpen := strings.Index(htmlStr[pos:], "<div")
			nextClose := strings.Index(htmlStr[pos:], "</div>")
			if nextClose == -1 {
				t.Fatalf("unclosed <div> starting at %s", marker)
			}
			if nextOpen != -1 && nextOpen < nextClose {
				depth++
				pos += nextOpen + 4
			} else {
				depth--
				pos += nextClose + len("</div>")
				if depth == 0 {
					return htmlStr[start:pos]
				}
			}
		}
		t.Fatalf("unbalanced <div> starting at %s", marker)
		return ""
	}

	// data-resolved attribute on both unresolved and resolved cards
	if !strings.Contains(pageHTML, fmt.Sprintf(`id="thread-%d" class="thread-card thread-unresolved`, thUnresDraft.ID)) {
		t.Errorf("expected thread-%d rendered with thread-unresolved class", thUnresDraft.ID)
	}
	unresCardPrefix := fmt.Sprintf(`id="thread-%d"`, thUnresDraft.ID)
	if idx := strings.Index(pageHTML, unresCardPrefix); idx == -1 || !strings.Contains(pageHTML[idx:min(len(pageHTML), idx+240)], `data-resolved="false"`) {
		t.Errorf("expected thread-%d to have data-resolved=\"false\"", thUnresDraft.ID)
	}
	resCardPrefix := fmt.Sprintf(`id="thread-%d"`, thResWithReply.ID)
	if idx := strings.Index(pageHTML, resCardPrefix); idx == -1 || !strings.Contains(pageHTML[idx:min(len(pageHTML), idx+240)], `data-resolved="true"`) {
		t.Errorf("expected thread-%d to have data-resolved=\"true\"", thResWithReply.ID)
	}

	// Unresolved root draft edit form: <form>, unchecked resolved checkbox, Cmd/Ctrl+Enter hint
	editUnresHTML := extractEditBlock(pageHTML, mUnresDraft.ID)
	if !strings.Contains(editUnresHTML, fmt.Sprintf(`<form onsubmit="submitEditMessage(event,  %d )">`, mUnresDraft.ID)) ||
		!strings.Contains(editUnresHTML, `placeholder="Edit comment... (Cmd/Ctrl+Enter)"`) ||
		!strings.Contains(editUnresHTML, `required`) ||
		!strings.Contains(editUnresHTML, fmt.Sprintf(`<label class="resolved-toggle"><input type="checkbox" name="resolved"  onchange="handleEditResolvedChange(this,  %d )"> Resolved</label>`, mUnresDraft.ID)) ||
		strings.Contains(editUnresHTML, `name="resolved" checked`) ||
		!strings.Contains(editUnresHTML, `<span class="comment-hint">Cmd/Ctrl+Enter</span>`) {
		t.Errorf("unexpected unresolved root edit form HTML: %s", editUnresHTML)
	}

	// Resolved draft reply edit form AND resolved root draft edit form: must use $.Resolved (NOT .Resolved), so CHECKED!
	for _, msgID := range []int64{mResDraftReply.ID, mResRootDraft.ID} {
		editResHTML := extractEditBlock(pageHTML, msgID)
		if !strings.Contains(editResHTML, fmt.Sprintf(`<form onsubmit="submitEditMessage(event,  %d )">`, msgID)) ||
			!strings.Contains(editResHTML, fmt.Sprintf(`<label class="resolved-toggle"><input type="checkbox" name="resolved" checked onchange="handleEditResolvedChange(this,  %d )"> Resolved</label>`, msgID)) {
			t.Errorf("expected checked resolved checkbox for msg %d on resolved thread, got: %s", msgID, editResHTML)
		}
	}
	// Reply form checkboxes: unchecked for unresolved thread, checked for resolved threads.
	unresReplyHTML := extractDivBlock(pageHTML, fmt.Sprintf(`id="reply-form-%d"`, thUnresDraft.ID))
	if !strings.Contains(unresReplyHTML, fmt.Sprintf(`<label class="resolved-toggle"><input type="checkbox" name="resolved"  onchange="handleReplyResolvedChange(this,  %d )"> Resolved</label>`, thUnresDraft.ID)) ||
		strings.Contains(unresReplyHTML, `name="resolved" checked`) {
		t.Errorf("expected unchecked resolved checkbox in reply form for unresolved thread %d, got: %s", thUnresDraft.ID, unresReplyHTML)
	}
	for _, tid := range []int64{thResWithReply.ID, thResRootDraft.ID} {
		resReplyHTML := extractDivBlock(pageHTML, fmt.Sprintf(`id="reply-form-%d"`, tid))
		if !strings.Contains(resReplyHTML, fmt.Sprintf(`<label class="resolved-toggle"><input type="checkbox" name="resolved" checked onchange="handleReplyResolvedChange(this,  %d )"> Resolved</label>`, tid)) {
			t.Errorf("expected checked resolved checkbox in reply form for resolved thread %d, got: %s", tid, resReplyHTML)
		}
	}

	// Verify .thread-header has NO Reply/Resolve/Reopen and only renders .thread-actions when $first.Draft is true,
	// and .thread-footer sits after the last .message-body with Reply + Resolve/Reopen.
	for _, tc := range []struct {
		threadID     int64
		firstDraft   bool
		firstMsgID   int64
		lastMsgID    int64
		resolved     bool
		expectAction string
	}{
		{thUnresDraft.ID, true, mUnresDraft.ID, mUnresDraft.ID, false, "Resolve"},
		{thResWithReply.ID, false, mResPublishedRoot.ID, mResDraftReply.ID, true, "Reopen"},
		{thResRootDraft.ID, true, mResRootDraft.ID, mResRootDraft.ID, true, "Reopen"},
	} {
		cardHTML := extractDivBlock(pageHTML, fmt.Sprintf(`id="thread-%d"`, tc.threadID))
		headerHTML := extractDivBlock(cardHTML, `class="thread-header"`)
		for _, forbidden := range []string{"Reply", "Resolve", "Reopen", "toggleReplyForm", "resolveThread"} {
			if strings.Contains(headerHTML, forbidden) {
				t.Errorf("thread-%d .thread-header must not contain %q, got: %s", tc.threadID, forbidden, headerHTML)
			}
		}
		if tc.firstDraft {
			if !strings.Contains(headerHTML, `class="thread-actions"`) ||
				!strings.Contains(headerHTML, fmt.Sprintf(`openEditMessageForm( %d )`, tc.firstMsgID)) ||
				!strings.Contains(headerHTML, fmt.Sprintf(`deleteMessage( %d )`, tc.firstMsgID)) {
				t.Errorf("thread-%d .thread-header expected .thread-actions Edit/Delete for root draft %d, got: %s", tc.threadID, tc.firstMsgID, headerHTML)
			}
		} else {
			if strings.Contains(headerHTML, `thread-actions`) || strings.Contains(headerHTML, "Edit") || strings.Contains(headerHTML, "Delete") {
				t.Errorf("thread-%d .thread-header must omit .thread-actions when $first.Draft is false, got: %s", tc.threadID, headerHTML)
			}
		}

		footerHTML := extractDivBlock(cardHTML, `class="thread-footer"`)
		if !strings.Contains(footerHTML, fmt.Sprintf(`toggleReplyForm( %d )`, tc.threadID)) ||
			!strings.Contains(footerHTML, ">Reply</button>") ||
			!strings.Contains(footerHTML, fmt.Sprintf(`resolveThread( %d , %t)`, tc.threadID, !tc.resolved)) ||
			!strings.Contains(footerHTML, fmt.Sprintf(">%s</button>", tc.expectAction)) {
			t.Errorf("thread-%d .thread-footer missing expected actions, got: %s", tc.threadID, footerHTML)
		}

		lastMsgMarker := fmt.Sprintf(`id="msg-%d"`, tc.lastMsgID)
		lastMsgIdx := strings.Index(cardHTML, lastMsgMarker)
		footerIdx := strings.Index(cardHTML, `class="thread-footer"`)
		replyFormIdx := strings.Index(cardHTML, fmt.Sprintf(`id="reply-form-%d"`, tc.threadID))
		if lastMsgIdx == -1 || footerIdx == -1 || replyFormIdx == -1 || !(lastMsgIdx < footerIdx && footerIdx < replyFormIdx) {
			t.Errorf("thread-%d expected .thread-footer after last message (%s at %d) and before reply-form (%d), got footerIdx=%d", tc.threadID, lastMsgMarker, lastMsgIdx, replyFormIdx, footerIdx)
		}
	}
	// Published non-draft message must NOT render a hidden .msg-edit-form.
	if !strings.Contains(pageHTML, fmt.Sprintf(`id="msg-%d"`, mResPublishedRoot.ID)) {
		t.Errorf("expected published root message id=\"msg-%d\" in rendered HTML", mResPublishedRoot.ID)
	}
	if strings.Contains(pageHTML, fmt.Sprintf(`id="msg-edit-%d"`, mResPublishedRoot.ID)) {
		t.Errorf("expected published non-draft message %d to omit #msg-edit-%d", mResPublishedRoot.ID, mResPublishedRoot.ID)
	}

	// 2. POST /api/threads/edit toggles resolution when resolved is provided, and preserves it when omitted.
	postEdit := func(payload map[string]any) map[string]any {
		raw, _ := json.Marshal(payload)
		r, err := http.Post(ts.URL+"/api/threads/edit", "application/json", bytes.NewReader(raw))
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/threads/edit failed: status=%v err=%v", r.StatusCode, err)
		}
		defer r.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// 2a. Unresolved root draft -> mark resolved: true
	resA := postEdit(map[string]any{
		"message_id": mUnresDraft.ID,
		"body":       "edited and resolved",
		"resolved":   true,
	})
	thAfterA, err := d.ThreadByID(thUnresDraft.ID)
	if err != nil || thAfterA == nil || !thAfterA.Resolved {
		t.Fatalf("expected DB thread %d resolved=true, got %+v (err=%v)", thUnresDraft.ID, thAfterA, err)
	}
	htmlA, _ := resA["html"].(string)
	if strings.Contains(htmlA, "thread-unresolved") || !strings.Contains(htmlA, `data-resolved="true"`) || !strings.Contains(htmlA, "Reopen") || !strings.Contains(htmlA, "badge-reviewed") {
		t.Errorf("expected resolved card HTML after edit, got: %s", htmlA)
	}
	ccA, _ := resA["change_counts"].(map[string]any)
	fcA, _ := resA["file_counts"].(map[string]any)
	if ccA["unresolved"] != float64(0) || ccA["resolved"] != float64(3) || fcA["resolved"] != float64(3) {
		t.Errorf("expected change/file counts unresolved=0 resolved=3, got change=%v file=%v", ccA, fcA)
	}

	// 2b. Draft reply on resolved thread -> reopen (resolved: false)
	resB := postEdit(map[string]any{
		"message_id": mResDraftReply.ID,
		"body":       "reopening via draft reply edit",
		"resolved":   false,
	})
	thAfterB, err := d.ThreadByID(thResWithReply.ID)
	if err != nil || thAfterB == nil || thAfterB.Resolved {
		t.Fatalf("expected DB thread %d resolved=false, got %+v (err=%v)", thResWithReply.ID, thAfterB, err)
	}
	htmlB, _ := resB["html"].(string)
	if !strings.Contains(htmlB, "thread-unresolved") || !strings.Contains(htmlB, `data-resolved="false"`) || !strings.Contains(htmlB, "Resolve") {
		t.Errorf("expected unresolved card HTML after edit, got: %s", htmlB)
	}
	ccB, _ := resB["change_counts"].(map[string]any)
	if ccB["unresolved"] != float64(1) || ccB["resolved"] != float64(2) {
		t.Errorf("expected change counts unresolved=1 resolved=2, got %v", ccB)
	}

	// 2c. Edit omitting "resolved" preserves existing resolution state (currently false).
	resC := postEdit(map[string]any{
		"message_id": mResDraftReply.ID,
		"body":       "body only edit",
	})
	thAfterC, err := d.ThreadByID(thResWithReply.ID)
	if err != nil || thAfterC == nil || thAfterC.Resolved {
		t.Fatalf("expected DB thread %d to preserve resolved=false when omitted, got %+v", thResWithReply.ID, thAfterC)
	}
	if thObj, ok := resC["thread"].(map[string]any); !ok || thObj["resolved"] != false {
		t.Errorf("expected thread.resolved=false in response, got %v", resC["thread"])
	}

	// 3. GET /api/threads/list explicitly emits boolean "resolved" for unresolved, resolved, and draft.
	checkList := func(status string) ([]map[string]any, string) {
		u := fmt.Sprintf("%s/api/threads/list?status=%s&repo=%s&change=%s", ts.URL, status, rec.Slug, c.Key)
		r, err := http.Get(u)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s failed: status=%v err=%v", u, r.StatusCode, err)
		}
		defer r.Body.Close()
		rawBytes, _ := io.ReadAll(r.Body)
		var parsed struct {
			Threads []map[string]any `json:"threads"`
		}
		if err := json.Unmarshal(rawBytes, &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed.Threads, string(rawBytes)
	}

	unresItems, unresRaw := checkList("unresolved")
	if len(unresItems) != 1 || unresItems[0]["resolved"] != false || !strings.Contains(unresRaw, `"resolved":false`) {
		t.Errorf("expected unresolved list item with explicit resolved:false, got %s", unresRaw)
	}
	resItems, resRaw := checkList("resolved")
	if len(resItems) != 2 || resItems[0]["resolved"] != true || resItems[1]["resolved"] != true || !strings.Contains(resRaw, `"resolved":true`) {
		t.Errorf("expected resolved list items with explicit resolved:true, got %s", resRaw)
	}
	draftItems, draftRaw := checkList("draft")
	if len(draftItems) != 3 || !strings.Contains(draftRaw, `"resolved":false`) || !strings.Contains(draftRaw, `"resolved":true`) {
		t.Errorf("expected draft list with both resolved:false and resolved:true, got %s", draftRaw)
	}

	// 4. Node VM tests: Cmd/Ctrl+Enter across all 3 forms, live card color + sibling sync + checkbox-only draft + Cancel/Escape restore, and menu row classes.
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	nodeScript := fmt.Sprintf(`
const vm = require("vm");
const fs = require("fs");

const winListeners = {};
const docListeners = {};
const fetches = [];

function createEl(tag) {
  const children = [];
  const attrs = {};
  const classes = new Set();
  const el = {
    tagName: tag.toUpperCase(),
    id: "",
    name: "",
    type: "",
    value: "",
    checked: false,
    scrollHeight: 60,
    style: {},
    parentNode: null,
    childNodes: children,
    get children() { return children; },
    get firstElementChild() { return children.length > 0 ? children[0] : null; },
    get parentElement() { return el.parentNode; },
    get nextSibling() {
      if (!el.parentNode) return null;
      const sibs = el.parentNode.childNodes;
      const idx = sibs.indexOf(el);
      return (idx >= 0 && idx + 1 < sibs.length) ? sibs[idx + 1] : null;
    },
    getAttribute(k) { return attrs[k] !== undefined ? attrs[k] : null; },
    setAttribute(k, v) { attrs[k] = String(v); },
    hasAttribute(k) { return attrs[k] !== undefined; },
    removeAttribute(k) { delete attrs[k]; },
    focus() { el._focused = true; },
    blur() { el._blurred = true; },
    scrollIntoView() {},
    getBoundingClientRect() { return { top: 50, bottom: 70, left: 50, right: 150, width: 100, height: 20 }; },
    appendChild(child) {
      child.parentNode = el;
      children.push(child);
      return child;
    },
    contains(other) {
      let cur = other;
      while (cur) {
        if (cur === el) return true;
        cur = cur.parentNode;
      }
      return false;
    },
    insertBefore(child, ref) {
      child.parentNode = el;
      if (!ref) {
        children.push(child);
        return child;
      }
      const idx = children.indexOf(ref);
      if (idx === -1) children.push(child);
      else children.splice(idx, 0, child);
      return child;
    },
    replaceWith(...nodes) {
      if (!el.parentNode) return;
      const sibs = el.parentNode.childNodes;
      const idx = sibs.indexOf(el);
      if (idx === -1) return;
      for (const n of nodes) {
        if (n && typeof n === "object") n.parentNode = el.parentNode;
      }
      sibs.splice(idx, 1, ...nodes);
      el.parentNode = null;
    },
    remove() {
      if (el.parentNode) {
        const idx = el.parentNode.childNodes.indexOf(el);
        if (idx !== -1) el.parentNode.childNodes.splice(idx, 1);
        el.parentNode = null;
      }
    },
    closest(sel) {
      const parts = sel.split(",").map(s => s.trim());
      let cur = el;
      while (cur) {
        for (const p of parts) {
          if (matchesSimpleSelector(cur, p)) return cur;
        }
        cur = cur.parentNode;
      }
      return null;
    },
    querySelector(sel) {
      const all = el.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const parts = sel.split(",").map(s => s.trim());
      const res = [];
      function walk(node) {
        for (const c of node.childNodes) {
          for (const p of parts) {
            if (matchesSimpleSelector(c, p) && !res.includes(c)) {
              res.push(c);
            }
          }
          walk(c);
        }
      }
      walk(el);
      return res;
    },
    addEventListener(ev, fn) {
      if (!el._listeners) el._listeners = {};
      if (!el._listeners[ev]) el._listeners[ev] = [];
      el._listeners[ev].push(fn);
    },
    removeEventListener() {}
  };

  Object.defineProperty(el, "className", {
    get() { return Array.from(classes).join(" "); },
    set(v) {
      classes.clear();
      String(v || "").trim().split(/\s+/).forEach(c => { if (c) classes.add(c); });
    }
  });
  // Intentionally omit 2-arg classList.toggle to verify add/remove compatibility across mock DOMs.
  el.classList = {
    add(...cls) { cls.forEach(c => classes.add(c)); },
    remove(...cls) { cls.forEach(c => classes.delete(c)); },
    contains(c) { return classes.has(c); }
  };

  Object.defineProperty(el, "innerHTML", {
    get() { return el._html || ""; },
    set(v) {
      el._html = String(v || "");
      children.length = 0;
      // Parse row menu items or openCommentForm snippet cleanly for querySelector/class assertions.
      const rowRe = /<div\s+class="([^"]*unres-item-row[^"]*)"([^>]*)>/g;
      let m;
      while ((m = rowRe.exec(el._html)) !== null) {
        const row = createEl("DIV");
        row.className = m[1];
        const attrRe = /([a-z-]+)="([^"]*)"/g;
        let am;
        while ((am = attrRe.exec(m[2])) !== null) row.setAttribute(am[1], am[2]);
        el.appendChild(row);
      }
      if (el._html.includes("new-comment-card")) {
        const card = createEl("DIV");
        card.className = "thread-card comment-form new-comment-card thread-unresolved";
        const form = createEl("FORM");
        form.onsubmit = function(ev) { win.submitInlineComment(ev, 20, 20, "R", 0, 0); };
        const ta = createEl("TEXTAREA");
        ta.id = "inlineCommentText";
        const lbl = createEl("LABEL");
        lbl.className = "resolved-toggle";
        const cb = createEl("INPUT");
        cb.type = "checkbox";
        cb.name = "resolved";
        cb.checked = false;
        lbl.appendChild(cb);
        form.appendChild(ta);
        form.appendChild(lbl);
        card.appendChild(form);
        el.appendChild(card);
      }
      if (el._html.includes('class="thread-card') && !el._html.includes("new-comment-card")) {
        const tm = el._html.match(/<div\s+id="(thread-\d+)"\s+class="([^"]*)"[^>]*data-resolved="(true|false)"/);
        if (tm) {
          const card = createEl("DIV");
          card.id = tm[1];
          card.className = tm[2];
          card.setAttribute("data-resolved", tm[3]);
          const mbRe = /<div\s+class="message-body[^"]*"\s+id="(msg-\d+)"/g;
          let mbm;
          while ((mbm = mbRe.exec(el._html)) !== null) {
            const mb = createEl("DIV");
            mb.id = mbm[1];
            mb.className = "message-body";
            card.appendChild(mb);
          }
          const efRe = /<div\s+class="msg-edit-form"\s+id="msg-edit-(\d+)"\s+data-orig-body="([^"]*)"([^>]*)>([\s\S]*?)<\/form><\/div>/g;
          let efm;
          while ((efm = efRe.exec(el._html)) !== null) {
            const eid = Number(efm[1]);
            const ef = createEl("DIV");
            ef.id = "msg-edit-" + efm[1];
            ef.className = "msg-edit-form";
            ef.setAttribute("data-orig-body", efm[2]);
            ef.style.display = "none";
            const form = createEl("FORM");
            form.onsubmit = function(ev) { win.submitEditMessage(ev, eid); };
            const ta = createEl("TEXTAREA");
            const tam = efm[4].match(/<textarea[^>]*>([\s\S]*?)<\/textarea>/);
            ta.value = tam ? tam[1] : efm[2];
            const cb = createEl("INPUT");
            cb.type = "checkbox";
            cb.name = "resolved";
            cb.checked = /<input[^>]*name="resolved"[^>]*checked/.test(efm[4]);
            form.appendChild(ta);
            form.appendChild(cb);
            ef.appendChild(form);
            card.appendChild(ef);
          }
          const rfM = el._html.match(/<div\s+class="comment-form reply-form"\s+id="reply-form-(\d+)"([^>]*)>([\s\S]*?)<\/form><\/div>/);
          if (rfM) {
            const tid = Number(rfM[1]);
            const rf = createEl("DIV");
            rf.id = "reply-form-" + rfM[1];
            rf.className = "comment-form reply-form";
            rf.style.display = "none";
            const form = createEl("FORM");
            form.onsubmit = function(ev) { win.replyThread(ev, tid); };
            const ta = createEl("TEXTAREA");
            ta.value = "";
            const cb = createEl("INPUT");
            cb.type = "checkbox";
            cb.name = "resolved";
            cb.checked = /<input[^>]*name="resolved"[^>]*checked/.test(rfM[3]);
            form.appendChild(ta);
            form.appendChild(cb);
            rf.appendChild(form);
            card.appendChild(rf);
          }
          el.appendChild(card);
        }
      }
    }
  });

  Object.defineProperty(el, "outerHTML", {
    get() { return el._outerHTML || ""; },
    set(v) { el._outerHTML = String(v || ""); }
  });

  return el;
}

function matchesSimpleSelector(node, sel) {
  if (!node || !node.tagName || !sel) return false;
  sel = sel.trim();
  const parts = sel.split(/\s+/);
  if (parts.length > 1) {
    if (!matchesSimpleSelector(node, parts[parts.length - 1])) return false;
    let anc = node.parentNode;
    for (let i = parts.length - 2; i >= 0; i--) {
      while (anc && !matchesSimpleSelector(anc, parts[i])) {
        anc = anc.parentNode;
      }
      if (!anc) return false;
      anc = anc.parentNode;
    }
    return true;
  }
  if (sel.startsWith("#")) return node.id === sel.slice(1);
  const mEq = sel.match(/^([a-z]+)?(?:\.([a-z0-9_.-]+))?\[([a-z0-9_-]+)="([^"]*)"\]$/i);
  if (mEq) {
    if (mEq[1] && node.tagName !== mEq[1].toUpperCase()) return false;
    if (mEq[2] && !mEq[2].split(".").every(c => node.classList && node.classList.contains(c))) return false;
    const attr = mEq[3];
    const val = mEq[4];
    if (attr === "name" && node.name === val) return true;
    return !!(node.getAttribute && node.getAttribute(attr) === val);
  }
  const mPres = sel.match(/^([a-z]+)?(?:\.([a-z0-9_.-]+))?\[([a-z0-9_-]+)\]$/i);
  if (mPres) {
    if (mPres[1] && node.tagName !== mPres[1].toUpperCase()) return false;
    if (mPres[2] && !mPres[2].split(".").every(c => node.classList && node.classList.contains(c))) return false;
    const attr = mPres[3];
    return !!(node.hasAttribute && node.hasAttribute(attr));
  }
  if (sel.startsWith(".")) {
    const clsList = sel.slice(1).split(".");
    return clsList.every(c => node.classList && node.classList.contains(c));
  }
  return node.tagName === sel.toUpperCase();
}

const body = createEl("BODY");
function findById(node, id) {
  if (!node) return null;
  if (node.id === id) return node;
  for (const c of (node.childNodes || [])) {
    const f = findById(c, id);
    if (f) return f;
  }
  return null;
}

const countsEl = createEl("DIV");
countsEl.id = "changeCommentCounts";
countsEl.setAttribute("data-repo", %q);
countsEl.setAttribute("data-change", %q);
countsEl.setAttribute("data-base", "parent");
countsEl.setAttribute("data-target", "live");
const unresBadge = createEl("SPAN");
unresBadge.className = "badge badge-count-unresolved";
unresBadge.textContent = "1 unresolved";
const resBadge = createEl("SPAN");
resBadge.className = "badge badge-count-resolved";
resBadge.textContent = "2 resolved";
const draftBadge = createEl("SPAN");
draftBadge.className = "badge badge-draft";
draftBadge.textContent = "2 draft";
countsEl.appendChild(unresBadge);
countsEl.appendChild(resBadge);
countsEl.appendChild(draftBadge);
body.appendChild(countsEl);

const diffCont = createEl("DIV");
diffCont.className = "diff-container";
diffCont.setAttribute("data-repo", %q);
diffCont.setAttribute("data-change", %q);
diffCont.setAttribute("data-file-path", "hello.go");
diffCont.setAttribute("data-left-rev", "base1");
diffCont.setAttribute("data-right-rev", "head1");
body.appendChild(diffCont);

const table = createEl("TABLE");
table.className = "diff-table diff-table-unified";
const tbody = createEl("TBODY");
const tr20 = createEl("TR");
tr20.setAttribute("data-line-r", "20");
tbody.appendChild(tr20);
table.appendChild(tbody);
diffCont.appendChild(table);

// Thread 101: resolved card with both #reply-form-101 and #msg-edit-202 inside <form>
const card101 = createEl("DIV");
card101.id = "thread-101";
card101.className = "thread-card";
card101.setAttribute("data-resolved", "true");
diffCont.appendChild(card101);

const msgBody202 = createEl("DIV");
msgBody202.id = "msg-202";
msgBody202.className = "message-body";
card101.appendChild(msgBody202);

const msgContent202 = createEl("DIV");
msgContent202.id = "msg-content-202";
msgBody202.appendChild(msgContent202);

const editWrap202 = createEl("DIV");
editWrap202.id = "msg-edit-202";
editWrap202.className = "msg-edit-form";
editWrap202.setAttribute("data-orig-body", "Orig reply");
editWrap202.style.display = "none";
const editForm202 = createEl("FORM");
editForm202.onsubmit = function(ev) { win.submitEditMessage(ev, 202); };
editForm202.requestSubmit = function() {
  const ev = { preventDefault() { this.defaultPrevented = true; }, defaultPrevented: false, target: editForm202 };
  editForm202.onsubmit(ev);
};
const editTa202 = createEl("TEXTAREA");
editTa202.id = "edit-ta-202";
editTa202.value = "Orig reply";
const editLbl202 = createEl("LABEL");
editLbl202.className = "resolved-toggle";
const editCb202 = createEl("INPUT");
editCb202.type = "checkbox";
editCb202.name = "resolved";
editCb202.checked = true;
editLbl202.appendChild(editCb202);
editForm202.appendChild(editTa202);
editForm202.appendChild(editLbl202);
editWrap202.appendChild(editForm202);
msgBody202.appendChild(editWrap202);

const replyWrap101 = createEl("DIV");
replyWrap101.id = "reply-form-101";
replyWrap101.className = "comment-form reply-form";
replyWrap101.style.display = "none";
const replyForm101 = createEl("FORM");
replyForm101.onsubmit = function(ev) { win.replyThread(ev, 101); };
replyForm101.requestSubmit = function() {
  const ev = { preventDefault() { this.defaultPrevented = true; }, defaultPrevented: false, target: replyForm101 };
  replyForm101.onsubmit(ev);
};
const replyTa101 = createEl("TEXTAREA");
replyTa101.value = "";
const replyCb101 = createEl("INPUT");
replyCb101.type = "checkbox";
replyCb101.name = "resolved";
replyCb101.checked = true;
replyForm101.appendChild(replyTa101);
replyForm101.appendChild(replyCb101);
replyWrap101.appendChild(replyForm101);
card101.appendChild(replyWrap101);

// Thread 200 / legacy edit form 201 without requestSubmit and without resolved checkbox
const legacyEdit201 = createEl("DIV");
legacyEdit201.id = "msg-edit-201";
legacyEdit201.className = "msg-edit-form";
legacyEdit201.style.display = "block";
legacyEdit201.setAttribute("data-orig-body", "Legacy");
const legacyTa201 = createEl("TEXTAREA");
legacyTa201.value = "Legacy updated";
legacyEdit201.appendChild(legacyTa201);
diffCont.appendChild(legacyEdit201);

const doc = {
  body: body,
  activeElement: null,
  documentElement: {
    classList: { contains() { return false; }, add() {}, remove() {} },
    style: { setProperty() {}, getPropertyValue() { return ""; } },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {}
  },
  getElementById(id) { return findById(body, id); },
  createElement(tag) { return createEl(tag); },
  createTextNode(txt) { return { nodeType: 3, textContent: txt, parentNode: null }; },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); },
  addEventListener(ev, fn) {
    if (!docListeners[ev]) docListeners[ev] = [];
    docListeners[ev].push(fn);
  },
  removeEventListener() {}
};

const ls = new Map();
const win = {
  document: doc,
  location: { pathname: "/r/" + %q + "/c/" + %q, search: "", hash: "", href: "" },
  history: { replaceState() {} },
  addEventListener(ev, fn) {
    if (!winListeners[ev]) winListeners[ev] = [];
    winListeners[ev].push(fn);
  },
  removeEventListener() {},
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: {
    get length() { return ls.size; },
    key(i) { return Array.from(ls.keys())[i] || null; },
    getItem(k) { return ls.has(k) ? ls.get(k) : null; },
    setItem(k, v) { ls.set(k, String(v)); },
    removeItem(k) { ls.delete(k); }
  },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; },
  fetch(url, opts) {
    fetches.push({ url, opts });
    if (win._customFetch) return win._customFetch(url, opts);
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ sig: "s1" }) });
  }
};
win.window = win;

const sandbox = vm.createContext({
  window: win,
  document: doc,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  URLSearchParams: URLSearchParams,
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  fetch: win.fetch,
  NodeFilter: { SHOW_TEXT: 4 },
  Node: { ELEMENT_NODE: 1, TEXT_NODE: 3 }
});

vm.runInContext(fs.readFileSync("static/app.js", "utf8"), sandbox);

(async () => {
  const keydown = winListeners["keydown"][0];
  if (!keydown) throw new Error("missing window keydown listener");

  // A. Cmd+Enter / Ctrl+Enter on openCommentForm (textarea, checkbox, and label)
  win.openCommentForm(tr20, 20, "R");
  const inlineRow = doc.getElementById("inlineNewComment");
  const inlineTa = inlineRow.querySelector("textarea");
  const inlineCb = inlineRow.querySelector('input[name="resolved"]');
  const inlineLbl = inlineRow.querySelector(".resolved-toggle");
  const inlineForm = inlineRow.querySelector("form");
  if (!inlineCb || !inlineForm) throw new Error("missing form or checkbox in openCommentForm");
  if (inlineCb.checked !== false) throw new Error("expected new inline comment Resolved checkbox unchecked by default");

  inlineTa.value = "New comment text";
  inlineCb.checked = true;
  let prevented = false;
  keydown({ metaKey: true, ctrlKey: false, key: "Enter", target: inlineTa, preventDefault() { prevented = true; } });
  if (!prevented) throw new Error("expected preventDefault on Cmd+Enter in inline textarea");
  await new Promise(r => setTimeout(r, 15));
  const add1 = fetches.find(f => f.url === "/api/threads/add");
  if (!add1) throw new Error("expected Cmd+Enter in inline textarea to submit /api/threads/add");
  const addBody1 = JSON.parse(add1.opts.body);
  if (addBody1.body !== "New comment text" || addBody1.resolved !== true) {
    throw new Error("unexpected inline Cmd+Enter payload: " + add1.opts.body);
  }

  // Test Ctrl+Enter with focus on checkbox & label even when requestSubmit is absent
  fetches.length = 0;
  delete inlineForm.requestSubmit;
  inlineCb.checked = false;
  prevented = false;
  keydown({ metaKey: false, ctrlKey: true, key: "Enter", target: inlineCb, preventDefault() { prevented = true; } });
  if (!prevented) throw new Error("expected preventDefault on Ctrl+Enter in inline checkbox");
  await new Promise(r => setTimeout(r, 15));
  if (!fetches.some(f => f.url === "/api/threads/add" && JSON.parse(f.opts.body).resolved === false)) {
    throw new Error("expected Ctrl+Enter on checkbox to submit unresolved comment");
  }
  fetches.length = 0;
  prevented = false;
  keydown({ metaKey: true, ctrlKey: false, key: "Enter", target: inlineLbl, preventDefault() { prevented = true; } });
  if (!prevented) throw new Error("expected preventDefault on Cmd+Enter in inline label");
  await new Promise(r => setTimeout(r, 15));
  if (!fetches.some(f => f.url === "/api/threads/add")) {
    throw new Error("expected Cmd+Enter on .resolved-toggle label to submit");
  }
  win.closeInlineComment(true);

  // Hotkey 'j' + 'c' opens inline comment form cleanly without ReferenceError when window !== globalThis
  keydown({ metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, key: "j", target: body, preventDefault() {} });
  let cPrevented = false;
  keydown({ metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, key: "c", target: body, preventDefault() { cPrevented = true; } });
  if (!cPrevented || !doc.getElementById("inlineNewComment")) {
    throw new Error("expected hotkey 'c' to call openCommentForm and render #inlineNewComment");
  }
  win.closeInlineComment(true);

  // B. Edit form on resolved thread #101: live color, editing-msg class, sibling sync, checkbox-only draft, Escape/Cancel restore
  win.openEditMessageForm(202, { noFocus: true });
  if (editWrap202.style.display !== "block" || card101.classList.contains("thread-unresolved")) {
    throw new Error("expected resolved thread card without thread-unresolved initially");
  }
  if (!msgBody202.classList.contains("editing-msg")) {
    throw new Error("expected openEditMessageForm to add editing-msg to .message-body");
  }

  // Uncheck Resolved without editing text -> live class + sibling replyCb sync + draft saved
  editCb202.checked = false;
  win.handleEditResolvedChange(editCb202, 202);
  if (!card101.classList.contains("thread-unresolved")) {
    throw new Error("expected card101 to gain thread-unresolved when edit Resolved checkbox unchecked");
  }
  if (replyCb101.checked !== false) {
    throw new Error("expected sibling replyCb101 synced to false");
  }
  const editKey202 = "review_draft:edit:" + %q + ":" + %q + ":202";
  const savedCheckboxOnly = win.formDraftsMap.get(editKey202);
  if (!savedCheckboxOnly || savedCheckboxOnly.content !== "Orig reply" || savedCheckboxOnly.unresolved !== true) {
    throw new Error("expected checkbox-only edit draft saved, got: " + JSON.stringify(savedCheckboxOnly));
  }

  // Re-check Resolved back ON while text unchanged -> draft cleared and card highlight removed
  editCb202.checked = true;
  win.handleEditResolvedChange(editCb202, 202);
  if (card101.classList.contains("thread-unresolved") || win.formDraftsMap.has(editKey202) || replyCb101.checked !== true) {
    throw new Error("expected draft cleared and highlight removed when checkbox reverted");
  }

  // Uncheck Resolved, Escape -> hides form, removes editing-msg, restores card color/cb, KEEPS draft
  editCb202.checked = false;
  win.handleEditResolvedChange(editCb202, 202);
  win.closeEditMessageForm(202, false);
  if (editWrap202.style.display !== "none" || msgBody202.classList.contains("editing-msg") || card101.classList.contains("thread-unresolved") || editCb202.checked !== true || replyCb101.checked !== true) {
    throw new Error("expected Escape closeEditMessageForm to restore original resolved state and remove editing-msg");
  }
  if (!win.formDraftsMap.has(editKey202)) {
    throw new Error("expected Escape to keep checkbox-only edit draft in formDraftsMap");
  }

  // restoreSavedDrafts -> reopens msg-edit-202, adds editing-msg, restores checkbox + card yellow
  win.restoreSavedDrafts();
  if (editWrap202.style.display !== "block" || !msgBody202.classList.contains("editing-msg") || editCb202.checked !== false || !card101.classList.contains("thread-unresolved")) {
    throw new Error("expected restoreSavedDrafts to reopen msg-edit-202 with unresolved=true, editing-msg, and thread-unresolved");
  }

  // Cancel -> clears draft, restores original body, hides form, removes editing-msg, restores card color & checkbox, and sends string message_id in delete payload
  fetches.length = 0;
  editTa202.value = "Temp text";
  win.closeEditMessageForm(202, true);
  if (win.formDraftsMap.has(editKey202) || editTa202.value !== "Orig reply" || editCb202.checked !== true || card101.classList.contains("thread-unresolved") || msgBody202.classList.contains("editing-msg")) {
    throw new Error("expected Cancel to revert text, checkbox, card class, editing-msg, and clear draft");
  }
  const editDelFetch = fetches.find(f => f.url.startsWith("/api/form-drafts") && JSON.parse(f.opts.body).action === "delete");
  if (!editDelFetch || typeof JSON.parse(editDelFetch.opts.body).message_id !== "string" || JSON.parse(editDelFetch.opts.body).message_id !== "202") {
    throw new Error("expected deleteFormDraftServer to send string message_id='202', got: " + JSON.stringify(editDelFetch));
  }

  // Cmd/Ctrl+Enter inside msg-edit-202 (from checkbox & textarea)
  win.openEditMessageForm(202, { noFocus: true });
  editTa202.value = "Edited reply";
  editCb202.checked = false;
  win.handleEditResolvedChange(editCb202, 202);
  fetches.length = 0;
  prevented = false;
  keydown({ metaKey: false, ctrlKey: true, key: "Enter", target: editCb202, preventDefault() { prevented = true; } });
  if (!prevented) throw new Error("expected preventDefault on Ctrl+Enter in edit checkbox");
  await new Promise(r => setTimeout(r, 15));
  const editFetch = fetches.find(f => f.url === "/api/threads/edit");
  if (!editFetch) throw new Error("expected Ctrl+Enter in edit checkbox to post /api/threads/edit");
  const editPayload = JSON.parse(editFetch.opts.body);
  if (editPayload.message_id !== 202 || editPayload.body !== "Edited reply" || editPayload.resolved !== false) {
    throw new Error("unexpected edit submit payload: " + editFetch.opts.body);
  }
  win.closeEditMessageForm(202, true);

  // Legacy 1-arg submitEditMessage(201) without resolved checkbox omits resolved
  fetches.length = 0;
  win.submitEditMessage(201);
  await new Promise(r => setTimeout(r, 15));
  const legacyEditFetch = fetches.find(f => f.url === "/api/threads/edit");
  if (!legacyEditFetch) throw new Error("expected legacy 1-arg submitEditMessage to post /api/threads/edit");
  const legacyEditPayload = JSON.parse(legacyEditFetch.opts.body);
  if ("resolved" in legacyEditPayload) {
    throw new Error("expected 1-arg submitEditMessage without checkbox to omit resolved, got: " + legacyEditFetch.opts.body);
  }

  // Fallback Ctrl+Enter on legacy msg-edit-201 without <form> wrapper
  fetches.length = 0;
  prevented = false;
  keydown({ metaKey: true, ctrlKey: false, key: "Enter", target: legacyTa201, preventDefault() { prevented = true; } });
  if (!prevented) throw new Error("expected preventDefault on Cmd+Enter in fallback edit wrapper");
  await new Promise(r => setTimeout(r, 15));
  if (!fetches.some(f => f.url === "/api/threads/edit" && JSON.parse(f.opts.body).message_id === 201)) {
    throw new Error("expected fallback Cmd+Enter in msg-edit-201 to invoke submitEditMessage(201)");
  }
  win.closeEditMessageForm(201, true);

  // C. Reply form toggle (reply-open class), live color, Cancel & isClosing restore, sibling open-form guard, and Cmd+Enter
  win.toggleReplyForm(101, false, { noFocus: true });
  if (!card101.classList.contains("reply-open")) {
    throw new Error("expected toggleReplyForm open to add reply-open to .thread-card");
  }
  replyTa101.value = "Reopening via reply";
  replyCb101.checked = false;
  win.handleReplyResolvedChange(replyCb101, 101);
  if (!card101.classList.contains("thread-unresolved") || editCb202.checked !== false) {
    throw new Error("expected reply checkbox change to color card101 and sync editCb202");
  }
  // Open edit form while reply form is open, then close edit form -> still-open reply form keeps card unresolved and reply-open!
  win.openEditMessageForm(202, { noFocus: true });
  if (!msgBody202.classList.contains("editing-msg")) {
    throw new Error("expected editing-msg added when edit form opened alongside reply form");
  }
  win.closeEditMessageForm(202, true);
  if (!card101.classList.contains("thread-unresolved") || !card101.classList.contains("reply-open") || replyCb101.checked !== false || msgBody202.classList.contains("editing-msg")) {
    throw new Error("expected still-open reply form to preserve thread-unresolved and reply-open when edit form closes");
  }
  // Toggle reply form closed (non-cancel isClosing branch) -> removes reply-open and restores card101 to data-resolved="true"
  win.toggleReplyForm(101, false);
  if (replyWrap101.style.display !== "none" || card101.classList.contains("reply-open") || card101.classList.contains("thread-unresolved") || replyCb101.checked !== true || editCb202.checked !== true) {
    throw new Error("expected closing reply form to remove reply-open and restore original resolved card state and checkboxes");
  }
  // Open reply, uncheck Resolved, Cancel -> removes reply-open, restores card101, and clears reply draft with string thread_id
  win.toggleReplyForm(101, false, { noFocus: true });
  if (!card101.classList.contains("reply-open")) {
    throw new Error("expected reply-open on card101 when reopening reply form");
  }
  replyTa101.value = "Draft reply";
  replyCb101.checked = false;
  win.handleReplyResolvedChange(replyCb101, 101);
  fetches.length = 0;
  win.toggleReplyForm(101, true);
  if (card101.classList.contains("reply-open") || card101.classList.contains("thread-unresolved") || replyCb101.checked !== true || win.formDraftsMap.has("review_draft:reply:" + %q + ":" + %q + ":101")) {
    throw new Error("expected reply Cancel to remove reply-open, clear draft, and restore card state");
  }
  const repDelFetch = fetches.find(f => f.url.startsWith("/api/form-drafts") && JSON.parse(f.opts.body).action === "delete");
  if (!repDelFetch || typeof JSON.parse(repDelFetch.opts.body).thread_id !== "string" || JSON.parse(repDelFetch.opts.body).thread_id !== "101") {
    throw new Error("expected reply Cancel deleteFormDraftServer to send string thread_id='101', got: " + JSON.stringify(repDelFetch));
  }

  // Cmd+Enter in reply checkbox submits /api/threads/reply
  win.toggleReplyForm(101, false, { noFocus: true });
  replyTa101.value = "Submitted reply";
  replyCb101.checked = true;
  fetches.length = 0;
  prevented = false;
  keydown({ metaKey: true, ctrlKey: false, key: "Enter", target: replyCb101, preventDefault() { prevented = true; } });
  if (!prevented) throw new Error("expected preventDefault on Cmd+Enter in reply checkbox");
  await new Promise(r => setTimeout(r, 15));
  const repFetch = fetches.find(f => f.url === "/api/threads/reply");
  if (!repFetch || JSON.parse(repFetch.opts.body).resolved !== true || JSON.parse(repFetch.opts.body).body !== "Submitted reply") {
    throw new Error("unexpected reply Cmd+Enter payload: " + JSON.stringify(repFetch));
  }

  // D. Menu row color classes across draft, unresolved, resolved lists
  win.enhanceChangeUnresolvedBadge();
  win._customFetch = (url) => {
    if (url.includes("status=draft")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          threads: [
            { id: 1, message_id: 11, resolved: false, path: "hello.go", line: 1, has_draft: true, author: "alice", preview: "unres draft" },
            { id: 2, message_id: 22, resolved: true, path: "hello.go", line: 2, has_draft: true, author: "bob", preview: "res draft" }
          ]
        })
      });
    }
    if (url.includes("status=resolved")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          threads: [
            { id: 3, resolved: true, path: "hello.go", line: 3, author: "carol", preview: "resolved thread" }
          ]
        })
      });
    }
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        threads: [
          { id: 4, resolved: false, path: "hello.go", line: 4, author: "dave", preview: "unres thread" }
        ]
      })
    });
  };

  for (const fn of (docListeners.click || [])) {
    fn({ target: draftBadge, preventDefault() {}, stopPropagation() {} });
  }
  await new Promise(r => setTimeout(r, 20));
  const menu = body.querySelector(".unresolved-threads-menu");
  const draftRows = menu.childNodes.filter(n => n.classList && n.classList.contains("unres-item-row"));
  if (draftRows.length !== 2 ||
      !draftRows[0].classList.contains("item-unresolved") || draftRows[0].classList.contains("item-resolved") ||
      !draftRows[1].classList.contains("item-resolved") || draftRows[1].classList.contains("item-unresolved")) {
    throw new Error("expected draft rows to have item-unresolved then item-resolved, got: " + draftRows.map(r => r.className).join(" | "));
  }

  for (const fn of (docListeners.click || [])) {
    fn({ target: resBadge, preventDefault() {}, stopPropagation() {} });
  }
  await new Promise(r => setTimeout(r, 20));
  const resRows = menu.childNodes.filter(n => n.classList && n.classList.contains("unres-item-row"));
  if (resRows.length !== 1 || !resRows[0].classList.contains("item-resolved")) {
    throw new Error("expected resolved row to have item-resolved, got: " + resRows.map(r => r.className).join(" | "));
  }

  for (const fn of (docListeners.click || [])) {
    fn({ target: unresBadge, preventDefault() {}, stopPropagation() {} });
  }
  await new Promise(r => setTimeout(r, 20));
  const unresRows = menu.childNodes.filter(n => n.classList && n.classList.contains("unres-item-row"));
  if (unresRows.length !== 1 || !unresRows[0].classList.contains("item-unresolved")) {
    throw new Error("expected unresolved row to have item-unresolved, got: " + unresRows.map(r => r.className).join(" | "));
  }

  // E. Escape close after clearing msg-edit-202 to whitespace resets ta.value to orig
  // Close the unresolved menu opened in section D first.
  for (const fn of (docListeners.click || [])) {
    fn({ target: body, preventDefault() {}, stopPropagation() {} });
  }
  win._customFetch = null;
  win.openEditMessageForm(202, { noFocus: true });
  editTa202.value = "   ";
  win.handleEditMsgInput(editTa202, 202);
  if (win.formDraftsMap.has(editKey202)) {
    throw new Error("expected whitespace-only edit input to delete draft");
  }
  doc.activeElement = editTa202;
  editTa202.blur = () => { if (doc.activeElement === editTa202) doc.activeElement = body; };
  // 1st Escape blurs textarea, 2nd Escape calls closeEditMessageForm(202, false)
  keydown({ key: "Escape", target: editTa202, preventDefault() {} });
  let escPrevented = false;
  keydown({ key: "Escape", target: body, preventDefault() { escPrevented = true; } });
  if (!escPrevented || editWrap202.style.display !== "none" || editTa202.value !== "Orig reply") {
    throw new Error("expected Escape after whitespace clear to hide edit form and reset ta.value to orig, got display=" + editWrap202.style.display + " val=" + JSON.stringify(editTa202.value));
  }
  win.openEditMessageForm(202, { noFocus: true });
  if (editTa202.value !== "Orig reply") {
    throw new Error("expected reopening msg-edit-202 after Escape to show orig body, got: " + JSON.stringify(editTa202.value));
  }
  win.closeEditMessageForm(202, true);

  // F. Double Cmd+Enter / submit guard prevents duplicate in-flight POSTs across inline, edit, and reply forms
  let releasePending;
  let pendingHandler = null;
  win._customFetch = (url, opts) => {
    if (url.startsWith("/api/threads/")) {
      return new Promise(resolve => {
        releasePending = (res) => resolve(res);
        if (pendingHandler) pendingHandler();
      });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ sig: "s1" }) });
  };

  // F1: inline form
  win.openCommentForm(tr20, 20, "R");
  const gInlineRow = doc.getElementById("inlineNewComment");
  const gInlineTa = gInlineRow.querySelector("textarea");
  gInlineTa.value = "Single in-flight inline";
  fetches.length = 0;
  keydown({ metaKey: true, ctrlKey: false, key: "Enter", target: gInlineTa, preventDefault() {} });
  keydown({ metaKey: true, ctrlKey: false, key: "Enter", target: gInlineTa, preventDefault() {} });
  await new Promise(r => setTimeout(r, 10));
  if (fetches.filter(f => f.url === "/api/threads/add").length !== 1) {
    throw new Error("expected double Cmd+Enter on inline form while pending to fire only 1 POST");
  }
  releasePending({ ok: false });
  await new Promise(r => setTimeout(r, 15));
  // After rejection, .finally unlocks _submitting so retry succeeds
  fetches.length = 0;
  keydown({ metaKey: true, ctrlKey: false, key: "Enter", target: gInlineTa, preventDefault() {} });
  await new Promise(r => setTimeout(r, 10));
  if (fetches.filter(f => f.url === "/api/threads/add").length !== 1) {
    throw new Error("expected retry after inline reject to unlock _submitting");
  }
  releasePending({ ok: true, json: () => Promise.resolve({ sig: "s1" }) });
  await new Promise(r => setTimeout(r, 15));
  win.closeInlineComment(true);

  // F2: edit form
  win.openEditMessageForm(202, { noFocus: true });
  editTa202.value = "Single in-flight edit";
  fetches.length = 0;
  keydown({ metaKey: true, ctrlKey: false, key: "Enter", target: editTa202, preventDefault() {} });
  keydown({ metaKey: true, ctrlKey: false, key: "Enter", target: editTa202, preventDefault() {} });
  await new Promise(r => setTimeout(r, 10));
  if (fetches.filter(f => f.url === "/api/threads/edit").length !== 1) {
    throw new Error("expected double Cmd+Enter on edit form while pending to fire only 1 POST");
  }
  releasePending({ ok: true, json: () => Promise.resolve({ sig: "s1" }) });
  await new Promise(r => setTimeout(r, 15));
  win.closeEditMessageForm(202, true);

  // F3: reply form
  win.toggleReplyForm(101, false, { noFocus: true });
  replyTa101.value = "Single in-flight reply";
  fetches.length = 0;
  keydown({ metaKey: false, ctrlKey: true, key: "Enter", target: replyTa101, preventDefault() {} });
  keydown({ metaKey: false, ctrlKey: true, key: "Enter", target: replyTa101, preventDefault() {} });
  await new Promise(r => setTimeout(r, 10));
  if (fetches.filter(f => f.url === "/api/threads/reply").length !== 1) {
    throw new Error("expected double Ctrl+Enter on reply form while pending to fire only 1 POST");
  }
  releasePending({ ok: true, json: () => Promise.resolve({ sig: "s1" }) });
  await new Promise(r => setTimeout(r, 15));
  win.toggleReplyForm(101, true);

  // G. resolveThread syncs d.unresolved on reply/edit drafts, cleans now-no-op edit draft, avoids ID collision, and preserves sibling reply across card replacement
  win._customFetch = (url, opts) => {
    if (url === "/api/threads/resolve") {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          sig: "s2",
          thread: { id: 101, resolved: false, messages: [{ id: 202, body: "Orig reply" }] },
          html: '<div id="thread-101" class="thread-card thread-unresolved" data-resolved="false"><div class="message-body" id="msg-202"></div><div class="msg-edit-form" id="msg-edit-202" data-orig-body="Orig reply" style="display: none;"><form><textarea>Orig reply</textarea><input type="checkbox" name="resolved"></form></div><div class="thread-footer"></div><div class="comment-form reply-form" id="reply-form-101" style="display: none;"><form><textarea></textarea><input type="checkbox" name="resolved"></form></div></div>'
        })
      });
    }
    if (url === "/api/publish") {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          sig: "s3",
          published: 1,
          thread_html: {
            "101": '<div id="thread-101" class="thread-card thread-unresolved" data-resolved="false"><div class="message-body" id="msg-202"></div><div class="thread-footer"></div><div class="comment-form reply-form" id="reply-form-101" style="display: none;"><form><textarea></textarea><input type="checkbox" name="resolved"></form></div></div>'
          }
        })
      });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ sig: "s2" }) });
  };

  card101.setAttribute("data-resolved", "true");
  card101.classList.remove("thread-unresolved");
  win.toggleReplyForm(101, false, { noFocus: true });
  replyTa101.value = "Keep sibling reply";
  replyCb101.checked = true;
  win.handleReplyInput(replyTa101, 101);
  win.openEditMessageForm(202, { noFocus: true });
  editCb202.checked = false;
  win.handleEditResolvedChange(editCb202, 202);
  // Unrelated message whose ID == 101 must NOT be treated as thread 101 reply draft
  const unrelatedEditKey = "review_draft:edit:" + %q + ":" + %q + ":101";
  win.formDraftsMap.set(unrelatedEditKey, { draft_key: unrelatedEditKey, message_id: "101", content: "unrelated", unresolved: false });

  win.resolveThread(101, false);
  await new Promise(r => setTimeout(r, 20));
  const replyKey101 = "review_draft:reply:" + %q + ":" + %q + ":101";
  const syncedReply = win.formDraftsMap.get(replyKey101);
  if (!syncedReply || syncedReply.unresolved !== true || syncedReply.content !== "Keep sibling reply") {
    throw new Error("expected resolveThread to sync reply draft unresolved=true, got: " + JSON.stringify(syncedReply));
  }
  if (win.formDraftsMap.has(editKey202)) {
    throw new Error("expected checkbox-only edit draft to be deleted when thread resolved state now matches");
  }
  const unrelatedAfter = win.formDraftsMap.get(unrelatedEditKey);
  if (!unrelatedAfter || unrelatedAfter.unresolved !== false) {
    throw new Error("expected unrelated message 101 draft to stay untouched by thread 101 resolve, got: " + JSON.stringify(unrelatedAfter));
  }
  const newCard101 = doc.getElementById("thread-101");
  const preservedReplyWrap = doc.getElementById("reply-form-101");
  const preservedEditWrap = doc.getElementById("msg-edit-202");
  if (!newCard101 || newCard101 === card101 || !newCard101.classList.contains("reply-open") ||
      !preservedReplyWrap || preservedReplyWrap.style.display !== "block" ||
      preservedReplyWrap.querySelector("textarea").value !== "Keep sibling reply" ||
      preservedReplyWrap.querySelector('input[name="resolved"]').checked ||
      (preservedEditWrap && preservedEditWrap.style.display === "block")) {
    throw new Error("expected replaceThreadCardHTML on resolveThread to preserve open sibling reply draft only");
  }
  win.formDraftsMap.delete(unrelatedEditKey);

  // H. publishDrafts purges edit drafts and timers, renders non-edit thread card, and keeps sibling reply draft open
  const newEditTa202 = preservedEditWrap.querySelector("textarea");
  win.openEditMessageForm(202, { noFocus: true });
  newEditTa202.value = "Pending edit timer text";
  win.handleEditMsgInput(newEditTa202, 202);
  win.formDraftsMap.set("edit:202", { draft_key: "edit:202", message_id: "202", content: "legacy" });
  fetches.length = 0;
  win.publishDrafts(%q, %q);
  await new Promise(r => setTimeout(r, 200));
  if (win.formDraftsMap.has(editKey202) || win.formDraftsMap.has("edit:202")) {
    throw new Error("expected publishDrafts to purge all edit form drafts");
  }
  if (fetches.some(f => f.url.startsWith("/api/form-drafts") && JSON.parse(f.opts.body).action === "save" && JSON.parse(f.opts.body).draft_key === editKey202)) {
    throw new Error("expected publishDrafts to cancel pending edit draft save timer");
  }
  if (doc.getElementById("msg-edit-202")) {
    throw new Error("expected published card HTML to omit #msg-edit-202");
  }
  const replyAfterPub = doc.getElementById("reply-form-101");
  if (!replyAfterPub || replyAfterPub.style.display !== "block" || replyAfterPub.querySelector("textarea").value !== "Keep sibling reply") {
    throw new Error("expected publishDrafts to keep unsaved sibling reply draft open across card replacement");
  }

  // I. findFormDraft must not cross-collide across draft types (edit:101 vs reply:101) or files (other.go:R:20 vs hello.go:R:20),
  // submitEditMessage must sync sibling reply unresolved state, and replaceThreadCardHTML must not reopen unrelated sibling cards.
  const repoSlug = countsEl.getAttribute("data-repo");
  const chgKey = countsEl.getAttribute("data-change");
  win.toggleReplyForm(101, true);
  win.formDraftsMap.clear();
  win.formDraftsMap.set("review_draft:edit:" + repoSlug + ":" + chgKey + ":101", {
    draft_key: "review_draft:edit:" + repoSlug + ":" + chgKey + ":101",
    message_id: "101",
    content: "Edit for msg 101",
    unresolved: true
  });
  win.formDraftsMap.set("review_draft:new:" + repoSlug + ":" + chgKey + ":other.go:R:20", {
    draft_key: "review_draft:new:" + repoSlug + ":" + chgKey + ":other.go:R:20",
    target_path: "other.go",
    side: "R",
    line: 20,
    content: "Other file line 20"
  });
  win.toggleReplyForm(101, false, { noFocus: true });
  const r101AfterOpen = doc.getElementById("reply-form-101");
  if (r101AfterOpen.querySelector("textarea").value !== "") {
    throw new Error("expected reply-form-101 not to pull edit:101 draft, got: " + r101AfterOpen.querySelector("textarea").value);
  }
  win.toggleReplyForm(101, true);
  win.openCommentForm(tr20, 20, "R", false, { noFocus: true });
  if (doc.getElementById("inlineNewComment").querySelector("textarea").value !== "") {
    throw new Error("expected inline comment on hello.go:R:20 not to pull other.go:R:20 draft");
  }
  win.closeInlineComment(true);

  // Put a fresh unresolved card #thread-101 with #msg-edit-202 and #reply-form-101 PLUS a sibling #thread-999 with an Escape-closed draft in diffCont
  const curCard101 = doc.getElementById("thread-101");
  const freshWrap = createEl("DIV");
  freshWrap.innerHTML = '<div id="thread-101" class="thread-card thread-unresolved" data-resolved="false"><div class="message-body" id="msg-202"></div><div class="msg-edit-form" id="msg-edit-202" data-orig-body="Orig reply" style="display: none;"><form><textarea>Orig reply</textarea><input type="checkbox" name="resolved"></form></div><div class="thread-footer"></div><div class="comment-form reply-form" id="reply-form-101" style="display: none;"><form><textarea></textarea><input type="checkbox" name="resolved"></form></div></div>';
  curCard101.replaceWith(freshWrap.firstElementChild);

  const sibCard999 = createEl("DIV");
  sibCard999.id = "thread-999";
  sibCard999.className = "thread-card";
  sibCard999.setAttribute("data-resolved", "true");
  const sibReplyWrap999 = createEl("DIV");
  sibReplyWrap999.id = "reply-form-999";
  sibReplyWrap999.className = "comment-form reply-form";
  sibReplyWrap999.style.display = "none";
  const sibTa999 = createEl("TEXTAREA");
  sibReplyWrap999.appendChild(sibTa999);
  sibCard999.appendChild(sibReplyWrap999);
  diffCont.appendChild(sibCard999);
  win.formDraftsMap.set("review_draft:reply:" + repoSlug + ":" + chgKey + ":999", {
    draft_key: "review_draft:reply:" + repoSlug + ":" + chgKey + ":999",
    thread_id: "999",
    content: "Escaped sibling reply",
    unresolved: false
  });
  win.formDraftsMap.set(replyKey101, {
    draft_key: replyKey101,
    thread_id: "101",
    content: "Sibling reply kept open",
    unresolved: true
  });

  win._customFetch = (url) => {
    if (url === "/api/threads/edit") {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          sig: "s4",
          thread: { id: 101, resolved: true, messages: [{ id: 202, body: "Resolved via edit" }] },
          html: '<div id="thread-101" class="thread-card" data-resolved="true"><div class="message-body" id="msg-202"></div><div class="msg-edit-form" id="msg-edit-202" data-orig-body="Resolved via edit" style="display: none;"><form><textarea>Resolved via edit</textarea><input type="checkbox" name="resolved" checked></form></div><div class="thread-footer"></div><div class="comment-form reply-form" id="reply-form-101" style="display: none;"><form><textarea></textarea><input type="checkbox" name="resolved" checked></form></div></div>'
        })
      });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ sig: "s4" }) });
  };
  win.openEditMessageForm(202, { noFocus: true });
  const editWrapI = doc.getElementById("msg-edit-202");
  editWrapI.querySelector("textarea").value = "Resolved via edit";
  editWrapI.querySelector('input[name="resolved"]').checked = true;
  win.submitEditMessage(202);
  await new Promise(r => setTimeout(r, 20));
  const syncedAfterEdit = win.formDraftsMap.get(replyKey101);
  if (!syncedAfterEdit || syncedAfterEdit.unresolved !== false) {
    throw new Error("expected submitEditMessage to sync sibling reply draft unresolved=false, got: " + JSON.stringify(syncedAfterEdit));
  }
  const cardAfterEdit = doc.getElementById("thread-101");
  const replyWrapAfterEdit = doc.getElementById("reply-form-101");
  if (!cardAfterEdit || cardAfterEdit.classList.contains("thread-unresolved") ||
      !replyWrapAfterEdit || replyWrapAfterEdit.style.display !== "block" ||
      !replyWrapAfterEdit.querySelector('input[name="resolved"]').checked) {
    throw new Error("expected resolved edit card to keep sibling reply checked and stay resolved");
  }
  if (sibReplyWrap999.style.display !== "none") {
    throw new Error("expected replaceThreadCardHTML on thread-101 not to reopen sibling thread-999 reply form");
  }

  console.log("OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key, rec.Slug, c.Key)

	cmd := exec.Command(nodeBin, "-e", nodeScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test in TestCommentFormControlsAndMenuRowColors failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected node test to print OK, got: %s", string(out))
	}

	cssBytes, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	cssStr := string(cssBytes)
	for _, want := range []string{
		"--unresolved-hover-bg: #f7dc96;",
		"--resolved-hover-bg: #dfe5ec;",
		"--unresolved-hover-bg: #70561e;",
		"--resolved-hover-bg: #383d45;",
		".thread-card.thread-unresolved .resolved-toggle",
		"padding: 4px 8px 6px;",
		".thread-card:not(.reply-open) > .message-body:not(.editing-msg):has(+ .thread-footer)",
		".thread-card.reply-open > .message-body:has(+ .thread-footer)",
		".thread-footer",
		".thread-card.reply-open > .thread-footer",
		".thread-card > .message-body.editing-msg + .thread-footer",
		".resolved-toggle",
		".unres-item-row.item-unresolved",
		".unres-item-row.item-resolved",
	} {
		if !strings.Contains(cssStr, want) {
			t.Errorf("expected style.css to contain %q", want)
		}
	}
	if got := strings.Count(cssStr, "--unresolved-hover-bg: #70561e;"); got != 2 {
		t.Errorf("expected 2 dark-mode definitions of --unresolved-hover-bg, got %d", got)
	}
	if got := strings.Count(cssStr, "--resolved-hover-bg: #383d45;"); got != 2 {
		t.Errorf("expected 2 dark-mode definitions of --resolved-hover-bg, got %d", got)
	}

	// 5. Real HTTP round-trip: save and delete an edit form-draft using string message_id (matching deleteFormDraftServer).
	draftKey := fmt.Sprintf("review_draft:edit:%s:%s:%d", rec.Slug, c.Key, mResDraftReply.ID)
	saveFD, _ := json.Marshal(map[string]any{
		"repo":       rec.Slug,
		"change":     c.Key,
		"action":     "save",
		"draft_key":  draftKey,
		"message_id": fmt.Sprintf("%d", mResDraftReply.ID),
		"unresolved": true,
		"content":    "body only edit",
	})
	respSaveFD, err := http.Post(ts.URL+"/api/form-drafts", "application/json", bytes.NewReader(saveFD))
	if err != nil || respSaveFD.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/form-drafts save failed: status=%v err=%v", respSaveFD.StatusCode, err)
	}
	respSaveFD.Body.Close()

	delFD, _ := json.Marshal(map[string]any{
		"repo":       rec.Slug,
		"change":     c.Key,
		"action":     "delete",
		"draft_key":  draftKey,
		"message_id": fmt.Sprintf("%d", mResDraftReply.ID),
	})
	respDelFD, err := http.Post(ts.URL+"/api/form-drafts", "application/json", bytes.NewReader(delFD))
	if err != nil || respDelFD.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/form-drafts delete failed: status=%v err=%v", respDelFD.StatusCode, err)
	}
	respDelFD.Body.Close()

	draftsLeft, err := d.FormDraftsForChange(rec.ID, c.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(draftsLeft) != 0 {
		t.Errorf("expected 0 form drafts after delete round-trip, got %d (%+v)", len(draftsLeft), draftsLeft)
	}

	// 6. POST /api/publish clears edit form-drafts, omits #msg-edit- on published HTML, and preserves reply form-drafts.
	editKeepKey := fmt.Sprintf("review_draft:edit:%s:%s:%d", rec.Slug, c.Key, mResDraftReply.ID)
	replyKeepKey := fmt.Sprintf("review_draft:reply:%s:%s:%d", rec.Slug, c.Key, thResWithReply.ID)
	if err := d.SaveFormDraft(rec.ID, c.Key, editKeepKey, "", "", 0, 0, 0, 0, true, "", fmt.Sprintf("%d", mResDraftReply.ID), "unsaved edit before publish"); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveFormDraft(rec.ID, c.Key, replyKeepKey, "", "", 0, 0, 0, 0, true, fmt.Sprintf("%d", thResWithReply.ID), "", "unsaved reply before publish"); err != nil {
		t.Fatal(err)
	}
	pubBody, _ := json.Marshal(map[string]any{
		"repo":   rec.Slug,
		"change": c.Key,
	})
	respPub, err := http.Post(ts.URL+"/api/publish", "application/json", bytes.NewReader(pubBody))
	if err != nil || respPub.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/publish failed: status=%v err=%v", respPub.StatusCode, err)
	}
	var pubOut struct {
		Published  int64             `json:"published"`
		ThreadHTML map[string]string `json:"thread_html"`
	}
	if err := json.NewDecoder(respPub.Body).Decode(&pubOut); err != nil {
		respPub.Body.Close()
		t.Fatal(err)
	}
	respPub.Body.Close()
	if pubOut.Published != 3 {
		t.Errorf("expected 3 published draft messages, got %d", pubOut.Published)
	}
	for tid, thHTML := range pubOut.ThreadHTML {
		if strings.Contains(thHTML, `id="msg-edit-`) {
			t.Errorf("expected published thread %s HTML to omit msg-edit forms, got: %s", tid, thHTML)
		}
	}
	draftsAfterPub, err := d.FormDraftsForChange(rec.ID, c.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(draftsAfterPub) != 1 || draftsAfterPub[0].DraftKey != replyKeepKey {
		t.Errorf("expected only reply draft to remain after /api/publish, got %+v", draftsAfterPub)
	}
}

func TestRefsListCSSRules(t *testing.T) {
	cssBytes, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	cssStr := string(cssBytes)

	ruleBody := func(selector string) string {
		marker := selector + " {"
		idx := strings.Index(cssStr, marker)
		if idx < 0 {
			t.Fatalf("missing CSS rule %q in style.css", selector)
		}
		rest := cssStr[idx+len(marker):]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			t.Fatalf("unterminated CSS rule %q in style.css", selector)
		}
		return rest[:end]
	}

	innerBody := ruleBody(".sym-list-inner")
	if !strings.Contains(innerBody, "min-width: 100%;") || !strings.Contains(innerBody, "width: max-content;") {
		t.Errorf(".sym-list-inner missing full-scroll sizing, got:\n%s", innerBody)
	}

	groupHdrBody := ruleBody(".sym-group-header")
	if !strings.Contains(groupHdrBody, "white-space: nowrap;") {
		t.Errorf(".sym-group-header missing white-space: nowrap;, got:\n%s", groupHdrBody)
	}
	if strings.Contains(groupHdrBody, "flex-wrap: wrap;") {
		t.Errorf(".sym-group-header should not contain flex-wrap: wrap;, got:\n%s", groupHdrBody)
	}

	rowBody := ruleBody(".sym-ref-row")
	if strings.Contains(rowBody, "min-width: min-content;") {
		t.Errorf(".sym-ref-row should not contain min-width: min-content;, got:\n%s", rowBody)
	}

	codeBody := ruleBody(".sym-ref-code")
	if !strings.Contains(codeBody, "white-space: pre;") || !strings.Contains(codeBody, "flex: 0 0 auto;") {
		t.Errorf(".sym-ref-code missing white-space: pre; flex: 0 0 auto;, got:\n%s", codeBody)
	}
	for _, forbidden := range []string{"overflow: hidden", "text-overflow: ellipsis", "width: 0"} {
		if strings.Contains(codeBody, forbidden) {
			t.Errorf(".sym-ref-code must not contain %q, got:\n%s", forbidden, codeBody)
		}
	}

	callerChildrenBody := ruleBody(".sym-caller-children")
	if !strings.Contains(callerChildrenBody, "background: var(--surface);") || !strings.Contains(callerChildrenBody, "border-left: 2px solid var(--border-muted);") {
		t.Errorf(".sym-caller-children missing var(--surface) or left border, got:\n%s", callerChildrenBody)
	}
	if strings.Contains(cssStr, "--bg-canvas-inset") {
		t.Errorf("style.css should not reference undefined --bg-canvas-inset")
	}

	nestedCallerBody := ruleBody(".sym-caller-children .sym-caller-children")
	if !strings.Contains(nestedCallerBody, "border-left: none;") || !strings.Contains(nestedCallerBody, "background: none;") {
		t.Errorf(".sym-caller-children .sym-caller-children must reset border-left and background, got:\n%s", nestedCallerBody)
	}

	// Ensure --surface is distinct from --bg in light, explicit dark, and prefers-color-scheme: dark blocks.
	if !strings.Contains(cssStr, "--surface: #f6f8fa;") || strings.Count(cssStr, "--surface: #25282c;") != 2 {
		t.Errorf("expected light and 2x dark --surface definitions in style.css")
	}
}

func TestSymbolPopupWindowShade(t *testing.T) {
	cssBytes, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	cssStr := string(cssBytes)
	for _, want := range []string{
		".sym-btn-shade,",
		".symbol-popup.sym-shaded,",
		"height: auto !important;",
		"min-height: 0 !important;",
		"> .sym-popup-content > :not(.sym-popup-header)",
		"border-bottom: none;",
		".symbol-popup.sym-shaded .sym-resize-n,",
		".symbol-popup.sym-shaded .sym-resize-se,",
	} {
		if !strings.Contains(cssStr, want) {
			t.Errorf("expected style.css to contain %q", want)
		}
	}

	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur._textContent = (cur._textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT"].includes(tag)) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    nodeType: 1,
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    style: {},
    children: [],
    parentElement: null,
    get parentNode() { return this.parentElement; },
    get childNodes() { return this.children; },
    _textContent: "",
    get textContent() {
      let t = this._textContent || "";
      for (const ch of this.children) {
        t += ch.textContent;
      }
      return t;
    },
    set textContent(v) {
      this._textContent = String(v);
      this.children = [];
    },
    listeners: {},
    get offsetWidth() {
      if (this._offsetWidth !== undefined) return this._offsetWidth;
      if (this.classList && this.classList.contains("sym-popup-actions")) return 80;
      if (this.classList && this.classList.contains("sym-btn-back")) return 24;
      return 300;
    },
    set offsetWidth(v) { this._offsetWidth = v; },
    get offsetHeight() {
      if (this._offsetHeight !== undefined) return this._offsetHeight;
      if (this._rect && this._rect.height !== undefined) return this._rect.height;
      return 200;
    },
    set offsetHeight(v) { this._offsetHeight = v; },
    scrollTop: 0,
    scrollLeft: 0,
    _rect: { top: 100, left: 100, bottom: 300, right: 400, width: 300, height: 200 },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    removeAttribute(k) { delete this.attributes[k]; },
    getBoundingClientRect() { return { ...this._rect }; },
    focus() { doc.activeElement = this; },
    blur() { if (doc.activeElement === this) doc.activeElement = null; },
    addEventListener(event, fn) {
      if (!this.listeners[event]) this.listeners[event] = [];
      this.listeners[event].push(fn);
    },
    removeEventListener(event, fn) {
      if (this.listeners[event]) this.listeners[event] = this.listeners[event].filter(f => f !== fn);
    },
    appendChild(child) {
      if (!child) return child;
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    remove() {
      if (this.parentElement) {
        this.parentElement.children = this.parentElement.children.filter(c => c !== this);
        this.parentElement = null;
      }
    },
    get classList() {
      return {
        contains: (c) => (el.className || "").split(/\s+/).includes(c),
        add: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.add(c));
          el.className = Array.from(parts).join(" ");
        },
        remove: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.delete(c));
          el.className = Array.from(parts).join(" ");
        }
      };
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (node.tagName && matchesSel(node, sel)) return node;
        node = node.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const results = [];
      function search(n) {
        for (const ch of n.children) {
          if (matchesSel(ch, sel)) results.push(ch);
          search(ch);
        }
      }
      search(this);
      return results;
    },
    set innerHTML(val) {
      this._innerHTML = val;
      this.children = parseHtml(val, this);
    },
    get innerHTML() {
      return this._innerHTML || "";
    }
  };
  return el;
}

function matchesSingleSel(node, p) {
  if (!node || !node.tagName) return false;
  if (p.includes("[")) {
    const bIdx = p.indexOf("[");
    const tagPart = p.slice(0, bIdx);
    const attrPart = p.slice(bIdx + 1, -1);
    if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) return false;
    if (attrPart.includes("=")) {
      const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
      return !!(node.getAttribute && node.getAttribute(k) === v);
    }
    return !!(node.hasAttribute && node.hasAttribute(attrPart));
  }
  if (p.includes(".")) {
    const segs = p.split(".");
    const tagPart = segs[0];
    const clsParts = segs.slice(1);
    if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) return false;
    return clsParts.every(c => node.classList && node.classList.contains && node.classList.contains(c));
  }
  if (p.startsWith("#")) {
    return node.id === p.slice(1);
  }
  return p.toLowerCase() === (node.tagName || "").toLowerCase();
}

function matchesSel(node, sel) {
  const parts = sel.split(",").map(s => s.trim()).filter(Boolean);
  for (const p of parts) {
    const tokens = p.split(/\s+/).filter(Boolean);
    if (tokens.length === 0) continue;
    if (!matchesSingleSel(node, tokens[tokens.length - 1])) continue;
    let ok = true;
    let cur = node.parentElement;
    for (let i = tokens.length - 2; i >= 0; i--) {
      while (cur && !matchesSingleSel(cur, tokens[i])) {
        cur = cur.parentElement;
      }
      if (!cur) {
        ok = false;
        break;
      }
      cur = cur.parentElement;
    }
    if (ok) return true;
  }
  return false;
}

const docListeners = {};
const winListeners = {};
const body = createMockElement("BODY");
const appHdr = createMockElement("HEADER");
appHdr.className = "app-header";
appHdr._rect = { top: 0, left: 0, bottom: 48, right: 1000, width: 1000, height: 48 };
body.appendChild(appHdr);

const doc = {
  readyState: "complete",
  activeElement: null,
  documentElement: { clientWidth: 1000, clientHeight: 1000, getAttribute() { return ""; }, setAttribute() {}, style: { setProperty() {}, getPropertyValue() { return ""; } } },
  body,
  getElementById(id) {
    if (id === "shortcutsModal") return null;
    const all = body.querySelectorAll("#" + id);
    return all.length > 0 ? all[0] : null;
  },
  createElement(tag) { return createMockElement(tag); },
  createTreeWalker(root) { return { currentNode: root, nextNode() { return null; }, previousNode() { return null; } }; },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (docListeners[event]) docListeners[event] = docListeners[event].filter(f => f !== fn);
  },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); }
};

let nextFetchStatus = 200;
const win = {
  document: doc,
  NodeFilter: { SHOW_TEXT: 4 },
  location: { pathname: "/r/test/c/abc", search: "", hash: "", reload() {} },
  addEventListener(event, fn) {
    if (!winListeners[event]) winListeners[event] = [];
    winListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (winListeners[event]) winListeners[event] = winListeners[event].filter(f => f !== fn);
  },
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true }; },
  window: null,
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800,
  fetch(url) {
    if (nextFetchStatus !== 200) {
      return Promise.resolve({
        ok: false,
        status: nextFetchStatus,
        json: () => Promise.resolve({ error: "boom" }),
      });
    }
    if (url.startsWith("/api/symbol/refs")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "SymA",
          refs: [{ group: "repo", path: "foo.go", display: "foo.go", line: 10, col: 5, html: "SymA()", in_repo: true }]
        })
      });
    }
    if (url.startsWith("/api/symbol/impls")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          symbol_name: "SymA",
          impls: [{ group: "repo", path: "foo.go", display: "foo.go", line: 12, in_repo: true }]
        })
      });
    }
    if (url.startsWith("/api/diff/move-preview")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          found: true,
          path: "foo.go",
          side: "R",
          start_line: 1,
          end_line: 3,
          rows: [{ line: 1, html: "<span class=\"tok-id\">SymA</span>", moved: true }]
        })
      });
    }
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        found: true,
        symbol_name: "SymA",
        def_path: "foo.go",
        def_line: 42,
        in_repo: true,
        display_path: "foo.go",
        signature: "func SymA()",
        snippet: [
          { line: 42, active: true, html: "<span class=\"tok-kw\">func</span> <span class=\"tok-id\">SymA</span>() {}" },
        ],
        can_refs: true,
        can_impls: true,
      })
    });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  NodeFilter: win.NodeFilter,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  fetch: win.fetch,
  URLSearchParams,
  Promise,
  setTimeout,
  clearTimeout,
  console,
  AbortController,
});
vm.runInContext(code, ctx);

(async () => {
  const container = createMockElement("DIV");
  container.className = "diff-container";
  container.setAttribute("data-file-path", "foo.go");
  container.setAttribute("data-repo", "testrepo");
  container.setAttribute("data-change", "123");
  body.appendChild(container);
  const tr = createMockElement("TR");
  tr.setAttribute("data-line-r", "10");
  container.appendChild(tr);
  const td = createMockElement("TD");
  td.setAttribute("data-side", "R");
  tr.appendChild(td);
  const tok = createMockElement("SPAN");
  tok.className = "tok-id";
  tok.textContent = "SymA";
  tok._rect = { top: 100, left: 100, bottom: 120, right: 150, width: 50, height: 20 };
  td.appendChild(tok);

  // 1. Open popup via openSymbolPopupAt; initial state is unshaded with shade button
  await win.openSymbolPopupAt(container, "R", 10, 1, tok, true);
  if (win.openSymbolPopups.length !== 1) throw new Error("expected 1 popup");
  const inst = win.openSymbolPopups[0];
  if (inst.shaded !== false || inst.usedShade !== false) {
    throw new Error("expected initial shaded=false usedShade=false");
  }
  let shadeBtn = inst.el.querySelector(".sym-btn-shade");
  if (!shadeBtn || shadeBtn.textContent !== "−" || shadeBtn.getAttribute("aria-expanded") !== "true") {
    throw new Error("expected initial .sym-btn-shade with minus glyph and aria-expanded=true");
  }

  // 2. Click .sym-btn-shade -> toggles shaded=true, usedShade=true, .sym-shaded class, glyph '+'
  const clickFn = inst.el.listeners["click"][0];
  await clickFn({ target: shadeBtn, preventDefault() {}, stopPropagation() {} });
  if (!inst.shaded || !inst.usedShade || !inst.el.classList.contains("sym-shaded")) {
    throw new Error("expected popup shaded after .sym-btn-shade click");
  }
  shadeBtn = inst.el.querySelector(".sym-btn-shade");
  if (shadeBtn.textContent !== "+" || shadeBtn.getAttribute("aria-expanded") !== "false") {
    throw new Error("expected '+' glyph and aria-expanded=false while shaded");
  }

  // applyIdentOccurrences must skip shaded popups
  win.applyIdentOccurrences("SymA");
  const tokInShaded = inst.el.querySelector(".sym-snippet-code .tok-id");
  if (!tokInShaded || tokInShaded.classList.contains("ident-occ-hl")) {
    throw new Error("expected shaded popup spans skipped by applyIdentOccurrences");
  }

  // 3. Double-click on interactive controls in header must NOT toggle shade
  const dblclickFn = inst.el.listeners["dblclick"][0];
  const locLink = inst.el.querySelector("a.sym-popup-loc-link");
  const refsBtn = inst.el.querySelector(".sym-btn-refs");
  dblclickFn({ target: locLink, preventDefault() {}, stopPropagation() {} });
  dblclickFn({ target: refsBtn, preventDefault() {}, stopPropagation() {} });
  dblclickFn({ target: shadeBtn, preventDefault() {}, stopPropagation() {} });
  if (!inst.shaded) {
    throw new Error("expected dblclick on header link/buttons not to toggle shade");
  }

  // Double-click on header title area (.sym-popup-loc, or header itself) unshades it
  const locDiv = inst.el.querySelector(".sym-popup-loc");
  dblclickFn({ target: locDiv, preventDefault() {}, stopPropagation() {} });
  if (inst.shaded || inst.el.classList.contains("sym-shaded")) {
    throw new Error("expected dblclick on .sym-popup-loc to unshade");
  }
  if (!inst.usedShade) {
    throw new Error("expected usedShade to remain true after unshading");
  }
  const tokUnshaded = inst.el.querySelector(".sym-snippet-code .tok-id");
  if (!tokUnshaded || !tokUnshaded.classList.contains("ident-occ-hl")) {
    throw new Error("expected unshaded popup spans highlighted on unshade");
  }

  // 4. Preserve resizedH inline height across shade -> unshade
  inst.resizedH = true;
  inst.el.classList.add("sym-resized-h");
  inst.el.style.height = "420px";
  win.togglePopupShade(inst);
  if (!inst.shaded || inst.el.style.height !== "420px") {
    throw new Error("expected style.height preserved while shaded, got: " + inst.el.style.height);
  }
  win.togglePopupShade(inst);
  if (inst.shaded || inst.el.style.height !== "420px" || !inst.el.classList.contains("sym-resized-h")) {
    throw new Error("expected resized height restored after unshade");
  }

  // Preserve snapped inline height across shade -> unshade
  win.applyPopupSnap(inst, "left");
  const snappedH = inst.el.style.height;
  win.togglePopupShade(inst);
  if (!inst.shaded || !inst.snapped || inst.el.style.height !== snappedH) {
    throw new Error("expected snapped height preserved while shaded");
  }
  win.togglePopupShade(inst);
  if (inst.shaded || !inst.snapped || inst.el.style.height !== snappedH) {
    throw new Error("expected snapped height preserved after unshade");
  }

  // 5. Clicking Refs while shaded auto-unshades and preserves usedShade
  win.setPopupShaded(inst, true, true);
  await clickFn({ target: inst.el.querySelector(".sym-btn-refs"), preventDefault() {}, stopPropagation() {} });
  if (inst.shaded || inst.el.classList.contains("sym-shaded")) {
    throw new Error("expected Refs click to auto-unshade popup");
  }
  if (!inst.usedShade) {
    throw new Error("expected usedShade preserved across Refs auto-unshade");
  }
  if (!inst.el.querySelector(".sym-btn-shade") || inst.el.querySelector(".sym-btn-shade").textContent !== "−") {
    throw new Error("expected shade button in Refs view with minus glyph");
  }

  // 6. Shading in Refs view then clicking Back auto-unshades and preserves usedShade
  win.setPopupShaded(inst, true, true);
  const backBtn = inst.el.querySelector(".sym-btn-back");
  await clickFn({ target: backBtn, preventDefault() {}, stopPropagation() {} });
  if (inst.shaded || inst.el.classList.contains("sym-shaded")) {
    throw new Error("expected Back click to auto-unshade popup");
  }
  if (!inst.usedShade) {
    throw new Error("expected usedShade preserved across Back auto-unshade");
  }

  // 7. Error header and move-preview popup also include .sym-btn-shade
  win.closeSpecificSymbolPopup(inst);
  nextFetchStatus = 500;
  await win.openSymbolPopupAt(container, "R", 10, 1, tok, false);
  const errInst = win.openSymbolPopups[0];
  if (!errInst.el.querySelector(".sym-btn-shade")) {
    throw new Error("expected .sym-btn-shade in error popup header");
  }
  win.closeSpecificSymbolPopup(errInst);

  nextFetchStatus = 200;
  const moveBtn = createMockElement("BUTTON");
  moveBtn.setAttribute("data-peer-path", "foo.go");
  moveBtn.setAttribute("data-peer-start", "1");
  moveBtn.setAttribute("data-peer-end", "3");
  moveBtn.setAttribute("data-peer-side", "R");
  container.appendChild(moveBtn);
  await win.handleOpenMovePeerPopup({ button: 0, preventDefault() {}, stopPropagation() {} }, moveBtn);
  const moveInst = win.openSymbolPopups[0];
  if (!moveInst.el.querySelector(".sym-btn-shade")) {
    throw new Error("expected .sym-btn-shade in move popup header");
  }
  // 8. Snapping a shaded resizedH popup unshades it into the snap zone and preserves real preSnapSize.height
  moveInst.resizedH = true;
  moveInst.el.style.height = "420px";
  win.togglePopupShade(moveInst);
  moveInst.el.offsetHeight = 28;
  win.applyPopupSnap(moveInst, "right");
  if (moveInst.shaded || moveInst.el.classList.contains("sym-shaded")) {
    throw new Error("expected applyPopupSnap to unshade popup");
  }
  if (!moveInst.usedShade) {
    throw new Error("expected applyPopupSnap to preserve usedShade");
  }
  if (!moveInst.preSnapSize || moveInst.preSnapSize.height !== 420) {
    throw new Error("expected preSnapSize.height === 420, got " + JSON.stringify(moveInst.preSnapSize));
  }
  win.closeSpecificSymbolPopup(moveInst);

  console.log("OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TestSymbolPopupWindowShade node test failed: %v\n%s", err, string(out))
	} else if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected OK output, got: %s", string(out))
	}
}

func TestSymbolPopupShiftEscapeAndShiftRightClick(t *testing.T) {
	layoutBytes, err := os.ReadFile(filepath.Join("tmpl", "layout.html"))
	if err != nil {
		t.Fatalf("failed to read tmpl/layout.html: %v", err)
	}
	layoutStr := string(layoutBytes)
	if !strings.Contains(layoutStr, "<kbd>Shift</kbd>+<kbd>Esc</kbd>") {
		t.Errorf("expected layout.html shortcuts modal to mention Shift+Esc")
	}

	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur._textContent = (cur._textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT"].includes(tag)) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function createMockElement(tag) {
  const el = {
    nodeType: 1,
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    style: {},
    children: [],
    parentElement: null,
    get parentNode() { return this.parentElement; },
    get childNodes() { return this.children; },
    _textContent: "",
    get textContent() {
      let t = this._textContent || "";
      for (const ch of this.children) t += ch.textContent;
      return t;
    },
    set textContent(v) {
      this._textContent = String(v);
      this.children = [];
    },
    listeners: {},
    _rect: { top: 10, left: 10, right: 200, bottom: 200, width: 190, height: 190 },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    removeAttribute(k) { delete this.attributes[k]; },
    getBoundingClientRect() { return { ...this._rect }; },
    focus() { doc.activeElement = this; },
    blur() { if (doc.activeElement === this) doc.activeElement = null; },
    addEventListener(event, fn) {
      if (!this.listeners[event]) this.listeners[event] = [];
      this.listeners[event].push(fn);
    },
    removeEventListener(event, fn) {
      if (this.listeners[event]) this.listeners[event] = this.listeners[event].filter(f => f !== fn);
    },
    appendChild(child) {
      if (!child) return child;
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    remove() {
      if (this.parentElement) {
        this.parentElement.children = this.parentElement.children.filter(c => c !== this);
        this.parentElement = null;
      }
    },
    get classList() {
      return {
        contains: (c) => (el.className || "").split(/\s+/).includes(c),
        add: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.add(c));
          el.className = Array.from(parts).join(" ");
        },
        remove: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.delete(c));
          el.className = Array.from(parts).join(" ");
        }
      };
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (node.tagName && matchesSel(node, sel)) return node;
        node = node.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const results = [];
      function search(n) {
        for (const ch of n.children) {
          if (matchesSel(ch, sel)) results.push(ch);
          search(ch);
        }
      }
      search(this);
      return results;
    },
    set innerHTML(val) {
      this._innerHTML = val;
      this.children = parseHtml(val, this);
    },
    get innerHTML() {
      return this._innerHTML || "";
    }
  };
  return el;
}

function matchesSingleSel(node, p) {
  if (!node || !node.tagName) return false;
  if (p.includes("[")) {
    const bIdx = p.indexOf("[");
    const tagPart = p.slice(0, bIdx);
    const attrPart = p.slice(bIdx + 1, -1);
    if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) return false;
    if (attrPart.includes("=")) {
      const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
      return !!(node.getAttribute && node.getAttribute(k) === v);
    }
    return !!(node.hasAttribute && node.hasAttribute(attrPart));
  }
  if (p.includes(".")) {
    const segs = p.split(".");
    const tagPart = segs[0];
    const clsParts = segs.slice(1);
    if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) return false;
    return clsParts.every(c => node.classList && node.classList.contains && node.classList.contains(c));
  }
  if (p.startsWith("#")) {
    return node.id === p.slice(1);
  }
  return p.toLowerCase() === (node.tagName || "").toLowerCase();
}

function matchesSel(node, sel) {
  const parts = sel.split(",").map(s => s.trim()).filter(Boolean);
  for (const p of parts) {
    const tokens = p.split(/\s+/).filter(Boolean);
    if (tokens.length === 0) continue;
    if (!matchesSingleSel(node, tokens[tokens.length - 1])) continue;
    let ok = true;
    let cur = node.parentElement;
    for (let i = tokens.length - 2; i >= 0; i--) {
      while (cur && !matchesSingleSel(cur, tokens[i])) cur = cur.parentElement;
      if (!cur) { ok = false; break; }
      cur = cur.parentElement;
    }
    if (ok) return true;
  }
  return false;
}

const docListeners = {};
const winListeners = {};
const body = createMockElement("BODY");
const doc = {
  readyState: "complete",
  activeElement: body,
  documentElement: { clientWidth: 1000, clientHeight: 1000, getAttribute() { return ""; }, setAttribute() {}, style: { setProperty() {}, getPropertyValue() { return ""; } } },
  body,
  getElementById(id) {
    if (id === "shortcutsModal") return null;
    const all = body.querySelectorAll("#" + id);
    return all.length > 0 ? all[0] : null;
  },
  createElement(tag) { return createMockElement(tag); },
  createTreeWalker(root) { return { currentNode: root, nextNode() { return null; }, previousNode() { return null; } }; },
  addEventListener(event, fn) {
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (docListeners[event]) docListeners[event] = docListeners[event].filter(f => f !== fn);
  },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); }
};

const win = {
  document: doc,
  NodeFilter: { SHOW_TEXT: 4 },
  location: { pathname: "/r/test/c/abc", search: "", hash: "", reload() {} },
  addEventListener(event, fn) {
    if (!winListeners[event]) winListeners[event] = [];
    winListeners[event].push(fn);
  },
  removeEventListener(event, fn) {
    if (winListeners[event]) winListeners[event] = winListeners[event].filter(f => f !== fn);
  },
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return { isCollapsed: true, toString: () => "" }; },
  window: null,
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1000,
  innerHeight: 800
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  NodeFilter: win.NodeFilter,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  URLSearchParams,
  Promise,
  setTimeout,
  clearTimeout,
  console
});
vm.runInContext(code, ctx);

function makePopup(id, withHistory, rect) {
  const el = createMockElement("DIV");
  el.className = "symbol-popup";
  el.id = id;
  el.style.zIndex = "205";
  el._rect = rect || { left: 10, top: 10, right: 200, bottom: 200, width: 190, height: 190 };
  const snip = createMockElement("DIV");
  snip.className = "sym-popup-snippet";
  const tok = createMockElement("SPAN");
  tok.className = "tok-id";
  tok.textContent = "Foo";
  snip.appendChild(tok);
  el.appendChild(snip);
  body.appendChild(el);
  const inst = {
    el,
    _zOrder: 205,
    history: withHistory ? [{ kind: "symbol", data: { symbol_name: "Root" }, ctx: {} }] : [],
    currentState: { kind: "refs", data: { symbol_name: "Child" }, ctx: {} }
  };
  win.openSymbolPopups.push(inst);
  return { el, inst, tok };
}

const keydown = winListeners["keydown"][0];
const ctxListeners = docListeners["contextmenu"] || [];
const pmListeners = docListeners["pointermove"] || [];

// 1. Shift+Escape focused on popup 1 closes ONLY popup 1 immediately (skipping history back), leaving popup 2 open
let p1 = makePopup("p1", true, { left: 10, top: 10, right: 200, bottom: 200, width: 190, height: 190 });
let p2 = makePopup("p2", true, { left: 250, top: 10, right: 440, bottom: 200, width: 190, height: 190 });
win.setActivePopupInst(p1.inst);
doc.activeElement = p1.el;
let prevented = false;
keydown({ key: "Escape", shiftKey: true, target: p1.el, preventDefault() { prevented = true; } });
if (!prevented) throw new Error("Shift+Esc focused on popup should call preventDefault");
if (win.openSymbolPopups.length !== 1 || win.openSymbolPopups[0] !== p2.inst) {
  throw new Error("Shift+Esc focused on p1 should close only p1 immediately without stepping history");
}

// 2. Shift+Right-Click on blank area inside popup 1 closes ONLY popup 1 immediately despite history, leaving popup 2 open
p1 = makePopup("p1b", true, { left: 10, top: 10, right: 200, bottom: 200, width: 190, height: 190 });
win.setActivePopupInst(null);
doc.activeElement = body;
prevented = false;
for (const fn of ctxListeners) {
  fn({ button: 2, shiftKey: true, clientX: 50, clientY: 50, defaultPrevented: false, preventDefault() { prevented = true; }, target: p1.el });
}
if (!prevented) throw new Error("Shift+Right-Click on popup blank area should call preventDefault");
if (win.openSymbolPopups.length !== 1 || win.openSymbolPopups[0] !== p2.inst) {
  throw new Error("Shift+Right-Click on p1 should close only p1 immediately, leaving p2 open");
}

// 3. Shift+Right-Click over hovered token inside popup preserves native context menu
prevented = false;
for (const fn of ctxListeners) {
  fn({ button: 2, shiftKey: true, clientX: 300, clientY: 50, defaultPrevented: false, preventDefault() { prevented = true; }, target: p2.tok });
}
if (prevented || win.openSymbolPopups.length !== 1) {
  throw new Error("Shift+Right-Click over code token in popup should preserve native menu and keep popup open");
}

// 4. Outside Shift+Escape closes ALL popups while preserving inline comment form, reply form, and textarea focus
p1 = makePopup("p1c", true, { left: 10, top: 10, right: 200, bottom: 200, width: 190, height: 190 });
const inlineForm = createMockElement("TR");
inlineForm.id = "inlineNewComment";
const ta = createMockElement("TEXTAREA");
inlineForm.appendChild(ta);
body.appendChild(inlineForm);
const replyForm = createMockElement("DIV");
replyForm.className = "reply-form";
replyForm.style.display = "block";
body.appendChild(replyForm);

// Stray untracked DOM popup node too
const stray = createMockElement("DIV");
stray.className = "symbol-popup";
stray.id = "stray-pop";
body.appendChild(stray);

for (const fn of pmListeners) {
  fn({ clientX: 600, clientY: 600 });
}
doc.activeElement = ta;
prevented = false;
keydown({ key: "Escape", shiftKey: true, target: ta, preventDefault() { prevented = true; } });
if (!prevented) throw new Error("outside Shift+Esc with open popups should call preventDefault");
if (win.openSymbolPopups.length !== 0 || body.querySelectorAll(".symbol-popup").length !== 0) {
  throw new Error("outside Shift+Esc should close all popups");
}
if (!body.querySelector("#inlineNewComment") || replyForm.style.display !== "block" || doc.activeElement !== ta) {
  throw new Error("outside Shift+Esc must NOT close comment forms or blur textarea");
}

// 5. Outside Shift+Right-Click on blank page area closes ALL popups while preserving comment forms
p1 = makePopup("p1d", true);
p2 = makePopup("p2d", false);
win.setActivePopupInst(null);
doc.activeElement = body;
prevented = false;
for (const fn of ctxListeners) {
  fn({ button: 2, shiftKey: true, clientX: 700, clientY: 700, defaultPrevented: false, preventDefault() { prevented = true; }, target: body });
}
if (!prevented) throw new Error("outside Shift+Right-Click with popups open should call preventDefault");
if (win.openSymbolPopups.length !== 0) {
  throw new Error("outside Shift+Right-Click should close all popups");
}
if (!body.querySelector("#inlineNewComment")) {
  throw new Error("outside Shift+Right-Click must NOT close inline comment form");
}

// 6. With 0 popups open, outside Shift+Esc and outside Shift+Right-Click do NOT call preventDefault or close comments
prevented = false;
keydown({ key: "Escape", shiftKey: true, target: body, preventDefault() { prevented = true; } });
if (prevented || !body.querySelector("#inlineNewComment")) {
  throw new Error("Shift+Esc with 0 popups must not preventDefault or close comment forms");
}
prevented = false;
for (const fn of ctxListeners) {
  fn({ button: 2, shiftKey: true, clientX: 700, clientY: 700, defaultPrevented: false, preventDefault() { prevented = true; }, target: body });
}
if (prevented || !body.querySelector("#inlineNewComment")) {
  throw new Error("Shift+Right-Click with 0 popups must preserve native context menu and comment forms");
}

console.log("OK");
`
	cmd := exec.Command(nodeBin, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TestSymbolPopupShiftEscapeAndShiftRightClick node test failed: %v\n%s", err, string(out))
	} else if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected OK output, got: %s", string(out))
	}
}

func TestSymbolPopupAutoCloseUninteracted(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const path = require("path");
const code = fs.readFileSync(path.join("static", "app.js"), "utf8");

function parseHtml(html, parent) {
  const root = { tagName: "ROOT", children: [], parentElement: parent };
  const stack = [root];
  const re = /<(\/)?([a-z0-9]+)([^>]*)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      cur._textContent = (cur._textContent || "") + m[4];
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toUpperCase();
    const attrs = m[3] || "";
    if (isClose) {
      if (stack.length > 1 && stack[stack.length - 1].tagName === tag) {
        stack.pop();
      }
    } else {
      const cur = stack[stack.length - 1];
      const el = createMockElement(tag);
      el.parentElement = cur;
      const classM = attrs.match(/class="([^"]+)"/);
      if (classM) el.className = classM[1];
      const idM = attrs.match(/id="([^"]+)"/);
      if (idM) el.id = idM[1];
      const attrRe = /([a-z0-9_-]+)(?:="([^"]*)")?/gi;
      let am;
      while ((am = attrRe.exec(attrs)) !== null) {
        el.attributes[am[1]] = am[2] !== undefined ? am[2] : "";
      }
      cur.children.push(el);
      if (!["IMG", "BR", "HR", "INPUT"].includes(tag)) {
        stack.push(el);
      }
    }
  }
  for (const ch of root.children) {
    ch.parentElement = parent;
  }
  return root.children;
}

function matchesSingleSel(node, p) {
  if (!node || !node.tagName) return false;
  if (p.includes("[")) {
    const bIdx = p.indexOf("[");
    const tagPart = p.slice(0, bIdx);
    const attrPart = p.slice(bIdx + 1, -1);
    if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) return false;
    if (attrPart.includes("=")) {
      const [k, v] = attrPart.split("=").map(s => s.replace(/["']/g, "").trim());
      return !!(node.getAttribute && node.getAttribute(k) === v);
    }
    return !!(node.hasAttribute && node.hasAttribute(attrPart));
  }
  if (p.includes(".")) {
    const segs = p.split(".");
    const tagPart = segs[0];
    const clsParts = segs.slice(1);
    if (tagPart && (node.tagName || "").toLowerCase() !== tagPart.toLowerCase()) return false;
    return clsParts.every(c => node.classList && node.classList.contains && node.classList.contains(c));
  }
  if (p.startsWith("#")) {
    return node.id === p.slice(1);
  }
  return p.toLowerCase() === (node.tagName || "").toLowerCase();
}

function matchesSel(node, sel) {
  const parts = sel.split(",").map(s => s.trim()).filter(Boolean);
  for (const p of parts) {
    const tokens = p.split(/\s+/).filter(Boolean);
    if (tokens.length === 0) continue;
    if (!matchesSingleSel(node, tokens[tokens.length - 1])) continue;
    let ok = true;
    let cur = node.parentElement;
    for (let i = tokens.length - 2; i >= 0; i--) {
      while (cur && !matchesSingleSel(cur, tokens[i])) cur = cur.parentElement;
      if (!cur) { ok = false; break; }
      cur = cur.parentElement;
    }
    if (ok) return true;
  }
  return false;
}

function createMockElement(tag) {
  const el = {
    nodeType: 1,
    tagName: tag.toUpperCase(),
    id: "",
    className: "",
    attributes: {},
    style: {},
    offsetWidth: 680,
    offsetHeight: 320,
    clientWidth: 680,
    clientHeight: 320,
    children: [],
    parentElement: null,
    get parentNode() { return this.parentElement; },
    get childNodes() { return this.children; },
    _textContent: "",
    get textContent() {
      let t = this._textContent || "";
      for (const ch of this.children) t += ch.textContent;
      return t;
    },
    set textContent(v) {
      this._textContent = String(v);
      this.children = [];
    },
    listeners: {},
    _rect: { top: 100, left: 100, right: 300, bottom: 200, width: 200, height: 100 },
    getAttribute(k) { return this.attributes[k] !== undefined ? this.attributes[k] : null; },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    hasAttribute(k) { return this.attributes[k] !== undefined; },
    removeAttribute(k) { delete this.attributes[k]; },
    getBoundingClientRect() { return { ...this._rect }; },
    focus() { doc.activeElement = this; },
    blur() { if (doc.activeElement === this) doc.activeElement = null; },
    addEventListener(event, fn, opts) {
      const capture = typeof opts === "boolean" ? opts : !!(opts && opts.capture);
      if (!this.listeners[event]) this.listeners[event] = [];
      this.listeners[event].push({ fn, capture });
    },
    removeEventListener(event, fn, opts) {
      const capture = typeof opts === "boolean" ? opts : !!(opts && opts.capture);
      if (this.listeners[event]) {
        this.listeners[event] = this.listeners[event].filter(x => !(x.fn === fn && x.capture === capture));
      }
    },
    appendChild(child) {
      if (!child) return child;
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    remove() {
      if (this.parentElement) {
        this.parentElement.children = this.parentElement.children.filter(c => c !== this);
        this.parentElement = null;
      }
    },
    get classList() {
      return {
        contains: (c) => (el.className || "").split(/\s+/).includes(c),
        add: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.add(c));
          el.className = Array.from(parts).join(" ");
        },
        remove: (...cls) => {
          const parts = new Set((el.className || "").split(/\s+/).filter(Boolean));
          cls.forEach(c => parts.delete(c));
          el.className = Array.from(parts).join(" ");
        }
      };
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (node.tagName && matchesSel(node, sel)) return node;
        node = node.parentElement;
      }
      return null;
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all.length > 0 ? all[0] : null;
    },
    querySelectorAll(sel) {
      const results = [];
      function search(n) {
        for (const ch of n.children) {
          if (matchesSel(ch, sel)) results.push(ch);
          search(ch);
        }
      }
      search(this);
      return results;
    },
    set innerHTML(val) {
      this._innerHTML = val;
      this.children = parseHtml(val, this);
    },
    get innerHTML() {
      return this._innerHTML || "";
    }
  };
  return el;
}

const docListeners = {};
const winListeners = {};
const body = createMockElement("BODY");
body.offsetHeight = 2000;
let currentSelection = { isCollapsed: true, anchorNode: null, toString: () => "" };

const doc = {
  readyState: "complete",
  activeElement: body,
  documentElement: { clientWidth: 1200, clientHeight: 900, getAttribute() { return ""; }, setAttribute() {}, style: { setProperty() {}, getPropertyValue() { return ""; } } },
  body,
  getElementById(id) {
    if (id === "shortcutsModal") return null;
    const all = body.querySelectorAll("#" + id);
    return all.length > 0 ? all[0] : null;
  },
  createElement(tag) { return createMockElement(tag); },
  createTreeWalker(root) { return { currentNode: root, nextNode() { return null; }, previousNode() { return null; } }; },
  addEventListener(event, fn, opts) {
    const capture = typeof opts === "boolean" ? opts : !!(opts && opts.capture);
    if (!docListeners[event]) docListeners[event] = [];
    docListeners[event].push({ fn, capture });
  },
  removeEventListener(event, fn, opts) {
    const capture = typeof opts === "boolean" ? opts : !!(opts && opts.capture);
    if (docListeners[event]) {
      docListeners[event] = docListeners[event].filter(x => !(x.fn === fn && x.capture === capture));
    }
  },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); }
};

const win = {
  document: doc,
  NodeFilter: { SHOW_TEXT: 4 },
  location: { pathname: "/r/test/c/abc", search: "", hash: "", reload() {} },
  addEventListener(event, fn, opts) {
    const capture = typeof opts === "boolean" ? opts : !!(opts && opts.capture);
    if (!winListeners[event]) winListeners[event] = [];
    winListeners[event].push({ fn, capture });
  },
  removeEventListener(event, fn, opts) {
    const capture = typeof opts === "boolean" ? opts : !!(opts && opts.capture);
    if (winListeners[event]) {
      winListeners[event] = winListeners[event].filter(x => !(x.fn === fn && x.capture === capture));
    }
  },
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { getItem() { return null; }, setItem() {} },
  sessionStorage: { getItem() { return null; }, setItem() {} },
  getSelection() { return currentSelection; },
  window: null,
  scrollX: 0,
  scrollY: 0,
  innerWidth: 1200,
  innerHeight: 900,
  fetch(url) {
    if (url.includes("/api/symbol/refs")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ found: true, symbol_name: "TargetSym", refs: [] })
      });
    }
    if (url.includes("/api/diff/move-preview")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ found: true, path: "foo.go", start: 1, end: 3, lines: [] })
      });
    }
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({
        found: true,
        symbol_name: "TargetSym",
        display_path: "foo.go",
        def_path: "foo.go",
        def_line: 10,
        in_repo: true,
        can_refs: true,
        can_impls: true,
        signature_html: "func <span class=\"tok-id\">TargetSym</span>()",
        snippet: [
          { line: 10, html: "func <span class=\"tok-id\">TargetSym</span>() {}" }
        ]
      })
    });
  }
};
win.window = win;

const ctx = vm.createContext({
  window: win,
  document: doc,
  NodeFilter: win.NodeFilter,
  Node: { ELEMENT_NODE: 1, TEXT_NODE: 3 },
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  URLSearchParams,
  AbortController,
  fetch: win.fetch.bind(win),
  Promise,
  setTimeout,
  clearTimeout,
  console
});
vm.runInContext(code, ctx);

async function dispatchDoc(event, evt) {
  const list = (docListeners[event] || []).slice();
  for (const item of list.filter(x => x.capture)) {
    await item.fn(evt);
  }
  for (const item of list.filter(x => !x.capture)) {
    await item.fn(evt);
  }
}

async function dispatchElAndDoc(el, event, evt) {
  const docList = (docListeners[event] || []).slice();
  for (const item of docList.filter(x => x.capture)) {
    await item.fn(evt);
  }
  const chain = [];
  for (let cur = el; cur; cur = cur.parentElement) chain.push(cur);
  for (let i = chain.length - 1; i >= 0; i--) {
    const ls = (chain[i].listeners[event] || []).filter(x => x.capture);
    for (const x of ls) await x.fn(evt);
  }
  let stopped = false;
  const wrapped = Object.assign({
    preventDefault() { this.defaultPrevented = true; },
    stopPropagation() { stopped = true; }
  }, evt);
  for (let i = 0; i < chain.length && !stopped; i++) {
    const ls = (chain[i].listeners[event] || []).filter(x => !x.capture);
    for (const x of ls) {
      await x.fn(wrapped);
      if (stopped) break;
    }
  }
  if (!stopped) {
    for (const item of docList.filter(x => !x.capture)) {
      await item.fn(wrapped);
    }
  }
  return wrapped;
}

async function dispatchWin(event, evt) {
  const list = (winListeners[event] || []).slice();
  for (const item of list.filter(x => x.capture)) {
    await item.fn(evt);
  }
  for (const item of list.filter(x => !x.capture)) {
    await item.fn(evt);
  }
}

(async () => {
  // Confirm docListeners["click"][0] remains the bubble symbol opener
  const firstClickEntry = docListeners["click"][0];
  if (!firstClickEntry || firstClickEntry.capture !== false) {
    throw new Error("expected docListeners['click'][0] to stay the bubble symbol opener");
  }

  const container = createMockElement("DIV");
  container.className = "diff-container";
  container.setAttribute("data-file-path", "foo.go");
  container.setAttribute("data-repo", "r");
  container.setAttribute("data-change", "c");
  body.appendChild(container);

  const table = createMockElement("TABLE");
  table.className = "diff-table";
  container.appendChild(table);

  const tr = createMockElement("TR");
  tr.className = "diff-row";
  tr.setAttribute("data-line-r", "10");
  table.appendChild(tr);

  const td = createMockElement("TD");
  td.setAttribute("data-side", "R");
  tr.appendChild(td);

  const tokA = createMockElement("SPAN");
  tokA.className = "tok-id";
  tokA.textContent = "SymA";
  tokA._rect = { top: 120, left: 100, bottom: 140, right: 150, width: 50, height: 20 };
  td.appendChild(tokA);

  const tokB = createMockElement("SPAN");
  tokB.className = "tok-id";
  tokB.textContent = "SymB";
  tokB._rect = { top: 150, left: 200, bottom: 170, right: 250, width: 50, height: 20 };
  td.appendChild(tokB);

  const kwTok = createMockElement("SPAN");
  kwTok.className = "tok-kw";
  kwTok.textContent = "return";
  td.appendChild(kwTok);

  const commentBtn = createMockElement("BUTTON");
  commentBtn.className = "line-comment-btn";
  td.appendChild(commentBtn);

  // Non-Go container
  const mdContainer = createMockElement("DIV");
  mdContainer.className = "diff-container";
  mdContainer.setAttribute("data-file-path", "README.md");
  body.appendChild(mdContainer);
  const mdTable = createMockElement("TABLE");
  mdTable.className = "diff-table";
  mdContainer.appendChild(mdTable);
  const mdTr = createMockElement("TR");
  mdTr.setAttribute("data-line-r", "5");
  mdTable.appendChild(mdTr);
  const mdTd = createMockElement("TD");
  mdTd.setAttribute("data-side", "R");
  mdTr.appendChild(mdTd);
  const mdTok = createMockElement("SPAN");
  mdTok.className = "tok-id";
  mdTok.textContent = "Word";
  mdTd.appendChild(mdTok);

  async function realClick(target, extra) {
    const base = Object.assign({ button: 0, detail: 1, clientX: 500, clientY: 500, target, preventDefault() {}, stopPropagation() {} }, extra || {});
    await dispatchElAndDoc(target, "pointerdown", base);
    return await dispatchElAndDoc(target, "click", base);
  }

  // 1. Uninteracted popup stays open when clicking inside it (including Refs + chaining), and stays non-persistent
  await realClick(tokA);
  if (win.openSymbolPopups.length !== 1) throw new Error("expected 1 popup open");
  let p1 = win.openSymbolPopups[0];
  if (win.isPopupPersistent(p1)) throw new Error("new popup must not be persistent initially");

  const refsBtn = p1.el.querySelector(".sym-btn-refs");
  if (!refsBtn) throw new Error("expected .sym-btn-refs in rendered popup");
  await realClick(refsBtn);
  if (win.openSymbolPopups.length !== 1 || win.openSymbolPopups[0] !== p1) {
    throw new Error("clicking Refs inside popup must keep popup open");
  }
  await realClick(p1.el);
  if (win.openSymbolPopups.length !== 1 || win.isPopupPersistent(p1)) {
    throw new Error("clicking inside popup / viewing Refs must keep popup open and still non-persistent");
  }

  // 2. Outside pointerdown with right-click (button: 2) does NOT close p1 prematurely; subsequent contextmenu closes it with preventDefault
  await dispatchDoc("pointerdown", { button: 2, clientX: 700, clientY: 700, target: body });
  if (win.openSymbolPopups.length !== 1) throw new Error("outside right-click pointerdown must NOT close popup before contextmenu");
  const ctxEvt = { button: 2, clientX: 700, clientY: 700, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }, target: body };
  await dispatchDoc("contextmenu", ctxEvt);
  if (!ctxEvt.defaultPrevented || win.openSymbolPopups.length !== 0) {
    throw new Error("outside contextmenu should close uninteracted popup and call preventDefault");
  }

  // 3. Outside primary pointerdown (button: 0 and button: undefined) closes uninteracted popup + cleans stray DOM nodes
  await realClick(tokA);
  const stray = createMockElement("DIV");
  stray.className = "symbol-popup";
  stray.id = "stray-pop";
  body.appendChild(stray);
  await dispatchDoc("pointerdown", { button: 0, target: body });
  if (win.openSymbolPopups.length !== 0 || body.querySelectorAll(".symbol-popup").length !== 0 || tokA.classList.contains("sym-active")) {
    throw new Error("outside left pointerdown should close uninteracted popup, remove sym-active, and clean stray popups");
  }

  await realClick(tokA);
  await dispatchDoc("pointerdown", { target: td }); // button === undefined
  if (win.openSymbolPopups.length !== 0) {
    throw new Error("outside pointerdown with button=undefined should close uninteracted popup");
  }

  // 4. Outside click (even WITHOUT prior pointerdown) closes uninteracted popup across all non-opener targets
  for (const [label, target, extra] of [
    ["body", body, {}],
    ["commentBtn", commentBtn, {}],
    ["keyword", kwTok, {}],
    ["nonGoTok", mdTok, {}],
    ["doubleClickTok", tokB, { detail: 2 }],
  ]) {
    await firstClickEntry.fn({ button: 0, detail: 1, target: tokA, preventDefault() {}, stopPropagation() {} });
    if (win.openSymbolPopups.length !== 1) throw new Error("setup failed before " + label);
    await dispatchDoc("click", Object.assign({ button: 0, detail: 1, target, preventDefault() {}, stopPropagation() {} }, extra));
    if (win.openSymbolPopups.length !== 0) {
      throw new Error("click without pointerdown on " + label + " should close uninteracted popup");
    }
  }

  // 5. Text selection anchored inside popup releasing outside must not close popup on that click; subsequent outside click closes it
  await realClick(tokA);
  p1 = win.openSymbolPopups[0];
  const locInside = p1.el.querySelector(".sym-popup-loc");
  await dispatchDoc("pointerdown", { button: 0, target: locInside });
  currentSelection = { isCollapsed: false, anchorNode: { nodeType: 3, parentElement: locInside }, toString: () => "foo.go" };
  await dispatchDoc("click", { button: 0, detail: 1, target: body, preventDefault() {}, stopPropagation() {} });
  if (win.openSymbolPopups.length !== 1) {
    throw new Error("drag-selecting text from inside popup and releasing outside should keep popup open");
  }
  currentSelection = { isCollapsed: true, anchorNode: null, toString: () => "" };
  await realClick(body);
  if (win.openSymbolPopups.length !== 0) {
    throw new Error("subsequent outside click after selection should close uninteracted popup");
  }

  // 6. Opening via tokB, handleOpenMovePeerPopup, handleStickyScopeClick, or openSymbolPopupAt directly closes prior uninteracted popup
  await firstClickEntry.fn({ button: 0, detail: 1, target: tokA, preventDefault() {}, stopPropagation() {} });
  const firstInst = win.openSymbolPopups[0];
  await dispatchDoc("click", { button: 0, detail: 1, target: tokB, preventDefault() {}, stopPropagation() {} });
  if (win.openSymbolPopups.length !== 1 || win.openSymbolPopups[0] === firstInst) {
    throw new Error("clicking tokB via doc click should auto-close uninteracted tokA popup and open tokB");
  }
  const secondInst = win.openSymbolPopups[0];

  const moveBtn = createMockElement("BUTTON");
  moveBtn.setAttribute("data-peer-path", "foo.go");
  moveBtn.setAttribute("data-peer-start", "1");
  moveBtn.setAttribute("data-peer-end", "3");
  moveBtn.setAttribute("data-peer-side", "R");
  container.appendChild(moveBtn);
  await win.handleOpenMovePeerPopup(null, moveBtn);
  if (win.openSymbolPopups.length !== 1 || win.openSymbolPopups[0] === secondInst) {
    throw new Error("handleOpenMovePeerPopup should close prior uninteracted popup");
  }
  const moveInst = win.openSymbolPopups[0];

  const scopeBar = createMockElement("DIV");
  scopeBar.setAttribute("data-name-line", "10");
  scopeBar.setAttribute("data-name-col", "5");
  scopeBar.setAttribute("data-active-side", "R");
  container.appendChild(scopeBar);
  await win.handleStickyScopeClick(null, scopeBar);
  if (win.openSymbolPopups.length !== 1 || win.openSymbolPopups[0] === moveInst) {
    throw new Error("handleStickyScopeClick should close prior uninteracted popup");
  }

  await win.openSymbolPopupAt(container, "R", 10, 1, tokA, true);
  if (win.openSymbolPopups.length !== 1) {
    throw new Error("direct openSymbolPopupAt should close prior uninteracted popup");
  }
  win.closeAllSymbolPopups();

  // 7. Text node target inside popup preserves that popup
  await realClick(tokA);
  p1 = win.openSymbolPopups[0];
  const textTargetInside = { nodeType: 3, parentElement: p1.el.querySelector(".sym-popup-loc") };
  await dispatchDoc("pointerdown", { button: 0, target: textTargetInside });
  await dispatchDoc("click", { button: 0, detail: 1, target: textTargetInside, preventDefault() {}, stopPropagation() {} });
  if (win.openSymbolPopups.length !== 1) {
    throw new Error("text node target inside popup should keep popup open");
  }
  win.closeAllSymbolPopups();

  // 8. ALL persistence flags (moved, resizedW, resizedH, snapped, shaded, usedShade after unshade) keep popups open
  //    while clicking outside or opening a new popup closes any unpinned peer popup!
  // 8a. moved (real header drag)
  await realClick(tokA);
  const pMoved = win.openSymbolPopups[0];
  const hdr = pMoved.el.querySelector(".sym-popup-header");
  await dispatchElAndDoc(hdr, "pointerdown", { button: 0, clientX: 120, clientY: 130, target: hdr });
  await dispatchWin("pointermove", { clientX: 145, clientY: 150 });
  await dispatchWin("pointerup", { clientX: 145, clientY: 150 });
  if (!pMoved.moved || !win.isPopupPersistent(pMoved)) throw new Error("expected pMoved persistent after drag");

  // 8b. resizedW (real east resize drag)
  await realClick(tokB);
  const pResW = win.openSymbolPopups.find(p => p !== pMoved);
  const handleE = pResW.el.querySelector(".sym-resize-e");
  await dispatchElAndDoc(handleE, "pointerdown", { button: 0, clientX: 300, clientY: 200, target: handleE });
  await dispatchWin("pointermove", { clientX: 340, clientY: 200 });
  await dispatchWin("pointerup", { clientX: 340, clientY: 200 });
  if (!pResW.resizedW || !win.isPopupPersistent(pResW)) throw new Error("expected pResW persistent after horizontal resize");

  // 8c. resizedH (real south resize drag)
  await realClick(tokA);
  const pResH = win.openSymbolPopups.find(p => p !== pMoved && p !== pResW);
  const handleS = pResH.el.querySelector(".sym-resize-s");
  await dispatchElAndDoc(handleS, "pointerdown", { button: 0, clientX: 200, clientY: 300, target: handleS });
  await dispatchWin("pointermove", { clientX: 200, clientY: 350 });
  await dispatchWin("pointerup", { clientX: 200, clientY: 350 });
  if (!pResH.resizedH || !win.isPopupPersistent(pResH)) throw new Error("expected pResH persistent after vertical resize");

  // 8d. snapped
  await realClick(tokB);
  const pSnap = win.openSymbolPopups.find(p => p !== pMoved && p !== pResW && p !== pResH);
  win.applyPopupSnap(pSnap, "left");
  if (!pSnap.snapped || !win.isPopupPersistent(pSnap)) throw new Error("expected pSnap persistent after snap");

  // 8e. shaded
  await realClick(tokA);
  const pShade = win.openSymbolPopups.find(p => p !== pMoved && p !== pResW && p !== pResH && p !== pSnap);
  await realClick(pShade.el.querySelector(".sym-btn-shade"));
  if (!pShade.shaded || !pShade.usedShade || !win.isPopupPersistent(pShade)) {
    throw new Error("expected pShade persistent after shading");
  }

  // 8f. usedShade after unshading
  await realClick(tokB);
  const pUnshade = win.openSymbolPopups.find(p => p !== pMoved && p !== pResW && p !== pResH && p !== pSnap && p !== pShade);
  await realClick(pUnshade.el.querySelector(".sym-btn-shade"));
  await realClick(pUnshade.el.querySelector(".sym-btn-shade"));
  if (pUnshade.shaded || !pUnshade.usedShade || !win.isPopupPersistent(pUnshade)) {
    throw new Error("expected pUnshade persistent via usedShade after unshading");
  }

  // Open an uninteracted 7th popup pTemp alongside the 6 pinned popups
  await realClick(tokA);
  if (win.openSymbolPopups.length !== 7) {
    throw new Error("expected 6 persistent + 1 uninteracted popups open, got " + win.openSymbolPopups.length);
  }
  const pTemp = win.openSymbolPopups[6];
  if (win.isPopupPersistent(pTemp)) throw new Error("expected pTemp to be uninteracted");

  // Click inside pMoved -> pTemp (unpinned) auto-closes while all 6 persistent popups stay open!
  await realClick(pMoved.el);
  if (win.openSymbolPopups.length !== 6 || win.openSymbolPopups.includes(pTemp)) {
    throw new Error("clicking persistent pMoved should close unpinned pTemp and keep all 6 persistent popups");
  }

  // Re-open pTemp, then click outside on body -> pTemp auto-closes, all 6 persistent popups stay open!
  await realClick(tokA);
  if (win.openSymbolPopups.length !== 7) throw new Error("expected 7 popups with new pTemp");
  await realClick(body);
  if (win.openSymbolPopups.length !== 6) {
    throw new Error("clicking outside on body should close only unpinned pTemp, got " + win.openSymbolPopups.length);
  }
  for (const pinned of [pMoved, pResW, pResH, pSnap, pShade, pUnshade]) {
    if (!win.openSymbolPopups.includes(pinned)) {
      throw new Error("expected pinned popup " + pinned.id + " to stay open after outside click");
    }
  }

  console.log("OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TestSymbolPopupAutoCloseUninteracted node test failed: %v\n%s", err, string(out))
	} else if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected OK output, got: %s", string(out))
	}
}

func TestCommentCollapseExpand(t *testing.T) {
	// 1. Unit tests for collapseCommentPreview (fast-path ASCII, UTF-8, boundary trailing space, equivalence).
	refCollapsePreview := func(s string) string {
		joined := strings.Join(strings.Fields(s), " ")
		runes := []rune(joined)
		if len(runes) > 240 {
			return string(runes[:240]) + "…"
		}
		return joined
	}
	longRunes := strings.Repeat("é", 240)
	longASCII := strings.Repeat("a", 240)
	shortASCII := strings.Repeat("ab c ", 48)[:239]
	for _, sample := range []string{
		"",
		"   \n\t  ",
		"  hello\n\tworld   foo  ",
		" leading",
		"trailing ",
		"two  spaces",
		"single-line ascii comment",
		shortASCII,
		longASCII,
		longASCII + "   ",
		longASCII + " b",
		strings.Repeat("a", 239) + "   ",
		strings.Repeat("a", 239) + "   b",
		longRunes,
		longRunes + "\t\n ",
		strings.Repeat("é", 239) + "\n\tb",
		strings.Repeat("é", 245) + "\nextra",
		"a\u00a0\u2003b\u0085c",
		"a\nb\r\nc\td   " + strings.Repeat("🚀", 250),
	} {
		want := refCollapsePreview(sample)
		if got := collapseCommentPreview(sample); got != want {
			t.Fatalf("collapseCommentPreview(%q) = %q (runes=%d), want %q (runes=%d)", sample, got, utf8.RuneCountInString(got), want, utf8.RuneCountInString(want))
		}
	}

	// 2. Seed change with unresolved (single & multi), resolved-published, resolved+draft-reply, and resolved-draft-root threads.
	srv, ts, d, repoDir := setupTestServer(t)
	defer srv.broker.Close()
	defer ts.Close()
	defer d.Close()

	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := vcs.Open(repoDir, srv.cache)
	changes, _ := repo.Changes()
	c := changes[0]

	if emptyHTML, err := srv.renderThreadCard(repo, &db.Thread{ID: 99999}, c.Parent, c.Rev); err != nil || emptyHTML != "" {
		t.Fatalf("renderThreadCard empty messages = (%q, %v), want (\"\", nil)", emptyHTML, err)
	}

	thUnresSingle, mUnresSingle, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "hello.go", "R", 1, 1, "package main", "alice", "single unresolved comment", false)
	if err != nil {
		t.Fatal(err)
	}

	thUnresMulti, mUnresRoot, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "hello.go", "R", 1, 1, "package main", "alice", "unres root\n\tsecond   line", false)
	if err != nil {
		t.Fatal(err)
	}
	mUnresReply, err := d.AddMessage(thUnresMulti.ID, "bob", "unres reply", false)
	if err != nil {
		t.Fatal(err)
	}

	longResBody := "res  root\n" + strings.Repeat("é", 240) + " trailing"
	thResPubMulti, mResPubRoot, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "hello.go", "R", 2, 2, "", "carol", longResBody, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetResolved(thResPubMulti.ID, true); err != nil {
		t.Fatal(err)
	}
	mResPubReply, err := d.AddMessage(thResPubMulti.ID, "dave", "res reply <escaped>", false)
	if err != nil {
		t.Fatal(err)
	}

	thResDraftReply, mResDRRoot, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "hello.go", "R", 2, 2, "", "erin", "published root", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetResolved(thResDraftReply.ID, true); err != nil {
		t.Fatal(err)
	}
	mResDRReply, err := d.AddMessage(thResDraftReply.ID, "alice", "draft reply body", true)
	if err != nil {
		t.Fatal(err)
	}

	thResDraftRoot, mResDRootMsg, err := d.AddThread(rec.ID, c.Key, c.Rev, 0, "hello.go", "R", 3, 3, "func main() {}", "alice", "resolved draft root", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetResolved(thResDraftRoot.ID, true); err != nil {
		t.Fatal(err)
	}

	// 3. Verify server-rendered change page HTML and shortcuts modal.
	pageURL := fmt.Sprintf("%s/r/%s/c/%s?diffs=all", ts.URL, rec.Slug, c.Key)
	resp, err := http.Get(pageURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s failed: status=%v err=%v", pageURL, resp.StatusCode, err)
	}
	pageBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	pageHTML := string(pageBytes)

	if !strings.Contains(pageHTML, `<tr><td><kbd>e</kbd> / <kbd>E</kbd></td><td>Expand / collapse all comments</td></tr>`) {
		t.Errorf("expected #shortcutsModal to document e / E comment expand/collapse shortcuts")
	}

	extractDivBlock := func(htmlStr, marker string) string {
		idx := strings.Index(htmlStr, marker)
		if idx == -1 {
			t.Fatalf("missing %s in HTML", marker)
		}
		start := strings.LastIndex(htmlStr[:idx+1], "<div")
		if start == -1 {
			t.Fatalf("missing opening <div for %s", marker)
		}
		depth := 0
		pos := start
		for pos < len(htmlStr) {
			nextOpen := strings.Index(htmlStr[pos:], "<div")
			nextClose := strings.Index(htmlStr[pos:], "</div>")
			if nextClose == -1 {
				t.Fatalf("unclosed <div> starting at %s", marker)
			}
			if nextOpen != -1 && nextOpen < nextClose {
				depth++
				pos += nextOpen + 4
			} else {
				depth--
				pos += nextClose + len("</div>")
				if depth == 0 {
					return htmlStr[start:pos]
				}
			}
		}
		t.Fatalf("unterminated <div> starting at %s", marker)
		return ""
	}

	// Unresolved 1-message thread: single message starts open.
	unresSingleCard := extractDivBlock(pageHTML, fmt.Sprintf(`id="thread-%d"`, thUnresSingle.ID))
	if strings.Contains(unresSingleCard, "first-msg-collapsed") || strings.Contains(unresSingleCard, "msg-collapsed") {
		t.Errorf("unresolved single-message thread %d should start open, got: %s", thUnresSingle.ID, unresSingleCard)
	}
	unresSingleHdr := extractDivBlock(unresSingleCard, `class="thread-header"`)
	if !strings.Contains(unresSingleHdr, `data-collapsed="false"`) ||
		!strings.Contains(unresSingleHdr, `aria-expanded="true"`) ||
		!strings.Contains(unresSingleHdr, `<span class="comment-collapsed-preview">single unresolved comment</span>`) {
		t.Errorf("unresolved single root header unexpected: %s", unresSingleHdr)
	}

	// Unresolved multi-message thread: earlier message(s) start collapsed, only latest reply open.
	unresCard := extractDivBlock(pageHTML, fmt.Sprintf(`id="thread-%d"`, thUnresMulti.ID))
	if !strings.Contains(unresCard, "first-msg-collapsed") {
		t.Errorf("unresolved multi-message thread %d should have first-msg-collapsed on root, got: %s", thUnresMulti.ID, unresCard)
	}
	unresHdr := extractDivBlock(unresCard, `class="thread-header"`)
	if !strings.Contains(unresHdr, `data-collapsed="true"`) ||
		!strings.Contains(unresHdr, `aria-expanded="false"`) ||
		!strings.Contains(unresHdr, `<span class="comment-collapsed-preview">unres root second line</span>`) {
		t.Errorf("unresolved root header unexpected: %s", unresHdr)
	}
	unresRootMsg := extractDivBlock(unresCard, fmt.Sprintf(`id="msg-%d"`, mUnresRoot.ID))
	if !strings.Contains(unresRootMsg, `class="message-body msg-collapsed"`) {
		t.Errorf("unresolved multi-message root %d should start collapsed, got: %s", mUnresRoot.ID, unresRootMsg)
	}
	unresReplyMsg := extractDivBlock(unresCard, fmt.Sprintf(`id="msg-%d"`, mUnresReply.ID))
	if strings.Contains(unresReplyMsg, "msg-collapsed") ||
		!strings.Contains(unresReplyMsg, `class="message-body message-reply"`) ||
		!strings.Contains(unresReplyMsg, `data-collapsed="false"`) ||
		!strings.Contains(unresReplyMsg, `aria-expanded="true"`) {
		t.Errorf("unresolved latest reply %d should start open: %s", mUnresReply.ID, unresReplyMsg)
	}

	// Resolved multi-message published thread: all collapsed + truncated preview.
	resPubCard := extractDivBlock(pageHTML, fmt.Sprintf(`id="thread-%d"`, thResPubMulti.ID))
	if !strings.Contains(resPubCard, "first-msg-collapsed") {
		t.Errorf("resolved published thread %d should have first-msg-collapsed, got: %s", thResPubMulti.ID, resPubCard)
	}
	resPubHdr := extractDivBlock(resPubCard, `class="thread-header"`)
	wantPreview := collapseCommentPreview(longResBody)
	if !strings.Contains(resPubHdr, `data-collapsed="true"`) ||
		!strings.Contains(resPubHdr, `aria-expanded="false"`) ||
		!strings.Contains(resPubHdr, fmt.Sprintf(`<span class="comment-collapsed-preview">%s</span>`, wantPreview)) {
		t.Errorf("resolved published root header unexpected (want preview %q): %s", wantPreview, resPubHdr)
	}
	resPubRootMsg := extractDivBlock(resPubCard, fmt.Sprintf(`id="msg-%d"`, mResPubRoot.ID))
	if !strings.Contains(resPubRootMsg, `class="message-body msg-collapsed"`) {
		t.Errorf("resolved published root msg should have msg-collapsed: %s", resPubRootMsg)
	}
	resPubReplyMsg := extractDivBlock(resPubCard, fmt.Sprintf(`id="msg-%d"`, mResPubReply.ID))
	if !strings.Contains(resPubReplyMsg, `class="message-body message-reply msg-collapsed"`) ||
		!strings.Contains(resPubReplyMsg, `data-collapsed="true"`) ||
		!strings.Contains(resPubReplyMsg, `aria-expanded="false"`) ||
		!strings.Contains(resPubReplyMsg, `res reply &lt;escaped&gt;`) {
		t.Errorf("resolved published reply unexpected: %s", resPubReplyMsg)
	}

	// Resolved thread with published root + draft reply: published root collapsed, draft reply expanded.
	resDRCard := extractDivBlock(pageHTML, fmt.Sprintf(`id="thread-%d"`, thResDraftReply.ID))
	if !strings.Contains(resDRCard, "first-msg-collapsed") {
		t.Errorf("resolved thread %d with published root should have first-msg-collapsed", thResDraftReply.ID)
	}
	resDRRootMsg := extractDivBlock(resDRCard, fmt.Sprintf(`id="msg-%d"`, mResDRRoot.ID))
	if !strings.Contains(resDRRootMsg, `class="message-body msg-collapsed"`) {
		t.Errorf("published root %d should start collapsed, got: %s", mResDRRoot.ID, resDRRootMsg)
	}
	resDRReplyMsg := extractDivBlock(resDRCard, fmt.Sprintf(`id="msg-%d"`, mResDRReply.ID))
	if strings.Contains(resDRReplyMsg, "msg-collapsed") ||
		!strings.Contains(resDRReplyMsg, `data-collapsed="false"`) ||
		!strings.Contains(resDRReplyMsg, `aria-expanded="true"`) {
		t.Errorf("draft reply %d on resolved thread should start expanded, got: %s", mResDRReply.ID, resDRReplyMsg)
	}

	// Resolved thread with draft root: draft root starts expanded.
	resDRootCard := extractDivBlock(pageHTML, fmt.Sprintf(`id="thread-%d"`, thResDraftRoot.ID))
	if strings.Contains(resDRootCard, "first-msg-collapsed") || strings.Contains(resDRootCard, "msg-collapsed") {
		t.Errorf("resolved thread %d with draft root should render expanded, got: %s", thResDraftRoot.ID, resDRootCard)
	}
	resDRootHdr := extractDivBlock(resDRootCard, `class="thread-header"`)
	if !strings.Contains(resDRootHdr, `data-collapsed="false"`) || !strings.Contains(resDRootHdr, `aria-expanded="true"`) {
		t.Errorf("resolved draft root header should have data-collapsed=false, got: %s", resDRootHdr)
	}

	// Pre-render server API responses we'll feed back into mock fetch in Node vm.
	postAPI := func(endpoint string, payload map[string]any) string {
		raw, _ := json.Marshal(payload)
		r, err := http.Post(ts.URL+endpoint, "application/json", bytes.NewReader(raw))
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("POST %s failed: status=%v err=%v", endpoint, r.StatusCode, err)
		}
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// Capture real server API payloads to exercise client JS mutations against server-rendered HTML.
	replyUnresSingleJSON := postAPI("/api/threads/reply", map[string]any{
		"thread_id": thUnresSingle.ID,
		"body":      "new reply on single unres thread",
		"resolved":  false,
		"left_rev":  c.Parent,
		"right_rev": c.Rev,
	})
	var singleReplyOut struct {
		Message struct {
			ID int64 `json:"id"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(replyUnresSingleJSON), &singleReplyOut); err != nil {
		t.Fatal(err)
	}
	deleteUnresSingleReplyJSON := postAPI("/api/threads/delete", map[string]any{
		"message_id": singleReplyOut.Message.ID,
		"left_rev":   c.Parent,
		"right_rev":  c.Rev,
	})

	resolveTrueJSON := postAPI("/api/threads/resolve", map[string]any{
		"thread_id": thUnresMulti.ID,
		"resolved":  true,
		"left_rev":  c.Parent,
		"right_rev": c.Rev,
	})
	resolveFalseJSON := postAPI("/api/threads/resolve", map[string]any{
		"thread_id": thUnresMulti.ID,
		"resolved":  false,
		"left_rev":  c.Parent,
		"right_rev": c.Rev,
	})
	editDRReplyJSON := postAPI("/api/threads/edit", map[string]any{
		"message_id": mResDRReply.ID,
		"body":       "edited draft reply body",
		"left_rev":   c.Parent,
		"right_rev":  c.Rev,
	})
	replyResPubJSON := postAPI("/api/threads/reply", map[string]any{
		"thread_id": thResPubMulti.ID,
		"body":      "new third draft reply",
		"resolved":  true,
		"left_rev":  c.Parent,
		"right_rev": c.Rev,
	})
	var replyOut struct {
		Message struct {
			ID int64 `json:"id"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(replyResPubJSON), &replyOut); err != nil {
		t.Fatal(err)
	}
	deleteResPubReplyJSON := postAPI("/api/threads/delete", map[string]any{
		"message_id": replyOut.Message.ID,
		"left_rev":   c.Parent,
		"right_rev":  c.Rev,
	})
	publishDraftsJSON := postAPI("/api/publish", map[string]any{
		"repo":      rec.Slug,
		"change":    c.Key,
		"left_rev":  c.Parent,
		"right_rev": c.Rev,
	})

	// 4. Verify client JS behavior with Node vm against the real rendered HTML strings.
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	fixturesJSON, err := json.Marshal(map[string]any{
		"repo":              rec.Slug,
		"change":            c.Key,
		"leftRev":           c.Parent,
		"rightRev":          c.Rev,
		"cards":             []string{unresSingleCard, unresCard, resPubCard, resDRCard, resDRootCard},
		"thUnresSingle":     thUnresSingle.ID,
		"mUnresSingle":      mUnresSingle.ID,
		"thUnresMulti":      thUnresMulti.ID,
		"mUnresRoot":        mUnresRoot.ID,
		"mUnresReply":       mUnresReply.ID,
		"thResPubMulti":     thResPubMulti.ID,
		"mResPubRoot":       mResPubRoot.ID,
		"mResPubReply":      mResPubReply.ID,
		"thResDR":           thResDraftReply.ID,
		"mResDRRoot":        mResDRRoot.ID,
		"mResDRReply":       mResDRReply.ID,
		"thResDRoot":        thResDraftRoot.ID,
		"mResDRoot":         mResDRootMsg.ID,
		"allMsgIDs":         []int64{mUnresSingle.ID, mUnresRoot.ID, mUnresReply.ID, mResPubRoot.ID, mResPubReply.ID, mResDRRoot.ID, mResDRReply.ID, mResDRootMsg.ID},
		"replyUnresSingle":  json.RawMessage(replyUnresSingleJSON),
		"deleteUnresSingle": json.RawMessage(deleteUnresSingleReplyJSON),
		"editPayload":       json.RawMessage(editDRReplyJSON),
		"replyPayload":      json.RawMessage(replyResPubJSON),
		"deletePayload":     json.RawMessage(deleteResPubReplyJSON),
		"resTrue":           json.RawMessage(resolveTrueJSON),
		"resFalse":          json.RawMessage(resolveFalseJSON),
		"publishPayload":    json.RawMessage(publishDraftsJSON),
	})
	if err != nil {
		t.Fatal(err)
	}

	nodeScript := "const FIX = " + string(fixturesJSON) + ";\n" + `
const fs = require("fs");
const vm = require("vm");
const path = require("path");

function decodeEnt(s) {
  return String(s || "")
    .replace(/&quot;/g, '"')
    .replace(/&#34;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&amp;/g, "&");
}

function createEl(tag) {
  const classes = new Set();
  const attrs = {};
  const childNodes = [];
  const el = {
    nodeType: 1,
    tagName: String(tag || "DIV").toUpperCase(),
    attributes: attrs,
    childNodes: childNodes,
    parentNode: null,
    style: {},
    value: "",
    checked: false,
    offsetTop: 0,
    offsetHeight: 40,
    offsetWidth: 200,
    offsetParent: {},
    _listeners: {},
    focus() { doc.activeElement = el; },
    blur() { if (doc.activeElement === el) doc.activeElement = null; },
    setAttribute(k, v) {
      if (k === "id") { el.id = String(v); }
      else if (k === "class") { el.className = String(v); }
      attrs[k] = String(v);
    },
    getAttribute(k) {
      if (k === "id") return el.id !== undefined && el.id !== "" ? el.id : (attrs.id !== undefined ? attrs.id : null);
      if (k === "class") return el.className;
      return attrs[k] !== undefined ? attrs[k] : null;
    },
    hasAttribute(k) { return el.getAttribute(k) !== null; },
    removeAttribute(k) {
      if (k === "id") el.id = "";
      delete attrs[k];
    },
    appendChild(child) {
      if (!child) return child;
      child.parentNode = el;
      childNodes.push(child);
      return child;
    },
    get children() { return childNodes.filter(n => n.nodeType === 1); },
    get firstElementChild() { return childNodes.find(n => n.nodeType === 1) || null; },
    get parentElement() { return (el.parentNode && el.parentNode.nodeType === 1) ? el.parentNode : null; },
    contains(target) {
      let cur = target;
      while (cur) {
        if (cur === el) return true;
        cur = cur.parentNode;
      }
      return false;
    },
    replaceWith(...nodes) {
      if (!el.parentNode) return;
      const sibs = el.parentNode.childNodes;
      const idx = sibs.indexOf(el);
      if (idx === -1) return;
      for (const n of nodes) {
        if (n && typeof n === "object") n.parentNode = el.parentNode;
      }
      sibs.splice(idx, 1, ...nodes);
      el.parentNode = null;
    },
    remove() {
      if (!el.parentNode) return;
      const idx = el.parentNode.childNodes.indexOf(el);
      if (idx !== -1) el.parentNode.childNodes.splice(idx, 1);
      el.parentNode = null;
    },
    closest(sel) {
      const parts = sel.split(",").map(s => s.trim());
      let cur = el;
      while (cur) {
        for (const p of parts) {
          if (matchesSimpleSelector(cur, p)) return cur;
        }
        cur = cur.parentNode;
      }
      return null;
    },
    querySelector(sel) { return el.querySelectorAll(sel)[0] || null; },
    querySelectorAll(sel) {
      const parts = sel.split(",").map(s => s.trim());
      const res = [];
      function walk(node) {
        for (const c of (node.childNodes || [])) {
          if (c.nodeType === 1) {
            for (const p of parts) {
              if (matchesSimpleSelector(c, p) && !res.includes(c)) res.push(c);
            }
            walk(c);
          }
        }
      }
      walk(el);
      return res;
    },
    addEventListener(ev, fn) {
      if (!el._listeners[ev]) el._listeners[ev] = [];
      el._listeners[ev].push(fn);
    },
    removeEventListener() {},
    scrollIntoView() {
      scrollCalls.push(el.id || el.className);
      win.scrollY = Math.max(0, el.offsetTop - 100);
    },
    getBoundingClientRect() {
      const top = el.offsetTop - (win.scrollY || 0);
      return { top, bottom: top + (el.offsetHeight || 40), left: 0, right: 200, width: 200, height: el.offsetHeight || 40 };
    }
  };

  Object.defineProperty(el, "className", {
    get() { return Array.from(classes).join(" "); },
    set(v) {
      classes.clear();
      String(v || "").trim().split(/\s+/).forEach(c => { if (c) classes.add(c); });
    }
  });
  el.classList = {
    add(...cls) { cls.forEach(c => classes.add(c)); },
    remove(...cls) { cls.forEach(c => classes.delete(c)); },
    contains(c) { return classes.has(c); }
  };
  Object.defineProperty(el, "innerHTML", {
    get() { return el._html || ""; },
    set(v) {
      el._html = String(v || "");
      childNodes.length = 0;
      parseHTMLInto(el, el._html);
    }
  });
  return el;
}

const VOID_TAGS = new Set(["input", "br", "hr", "img", "polyline", "path", "circle", "rect", "line"]);
function parseHTMLInto(root, htmlStr) {
  const stack = [root];
  const tokenRe = /<(\/?)([a-zA-Z0-9-]+)([^>]*)>|([^<]+)/g;
  let m;
  while ((m = tokenRe.exec(htmlStr)) !== null) {
    if (m[4]) {
      const cur = stack[stack.length - 1];
      const txt = decodeEnt(m[4]);
      if (cur.tagName === "TEXTAREA") cur.value += txt;
      cur.appendChild({ nodeType: 3, textContent: txt, parentNode: cur });
      continue;
    }
    const isClose = m[1] === "/";
    const tag = m[2].toLowerCase();
    const attrStr = m[3] || "";
    const selfClose = attrStr.trim().endsWith("/") || VOID_TAGS.has(tag);
    if (isClose) {
      for (let i = stack.length - 1; i > 0; i--) {
        if (stack[i].tagName === tag.toUpperCase()) {
          stack.length = i;
          break;
        }
      }
      continue;
    }
    const node = createEl(tag);
    const attrRe = /([a-zA-Z0-9_:-]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>/]+)))?/g;
    let am;
    while ((am = attrRe.exec(attrStr)) !== null) {
      if (am[1] === "/") continue;
      const k = am[1];
      const val = decodeEnt(am[2] !== undefined ? am[2] : (am[3] !== undefined ? am[3] : (am[4] !== undefined ? am[4] : "")));
      if (k === "checked") node.checked = true;
      else if (k === "style") {
        const dm = val.match(/display\s*:\s*([^;]+)/i);
        if (dm) node.style.display = dm[1].trim();
      }
      node.setAttribute(k, val);
    }
    stack[stack.length - 1].appendChild(node);
    if (!selfClose) stack.push(node);
  }
}

function matchCompound(node, sel) {
  if (!node || node.nodeType !== 1 || !sel) return false;
  if (sel.includes(":not(")) {
    const main = sel.slice(0, sel.indexOf(":not("));
    const inner = sel.slice(sel.indexOf(":not(") + 5, -1);
    return (!main || matchCompound(node, main)) && !matchCompound(node, inner);
  }
  if (sel.startsWith("#")) return node.id === sel.slice(1);
  const parts = sel.split(".");
  const tag = parts[0];
  if (tag && node.tagName !== tag.toUpperCase()) return false;
  for (let i = 1; i < parts.length; i++) {
    if (!node.classList || !node.classList.contains(parts[i])) return false;
  }
  return true;
}

function matchesSimpleSelector(node, sel) {
  if (!node || node.nodeType !== 1 || !sel) return false;
  sel = sel.trim();
  const parts = sel.split(/\s+/);
  if (parts.length === 1) return matchCompound(node, parts[0]);
  if (!matchCompound(node, parts[parts.length - 1])) return false;
  let anc = node.parentNode;
  for (let i = parts.length - 2; i >= 0; i--) {
    while (anc && !matchCompound(anc, parts[i])) anc = anc.parentNode;
    if (!anc) return false;
    anc = anc.parentNode;
  }
  return true;
}

const body = createEl("BODY");
const docListeners = {};
const winListeners = {};
const scrollCalls = [];
let currentSelection = { isCollapsed: true, toString() { return ""; } };
let customFetch = null;

function findById(node, id) {
  if (!node) return null;
  if (node.id === id) return node;
  for (const c of (node.childNodes || [])) {
    if (c.nodeType === 1) {
      const f = findById(c, id);
      if (f) return f;
    }
  }
  return null;
}

const doc = {
  body: body,
  activeElement: null,
  documentElement: {
    clientWidth: 1000,
    clientHeight: 800,
    classList: { contains() { return false; }, add() {}, remove() {} },
    style: { setProperty() {}, getPropertyValue() { return ""; } },
    getAttribute() { return ""; },
    setAttribute() {},
    removeAttribute() {}
  },
  getElementById(id) { return findById(body, id); },
  createElement(tag) { return createEl(tag); },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); },
  addEventListener(ev, fn) { (docListeners[ev] = docListeners[ev] || []).push(fn); },
  removeEventListener() {}
};

const win = {
  document: doc,
  location: { pathname: "/r/" + FIX.repo + "/c/" + FIX.change, search: "", hash: "", href: "" },
  history: { replaceState() {} },
  scrollY: 0,
  innerHeight: 800,
  addEventListener(ev, fn) { (winListeners[ev] = winListeners[ev] || []).push(fn); },
  removeEventListener() {},
  matchMedia() { return { matches: false, addEventListener() {} }; },
  localStorage: { length: 0, key() { return null; }, getItem() { return null; }, setItem() {}, removeItem() {} },
  sessionStorage: { _m: new Map(), getItem(k) { return this._m.has(k) ? this._m.get(k) : null; }, setItem(k, v) { this._m.set(k, String(v)); } },
  getSelection() { return currentSelection; },
  fetch(url, opts) {
    if (customFetch) return customFetch(url, opts);
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ sig: "s1" }) });
  }
};
win.window = win;

const sandbox = vm.createContext({
  window: win,
  document: doc,
  localStorage: win.localStorage,
  sessionStorage: win.sessionStorage,
  URLSearchParams: URLSearchParams,
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  fetch: win.fetch,
  NodeFilter: { SHOW_TEXT: 4 },
  Node: { ELEMENT_NODE: 1, TEXT_NODE: 3 }
});

vm.runInContext(fs.readFileSync(path.join("static", "app.js"), "utf8"), sandbox);

function dispatchInlineClick(target, opts) {
  let stopped = false;
  const ev = {
    target,
    button: opts && opts.button !== undefined ? opts.button : 0,
    preventDefault() {},
    stopPropagation() { stopped = true; }
  };
  let cur = target;
  while (cur && !stopped) {
    ev.currentTarget = cur;
    if (cur.nodeType === 1) {
      const code = cur.getAttribute("onclick");
      if (code) {
        vm.runInContext("(function(event){ with(window){ " + code + " } })", sandbox).call(cur, ev);
      }
      for (const fn of (cur._listeners.click || [])) {
        fn.call(cur, ev);
      }
    }
    cur = cur.parentNode;
  }
}

(async () => {
  const appHeader = createEl("DIV");
  appHeader.className = "app-header";
  appHeader.offsetHeight = 50;
  body.appendChild(appHeader);

  const countsEl = createEl("DIV");
  countsEl.id = "changeCommentCounts";
  countsEl.setAttribute("data-repo", FIX.repo);
  countsEl.setAttribute("data-change", FIX.change);
  countsEl.setAttribute("data-base", FIX.leftRev);
  countsEl.setAttribute("data-target", FIX.rightRev);
  body.appendChild(countsEl);

  const allFiles = createEl("DIV");
  allFiles.className = "diff-all-files";
  body.appendChild(allFiles);

  const sec = createEl("DIV");
  sec.className = "diff-file-section";
  sec.setAttribute("data-file-path", "hello.go");
  allFiles.appendChild(sec);

  const fileBody = createEl("DIV");
  fileBody.className = "diff-file-body";
  sec.appendChild(fileBody);

  const diffCont = createEl("DIV");
  diffCont.className = "diff-container";
  diffCont.setAttribute("data-repo", FIX.repo);
  diffCont.setAttribute("data-change", FIX.change);
  diffCont.setAttribute("data-file-path", "hello.go");
  diffCont.setAttribute("data-left-rev", FIX.leftRev);
  diffCont.setAttribute("data-right-rev", FIX.rightRev);
  fileBody.appendChild(diffCont);

  FIX.cards.forEach((cardHTML, i) => {
    const slot = createEl("DIV");
    slot.innerHTML = cardHTML;
    const card = slot.firstElementChild;
    card.offsetTop = (i + 1) * 100;
    diffCont.appendChild(card);
  });

  // Add a .new-comment-card that e/E must skip.
  const newCard = createEl("DIV");
  newCard.className = "thread-card comment-form new-comment-card thread-unresolved";
  diffCont.appendChild(newCard);

  const keydown = winListeners.keydown[0];

  // A. Click .thread-header toggles root only (single unresolved starts open, multi unresolved starts root-collapsed + reply-open)
  const unresSingleCard = doc.getElementById("thread-" + FIX.thUnresSingle);
  const unresSingleRoot = doc.getElementById("msg-" + FIX.mUnresSingle);
  if (unresSingleCard.classList.contains("first-msg-collapsed") || unresSingleRoot.classList.contains("msg-collapsed")) {
    throw new Error("unresolved single-message thread should start expanded");
  }

  const unresCard = doc.getElementById("thread-" + FIX.thUnresMulti);
  const unresHdr = unresCard.querySelector(".thread-header");
  const unresRootMsg = doc.getElementById("msg-" + FIX.mUnresRoot);
  const unresReplyMsg = doc.getElementById("msg-" + FIX.mUnresReply);
  const unresBtn = unresHdr.querySelector(".comment-collapse-btn");
  if (!unresCard.classList.contains("first-msg-collapsed") ||
      !unresRootMsg.classList.contains("msg-collapsed") ||
      unresReplyMsg.classList.contains("msg-collapsed")) {
    throw new Error("unresolved multi-message thread should start with root collapsed and reply open");
  }
  const unresAuthorText = unresHdr.querySelector(".thread-author").childNodes[0];
  dispatchInlineClick(unresAuthorText);
  if (unresCard.classList.contains("first-msg-collapsed") ||
      unresRootMsg.classList.contains("msg-collapsed") ||
      unresRootMsg.getAttribute("data-user-toggled") !== "true" ||
      unresHdr.getAttribute("data-collapsed") !== "false" ||
      unresBtn.getAttribute("aria-expanded") !== "true" ||
      unresBtn.getAttribute("aria-label") !== "Collapse comment" ||
      unresBtn.getAttribute("title") !== "Collapse comment" ||
      unresReplyMsg.classList.contains("msg-collapsed")) {
    throw new Error("clicking .thread-header should expand root message only and mark data-user-toggled");
  }
  dispatchInlineClick(unresAuthorText);
  if (!unresCard.classList.contains("first-msg-collapsed") ||
      !unresRootMsg.classList.contains("msg-collapsed") ||
      unresHdr.getAttribute("data-collapsed") !== "true" ||
      unresBtn.getAttribute("aria-expanded") !== "false" ||
      unresBtn.getAttribute("aria-label") !== "Expand comment" ||
      unresBtn.getAttribute("title") !== "Expand comment") {
    throw new Error("second click on .thread-header should collapse root message again");
  }

  // B. Non-primary clicks and active text selection block header click, EXCEPT clicking .comment-collapse-btn (even its <polyline> child)
  dispatchInlineClick(unresHdr, { button: 1 });
  dispatchInlineClick(unresHdr, { button: 2 });
  if (!unresRootMsg.classList.contains("msg-collapsed")) {
    throw new Error("middle/right clicks on header must not toggle collapse");
  }
  currentSelection = { isCollapsed: false, toString() { return "selected text"; } };
  dispatchInlineClick(unresHdr);
  if (!unresRootMsg.classList.contains("msg-collapsed")) {
    throw new Error("active text selection should prevent header click from toggling");
  }
  const unresPoly = unresBtn.querySelector("polyline");
  dispatchInlineClick(unresPoly);
  currentSelection = { isCollapsed: true, toString() { return ""; } };
  if (unresCard.classList.contains("first-msg-collapsed") ||
      unresRootMsg.classList.contains("msg-collapsed") ||
      unresHdr.getAttribute("data-collapsed") !== "false" ||
      unresBtn.getAttribute("aria-expanded") !== "true" ||
      unresBtn.getAttribute("aria-label") !== "Collapse comment") {
    throw new Error("clicking .comment-collapse-btn polyline should expand root message even during selection");
  }

  // C. Clicking .reply-header on resolved thread expands reply while keeping root collapsed
  const resPubCard = doc.getElementById("thread-" + FIX.thResPubMulti);
  const resPubRootMsg = doc.getElementById("msg-" + FIX.mResPubRoot);
  const resPubReplyMsg = doc.getElementById("msg-" + FIX.mResPubReply);
  const resPubReplyHdr = resPubReplyMsg.querySelector(".reply-header");
  dispatchInlineClick(resPubReplyHdr);
  if (!resPubCard.classList.contains("first-msg-collapsed") ||
      !resPubRootMsg.classList.contains("msg-collapsed") ||
      resPubReplyMsg.classList.contains("msg-collapsed") ||
      resPubReplyHdr.getAttribute("data-collapsed") !== "false" ||
      resPubReplyHdr.querySelector(".comment-collapse-btn").getAttribute("aria-expanded") !== "true") {
    throw new Error("clicking .reply-header should expand reply only while keeping root collapsed");
  }
  dispatchInlineClick(resPubReplyHdr);
  if (!resPubReplyMsg.classList.contains("msg-collapsed")) {
    throw new Error("second click on .reply-header should collapse reply again");
  }

  // D. Clicking inside .thread-actions or another header <button> bubbles to header WITHOUT toggling collapse
  const resDRReplyMsg = doc.getElementById("msg-" + FIX.mResDRReply);
  const resDRReplyEditBtn = resDRReplyMsg.querySelector(".thread-actions button");
  dispatchInlineClick(resDRReplyEditBtn);
  if (resDRReplyMsg.classList.contains("msg-collapsed") ||
      !resDRReplyMsg.classList.contains("editing-msg") ||
      doc.getElementById("msg-edit-" + FIX.mResDRReply).style.display !== "block") {
    throw new Error("clicking Edit inside .thread-actions must open edit form without collapsing");
  }
  const extraHdrBtn = createEl("BUTTON");
  resDRReplyMsg.querySelector(".reply-header .thread-meta").appendChild(extraHdrBtn);
  dispatchInlineClick(extraHdrBtn);
  extraHdrBtn.remove();
  if (resDRReplyMsg.classList.contains("msg-collapsed")) {
    throw new Error("clicking any non-toggle <button> in header must not collapse message");
  }
  win.closeEditMessageForm(FIX.mResDRReply, false);

  // E. openEditMessageForm and restoreSavedDrafts on a collapsed draft auto-expand it
  win.setCommentCollapsed(FIX.mResDRReply, true);
  if (!resDRReplyMsg.classList.contains("msg-collapsed")) {
    throw new Error("expected manual collapse on draft reply before openEditMessageForm");
  }
  win.openEditMessageForm(FIX.mResDRReply, { noFocus: true });
  if (resDRReplyMsg.classList.contains("msg-collapsed") ||
      resDRReplyMsg.querySelector(".reply-header").getAttribute("data-collapsed") !== "false") {
    throw new Error("openEditMessageForm must auto-expand collapsed reply draft");
  }
  win.closeEditMessageForm(FIX.mResDRReply, false);

  win.setCommentCollapsed(FIX.mResDRoot, true);
  const resDRootCard = doc.getElementById("thread-" + FIX.thResDRoot);
  if (!resDRootCard.classList.contains("first-msg-collapsed")) {
    throw new Error("expected manual collapse on draft root before restoreSavedDrafts");
  }
  const rootEditKey = "review_draft:edit:" + FIX.repo + ":" + FIX.change + ":" + FIX.mResDRoot;
  win.formDraftsMap.set(rootEditKey, { draft_key: rootEditKey, message_id: String(FIX.mResDRoot), content: "restored edit" });
  win.restoreSavedDrafts(resDRootCard);
  if (resDRootCard.classList.contains("first-msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResDRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-edit-" + FIX.mResDRoot).style.display !== "block") {
    throw new Error("restoreSavedDrafts opening edit form must auto-expand collapsed draft root");
  }
  win.closeEditMessageForm(FIX.mResDRoot, true);

  // F. submitEditMessage preserves data-user-toggled collapse/expand on existing sibling messages in thread
  win.setCommentCollapsed(FIX.mResDRRoot, false, { userToggled: true });
  win.openEditMessageForm(FIX.mResDRReply, { noFocus: true });
  doc.getElementById("edit-ta-" + FIX.mResDRReply).value = "edited draft reply body";
  customFetch = (url) => {
    if (url === "/api/threads/edit") return Promise.resolve({ ok: true, json: () => Promise.resolve(FIX.editPayload) });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ sig: "s2" }) });
  };
  win.submitEditMessage(FIX.mResDRReply);
  await new Promise(r => setTimeout(r, 20));
  const resDRCardAfterEdit = doc.getElementById("thread-" + FIX.thResDR);
  if (resDRCardAfterEdit.classList.contains("first-msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResDRRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResDRReply).classList.contains("msg-collapsed")) {
    throw new Error("submitEditMessage should preserve user-toggled expanded published root");
  }

  // G1. Un-toggled 1-msg unresolved thread: reply collapses superseded root; deleting reply re-opens promoted root
  const unresSingleReplyForm = doc.getElementById("reply-form-" + FIX.thUnresSingle);
  unresSingleReplyForm.querySelector("textarea").value = "new reply on single unres thread";
  customFetch = (url) => {
    if (url === "/api/threads/reply") return Promise.resolve({ ok: true, json: () => Promise.resolve(FIX.replyUnresSingle) });
    if (url === "/api/threads/delete") return Promise.resolve({ ok: true, json: () => Promise.resolve(FIX.deleteUnresSingle) });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ sig: "s2b" }) });
  };
  win.replyThread({ preventDefault() {}, target: unresSingleReplyForm.querySelector("form") }, FIX.thUnresSingle);
  await new Promise(r => setTimeout(r, 20));
  const singleReplyId = FIX.replyUnresSingle.message.id;
  if (!doc.getElementById("thread-" + FIX.thUnresSingle).classList.contains("first-msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mUnresSingle).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + singleReplyId).classList.contains("msg-collapsed")) {
    throw new Error("replyThread on un-toggled single unresolved thread must collapse prior root and open new reply");
  }
  win.deleteMessage(singleReplyId);
  await new Promise(r => setTimeout(r, 20));
  if (doc.getElementById("thread-" + FIX.thUnresSingle).classList.contains("first-msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mUnresSingle).classList.contains("msg-collapsed")) {
    throw new Error("deleteMessage removing trailing reply must re-open newly-promoted single unresolved root");
  }

  // G2. Manually toggled messages (data-user-toggled) preserve explicit state across replyThread & deleteMessage
  win.setCommentCollapsed(FIX.mResPubRoot, false, { userToggled: true });
  win.setCommentCollapsed(FIX.mResPubReply, true, { userToggled: true });
  const resPubReplyForm = doc.getElementById("reply-form-" + FIX.thResPubMulti);
  resPubReplyForm.querySelector("textarea").value = "new third draft reply";
  customFetch = (url) => {
    if (url === "/api/threads/reply") return Promise.resolve({ ok: true, json: () => Promise.resolve(FIX.replyPayload) });
    if (url === "/api/threads/delete") return Promise.resolve({ ok: true, json: () => Promise.resolve(FIX.deletePayload) });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ sig: "s3" }) });
  };
  win.replyThread({ preventDefault() {}, target: resPubReplyForm.querySelector("form") }, FIX.thResPubMulti);
  await new Promise(r => setTimeout(r, 20));
  const newReplyId = FIX.replyPayload.message.id;
  if (doc.getElementById("thread-" + FIX.thResPubMulti).classList.contains("first-msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResPubRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResPubRoot).getAttribute("data-user-toggled") !== "true" ||
      !doc.getElementById("msg-" + FIX.mResPubReply).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + newReplyId).classList.contains("msg-collapsed")) {
    throw new Error("replyThread must preserve user-toggled root expanded, old reply collapsed, and start new draft reply expanded");
  }

  win.deleteMessage(newReplyId);
  await new Promise(r => setTimeout(r, 20));
  if (doc.getElementById("thread-" + FIX.thResPubMulti).classList.contains("first-msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResPubRoot).classList.contains("msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mResPubReply).classList.contains("msg-collapsed")) {
    throw new Error("deleteMessage must preserve user-toggled root expanded and reply collapsed");
  }

  // H. resolveThread resets data-user-toggled: resolving collapses all; reopening opens ONLY latest message
  win.setCommentCollapsed(FIX.mUnresRoot, false, { userToggled: true });
  customFetch = (url, opts) => {
    if (url === "/api/threads/resolve") {
      const reqBody = JSON.parse(opts.body);
      return Promise.resolve({ ok: true, json: () => Promise.resolve(reqBody.resolved ? FIX.resTrue : FIX.resFalse) });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ sig: "s4" }) });
  };
  win.resolveThread(FIX.thUnresMulti, true);
  await new Promise(r => setTimeout(r, 20));
  if (!doc.getElementById("thread-" + FIX.thUnresMulti).classList.contains("first-msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mUnresRoot).classList.contains("msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mUnresReply).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mUnresRoot).hasAttribute("data-user-toggled")) {
    throw new Error("resolveThread(true) should reset userToggled and collapse all published messages");
  }
  win.resolveThread(FIX.thUnresMulti, false);
  await new Promise(r => setTimeout(r, 20));
  if (!doc.getElementById("thread-" + FIX.thUnresMulti).classList.contains("first-msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mUnresRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mUnresReply).classList.contains("msg-collapsed")) {
    throw new Error("resolveThread(false) should keep earlier message collapsed and open only latest message");
  }

  // I. e expands all, E collapses all (skipping .new-comment-card), ignored inside inputs
  let ePrevented = false;
  keydown({ key: "E", target: body, preventDefault() { ePrevented = true; } });
  if (!ePrevented) throw new Error("E shortcut should call preventDefault");
  for (const id of FIX.allMsgIDs) {
    const mb = doc.getElementById("msg-" + id);
    if (!mb.classList.contains("msg-collapsed")) {
      throw new Error("E shortcut should collapse msg-" + id);
    }
  }
  if (newCard.classList.contains("first-msg-collapsed")) {
    throw new Error("E shortcut must skip .new-comment-card");
  }

  const dummyTa = createEl("TEXTAREA");
  keydown({ key: "e", target: dummyTa, preventDefault() { throw new Error("must not handle e inside textarea"); } });
  if (!doc.getElementById("msg-" + FIX.mUnresRoot).classList.contains("msg-collapsed")) {
    throw new Error("e inside textarea should not expand comments");
  }

  // J. N / P keyboard thread navigation does NOT expand collapsed comments
  win.scrollY = 0;
  win.threadNavState.el = null;
  keydown({ key: "N", target: body, preventDefault() {} });
  if (!win.threadNavState.el) {
    throw new Error("expected N navigation to select a thread card");
  }
  if (!doc.getElementById("msg-" + FIX.mUnresRoot).classList.contains("msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mResPubRoot).classList.contains("msg-collapsed")) {
    throw new Error("N navigation must leave collapsed comments collapsed");
  }
  keydown({ key: "P", target: body, preventDefault() {} });
  for (const id of [FIX.mUnresRoot, FIX.mResPubRoot]) {
    if (!doc.getElementById("msg-" + id).classList.contains("msg-collapsed")) {
      throw new Error("P navigation must leave collapsed comments collapsed");
    }
  }

  // K. e expands all comments
  let expandPrevented = false;
  keydown({ key: "e", target: body, preventDefault() { expandPrevented = true; } });
  if (!expandPrevented) throw new Error("e shortcut should call preventDefault");
  for (const id of FIX.allMsgIDs) {
    const mb = doc.getElementById("msg-" + id);
    if (mb.classList.contains("msg-collapsed")) {
      throw new Error("e shortcut should expand msg-" + id);
    }
  }

  // L. jumpToUnresolvedThread (via revealThreadForJump):
  // - Jumping to thread without msgId when at least one message is already open preserves collapsed earlier messages
  // - Jumping with msgId expands ONLY that specific message
  // - Jumping without msgId when ALL messages are collapsed expands all messages in target thread only
  win.resolveThread(FIX.thUnresMulti, false);
  await new Promise(r => setTimeout(r, 20));
  if (!doc.getElementById("msg-" + FIX.mUnresRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mUnresReply).classList.contains("msg-collapsed")) {
    throw new Error("setup failed: expected thUnresMulti root collapsed and reply open");
  }
  await win.jumpToUnresolvedThread(FIX.thUnresMulti, "hello.go");
  if (!doc.getElementById("msg-" + FIX.mUnresRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mUnresReply).classList.contains("msg-collapsed")) {
    throw new Error("jumpToUnresolvedThread without msgId must keep earlier messages collapsed when latest reply is already open");
  }
  await win.jumpToUnresolvedThread(FIX.thUnresMulti, "hello.go", FIX.mUnresRoot);
  if (doc.getElementById("msg-" + FIX.mUnresRoot).classList.contains("msg-collapsed")) {
    throw new Error("jumpToUnresolvedThread with explicit root msgId must expand targeted root message");
  }

  keydown({ key: "E", target: body, preventDefault() {} });
  scrollCalls.length = 0;
  await win.jumpToUnresolvedThread(FIX.thResPubMulti, "hello.go", FIX.mResPubReply);
  if (!doc.getElementById("thread-" + FIX.thResPubMulti).classList.contains("first-msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mResPubRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResPubReply).classList.contains("msg-collapsed") ||
      scrollCalls.length === 0) {
    throw new Error("jumpToUnresolvedThread with msgId must expand ONLY targeted message and scroll");
  }
  win.setCommentCollapsed(FIX.mResPubReply, true);
  await win.jumpToUnresolvedThread(FIX.thResPubMulti, "hello.go");
  if (doc.getElementById("thread-" + FIX.thResPubMulti).classList.contains("first-msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResPubRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResPubReply).classList.contains("msg-collapsed")) {
    throw new Error("jumpToUnresolvedThread without msgId must expand all messages when all are collapsed");
  }
  if (!doc.getElementById("msg-" + FIX.mUnresRoot).classList.contains("msg-collapsed")) {
    throw new Error("jumpToUnresolvedThread must not expand sibling threads");
  }

  // M. scrollToHashLine (#thread-<id>-msg-<msgId> and #thread-<id>)
  keydown({ key: "E", target: body, preventDefault() {} });
  sec.classList.add("collapsed");
  fileBody.style.display = "none";
  win.location.hash = "#thread-" + FIX.thResDR + "-msg-" + FIX.mResDRReply;
  await win.scrollToHashLine();
  if (sec.classList.contains("collapsed") ||
      !doc.getElementById("thread-" + FIX.thResDR).classList.contains("first-msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mResDRRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResDRReply).classList.contains("msg-collapsed")) {
    throw new Error("scrollToHashLine with -msg-<id> must expand file section and ONLY targeted message");
  }
  win.setCommentCollapsed(FIX.mResDRReply, true);
  win.location.hash = "#thread-" + FIX.thResDR;
  await win.scrollToHashLine();
  if (doc.getElementById("thread-" + FIX.thResDR).classList.contains("first-msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResDRRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mResDRReply).classList.contains("msg-collapsed")) {
    throw new Error("scrollToHashLine #thread-<id> must expand all messages when all are collapsed");
  }

  // N. Collapsing blurs hidden activeElement (textareas inside hidden bodies), while keeping .comment-collapse-btn focus
  doc.querySelectorAll(".thread-card.jump-target-flash").forEach(c => c.classList.remove("jump-target-flash"));
  win.closeAllSymbolPopups();
  win.openEditMessageForm(FIX.mResDRReply);
  const editReplyTa = doc.getElementById("edit-ta-" + FIX.mResDRReply);
  if (doc.activeElement !== editReplyTa) {
    throw new Error("expected openEditMessageForm to focus reply textarea");
  }
  win.setCommentCollapsed(FIX.mResDRReply, true);
  if (doc.activeElement !== null) {
    throw new Error("collapsing reply with focused edit textarea must blur activeElement");
  }
  if (doc.getElementById("thread-" + FIX.thResDR).classList.contains("comment-z-active")) {
    throw new Error("collapsing last visible edit form must clear comment-z-active");
  }
  win.setCommentCollapsed(FIX.mResDRReply, false);
  if (!doc.getElementById("thread-" + FIX.thResDR).classList.contains("comment-z-active")) {
    throw new Error("re-expanding editing message must restore comment-z-active");
  }
  const replyCollapseBtn = doc.getElementById("msg-" + FIX.mResDRReply).querySelector(".comment-collapse-btn");
  replyCollapseBtn.focus();
  win.setCommentCollapsed(FIX.mResDRReply, true);
  if (doc.activeElement !== replyCollapseBtn) {
    throw new Error("collapsing reply must keep focus on .comment-collapse-btn inside .reply-header");
  }
  win.closeEditMessageForm(FIX.mResDRReply, false);

  win.openEditMessageForm(FIX.mResDRoot);
  if (doc.activeElement !== doc.getElementById("edit-ta-" + FIX.mResDRoot)) {
    throw new Error("expected openEditMessageForm to focus root textarea");
  }
  win.setCommentCollapsed(FIX.mResDRoot, true);
  if (doc.activeElement !== null) {
    throw new Error("collapsing root comment with focused edit textarea must blur activeElement");
  }
  win.closeEditMessageForm(FIX.mResDRoot, false);

  // O. triggerEscapeAction skips hidden .msg-edit-form inside collapsed messages (isEditFormVisible)
  win.openEditMessageForm(FIX.mResDRReply, { noFocus: true });
  doc.getElementById("msg-edit-" + FIX.mResDRReply)._zOrder = 250;
  win.setCommentCollapsed(FIX.mResDRReply, true);
  doc.activeElement = null;
  if (win.triggerEscapeAction(body) !== false) {
    throw new Error("Escape must not close a collapsed/hidden .msg-edit-form");
  }
  if (doc.getElementById("msg-edit-" + FIX.mResDRReply).style.display !== "block") {
    throw new Error("hidden collapsed .msg-edit-form must remain untouched by Escape");
  }
  const visibleReplyForm = doc.getElementById("reply-form-" + FIX.thUnresMulti);
  visibleReplyForm.style.display = "block";
  visibleReplyForm._zOrder = 200;
  doc.getElementById("thread-" + FIX.thUnresMulti).classList.add("reply-open");
  if (win.triggerEscapeAction(body) !== true) {
    throw new Error("Escape should close the lower-z visible reply form while skipping hidden collapsed edit form");
  }
  if (visibleReplyForm.style.display !== "none" ||
      doc.getElementById("msg-edit-" + FIX.mResDRReply).style.display !== "block") {
    throw new Error("Escape should close visible reply form and keep hidden edit form untouched");
  }
  win.setCommentCollapsed(FIX.mResDRReply, false);
  if (win.triggerEscapeAction(body) !== true ||
      doc.getElementById("msg-edit-" + FIX.mResDRReply).style.display !== "none") {
    throw new Error("Escape should close edit form again once message is re-expanded");
  }

  // P. publishDrafts collapses newly published drafts on resolved threads (even if toggled while drafting),
  // while preserving explicit user toggles (both expanded and collapsed) on already-published messages.
  win.setCommentCollapsed(FIX.mResDRReply, false, { userToggled: true });
  win.setCommentCollapsed(FIX.mUnresRoot, false, { userToggled: true });
  customFetch = (url) => {
    if (url === "/api/publish") return Promise.resolve({ ok: true, json: () => Promise.resolve(FIX.publishPayload) });
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ sig: "s5" }) });
  };
  win.publishDrafts(FIX.repo, FIX.change);
  await new Promise(r => setTimeout(r, 20));
  if (!doc.getElementById("thread-" + FIX.thResDR).classList.contains("first-msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mResDRRoot).classList.contains("msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mResDRReply).classList.contains("msg-collapsed") ||
      !doc.getElementById("thread-" + FIX.thResDRoot).classList.contains("first-msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mResDRoot).classList.contains("msg-collapsed") ||
      doc.getElementById("thread-" + FIX.thUnresMulti).classList.contains("first-msg-collapsed") ||
      doc.getElementById("msg-" + FIX.mUnresRoot).classList.contains("msg-collapsed") ||
      !doc.getElementById("msg-" + FIX.mUnresReply).classList.contains("msg-collapsed")) {
    throw new Error("publishDrafts should collapse newly published resolved drafts while preserving user toggles on already-published messages");
  }

  console.log("OK");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`

	cmd := exec.Command(nodeBin, "-e", nodeScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TestCommentCollapseExpand node test failed: %v\n%s", err, string(out))
	} else if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected OK output, got: %s", string(out))
	}

	cssBytes, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	cssStr := string(cssBytes)
	for _, want := range []string{
		".thread-card.thread-unresolved .reply-header",
		".comment-collapsed-preview",
		".comment-collapse-btn",
		".comment-collapse-btn:focus-visible",
		".comment-collapse-icon",
		".thread-header[data-collapsed=\"true\"]:hover .comment-collapse-btn",
		".thread-card.first-msg-collapsed > .thread-header",
		".thread-card.first-msg-collapsed > .thread-anchor-quote",
		".thread-card.first-msg-collapsed > .message-body:not(.message-reply)",
		".message-body.message-reply.msg-collapsed > .msg-content",
		".thread-card > .message-body.msg-collapsed + .thread-footer",
		".thread-card.thread-unresolved > .message-body.msg-collapsed + .thread-footer",
		".thread-card:not(.reply-open) > .message-body.editing-msg.msg-collapsed + .thread-footer",
	} {
		if !strings.Contains(cssStr, want) {
			t.Errorf("expected style.css to contain %q", want)
		}
	}
	if strings.Contains(cssStr, "transition: transform 0.12s ease;") {
		t.Errorf("expected style.css to omit transform transition on .comment-collapse-icon")
	}
}
