package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/vcs"
)

// StateSnapshot captures the overall and per-category signatures for a page scope.
type StateSnapshot struct {
	Sig       string `json:"sig"`
	VCS       string `json:"vcs,omitempty"`
	Comments  string `json:"comments,omitempty"`
	Snapshots string `json:"snapshots,omitempty"`
	Marks     string `json:"marks,omitempty"`
	Repos     string `json:"repos,omitempty"`
}

type sseClient struct {
	ch        chan string
	repoSlug  string
	changeKey string
}

// SSEBroker handles Server-Sent Events fan-out to connected browser clients.
type SSEBroker struct {
	mu           sync.Mutex
	clients      map[*sseClient]bool
	clientOrder  []*sseClient
	d            *db.DB
	cache        *vcs.Cache
	lastDBDigest string
	lastRepoVCS  map[string]string
	stopCh       chan struct{}
}

// NewSSEBroker creates and starts an SSE broker with background zero-subprocess VCS watching.
func NewSSEBroker(d *db.DB, cache *vcs.Cache) *SSEBroker {
	b := &SSEBroker{
		clients: make(map[*sseClient]bool),
		d:       d,
		cache:   cache,
		stopCh:  make(chan struct{}),
	}
	go b.watchLoop()
	return b
}

// Close terminates the broker and background watching goroutine.
func (b *SSEBroker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.stopCh:
	default:
		close(b.stopCh)
	}
	for cl := range b.clients {
		close(cl.ch)
		delete(b.clients, cl)
	}
	b.clientOrder = nil
}

// StateSignature returns the first 16 hex chars of SHA-256(b.d.StateDigest() + ":" + b.currentVCSSignature()).
func (b *SSEBroker) StateSignature() string {
	return b.StateFor("", "").Sig
}

// StateFor computes a semantic state signature and component digests scoped to the
// entire server (repoSlug == ""), a single repository (changeKey == ""), or a single change.
func (b *SSEBroker) StateFor(repoSlug, changeKey string) StateSnapshot {
	if repoSlug == "" {
		var dbDig string
		var parts db.StateParts
		if b.d != nil {
			dbDig, parts = b.d.StateDigestFor(0, "")
		}
		vcsSig := b.currentVCSSignature()
		return buildStateSnapshot(dbDig, vcsSig, parts)
	}

	if b.d == nil {
		return StateSnapshot{}
	}
	rec, err := b.d.RepoBySlug(repoSlug)
	if err != nil || rec == nil {
		return buildStateSnapshot("missing:"+repoSlug, "", db.StateParts{Repos: "missing"})
	}

	dbDig, parts := b.d.StateDigestFor(rec.ID, changeKey)
	vcsSig := ""
	if changeKey != "" {
		vcsSig = b.changeVCSSignature(rec.RootPath, changeKey)
	} else if b.cache != nil {
		vcsSig = b.cache.WatchSignature([]string{rec.RootPath})
	}
	return buildStateSnapshot(dbDig, vcsSig, parts)
}

func buildStateSnapshot(dbDig, vcsSig string, parts db.StateParts) StateSnapshot {
	sum := sha256.Sum256([]byte(dbDig + ":" + vcsSig))
	sig := hex.EncodeToString(sum[:])[:16]
	shortVCS := vcsSig
	if len(shortVCS) > 16 {
		vSum := sha256.Sum256([]byte(vcsSig))
		shortVCS = hex.EncodeToString(vSum[:])[:16]
	}
	return StateSnapshot{
		Sig:       sig,
		VCS:       shortVCS,
		Comments:  parts.Comments,
		Snapshots: parts.Snapshots,
		Marks:     parts.Marks,
		Repos:     parts.Repos,
	}
}

func (b *SSEBroker) changeVCSSignature(rootPath, changeKey string) string {
	repo, err := vcs.Open(rootPath, b.cache)
	if err != nil {
		if b.cache != nil {
			return b.cache.WatchSignature([]string{rootPath})
		}
		return ""
	}
	if changes, err := repo.Changes(); err == nil {
		if ch := vcs.ResolveChange(changes, changeKey); ch != nil {
			return ch.Rev + ":" + ch.Parent
		}
	}
	if ch, err := repo.Commit(changeKey); err == nil && ch != nil {
		return ch.Rev + ":" + ch.Parent
	}
	return "gone"
}

func (b *SSEBroker) currentVCSSignature() string {
	if b.d == nil || b.cache == nil {
		return ""
	}
	repos, err := b.d.Repos()
	if err != nil || len(repos) == 0 {
		return ""
	}
	var roots []string
	for _, r := range repos {
		roots = append(roots, r.RootPath)
	}
	return b.cache.WatchSignature(roots)
}

func (b *SSEBroker) snapshotWatchState() (string, map[string]string) {
	var dbDig string
	if b.d != nil {
		dbDig = b.d.StateDigest()
	}
	repoVCS := make(map[string]string)
	if b.d != nil && b.cache != nil {
		if repos, err := b.d.Repos(); err == nil {
			for _, r := range repos {
				repoVCS[r.Slug] = b.cache.WatchSignature([]string{r.RootPath})
			}
		}
	}
	return dbDig, repoVCS
}

