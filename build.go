package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

type BuildResult struct {
	Commit     string `json:"commit"`
	BinaryPath string `json:"binary_path"`
	Warning    string `json:"warning,omitempty"`
}

// Build clones (or updates) the exploit repo and compiles shofel2_t124.
//
// This branch's own EMMC_RECOVERY_GUIDE.md documents a real prior incident:
// an earlier write-chunk-size "optimization" on this exact codebase
// corrupted eMMC sectors on a test unit. We record the exact commit built
// so a bad run is reproducible/diagnosable, and deliberately do NOT
// auto-pull on every build once a binary already exists — see BuildIfNeeded.
func Build(cfg *Config) (*BuildResult, error) {
	if err := os.MkdirAll(cfg.VendorPath, 0o755); err != nil {
		return nil, err
	}

	if _, err := os.Stat(cfg.RepoDir); os.IsNotExist(err) {
		cmd := exec.Command("git", "clone", "--depth", "1", "--branch", cfg.RepoBranch, cfg.RepoURL, cfg.RepoDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git clone failed: %w: %s", err, string(out))
		}
	}

	buildCmd := exec.Command("make", "-C", cfg.RepoDir, fmt.Sprintf("-j%d", runtime.NumCPU()))
	if out, err := buildCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("make failed: %w: %s", err, truncate(string(out), 2000))
	}

	if _, err := os.Stat(cfg.BinaryPath); err != nil {
		return nil, fmt.Errorf("build finished but %s not found", cfg.BinaryPath)
	}

	out, err := exec.Command("git", "-C", cfg.RepoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-parse failed: %w", err)
	}
	commit := trimNL(string(out))

	res := &BuildResult{
		Commit:     commit,
		BinaryPath: cfg.BinaryPath,
		Warning:    "This exploit branch has a documented history of eMMC write corruption from a prior chunk-size optimization (see upstream EMMC_RECOVERY_GUIDE.md). The commit above is recorded in state.json for reproducibility.",
	}

	st, _ := LoadState(cfg)
	st.BuildCommit = commit
	_ = SaveState(cfg, st)

	return res, nil
}

// BuildIfNeeded skips the clone/build entirely if the binary already
// exists — deliberately does not silently `git pull` a possibly-different,
// unreviewed HEAD on every invocation. Run `jibotool build --update` to
// pull explicitly.
func BuildIfNeeded(cfg *Config, forceUpdate bool) (*BuildResult, error) {
	if forceUpdate && dirExists(cfg.RepoDir) {
		if out, err := exec.Command("git", "-C", cfg.RepoDir, "pull", "--ff-only").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git pull failed: %w: %s", err, string(out))
		}
	}
	if !forceUpdate && fileExists(cfg.BinaryPath) {
		out, err := exec.Command("git", "-C", cfg.RepoDir, "rev-parse", "HEAD").Output()
		commit := ""
		if err == nil {
			commit = trimNL(string(out))
		}
		return &BuildResult{Commit: commit, BinaryPath: cfg.BinaryPath, Warning: "using existing build; pass --update to git pull + rebuild"}, nil
	}
	return Build(cfg)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
