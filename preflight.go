package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

type CheckResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Fixed  bool   `json:"fixed,omitempty"`
}

type PreflightResult struct {
	OK     bool          `json:"ok"`
	Checks []CheckResult `json:"checks"`
}

// requiredBins mirrors jibo-unlock.sh's phase0 tool list minus python3
// (GPT parsing is now native Go) and libusb (checked separately via dpkg,
// since it's a -dev package with no standalone binary on PATH).
var requiredBins = []string{"git", "make", "arm-none-eabi-gcc", "debugfs", "e2fsck", "lsusb"}

const udevRulePath = "/etc/udev/rules.d/99-tegra-rcm.rules"
const udevRuleContent = `SUBSYSTEM=="usb", ATTRS{idVendor}=="0955", MODE="0666"` + "\n"

// minFreeBytesForOneRun is a conservative estimate: backup + work + verify
// images at up to ~500 MB each for Jibo's /var partition, plus headroom.
// This is the check that would have caught /home's 669 MB free (not enough)
// before EMMC_READ started filling the disk mid-operation.
const minFreeBytesForOneRun = 2 * 1024 * 1024 * 1024 // 2 GiB

func checkBinary(bin string) CheckResult {
	if _, err := exec.LookPath(bin); err != nil {
		return CheckResult{Name: "binary:" + bin, OK: false, Detail: "not found on PATH"}
	}
	return CheckResult{Name: "binary:" + bin, OK: true}
}

func Preflight(cfg *Config, fix bool) PreflightResult {
	var checks []CheckResult
	checks = append(checks, checkOS())

	binResults := map[string]CheckResult{}
	for _, bin := range requiredBins {
		binResults[bin] = checkBinary(bin)
	}

	libusbOK := checkLibusbDev()

	if fix {
		binToPkg := map[string]string{
			"git":               "git",
			"make":              "make",
			"arm-none-eabi-gcc": "gcc-arm-none-eabi",
			"debugfs":           "e2fsprogs",
			"e2fsck":            "e2fsprogs",
			"lsusb":             "usbutils",
		}
		var pkgs []string
		seen := map[string]bool{}
		var neededByBin []string
		for _, bin := range requiredBins {
			if !binResults[bin].OK {
				neededByBin = append(neededByBin, bin)
				if p := binToPkg[bin]; p != "" && !seen[p] {
					pkgs = append(pkgs, p)
					seen[p] = true
				}
			}
		}
		if !libusbOK.OK {
			pkgs = append(pkgs, "libusb-1.0-0-dev")
		}
		if len(pkgs) > 0 {
			fixed, detail := aptInstall(pkgs)
			checks = append(checks, CheckResult{Name: "apt-get install", OK: fixed, Detail: detail, Fixed: fixed})
			if fixed {
				// Re-verify rather than assume — apt can report success
				// while still missing an expected binary in edge cases
				// (wrong package name, partial install, etc.).
				for _, bin := range neededByBin {
					binResults[bin] = checkBinary(bin)
					if binResults[bin].OK {
						binResults[bin] = CheckResult{Name: binResults[bin].Name, OK: true, Fixed: true}
					}
				}
				libusbOK = checkLibusbDev()
			}
		}
	}

	for _, bin := range requiredBins {
		checks = append(checks, binResults[bin])
	}
	checks = append(checks, libusbOK)
	checks = append(checks, checkUdevRule(fix))
	checks = append(checks, checkWorkDirSpace(cfg, fix))
	checks = append(checks, checkUSBVisible(cfg))

	allOK := true
	for _, c := range checks {
		if !c.OK {
			allOK = false
		}
	}
	return PreflightResult{OK: allOK, Checks: checks}
}

func checkOS() CheckResult {
	if runtime.GOOS != "linux" {
		return CheckResult{Name: "os", OK: false, Detail: fmt.Sprintf("running on %s; the Tegra RCM USB loader requires Linux usbdevfs ioctls (macOS/Windows won't work as the host)", runtime.GOOS)}
	}
	return CheckResult{Name: "os", OK: true, Detail: runtime.GOOS + "/" + runtime.GOARCH}
}

