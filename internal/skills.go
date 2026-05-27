package internal

import (
	"embed"
	"strings"
)

//go:embed all:skills
var skillsFS embed.FS

func ReadSkill(name string) string {
	for _, candidate := range skillNameCandidates(name) {
		data, err := skillsFS.ReadFile("skills/" + candidate + ".md")
		if err == nil {
			return string(data)
		}
	}
	return ""
}

func ReadTemplate(name string) string {
	data, err := skillsFS.ReadFile("skills/templates/" + name + ".md")
	if err != nil {
		return ""
	}
	return string(data)
}

func skillNameCandidates(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	candidates := []string{name}
	if strings.HasPrefix(name, "mission-") {
		candidates = []string{"quest-" + strings.TrimPrefix(name, "mission-"), name}
	} else if name == "spec-to-mission" {
		candidates = []string{"spec-to-quest", name}
	} else if name == "init-mission" {
		candidates = []string{"init-quest", name}
	}

	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		for _, existing := range candidates {
			if existing == candidate {
				return
			}
		}
		candidates = append(candidates, candidate)
	}

	switch {
	case strings.HasPrefix(name, "quest-"):
		add("mission-" + strings.TrimPrefix(name, "quest-"))
	}

	switch name {
	case "spec-to-mission":
		add("spec-to-quest")
	case "spec-to-quest":
		add("spec-to-mission")
	case "init-mission":
		add("init-quest")
	case "init-quest":
		add("init-mission")
	}

	return candidates
}
