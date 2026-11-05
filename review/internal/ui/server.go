package ui

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/engine"
	"nicholas.sh/cmd/review/internal/gosym"
	"nicholas.sh/cmd/review/internal/vcs"
)

//go:embed static/*
var staticFS embed.FS

//go:embed tmpl/*
var tmplFS embed.FS

// Server represents the review HTTP UI server.
type Server struct {
	d                  *db.DB
	cache              *vcs.Cache
	ownsCache          bool
	broker             *SSEBroker
	sym                *gosym.Analyzer
	moveCache          *moveCache
	templates          map[string]*template.Template
	staticVersion      string
	httpServer         *http.Server
	listenAddr         string
	copyHost           string
	activeForeground   atomic.Int32
	lastForegroundNano atomic.Int64
}

// NewServer initializes the UI HTTP server.
func NewServer(d *db.DB, cache *vcs.Cache, listenAddr string) (*Server, error) {
	// Compute static asset version hash
	h := sha256.New()
	_ = fs.WalkDir(staticFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := staticFS.ReadFile(path)
			h.Write(data)
		}
		return nil
	})
	staticVer := hex.EncodeToString(h.Sum(nil))[:12]

	tmplFuncs := template.FuncMap{
		"TrimSlash": func(p string) string {
			return strings.TrimPrefix(p, "/")
		},
		"ShortRev": func(rev string) string {
			if len(rev) > 12 {
				return rev[:12]
			}
			return rev
		},
		"Markdown":            RenderMarkdown,
		"CollapsePreview":     collapseCommentPreview,
		"LastIndex":           func(msgs []*db.Message) int { return len(msgs) - 1 },
		"FormatCommitMessage": FormatCommitMessage,
		"ReverseSnapshots": func(snaps []*db.Snapshot) []*db.Snapshot {
			out := make([]*db.Snapshot, len(snaps))
			for i, s := range snaps {
				out[len(snaps)-1-i] = s
			}
			return out
		},
	}

	baseTmpl, err := template.New("layout.html").Funcs(tmplFuncs).ParseFS(tmplFS, "tmpl/layout.html", "tmpl/diff_block.html")
	if err != nil {
		return nil, fmt.Errorf("parsing base template: %w", err)
	}

	pageNames := []string{"dashboard.html", "repo.html", "change.html", "diff.html"}
	templates := make(map[string]*template.Template)
	for _, page := range pageNames {
		tClone, err := baseTmpl.Clone()
		if err != nil {
			return nil, err
		}
		tPage, err := tClone.ParseFS(tmplFS, "tmpl/"+page)
		if err != nil {
			return nil, fmt.Errorf("parsing template %s: %w", page, err)
		}
		templates[page] = tPage
	}

	ownsCache := false
	if cache == nil {
		cache = vcs.NewCache()
		ownsCache = true
	}

	broker := NewSSEBroker(d, cache)

	s := &Server{
		d:             d,
		cache:         cache,
		ownsCache:     ownsCache,
		broker:        broker,
		sym:           gosym.NewAnalyzer(),
		moveCache:     newMoveCache(),
		templates:     templates,
		staticVersion: staticVer,
		listenAddr:    listenAddr,
		copyHost:      extractCopyHost(listenAddr),
	}
	s.lastForegroundNano.Store(time.Now().UnixNano())
	if s.sym != nil {
		s.sym.SetVCSCache(cache)
		s.sym.SetIdleCheck(func() bool {
			return s.activeForeground.Load() == 0 && time.Since(time.Unix(0, s.lastForegroundNano.Load())) >= 250*time.Millisecond
		})
	}

	mux := http.NewServeMux()

	// Static assets with immutable caching
	staticSub, _ := fs.Sub(staticFS, "static")
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /static/{file...}", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})))

	// Health check
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// SSE Events
	mux.Handle("GET /api/events", s.broker)

	// HTML Pages
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /r/{repo}", s.handleRepo)
	mux.HandleFunc("GET /r/{repo}/c/{key}", s.handleChange)
	mux.HandleFunc("GET /r/{repo}/c/{key}/ext", s.handleExternalFile)
	mux.HandleFunc("GET /r/{repo}/c/{key}/f/{file...}", s.handleDiff)

	// API Endpoints
	mux.HandleFunc("GET /api/state", s.handleAPIState)
	mux.HandleFunc("POST /api/notify", s.handleAPINotify)
	mux.HandleFunc("GET /api/diff/expand", s.handleAPIDiffExpand)
	mux.HandleFunc("GET /api/diff/file", s.handleAPIDiffFile)
	mux.HandleFunc("GET /api/diff/move-preview", s.handleAPIMovePreview)
	mux.HandleFunc("POST /api/snapshot", s.handleAPISnapshot)
	mux.HandleFunc("POST /api/publish", s.handleAPIPublish)
	mux.HandleFunc("POST /api/threads/add", s.handleAPIThreadsAdd)
	mux.HandleFunc("POST /api/threads/reply", s.handleAPIThreadsReply)
	mux.HandleFunc("POST /api/threads/edit", s.handleAPIThreadsEdit)
	mux.HandleFunc("POST /api/threads/delete", s.handleAPIThreadsDelete)
	mux.HandleFunc("POST /api/threads/resolve", s.handleAPIThreadsResolve)
	mux.HandleFunc("GET /api/threads/list", s.handleAPIThreadsList)
	mux.HandleFunc("GET /api/form-drafts", s.handleAPIFormDrafts)
	mux.HandleFunc("POST /api/form-drafts", s.handleAPIFormDrafts)
	mux.HandleFunc("POST /api/files/reviewed", s.handleAPIFilesReviewed)
	mux.HandleFunc("POST /api/changes/reviewed", s.handleAPIChangesReviewed)
	mux.HandleFunc("POST /api/repos/forget", s.handleAPIReposForget)
	mux.HandleFunc("POST /api/prefs", s.handleAPIPrefs)
	mux.HandleFunc("GET /api/symbol", s.handleAPISymbol)
	mux.HandleFunc("GET /api/symbol/refs", s.handleAPISymbolRefs)
	mux.HandleFunc("POST /api/symbol/refs/batch", s.handleAPISymbolRefsBatch)
	mux.HandleFunc("GET /api/symbol/impls", s.handleAPISymbolImpls)

	// Fallback for directory paths pasted from shell or trailing slashes
	mux.HandleFunc("GET /{path...}", s.handleFallback)

	s.httpServer = &http.Server{
		Addr:    listenAddr,
		Handler: s.trackForeground(s.protectCrossOrigin(compressResponse(mux))),
	}

	return s, nil
}

