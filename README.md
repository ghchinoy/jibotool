# jibotool

A Go CLI that drives the Jibo RCM eMMC-unlock exploit from **non-interactive,
JSON-emitting subcommands**, so it can be operated over a plain SSH
connection by an automated driver (including an AI assistant) rather than
requiring a human at an interactive terminal for every step.

It does not reimplement the RCM/USB exploit itself — `shofel2_t124` still
does that. `jibotool` is an orchestration layer: GPT partition parsing,
`debugfs` edits, mandatory write verification, background job management,
and structured status.

## Scope

- **What it does:** orchestrates the community-known Jibo (Tegra K1)
  RCM/eMMC unlock — health check, partition discovery, backup, an
  offline `mode.json` edit, write, and mandatory read-back verification —
  as scriptable, JSON-emitting subcommands.
- **What it doesn't do:** it doesn't implement the RCM/USB exploit itself
  (that's `shofel2_t124`, unmodified), and it targets exactly one
  partition (`/var`) for exactly one purpose (flipping Jibo into
  `int-developer` mode, or patching a handful of other config files the
  same way). It's not a general eMMC-flashing tool.
- **What it requires:** a Linux host with a physical USB connection to
  Jibo. The one step that can never be automated is the physical RCM
  button combo — every long-running command reports
  `"waiting_for_rcm": true` until a human does it.

## ⚠️ Safety & caveats — read before running anything

This tool writes raw sectors to a real, largely irreplaceable piece of
hardware.

- **The write step is genuinely irreversible-risk.** `write`/`restore`
  require an exact confirm phrase (`I_UNDERSTAND_THIS_WRITES_REAL_EMMC`)
  as a CLI argument — a deliberate gate at the tool level, not just a
  conversational one.
- **Always keep the backup image** `backup` produces, off the host, until
  you've confirmed the unlock booted successfully.
- **Never power-cycle Jibo if write verification fails.** `write`/`restore`
  refuse to claim success until a full SHA256 read-back matches; if it
  doesn't, retry or restore from backup with the USB connection still
  live — see [`docs/USERGUIDE.md`](docs/USERGUIDE.md#if-write-verification-fails).
- **Check eMMC health first on long-stored units** (`jibotool health`) —
  years of shelf time can mean significant flash wear.
- **macOS and Windows cannot drive this.** Linux `usbdevfs` ioctls only.

The full finding-by-finding safety review (what reading the actual exploit
source turned up, and exactly how `jibotool` mitigates each one) is in
[`docs/USERGUIDE.md`](docs/USERGUIDE.md#safety-review-what-a-careful-read-of-the-exploit-source-turned-up).

## Quickstart

| Requirement | Notes |
|---|---|
| Linux host | A Coral Dev Board running Mendel Linux, or any Linux box with a spare USB port |
| `gcc-arm-none-eabi`, `libusb-1.0-0-dev`, `e2fsprogs`, `git`, `make`, `go` | Build and orchestration tools — `jibotool preflight --fix` installs what's missing |
| Micro-USB cable | Connects to Jibo's rear RCM port |

```bash
jibotool preflight --fix
jibotool build
jibotool health                                  # check eMMC wear before anything else
jibotool status                                  # EMMC_STATUS health check
jibotool discover                                # locate /var partition
jibotool backup --background                     # returns a job dir immediately
jibotool jobs <job-id>                            # poll progress
jibotool edit <backup.img>
jibotool write <work.img> I_UNDERSTAND_THIS_WRITES_REAL_EMMC --background
jibotool jobs <job-id>                            # poll until done+verified
```

Each command that needs Jibo in RCM mode (hold the lower back button,
tap the middle button) sits with `"waiting_for_rcm": true` until you do
that — trigger it any time after starting the command.

**For the full walkthrough — explanations at each step, the RCM combo in
detail, the background-job polling model, patching other files (e.g. WiFi
config), what to do if verification fails, and troubleshooting — see the
[User Guide](docs/USERGUIDE.md).**

## Build

```bash
make build          # host arch, ./bin/jibotool
make arm64          # cross-compile linux/arm64, ./bin/jibotool-linux-arm64
make armv7          # cross-compile linux/armv7, ./bin/jibotool-linux-armv7
make test
```

## Command reference

```
jibotool — non-interactive driver for the Jibo RCM eMMC unlock

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
  version                          Print the jibotool version.

Flags (can be passed to any command):
  --workdir <path>                 Override default work directory (default: /opt/jibo-unlock)
  --background                     Run command as a detached background process; prints a job ID to poll.
  --job-dir <path>                 (Internal) Used by background child processes to track task execution.

Hardware Note:
  All long-running commands (status/discover/backup/write/restore/health) require Jibo to be physically
  put into RCM mode (Hold LOWER small back button, tap MIDDLE button). They will report "waiting_for_rcm": true
  in their status/JSON envelope until you trigger RCM.
```

## Architecture

![Jibo Hardware Interaction Architecture](docs/architecture.webp)

*(Source: [`docs/architecture.dot`](docs/architecture.dot))*

- **`shofel2_t124`** (C, unmodified): all actual RCM/USB/eMMC hardware
  access. Requires a physical USB connection to Jibo, so it must run on
  whatever host is physically wired to the robot.
- **`jibotool`** (this repo, Go, cross-compilable to `linux/arm64` or
  `linux/armv7`): wraps `shofel2_t124` as a subprocess, parses its output
  (including detecting the "waiting for RCM" state so a driver knows
  exactly when to prompt for the physical button combo), does GPT parsing
  natively, shells out to `debugfs` for ext4 edits, and manages background
  jobs plus JSON status files.
- **The physical RCM trigger** can never be automated — see
  [Safety & caveats](#-safety--caveats--read-before-running-anything) above.

The end-to-end command sequence, and the background-job state machine
behind `--background`/`jobs`, are diagrammed in the
[User Guide](docs/USERGUIDE.md).

## What you get after unlock

**A static green checkmark on Jibo's screen, and root SSH access** — that's
the confirmed sign the unlock worked. It's *not* the familiar animated eye:
stock `int-developer` mode doesn't launch any skill process by default, and
even once one is launched it needs a reachable cloud substitute to do
anything beyond show its own offline error screen. See
[What Jibo actually looks like](docs/USERGUIDE.md#what-jibo-actually-looks-like)
and [Next steps](docs/USERGUIDE.md#next-steps-restoring-full-functionality)
in the User Guide for the full picture.

- Root SSH: `ssh root@<jibo-ip>` password `jibo`, **change immediately**.
- The rootfs is mounted read-only by default; remount to make changes:
  ```bash
  mount -o remount,rw /
  # ...edits...
  mount -o remount,ro /
  ```
- The stock firmware is otherwise intact — all partitions except `/var`
  are untouched. Full details, plus known post-unlock gotchas (WiFi DHCP
  race, `/tmp` `noexec`, etc.), are in the
  [User Guide](docs/USERGUIDE.md#what-you-get-after-unlock).

## Data safety

Only Jibo's `/var` partition is read or written, and only after
`discover`'s partition-identification result is checked against a
`debugfs`-listed `/jibo` directory. All other partitions (rootfs, skills)
are never touched. `backup` produces a complete image of `/var` before any
modification; `restore` returns a unit to that exact pre-unlock state and
is itself followed by a read-back verification.

## Background & history

`jibotool` exists because an earlier interactive bash script
(`jibo-unlock.sh`) didn't survive contact with non-interactive SSH driving,
and because a from-scratch safety review of the exploit's C source turned
up several findings (hex-vs-decimal sector args, no built-in write
verification, an inode-type footgun in `mode.json`) that shaped how this
tool is built. The full story — why Jibo needs this at all, how the RCM
exploit works, why a Go CLI instead of a script, and the complete
safety-review writeup — is in the
[User Guide](docs/USERGUIDE.md#why-this-exists-background-and-design-rationale).

## Credits

- [mkemka/jibo-unlock-notes](https://github.com/mkemka/jibo-unlock-notes), the original end-to-end unlock recipe
- [devsparx/ShofEL2-for-T124-Jibo-Edition](https://github.com/devsparx/ShofEL2-for-T124-Jibo-Edition), the exploit fork with eMMC read/write
- [wertus33333/ShofEL2-for-T124](https://github.com/wertus33333/ShofEL2-for-T124), upstream ShofEL2 compilation for T124
- [fail0verflow — ShofEL2 Vulnerability Write-Up](https://fail0verflow.com/blog/2018/shofel2/), original vulnerability and Fusée Gelée exploit analysis
- [Katherine Temkin — Fusée Gelée Exploit Report](https://github.com/Qyriad/fusee-launcher/blob/master/report/fusee_gelee.md), detailed buffer-overflow exploit blueprint
- [Jibo Revival Group / JiboAutoMod](https://github.com/Jibo-Revival-Group/JiboAutoMod), non-interactive orchestration and USB-settling prior art

## License

Apache License 2.0, see [LICENSE](LICENSE).
