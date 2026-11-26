package ui

import (
	"context"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/engine"
	"nicholas.sh/cmd/review/internal/gosym"
	"nicholas.sh/cmd/review/internal/vcs"
)

type apiSearchCodeLine struct {
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	LineHTML string `json:"line_html"`
}

type apiSearchCodeGroup struct {
	Path     string              `json:"path"`
	InChange bool                `json:"in_change,omitempty"`
	Lines    []apiSearchCodeLine `json:"lines"`
}

type apiSearchComment struct {
	ThreadID      int64  `json:"thread_id"`
	MessageID     int64  `json:"message_id"`
	ChangeKey     string `json:"change_key"`
	Path          string `json:"path"`
	Side          string `json:"side"`
	Line          int    `json:"line"`
	EndLine       int    `json:"end_line"`
	SnapshotNum   int    `json:"snapshot_num"`
	Author        string `json:"author"`
	Resolved      bool   `json:"resolved"`
	Draft         bool   `json:"draft,omitempty"`
	Stale         bool   `json:"stale,omitempty"`
	InCurrentView bool   `json:"in_current_view"`
	SnippetHTML   string `json:"snippet_html"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	RelTime       string `json:"rel_time,omitempty"`
	ExactTime     string `json:"exact_time,omitempty"`
}

type apiSearchResponse struct {
	Query     string                     `json:"query"`
	Files     []engine.SearchFileMatch   `json:"files,omitempty"`
	Symbols   []engine.SearchSymbolMatch `json:"symbols,omitempty"`
	Comments  []apiSearchComment         `json:"comments,omitempty"`
	Code      []apiSearchCodeGroup       `json:"code,omitempty"`
	TotalCode int                        `json:"total_code"`
	Truncated bool                       `json:"truncated,omitempty"`
}

func (s *Server) resolveSearchRevs(repo vcs.Repo, repoID int64, changeKey, baseSel, targetSel, side string) (c *vcs.Change, snaps []*db.Snapshot, baseRev, targetRev, searchRev string, isLiveTarget bool) {
	if changeKey != "" {
		resolved, err := s.resolveChange(repo, changeKey)
		if err == nil && resolved != nil {
			c = resolved
			snaps, _ = s.d.Snapshots(repoID, c.Key)
			snaps, _ = engine.SyncLiveRebasedSnapshot(s.d, repo, repoID, c, snaps)
			baseRev = c.Parent
			targetRev = c.Rev
			isLiveTarget = true

			if targetSel != "" && targetSel != "live" {
				isLiveTarget = false
				if strings.HasPrefix(targetSel, "snap:") {
					if num, err := strconv.Atoi(strings.TrimPrefix(targetSel, "snap:")); err == nil && num > 0 {
						for _, sn := range snaps {
							if sn.Num == num {
								targetRev = sn.Rev
								break
							}
						}
					}
				} else {
					targetRev = targetSel
					isLiveTarget = (targetRev == c.Rev)
				}
			}

			if strings.HasPrefix(baseSel, "snap:") {
				if num, err := strconv.Atoi(strings.TrimPrefix(baseSel, "snap:")); err == nil && num > 0 {
					for _, sn := range snaps {
						if sn.Num == num {
							baseRev = sn.Rev
							break
						}
					}
				}
			} else if baseSel != "" && baseSel != "parent" {
				baseRev = baseSel
			}

			searchRev = targetRev
			if side == "L" && baseRev != "" {
				searchRev = baseRev
			}
			return
		}
	}

	// Repo-level fallback (e.g. searching from /r/<slug> without a specific change).
	if changes, err := repo.Changes(); err == nil {
		for _, ch := range changes {
			if ch != nil && !ch.Working && ch.Rev != "" && ch.Rev != "working" {
				searchRev = ch.Rev
				break
			}
		}
	}
	if searchRev == "" {
		if head, err := repo.Commit("HEAD"); err == nil && head != nil {
			searchRev = head.Rev
		}
	}
	targetRev = searchRev
	return
}

func (s *Server) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	repoSlug := q.Get("repo")
	changeKey := q.Get("change")
	baseSel := q.Get("base")
	targetSel := q.Get("target")
	side := q.Get("side")
	query := q.Get("q")

	if repoSlug == "" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}
	rec, err := s.d.RepoBySlug(repoSlug)
	if err != nil || rec == nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}

	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	c, snaps, baseRev, targetRev, searchRev, isLiveTarget := s.resolveSearchRevs(repo, rec.ID, changeKey, baseSel, targetSel, side)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	res, err := engine.CodeSearch(ctx, engine.CodeSearchOptions{
		DB:           s.d,
		Repo:         repo,
		RepoID:       rec.ID,
		Change:       c,
		BaseRev:      baseRev,
		TargetRev:    targetRev,
		SearchRev:    searchRev,
		Snapshots:    snaps,
		IsLiveTarget: isLiveTarget,
		Query:        query,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	out := apiSearchResponse{
		Query:     res.Query,
		Files:     res.Files,
		Symbols:   res.Symbols,
		TotalCode: res.TotalCode,
		Truncated: res.Truncated,
	}

	if len(res.Comments) > 0 {
		out.Comments = make([]apiSearchComment, len(res.Comments))
		for i, cm := range res.Comments {
			out.Comments[i] = apiSearchComment{
				ThreadID:      cm.ThreadID,
				MessageID:     cm.MessageID,
				ChangeKey:     cm.ChangeKey,
				Path:          cm.Path,
				Side:          cm.Side,
				Line:          cm.Line,
				EndLine:       cm.EndLine,
				SnapshotNum:   cm.SnapshotNum,
				Author:        cm.Author,
				Resolved:      cm.Resolved,
				Draft:         cm.Draft,
				Stale:         cm.Stale,
				InCurrentView: cm.InCurrentView,
				SnippetHTML:   renderHighlightedText(cm.Snippet, cm.Spans),
				UpdatedAt:     formatISOTime(cm.ActivityTime),
				RelTime:       formatRelativeTime(cm.ActivityTime, now),
				ExactTime:     formatExactTime(cm.ActivityTime),
			}
		}
	}

	if len(res.Code) > 0 {
		out.Code = make([]apiSearchCodeGroup, len(res.Code))
		for i, grp := range res.Code {
			lang := diff.DetectLang(grp.Path)
			lines := make([]apiSearchCodeLine, len(grp.Lines))
			for j, ln := range grp.Lines {
				lines[j] = apiSearchCodeLine{
					Line:     ln.Line,
					Col:      ln.Col,
					LineHTML: renderSearchCodeLineHTML(ln.Text, lang, ln.Spans),
				}
			}
			out.Code[i] = apiSearchCodeGroup{
				Path:     grp.Path,
				InChange: grp.InChange,
				Lines:    lines,
			}
		}
	}

	writeJSON(w, http.StatusOK, out)
}

func renderHighlightedText(text string, spans []engine.SearchMatchSpan) string {
	if len(spans) == 0 {
		return html.EscapeString(text)
	}
	var b strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp.Start < pos || sp.Start > len(text) {
			continue
		}
		end := min(sp.End, len(text))
		if sp.Start > pos {
			b.WriteString(html.EscapeString(text[pos:sp.Start]))
		}
		if end > sp.Start {
			b.WriteString(`<mark class="cs-match">`)
			b.WriteString(html.EscapeString(text[sp.Start:end]))
			b.WriteString(`</mark>`)
		}
		pos = end
	}
	if pos < len(text) {
		b.WriteString(html.EscapeString(text[pos:]))
	}
	return b.String()
}

func renderSearchCodeLineHTML(line string, lang diff.Lang, spans []engine.SearchMatchSpan) string {
	// Trim leading whitespace for compact search result rows, windowing around late matches on long lines.
	trimmed := strings.TrimLeft(line, " \t")
	offset := len(line) - len(trimmed)
	prefix := ""
	suffix := ""
	const maxLen = 240
	if len(trimmed) > maxLen {
		start := 0
		if len(spans) > 0 && spans[0].Start-offset > maxLen-60 {
			start = spans[0].Start - offset - 40
			if start < 0 {
				start = 0
			}
			for start < len(trimmed) && !utf8.RuneStart(trimmed[start]) {
				start++
			}
			if start > 0 {
				prefix = "…"
			}
		}
		end := min(start+maxLen, len(trimmed))
		for end > start && end < len(trimmed) && !utf8.RuneStart(trimmed[end]) {
			end--
		}
		if end < len(trimmed) {
			suffix = "…"
		}
		trimmed = trimmed[start:end]
		offset += start
	}
	adjSpans := make([]engine.SearchMatchSpan, 0, len(spans))
	for _, sp := range spans {
		s := sp.Start - offset
		e := sp.End - offset
		if e <= 0 || s >= len(trimmed) {
			continue
		}
		if s < 0 {
			s = 0
		}
		if e > len(trimmed) {
			e = len(trimmed)
		}
		adjSpans = append(adjSpans, engine.SearchMatchSpan{Start: s, End: e})
	}

	if lang == diff.LangNone {
		return prefix + renderHighlightedText(trimmed, adjSpans) + suffix
	}

	tokLines := diff.HighlightLines([]string{trimmed}, lang)
	var toks []diff.TokenSpan
	if len(tokLines) > 0 {
		toks = tokLines[0]
	}

	// Walk byte boundaries from both tokens and match spans so syntax classes and <mark> compose cleanly.
	var b strings.Builder
	b.WriteString(prefix)
	ti, si, pos := 0, 0, 0
	n := len(trimmed)
	for pos < n {
		for ti < len(toks) && toks[ti].End <= pos {
			ti++
		}
		for si < len(adjSpans) && adjSpans[si].End <= pos {
			si++
		}
		next := n
		var tokClass string
		if ti < len(toks) {
			if toks[ti].Start <= pos {
				tokClass = string(toks[ti].Class)
				next = min(next, toks[ti].End)
			} else {
				next = min(next, toks[ti].Start)
			}
		}
		inMatch := false
		if si < len(adjSpans) {
			if adjSpans[si].Start <= pos {
				inMatch = true
				next = min(next, adjSpans[si].End)
			} else {
				next = min(next, adjSpans[si].Start)
			}
		}
		if next <= pos {
			next = pos + 1
		}
		seg := html.EscapeString(trimmed[pos:next])
		if inMatch {
			b.WriteString(`<mark class="cs-match">`)
		}
		if tokClass != "" {
			b.WriteString(`<span class="`)
			b.WriteString(tokClass)
			b.WriteString(`">`)
			b.WriteString(seg)
			b.WriteString(`</span>`)
		} else {
			b.WriteString(seg)
		}
		if inMatch {
			b.WriteString(`</mark>`)
		}
		pos = next
	}
	b.WriteString(suffix)
	return b.String()
}

