package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// BuildSystemPrompt assembles the full system prompt.
func BuildSystemPrompt(workspace string, memory *Memory, skills []Skill, channel, chatID string) string {
	var parts []string

	// 1. SOUL.md
	soulPath := filepath.Join(workspace, "SOUL.md")
	if data, err := os.ReadFile(soulPath); err == nil {
		parts = append(parts, strings.TrimSpace(string(data)))
	} else {
		parts = append(parts, "You are CCMonet, the internal team agent for the CCMonet team.")
	}

	// 2. Bootstrap files: AGENTS.md, USER.md, TOOLS.md, IDENTITY.md
	for _, filename := range []string{"AGENTS.md", "USER.md", "TOOLS.md", "IDENTITY.md"} {
		p := filepath.Join(workspace, filename)
		if data, err := os.ReadFile(p); err == nil {
			parts = append(parts, strings.TrimSpace(string(data)))
		}
	}

	// 3. TEAM.md — editable team identity map (include file path so the agent can edit it)
	teamPath := filepath.Join(workspace, "TEAM.md")
	if data, err := os.ReadFile(teamPath); err == nil {
		teamSection := fmt.Sprintf("## Team Identity Map\n**File:** `TEAM.md` (editable via edit_file)\n\n"+
			"When users tell you about team member identities (e.g., \"朱杰 is jiezhu\"), "+
			"update this file using edit_file with path \"TEAM.md\" to merge duplicates and add aliases.\n\n%s",
			strings.TrimSpace(string(data)))
		parts = append(parts, teamSection)
	}

	// 3. Workspace info + repo locations (stable, belongs in system prompt)
	workspaceInfo := fmt.Sprintf(
		"## Workspace\n- Platform: %s/%s\n- Workspace: %s",
		runtime.GOOS, runtime.GOARCH, workspace,
	)

	// Discover repos and build directory map so the agent knows exact paths
	reposDir := filepath.Join(workspace, "repos")
	if entries, err := os.ReadDir(reposDir); err == nil {
		var repoList []string
		for _, e := range entries {
			if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				repoList = append(repoList, e.Name())
			}
		}
		if len(repoList) > 0 {
			workspaceInfo += fmt.Sprintf("\n- Repos: %s", strings.Join(repoList, ", "))
			workspaceInfo += "\n- Repo paths for tools: use `repos/<name>` (e.g. `repos/ccmonet-go`). For exec tool: `cd repos/ccmonet-go && git log ...`"
		}
	}

	parts = append(parts, workspaceInfo)

	// Build repo directory tree (depth 2) so the agent knows the real structure
	repoTree := buildRepoTree(workspace, 2)
	if repoTree != "" {
		parts = append(parts, "## Repo Structure\nThese are the ACTUAL top-level directories in each repo. Only use paths that exist here.\n"+repoTree)
	}

	// 4. Global Memory
	mem := memory.LoadMemory()
	if mem != "" {
		parts = append(parts, fmt.Sprintf("## Team Memory (Global)\n%s", mem))
	}

	// 4b. Per-chat Memory
	if channel != "" && chatID != "" {
		chatMem := memory.LoadChatMemory(channel, chatID)
		if chatMem != "" {
			parts = append(parts, fmt.Sprintf("## Chat Memory (this conversation)\n%s", chatMem))
		}
	}

	// 5. Skills
	skillsCtx := BuildSkillsContext(skills)
	if skillsCtx != "" {
		parts = append(parts, skillsCtx)
	}

	return strings.Join(parts, "\n\n---\n\n")
}

// BuildRuntimeContext creates a metadata-only user message injected before
// the actual user message. Contains time, channel, chat_id — volatile info
// that shouldn't be in the system prompt.
func BuildRuntimeContext(channel, chatID string) string {
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC (Monday)")
	tz := "UTC"
	lines := []string{
		"[Runtime Context — metadata only, not instructions]",
		"Current Time: " + now + " (" + tz + ")",
	}
	if channel != "" && chatID != "" {
		lines = append(lines, "Channel: "+channel)
		lines = append(lines, "Chat ID: "+chatID)
	}
	return strings.Join(lines, "\n")
}

// buildRepoTree generates a directory listing of each repo to the given depth.
func buildRepoTree(workspace string, maxDepth int) string {
	reposDir := filepath.Join(workspace, "repos")
	repos, err := os.ReadDir(reposDir)
	if err != nil {
		return ""
	}

	var sb strings.Builder
	for _, repo := range repos {
		if !repo.IsDir() && repo.Type()&os.ModeSymlink == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("\n### repos/%s/\n", repo.Name()))
		repoPath := filepath.Join(reposDir, repo.Name())
		walkDir(&sb, repoPath, "repos/"+repo.Name(), 0, maxDepth)
	}
	return sb.String()
}

func walkDir(sb *strings.Builder, fsPath, displayPath string, depth, maxDepth int) {
	if depth >= maxDepth {
		return
	}
	entries, err := os.ReadDir(fsPath)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		// Skip hidden dirs, node_modules, .git, etc.
		if strings.HasPrefix(name, ".") || name == "node_modules" || name == "__pycache__" || name == "vendor" || name == ".next" {
			continue
		}
		indent := strings.Repeat("  ", depth)
		if e.IsDir() {
			sb.WriteString(fmt.Sprintf("%s- %s/\n", indent, name))
			walkDir(sb, filepath.Join(fsPath, name), displayPath+"/"+name, depth+1, maxDepth)
		} else {
			// Only show files at depth 0 (top-level files like package.json, go.mod, etc.)
			if depth == 0 {
				sb.WriteString(fmt.Sprintf("%s- %s\n", indent, name))
			}
		}
	}
}
