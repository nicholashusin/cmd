package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/engine"
	"nicholas.sh/cmd/review/internal/gosym"
	"nicholas.sh/cmd/review/internal/vcs"
)

type diffRowView struct {
	diff.Row
	RenderLeftHTML  any
	RenderRightHTML any
}

// FileDiffData holds all data necessary to render a single file diff block.
type FileDiffData struct {
	Lazy               bool
	InDiff             bool
	SingleColumn       bool
	ReadOnly           bool
	ExtSide            string
	ExtPkg             string
	ExtFile            string
	OriginPath         string
	RepoSlug           string
	ChangeKey          string
	LeftRev            string
	RightRev           string
	BaseParam          string
	TargetParam        string
	FilePath           string
	StatusCode         string
	StatusTitle        string
	FileIndex          int
	DiffMode           string
	CtxParam           string
	IsAllRebased       bool
	IsBinary           bool
	IsTooLarge         bool
	FileReviewed       bool
	Added              int
	Deleted            int
	Rows               []diffRowView
	FileThreads        []*engine.ThreadView
	LeftThreads        map[int][]*engine.ThreadView
	RightThreads       map[int][]*engine.ThreadView
	NextUnreviewedFile string
	CommentCounts      engine.CommentCounts
	ScopesJSON         string
	ScopesSide         string
	ScopesLJSON        string
	ScopesRJSON        string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	repos, err := s.d.Repos()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type repoCard struct {
		Record    *db.RepoRecord
		GraphRows []vcs.GraphRow
		Summaries map[string]*db.ChangeSummary
		Available bool
	}

	if len(repos) == 0 {
		s.renderTemplate(w, "dashboard.html", map[string]any{
			"StaticVersion": s.staticVersion,
			"Repos":         []*repoCard{},
		})
		return
	}

	repoIDs := make([]int64, len(repos))
	for i, rec := range repos {
		repoIDs[i] = rec.ID
	}
	multiSums, _ := s.d.RepoSummariesMulti(repoIDs)

	cards := make([]*repoCard, len(repos))
	numWorkers := min(len(repos), 8)
	ch := make(chan int, len(repos))
	for i := range repos {
		ch <- i
	}
	close(ch)

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for w := 0; w < numWorkers; w++ {
		go func() {
			defer wg.Done()
			for i := range ch {
				rec := repos[i]
				repo, err := vcs.Open(rec.RootPath, s.cache)
				if err != nil {
					cards[i] = &repoCard{
						Record:    rec,
						Available: false,
					}
					continue
				}
				changes, err := repo.Changes()
				if err != nil {
					cards[i] = &repoCard{
						Record:    rec,
						Available: true,
					}
					continue
				}
				graphRows := vcs.BuildGraph(changes, func(c *vcs.Change) bool {
					return !c.Working && !c.Empty
				})
				var sums map[string]*db.ChangeSummary
				if multiSums != nil {
					sums = multiSums[rec.ID]
				}
				cards[i] = &repoCard{
					Record:    rec,
					GraphRows: graphRows,
					Summaries: sums,
					Available: true,
				}
			}
		}()
	}
	wg.Wait()

	s.renderTemplate(w, "dashboard.html", map[string]any{
		"StaticVersion": s.staticVersion,
		"Repos":         cards,
	})
}

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("repo")
	rec, err := s.d.RepoBySlug(slug)
	if err != nil || rec == nil {
		http.NotFound(w, r)
		return
	}

	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	changes, err := repo.Changes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	graphRows := vcs.BuildGraph(changes, func(c *vcs.Change) bool {
		return !c.Working && !c.Empty
	})
	sums, _ := s.d.RepoSummaries(rec.ID)

	s.renderTemplate(w, "repo.html", map[string]any{
		"StaticVersion": s.staticVersion,
		"Record":        rec,
		"GraphRows":     graphRows,
		"Summaries":     sums,
	})
}

