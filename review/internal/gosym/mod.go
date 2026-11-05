package gosym

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"nicholas.sh/cmd/review/internal/vcs"
)

type goEnv struct {
	goroot     string
	gomodcache string
	gopath     string
	goos       string
	goarch     string
}

var (
	cachedEnv       goEnv
	cachedEnvOnce   sync.Once
	testEnvOverride *goEnv
	testEnvMu       sync.RWMutex
)

func getGoEnv() goEnv {
	testEnvMu.RLock()
	if testEnvOverride != nil {
		e := *testEnvOverride
		testEnvMu.RUnlock()
		return e
	}
	testEnvMu.RUnlock()

	cachedEnvOnce.Do(func() {
		cachedEnv = detectGoEnv()
	})
	return cachedEnv
}

func setTestGoEnv(env *goEnv) func() {
	testEnvMu.Lock()
	old := testEnvOverride
	testEnvOverride = env
	testEnvMu.Unlock()
	return func() {
		testEnvMu.Lock()
		testEnvOverride = old
		testEnvMu.Unlock()
	}
}

func detectGoEnv() goEnv {
	env := goEnv{
		goos:   runtime.GOOS,
		goarch: runtime.GOARCH,
	}
	if v := os.Getenv("GOOS"); v != "" {
		env.goos = v
	}
	if v := os.Getenv("GOARCH"); v != "" {
		env.goarch = v
	}

	// Fast path without spawning subprocesses when GOROOT is already available
	if r := os.Getenv("GOROOT"); r != "" && dirExists(filepath.Join(r, "src", "runtime")) {
		env.goroot = r
	} else if r := runtime.GOROOT(); r != "" && dirExists(filepath.Join(r, "src", "runtime")) {
		env.goroot = r
	}

	if gp := os.Getenv("GOPATH"); gp != "" {
		env.gopath = gp
	} else if home, err := os.UserHomeDir(); err == nil {
		env.gopath = filepath.Join(home, "go")
	}

	if mc := os.Getenv("GOMODCACHE"); mc != "" {
		env.gomodcache = mc
	} else if env.gopath != "" {
		env.gomodcache = filepath.Join(env.gopath, "pkg", "mod")
	}

	if env.goroot != "" {
		return env
	}

	goBin := ""
	if p, err := exec.LookPath("go"); err == nil {
		goBin = p
	} else if r := runtime.GOROOT(); r != "" {
		p := filepath.Join(r, "bin", "go")
		if _, err := os.Stat(p); err == nil {
			goBin = p
		}
	} else if p := "/usr/local/go/bin/go"; fileExists(p) {
		goBin = p
	}

	if goBin != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, goBin, "env", "-json", "GOROOT", "GOMODCACHE", "GOPATH", "GOOS", "GOARCH")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			var raw map[string]string
			if err := json.Unmarshal(out.Bytes(), &raw); err == nil {
				if raw["GOROOT"] != "" && dirExists(filepath.Join(raw["GOROOT"], "src", "runtime")) {
					env.goroot = raw["GOROOT"]
				}
				if raw["GOMODCACHE"] != "" {
					env.gomodcache = raw["GOMODCACHE"]
				}
				if raw["GOPATH"] != "" {
					env.gopath = raw["GOPATH"]
				}
				if raw["GOOS"] != "" {
					env.goos = raw["GOOS"]
				}
				if raw["GOARCH"] != "" {
					env.goarch = raw["GOARCH"]
				}
			}
		}
	}

	return env
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

type replaceTarget struct {
	path    string
	version string
	isLocal bool
}

type moduleInfo struct {
	modPath     string
	modDir      string // slash path relative to repo root, e.g. "" or "src"
	requires    map[string]string
	replaces    map[string]replaceTarget
	siblingMods map[string]string // modPath -> modDir in repo (from go.work)
}

