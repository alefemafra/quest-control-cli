package internal

import "testing"

func TestSkillNameCandidates_PrefersQuestForLegacyNames(t *testing.T) {
	candidates := skillNameCandidates("mission-spec")
	if len(candidates) < 2 {
		t.Fatalf("expected at least 2 candidates, got %v", candidates)
	}
	if candidates[0] != "quest-spec" || candidates[1] != "mission-spec" {
		t.Fatalf("unexpected candidate order: %v", candidates)
	}
}

func TestReadSkill_QuestAndMissionAliasResolveSameContent(t *testing.T) {
	quest := ReadSkill("quest-worker")
	mission := ReadSkill("mission-worker")
	if quest == "" || mission == "" {
		t.Fatalf("expected both aliases to resolve skill content")
	}
	if quest != mission {
		t.Fatalf("expected quest-worker and mission-worker aliases to match")
	}
}

func TestReadSkill_SpecToQuestFallsBackToLegacyFile(t *testing.T) {
	skill := ReadSkill("spec-to-quest")
	if skill == "" {
		t.Fatal("expected spec-to-quest skill to resolve via compatibility fallback")
	}
}