// InitBaseline initializes the watch baselines if they have not been populated yet.
func (b *SSEBroker) InitBaseline() {
	b.mu.Lock()
	ready := b.lastDBDigest != "" && b.lastRepoVCS != nil
	b.mu.Unlock()
	if ready {
		return
	}
	dbDig, repoVCS := b.snapshotWatchState()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastDBDigest == "" {
		b.lastDBDigest = dbDig
	}
	if b.lastRepoVCS == nil {
		b.lastRepoVCS = repoVCS
	}
}

// Broadcast sends an SSE message to matching connected clients.
func (b *SSEBroker) Broadcast(event string, payload any) {
	b.mu.Lock()
	hasClients := len(b.clients) > 0
	b.mu.Unlock()

	if !hasClients {
		if event == "reload" && b.d != nil {
			dbDig := b.d.StateDigest()
			b.mu.Lock()
			b.lastDBDigest = dbDig
			b.mu.Unlock()
		}
		return
	}

	var evtRepo, evtChange string
	if event == "reload" {
		var dbDig string
		var parts db.StateParts
		if b.d != nil {
			dbDig, parts = b.d.StateDigestFor(0, "")
		}
		sig := buildStateSnapshot(dbDig, b.currentVCSSignature(), parts).Sig
		m := make(map[string]any)
		if payload != nil {
			switch p := payload.(type) {
			case map[string]string:
				for k, v := range p {
					m[k] = v
				}
			case map[string]any:
				for k, v := range p {
					m[k] = v
				}
			default:
				if bts, err := json.Marshal(p); err == nil {
					_ = json.Unmarshal(bts, &m)
				}
			}
		}
		if r, ok := m["repo"].(string); ok {
			evtRepo = r
		}
		if c, ok := m["change"].(string); ok {
			evtChange = c
		}
		m["sig"] = sig
		payload = m

		if b.d != nil {
			b.mu.Lock()
			b.lastDBDigest = dbDig
			b.mu.Unlock()
		}
	}

	dataBytes, _ := json.Marshal(payload)
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(dataBytes))

	b.mu.Lock()
	defer b.mu.Unlock()

	for cl := range b.clients {
		if event == "reload" {
			if evtRepo != "" && cl.repoSlug != "" && cl.repoSlug != evtRepo {
				continue
			}
			if evtChange != "" && cl.changeKey != "" && cl.changeKey != evtChange {
				continue
			}
		}
		select {
		case cl.ch <- msg:
		default:
			// Non-blocking drop if client is saturated
		}
	}
}

func (b *SSEBroker) watchLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-b.stopCh:
			return
		case <-pingTicker.C:
			b.Broadcast("ping", map[string]int64{"time": time.Now().Unix()})
		case <-ticker.C:
			b.mu.Lock()
			numClients := len(b.clients)
			b.mu.Unlock()
			if numClients == 0 {
				continue
			}

			dbDig, repoVCS := b.snapshotWatchState()

			b.mu.Lock()
			dbChanged := false
			if b.lastDBDigest == "" {
				b.lastDBDigest = dbDig
			} else if dbDig != b.lastDBDigest {
				b.lastDBDigest = dbDig
				dbChanged = true
			}

			var changedRepos []string
			if b.lastRepoVCS == nil {
				b.lastRepoVCS = repoVCS
			} else {
				for slug, vSig := range repoVCS {
					if prev, ok := b.lastRepoVCS[slug]; !ok || prev != vSig {
						changedRepos = append(changedRepos, slug)
					}
				}
				for slug := range b.lastRepoVCS {
					if _, ok := repoVCS[slug]; !ok {
						changedRepos = append(changedRepos, slug)
					}
				}
				if len(changedRepos) > 0 {
					b.lastRepoVCS = repoVCS
				}
			}
			b.mu.Unlock()

			if dbChanged {
				b.Broadcast("reload", map[string]string{"reason": "db"})
			}
			for _, slug := range changedRepos {
				b.Broadcast("reload", map[string]string{"reason": "vcs", "repo": slug})
			}
		}
	}
}

// ServeHTTP handles GET /api/events streaming SSE connections.
func (b *SSEBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	repoSlug := r.URL.Query().Get("repo")
	changeKey := r.URL.Query().Get("change")
	cl := &sseClient{
		ch:        make(chan string, 16),
		repoSlug:  repoSlug,
		changeKey: changeKey,
	}

	b.mu.Lock()
	select {
	case <-b.stopCh:
		b.mu.Unlock()
		return
	default:
	}
	for len(b.clientOrder) >= 64 {
		oldest := b.clientOrder[0]
		b.clientOrder = b.clientOrder[1:]
		if b.clients[oldest] {
			delete(b.clients, oldest)
			close(oldest.ch)
		}
	}
	b.clients[cl] = true
	b.clientOrder = append(b.clientOrder, cl)
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		if b.clients[cl] {
			delete(b.clients, cl)
			close(cl.ch)
		}
		for i, item := range b.clientOrder {
			if item == cl {
				b.clientOrder = append(b.clientOrder[:i], b.clientOrder[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
	}()

	b.InitBaseline()

	// Send initial connected event
	connPayload, _ := json.Marshal(map[string]string{"sig": b.StateFor(repoSlug, changeKey).Sig})
	fmt.Fprintf(w, "event: connected\ndata: %s\n\n", string(connPayload))
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-cl.ch:
			if !ok {
				return
			}
			_, _ = fmt.Fprint(w, msg)
			flusher.Flush()
		}
	}
}
