package repos

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PlatoX-Type/monet-bot/config"
)

// InitAll clones missing repos and pulls existing ones.
// In local mode, uses SSH remotes. In cloud mode, uses HTTPS+PAT remotes.
func InitAll(cfg *config.Config) {
	log.Printf("[repos] environment: %s", cfg.Env())

	for _, repo := range cfg.Repos {
		path := repo.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(cfg.Workspace, path)
		}

		// Pick remote based on mode
		remote := repo.Remote
		if cfg.IsLocal() && repo.RemoteLocal != "" {
			remote = repo.RemoteLocal
		}
		if remote == "" {
			log.Printf("[repos] %s: no remote configured for %s mode", repo.Name, cfg.Mode)
			continue
		}

		// Resolve branch for current environment
		branch := cfg.RepoBranch(repo)
		if strings.Contains(branch, "*") {
			resolved := resolveGlobBranch(remote, branch)
			if resolved == "" {
				log.Printf("[repos] %s: no remote branch matching pattern '%s'", repo.Name, branch)
				continue
			}
			log.Printf("[repos] %s: resolved '%s' -> '%s'", repo.Name, branch, resolved)
			branch = resolved
		}

		gitDir := filepath.Join(path, ".git")
		if _, err := os.Stat(gitDir); err == nil {
			// Repo exists — switch branch if needed, then pull
			current := currentBranch(path)
			if branch != "" && current != branch {
				log.Printf("[repos] %s: switching %s -> %s", repo.Name, current, branch)
				switchBranch(path, branch, repo.Name)
			}
			log.Printf("[repos] pulling %s (%s)...", repo.Name, branch)
			pull(path, repo.Name)
			continue
		}

		// Clone
		os.MkdirAll(filepath.Dir(path), 0o755)
		log.Printf("[repos] cloning %s branch=%s (%s mode)...", repo.Name, branch, cfg.Mode)
		args := []string{"clone", "--depth", "50"}
		if branch != "" {
			args = append(args, "--branch", branch)
		}
		args = append(args, remote, path)
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("[repos] clone failed for %s: %s", repo.Name, string(out))
		} else {
			log.Printf("[repos] cloned %s", repo.Name)
		}
	}
}

// PullLoop periodically pulls all repos (and re-resolves glob branches).
func PullLoop(cfg *config.Config, intervalMin int) {
	for {
		time.Sleep(time.Duration(intervalMin) * time.Minute)
		for _, repo := range cfg.Repos {
			path := repo.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(cfg.Workspace, path)
			}
			if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
				continue
			}

			// Re-resolve glob branches (e.g. a new release-* branch may have appeared)
			branch := cfg.RepoBranch(repo)
			if strings.Contains(branch, "*") {
				remote := repo.Remote
				if cfg.IsLocal() && repo.RemoteLocal != "" {
					remote = repo.RemoteLocal
				}
				resolved := resolveGlobBranch(remote, branch)
				if resolved != "" {
					current := currentBranch(path)
					if current != resolved {
						log.Printf("[repos] %s: new branch detected '%s' -> '%s'", repo.Name, current, resolved)
						switchBranch(path, resolved, repo.Name)
					}
				}
			}

			pull(path, repo.Name)
		}
	}
}

func pull(path, name string) {
	// Try fast-forward pull first
	cmd := exec.Command("git", "pull", "--ff-only", "--quiet")
	cmd.Dir = path
	if out, err := cmd.CombinedOutput(); err != nil {
		// If ff-only fails (diverged), hard reset to remote
		log.Printf("[repos] pull --ff-only failed for %s, resetting to origin...", name)
		branch := currentBranch(path)
		reset := exec.Command("git", "reset", "--hard", "origin/"+branch)
		reset.Dir = path
		if out2, err2 := reset.CombinedOutput(); err2 != nil {
			log.Printf("[repos] reset failed for %s: %s / %s", name, string(out), string(out2))
		}
	}
}

// currentBranch returns the current checked-out branch name.
func currentBranch(path string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = path
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// switchBranch fetches and checks out a branch.
func switchBranch(path, branch, name string) {
	// Fetch the branch first
	fetch := exec.Command("git", "fetch", "origin", branch)
	fetch.Dir = path
	if out, err := fetch.CombinedOutput(); err != nil {
		log.Printf("[repos] fetch failed for %s/%s: %s", name, branch, string(out))
		return
	}

	// Checkout (create tracking branch if needed)
	checkout := exec.Command("git", "checkout", branch)
	checkout.Dir = path
	if out, err := checkout.CombinedOutput(); err != nil {
		// Try creating from remote
		checkout2 := exec.Command("git", "checkout", "-b", branch, "origin/"+branch)
		checkout2.Dir = path
		if out2, err2 := checkout2.CombinedOutput(); err2 != nil {
			log.Printf("[repos] checkout failed for %s/%s: %s / %s", name, branch, string(out), string(out2))
		}
	}
}

// resolveGlobBranch finds the latest remote branch matching a glob pattern.
// e.g. "release-*" resolves to "release-2026-03-08" (sorted, latest wins).
func resolveGlobBranch(remote, pattern string) string {
	cmd := exec.Command("git", "ls-remote", "--heads", remote)
	out, err := cmd.Output()
	if err != nil {
		log.Printf("[repos] ls-remote failed for %s: %v", remote, err)
		return ""
	}

	// Parse: "<sha>\trefs/heads/<branch>"
	prefix := strings.TrimSuffix(pattern, "*")
	var matches []string
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		ref := strings.TrimPrefix(parts[1], "refs/heads/")
		if strings.HasPrefix(ref, prefix) {
			matches = append(matches, ref)
		}
	}

	if len(matches) == 0 {
		return ""
	}

	// Sort and return the latest (alphabetically last = latest date)
	sort.Strings(matches)
	return matches[len(matches)-1]
}
