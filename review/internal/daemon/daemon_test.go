package daemon

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestServerInfoAndStop(t *testing.T) {
	configDir := t.TempDir()

	// Spawn a real long-running process (sleep 30)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid

	err := WriteServerInfo(configDir, "localhost:9999", pid)
	if err != nil {
		t.Fatal(err)
	}

	info, err := GetServerInfo(configDir)
	if err != nil || info == nil || info.PID != pid {
		t.Fatalf("GetServerInfo failed: %v", info)
	}

	if !IsAlive(pid) {
		t.Errorf("expected process %d to be alive", pid)
	}

	stopped, err := Stop(configDir)
	if err != nil || stopped == nil || stopped.PID != pid {
		t.Fatalf("Stop failed: %v", err)
	}

	// Reap child process so zombie status is cleared
	_ = cmd.Wait()

	if IsAlive(pid) {
		t.Errorf("expected process %d to be stopped", pid)
	}

	// server.json should be removed
	_, err = GetServerInfo(configDir)
	if err == nil {
		t.Errorf("expected server.json to be removed")
	}
}

func TestNotify(t *testing.T) {
	var notified atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/notify" && r.Method == http.MethodPost {
			notified.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	configDir := t.TempDir()
	dbPath := filepath.Join(configDir, "review.db")

	// Dummy alive process: current test process PID
	myPID := exec.Command("true").Process
	_ = myPID

	// Write server.json pointing to test server
	// Strip "http://" from server.URL for addr
	addr := server.Listener.Addr().String()
	cmd := exec.Command("sleep", "5")
	_ = cmd.Start()
	defer func() {
		_ = cmd.Process.Kill()
	}()

	_ = WriteServerInfo(configDir, addr, cmd.Process.Pid)

	Notify(dbPath)

	if notified.Load() != 1 {
		t.Errorf("expected 1 notify request, got %d", notified.Load())
	}
}