func checkLibusbDev() CheckResult {
	out, err := exec.Command("dpkg", "-s", "libusb-1.0-0-dev").CombinedOutput()
	if err == nil && contains(string(out), "Status: install ok installed") {
		return CheckResult{Name: "libusb-1.0-0-dev", OK: true}
	}
	return CheckResult{Name: "libusb-1.0-0-dev", OK: false, Detail: "not installed (needed to build shofel2_t124)"}
}

func aptInstall(pkgs []string) (bool, string) {
	args := append([]string{"apt-get", "install", "-y"}, pkgs...)
	cmd := exec.Command("sudo", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("apt-get install failed: %v: %s", err, truncate(string(out), 500))
	}
	return true, fmt.Sprintf("installed: %v", pkgs)
}

func checkUdevRule(fix bool) CheckResult {
	if _, err := os.Stat(udevRulePath); err == nil {
		return CheckResult{Name: "udev-rule", OK: true, Detail: udevRulePath}
	}
	if !fix {
		return CheckResult{Name: "udev-rule", OK: false, Detail: "missing " + udevRulePath + " (run with --fix, or shofel2_t124 will need sudo)"}
	}
	cmd := exec.Command("sudo", "tee", udevRulePath)
	cmd.Stdin = fixedReader(udevRuleContent)
	if out, err := cmd.CombinedOutput(); err != nil {
		return CheckResult{Name: "udev-rule", OK: false, Detail: fmt.Sprintf("failed to write rule: %v: %s", err, string(out))}
	}
	_ = exec.Command("sudo", "udevadm", "control", "--reload-rules").Run()
	_ = exec.Command("sudo", "udevadm", "trigger").Run()
	return CheckResult{Name: "udev-rule", OK: true, Detail: "installed " + udevRulePath, Fixed: true}
}

func checkWorkDirSpace(cfg *Config, fix bool) CheckResult {
	if fix {
		if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
			// WorkDir might be under a root-owned path like /opt; retry with sudo + chown.
			_ = exec.Command("sudo", "mkdir", "-p", cfg.WorkDir).Run()
			if u, err2 := exec.Command("id", "-un").Output(); err2 == nil {
				_ = exec.Command("sudo", "chown", "-R", trimNL(string(u)), cfg.WorkDir).Run()
			}
		}
	}

	var stat syscall.Statfs_t
	checkPath := cfg.WorkDir
	if _, err := os.Stat(checkPath); os.IsNotExist(err) {
		checkPath = "/" // fall back to the root filesystem's free space if the dir doesn't exist yet
	}
	if err := syscall.Statfs(checkPath, &stat); err != nil {
		return CheckResult{Name: "disk-space", OK: false, Detail: fmt.Sprintf("statfs failed: %v", err)}
	}
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	freeMB := freeBytes / 1024 / 1024
	ok := freeBytes >= minFreeBytesForOneRun
	detail := fmt.Sprintf("%d MB free at %s (need >= %d MB for one backup+work+verify image set)", freeMB, cfg.WorkDir, minFreeBytesForOneRun/1024/1024)
	return CheckResult{Name: "disk-space", OK: ok, Detail: detail}
}

func checkUSBVisible(cfg *Config) CheckResult {
	out, err := exec.Command("lsusb", "-d", cfg.VID+":"+cfg.PID).CombinedOutput()
	if err == nil && len(trimNL(string(out))) > 0 {
		return CheckResult{Name: "usb-rcm-device", OK: true, Detail: trimNL(string(out))}
	}
	// Informational only — Jibo doesn't need to be in RCM mode to pass
	// preflight, only for the actual operations later.
	return CheckResult{Name: "usb-rcm-device", OK: true, Detail: "not currently visible (informational only — trigger RCM before running discover/backup/write)"}
}
