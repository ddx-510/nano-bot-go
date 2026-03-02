package repos

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/PlatoX-Type/monet-bot/config"
)

// InitAll clones missing repos and pulls existing ones.
// In local mode, uses SSH remotes. In cloud mode, uses HTTPS+PAT remotes.
func InitAll(cfg *config.Config) {
	for _, repo := range cfg.Repos {
		path := repo.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(cfg.Workspace, path)
		}

		gitDir := filepath.Join(path, ".git")
		if _, err := os.Stat(gitDir); err == nil {
			log.Printf("[repos] pulling %s...", repo.Name)
			pull(path, repo.Name)
			continue
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

		os.MkdirAll(filepath.Dir(path), 0o755)
		log.Printf("[repos] cloning %s (%s mode)...", repo.Name, cfg.Mode)
		args := []string{"clone", "--depth", "50"}
		if repo.Branch != "" {
			args = append(args, "--branch", repo.Branch)
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

// PullLoop periodically pulls all repos.
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
			pull(path, repo.Name)
		}
	}
}

func pull(path, name string) {
	cmd := exec.Command("git", "pull", "--quiet")
	cmd.Dir = path
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[repos] pull failed for %s: %s", name, string(out))
	}
}
