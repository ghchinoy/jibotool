package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds the fixed parameters for driving the Jibo unlock exploit.
// These mirror jibo-unlock.sh exactly — this is a translation of already
// safety-reviewed logic, not a redesign of it.
type Config struct {
	WorkDir     string // root directory for everything jibotool produces
	RepoURL     string
	RepoBranch  string
	RepoDir     string // WorkDir/vendor/ShofEL2-for-T124-Jibo-Edition
	BinaryPath  string // RepoDir/shofel2_t124
	VendorPath  string // WorkDir/vendor
	BackupsPath string // WorkDir/backups
	JobsPath    string // WorkDir/jobs
	StatePath   string // WorkDir/state.json
	VID         string // Jibo RCM USB vendor ID, "0955"
	PID         string // Jibo RCM USB product ID, "7740"
}

// DefaultWorkDir avoids $HOME deliberately: on the Coral Dev Board /home is
// its own small partition (confirmed 669 MB free on the unit this was built
// against) which is not enough room for a backup+work+verify image trio
// (~500 MB each). / has much more headroom. Override with --workdir or
// JIBOTOOL_WORKDIR if this default doesn't fit your board's partitioning.
const DefaultWorkDir = "/opt/jibo-unlock"

const (
	DefaultRepoURL    = "https://github.com/devsparx/ShofEL2-for-T124-Jibo-Edition"
	DefaultRepoBranch = "improvements/IncreasedUSBReadWriteSpeed"
	DefaultVID        = "0955"
	DefaultPID        = "7740" // Jibo in RCM; Jetson TK1=7140, Shield TK1=7f40
)

func LoadConfig(workDirFlag string) *Config {
	workDir := workDirFlag
	if workDir == "" {
		workDir = os.Getenv("JIBOTOOL_WORKDIR")
	}
	if workDir == "" {
		workDir = DefaultWorkDir
	}

	repoDir := filepath.Join(workDir, "vendor", "ShofEL2-for-T124-Jibo-Edition")

	return &Config{
		WorkDir:     workDir,
		RepoURL:     DefaultRepoURL,
		RepoBranch:  DefaultRepoBranch,
		RepoDir:     repoDir,
		BinaryPath:  filepath.Join(repoDir, "shofel2_t124"),
		VendorPath:  filepath.Join(workDir, "vendor"),
		BackupsPath: filepath.Join(workDir, "backups"),
		JobsPath:    filepath.Join(workDir, "jobs"),
		StatePath:   filepath.Join(workDir, "state.json"),
		VID:         DefaultVID,
		PID:         DefaultPID,
	}
}

// State persists discovered facts across separate jibotool invocations,
// replacing jibo-unlock.sh's partition_env.sh / build_commit.env.
type State struct {
	BuildCommit       string `json:"build_commit,omitempty"`
	PartitionName     string `json:"partition_name,omitempty"`
	PartitionStartHex string `json:"partition_start_hex,omitempty"` // e.g. "0x2103296" — always hex, never decimal
	PartitionSizeHex  string `json:"partition_size_hex,omitempty"`
	PartitionSizeMB   int64  `json:"partition_size_mb,omitempty"`
}

func LoadState(cfg *Config) (*State, error) {
	st := &State{}
	b, err := os.ReadFile(cfg.StatePath)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	return st, nil
}

func SaveState(cfg *Config, st *State) error {
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cfg.StatePath)
}