func (m *moduleInfo) clone() *moduleInfo {
	c := &moduleInfo{
		requires:    make(map[string]string),
		replaces:    make(map[string]replaceTarget),
		siblingMods: make(map[string]string),
	}
	if m == nil {
		return c
	}
	c.modPath = m.modPath
	c.modDir = m.modDir
	for k, v := range m.requires {
		c.requires[k] = v
	}
	for k, v := range m.replaces {
		c.replaces[k] = v
	}
	for k, v := range m.siblingMods {
		c.siblingMods[k] = v
	}
	return c
}

func (m *moduleInfo) matchReplaceTarget(importPath string) (target replaceTarget, sub string, ok bool) {
	if m == nil || len(m.replaces) == 0 {
		return replaceTarget{}, "", false
	}
	var longestOrig string
	for origMod, rep := range m.replaces {
		if importPath == origMod || strings.HasPrefix(importPath, origMod+"/") {
			if len(origMod) > len(longestOrig) {
				longestOrig = origMod
				target = rep
			}
		}
	}
	if longestOrig == "" {
		return replaceTarget{}, "", false
	}
	sub = strings.TrimPrefix(strings.TrimPrefix(importPath, longestOrig), "/")
	return target, sub, true
}

func (m *moduleInfo) resolveRequireDir(importPath, gomodcache string) (string, bool) {
	if m == nil || gomodcache == "" || len(m.requires) == 0 {
		return "", false
	}
	var longestReq string
	var longestVer string
	for reqMod, ver := range m.requires {
		if importPath == reqMod || strings.HasPrefix(importPath, reqMod+"/") {
			if len(reqMod) > len(longestReq) {
				longestReq = reqMod
				longestVer = ver
			}
		}
	}
	if longestReq == "" {
		return "", false
	}
	sub := strings.TrimPrefix(strings.TrimPrefix(importPath, longestReq), "/")
	escMod := escapeModPath(longestReq)
	escVer := escapeModPath(longestVer)
	targetDir := filepath.Join(gomodcache, escMod+"@"+escVer)
	if sub != "" {
		targetDir = filepath.Join(targetDir, filepath.FromSlash(sub))
	}
	if dirHasGoFiles(targetDir) {
		return targetDir, true
	}
	return "", false
}

func resolveModCacheReplaceDir(rep replaceTarget, sub, gomodcache string) (string, bool) {
	if rep.isLocal || rep.version == "" || gomodcache == "" {
		return "", false
	}
	escMod := escapeModPath(rep.path)
	escVer := escapeModPath(rep.version)
	modDir := filepath.Join(gomodcache, escMod+"@"+escVer)
	targetDir := modDir
	if sub != "" {
		targetDir = filepath.Join(modDir, filepath.FromSlash(sub))
	}
	if dirHasGoFiles(targetDir) {
		return targetDir, true
	}
	return "", false
}