func (s *Server) trackForeground(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		s.activeForeground.Add(1)
		defer func() {
			s.activeForeground.Add(-1)
			s.lastForegroundNano.Store(time.Now().UnixNano())
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleFallback(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/r/") && strings.HasSuffix(r.URL.Path, "/") {
		target := strings.TrimRight(r.URL.Path, "/")
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}

	trimmed := strings.Trim(r.URL.Path, "/")
	if trimmed != "" {
		if rec, _ := s.d.RepoBySlug(trimmed); rec != nil {
			target := "/r/" + rec.Slug
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}

		fullPath := "/" + trimmed
		repos, _ := s.d.Repos()
		for _, rec := range repos {
			if rec.RootPath == fullPath || strings.HasPrefix(fullPath, rec.RootPath+"/") {
				target := "/r/" + rec.Slug
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusFound)
				return
			}
		}
	}

	http.NotFound(w, r)
}

// resolveChange resolves a change key, commit hash/prefix, or bookmark into a *vcs.Change.
func (s *Server) resolveChange(repo vcs.Repo, key string) (*vcs.Change, error) {
	if repo == nil || key == "" {
		return nil, fmt.Errorf("empty repo or change key")
	}
	changes, err := repo.Changes()
	if err == nil {
		if ch := vcs.ResolveChange(changes, key); ch != nil {
			return ch, nil
		}
	}
	return repo.Commit(key)
}

var (
	reCommitURL = regexp.MustCompile(`https?://[^\s<>"']+`)
)

// FormatCommitMessage formats raw commit messages, safely escaping HTML and auto-linking
// explicit http(s) URLs.
func FormatCommitMessage(msg string) template.HTML {
	msg = strings.ReplaceAll(msg, "\x00", "")
	escaped := html.EscapeString(msg)

	escaped = reCommitURL.ReplaceAllStringFunc(escaped, func(u string) string {
		raw := html.UnescapeString(u)
		quoteSuffix := ""
		if idx := strings.IndexAny(raw, "<>\"'"); idx != -1 {
			quoteSuffix = raw[idx:]
			raw = raw[:idx]
		}
		cleanURL, trailing := trimURLTrailingPunct(raw)
		if schemeEnd := strings.Index(cleanURL, "://"); schemeEnd != -1 && len(cleanURL) > schemeEnd+3 && isSafeURL(cleanURL) {
			safeURL := html.EscapeString(cleanURL)
			return fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener noreferrer">%s</a>%s`, safeURL, safeURL, html.EscapeString(trailing+quoteSuffix))
		}
		return u
	})

	return template.HTML(escaped)
}

