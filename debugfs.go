package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"path"
	"strings"
)

// This package shells out to e2fsprogs' debugfs rather than reimplementing
// ext4 read/write in Go. debugfs is mature, widely trusted for exactly this
// kind of offline image surgery, and this is the same tool jibo-unlock.sh
// already used and had reviewed — no reason to take on the risk of a
// hand-rolled ext4 writer for a hardware-safety-critical edit.

// debugfsReadOnly runs a single -R command and returns stdout and stderr
// separately. This matters: debugfs prints its own version banner
// ("debugfs 1.44.5 (...)") to stderr on every invocation. Using
// CombinedOutput() (as an earlier version of this file did) merges that
// banner into what should be pure file content — found live, on real
// hardware, when it silently broke an exact-string equality check in
// CmdEdit's mode.json verification. Callers that need pure content (like
// DebugfsCat) get stdout alone; callers that want full context for a human
// or a substring-only check (like DebugfsLs's use in LooksLikeValidVar) can
// still see stderr via the second return value.
func debugfsReadOnly(image, cmd string) (stdout string, stderr string, err error) {
	c := exec.Command("debugfs", "-R", cmd, image)
	var outBuf, errBuf bytes.Buffer
	c.Stdout = &outBuf
	c.Stderr = &errBuf
	err = c.Run()
	return outBuf.String(), errBuf.String(), err
}

// DebugfsCat returns the exact contents of a file inside an ext4 image —
// stdout only, no debugfs banner or diagnostic text mixed in.
func DebugfsCat(image, path string) (string, error) {
	out, _, err := debugfsReadOnly(image, fmt.Sprintf("cat %s", path))
	return out, err
}

// DebugfsLs returns the `ls -l` listing of a directory inside the image
// (stdout only).
func DebugfsLs(image, path string) (string, error) {
	out, _, err := debugfsReadOnly(image, fmt.Sprintf("ls -l %s", path))
	return out, err
}

// DebugfsStat returns the `stat` output for a path inside the image
// (stdout only).
func DebugfsStat(image, path string) (string, error) {
	out, _, err := debugfsReadOnly(image, fmt.Sprintf("stat %s", path))
	return out, err
}

// DebugfsWriteFile writes hostFile into the image at imagePath and — this is
// the detail that actually matters — explicitly fixes the inode's mode/uid/gid.
// If mode is left at a bare 0644 (missing the S_IFREG type bits), debugfs
// reports the inode as "Type: bad type" and Jibo's init silently ignores the
// file. This exact failure mode is why jibo-unlock.sh calls this out
// prominently; codified here so it can't be forgotten in a rewrite.
//
// Also found live (and independently confirmed against the Jibo Revival
// Group's JiboAutoMod tool, which does the same thing): debugfs's `write`
// command refuses to write over a path that already exists
// ("Ext2 file already exists"), and — critically — that failure did NOT
// surface as a non-zero process exit code, so the write silently did
// nothing while jibotool reported success. `rm` the target first.
//
// A second, separate bug found live: the board's debugfs (1.44.5, from
// 2018 — much older than a typical dev machine's) silently fails to link
// the new inode into its parent directory when `write`'s destination is
// given as a full path (e.g. "write hostfile /jibo/mode.json") — it
// allocates the inode, but the file never shows up in `ls /jibo`
// afterward, and every subsequent lookup by that path fails with
// "File not found by ext2_lookup". `cd`-ing into the parent directory
// first and using a bare filename for rm/write/set_inode_field avoids it
// entirely, and was confirmed to work correctly on both the board's 1.44.5
// and a current 1.47.4 debugfs.
func DebugfsWriteFile(image, hostFile, imagePath string) error {
	dir := path.Dir(imagePath)
	base := path.Base(imagePath)

	script := strings.Join([]string{
		fmt.Sprintf("cd %s", dir),
		fmt.Sprintf("rm %s", base), // ignore failure here — fine if it didn't exist yet
		fmt.Sprintf("write %s %s", hostFile, base),
		fmt.Sprintf("set_inode_field %s mode 0100644", base),
		fmt.Sprintf("set_inode_field %s uid 0", base),
		fmt.Sprintf("set_inode_field %s gid 0", base),
		"",
	}, "\n")

	cmd := exec.Command("debugfs", "-w", image)
	cmd.Stdin = bytes.NewBufferString(script)
	out, err := cmd.CombinedOutput() // fine here — we're checking for a specific substring, not doing exact comparison
	if err != nil {
		return fmt.Errorf("debugfs -w failed: %w (output: %s)", err, string(out))
	}
	outStr := string(out)
	if strings.Contains(outStr, "already exists") ||
		strings.Contains(outStr, "File not found") ||
		!strings.Contains(outStr, "Allocated inode") {
		// debugfs doesn't propagate a sub-command failure as a process exit
		// code, so this is the only signal available that `write` itself
		// didn't do what we expect (this exact scenario — "Ext2 file
		// already exists" with exit code 0 — is what caused a silent
		// no-op write on real hardware). Surface it explicitly rather
		// than reporting success on an unwritten file.
		return fmt.Errorf("debugfs write command may have failed (expected an 'Allocated inode' confirmation): %s", truncate(outStr, 500))
	}
	return nil
}

// LooksLikeValidVar applies the same manual-review heuristic jibo-unlock.sh
// asked a human to eyeball: does this partition actually contain
// /jibo/mode.json? A valid GPT signature alone doesn't prove partition
// index 4 is really Jibo's /var on this specific board (Tegra devices often
// layer an NVIDIA BCT/PT scheme too) — this checks the actual filesystem
// contents, not just the partition table entry.
func LooksLikeValidVar(image string) (bool, string, error) {
	rootLs, err := DebugfsLs(image, "/")
	if err != nil {
		return false, "", err
	}
	jiboLs, err := DebugfsLs(image, "/jibo")
	if err != nil {
		// Not fatal — the directory listing itself failing is strong
		// evidence this isn't the right partition, report it as such
		// rather than erroring the whole command out.
		return false, rootLs, nil
	}
	looksValid := strings.Contains(jiboLs, "mode.json")
	combined := "== / ==\n" + rootLs + "\n== /jibo ==\n" + jiboLs
	return looksValid, combined, nil
}