func findModuleInfo(repo vcs.Repo, rev, filePath string, rc *diskCache) *moduleInfo {
	cleanFile := path.Clean(filepath.ToSlash(filePath))
	dir := path.Dir(cleanFile)
	if dir == "." {
		dir = ""
	}

	if rc != nil {
		rc.modMu.RLock()
		if m, ok := rc.modInfos[dir]; ok {
			rc.modMu.RUnlock()
			return m
		}
		rc.modMu.RUnlock()
	}

	var foundModDir string
	var modData []byte

	// Walk upward from file's directory to repo root
	curr := dir
	for {
		modFile := "go.mod"
		if curr != "" {
			modFile = path.Join(curr, "go.mod")
		}
		data, err := repo.Content(rev, modFile)
		if err == nil && len(data) > 0 {
			foundModDir = curr
			modData = data
			break
		}
		if curr == "" {
			break
		}
		next := path.Dir(curr)
		if next == "." || next == curr {
			next = ""
		}
		curr = next
	}

	// Fallback for Go standard library repository root (where go.mod lives at src/go.mod)
	if len(modData) == 0 {
		if data, err := repo.Content(rev, "src/go.mod"); err == nil && len(data) > 0 {
			foundModDir = "src"
			modData = data
		}
	}

	var m *moduleInfo
	if len(modData) > 0 {
		m = parseGoMod(modData)
		m.modDir = foundModDir
	} else {
		m = &moduleInfo{
			requires:    make(map[string]string),
			replaces:    make(map[string]replaceTarget),
			siblingMods: make(map[string]string),
		}
	}

	// Check for go.work in repo root or parent directories to discover sibling modules
	workData, err := repo.Content(rev, "go.work")
	if err == nil && len(workData) > 0 {
		useDirs := parseGoWork(workData)
		for _, useDir := range useDirs {
			cleanUse := path.Clean(useDir)
			if cleanUse == "." {
				cleanUse = ""
			}
			if cleanUse == m.modDir {
				continue
			}
			siblingModPath := path.Join(cleanUse, "go.mod")
			if sData, err := repo.Content(rev, siblingModPath); err == nil && len(sData) > 0 {
				sMod := parseGoMod(sData)
				if sMod.modPath != "" {
					if m.siblingMods == nil {
						m.siblingMods = make(map[string]string)
					}
					m.siblingMods[sMod.modPath] = cleanUse
					if m.requires == nil {
						m.requires = make(map[string]string)
					}
					for reqMod, reqVer := range sMod.requires {
						if _, exists := m.requires[reqMod]; !exists {
							m.requires[reqMod] = reqVer
						}
					}
					if m.replaces == nil {
						m.replaces = make(map[string]replaceTarget)
					}
					for repMod, repTarget := range sMod.replaces {
						if _, exists := m.replaces[repMod]; !exists {
							m.replaces[repMod] = repTarget
						}
					}
				}
			}
		}
	}

	// In Go standard library repository (e.g. ~/Repositories/go):
	// src/go.mod defines "module std"
	// src/cmd/go.mod defines "module cmd"
	if m.modPath == "std" && m.modDir == "src" {
		// Sibling cmd module
		if m.siblingMods == nil {
			m.siblingMods = make(map[string]string)
		}
		m.siblingMods["cmd"] = "src/cmd"
	} else if m.modPath == "cmd" && m.modDir == "src/cmd" {
		if m.siblingMods == nil {
			m.siblingMods = make(map[string]string)
		}
		m.siblingMods["std"] = "src"
	}

	if rc != nil {
		rc.modMu.Lock()
		rc.modInfos[dir] = m
		rc.modMu.Unlock()
	}

	return m
}

func parseGoMod(data []byte) *moduleInfo {
	m := &moduleInfo{
		requires:    make(map[string]string),
		replaces:    make(map[string]replaceTarget),
		siblingMods: make(map[string]string),
	}

	lines := strings.Split(string(data), "\n")
	inRequire := false
	inReplace := false

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if i := strings.Index(line, "//"); i != -1 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}

		if inRequire {
			if line == ")" {
				inRequire = false
				continue
			}
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				m.requires[parts[0]] = parts[1]
			}
			continue
		}

		if inReplace {
			if line == ")" {
				inReplace = false
				continue
			}
			parseReplaceLine(m, line)
			continue
		}

		if strings.HasPrefix(line, "module ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				m.modPath = strings.Trim(parts[1], `"'`)
			}
			continue
		}

		if line == "require (" {
			inRequire = true
			continue
		}
		if strings.HasPrefix(line, "require ") {
			parts := strings.Fields(strings.TrimPrefix(line, "require "))
			if len(parts) >= 2 {
				m.requires[parts[0]] = parts[1]
			}
			continue
		}

		if line == "replace (" {
			inReplace = true
			continue
		}
		if strings.HasPrefix(line, "replace ") {
			parseReplaceLine(m, strings.TrimPrefix(line, "replace "))
			continue
		}
	}

	return m
}

func parseReplaceLine(m *moduleInfo, line string) {
	arrow := strings.Index(line, "=>")
	if arrow == -1 {
		return
	}
	left := strings.TrimSpace(line[:arrow])
	right := strings.TrimSpace(line[arrow+2:])

	leftParts := strings.Fields(left)
	if len(leftParts) == 0 {
		return
	}
	origMod := leftParts[0]

	rightParts := strings.Fields(right)
	if len(rightParts) == 0 {
		return
	}

	targetPath := rightParts[0]
	targetVer := ""
	if len(rightParts) >= 2 {
		targetVer = rightParts[1]
	}

	isLocal := strings.HasPrefix(targetPath, "./") || strings.HasPrefix(targetPath, "../") || filepath.IsAbs(targetPath) || targetPath == "."
	m.replaces[origMod] = replaceTarget{
		path:    targetPath,
		version: targetVer,
		isLocal: isLocal,
	}
}