func (s *Server) protectCrossOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify cross-origin on mutating POST requests
		if r.Method == http.MethodPost {
			secFetchSite := r.Header.Get("Sec-Fetch-Site")
			if secFetchSite != "" && secFetchSite != "same-origin" && secFetchSite != "same-site" && secFetchSite != "none" {
				http.Error(w, "Cross-origin request blocked", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	skipGzip    bool
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if code == http.StatusNotModified || code == http.StatusNoContent {
		w.skipGzip = true
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Add("Vary", "Accept-Encoding")
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		w.WriteHeader(http.StatusOK)
	}
	if w.skipGzip {
		return w.ResponseWriter.Write(b)
	}
	return w.gz.Write(b)
}

func (w *gzipResponseWriter) Flush() {
	if !w.skipGzip && w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func compressResponse(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events" ||
			r.Method == http.MethodHead ||
			!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		gz := gzip.NewWriter(w)
		gzw := &gzipResponseWriter{
			ResponseWriter: w,
			gz:             gz,
		}
		defer func() {
			if !gzw.skipGzip {
				_ = gz.Close()
			}
		}()
		next.ServeHTTP(gzw, r)
	})
}

// extractCopyHost extracts a valid host:port string suitable for sharing copy links,
// or empty string if no specific host was configured (e.g. empty or wildcard).
func extractCopyHost(listenAddr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		return ""
	}
	return net.JoinHostPort(host, port)
}

