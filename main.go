// jibotool drives the Jibo RCM eMMC-unlock exploit (shofel2_t124) from
// non-interactive, JSON-emitting subcommands, so it can be operated over a
// plain SSH connection from a remote automated driver — rather than
// requiring an interactive TTY the way `mdt shell` / bash prompts do.
//
// It does NOT reimplement the RCM/USB exploit itself; that stays in the
// already-reviewed shofel2_t124 C binary (devsparx/ShofEL2-for-T124-Jibo-Edition).
// jibotool is an orchestration layer: GPT parsing, debugfs edits, mandatory
// write verification, background job management, and structured status —
// translating the safety logic originally built into jibo-unlock.sh.
//
// See docs/jibotool.md for the full design and usage.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// widenPATH ensures /sbin and /usr/sbin are searched, not just the user
// bin dirs. Found the hard way: a non-interactive SSH command's PATH
// (/usr/local/bin:/usr/bin:/bin:/usr/games on the board this was built
// against) does not include /sbin, where debugfs and e2fsck actually live
// (dpkg -L e2fsprogs confirms this). A human's interactive login shell
// often has a different PATH than what sshd hands a non-interactive
// command — exactly the kind of gap that only shows up once you actually
// drive a board over plain SSH instead of an interactive session, which is
// the whole reason this tool exists. Fix it once, here, rather than special
// -casing every exec.LookPath/exec.Command call downstream.
func widenPATH() {
	extra := []string{"/sbin", "/usr/sbin", "/usr/local/sbin"}
	current := os.Getenv("PATH")
	parts := strings.Split(current, ":")
	have := map[string]bool{}
	for _, p := range parts {
		have[p] = true
	}
	for _, e := range extra {
		if !have[e] {
			parts = append(parts, e)
		}
	}
	os.Setenv("PATH", strings.Join(parts, ":"))
}

type Envelope struct {
	OK      bool   `json:"ok"`
	Command string `json:"command"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"ok":false,"error":"failed to marshal result: %v"}`+"\n", err)
		return
	}
	fmt.Println(string(b))
}

var commandFuncs = map[string]func(*Config, *Reporter, []string){
	"status":     CmdStatus,
	"discover":   CmdDiscover,
	"backup":     CmdBackup,
	"edit":       CmdEdit,
	"patch-file": CmdPatchFile,
	"write":      CmdWrite,
	"restore":    CmdRestore,
	"health":     CmdHealth,
}

func main() {
	widenPATH()

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	subcmd := os.Args[1]
	rest := os.Args[2:]

	flags, remaining := extractFlags(rest)
	cfg := LoadConfig(flags.workDir)

	switch subcmd {
	case "help", "-h", "--help":
		printUsage()

	case "version":
		fmt.Println("jibotool 0.1.0")

	case "preflight":
		res := Preflight(cfg, flags.fix)
		printJSON(res)
		if !res.OK {
			os.Exit(1)
		}

	case "build":
		res, err := BuildIfNeeded(cfg, flags.update)
		if err != nil {
			printJSON(Envelope{OK: false, Command: subcmd, Error: err.Error()})
			os.Exit(1)
		}
		printJSON(Envelope{OK: true, Command: subcmd, Data: res})

	case "jobs":
		handleJobs(cfg, remaining)

	case "status", "discover", "backup", "edit", "patch-file", "write", "restore", "health":
		fn := commandFuncs[subcmd]
		runCommand(cfg, subcmd, fn, remaining, flags)

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", subcmd)
		printUsage()
		os.Exit(1)
	}
}

// runCommand handles the foreground/background split: if --background was
// passed and we are the original (parent) invocation (jobDir not yet set),
// re-exec ourselves detached and return the job directory immediately. If
// jobDir IS set, we're either the re-exec'd child or an explicit resume —
// either way, run the real command logic and report through that job's
// status.json.
func runCommand(cfg *Config, subcmd string, fn func(*Config, *Reporter, []string), args []string, flags Flags) {
	if flags.background && flags.jobDir == "" {
		fullArgs := append([]string{subcmd, "--workdir", cfg.WorkDir}, args...)
		jobDir, err := StartBackground(cfg, fullArgs)
		if err != nil {
			printJSON(Envelope{OK: false, Command: subcmd, Error: err.Error()})
			os.Exit(1)
		}
		printJSON(Envelope{OK: true, Command: subcmd, Data: map[string]string{
			"job_dir": jobDir,
			"hint":    "poll with: jibotool jobs " + jobDirBase(jobDir),
		}})
		return
	}

	r := NewReporter(subcmd, flags.jobDir)
	fn(cfg, r, args)
}

func handleJobs(cfg *Config, args []string) {
	if len(args) == 0 {
		ids, err := ListJobs(cfg)
		if err != nil {
			printJSON(Envelope{OK: false, Command: "jobs", Error: err.Error()})
			os.Exit(1)
		}
		printJSON(Envelope{OK: true, Command: "jobs", Data: ids})
		return
	}
	st, err := ReadJobStatus(cfg, args[0])
	if err != nil {
		printJSON(Envelope{OK: false, Command: "jobs", Error: err.Error()})
		os.Exit(1)
	}
	printJSON(st)
}

func printUsage() {
	fmt.Print(`jibotool — non-interactive driver for the Jibo RCM eMMC unlock

Usage: jibotool <command> [args] [flags]

Commands:
  preflight [--fix]              Check required tools/udev/disk space; --fix to remediate
  build [--update]                Clone/build shofel2_t124 (skips if already built; --update to git pull)
  status                          Run EMMC_STATUS, report eMMC controller health
  discover                        Read GPT, locate Jibo's /var partition, save to state.json
  backup                          Full read of /var to a timestamped image + content review
  edit <backup.img>                Patch mode.json to int-developer on a copy of the backup
  patch-file <img> <path-in-img> <content-file> [--show-diff]
                                   Patch any other file inside an image in place (e.g. WiFi
                                   config). Content is redacted from output unless --show-diff
                                   is passed — use for anything that might contain secrets.
  write <img> I_UNDERSTAND_THIS_WRITES_REAL_EMMC
                                   Write image back to /var + mandatory read-back verification
  restore <img> I_UNDERSTAND_THIS_WRITES_REAL_EMMC
                                   Same as write, for restoring a prior backup
  health                          Read EXT_CSD, report eMMC wear/life estimation
  jobs [id]                        List background jobs, or show one job's status.json

Flags (any command):
  --workdir <path>                 Override work directory (default /opt/jibo-unlock)
  --background                     Run this command detached; prints a job dir to poll
  --job-dir <path>                 (internal) used by background child processes

All long-running commands (status/discover/backup/write/restore/health)
require Jibo to be physically put into RCM mode. They will report
"waiting_for_rcm": true in their status until you do so — trigger it any
time after starting the command, foreground or background.
`)
}
