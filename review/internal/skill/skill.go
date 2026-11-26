package skill

import (
	_ "embed"
	"errors"
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
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	anyInstall := *installFlag || *projectFlag || *outDir != ""
	if *printFlag || !anyInstall {
		if _, err := stdout.Write(skillDoc); err != nil {
			return err
		}
		if !anyInstall {
			return nil
		}
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
		target := *outDir
		if !filepath.IsAbs(target) && workDir != "" {
			target = filepath.Join(workDir, target)
		}
		path, err := Install(target)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Installed review skill to %s\n", path)
	}

	if *installFlag {
		paths := DefaultInstallPaths()
		if len(paths) == 0 {
			return errors.New("could not determine default skill install paths")
		}
		var errs []error
		for _, p := range paths {
			installedPath, err := Install(p)
			if err != nil {
				errs = append(errs, err)
			} else {
				fmt.Fprintf(stdout, "Installed review skill to %s\n", installedPath)
			}
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
	}

	return nil
}
