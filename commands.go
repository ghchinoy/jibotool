package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// handshakeFailureSignatures are shofel2_t124 error strings that occur
// during the RCM handshake itself, before any real eMMC operation starts.
// Found by comparing notes with the Jibo Revival Group's JiboAutoMod tool:
// their vendored shofel2_t124 retries the Chip ID read up to 12 times at
// 500ms intervals before giving up. The devsparx build we use has no such
// retry — it fails immediately on the first bad read. Rather than patch
// the vendored C, we retry the whole invocation at this layer.
var handshakeFailureSignatures = []string{
	"Couldn't read Chip ID",
	"Couldn't send RCM CMD",
	"Couldn't open the usb",
}

const (
	rcmHandshakeMaxAttempts = 5
	rcmHandshakeRetryDelay  = 2 * time.Second

	// rcmSettleDelay is paid after every successful shofel2_t124 exit.
	// Confirmed via dmesg on real hardware: after a payload finishes and
	// sends EMMC_CMD_EXIT, the USB device disconnects and re-enumerates
	// on its own — no physical RCM re-trigger needed between chained
	// operations — but the very next invocation can fail if it's issued
	// before the OS finishes that re-enumeration. This matches the Jibo
	// Revival Group's JiboAutoMod tool, which has the same pause
	// (1.5s, macOS-specific in their code) for exactly this reason.
	rcmSettleDelay = 2 * time.Second
)

// isHandshakeStageFailure reports whether a failed run never got past the
// RCM handshake (never saw "K1 in RCM mode connected."). Only these
// failures are safe to silently retry — once the handshake succeeds and a
// real eMMC operation is underway, a failure must be surfaced immediately,
// never masked by an automatic retry. This matters most for EMMC_WRITE:
// silently re-attempting a write that failed mid-transfer, without the
// caller ever seeing it happened, is exactly the kind of thing that turns
// a recoverable glitch into real data corruption.
func isHandshakeStageFailure(output string, connected bool) bool {
	if connected {
		return false
	}
	for _, sig := range handshakeFailureSignatures {
		if strings.Contains(output, sig) {
			return true
		}
	}
	return false
}

// runShofel wraps RunShofel2 with Reporter updates and a bounded retry loop
// for transient RCM-handshake failures, so every command gets consistent
// "waiting for RCM" / progress reporting for free. This is what a driver
// polls to know exactly when to tell a human "trigger RCM now" — informed
// by the tool's actual state, not a guess made before starting.
func runShofel(cfg *Config, r *Reporter, args []string) (string, error) {
	var lastOut string
	var lastErr error

	for attempt := 1; attempt <= rcmHandshakeMaxAttempts; attempt++ {
		connected := false
		out, err := RunShofel2(cfg, args, func(ev LineEvent) {
			if ev.WaitingForRCM {
				r.WaitingForRCM(true)
			}
			if ev.Connected {
				connected = true
				r.WaitingForRCM(false)
			}
			if ev.Progress != nil {
				r.Progress(ev.Progress)
			}
		})
		lastOut, lastErr = out, err

		if err == nil {
			time.Sleep(rcmSettleDelay) // let the device finish its automatic disconnect/re-enumerate cycle
			return out, nil
		}
		if !isHandshakeStageFailure(out, connected) {
			return out, err // failure past the handshake — a real operational failure, surface it now
		}
		if attempt < rcmHandshakeMaxAttempts {
			r.Phase(fmt.Sprintf("RCM handshake attempt %d/%d failed transiently (%s), retrying in %s...",
				attempt, rcmHandshakeMaxAttempts, firstMatchingSignature(out), rcmHandshakeRetryDelay))
			time.Sleep(rcmHandshakeRetryDelay)
		}
	}
	return lastOut, lastErr
}

