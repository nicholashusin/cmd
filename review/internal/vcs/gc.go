package vcs

import (
	"fmt"
	"strings"
)

// StalePins returns stale snapshot pin refs without deleting them.
func StalePins(repo Repo, activeKeys map[string]bool) ([]string, error) {
	pins, err := repo.ListPins()
	if err != nil {
		return nil, fmt.Errorf("listing pins: %w", err)
	}

	var stale []string
	for _, pin := range pins {
		// pin format: refs/reviewed/<key>/...
		parts := strings.Split(pin, "/")
		if len(parts) >= 3 && parts[0] == "refs" && parts[1] == "reviewed" {
			key := parts[2]
			if !activeKeys[key] {
				stale = append(stale, pin)
			}
		}
	}

	return stale, nil
}

// PrunePins removes stale refs/reviewed/<key>/* pins for changes that are no longer
// pending in the repository.
func PrunePins(repo Repo, activeKeys map[string]bool) ([]string, error) {
	stale, err := StalePins(repo, activeKeys)
	if err != nil {
		return nil, err
	}

	var deleted []string
	for _, ref := range stale {
		if err := repo.DeletePin(ref); err == nil {
			deleted = append(deleted, ref)
		}
	}

	return deleted, nil
}
