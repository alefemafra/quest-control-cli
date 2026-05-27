package internal

import (
	"os"
	"path/filepath"
)

const (
	primaryArtifactDirName = "quest"
	legacyArtifactDirName  = "mission"
)

// ResolveArtifactDir returns the artifact directory for a spec folder.
// Preference order:
//  1. quest/ when it already exists
//  2. mission/ when only legacy layout exists
//  3. quest/ as the default for new specs
func ResolveArtifactDir(specDir string) string {
	questDir := filepath.Join(specDir, primaryArtifactDirName)
	missionDir := filepath.Join(specDir, legacyArtifactDirName)

	if hasArtifactContents(questDir) {
		return questDir
	}
	if hasArtifactContents(missionDir) {
		return missionDir
	}
	return questDir
}

func LegacyArtifactDir(specDir string) string {
	return filepath.Join(specDir, legacyArtifactDirName)
}

func PrimaryArtifactDir(specDir string) string {
	return filepath.Join(specDir, primaryArtifactDirName)
}

func hasArtifactContents(dir string) bool {
	markers := []string{
		"features.json",
		"validation-contract.md",
		"knowledge-base.md",
		"project-context.md",
		"codebase-analysis.md",
		"critique-criteria.local.md",
	}
	for _, marker := range markers {
		if fileExists(filepath.Join(dir, marker)) {
			return true
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "runs")); err == nil && info.IsDir() {
		return true
	}
	return false
}
