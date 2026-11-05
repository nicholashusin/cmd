package ui

import (
	"encoding/json"
	"runtime"
	"strings"
	"sync"

	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/engine"
	"nicholas.sh/cmd/review/internal/vcs"
)

type moveCacheKey struct {
	repoRoot  string
	baseRev   string
	targetRev string
}

type moveFileData struct {
	baseData    []byte
	targetData  []byte
	baseLines   []string
	targetLines []string
	rawRows     []diff.Row
	isBinary    bool
	isTooLarge  bool

	scopesOnce  sync.Once
	scopesSide  string
	scopesJSON  string
	scopesLJSON string
	scopesRJSON string
}

func extractFileScopes(cleanPath string, baseData []byte, baseLines []string, targetData []byte, targetLines []string) (side, scopesJSON, scopesLJSON, scopesRJSON string) {
	var rJSON, lJSON string
	if len(targetLines) > 0 {
		rSpans := diff.ExtractScopes(cleanPath, targetData, targetLines)
		if len(rSpans) > 0 {
			if b, err := json.Marshal(rSpans); err == nil {
				rJSON = string(b)
			}
		}
	}
	if len(baseLines) > 0 {
		lSpans := diff.ExtractScopes(cleanPath, baseData, baseLines)
		if len(lSpans) > 0 {
			if b, err := json.Marshal(lSpans); err == nil {
				lJSON = string(b)
			}
		}
	}
	side = "R"
	scopesJSON = rJSON
	if rJSON == "" && lJSON != "" {
		side = "L"
		scopesJSON = lJSON
	}
	return side, scopesJSON, lJSON, rJSON
}

func (mfd *moveFileData) scopes(cleanPath string) (side, scopesJSON, scopesLJSON, scopesRJSON string) {
	mfd.scopesOnce.Do(func() {
		mfd.scopesSide, mfd.scopesJSON, mfd.scopesLJSON, mfd.scopesRJSON = extractFileScopes(
			cleanPath,
			mfd.baseData,
			mfd.baseLines,
			mfd.targetData,
			mfd.targetLines,
		)
	})
	return mfd.scopesSide, mfd.scopesJSON, mfd.scopesLJSON, mfd.scopesRJSON
}

func (mfd *moveFileData) cloneRows() []diff.Row {
	if len(mfd.rawRows) == 0 {
		return nil
	}
	cp := make([]diff.Row, len(mfd.rawRows))
	copy(cp, mfd.rawRows)
	return cp
}

// MoveBundle caches precomputed file blobs, Myers diff rows, and move detections
// for a single change view between baseRev and targetRev.
type MoveBundle struct {
	files map[string]*moveFileData // keyed by clean slash path
}

type moveCache struct {
	mu      sync.Mutex
	entries map[moveCacheKey]*MoveBundle
	order   []moveCacheKey
}

func newMoveCache() *moveCache {
	return &moveCache{
		entries: make(map[moveCacheKey]*MoveBundle),
	}
}

func (c *moveCache) Get(key moveCacheKey) (*MoveBundle, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	b, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, key)
			break
		}
	}
	return b, true
}

func (c *moveCache) Put(key moveCacheKey, bundle *MoveBundle) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.entries[key]; ok {
		c.entries[key] = bundle
		return
	}

	const maxEntries = 16
	for len(c.order) >= maxEntries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}

	c.entries[key] = bundle
	c.order = append(c.order, key)
}

func (c *moveCache) EvictRepo(repoRoot string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var newOrder []moveCacheKey
	for _, k := range c.order {
		if k.repoRoot == repoRoot {
			delete(c.entries, k)
		} else {
			newOrder = append(newOrder, k)
		}
	}
	c.order = newOrder
}

func (s *Server) evictMoveCache(repoRoot string) {
	if s.moveCache != nil && repoRoot != "" {
		s.moveCache.EvictRepo(repoRoot)
	}
}

func (s *Server) peekMoveBundle(repoRoot, baseRev, targetRev string) (*MoveBundle, bool) {
	if s.moveCache == nil || repoRoot == "" || !vcs.IsImmutableHexRev(baseRev) || !vcs.IsImmutableHexRev(targetRev) {
		return nil, false
	}
	key := moveCacheKey{
		repoRoot:  repoRoot,
		baseRev:   baseRev,
		targetRev: targetRev,
	}
	return s.moveCache.Get(key)
}

