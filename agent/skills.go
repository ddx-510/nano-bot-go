package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Skill struct {
	Name        string
	Description string
	Path        string
	AlwaysOn    bool
	Body        string
}

// LoadSkills reads skills from workspace/skills/ and any skills/ directories
// found inside cloned repos (repos/*/skills/).
// Supports two formats:
//   - Flat files: skills/{name}.md
//   - Directories: skills/{name}/SKILL.md
func LoadSkills(workspace string) []Skill {
	var skills []Skill

	// Primary: workspace/skills/
	skills = append(skills, loadSkillsFromDir(filepath.Join(workspace, "skills"), "skills")...)

	// Secondary: repos/*/skills/ — auto-discovered from cloned repos
	reposDir := filepath.Join(workspace, "repos")
	repoEntries, err := os.ReadDir(reposDir)
	if err == nil {
		for _, repo := range repoEntries {
			if !repo.IsDir() {
				continue
			}
			repoSkillsDir := filepath.Join(reposDir, repo.Name(), ".claude", "skills")
			if _, err := os.Stat(repoSkillsDir); err != nil {
				continue
			}
			relPrefix := filepath.Join("repos", repo.Name(), "skills")
			skills = append(skills, loadSkillsFromDir(repoSkillsDir, relPrefix)...)
		}
	}

	return skills
}

// loadSkillsFromDir scans a single directory for skills.
func loadSkillsFromDir(dir, relPrefix string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var skills []Skill
	for _, e := range entries {
		var path, relPath string

		if e.IsDir() {
			// Directory format: {dir}/{name}/SKILL.md
			skillFile := filepath.Join(dir, e.Name(), "SKILL.md")
			if _, err := os.Stat(skillFile); err != nil {
				continue
			}
			path = skillFile
			relPath = filepath.Join(relPrefix, e.Name(), "SKILL.md")
		} else if strings.HasSuffix(e.Name(), ".md") {
			// Flat file format: {dir}/{name}.md
			path = filepath.Join(dir, e.Name())
			relPath = filepath.Join(relPrefix, e.Name())
		} else {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		meta, body := parseFrontmatter(string(data))
		name := meta["name"]
		if name == "" {
			if e.IsDir() {
				name = e.Name()
			} else {
				name = strings.TrimSuffix(e.Name(), ".md")
			}
		}

		skills = append(skills, Skill{
			Name:        name,
			Description: meta["description"],
			Path:        relPath,
			AlwaysOn:    strings.ToLower(meta["always_on"]) == "true",
			Body:        body,
		})
	}
	return skills
}

func parseFrontmatter(text string) (map[string]string, string) {
	if !strings.HasPrefix(text, "---") {
		return nil, text
	}
	parts := strings.SplitN(text, "---", 3)
	if len(parts) < 3 {
		return nil, text
	}

	meta := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(parts[1]), "\n") {
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		meta[key] = val
	}
	return meta, strings.TrimSpace(parts[2])
}

// BuildSkillsContext builds the skills section for the system prompt.
func BuildSkillsContext(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Available Skills\n\n")

	for _, s := range skills {
		if s.AlwaysOn {
			fmt.Fprintf(&sb, "### %s\n%s\n\n", s.Name, s.Body)
		}
	}

	var onDemand []Skill
	for _, s := range skills {
		if !s.AlwaysOn {
			onDemand = append(onDemand, s)
		}
	}
	if len(onDemand) > 0 {
		sb.WriteString("### On-demand skills (use read_file to load when needed):\n")
		for _, s := range onDemand {
			fmt.Fprintf(&sb, "- **%s**: %s → `read_file(\"%s\")`\n", s.Name, s.Description, s.Path)
		}
	}

	return sb.String()
}