func firstMatchingSignature(output string) string {
	for _, sig := range handshakeFailureSignatures {
		if strings.Contains(output, sig) {
			return sig
		}
	}
	return "unknown handshake failure"
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSizeSectors(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return uint64(info.Size()) / sectorSize, nil
}

// --- status: EMMC_STATUS health/init check -----------------------------

type StatusData struct {
	Healthy bool   `json:"healthy"`
	Raw     string `json:"raw"`
}

func CmdStatus(cfg *Config, r *Reporter, _ []string) {
	r.Phase("waiting for RCM / running EMMC_STATUS")
	out, err := runShofel(cfg, r, []string{"EMMC_STATUS"})
	if err != nil {
		r.Done(false, StatusData{Raw: out}, fmt.Sprintf("EMMC_STATUS failed: %v", err))
		return
	}
	healthy := strings.Contains(out, "Magic:         0xcafe0000 (OK)") &&
		strings.Contains(out, "=== FULLY INITIALIZED! ===")
	r.Done(true, StatusData{Healthy: healthy, Raw: out}, "")
}

// --- discover: GPT read + parse, locate /var ----------------------------

type DiscoverData struct {
	Name             string `json:"name"`
	PartitionIndex   int    `json:"partition_index"`
	StartHex         string `json:"start_hex"`
	SizeHex          string `json:"size_hex"`
	SizeMB           int64  `json:"size_mb"`
	NameHasVar       bool   `json:"name_has_var"`
	UsedFallbackScan bool   `json:"used_fallback_scan"`
}

func CmdDiscover(cfg *Config, r *Reporter, _ []string) {
	tmpDump := filepath.Join(cfg.WorkDir, "gpt_sectors.bin")
	_ = os.MkdirAll(cfg.WorkDir, 0o755)

	r.Phase("waiting for RCM / checking eMMC status")
	statusOut, err := runShofel(cfg, r, []string{"EMMC_STATUS"})
	if err != nil || !strings.Contains(statusOut, "=== FULLY INITIALIZED! ===") {
		r.Done(false, StatusData{Raw: statusOut}, "eMMC did not report a healthy init — refusing to proceed with discovery. Inspect the raw EMMC_STATUS output before retrying.")
		return
	}

	r.Phase("waiting for RCM / reading GPT area")
	// 34 sectors: MBR + GPT header (LBA1) + up to 128 entries (32 sectors).
	// shofel2_t124 parses sector args as hex — hexArg() is the only path
	// that should ever build these strings.
	gptReadOut, err := runShofel(cfg, r, []string{"EMMC_READ", hexArg(0), hexArg(34), tmpDump})
	if err != nil {
		r.Done(false, StatusData{Raw: gptReadOut}, fmt.Sprintf("EMMC_READ (GPT) failed: %v", err))
		return
	}

	data, err := os.ReadFile(tmpDump)
	if err != nil {
		r.Done(false, nil, fmt.Sprintf("could not read GPT dump: %v", err))
		return
	}

	// Primary heuristic: partition index 4 ("partition 5"), per mkemka's
	// notes and independently confirmed against the Jibo Revival Group's
	// JiboAutoMod tool. That tool doesn't trust this unconditionally
	// though — it sanity-checks the size (400-600MB) and falls back to
	// scanning all partitions for a tighter 450-550MB match if that
	// check fails. Same approach here.
	const varPartitionIndex = 4
	const primaryMinMB, primaryMaxMB = 400, 600
	const fallbackMinMB, fallbackMaxMB = 450, 550

	part, primaryErr := ParseGPT(data, varPartitionIndex)
	usedFallback := false

	needsFallback := primaryErr != nil
	if primaryErr == nil {
		sizeMB := part.SizeSectors * sectorSize / 1024 / 1024
		needsFallback = sizeMB < primaryMinMB || sizeMB > primaryMaxMB
	}

	if needsFallback {
		minSec := uint64(fallbackMinMB) * 1024 * 1024 / sectorSize
		maxSec := uint64(fallbackMaxMB) * 1024 * 1024 / sectorSize
		if fbPart, fbErr := FindPartitionBySize(data, minSec, maxSec); fbErr == nil {
			part = fbPart
			usedFallback = true
		} else if primaryErr != nil {
			r.Done(false, nil, fmt.Sprintf(
				"GPT parse failed at partition index %d (%v), and size-based fallback scan (%d-%dMB) also found nothing (%v) — this may mean Jibo's eMMC does not use a Linux-visible GPT for this partition (Tegra devices often layer an NVIDIA BCT/PT scheme too); do not assume an offset without independent confirmation",
				varPartitionIndex, primaryErr, fallbackMinMB, fallbackMaxMB, fbErr))
			return
		}
		// else: primary parse succeeded but size looked implausible, and the
		// fallback scan also found nothing better — proceed with the primary
		// result. NameHasVar and the out-of-range size will still be visible
		// in the returned data for a driver/human to catch.
	}

	dd := DiscoverData{
		Name:             part.Name,
		PartitionIndex:   part.Index,
		StartHex:         hexArg(part.StartLBA),
		SizeHex:          hexArg(part.SizeSectors),
		SizeMB:           int64(part.SizeSectors * sectorSize / 1024 / 1024),
		NameHasVar:       strings.Contains(strings.ToLower(part.Name), "var"),
		UsedFallbackScan: usedFallback,
	}

	st, _ := LoadState(cfg)
	st.PartitionName = dd.Name
	st.PartitionStartHex = dd.StartHex
	st.PartitionSizeHex = dd.SizeHex
	st.PartitionSizeMB = dd.SizeMB
	_ = SaveState(cfg, st)

	r.Done(true, dd, "")
}

// --- backup: full EMMC_READ of the discovered partition -----------------

type BackupData struct {
	ImagePath  string `json:"image_path"`
	SizeMB     int64  `json:"size_mb"`
	SHA256     string `json:"sha256"`
	LooksValid bool   `json:"looks_valid_var"`
	Listing    string `json:"listing"` // debugfs ls of / and /jibo — inspect before trusting LooksValid alone
	E2fsckOut  string `json:"e2fsck_output"`
}

func CmdBackup(cfg *Config, r *Reporter, _ []string) {
	st, err := LoadState(cfg)
	if err != nil || st.PartitionStartHex == "" {
		r.Done(false, nil, "no partition discovered yet — run `jibotool discover` first")
		return
	}

	ts := time.Now().Format("20060102_150405")
	backupDir := filepath.Join(cfg.BackupsPath, ts)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		r.Done(false, nil, err.Error())
		return
	}
	imagePath := filepath.Join(backupDir, fmt.Sprintf("var_backup_%s.img", ts))

	r.Phase("waiting for RCM / reading full /var partition (~9 min)")
	backupReadOut, err := runShofel(cfg, r, []string{"EMMC_READ", st.PartitionStartHex, st.PartitionSizeHex, imagePath})
	if err != nil {
		r.Done(false, StatusData{Raw: backupReadOut}, fmt.Sprintf("EMMC_READ failed: %v", err))
		return
	}

	info, _ := os.Stat(imagePath)
	sizeMB := info.Size() / 1024 / 1024

	r.Phase("running e2fsck")
	e2fsckOut, _ := exec.Command("e2fsck", "-y", "-f", imagePath).CombinedOutput() // non-zero exit is expected even on success

	r.Phase("computing checksum")
	sum, err := sha256File(imagePath)
	if err != nil {
		r.Done(false, nil, err.Error())
		return
	}

	r.Phase("reviewing contents (does this look like Jibo's /var?)")
	looksValid, listing, _ := LooksLikeValidVar(imagePath)

	writeManifest(backupDir, map[string]any{
		"timestamp":       ts,
		"backup_image":    imagePath,
		"sha256":          sum,
		"size_mb":         sizeMB,
		"looks_valid_var": looksValid,
		"partition_name":  st.PartitionName,
		"partition_start": st.PartitionStartHex,
		"partition_size":  st.PartitionSizeHex,
		"build_commit":    st.BuildCommit,
	})

	bd := BackupData{
		ImagePath:  imagePath,
		SizeMB:     sizeMB,
		SHA256:     sum,
		LooksValid: looksValid,
		Listing:    listing,
		E2fsckOut:  string(e2fsckOut),
	}

	if !looksValid {
		r.Done(false, bd, "content review FAILED: /jibo/mode.json not found in this image. Do not proceed to edit/write — the discovered partition is likely wrong. Backup image is preserved for inspection.")
		return
	}

	r.Done(true, bd, "")
}