func (s *Server) getMoveBundle(repo vcs.Repo, view *engine.ChangeView) *MoveBundle {
	if repo == nil || view == nil {
		return nil
	}
	repoRoot := ""
	if view.RepoRecord != nil {
		repoRoot = view.RepoRecord.RootPath
	}
	canCache := repoRoot != "" && vcs.IsImmutableHexRev(view.BaseRev) && vcs.IsImmutableHexRev(view.TargetRev)
	key := moveCacheKey{
		repoRoot:  repoRoot,
		baseRev:   view.BaseRev,
		targetRev: view.TargetRev,
	}

	if canCache && s.moveCache != nil {
		if b, ok := s.moveCache.Get(key); ok {
			return b
		}
	}

	bundle := s.computeMoveBundle(repo, view)
	if canCache && s.moveCache != nil && bundle != nil {
		s.moveCache.Put(key, bundle)
	}
	return bundle
}

type fileDiffResult struct {
	cleanPath  string
	isBinary   bool
	isTooLarge bool
	mfd        *moveFileData
	moveFile   *diff.MoveFile
}

func (s *Server) computeMoveBundle(repo vcs.Repo, view *engine.ChangeView) *MoveBundle {
	var filesToProcess []*vcs.File
	if len(view.AllFiles) > 0 {
		filesToProcess = view.AllFiles
	} else {
		for _, vf := range view.Files {
			if vf != nil && vf.File != nil {
				filesToProcess = append(filesToProcess, vf.File)
			}
		}
	}

	bundle := &MoveBundle{
		files: make(map[string]*moveFileData),
	}

	var validFiles []*vcs.File
	for _, f := range filesToProcess {
		if f != nil && f.Path != vcs.CommitMsgPath {
			validFiles = append(validFiles, f)
		}
	}
	if len(validFiles) == 0 {
		return bundle
	}

	results := make([]fileDiffResult, len(validFiles))
	processFile := func(idx int) {
		f := validFiles[idx]
		cleanPath := strings.TrimPrefix(f.Path, "/")

		baseData, _ := repo.Content(view.BaseRev, f.Path)
		targetData, _ := repo.Content(view.TargetRev, f.Path)

		if diff.IsBinary(baseData) || diff.IsBinary(targetData) {
			results[idx] = fileDiffResult{cleanPath: cleanPath, isBinary: true}
			return
		}

		baseLines := diff.SplitLines(baseData)
		targetLines := diff.SplitLines(targetData)

		if len(baseLines) > 25000 || len(targetLines) > 25000 {
			results[idx] = fileDiffResult{cleanPath: cleanPath, isTooLarge: true}
			return
		}

		edits := diff.Edits(baseLines, targetLines)
		rawRows := diff.BuildDiffRows(baseLines, targetLines, edits)

		mfd := &moveFileData{
			baseData:    baseData,
			targetData:  targetData,
			baseLines:   baseLines,
			targetLines: targetLines,
			rawRows:     rawRows,
		}
		mf := &diff.MoveFile{
			Path:       cleanPath,
			LeftLines:  baseLines,
			RightLines: targetLines,
			Rows:       rawRows,
		}
		results[idx] = fileDiffResult{
			cleanPath: cleanPath,
			mfd:       mfd,
			moveFile:  mf,
		}
	}

	if len(validFiles) < 4 {
		for i := range validFiles {
			processFile(i)
		}
	} else {
		numWorkers := min(len(validFiles), max(1, runtime.NumCPU()), 8)
		ch := make(chan int, len(validFiles))
		for i := range validFiles {
			ch <- i
		}
		close(ch)

		var wg sync.WaitGroup
		wg.Add(numWorkers)
		for w := 0; w < numWorkers; w++ {
			go func() {
				defer wg.Done()
				for idx := range ch {
					processFile(idx)
				}
			}()
		}
		wg.Wait()
	}

	var moveFiles []diff.MoveFile
	var cleanPaths []string

	for _, res := range results {
		if res.cleanPath == "" {
			continue
		}
		if res.isBinary {
			bundle.files[res.cleanPath] = &moveFileData{isBinary: true}
			continue
		}
		if res.isTooLarge {
			bundle.files[res.cleanPath] = &moveFileData{isTooLarge: true}
			continue
		}
		if res.mfd != nil {
			bundle.files[res.cleanPath] = res.mfd
			if res.moveFile != nil {
				moveFiles = append(moveFiles, *res.moveFile)
				cleanPaths = append(cleanPaths, res.cleanPath)
			}
		}
	}

	if len(moveFiles) > 0 {
		diff.DetectMoves(moveFiles)
		for i, cp := range cleanPaths {
			if bundle.files[cp] != nil {
				bundle.files[cp].rawRows = moveFiles[i].Rows
			}
		}
	}

	return bundle
}
