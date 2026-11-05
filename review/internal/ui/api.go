package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
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

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK\n"))
}

func (s *Server) handleAPIState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if repoSlug := strings.TrimSpace(r.URL.Query().Get("repo")); repoSlug != "" && s.d != nil {
		if rec, err := s.d.RepoBySlug(repoSlug); err == nil && rec != nil {
			if repo, err := vcs.Open(rec.RootPath, s.cache); err == nil {
				_, _ = repo.Changes()
			}
		}
	}
	sig := ""
	if s.broker != nil {
		sig = s.broker.StateSignature()
	}
	writeJSON(w, http.StatusOK, map[string]string{"sig": sig})
}

func (s *Server) handleAPINotify(w http.ResponseWriter, r *http.Request) {
	s.broker.Broadcast("reload", map[string]string{"reason": "notify"})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAPISnapshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo   string `json:"repo"`
		Change string `json:"change"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rec, err := s.d.RepoBySlug(req.Repo)
	if err != nil || rec == nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}

	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	c, err := s.resolveChange(repo, req.Change)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	snap, created, err := engine.TakeSnapshot(s.d, repo, rec.ID, c, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broker.Broadcast("reload", map[string]any{"reason": "snapshot", "snap": snap})
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snap, "created": created})
}

func (s *Server) handleAPIPublish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo     string `json:"repo"`
		Change   string `json:"change"`
		LeftRev  string `json:"left_rev"`
		RightRev string `json:"right_rev"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rec, err := s.d.RepoBySlug(req.Repo)
	if err != nil || rec == nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}

	var res *engine.PublishResult
	var repo vcs.Repo
	var c *vcs.Change
	changeKey := req.Change
	if req.Change != "" {
		if r, err := vcs.Open(rec.RootPath, s.cache); err == nil {
			repo = r
			if resolved, err := s.resolveChange(repo, req.Change); err == nil && resolved != nil {
				c = resolved
				changeKey = c.Key
			}
		}
		res, err = engine.PublishChange(s.d, repo, rec.ID, changeKey, c, true)
	} else {
		if r, err := vcs.Open(rec.RootPath, s.cache); err == nil {
			repo = r
		}
		res, err = engine.PublishRepo(s.d, repo, rec.ID, true)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	count := res.Published

	var threadHTML map[string]string
	var hiddenThreadIDs []int64
	var fileCountsMap map[string]engine.CommentCounts
	var changeCounts engine.CommentCounts
	var snaps []*db.Snapshot

	if req.Change != "" {
		snaps, _ = s.d.Snapshots(rec.ID, changeKey)
	}

	if count > 0 && req.Change != "" {
		leftRev := req.LeftRev
		rightRev := req.RightRev
		if c != nil {
			if leftRev == "" {
				leftRev = c.Parent
			}
			if rightRev == "" {
				rightRev = c.Rev
			}
		}
		isLiveTarget := (c == nil || rightRev == "" || rightRev == c.Rev)

		threads, _ := s.d.ThreadsForChange(rec.ID, changeKey)
		threadHTML = make(map[string]string)
		hiddenThreadIDs = []int64{}
		fileCountsMap = make(map[string]engine.CommentCounts)
		changeCounts = engine.ComputeCommentCounts(threads)

		scope := engine.NewViewScope(snaps, leftRev, rightRev, isLiveTarget)
		var paths []string
		var visibleThreads []*db.Thread
		pathThreadMap := make(map[string][]*db.Thread)
		for _, th := range threads {
			norm := strings.TrimPrefix(th.Path, "/")
			if vcs.IsCommitMsgPath(th.Path) {
				norm = strings.TrimPrefix(vcs.CommitMsgPath, "/")
			}
			if _, exists := pathThreadMap[norm]; !exists {
				paths = append(paths, norm)
			}
			if _, visible := scope.ThreadSide(th); visible {
				pathThreadMap[norm] = append(pathThreadMap[norm], th)
				visibleThreads = append(visibleThreads, th)
			} else {
				hiddenThreadIDs = append(hiddenThreadIDs, th.ID)
			}
		}
		for _, p := range paths {
			fileCountsMap[p] = engine.ComputeCommentCounts(pathThreadMap[p])
		}
		if len(visibleThreads) > 0 {
			tvs := engine.ReanchorThreadsForView(repo, c, snaps, rightRev, leftRev, isLiveTarget, visibleThreads, nil)
			for _, tv := range tvs {
				if cardHTML, err := s.renderThreadView(tv); err == nil {
					threadHTML[strconv.FormatInt(tv.Thread.ID, 10)] = cardHTML
				}
			}
		}
	}

	if count > 0 || res.CreatedSnapshot {
		s.broker.Broadcast("reload", map[string]any{"reason": "publish", "count": count})
	}
	resp := map[string]any{
		"published":         count,
		"sig":               s.broker.StateSignature(),
		"change_counts":     changeCounts,
		"file_counts":       fileCountsMap,
		"thread_html":       threadHTML,
		"hidden_thread_ids": hiddenThreadIDs,
		"snapshot_created":  res.CreatedSnapshot,
	}
	if res.Snapshot != nil {
		resp["snapshot_num"] = res.Snapshot.Num
		resp["snapshot"] = res.Snapshot
	}
	if req.Change != "" {
		resp["snapshots"] = snaps
		if c != nil {
			resp["live_rev"] = c.Rev
			liveSnapNum := engine.SnapshotNumForRev(snaps, c.Rev)
			resp["snapshot_pending"] = (liveSnapNum == 0)
			if liveSnapNum > 0 {
				resp["target_desc"] = fmt.Sprintf("Snapshot %d", liveSnapNum)
			} else {
				resp["target_desc"] = "Live Snapshot"
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func resolveWebAuthor(reqAuthor string, repo vcs.Repo) string {
	if reqAuthor != "" {
		return reqAuthor
	}
	if repo != nil {
		if u := repo.User(); u != "" {
			return u
		}
	}
	return "reviewer"
}

func (s *Server) handleAPIThreadsAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo      string `json:"repo"`
		Change    string `json:"change"`
		Rev       string `json:"rev"`
		LeftRev   string `json:"left_rev"`
		RightRev  string `json:"right_rev"`
		Path      string `json:"path"`
		Side      string `json:"side"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		StartCol  int    `json:"start_col"`
		EndCol    int    `json:"end_col"`
		Author    string `json:"author"`
		Body      string `json:"body"`
		Resolved  bool   `json:"resolved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rec, err := s.d.RepoBySlug(req.Repo)
	if err != nil || rec == nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}

	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	c, err := s.resolveChange(repo, req.Change)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	author := resolveWebAuthor(req.Author, repo)

	th, msg, err := engine.AddCommentRangeWithTarget(s.d, repo, rec.ID, c, req.Rev, req.RightRev, req.Path, req.Side, req.StartLine, req.EndLine, req.StartCol, req.EndCol, author, req.Body, true, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if req.Resolved {
		_ = s.d.SetResolved(th.ID, true)
		th.Resolved = true
	}

	cleanPath := strings.TrimPrefix(th.Path, "/")
	addDraftKey := fmt.Sprintf("review_draft:new:%s:%s:%s:%s:%d", rec.Slug, c.Key, cleanPath, req.Side, req.StartLine)
	_ = s.d.DeleteFormDraft(rec.ID, c.Key, addDraftKey)

	if populatedTh, err := s.d.ThreadByID(th.ID); err == nil && populatedTh != nil {
		th = populatedTh
	}

	c, snaps, snapsLoaded := s.resolveViewContext(repo, c, rec.ID, c.Key, req.LeftRev, req.RightRev)
	cardHTML, _ := s.renderThreadCardInChange(repo, c, snaps, th, req.LeftRev, req.RightRev)
	fileCounts, changeCounts, _ := s.getCommentCountsForChangeView(repo, c, snaps, snapsLoaded, rec.ID, c.Key, th.Path, req.LeftRev, req.RightRev)

	s.broker.Broadcast("reload", map[string]any{"reason": "comment_add", "thread_id": th.ID})
	writeJSON(w, http.StatusOK, map[string]any{
		"thread":        th,
		"message":       msg,
		"html":          cardHTML,
		"sig":           s.broker.StateSignature(),
		"file_path":     strings.TrimPrefix(th.Path, "/"),
		"file_counts":   fileCounts,
		"change_counts": changeCounts,
	})
}

func (s *Server) handleAPIThreadsReply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ThreadID int64  `json:"thread_id"`
		Author   string `json:"author"`
		Body     string `json:"body"`
		Resolved *bool  `json:"resolved"`
		LeftRev  string `json:"left_rev"`
		RightRev string `json:"right_rev"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	th, err := s.d.ThreadByID(req.ThreadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if th == nil {
		http.Error(w, "thread not found", http.StatusNotFound)
		return
	}

	rec, _ := s.d.RepoByID(th.RepoID)
	var repo vcs.Repo
	if rec != nil {
		repo, _ = vcs.Open(rec.RootPath, s.cache)
	}

	author := resolveWebAuthor(req.Author, repo)

	msg, err := s.d.AddMessage(req.ThreadID, author, req.Body, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if req.Resolved != nil {
		_ = s.d.SetResolved(req.ThreadID, *req.Resolved)
	}

	if rec != nil {
		replyDraftKey := fmt.Sprintf("review_draft:reply:%s:%s:%d", rec.Slug, th.ChangeKey, th.ID)
		_ = s.d.DeleteFormDraft(th.RepoID, th.ChangeKey, replyDraftKey)
	}
	_ = s.d.DeleteFormDraftsForThread(th.RepoID, th.ChangeKey, strconv.FormatInt(th.ID, 10))

	th, err = s.d.ThreadByID(req.ThreadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	c, snaps, snapsLoaded := s.resolveViewContext(repo, nil, th.RepoID, th.ChangeKey, req.LeftRev, req.RightRev)
	cardHTML, _ := s.renderThreadCardInChange(repo, c, snaps, th, req.LeftRev, req.RightRev)
	fileCounts, changeCounts, _ := s.getCommentCountsForChangeView(repo, c, snaps, snapsLoaded, th.RepoID, th.ChangeKey, th.Path, req.LeftRev, req.RightRev)

	s.broker.Broadcast("reload", map[string]any{"reason": "comment_reply", "message_id": msg.ID})
	writeJSON(w, http.StatusOK, map[string]any{
		"message":       msg,
		"thread":        th,
		"html":          cardHTML,
		"sig":           s.broker.StateSignature(),
		"file_path":     strings.TrimPrefix(th.Path, "/"),
		"file_counts":   fileCounts,
		"change_counts": changeCounts,
	})
}

func (s *Server) handleAPIThreadsEdit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MessageID int64  `json:"message_id"`
		Body      string `json:"body"`
		Resolved  *bool  `json:"resolved"`
		LeftRev   string `json:"left_rev"`
		RightRev  string `json:"right_rev"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	msg, th, err := s.d.EditMessage(req.MessageID, req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if req.Resolved != nil {
		if err := s.d.SetResolved(th.ID, *req.Resolved); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	_ = s.d.DeleteFormDraftForMessage(th.RepoID, th.ChangeKey, strconv.FormatInt(req.MessageID, 10))
	if populatedTh, err := s.d.ThreadByID(th.ID); err == nil && populatedTh != nil {
		th = populatedTh
	}
	rec, _ := s.d.RepoByID(th.RepoID)
	if rec != nil {
		editDraftKey := fmt.Sprintf("review_draft:edit:%s:%s:%d", rec.Slug, th.ChangeKey, req.MessageID)
		_ = s.d.DeleteFormDraft(th.RepoID, th.ChangeKey, editDraftKey)
	}
	var repo vcs.Repo
	if rec != nil {
		repo, _ = vcs.Open(rec.RootPath, s.cache)
	}
	c, snaps, snapsLoaded := s.resolveViewContext(repo, nil, th.RepoID, th.ChangeKey, req.LeftRev, req.RightRev)
	cardHTML, _ := s.renderThreadCardInChange(repo, c, snaps, th, req.LeftRev, req.RightRev)
	fileCounts, changeCounts, _ := s.getCommentCountsForChangeView(repo, c, snaps, snapsLoaded, th.RepoID, th.ChangeKey, th.Path, req.LeftRev, req.RightRev)

	s.broker.Broadcast("reload", map[string]any{"reason": "comment_edit", "message_id": msg.ID})
	writeJSON(w, http.StatusOK, map[string]any{
		"message":       msg,
		"thread":        th,
		"html":          cardHTML,
		"sig":           s.broker.StateSignature(),
		"file_path":     strings.TrimPrefix(th.Path, "/"),
		"file_counts":   fileCounts,
		"change_counts": changeCounts,
	})
}

func (s *Server) handleAPIThreadsDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MessageID int64  `json:"message_id"`
		LeftRev   string `json:"left_rev"`
		RightRev  string `json:"right_rev"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	th, err := s.d.ThreadForMessage(req.MessageID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.d.DeleteDraftMessage(req.MessageID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	remainingThread, _ := s.d.ThreadByID(th.ID)
	threadDeleted := (remainingThread == nil || len(remainingThread.Messages) == 0)

	_ = s.d.DeleteFormDraftForMessage(th.RepoID, th.ChangeKey, strconv.FormatInt(req.MessageID, 10))
	rec, _ := s.d.RepoByID(th.RepoID)
	if rec != nil {
		delEditDraftKey := fmt.Sprintf("review_draft:edit:%s:%s:%d", rec.Slug, th.ChangeKey, req.MessageID)
		_ = s.d.DeleteFormDraft(th.RepoID, th.ChangeKey, delEditDraftKey)
	}

	if threadDeleted {
		_ = s.d.DeleteFormDraftsForThread(th.RepoID, th.ChangeKey, strconv.FormatInt(th.ID, 10))
		if rec != nil {
			delReplyDraftKey := fmt.Sprintf("review_draft:reply:%s:%s:%d", rec.Slug, th.ChangeKey, th.ID)
			_ = s.d.DeleteFormDraft(th.RepoID, th.ChangeKey, delReplyDraftKey)
		}
	}
	var repo vcs.Repo
	if rec != nil {
		repo, _ = vcs.Open(rec.RootPath, s.cache)
	}
	c, snaps, snapsLoaded := s.resolveViewContext(repo, nil, th.RepoID, th.ChangeKey, req.LeftRev, req.RightRev)
	var cardHTML string
	if !threadDeleted {
		cardHTML, _ = s.renderThreadCardInChange(repo, c, snaps, remainingThread, req.LeftRev, req.RightRev)
	}

	fileCounts, changeCounts, _ := s.getCommentCountsForChangeView(repo, c, snaps, snapsLoaded, th.RepoID, th.ChangeKey, th.Path, req.LeftRev, req.RightRev)

	s.broker.Broadcast("reload", map[string]any{"reason": "comment_delete", "message_id": req.MessageID})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "deleted",
		"thread_id":      th.ID,
		"thread_deleted": threadDeleted,
		"html":           cardHTML,
		"sig":            s.broker.StateSignature(),
		"file_path":      strings.TrimPrefix(th.Path, "/"),
		"file_counts":    fileCounts,
		"change_counts":  changeCounts,
	})
}

func (s *Server) handleAPIThreadsResolve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ThreadID int64  `json:"thread_id"`
		Resolved bool   `json:"resolved"`
		LeftRev  string `json:"left_rev"`
		RightRev string `json:"right_rev"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.d.SetResolved(req.ThreadID, req.Resolved); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	th, err := s.d.ThreadByID(req.ThreadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rec, _ := s.d.RepoByID(th.RepoID)
	var repo vcs.Repo
	if rec != nil {
		repo, _ = vcs.Open(rec.RootPath, s.cache)
	}
	c, snaps, snapsLoaded := s.resolveViewContext(repo, nil, th.RepoID, th.ChangeKey, req.LeftRev, req.RightRev)
	cardHTML, _ := s.renderThreadCardInChange(repo, c, snaps, th, req.LeftRev, req.RightRev)
	fileCounts, changeCounts, _ := s.getCommentCountsForChangeView(repo, c, snaps, snapsLoaded, th.RepoID, th.ChangeKey, th.Path, req.LeftRev, req.RightRev)

	s.broker.Broadcast("reload", map[string]any{"reason": "resolve", "thread_id": req.ThreadID})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"thread":        th,
		"html":          cardHTML,
		"sig":           s.broker.StateSignature(),
		"file_path":     strings.TrimPrefix(th.Path, "/"),
		"file_counts":   fileCounts,
		"change_counts": changeCounts,
	})
}

func (s *Server) handleAPIThreadsList(w http.ResponseWriter, r *http.Request) {
	repoSlug := r.URL.Query().Get("repo")
	changeKey := r.URL.Query().Get("change")
	baseSel := r.URL.Query().Get("base")
	targetSel := r.URL.Query().Get("target")

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

	c, err := s.resolveChange(repo, changeKey)
	if err != nil || c == nil {
		http.Error(w, "change not found", http.StatusNotFound)
		return
	}

	snaps, _ := s.d.Snapshots(rec.ID, c.Key)
	targetRev := c.Rev
	isLiveTarget := true
	if targetSel != "" && targetSel != "live" {
		isLiveTarget = false
		if strings.HasPrefix(targetSel, "snap:") {
			var num int
			if _, err := fmt.Sscanf(targetSel, "snap:%d", &num); err == nil {
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

	baseRev := c.Parent
	if strings.HasPrefix(baseSel, "snap:") {
		var num int
		if _, err := fmt.Sscanf(baseSel, "snap:%d", &num); err == nil {
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

	allThreads, err := s.d.ThreadsForChange(rec.ID, c.Key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	statusFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	var filteredThreads []*db.Thread
	for _, th := range allThreads {
		switch statusFilter {
		case "resolved":
			if th.Resolved {
				filteredThreads = append(filteredThreads, th)
			}
		case "draft":
			for _, m := range th.Messages {
				if m.Draft {
					filteredThreads = append(filteredThreads, th)
					break
				}
			}
		default:
			if !th.Resolved {
				filteredThreads = append(filteredThreads, th)
			}
		}
	}

	scope := engine.NewViewScope(snaps, baseRev, targetRev, isLiveTarget)
	tvs := engine.ReanchorThreadsForView(repo, c, snaps, targetRev, baseRev, isLiveTarget, filteredThreads, nil)

	type threadItem struct {
		ID            int64  `json:"id"`
		MessageID     int64  `json:"message_id,omitempty"`
		Resolved      bool   `json:"resolved"`
		Path          string `json:"path"`
		Side          string `json:"side"`
		Line          int    `json:"line"`
		EndLine       int    `json:"end_line"`
		Stale         bool   `json:"stale"`
		HasDraft      bool   `json:"has_draft"`
		Author        string `json:"author"`
		Preview       string `json:"preview"`
		PreviewHTML   string `json:"preview_html"`
		SnapshotNum   int    `json:"snapshot_num"`
		InCurrentView bool   `json:"in_current_view"`
	}

	items := make([]threadItem, 0, len(tvs))
	for _, tv := range tvs {
		th := tv.Thread
		_, inView := scope.ThreadSide(th)
		side := th.Side
		if inView {
			side = tv.DisplaySide()
		}
		cleanPath := strings.TrimPrefix(th.Path, "/")
		if cleanPath == "" {
			cleanPath = th.Path
		}
		if th.Path == vcs.CommitMsgPath {
			cleanPath = "COMMIT_MSG"
		}

		if statusFilter == "draft" {
			for _, m := range th.Messages {
				if !m.Draft {
					continue
				}
				preview, previewHTML := buildThreadPreview(m.Body)
				items = append(items, threadItem{
					ID:            th.ID,
					MessageID:     m.ID,
					Resolved:      th.Resolved,
					Path:          cleanPath,
					Side:          side,
					Line:          tv.CurrentStart,
					EndLine:       tv.CurrentEnd,
					Stale:         tv.Stale,
					HasDraft:      true,
					Author:        m.Author,
					Preview:       preview,
					PreviewHTML:   previewHTML,
					SnapshotNum:   th.SnapshotNum,
					InCurrentView: inView,
				})
			}
			continue
		}

		hasDraft := false
		firstMsg := ""
		author := ""
		for _, m := range th.Messages {
			if m.Draft {
				hasDraft = true
			}
			if firstMsg == "" && strings.TrimSpace(m.Body) != "" {
				firstMsg = m.Body
				author = m.Author
			}
		}
		preview, previewHTML := buildThreadPreview(firstMsg)

		items = append(items, threadItem{
			ID:            th.ID,
			Resolved:      th.Resolved,
			Path:          cleanPath,
			Side:          side,
			Line:          tv.CurrentStart,
			EndLine:       tv.CurrentEnd,
			Stale:         tv.Stale,
			HasDraft:      hasDraft,
			Author:        author,
			Preview:       preview,
			PreviewHTML:   previewHTML,
			SnapshotNum:   th.SnapshotNum,
			InCurrentView: inView,
		})
	}

	sort.SliceStable(items, func(i, j int) bool {
		pi, pj := items[i].Path, items[j].Path
		if (pi == "COMMIT_MSG") != (pj == "COMMIT_MSG") {
			return pi == "COMMIT_MSG"
		}
		if pi != pj {
			return pi < pj
		}
		if items[i].Line != items[j].Line {
			return items[i].Line < items[j].Line
		}
		if items[i].ID != items[j].ID {
			return items[i].ID < items[j].ID
		}
		return items[i].MessageID < items[j].MessageID
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"threads": items,
	})
}

func buildThreadPreview(body string) (preview, previewHTML string) {
	if strings.TrimSpace(body) == "" {
		return "", ""
	}
	rem := body
	for rem != "" {
		var line string
		line, rem, _ = strings.Cut(rem, "\n")
		if t := strings.TrimSpace(line); t != "" {
			preview = t
			break
		}
	}
	if len(preview) > 90 {
		if runes := []rune(preview); len(runes) > 90 {
			preview = string(runes[:87]) + "…"
		}
	}
	previewSrc := truncateMarkdownPreview(body, 12, 1200)
	previewHTML = string(RenderMarkdown(previewSrc))
	return preview, previewHTML
}

func truncateMarkdownPreview(src string, maxLines, maxBytes int) string {
	limit := len(src)
	if limit > maxBytes {
		limit = maxBytes
	}
	lineCount := 1
	cutByLine := false
	for i := 0; i < limit; i++ {
		if src[i] == '\n' {
			if lineCount >= maxLines {
				limit = i
				cutByLine = true
				break
			}
			lineCount++
		}
	}
	if !cutByLine && len(src) > maxBytes {
		for limit > 0 && !utf8.RuneStart(src[limit]) {
			limit--
		}
	}
	if limit >= len(src) {
		return src
	}
	out := src[:limit]

	inFence := false
	var fenceChar byte
	fenceLen := 0
	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if !inFence {
			if m := reFencedCode.FindStringSubmatch(line); m != nil {
				inFence = true
				fenceChar = m[1][0]
				fenceLen = len(m[1])
			}
			continue
		}
		trimLeading := strings.TrimLeft(line, " ")
		if len(line)-len(trimLeading) <= 3 {
			trim := strings.TrimSpace(trimLeading)
			if len(trim) >= fenceLen && trim[0] == fenceChar && strings.Trim(trim, string(fenceChar)) == "" {
				inFence = false
			}
		}
	}
	if inFence {
		out = strings.TrimRight(out, "\r\n") + "\n" + strings.Repeat(string(fenceChar), fenceLen)
	}
	return out
}

func (s *Server) handleAPIFilesReviewed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo     string `json:"repo"`
		Change   string `json:"change"`
		Rev      string `json:"rev"`
		Path     string `json:"path"`
		Reviewed bool   `json:"reviewed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rec, err := s.d.RepoBySlug(req.Repo)
	if err != nil || rec == nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}

	changeKey := req.Change
	if repo, err := vcs.Open(rec.RootPath, s.cache); err == nil {
		if c, err := s.resolveChange(repo, req.Change); err == nil && c != nil {
			changeKey = c.Key
		}
	}

	if vcs.IsCommitMsgPath(req.Path) {
		req.Path = vcs.CommitMsgPath
	}

	if err := s.d.SetFileReviewed(rec.ID, changeKey, req.Rev, req.Path, req.Reviewed); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broker.Broadcast("reload", map[string]any{"reason": "file_reviewed"})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "sig": s.broker.StateSignature()})
}

func (s *Server) handleAPIChangesReviewed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo     string `json:"repo"`
		Change   string `json:"change"`
		Rev      string `json:"rev"`
		Reviewed *bool  `json:"reviewed"`
		LGTM     *bool  `json:"lgtm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rec, err := s.d.RepoBySlug(req.Repo)
	if err != nil || rec == nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}

	changeKey := req.Change
	if repo, err := vcs.Open(rec.RootPath, s.cache); err == nil {
		if c, err := s.resolveChange(repo, req.Change); err == nil && c != nil {
			changeKey = c.Key
			if req.LGTM != nil && *req.LGTM {
				t := true
				req.Reviewed = &t
				files, _ := repo.Files(c.Parent, req.Rev)
				paths := []string{vcs.CommitMsgPath}
				for _, f := range files {
					if f.Path != vcs.CommitMsgPath {
						paths = append(paths, f.Path)
					}
				}
				_ = s.d.SetFileReviewedBatch(rec.ID, changeKey, req.Rev, paths, true)
			}
		}
	}

	if err := s.d.SetRevMark(rec.ID, changeKey, req.Rev, req.Reviewed, req.LGTM); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	revd, lgtm, _ := s.d.RevMark(rec.ID, changeKey, req.Rev)
	s.broker.Broadcast("reload", map[string]any{"reason": "change_reviewed"})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"sig":      s.broker.StateSignature(),
		"reviewed": revd,
		"lgtm":     lgtm,
	})
}

func (s *Server) handleAPIReposForget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo string `json:"repo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rec, err := s.d.ForgetRepo(req.Repo)
	if err != nil || rec == nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}

	if s.sym != nil {
		s.sym.Evict(rec.RootPath)
	}
	s.evictMoveCache(rec.RootPath)

	s.broker.Broadcast("reload", map[string]any{"reason": "repo_forget", "repo": req.Repo})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAPIPrefs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
		Val string `json:"val"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Key == "scope_side" {
		if req.Val != "L" {
			req.Val = "R"
		}
	}

	if err := s.d.SetPref(req.Key, req.Val); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAPIDiffExpand(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repoSlug := q.Get("repo")
	leftRev := q.Get("left_rev")
	rightRev := q.Get("right_rev")
	filePath := q.Get("path")
	mode := q.Get("mode")
	if mode == "" {
		mode = "split"
	}

	leftStart, _ := strconv.Atoi(q.Get("left_start"))
	leftEnd, _ := strconv.Atoi(q.Get("left_end"))
	rightStart, _ := strconv.Atoi(q.Get("right_start"))
	rightEnd, _ := strconv.Atoi(q.Get("right_end"))

	if leftStart < 0 {
		leftStart = 0
	}
	if leftEnd < 0 {
		leftEnd = 0
	}
	if rightStart < 0 {
		rightStart = 0
	}
	if rightEnd < 0 {
		rightEnd = 0
	}
	if leftStart > 10000000 || leftEnd > 10000000 || rightStart > 10000000 || rightEnd > 10000000 {
		http.Error(w, "line number out of range", http.StatusBadRequest)
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

	vcsPath := filePath
	if vcs.IsCommitMsgPath(filePath) {
		vcsPath = vcs.CommitMsgPath
	}

	var baseData []byte
	skipCommitMsgBase := false
	if vcsPath == vcs.CommitMsgPath {
		var c *vcs.Change
		var snaps []*db.Snapshot
		if changeKey := q.Get("change"); changeKey != "" {
			if resolved, err := s.resolveChange(repo, changeKey); err == nil && resolved != nil {
				c = resolved
				snaps, _ = s.d.Snapshots(rec.ID, c.Key)
			}
		}
		skipCommitMsgBase = !engine.CommitMsgHasBase(repo, c, snaps, leftRev, rightRev)
	}
	if leftRev != "" && !skipCommitMsgBase {
		baseData, _ = repo.Content(leftRev, vcsPath)
	}
	targetData, _ := repo.Content(rightRev, vcsPath)

	baseLines := diff.SplitLines(baseData)
	targetLines := diff.SplitLines(targetData)

	count := leftEnd - leftStart + 1
	if count <= 0 {
		count = rightEnd - rightStart + 1
	}
	if count <= 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	if count > 10000 {
		count = 10000
	}

	baseKeep := make([]bool, len(baseLines))
	targetKeep := make([]bool, len(targetLines))
	for k := 0; k < count; k++ {
		if leftStart > 0 {
			lLine := leftStart + k
			if lLine > 0 && lLine <= len(baseKeep) {
				baseKeep[lLine-1] = true
			}
		}
		if rightStart > 0 {
			rLine := rightStart + k
			if rLine > 0 && rLine <= len(targetKeep) {
				targetKeep[rLine-1] = true
			}
		}
	}

	lang := diff.DetectLang(vcsPath)
	baseTokens := diff.HighlightLinesKeep(baseLines, lang, baseKeep)
	targetTokens := diff.HighlightLinesKeep(targetLines, lang, targetKeep)

	var sb strings.Builder
	for k := 0; k < count; k++ {
		var lLine, rLine int
		if leftStart > 0 {
			lLine = leftStart + k
		}
		if rightStart > 0 {
			rLine = rightStart + k
		}

		var leftHTML, rightHTML string
		if lLine > 0 && lLine <= len(baseLines) {
			lText := baseLines[lLine-1]
			var tok []diff.TokenSpan
			if lLine <= len(baseTokens) {
				tok = baseTokens[lLine-1]
			}
			leftHTML = string(diff.RenderTokensHTMLWS(lText, tok, nil, false))
		}
		if rLine > 0 && rLine <= len(targetLines) {
			rText := targetLines[rLine-1]
			var tok []diff.TokenSpan
			if rLine <= len(targetTokens) {
				tok = targetTokens[rLine-1]
			}
			rightHTML = string(diff.RenderTokensHTMLWS(rText, tok, nil, false))
		}

		lLineStr := ""
		lClick := ""
		if lLine > 0 {
			lLineStr = strconv.Itoa(lLine)
			lClick = fmt.Sprintf(` onclick="openCommentForm(this.parentNode, %d, 'L')"`, lLine)
		}
		rLineStr := ""
		rClick := ""
		if rLine > 0 {
			rLineStr = strconv.Itoa(rLine)
			rClick = fmt.Sprintf(` onclick="openCommentForm(this.parentNode, %d, 'R')"`, rLine)
		}

		if mode == "unified" {
			displayHTML := rightHTML
			uSide := "R"
			if displayHTML == "" || rLine == 0 {
				displayHTML = leftHTML
				uSide = "L"
			}
			fmt.Fprintf(&sb, `<tr class="diff-row row-equal" data-line-l="%s" data-line-r="%s">`+
				`<td class="line-num" data-gutter-side="L"%s>%s</td>`+
				`<td class="line-num" data-gutter-side="R"%s>%s</td>`+
				`<td class="line-code-unified" data-side="%s">%s</td></tr>`+"\n",
				lLineStr, rLineStr, lClick, lLineStr, rClick, rLineStr, uSide, displayHTML)
		} else {
			fmt.Fprintf(&sb, `<tr class="diff-row row-equal" data-line-l="%s" data-line-r="%s">`+
				`<td class="line-num" data-gutter-side="L"%s>%s</td>`+
				`<td class="line-code" data-side="L">%s</td>`+
				`<td class="line-num" data-gutter-side="R"%s>%s</td>`+
				`<td class="line-code" data-side="R">%s</td></tr>`+"\n",
				lLineStr, rLineStr, lClick, lLineStr, leftHTML, rClick, rLineStr, rightHTML)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(sb.String()))
}

func (s *Server) handleAPIDiffFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repoSlug := q.Get("repo")
	changeKey := q.Get("change")
	filePath := q.Get("path")
	fileIdx, _ := strconv.Atoi(q.Get("index"))
	baseSel := q.Get("base")
	targetSel := q.Get("target")
	diffMode := q.Get("mode")
	if diffMode == "" {
		diffMode = "split"
	}
	ctxParam := q.Get("ctx")

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

	c, err := s.resolveChange(repo, changeKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	vcsPath := filePath
	if vcs.IsCommitMsgPath(vcsPath) {
		vcsPath = vcs.CommitMsgPath
	}

	view, err := engine.BuildChangeViewForFile(s.d, repo, rec, c, baseSel, targetSel, vcsPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var bundle *MoveBundle
	if vcsPath != vcs.CommitMsgPath {
		if b, ok := s.peekMoveBundle(rec.RootPath, view.BaseRev, view.TargetRev); ok {
			bundle = b
		} else {
			bundle = s.getMoveBundle(repo, view)
		}
	}
	fd := s.buildFileDiffData(repo, rec, c, view, vcsPath, fileIdx, baseSel, targetSel, diffMode, ctxParam, 25000, bundle)

	t, ok := s.templates["change.html"]
	if !ok {
		t, ok = s.templates["diff.html"]
	}
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = t.ExecuteTemplate(w, "diff_block", fd)
}

type movePreviewLine struct {
	Line     int    `json:"line"`
	HTML     string `json:"html"`
	Moved    bool   `json:"moved,omitempty"`
	Modified bool   `json:"modified,omitempty"`
}

type movePreviewResponse struct {
	Found         bool              `json:"found"`
	Path          string            `json:"path,omitempty"`
	Side          string            `json:"side,omitempty"`
	StartLine     int               `json:"start_line,omitempty"`
	EndLine       int               `json:"end_line,omitempty"`
	BlockModified bool              `json:"block_modified,omitempty"`
	LeftRev       string            `json:"left_rev,omitempty"`
	RightRev      string            `json:"right_rev,omitempty"`
	Rows          []movePreviewLine `json:"rows,omitempty"`
}

func (s *Server) handleAPIMovePreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	respondNotFound := func() {
		_ = json.NewEncoder(w).Encode(movePreviewResponse{Found: false})
	}

	q := r.URL.Query()
	repoSlug := q.Get("repo")
	changeKey := q.Get("change")
	baseSel := q.Get("base")
	targetSel := q.Get("target")
	filePath := q.Get("path")
	side := q.Get("side")
	if side != "L" {
		side = "R"
	}

	cleanPath := strings.TrimPrefix(filePath, "/")
	if repoSlug == "" || changeKey == "" || cleanPath == "" || cleanPath == "COMMIT_MSG" {
		respondNotFound()
		return
	}

	start, err := strconv.Atoi(q.Get("start"))
	if err != nil || start < 1 {
		respondNotFound()
		return
	}
	end, err := strconv.Atoi(q.Get("end"))
	if err != nil || end < start {
		end = start
	}

	rec, err := s.d.RepoBySlug(repoSlug)
	if err != nil || rec == nil {
		respondNotFound()
		return
	}

	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		respondNotFound()
		return
	}

	c, err := s.resolveChange(repo, changeKey)
	if err != nil {
		respondNotFound()
		return
	}

	view, err := engine.BuildChangeViewForFile(s.d, repo, rec, c, baseSel, targetSel, cleanPath)
	if err != nil {
		respondNotFound()
		return
	}

	var bundle *MoveBundle
	if b, ok := s.peekMoveBundle(rec.RootPath, view.BaseRev, view.TargetRev); ok {
		bundle = b
	} else {
		bundle = s.getMoveBundle(repo, view)
	}
	if bundle == nil {
		respondNotFound()
		return
	}

	mfd := bundle.files[cleanPath]
	if mfd == nil || mfd.isBinary || mfd.isTooLarge {
		respondNotFound()
		return
	}

	var lines []string
	if side == "L" {
		lines = mfd.baseLines
	} else {
		lines = mfd.targetLines
	}

	if len(lines) == 0 || start < 1 || start > len(lines) {
		respondNotFound()
		return
	}
	if end > len(lines) {
		end = len(lines)
	}

	ctxStart := max(1, start-3)
	ctxEnd := min(len(lines), end+3)

	type lineMeta struct {
		moved    bool
		modified bool
		intra    []diff.Span
	}
	metaByLine := make(map[int]lineMeta)
	var blockModified bool

	if side == "L" {
		for _, r := range mfd.rawRows {
			if r.LeftLine >= start && r.LeftLine <= end {
				lm := lineMeta{
					moved: r.MovedL != nil,
				}
				if r.MovedL != nil {
					lm.modified = r.MovedL.Modified
					if r.MovedL.BlockModified || r.MovedL.Modified {
						blockModified = true
					}
					if r.MovedL.Modified && len(r.MovedL.MovedIntra) > 0 {
						lm.intra = r.MovedL.MovedIntra
					}
				}
				metaByLine[r.LeftLine] = lm
			}
		}
	} else {
		for _, r := range mfd.rawRows {
			if r.RightLine >= start && r.RightLine <= end {
				lm := lineMeta{
					moved: r.MovedR != nil,
				}
				if r.MovedR != nil {
					lm.modified = r.MovedR.Modified
					if r.MovedR.BlockModified || r.MovedR.Modified {
						blockModified = true
					}
					if r.MovedR.Modified && len(r.MovedR.MovedIntra) > 0 {
						lm.intra = r.MovedR.MovedIntra
					}
				}
				metaByLine[r.RightLine] = lm
			}
		}
	}

	keep := make([]bool, len(lines))
	for i := ctxStart - 1; i < ctxEnd; i++ {
		keep[i] = true
	}
	tokens := diff.HighlightLinesKeep(lines, diff.DetectLang(cleanPath), keep)

	outRows := make([]movePreviewLine, 0, ctxEnd-ctxStart+1)
	for ln := ctxStart; ln <= ctxEnd; ln++ {
		var isMoved, isMod bool
		var intra []diff.Span
		if ln >= start && ln <= end {
			isMoved = true
			if lm, ok := metaByLine[ln]; ok {
				isMod = lm.modified
				intra = lm.intra
			}
		}
		var lineToks []diff.TokenSpan
		if ln-1 < len(tokens) {
			lineToks = tokens[ln-1]
		}
		lineHTML := string(diff.RenderTokensHTMLWS(lines[ln-1], lineToks, intra, false))
		outRows = append(outRows, movePreviewLine{
			Line:     ln,
			HTML:     lineHTML,
			Moved:    isMoved,
			Modified: isMod,
		})
	}

	resp := movePreviewResponse{
		Found:         true,
		Path:          cleanPath,
		Side:          side,
		StartLine:     start,
		EndLine:       end,
		BlockModified: blockModified,
		LeftRev:       view.BaseRev,
		RightRev:      view.TargetRev,
		Rows:          outRows,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) buildSymbolLookupRequest(r *http.Request, needBothRevs bool) (req gosym.LookupRequest, ok bool, errStatus int, errMsg string) {
	q := r.URL.Query()
	repoSlug := q.Get("repo")
	changeKey := q.Get("change")
	filePath := q.Get("path")
	pkgParam := q.Get("pkg")
	pkgFile := q.Get("pkg_file")
	side := q.Get("side")
	if side != "L" {
		side = "R"
	}
	line, _ := strconv.Atoi(q.Get("line"))
	col, _ := strconv.Atoi(q.Get("col"))
	leftRev := q.Get("left_rev")
	rightRev := q.Get("right_rev")
	baseSel := q.Get("base")
	targetSel := q.Get("target")

	cleanPath := ""
	if pkgParam != "" && pkgFile != "" {
		if !strings.HasSuffix(pkgFile, ".go") || filepath.Base(pkgFile) != pkgFile || strings.Contains(pkgFile, "..") {
			return req, false, 0, ""
		}
		if filePath != "" {
			cp := filepath.Clean(filePath)
			if strings.HasSuffix(cp, ".go") && !strings.HasPrefix(cp, "..") && !filepath.IsAbs(cp) {
				cleanPath = cp
			}
		}
	} else {
		cleanPath = filepath.Clean(filePath)
		if !strings.HasSuffix(cleanPath, ".go") || strings.HasPrefix(cleanPath, "..") || filepath.IsAbs(cleanPath) {
			return req, false, 0, ""
		}
	}
	if strings.HasPrefix(leftRev, "-") || strings.HasPrefix(rightRev, "-") {
		return req, false, 0, ""
	}

	rec, err := s.d.RepoBySlug(repoSlug)
	if err != nil || rec == nil {
		return req, false, http.StatusNotFound, "repo not found"
	}

	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		return req, false, http.StatusInternalServerError, err.Error()
	}

	baseRev := leftRev
	targetRev := rightRev

	needPrimary := (side == "L" && baseRev == "") || (side != "L" && targetRev == "")
	if needPrimary {
		c, err := s.resolveChange(repo, changeKey)
		if err != nil {
			return req, false, http.StatusNotFound, err.Error()
		}

		if cleanPath != "" {
			view, err := engine.BuildChangeViewForFile(s.d, repo, rec, c, baseSel, targetSel, cleanPath)
			if err != nil {
				return req, false, http.StatusInternalServerError, err.Error()
			}
			if baseRev == "" {
				baseRev = view.BaseRev
			}
			if targetRev == "" {
				targetRev = view.TargetRev
			}
		} else {
			view, err := engine.BuildChangeView(s.d, repo, rec, c, baseSel, targetSel)
			if err != nil {
				return req, false, http.StatusInternalServerError, err.Error()
			}
			if baseRev == "" {
				baseRev = view.BaseRev
			}
			if targetRev == "" {
				targetRev = view.TargetRev
			}
		}
	} else if needBothRevs && (baseRev == "" || targetRev == "") && changeKey != "" {
		if c, err := s.resolveChange(repo, changeKey); err == nil && c != nil {
			if cleanPath != "" {
				if view, err := engine.BuildChangeViewForFile(s.d, repo, rec, c, baseSel, targetSel, cleanPath); err == nil && view != nil {
					if baseRev == "" {
						baseRev = view.BaseRev
					}
					if targetRev == "" {
						targetRev = view.TargetRev
					}
				}
			} else {
				if view, err := engine.BuildChangeView(s.d, repo, rec, c, baseSel, targetSel); err == nil && view != nil {
					if baseRev == "" {
						baseRev = view.BaseRev
					}
					if targetRev == "" {
						targetRev = view.TargetRev
					}
				}
			}
		}
	}

	return gosym.LookupRequest{
		Repo:          repo,
		RepoRoot:      rec.RootPath,
		BaseRev:       baseRev,
		TargetRev:     targetRev,
		Side:          side,
		FilePath:      filepath.ToSlash(cleanPath),
		Line:          line,
		UTF16Col:      col,
		PkgImportPath: pkgParam,
		PkgFile:       pkgFile,
		Site:          q.Get("site") == "1",
	}, true, 0, ""
}

func (s *Server) handleAPISymbol(w http.ResponseWriter, r *http.Request) {
	req, ok, status, msg := s.buildSymbolLookupRequest(r, false)
	if !ok {
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}

	if s.sym == nil {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	res, err := s.sym.Lookup(ctx, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAPISymbolRefs(w http.ResponseWriter, r *http.Request) {
	req, ok, status, msg := s.buildSymbolLookupRequest(r, true)
	if !ok {
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}

	if s.sym == nil {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	res, err := s.sym.References(ctx, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

type symbolRefBatchItem struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Pkg     string `json:"pkg,omitempty"`
	PkgFile string `json:"pkg_file,omitempty"`
	Site    any    `json:"site,omitempty"`
}

func parseBatchSite(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case float64:
		return val != 0
	case int:
		return val != 0
	case string:
		return val == "1" || strings.EqualFold(val, "true")
	default:
		return false
	}
}

type symbolRefsBatchRequest struct {
	Repo     string               `json:"repo"`
	Change   string               `json:"change"`
	LeftRev  string               `json:"left_rev"`
	RightRev string               `json:"right_rev"`
	Base     string               `json:"base"`
	Target   string               `json:"target"`
	Side     string               `json:"side"`
	Requests []symbolRefBatchItem `json:"requests"`
}

type symbolRefsBatchResponse struct {
	Results []*gosym.RefsResult `json:"results"`
}

func (s *Server) handleAPISymbolRefsBatch(w http.ResponseWriter, r *http.Request) {
	var payload symbolRefsBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if payload.Repo == "" {
		payload.Repo = r.URL.Query().Get("repo")
	}
	if payload.Change == "" {
		payload.Change = r.URL.Query().Get("change")
	}
	if payload.LeftRev == "" {
		payload.LeftRev = r.URL.Query().Get("left_rev")
	}
	if payload.RightRev == "" {
		payload.RightRev = r.URL.Query().Get("right_rev")
	}
	if payload.Base == "" {
		payload.Base = r.URL.Query().Get("base")
	}
	if payload.Target == "" {
		payload.Target = r.URL.Query().Get("target")
	}
	if payload.Side == "" {
		payload.Side = r.URL.Query().Get("side")
	}
	if len(payload.Requests) == 0 {
		writeJSON(w, http.StatusOK, symbolRefsBatchResponse{Results: []*gosym.RefsResult{}})
		return
	}
	if len(payload.Requests) > 256 {
		http.Error(w, "too many requests in batch (max 256)", http.StatusBadRequest)
		return
	}

	rec, err := s.d.RepoBySlug(payload.Repo)
	if err != nil || rec == nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}

	repo, err := vcs.Open(rec.RootPath, s.cache)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	baseRev := payload.LeftRev
	targetRev := payload.RightRev
	side := payload.Side
	if side != "L" {
		side = "R"
	}

	needPrimary := (side == "L" && baseRev == "") || (side != "L" && targetRev == "")
	if needPrimary || ((baseRev == "" || targetRev == "") && payload.Change != "") {
		if c, err := s.resolveChange(repo, payload.Change); err == nil && c != nil {
			if view, err := engine.BuildChangeView(s.d, repo, rec, c, payload.Base, payload.Target); err == nil && view != nil {
				if baseRev == "" {
					baseRev = view.BaseRev
				}
				if targetRev == "" {
					targetRev = view.TargetRev
				}
			}
		}
	}

	if s.sym == nil {
		results := make([]*gosym.RefsResult, len(payload.Requests))
		for i := range results {
			results[i] = &gosym.RefsResult{Found: false}
		}
		writeJSON(w, http.StatusOK, symbolRefsBatchResponse{Results: results})
		return
	}

	reqs := make([]gosym.LookupRequest, len(payload.Requests))
	for i, item := range payload.Requests {
		cleanPath := ""
		if item.Pkg != "" && item.PkgFile != "" {
			if strings.HasSuffix(item.PkgFile, ".go") && filepath.Base(item.PkgFile) == item.PkgFile && !strings.Contains(item.PkgFile, "..") {
				if item.File != "" {
					cp := filepath.Clean(item.File)
					if strings.HasSuffix(cp, ".go") && !strings.HasPrefix(cp, "..") && !filepath.IsAbs(cp) {
						cleanPath = cp
					}
				}
			}
		} else {
			cp := filepath.Clean(item.File)
			if strings.HasSuffix(cp, ".go") && !strings.HasPrefix(cp, "..") && !filepath.IsAbs(cp) {
				cleanPath = cp
			}
		}

		reqs[i] = gosym.LookupRequest{
			Repo:          repo,
			RepoRoot:      rec.RootPath,
			BaseRev:       baseRev,
			TargetRev:     targetRev,
			Side:          side,
			FilePath:      filepath.ToSlash(cleanPath),
			Line:          item.Line,
			UTF16Col:      item.Col,
			PkgImportPath: item.Pkg,
			PkgFile:       item.PkgFile,
			Site:          parseBatchSite(item.Site),
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	results, err := s.sym.ReferencesBatch(ctx, reqs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, symbolRefsBatchResponse{Results: results})
}

func (s *Server) handleAPISymbolImpls(w http.ResponseWriter, r *http.Request) {
	req, ok, status, msg := s.buildSymbolLookupRequest(r, true)
	if !ok {
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}

	if s.sym == nil {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	res, err := s.sym.Implementations(ctx, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAPIFormDrafts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		repoSlug := r.URL.Query().Get("repo")
		changeKey := r.URL.Query().Get("change")

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

		c, err := s.resolveChange(repo, changeKey)
		if err != nil || c == nil {
			http.Error(w, "change not found", http.StatusNotFound)
			return
		}

		drafts, err := s.d.FormDraftsForChange(rec.ID, c.Key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if drafts == nil {
			drafts = []*db.FormDraft{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"drafts": drafts,
		})

	case http.MethodPost:
		var req struct {
			RepoSlug   string `json:"repo"`
			ChangeKey  string `json:"change"`
			Action     string `json:"action"`
			DraftKey   string `json:"draft_key"`
			TargetPath string `json:"target_path"`
			Side       string `json:"side"`
			Line       int    `json:"line"`
			EndLine    int    `json:"end_line"`
			StartCol   int    `json:"start_col"`
			EndCol     int    `json:"end_col"`
			Unresolved *bool  `json:"unresolved"`
			ThreadID   string `json:"thread_id"`
			MessageID  string `json:"message_id"`
			Content    string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.RepoSlug == "" {
			req.RepoSlug = r.URL.Query().Get("repo")
		}
		if req.ChangeKey == "" {
			req.ChangeKey = r.URL.Query().Get("change")
		}

		rec, err := s.d.RepoBySlug(req.RepoSlug)
		if err != nil || rec == nil {
			http.Error(w, "repo not found", http.StatusNotFound)
			return
		}

		repo, err := vcs.Open(rec.RootPath, s.cache)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		c, err := s.resolveChange(repo, req.ChangeKey)
		if err != nil || c == nil {
			http.Error(w, "change not found", http.StatusNotFound)
			return
		}

		if req.Action == "delete" || strings.TrimSpace(req.Content) == "" {
			if req.MessageID != "" && req.DraftKey == "" {
				_ = s.d.DeleteFormDraftForMessage(rec.ID, c.Key, req.MessageID)
			} else if req.ThreadID != "" && req.DraftKey == "" {
				_ = s.d.DeleteFormDraftsForThread(rec.ID, c.Key, req.ThreadID)
			} else if req.DraftKey != "" {
				_ = s.d.DeleteFormDraft(rec.ID, c.Key, req.DraftKey)
			}
		} else {
			if req.DraftKey != "" {
				unresolved := true
				if req.Unresolved != nil {
					unresolved = *req.Unresolved
				}
				if err := s.d.SaveFormDraft(rec.ID, c.Key, req.DraftKey, req.TargetPath, req.Side, req.Line, req.EndLine, req.StartCol, req.EndCol, unresolved, req.ThreadID, req.MessageID, req.Content); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
