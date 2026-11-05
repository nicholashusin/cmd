package vcs

import (
	"fmt"
	"strconv"
	"strings"
)

// StalePins returns stale snapshot pin refs without deleting them.
func StalePins(repo Repo, activeKeys map[string]bool) ([]string, error) {
	return StalePinsForSnapshots(repo, activeKeys, nil)
}

// StalePinsForSnapshots returns stale snapshot pin refs (including snap-<N> refs
// whose snapshot number is no longer valid for an active change) without deleting them.
func StalePinsForSnapshots(repo Repo, activeKeys map[string]bool, validSnapNums map[string]map[int]bool) ([]string, error) {
	pins, err := repo.ListPins()
	if err != nil {
		return nil, fmt.Errorf("listing pins: %w", err)
	}

	var stale []string
	for _, pin := range pins {
		// pin format: refs/reviewed/<key>/...
		parts := strings.Split(pin, "/")
		if len(parts) < 3 || parts[0] != "refs" || parts[1] != "reviewed" {
			continue
		}
		key := parts[2]
		if !activeKeys[key] {
			stale = append(stale, pin)
			continue
		}
		if validSnapNums != nil && len(parts) >= 4 && strings.HasPrefix(parts[3], "snap-") {
			if validMap, ok := validSnapNums[key]; ok {
				if n, err := strconv.Atoi(strings.TrimPrefix(parts[3], "snap-")); err == nil {
					if !validMap[n] {
						stale = append(stale, pin)
					}
				}
			}
		}
	}

	return stale, nil
}

// PrunePins removes stale refs/reviewed/<key>/* pins for changes that are no longer
// pending in the repository.
func PrunePins(repo Repo, activeKeys map[string]bool) ([]string, error) {
	return PrunePinsForSnapshots(repo, activeKeys, nil)
}

// PrunePinsForSnapshots removes stale refs/reviewed/<key>/* pins for changes that are no
// longer pending or snapshot numbers that are no longer valid in validSnapNums.
func PrunePinsForSnapshots(repo Repo, activeKeys map[string]bool, validSnapNums map[string]map[int]bool) ([]string, error) {
	stale, err := StalePinsForSnapshots(repo, activeKeys, validSnapNums)
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
