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

// SSEBroker handles Server-Sent Events fan-out to connected browser clients.
type SSEBroker struct {
	mu            sync.Mutex
	clients       map[chan string]bool
	clientOrder   []chan string
	d             *db.DB
	cache         *vcs.Cache
	lastSignature string
	stopCh        chan struct{}
}

// NewSSEBroker creates and starts an SSE broker with background zero-subprocess VCS watching.
func NewSSEBroker(d *db.DB, cache *vcs.Cache) *SSEBroker {
	b := &SSEBroker{
		clients: make(map[chan string]bool),
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
	for ch := range b.clients {
		close(ch)
		delete(b.clients, ch)
	}
	b.clientOrder = nil
}

// StateSignature returns the first 16 hex chars of SHA-256(b.d.StateDigest() + ":" + b.currentVCSSignature()).
func (b *SSEBroker) StateSignature() string {
	var dbDig string
	if b.d != nil {
		dbDig = b.d.StateDigest()
	}
	vcsSig := b.currentVCSSignature()
	combined := dbDig + ":" + vcsSig
	sum := sha256.Sum256([]byte(combined))
	hexStr := hex.EncodeToString(sum[:])
	if len(hexStr) > 16 {
		return hexStr[:16]
	}
	return hexStr
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

// InitLastSignature sets lastSignature only if it is currently empty.
func (b *SSEBroker) InitLastSignature(sig string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastSignature == "" {
		b.lastSignature = sig
	}
}

// Broadcast sends an SSE message to all connected clients.
func (b *SSEBroker) Broadcast(event string, payload any) {
	if event == "reload" {
		sig := b.StateSignature()
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
		m["sig"] = sig
		payload = m
	}

	dataBytes, _ := json.Marshal(payload)
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(dataBytes))

	b.mu.Lock()
	defer b.mu.Unlock()

	for ch := range b.clients {
		select {
		case ch <- msg:
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

			sig := b.StateSignature()
			b.mu.Lock()
			prev := b.lastSignature
			if prev == "" {
				b.lastSignature = sig
				b.mu.Unlock()
				continue
			}
			changed := (sig != prev)
			if changed {
				b.lastSignature = sig
			}
			b.mu.Unlock()

			if changed {
				b.Broadcast("reload", map[string]string{"reason": "vcs"})
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

	msgCh := make(chan string, 16)
	b.mu.Lock()
	for len(b.clientOrder) >= 64 {
		oldest := b.clientOrder[0]
		b.clientOrder = b.clientOrder[1:]
		if b.clients[oldest] {
			delete(b.clients, oldest)
			close(oldest)
		}
	}
	b.clients[msgCh] = true
	b.clientOrder = append(b.clientOrder, msgCh)
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		if b.clients[msgCh] {
			delete(b.clients, msgCh)
			close(msgCh)
		}
		for i, ch := range b.clientOrder {
			if ch == msgCh {
				b.clientOrder = append(b.clientOrder[:i], b.clientOrder[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
	}()

	// Send initial connected event
	connPayload, _ := json.Marshal(map[string]string{"sig": b.StateSignature()})
	fmt.Fprintf(w, "event: connected\ndata: %s\n\n", string(connPayload))
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgCh:
			if !ok {
				return
			}
			_, _ = fmt.Fprint(w, msg)
			flusher.Flush()
		}
	}
}
