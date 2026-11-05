package skill

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

//go:embed SKILL.md
var skillDoc []byte

// Content returns the raw markdown text of the embedded SKILL.md.
func Content() []byte {
	return skillDoc
}

// DefaultInstallPaths returns the standard skill directories for supported agent environments.
func DefaultInstallPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".claude", "skills", "review"),
		filepath.Join(home, ".gemini", "skills", "review"),
		filepath.Join(home, ".agents", "skills", "review"),
	}
}

// Install writes SKILL.md into the target directory.
func Install(targetDir string) (string, error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", fmt.Errorf("creating directory %s: %w", targetDir, err)
	}
	targetFile := filepath.Join(targetDir, "SKILL.md")
	if err := os.WriteFile(targetFile, skillDoc, 0644); err != nil {
		return "", fmt.Errorf("writing %s: %w", targetFile, err)
	}
	return targetFile, nil
}

// Run executes the `review skill` subcommand.
func Run(args []string, stdout io.Writer, workDir string) error {
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	fs.SetOutput(stdout)

	printFlag := fs.Bool("print", false, "Print SKILL.md to stdout")
	installFlag := fs.Bool("install", false, "Install skill to standard agent locations")
	projectFlag := fs.Bool("project", false, "Install skill to current project (.gemini/skills/review)")
	outDir := fs.String("o", "", "Install skill to a specific directory")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *printFlag || (!*installFlag && !*projectFlag && *outDir == "") {
		_, err := stdout.Write(skillDoc)
		return err
	}

	if *projectFlag {
		dir := filepath.Join(workDir, ".gemini", "skills", "review")
		path, err := Install(dir)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Installed review skill to %s\n", path)
	}

	if *outDir != "" {
		path, err := Install(*outDir)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Installed review skill to %s\n", path)
	}

	if *installFlag {
		paths := DefaultInstallPaths()
		for _, p := range paths {
			installedPath, err := Install(p)
			if err != nil {
				fmt.Fprintf(stdout, "Failed installing to %s: %v\n", p, err)
			} else {
				fmt.Fprintf(stdout, "Installed review skill to %s\n", installedPath)
			}
		}
	}

	return nil
}
