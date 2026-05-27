package internal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveArtifactDir_PrefersQuestWhenPresent(t *testing.T) {
	specDir := t.TempDir()
	questDir := filepath.Join(specDir, "quest")
	missionDir := filepath.Join(specDir, "mission")

	if err := os.MkdirAll(questDir, 0o755); err != nil {
		t.Fatalf("mkdir quest: %v", err)
	}
	if err := os.MkdirAll(missionDir, 0o755); err != nil {
		t.Fatalf("mkdir mission: %v", err)
	}
	if err := os.WriteFile(filepath.Join(questDir, "features.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write quest features: %v", err)
	}
	if err := os.WriteFile(filepath.Join(missionDir, "features.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write mission features: %v", err)
	}

	if got := ResolveArtifactDir(specDir); got != questDir {
		t.Fatalf("expected quest dir, got %q", got)
	}
}

func TestResolveArtifactDir_FallsBackToMission(t *testing.T) {
	specDir := t.TempDir()
	missionDir := filepath.Join(specDir, "mission")
	if err := os.MkdirAll(missionDir, 0o755); err != nil {
		t.Fatalf("mkdir mission: %v", err)
	}
	if err := os.WriteFile(filepath.Join(missionDir, "features.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write mission features: %v", err)
	}

	if got := ResolveArtifactDir(specDir); got != missionDir {
		t.Fatalf("expected mission fallback dir, got %q", got)
	}
}

func TestResolveArtifactDir_DefaultsToQuestWhenMissing(t *testing.T) {
	specDir := t.TempDir()
	want := filepath.Join(specDir, "quest")
	if got := ResolveArtifactDir(specDir); got != want {
		t.Fatalf("expected quest default dir, got %q", got)
	}
}
