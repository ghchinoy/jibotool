package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Status is the single JSON document a long-running command maintains for
// the entire duration of its work. A driver (human or, in this project's
// case, an AI assistant over SSH) polls this file with cheap, fast reads
// instead of blocking on one multi-minute SSH call — which is what made
// `mdt exec`/`mdt shell` awkward to drive programmatically in the first
// place (see docs/jibotool.md).
type Status struct {
	Command       string        `json:"command"`
	StartedAt     time.Time     `json:"started_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	Done          bool          `json:"done"`
	OK            bool          `json:"ok"`
	WaitingForRCM bool          `json:"waiting_for_rcm"`
	Phase         string        `json:"phase,omitempty"`
	Progress      *ProgressInfo `json:"progress,omitempty"`
	Error         string        `json:"error,omitempty"`
	Data          any           `json:"data,omitempty"`
}

// Reporter centralizes "what do we tell the driver right now" for both
// foreground (print final JSON, exit) and background (continuously update
// a status.json a driver polls) invocations, so command implementations
// don't need two code paths.
type Reporter struct {
	statusPath string // empty in foreground mode
	status     Status
}

func NewReporter(command, jobDir string) *Reporter {
	r := &Reporter{
		status: Status{Command: command, StartedAt: time.Now()},
	}
	if jobDir != "" {
		r.statusPath = filepath.Join(jobDir, "status.json")
	}
	r.flush()
	return r
}

func (r *Reporter) Phase(p string) {
	r.status.Phase = p
	r.flush()
}

func (r *Reporter) WaitingForRCM(waiting bool) {
	r.status.WaitingForRCM = waiting
	r.flush()
}

func (r *Reporter) Progress(p *ProgressInfo) {
	r.status.Progress = p
	r.flush()
}

// Done finalizes the status. In foreground mode this also prints the
// envelope to stdout, which is what a synchronous (non-backgrounded)
// invocation returns.
func (r *Reporter) Done(ok bool, data any, errMsg string) {
	r.status.Done = true
	r.status.OK = ok
	r.status.Data = data
	r.status.Error = errMsg
	r.flush()
	if r.statusPath == "" {
		printJSON(r.status)
	}
}

func (r *Reporter) flush() {
	r.status.UpdatedAt = time.Now()
	if r.statusPath == "" {
		return
	}
	b, err := json.MarshalIndent(r.status, "", "  ")
	if err != nil {
		return
	}
	tmp := r.statusPath + ".tmp"
	_ = os.WriteFile(tmp, b, 0o644)
	_ = os.Rename(tmp, r.statusPath) // atomic replace so a poller never reads a half-written file
}

// StartBackground re-execs the current binary with the same arguments plus
// --job-dir, detached from this process's session (so it survives the SSH
// session that launched it exiting), and returns immediately with the job
// directory the driver should poll.
func StartBackground(cfg *Config, args []string) (jobDir string, err error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}

	jobID := time.Now().Format("20060102_150405")
	jobDir = filepath.Join(cfg.JobsPath, jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return "", err
	}

	logPath := filepath.Join(jobDir, "output.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return "", err
	}

	childArgs := append(append([]string{}, args...), "--job-dir", jobDir)
	cmd := exec.Command(self, childArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from the SSH session's process group

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return "", err
	}
	_ = os.WriteFile(filepath.Join(jobDir, "pid"), []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0o644)

	// Deliberately do not Wait() — that's the point of "background".
	go func() { _ = cmd.Wait(); logFile.Close() }()

	return jobDir, nil
}

// ReadJobStatus is what `jibotool jobs <id>` uses — a fast, side-effect-free
// read, safe to poll every few seconds without any risk of interfering with
// the running operation.
func ReadJobStatus(cfg *Config, jobID string) (*Status, error) {
	path := filepath.Join(cfg.JobsPath, jobID, "status.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func ListJobs(cfg *Config) ([]string, error) {
	entries, err := os.ReadDir(cfg.JobsPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}