// renderTemplate executes the named template with the provided data.
func (s *Server) renderTemplate(w http.ResponseWriter, name string, data any) {
	t, ok := s.templates[name]
	if !ok {
		http.Error(w, fmt.Sprintf("template %s not found", name), http.StatusInternalServerError)
		return
	}
	if m, ok := data.(map[string]any); ok {
		if _, exists := m["CopyHost"]; !exists {
			m["CopyHost"] = s.copyHost
		}
		if _, exists := m["SyntaxPref"]; !exists {
			m["SyntaxPref"] = s.d.Pref("syntax", "on")
		}
		if _, exists := m["ThemePref"]; !exists {
			m["ThemePref"] = s.d.Pref("theme", "")
		}
		if _, exists := m["ScopeSidePref"]; !exists {
			m["ScopeSidePref"] = s.d.Pref("scope_side", "R")
		}
		if s.broker != nil {
			sig := s.broker.StateSignature()
			m["StateSig"] = sig
			s.broker.InitLastSignature(sig)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = t.Execute(w, data)
}

// Serve starts listening on listener.
func (s *Server) Serve(l net.Listener) error {
	if s.sym != nil {
		s.sym.StartBackgroundWarmup()
	}
	return s.httpServer.Serve(l)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.broker.Close()
	if s.sym != nil {
		s.sym.Close()
	}
	err := s.httpServer.Shutdown(ctx)
	if s.ownsCache && s.cache != nil {
		s.cache.Close()
	}
	return err
}

func (s *Server) renderThreadCard(repo vcs.Repo, th *db.Thread, leftRev, rightRev string) (string, error) {
	return s.renderThreadCardInChange(repo, nil, nil, th, leftRev, rightRev)
}

func (s *Server) renderThreadCardInChange(repo vcs.Repo, c *vcs.Change, snaps []*db.Snapshot, th *db.Thread, leftRev, rightRev string) (string, error) {
	var tv *engine.ThreadView
	if repo != nil && (leftRev != "" || rightRev != "") {
		var tvs []*engine.ThreadView
		if c != nil || snaps != nil {
			isLiveTarget := (c == nil || rightRev == "" || rightRev == c.Rev)
			tvs = engine.ReanchorThreadsForView(repo, c, snaps, rightRev, leftRev, isLiveTarget, []*db.Thread{th}, nil)
		} else {
			tvs = engine.ReanchorThreads(repo, rightRev, leftRev, []*db.Thread{th})
		}
		if len(tvs) > 0 {
			tv = tvs[0]
		}
	}
	if tv == nil {
		tv = &engine.ThreadView{
			Thread:       th,
			CurrentStart: th.StartLine,
			CurrentEnd:   th.EndLine,
			Stale:        th.Stale,
			AnchorText:   th.AnchorLines,
		}
	}
	return s.renderThreadView(tv)
}

func (s *Server) renderThreadView(tv *engine.ThreadView) (string, error) {
	if tv == nil {
		return "", nil
	}
	t, ok := s.templates["diff.html"]
	if !ok {
		for _, tmpl := range s.templates {
			t = tmpl
			break
		}
	}
	if t == nil {
		return "", fmt.Errorf("no templates available to render thread card")
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "thread_card", tv); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// collapseCommentPreview normalizes whitespace into a single line and truncates long previews.
func collapseCommentPreview(s string) string {
	// Zero-allocation fast path for short single-spaced printable ASCII comments.
	if len(s) <= 240 {
		fast := true
		prevSpace := false
		for i := 0; i < len(s); i++ {
			b := s[i]
			if b < 0x20 || b > 0x7e {
				fast = false
				break
			}
			if b == ' ' {
				if i == 0 || prevSpace {
					fast = false
					break
				}
				prevSpace = true
			} else {
				prevSpace = false
			}
		}
		if fast && !prevSpace {
			return s
		}
	}

	var b strings.Builder
	b.Grow(min(len(s), 256))
	pendingSpace := false
	emitted := 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			if emitted > 0 {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			if emitted >= 240 {
				b.WriteString("…")
				return b.String()
			}
			b.WriteByte(' ')
			emitted++
			pendingSpace = false
		}
		if emitted >= 240 {
			b.WriteString("…")
			return b.String()
		}
		b.WriteRune(r)
		emitted++
	}
	return b.String()
}

func (s *Server) getCommentCounts(repoID int64, changeKey, filePath string) (fileCounts engine.CommentCounts, changeCounts engine.CommentCounts, err error) {
	cUnres, cRes, cDraft, err := s.d.CommentCountsForChange(repoID, changeKey, "")
	if err != nil {
		return engine.CommentCounts{}, engine.CommentCounts{}, err
	}
	changeCounts = engine.CommentCounts{
		Unresolved: cUnres,
		Resolved:   cRes,
		Draft:      cDraft,
	}
	if filePath != "" {
		fUnres, fRes, fDraft, err := s.d.CommentCountsForChange(repoID, changeKey, filePath)
		if err != nil {
			return engine.CommentCounts{}, engine.CommentCounts{}, err
		}
		fileCounts = engine.CommentCounts{
			Unresolved: fUnres,
			Resolved:   fRes,
			Draft:      fDraft,
		}
	}
	return fileCounts, changeCounts, nil
}

func (s *Server) resolveViewContext(repo vcs.Repo, c *vcs.Change, repoID int64, changeKey, leftRev, rightRev string) (*vcs.Change, []*db.Snapshot, bool) {
	if repo == nil || (leftRev == "" && rightRev == "") {
		return c, nil, false
	}
	if c == nil {
		resolved, err := s.resolveChange(repo, changeKey)
		if err != nil || resolved == nil {
			return nil, nil, false
		}
		c = resolved
	}
	snaps, err := s.d.Snapshots(repoID, c.Key)
	if err != nil {
		return c, nil, false
	}
	return c, snaps, true
}

func (s *Server) getCommentCountsForChangeView(repo vcs.Repo, c *vcs.Change, snaps []*db.Snapshot, snapsLoaded bool, repoID int64, changeKey, filePath, leftRev, rightRev string) (fileCounts engine.CommentCounts, changeCounts engine.CommentCounts, err error) {
	if c != nil && c.Key != "" {
		changeKey = c.Key
	}
	if repo == nil || (leftRev == "" && rightRev == "") {
		return s.getCommentCounts(repoID, changeKey, filePath)
	}
	if c == nil {
		c, err = s.resolveChange(repo, changeKey)
		if err != nil || c == nil {
			return s.getCommentCounts(repoID, changeKey, filePath)
		}
		if c.Key != "" {
			changeKey = c.Key
		}
	}
	if !snapsLoaded {
		snaps, err = s.d.Snapshots(repoID, changeKey)
		if err != nil {
			return engine.CommentCounts{}, engine.CommentCounts{}, err
		}
	}
	threads, err := s.d.ThreadsForChange(repoID, changeKey)
	if err != nil {
		return engine.CommentCounts{}, engine.CommentCounts{}, err
	}
	changeCounts = engine.ComputeCommentCounts(threads)
	if filePath == "" {
		return engine.CommentCounts{}, changeCounts, nil
	}

	baseRev := leftRev
	if baseRev == "" {
		baseRev = c.Parent
	}
	targetRev := rightRev
	if targetRev == "" {
		targetRev = c.Rev
	}
	scope := engine.NewViewScope(snaps, baseRev, targetRev, targetRev == c.Rev)

	normFile := strings.TrimPrefix(filePath, "/")
	if vcs.IsCommitMsgPath(filePath) {
		normFile = strings.TrimPrefix(vcs.CommitMsgPath, "/")
	}
	var visibleFileThreads []*db.Thread
	for _, th := range threads {
		normTh := strings.TrimPrefix(th.Path, "/")
		if vcs.IsCommitMsgPath(th.Path) {
			normTh = strings.TrimPrefix(vcs.CommitMsgPath, "/")
		}
		if normTh != normFile {
			continue
		}
		if _, ok := scope.ThreadSide(th); ok {
			visibleFileThreads = append(visibleFileThreads, th)
		}
	}
	return engine.ComputeCommentCounts(visibleFileThreads), changeCounts, nil
}