// --- edit: patch mode.json on a copy of the backup -----------------------

type EditData struct {
	WorkImagePath string `json:"work_image_path"`
	Before        string `json:"mode_json_before"`
	After         string `json:"mode_json_after"`
}

const modeDeveloper = `{"mode":"int-developer"}`
const modeNormalSubstr = `"normal"`

func CmdEdit(cfg *Config, r *Reporter, args []string) {
	if len(args) < 1 {
		r.Done(false, nil, "usage: jibotool edit <backup-image-path>")
		return
	}
	backupImg := args[0]
	workImg := strings.TrimSuffix(backupImg, ".img") + "_work.img"

	r.Phase("copying backup to work image")
	if err := copyFile(backupImg, workImg); err != nil {
		r.Done(false, nil, err.Error())
		return
	}

	before, _ := DebugfsCat(workImg, "/jibo/mode.json")
	if !strings.Contains(before, modeNormalSubstr) {
		r.Phase("warning: mode.json did not contain the expected \"normal\" value — continuing, but verify this is intentional")
	}

	tmpFile, err := os.CreateTemp("", "jibo_mode_*.json")
	if err != nil {
		r.Done(false, nil, err.Error())
		return
	}
	defer os.Remove(tmpFile.Name())
	_, _ = tmpFile.WriteString(modeDeveloper)
	tmpFile.Close()

	r.Phase("writing new mode.json via debugfs")
	if err := DebugfsWriteFile(workImg, tmpFile.Name(), "/jibo/mode.json"); err != nil {
		r.Done(false, nil, err.Error())
		return
	}

	after, err := DebugfsCat(workImg, "/jibo/mode.json")
	if err != nil || after != modeDeveloper {
		r.Done(false, EditData{WorkImagePath: workImg, Before: before, After: after},
			fmt.Sprintf("verification failed: expected %q, got %q", modeDeveloper, after))
		return
	}

	statOut, _ := DebugfsStat(workImg, "/jibo/mode.json")
	if strings.Contains(statOut, "bad type") {
		r.Done(false, EditData{WorkImagePath: workImg, Before: before, After: after},
			"inode type is 'bad type' after set_inode_field — do NOT write this image back")
		return
	}

	r.Done(true, EditData{WorkImagePath: workImg, Before: before, After: after}, "")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// --- patch-file: general-purpose file patch inside an image --------------
//
// CmdEdit is hardcoded to /jibo/mode.json — the primary unlock action. This
// is the generalization of the same mechanism (built on the exact same
// DebugfsWriteFile that already carries the cd+relative-path fix and the
// inode mode/uid/gid repair) for any other file that needs patching after
// the unlock — e.g. /var/etc/wpa_supplicant.conf when the saved WiFi
// network doesn't match the current environment.
//
// Deliberately operates in place on whatever image path is given, rather
// than making its own copy — composability over convenience: the caller
// decides whether to patch the pristine backup (never do this) or a work
// copy (the norm), the same way CmdEdit's own copy-then-patch flow already
// established what "the work image" means for a given backup.
//
// Content is redacted from the returned data by default (BeforeLen/AfterLen
// only) since this command's very reason for existing is patching files
// that can contain secrets (WiFi passwords, tokens) — status.json is a
// persisted file on the board, and there's no reason to write a plaintext
// credential into it by default. Pass --show-diff to see actual content,
// for non-sensitive files.

type PatchFileData struct {
	ImagePath  string `json:"image_path"`
	TargetPath string `json:"target_path"`
	Verified   bool   `json:"verified"`
	BeforeLen  int    `json:"before_len"`
	AfterLen   int    `json:"after_len"`
	Before     string `json:"before,omitempty"`
	After      string `json:"after,omitempty"`
}

func CmdPatchFile(cfg *Config, r *Reporter, args []string) {
	showDiff := false
	var positional []string
	for _, a := range args {
		if a == "--show-diff" {
			showDiff = true
		} else {
			positional = append(positional, a)
		}
	}
	if len(positional) < 3 {
		r.Done(false, nil, "usage: jibotool patch-file <image> <path-in-image> <content-file-on-host> [--show-diff]")
		return
	}
	image, targetPath, contentFile := positional[0], positional[1], positional[2]

	r.Phase(fmt.Sprintf("reading current content of %s", targetPath))
	before, _ := DebugfsCat(image, targetPath) // best-effort — fine if it doesn't exist yet

	r.Phase(fmt.Sprintf("writing new content to %s via debugfs", targetPath))
	if err := DebugfsWriteFile(image, contentFile, targetPath); err != nil {
		r.Done(false, nil, err.Error())
		return
	}

	after, err := DebugfsCat(image, targetPath)
	if err != nil {
		r.Done(false, nil, fmt.Sprintf("post-write read failed: %v", err))
		return
	}

	wantBytes, err := os.ReadFile(contentFile)
	if err != nil {
		r.Done(false, nil, err.Error())
		return
	}
	verified := after == string(wantBytes)

	statOut, _ := DebugfsStat(image, targetPath)
	badType := strings.Contains(statOut, "bad type")

	data := PatchFileData{
		ImagePath:  image,
		TargetPath: targetPath,
		Verified:   verified,
		BeforeLen:  len(before),
		AfterLen:   len(after),
	}
	if showDiff {
		data.Before = before
		data.After = after
	}

	if badType {
		r.Done(false, data, "inode type is 'bad type' after set_inode_field — do NOT write this image back")
		return
	}
	if !verified {
		r.Done(false, data, "verification failed: content read back does not match the source file")
		return
	}

	r.Done(true, data, "")
}

// --- write: EMMC_WRITE + MANDATORY read-back verification -----------------
//
// shofel2_t124's own EMMC_WRITE only checks a single 4-byte status word from
// the ARM payload — confirmed directly from exploit/shofel2_t124.c, it does
// not verify the bytes actually landed correctly. Combined with this exact
// branch's documented history of write corruption, verification here is not
// optional and cannot be skipped by a flag.

type WriteData struct {
	WrittenSHA256  string `json:"written_sha256"`
	ReadBackSHA256 string `json:"readback_sha256"`
	Verified       bool   `json:"verified"`
}

const writeConfirmPhrase = "I_UNDERSTAND_THIS_WRITES_REAL_EMMC"

func CmdWrite(cfg *Config, r *Reporter, args []string) {
	if len(args) < 2 || args[1] != writeConfirmPhrase {
		r.Done(false, nil, fmt.Sprintf(
			"usage: jibotool write <image-path> %s\nThis writes to real eMMC. The confirm phrase must be passed exactly — this is a deliberate gate, not a formality.",
			writeConfirmPhrase))
		return
	}
	image := args[0]

	st, err := LoadState(cfg)
	if err != nil || st.PartitionStartHex == "" {
		r.Done(false, nil, "no partition discovered — run `jibotool discover` first")
		return
	}

	sectors, err := fileSizeSectors(image)
	if err != nil {
		r.Done(false, nil, err.Error())
		return
	}

	r.Phase("waiting for RCM / writing image (~14 min) — do not unplug Jibo")
	writeOut, err := runShofel(cfg, r, []string{"EMMC_WRITE", st.PartitionStartHex, image})
	if err != nil {
		r.Done(false, StatusData{Raw: writeOut}, fmt.Sprintf("EMMC_WRITE failed: %v — DO NOT power-cycle Jibo; eMMC state is unknown", err))
		return
	}

	writtenSum, err := sha256File(image)
	if err != nil {
		r.Done(false, nil, err.Error())
		return
	}

	verifyPath := image + ".verify"
	r.Phase("waiting for RCM again / reading back for verification (~9 min) — DO NOT power-cycle yet")
	verifyReadOut, err := runShofel(cfg, r, []string{"EMMC_READ", st.PartitionStartHex, hexArg(sectors), verifyPath})
	if err != nil {
		r.Done(false, map[string]any{"written_sha256": writtenSum, "raw": verifyReadOut},
			fmt.Sprintf("verification read failed: %v — DO NOT power-cycle Jibo; retry verification before proceeding", err))
		return
	}

	readbackSum, err := sha256File(verifyPath)
	if err != nil {
		r.Done(false, nil, err.Error())
		return
	}

	verified := writtenSum == readbackSum
	wd := WriteData{WrittenSHA256: writtenSum, ReadBackSHA256: readbackSum, Verified: verified}

	if !verified {
		r.Done(false, wd, "VERIFICATION FAILED — data read back does not match what was written. DO NOT power-cycle Jibo. Retry EMMC_WRITE with the same image, or restore from the original backup.")
		return
	}

	r.Done(true, wd, "")
}

// CmdRestore is CmdWrite with restore-specific messaging; the underlying
// safety behavior (mandatory verification, confirm phrase) is identical —
// a restore is still a real write to real eMMC.
func CmdRestore(cfg *Config, r *Reporter, args []string) {
	CmdWrite(cfg, r, args)
}

// --- health: EXT_CSD wear/life estimation --------------------------------

type HealthData struct {
	LifeTimeA string `json:"life_time_estimation_a"`
	LifeTimeB string `json:"life_time_estimation_b"`
	PreEOL    string `json:"pre_eol_info"`
	RawPath   string `json:"raw_path"`
}

var lifeLabels = map[byte]string{0: "normal", 1: "warning (>80% life used)", 2: "critical (>90% life used)"}

func CmdHealth(cfg *Config, r *Reporter, _ []string) {
	ts := time.Now().Format("20060102_150405")
	path := filepath.Join(cfg.WorkDir, fmt.Sprintf("ext_csd_%s.bin", ts))
	_ = os.MkdirAll(cfg.WorkDir, 0o755)

	r.Phase("waiting for RCM / reading EXT_CSD")
	extCsdOut, err := runShofel(cfg, r, []string{"EMMC_READ_EXT_CSD", path})
	if err != nil {
		r.Done(false, StatusData{Raw: extCsdOut}, fmt.Sprintf("EMMC_READ_EXT_CSD failed: %v", err))
		return
	}

	data, err := os.ReadFile(path)
	if err != nil || len(data) < 272 {
		r.Done(false, nil, "EXT_CSD file missing or too short")
		return
	}

	hd := HealthData{
		LifeTimeA: fmt.Sprintf("0x%02x (%s)", data[268], lifeLabels[data[268]]),
		LifeTimeB: fmt.Sprintf("0x%02x (%s)", data[269], lifeLabels[data[269]]),
		PreEOL:    fmt.Sprintf("0x%02x (%s)", data[271], lifeLabels[data[271]]),
		RawPath:   path,
	}
	r.Done(true, hd, "")
}

func writeManifest(dir string, data map[string]any) {
	path := filepath.Join(dir, "manifest.json")
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}
