package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nicholas.sh/cmd/review/internal/db"
)

// ServerInfo records the address and process ID of the running review daemon.
type ServerInfo struct {
	Addr string `json:"addr"`
	PID  int    `json:"pid"`
}

// ConfigDir returns the configuration directory for a given database path,
// or the default ~/.config/review directory.
func ConfigDir(dbPath string) string {
	if dbPath != "" {
		return filepath.Dir(dbPath)
	}
	return filepath.Dir(db.DefaultPath())
}

// GetServerInfo reads server.json from configDir.
func GetServerInfo(configDir string) (*ServerInfo, error) {
	file := filepath.Join(configDir, "server.json")
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var info ServerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// WriteServerInfo writes server.json to configDir.
func WriteServerInfo(configDir, addr string, pid int) error {
	_ = os.MkdirAll(configDir, 0755)
	info := ServerInfo{Addr: addr, PID: pid}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(configDir, "server.json"), data, 0644)
}

// RemoveServerInfo deletes server.json from configDir.
func RemoveServerInfo(configDir string) error {
	return os.Remove(filepath.Join(configDir, "server.json"))
}

// IsAlive checks whether a process with the given PID is currently alive.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// DialAddr converts an address with wildcard/empty host into one suitable for dialing (e.g. localhost).
func DialAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		return net.JoinHostPort("localhost", port)
	}
	return addr
}

// CheckHealth queries /healthz on the server address with a 200ms timeout.
func CheckHealth(addr string) bool {
	url := fmt.Sprintf("http://%s/healthz", DialAddr(addr))
	client := http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Notify sends a POST request to /api/notify on the running server, waking up SSE clients.
// Silently ignored if server is not running or unreachable.
func Notify(dbPath string) {
	configDir := ConfigDir(dbPath)
	info, err := GetServerInfo(configDir)
	if err != nil || info == nil || !IsAlive(info.PID) {
		return
	}

	url := fmt.Sprintf("http://%s/api/notify", DialAddr(info.Addr))
	client := http.Client{Timeout: 200 * time.Millisecond}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"reason":"cli"}`))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}
}

// Stop terminates any running review daemon in configDir.
func Stop(configDir string) (*ServerInfo, error) {
	info, err := GetServerInfo(configDir)
	if err != nil || info == nil {
		return nil, nil
	}

	if IsAlive(info.PID) {
		process, err := os.FindProcess(info.PID)
		if err == nil {
			_ = process.Signal(syscall.SIGTERM)
			// Wait up to 3 seconds for exit
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if !IsAlive(info.PID) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if IsAlive(info.PID) {
				_ = process.Signal(syscall.SIGKILL)
			}
		}
	}

	_ = RemoveServerInfo(configDir)
	return info, nil
}

// StartBackground spawns the review daemon in the background.
func StartBackground(binPath, dbPath, addr string) (*ServerInfo, error) {
	configDir := ConfigDir(dbPath)
	info, err := GetServerInfo(configDir)
	if err == nil && info != nil && IsAlive(info.PID) {
		return info, nil
	}

	_ = os.MkdirAll(configDir, 0755)
	logFile, err := os.OpenFile(filepath.Join(configDir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("creating server.log: %w", err)
	}

	args := []string{"-db", dbPath, "-foreground"}
	if addr != "" {
		args = append([]string{"-a", addr}, args...)
	}
	cmd := exec.Command(binPath, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("spawning daemon: %w", err)
	}

	pid := cmd.Process.Pid
	_ = logFile.Close()

	effectiveAddr := addr
	if effectiveAddr == "" {
		effectiveAddr = "localhost:2626"
	}
	checkAddr := DialAddr(effectiveAddr)

	// Wait up to 3 seconds for server to be healthy
	deadline := time.Now().Add(3 * time.Second)
	healthy := false
	for time.Now().Before(deadline) {
		if curInfo, err := GetServerInfo(configDir); err == nil && curInfo != nil && curInfo.Addr != "" {
			checkAddr = DialAddr(curInfo.Addr)
		}
		if CheckHealth(checkAddr) {
			healthy = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !healthy {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("daemon failed to start or pass /healthz within 3 seconds; check %s", filepath.Join(configDir, "server.log"))
	}

	if writtenInfo, err := GetServerInfo(configDir); err == nil && writtenInfo != nil && IsAlive(writtenInfo.PID) {
		return writtenInfo, nil
	}

	_ = WriteServerInfo(configDir, effectiveAddr, pid)
	return &ServerInfo{Addr: effectiveAddr, PID: pid}, nil
}
