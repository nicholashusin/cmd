package skill

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSkillContentAndInstall(t *testing.T) {
	content := Content()
	if len(content) == 0 {
		t.Fatalf("expected embedded skill content, got empty")
	}
	if !strings.Contains(string(content), "name: review") {
		t.Errorf("expected skill frontmatter 'name: review'")
	}
	if !strings.Contains(string(content), "Addressing Review Comments Workflow") {
		t.Errorf("expected skill workflow instructions")
	}

	targetDir := t.TempDir()
	path, err := Install(targetDir)
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, content) {
		t.Errorf("installed file content mismatch")
	}
}

func TestSkillRun(t *testing.T) {
	var buf bytes.Buffer
	err := Run([]string{"-print"}, &buf, "")
	if err != nil {
		t.Fatalf("Run -print failed: %v", err)
	}
	if !strings.Contains(buf.String(), "Review Skill") {
		t.Errorf("expected skill markdown in stdout")
	}

	buf.Reset()
	outDir := t.TempDir()
	err = Run([]string{"-o", outDir}, &buf, "")
	if err != nil {
		t.Fatalf("Run -o failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "SKILL.md")); err != nil {
		t.Errorf("expected SKILL.md to be installed in %s", outDir)
	}

	buf.Reset()
	projectDir := t.TempDir()
	err = Run([]string{"-project"}, &buf, projectDir)
	if err != nil {
		t.Fatalf("Run -project failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".gemini", "skills", "review", "SKILL.md")); err != nil {
		t.Errorf("expected SKILL.md in project directory")
	}
}

func TestDefaultInstallPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("os.UserHomeDir: %v", err)
	}
	want := []string{
		filepath.Join(home, ".claude", "skills", "review"),
		filepath.Join(home, ".gemini", "skills", "review"),
		filepath.Join(home, ".agents", "skills", "review"),
	}
	got := DefaultInstallPaths()
	if !slices.Equal(got, want) {
		t.Errorf("DefaultInstallPaths() = %v, want %v", got, want)
	}
}