func (s *Server) handleChange(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("repo")
	changeKey := r.PathValue("key")

	rec, err := s.d.RepoBySlug(slug)
	if err != nil || rec == nil {
		http.NotFound(w, r)
		return
	}

	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	c, err := s.resolveChange(repo, changeKey)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	baseSel := r.URL.Query().Get("base")
	targetSel := r.URL.Query().Get("target")

	view, err := engine.BuildChangeView(s.d, repo, rec, c, baseSel, targetSel)
	if err != nil {
		http.Error(w, fmt.Sprintf("Building change view: %v", err), http.StatusInternalServerError)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	var searchResults []engine.SearchFileResult
	if query != "" {
		searchResults, _ = engine.SearchChange(repo, c, view.TargetRev, view.BaseRev, query, true)
	}

	_, revLGTM, _ := s.d.RevMark(rec.ID, c.Key, view.TargetRev)
	isLGTM := revLGTM || (view.Summary != nil && view.Summary.LGTM)
	headerStatus := "active"
	if isLGTM {
		headerStatus = "lgtm"
	}

	diffsParam := r.URL.Query().Get("diffs")
	if diffsParam == "" {
		diffsParam = s.d.Pref("change_diffs", "all")
	}
	if diffsParam == "all" || diffsParam == "list" {
		_ = s.d.SetPref("change_diffs", diffsParam)
	}

	diffMode := r.URL.Query().Get("mode")
	if diffMode == "" {
		diffMode = r.URL.Query().Get("view")
	}
	if diffMode == "" {
		diffMode = s.d.Pref("diff_mode", "split")
	}

	ctxParam := r.URL.Query().Get("ctx")

	var diffFiles []*FileDiffData
	if diffsParam == "all" {
		bundle := s.getMoveBundle(repo, view)
		eagerLines := 0
		eagerCount := 0
		for _, f := range view.Files {
			fileIdx := len(diffFiles)
			fileLines := f.AddedLines + f.DeletedLines
			canEager := (eagerCount < 12 && eagerLines+fileLines <= 1000) || (len(f.Threads) > 0 && fileLines <= 3000)
			if canEager {
				eagerLines += fileLines
				eagerCount++
				fd := s.buildFileDiffData(repo, rec, c, view, f.Path, fileIdx, baseSel, targetSel, diffMode, ctxParam, 25000, bundle)
				diffFiles = append(diffFiles, fd)
			} else {
				diffFiles = append(diffFiles, &FileDiffData{
					Lazy:          true,
					RepoSlug:      rec.Slug,
					ChangeKey:     c.Key,
					LeftRev:       view.BaseRev,
					RightRev:      view.TargetRev,
					BaseParam:     baseSel,
					TargetParam:   targetSel,
					FilePath:      f.Path,
					StatusCode:    f.StatusCode(),
					StatusTitle:   f.StatusTitle(),
					FileIndex:     fileIdx,
					DiffMode:      diffMode,
					CtxParam:      ctxParam,
					FileReviewed:  f.Reviewed,
					Added:         f.AddedLines,
					Deleted:       f.DeletedLines,
					CommentCounts: f.CommentCounts,
				})
			}
		}
	}

	s.renderTemplate(w, "change.html", map[string]any{
		"StaticVersion": s.staticVersion,
		"Record":        rec,
		"View":          view,
		"Query":         query,
		"SearchResults": searchResults,
		"BaseParam":     baseSel,
		"TargetParam":   targetSel,
		"IsLGTM":        isLGTM,
		"HeaderStatus":  headerStatus,
		"DiffsMode":     diffsParam,
		"DiffMode":      diffMode,
		"CtxParam":      ctxParam,
		"DiffFiles":     diffFiles,
	})

	if s.sym != nil && view != nil {
		var goFiles []string
		for _, f := range view.Files {
			if strings.HasSuffix(f.Path, ".go") {
				goFiles = append(goFiles, f.Path)
			}
		}
		if len(goFiles) > 0 {
			if view.TargetRev != "" {
				s.sym.EnqueueWarm(gosym.WarmRequest{
					RepoRoot: rec.RootPath,
					Rev:      view.TargetRev,
					Files:    goFiles,
				})
			}
			if view.BaseRev != "" && view.BaseRev != view.TargetRev {
				s.sym.EnqueueWarm(gosym.WarmRequest{
					RepoRoot: rec.RootPath,
					Rev:      view.BaseRev,
					Files:    goFiles,
				})
			}
		}
	}
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("repo")
	changeKey := r.PathValue("key")
	filePath := r.PathValue("file")

	rec, err := s.d.RepoBySlug(slug)
	if err != nil || rec == nil {
		http.NotFound(w, r)
		return
	}

	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	c, err := s.resolveChange(repo, changeKey)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	baseSel := r.URL.Query().Get("base")
	targetSel := r.URL.Query().Get("target")

	view, err := engine.BuildChangeView(s.d, repo, rec, c, baseSel, targetSel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	vcsPath := filePath
	if vcs.IsCommitMsgPath(filePath) {
		vcsPath = vcs.CommitMsgPath
	}

	var prevFile, nextFile string
	fileIdx := -1
	for i, f := range view.Files {
		if f.Path == vcsPath || strings.TrimPrefix(f.Path, "/") == filePath {
			fileIdx = i
			break
		}
	}
	if fileIdx > 0 {
		prevFile = view.Files[fileIdx-1].Path
	}
	if fileIdx >= 0 && fileIdx < len(view.Files)-1 {
		nextFile = view.Files[fileIdx+1].Path
	} else if fileIdx == -1 && vcsPath == vcs.CommitMsgPath && len(view.Files) > 0 {
		nextFile = view.Files[0].Path
	}

	requestedMode := r.URL.Query().Get("mode")
	if requestedMode == "" {
		requestedMode = r.URL.Query().Get("view")
	}
	if requestedMode != "unified" && requestedMode != "split" {
		requestedMode = s.d.Pref("diff_mode", "split")
	}

	diffMode := requestedMode
	if fileIdx == -1 {
		diffMode = "unified"
	}

	ctxParam := r.URL.Query().Get("ctx")

	// Single-file view has no row cap (maxRows: 0)
	var bundle *MoveBundle
	if fileIdx != -1 && vcsPath != vcs.CommitMsgPath {
		bundle = s.getMoveBundle(repo, view)
	}
	diffData := s.buildFileDiffData(repo, rec, c, view, vcsPath, fileIdx, baseSel, targetSel, diffMode, ctxParam, 0, bundle)
	if !diffData.InDiff {
		diffData.DiffMode = "unified"
		diffData.SingleColumn = true
	}

	var nextUnreviewedFile string
	if len(view.Files) > 0 && fileIdx >= 0 {
		for i := 1; i < len(view.Files); i++ {
			idx := (fileIdx + i) % len(view.Files)
			vf := view.Files[idx]
			if !vf.Reviewed {
				nextUnreviewedFile = vf.Path
				break
			}
		}
	}
	diffData.NextUnreviewedFile = nextUnreviewedFile

	_, revLGTM, _ := s.d.RevMark(rec.ID, c.Key, view.TargetRev)
	isLGTM := revLGTM || (view.Summary != nil && view.Summary.LGTM)
	headerStatus := "active"
	if isLGTM {
		headerStatus = "lgtm"
	}

	s.renderTemplate(w, "diff.html", map[string]any{
		"StaticVersion": s.staticVersion,
		"Record":        rec,
		"View":          view,
		"FilePath":      vcsPath,
		"PrevFile":      prevFile,
		"NextFile":      nextFile,
		"FileReviewed":  diffData.FileReviewed,
		"InDiff":        diffData.InDiff,
		"External":      false,
		"IsLGTM":        isLGTM,
		"HeaderStatus":  headerStatus,
		"DiffMode":      requestedMode,
		"BaseParam":     baseSel,
		"TargetParam":   targetSel,
		"CtxParam":      ctxParam,
		"DiffData":      diffData,
	})

	if s.sym != nil && view != nil && strings.HasSuffix(vcsPath, ".go") {
		cleanVCS := strings.TrimPrefix(vcsPath, "/")
		warmRev := view.TargetRev
		if warmRev == "" || diffData.StatusCode == "D" {
			warmRev = view.BaseRev
		}
		if warmRev != "" {
			s.sym.EnqueueWarm(gosym.WarmRequest{
				RepoRoot: rec.RootPath,
				Rev:      warmRev,
				Files:    []string{cleanVCS},
			})
			if view.BaseRev != "" && view.BaseRev != warmRev && diffData.StatusCode != "A" {
				s.sym.EnqueueWarm(gosym.WarmRequest{
					RepoRoot: rec.RootPath,
					Rev:      view.BaseRev,
					Files:    []string{cleanVCS},
				})
			}
		}
	}
}

func (s *Server) handleExternalFile(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("repo")
	changeKey := r.PathValue("key")
	pkgParam := r.URL.Query().Get("pkg")
	fileParam := r.URL.Query().Get("file")
	originParam := r.URL.Query().Get("origin")
	side := r.URL.Query().Get("side")
	if side != "L" {
		side = "R"
	}
	baseSel := r.URL.Query().Get("base")
	targetSel := r.URL.Query().Get("target")

	if pkgParam == "" || fileParam == "" {
		http.Error(w, "missing pkg or file", http.StatusBadRequest)
		return
	}

	rec, err := s.d.RepoBySlug(slug)
	if err != nil || rec == nil {
		http.NotFound(w, r)
		return
	}

	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	c, err := s.resolveChange(repo, changeKey)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	view, err := engine.BuildChangeView(s.d, repo, rec, c, baseSel, targetSel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if s.sym == nil {
		http.NotFound(w, r)
		return
	}

	rev := view.TargetRev
	if side == "L" {
		rev = view.BaseRev
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	displayPath, content, ok := s.sym.ResolveExternalFile(ctx, gosym.ExternalFileRequest{
		PkgImportPath: pkgParam,
		PkgFile:       fileParam,
		OriginPath:    originParam,
		Repo:          repo,
		Rev:           rev,
	})
	if !ok {
		http.Error(w, "external file not found", http.StatusNotFound)
		return
	}

	diffData := &FileDiffData{
		RepoSlug:     rec.Slug,
		ChangeKey:    c.Key,
		LeftRev:      view.BaseRev,
		RightRev:     view.TargetRev,
		FilePath:     displayPath,
		FileIndex:    -1,
		DiffMode:     "unified",
		CtxParam:     "all",
		SingleColumn: true,
		ReadOnly:     true,
		ExtSide:      side,
		ExtPkg:       pkgParam,
		ExtFile:      fileParam,
		OriginPath:   originParam,
		InDiff:       false,
		BaseParam:    baseSel,
		TargetParam:  targetSel,
	}

	requestedMode := r.URL.Query().Get("mode")
	if requestedMode == "" {
		requestedMode = r.URL.Query().Get("view")
	}
	if requestedMode != "unified" && requestedMode != "split" {
		requestedMode = s.d.Pref("diff_mode", "split")
	}

	if diff.IsBinary(content) {
		diffData.IsBinary = true
	} else {
		lines := diff.SplitLines(content)
		toks := diff.HighlightLines(lines, diff.DetectLang(fileParam))
		rows := make([]diffRowView, len(lines))
		for i, line := range lines {
			lineNum := i + 1
			var lineToks []diff.TokenSpan
			if i < len(toks) {
				lineToks = toks[i]
			}
			rView := diffRowView{
				Row: diff.Row{
					Type:      diff.RowEqual,
					LeftText:  line,
					RightText: line,
				},
			}
			if side == "L" {
				rView.Row.LeftLine = lineNum
				rView.RenderLeftHTML = diff.RenderTokensHTMLWS(line, lineToks, nil, false)
			} else {
				rView.Row.RightLine = lineNum
				rView.RenderRightHTML = diff.RenderTokensHTMLWS(line, lineToks, nil, false)
			}
			rows[i] = rView
		}
		diffData.Rows = rows

		scopes := diff.ExtractScopes(fileParam, content, lines)
		if len(scopes) > 0 {
			diffData.ScopesSide = side
			if b, err := json.Marshal(scopes); err == nil {
				diffData.ScopesJSON = string(b)
				if side == "L" {
					diffData.ScopesLJSON = string(b)
				} else {
					diffData.ScopesRJSON = string(b)
				}
			}
		}
	}

	_, revLGTM, _ := s.d.RevMark(rec.ID, c.Key, view.TargetRev)
	isLGTM := revLGTM || (view.Summary != nil && view.Summary.LGTM)
	headerStatus := "active"
	if isLGTM {
		headerStatus = "lgtm"
	}

	s.renderTemplate(w, "diff.html", map[string]any{
		"StaticVersion": s.staticVersion,
		"Record":        rec,
		"View":          view,
		"FilePath":      displayPath,
		"DiffData":      diffData,
		"External":      true,
		"InDiff":        false,
		"IsLGTM":        isLGTM,
		"HeaderStatus":  headerStatus,
		"DiffMode":      requestedMode,
		"BaseParam":     baseSel,
		"TargetParam":   targetSel,
		"CtxParam":      "all",
	})
}

func (s *Server) buildFileDiffData(repo vcs.Repo, rec *db.RepoRecord, c *vcs.Change, view *engine.ChangeView, vcsPath string, fileIdx int, baseSel, targetSel, diffMode, ctxParam string, maxRows int, bundle *MoveBundle) *FileDiffData {
	data := &FileDiffData{
		RepoSlug:    rec.Slug,
		ChangeKey:   c.Key,
		LeftRev:     view.BaseRev,
		RightRev:    view.TargetRev,
		BaseParam:   baseSel,
		TargetParam: targetSel,
		FilePath:    vcsPath,
		FileIndex:   fileIdx,
		DiffMode:    diffMode,
		CtxParam:    ctxParam,
	}

	var matchedFile *engine.ViewFile
	for _, f := range view.Files {
		if f.Path == vcsPath || strings.TrimPrefix(f.Path, "/") == strings.TrimPrefix(vcsPath, "/") {
			matchedFile = f
			data.Added = f.AddedLines
			data.Deleted = f.DeletedLines
			data.StatusCode = f.StatusCode()
			data.StatusTitle = f.StatusTitle()
			data.FileReviewed = f.Reviewed
			break
		}
	}
	data.InDiff = matchedFile != nil

	if bundle == nil && fileIdx != -1 && vcsPath != vcs.CommitMsgPath {
		bundle = s.getMoveBundle(repo, view)
	}

	cleanPath := strings.TrimPrefix(vcsPath, "/")
	var baseData []byte
	var targetData []byte
	var baseLines []string
	var targetLines []string
	var rawRows []diff.Row

	var mfd *moveFileData
	if bundle != nil && vcsPath != vcs.CommitMsgPath {
		mfd = bundle.files[cleanPath]
	}

	if mfd != nil {
		if mfd.isBinary {
			data.IsBinary = true
			return data
		}
		if mfd.isTooLarge {
			if maxRows > 0 && maxRows <= 25000 {
				data.IsTooLarge = true
				return data
			}
			mfd = nil
		}
	}
	if mfd != nil {
		baseData = mfd.baseData
		targetData = mfd.targetData
		baseLines = mfd.baseLines
		targetLines = mfd.targetLines
		rawRows = mfd.cloneRows()
	} else {
		if !(vcsPath == vcs.CommitMsgPath && !view.HasCommitMsgBase) {
			baseData, _ = repo.Content(view.BaseRev, vcsPath)
		}
		targetData, _ = repo.Content(view.TargetRev, vcsPath)

		if diff.IsBinary(baseData) || diff.IsBinary(targetData) {
			data.IsBinary = true
			return data
		}

		baseLines = diff.SplitLines(baseData)
		targetLines = diff.SplitLines(targetData)

		if maxRows > 0 && (len(baseLines) > maxRows || len(targetLines) > maxRows) {
			data.IsTooLarge = true
			return data
		}

		edits := diff.Edits(baseLines, targetLines)
		rawRows = diff.BuildDiffRows(baseLines, targetLines, edits)
	}

	// Rebase attribution applies only when comparing two snapshots of the same change
	// whose parent revisions differ (never when diffing against c.Parent or /COMMIT_MSG).
	if view.BaseRev != c.Parent && vcsPath != vcs.CommitMsgPath {
		var baseParentRev, targetParentRev string
		for _, snap := range view.Snapshots {
			if snap.Rev == view.BaseRev {
				baseParentRev = snap.ParentRev
			}
			if snap.Rev == view.TargetRev {
				targetParentRev = snap.ParentRev
			}
		}
		if targetParentRev == "" {
			targetParentRev = c.Parent
		}
		if baseParentRev != "" && targetParentRev != "" && baseParentRev != targetParentRev {
			baseParentData, _ := repo.Content(baseParentRev, vcsPath)
			targetParentData, _ := repo.Content(targetParentRev, vcsPath)
			diff.MarkRebased(rawRows, baseData, targetData, baseParentData, targetParentData)
			data.IsAllRebased = diff.AllRebased(rawRows)
		}
	}

	var rawFileThreads []*db.Thread
	if matchedFile != nil {
		rawFileThreads = matchedFile.Threads
	} else {
		for _, th := range view.Threads {
			if th.Path == vcsPath || strings.TrimPrefix(th.Path, "/") == strings.TrimPrefix(vcsPath, "/") {
				rawFileThreads = append(rawFileThreads, th)
			}
		}
	}

	hasCommitMsgBase := vcsPath != vcs.CommitMsgPath || view.HasCommitMsgBase
	fileThreads, leftThreads, rightThreads, counts := engine.ReanchorViewThreadsWithLines(baseLines, targetLines, view.BaseRev, view.TargetRev, view.Snapshots, view.IsLiveTarget, hasCommitMsgBase, rawFileThreads)
	data.FileThreads = fileThreads
	data.LeftThreads = leftThreads
	data.RightThreads = rightThreads
	data.CommentCounts = counts

	hasChanges := false
	for _, r := range rawRows {
		if r.Type != diff.RowEqual {
			hasChanges = true
			break
		}
	}

	if mfd != nil {
		data.ScopesSide, data.ScopesJSON, data.ScopesLJSON, data.ScopesRJSON = mfd.scopes(cleanPath)
	} else {
		data.ScopesSide, data.ScopesJSON, data.ScopesLJSON, data.ScopesRJSON = extractFileScopes(cleanPath, baseData, baseLines, targetData, targetLines)
	}

	var collapsed []diff.Row
	if ctxParam == "all" || (!hasChanges && (maxRows == 0 || vcsPath == vcs.CommitMsgPath)) {
		collapsed = rawRows
	} else {
		ctxLines := 3
		collapsed = diff.Collapse(rawRows, ctxLines, func(left, right int) bool {
			return (left > 0 && len(leftThreads[left]) > 0) || (right > 0 && len(rightThreads[right]) > 0)
		})
	}

	var displayRows []diff.Row
	if diffMode == "unified" {
		displayRows = diff.Unified(collapsed)
	} else {
		displayRows = collapsed
	}

	baseKeep := make([]bool, len(baseLines))
	targetKeep := make([]bool, len(targetLines))
	for _, r := range displayRows {
		if r.Type != diff.RowSkip {
			if r.LeftLine > 0 && r.LeftLine <= len(baseKeep) {
				baseKeep[r.LeftLine-1] = true
			}
			if r.RightLine > 0 && r.RightLine <= len(targetKeep) {
				targetKeep[r.RightLine-1] = true
			}
		}
	}

	lang := diff.DetectLang(vcsPath)
	baseTokens := diff.HighlightLinesKeep(baseLines, lang, baseKeep)
	targetTokens := diff.HighlightLinesKeep(targetLines, lang, targetKeep)

	renderedRows := make([]diffRowView, len(displayRows))
	for i, r := range displayRows {
		rv := diffRowView{Row: r}
		if r.Type != diff.RowSkip {
			showWSLeft := false
			showWSRight := (r.Type == diff.RowAdd || r.Type == diff.RowChange)

			intraL := r.LeftIntra
			if r.MovedL != nil {
				if r.MovedL.Modified && len(r.MovedL.MovedIntra) > 0 {
					intraL = r.MovedL.MovedIntra
				} else if !r.MovedL.Modified {
					intraL = nil
				}
			}

			intraR := r.RightIntra
			if r.MovedR != nil {
				if r.MovedR.Modified && len(r.MovedR.MovedIntra) > 0 {
					intraR = r.MovedR.MovedIntra
				} else if !r.MovedR.Modified {
					intraR = nil
				}
			}

			if r.LeftLine > 0 && r.LeftLine <= len(baseTokens) {
				rv.RenderLeftHTML = diff.RenderTokensHTMLWS(r.LeftText, baseTokens[r.LeftLine-1], intraL, showWSLeft)
			} else if r.LeftText != "" {
				rv.RenderLeftHTML = diff.RenderLineHTML(r.LeftText, lang, intraL)
			}
			if r.RightLine > 0 && r.RightLine <= len(targetTokens) {
				rv.RenderRightHTML = diff.RenderTokensHTMLWS(r.RightText, targetTokens[r.RightLine-1], intraR, showWSRight)
			} else if r.RightText != "" {
				rv.RenderRightHTML = diff.RenderLineHTML(r.RightText, lang, intraR)
			}
		}
		renderedRows[i] = rv
	}
	data.Rows = renderedRows

	return data
}