func parseGoWork(data []byte) []string {
	var useDirs []string
	lines := strings.Split(string(data), "\n")
	inUse := false

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if i := strings.Index(line, "//"); i != -1 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}

		if inUse {
			if line == ")" {
				inUse = false
				continue
			}
			useDirs = append(useDirs, strings.Trim(line, `"'`))
			continue
		}

		if line == "use (" {
			inUse = true
			continue
		}
		if strings.HasPrefix(line, "use ") {
			dir := strings.TrimSpace(strings.TrimPrefix(line, "use "))
			useDirs = append(useDirs, strings.Trim(dir, `"'`))
			continue
		}
	}

	return useDirs
}

func escapeModPath(p string) string {
	var sb strings.Builder
	for _, r := range p {
		if unicode.IsUpper(r) {
			sb.WriteByte('!')
			sb.WriteRune(unicode.ToLower(r))
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func guessPkgName(importPath string) string {
	clean := path.Clean(importPath)
	base := path.Base(clean)
	// If base is a major version tag like "v2", use the parent directory's base
	if strings.HasPrefix(base, "v") && len(base) > 1 {
		isDigits := true
		for _, c := range base[1:] {
			if c < '0' || c > '9' {
				isDigits = false
				break
			}
		}
		if isDigits {
			parent := path.Dir(clean)
			if parent != "." && parent != "/" {
				base = path.Base(parent)
			}
		}
	}
	base = strings.ReplaceAll(base, "-", "_")
	base = strings.ReplaceAll(base, ".", "_")
	return base
}

func parseSemver(v string) (major, minor, patch int, prerelease string, ok bool) {
	v = strings.TrimPrefix(v, "v")
	if idx := strings.Index(v, "+"); idx != -1 {
		v = v[:idx]
	}
	if idx := strings.Index(v, "-"); idx != -1 {
		prerelease = v[idx+1:]
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	if len(parts) >= 1 {
		major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) >= 2 {
		minor, _ = strconv.Atoi(parts[1])
	}
	if len(parts) >= 3 {
		patch, _ = strconv.Atoi(parts[2])
	}
	return major, minor, patch, prerelease, true
}

func compareSemver(v1, v2 string) int {
	maj1, min1, pat1, pre1, _ := parseSemver(v1)
	maj2, min2, pat2, pre2, _ := parseSemver(v2)

	if maj1 != maj2 {
		if maj1 > maj2 {
			return 1
		}
		return -1
	}
	if min1 != min2 {
		if min1 > min2 {
			return 1
		}
		return -1
	}
	if pat1 != pat2 {
		if pat1 > pat2 {
			return 1
		}
		return -1
	}
	if pre1 == "" && pre2 != "" {
		return 1
	}
	if pre1 != "" && pre2 == "" {
		return -1
	}
	if pre1 == pre2 {
		return 0
	}

	p1Parts := strings.Split(pre1, ".")
	p2Parts := strings.Split(pre2, ".")
	for i := 0; i < len(p1Parts) && i < len(p2Parts); i++ {
		p1 := p1Parts[i]
		p2 := p2Parts[i]
		if p1 == p2 {
			continue
		}
		n1, err1 := strconv.Atoi(p1)
		n2, err2 := strconv.Atoi(p2)
		if err1 == nil && err2 == nil {
			if n1 > n2 {
				return 1
			}
			return -1
		}
		if err1 == nil && err2 != nil {
			return -1
		}
		if err1 != nil && err2 == nil {
			return 1
		}
		if p1 > p2 {
			return 1
		}
		return -1
	}
	if len(p1Parts) > len(p2Parts) {
		return 1
	}
	if len(p1Parts) < len(p2Parts) {
		return -1
	}
	return 0
}
