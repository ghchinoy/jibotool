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
  preflight [--fix]                Verify required host libraries (libusb, e2fsprogs) and udev rules;
                                   use --fix to automatically install dependencies (via apt-get).
  build [--update]                 Clone and compile the underlying shofel2_t124 exploit tool and ARM payloads;
                                   use --update to pull the latest commit from the devsparx repository.
  status                           Perform EMMC_STATUS check to verify eMMC controller has initialized cleanly.
  discover                         Read GPT partition table natively and locate Jibo's /var partition (Partition 5);
                                   saves partition sectors and offsets to state.json.
  backup                           Perform full read of Jibo's /var partition to a timestamped backup image (~9 min).
                                   Requires Jibo in RCM mode. Clean journal and verify structure automatically.
  edit <backup.img>                Create a copy of backup.img to var_work.img and patch mode.json to int-developer.
                                   Checks ext4 inode integrity and asserts correct file type.
  patch-file <img_path> <path_in_img> <local_content_file> [--show-diff]
                                   Patch any other file inside an image in-place (e.g. WiFi config).
                                   Content is redacted from output logs by default unless --show-diff is passed.
  write <work_img> I_UNDERSTAND_THIS_WRITES_REAL_EMMC
                                   Flash work_img back to real eMMC (~14 min) and perform a mandatory
                                   RCM read-back and SHA256 integrity verification (~9 min).
  restore <backup_img> I_UNDERSTAND_THIS_WRITES_REAL_EMMC
                                   Flash backup_img back to real eMMC (~14 min) to restore a prior clean state,
                                   followed by mandatory read-back and SHA256 integrity verification.
  health                           Read eMMC's EXT_CSD register and report Device Life Time and Pre-EOL status.
  jobs [id]                        List all background job IDs, or display a specific job's status.json.

Flags (can be passed to any command):
  --workdir <path>                 Override default work directory (default: /opt/jibo-unlock)
  --background                     Run command as a detached background process; prints a job ID to poll.
  --job-dir <path>                 (Internal) Used by background child processes to track task execution.

Hardware Note:
  All long-running commands (status/discover/backup/write/restore/health) require Jibo to be physically
  put into RCM mode (Hold LOWER small back button, tap MIDDLE button). They will report "waiting_for_rcm": true
  in their status/JSON envelope until you trigger RCM.
`)
}