// handleAPIFilePreview returns a gosym.Result-compatible highlighted file snippet
// so files selected in floating Code Search open directly inside the floating window.
func (s *Server) handleAPIFilePreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	repoSlug := q.Get("repo")
	changeKey := q.Get("change")
	baseSel := q.Get("base")
	targetSel := q.Get("target")
	side := q.Get("side")
	filePath := strings.TrimPrefix(q.Get("path"), "/")
	lineNum, _ := strconv.Atoi(q.Get("line"))
	if lineNum < 1 {
		lineNum = 1
	}

	if repoSlug == "" || filePath == "" {
		writeJSON(w, http.StatusOK, gosym.Result{Found: false})
		return
	}
	rec, err := s.d.RepoBySlug(repoSlug)
	if err != nil || rec == nil {
		writeJSON(w, http.StatusOK, gosym.Result{Found: false})
		return
	}
	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		writeJSON(w, http.StatusOK, gosym.Result{Found: false})
		return
	}

	_, _, _, _, searchRev, _ := s.resolveSearchRevs(repo, rec.ID, changeKey, baseSel, targetSel, side)
	if searchRev == "" {
		writeJSON(w, http.StatusOK, gosym.Result{Found: false})
		return
	}

	readPath := filePath
	if vcs.IsCommitMsgPath(filePath) {
		readPath = vcs.CommitMsgPath
	}
	data, err := repo.Content(searchRev, readPath)
	if err != nil || diff.IsBinary(data) {
		writeJSON(w, http.StatusOK, gosym.Result{Found: false})
		return
	}

	lines := diff.SplitLines(data)
	if len(lines) == 0 {
		writeJSON(w, http.StatusOK, gosym.Result{
			Found:       true,
			InRepo:      true,
			DefPath:     filePath,
			DisplayPath: filePath,
			DefLine:     1,
		})
		return
	}
	if lineNum > len(lines) {
		lineNum = len(lines)
	}

	const maxCount = 5000
	startLine := 1
	if lineNum > maxCount/2 {
		startLine = lineNum - maxCount/2
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
	truncated := startLine > 1 || endLine < len(lines)

	lang := diff.DetectLang(filePath)
	var allToks [][]diff.TokenSpan
	if startLine == 1 {
		allToks = diff.HighlightLines(lines[:endLine], lang)
	} else {
		keep := make([]bool, endLine)
		for i := startLine - 1; i < endLine; i++ {
			keep[i] = true
		}
		allToks = diff.HighlightLinesKeep(lines[:endLine], lang, keep)
	}

	snippet := make([]gosym.SnippetLine, 0, endLine-startLine+1)
	for ln := startLine; ln <= endLine; ln++ {
		var toks []diff.TokenSpan
		if ln-1 < len(allToks) {
			toks = allToks[ln-1]
		}
		snippet = append(snippet, gosym.SnippetLine{
			Line:   ln,
			HTML:   string(diff.RenderTokensHTML(lines[ln-1], toks, nil)),
			Active: ln == lineNum,
		})
	}

	writeJSON(w, http.StatusOK, gosym.Result{
		Found:            true,
		InRepo:           true,
		DefPath:          filePath,
		DisplayPath:      filePath,
		DefLine:          lineNum,
		Snippet:          snippet,
		SnippetTruncated: truncated,
	})
}
