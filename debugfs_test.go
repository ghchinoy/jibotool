package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireTool skips the test if the given binary isn't on PATH — these
// tests exercise real debugfs/mke2fs behavior and aren't meaningful
// without them, but shouldn't break a build environment that lacks them.
func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not found on PATH, skipping", name)
	}
}

// buildTestExt4Image creates a small ext4 image with one pre-existing file
// at the given path, so tests can exercise the "overwrite an existing
// file" scenario — which is exactly the case that silently failed on real
// hardware (debugfs's `write` command errors on an existing target with
// "Ext2 file already exists", but without a non-zero process exit code).
func buildTestExt4Image(t *testing.T, existingPath, existingContent string) string {
	t.Helper()
	requireTool(t, "mke2fs")
	requireTool(t, "debugfs")

	dir := t.TempDir()
	image := filepath.Join(dir, "test.img")

	// 8MB is comfortably enough for a minimal ext4 filesystem + one tiny file.
	f, err := os.Create(image)
	if err != nil {
		t.Fatalf("failed to create image file: %v", err)
	}
	if err := f.Truncate(8 * 1024 * 1024); err != nil {
		t.Fatalf("failed to size image file: %v", err)
	}
	f.Close()

	if out, err := exec.Command("mke2fs", "-q", "-t", "ext4", "-F", image).CombinedOutput(); err != nil {
		t.Fatalf("mke2fs failed: %v: %s", err, out)
	}

	// Create the directory structure and seed the pre-existing file via debugfs.
	hostFile := filepath.Join(dir, "seed.txt")
	if err := os.WriteFile(hostFile, []byte(existingContent), 0o644); err != nil {
		t.Fatalf("failed to write seed file: %v", err)
	}

	dirPath := filepath.Dir(existingPath)
	if dirPath != "/" {
		mkdirCmd := exec.Command("debugfs", "-w", image)
		mkdirCmd.Stdin = strings.NewReader("mkdir " + dirPath + "\n")
		_, _ = mkdirCmd.CombinedOutput() // ignore error — "/" always exists, nested dirs created as needed below
	}

	seedCmd := exec.Command("debugfs", "-w", image)
	seedCmd.Stdin = strings.NewReader("write " + hostFile + " " + existingPath + "\n")
	if out, err := seedCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to seed existing file: %v: %s", err, out)
	}

	return image
}

func TestDebugfsWriteFile_OverwritesExistingFile(t *testing.T) {
	// Regression test for two real bugs found on live hardware, both
	// triggered by exactly this scenario — overwriting an existing file
	// one directory level deep (/jibo/mode.json, not root-level):
	//
	//  1. debugfs's `write` command fails with "Ext2 file already exists"
	//     when the target path is already present, without a non-zero
	//     process exit code — an earlier version of DebugfsWriteFile
	//     silently left the original content untouched while reporting
	//     success. Fix: `rm` the target before `write`.
	//  2. On the board's debugfs (1.44.5, from 2018 — this may not
	//     reproduce against a newer local debugfs, e.g. 1.47.4 via
	//     Homebrew, which handled the absolute-path form fine), `write`
	//     with a full path as the destination (e.g.
	//     "write hostfile /jibo/mode.json") allocated an inode but never
	//     linked it into the parent directory — the file simply vanished
	//     from `ls /jibo` afterward. Fix: `cd` into the parent directory
	//     and use a bare filename for rm/write/set_inode_field.
	image := buildTestExt4Image(t, "/jibo/mode.json", `{"mode":"normal"}`)

	newContent := `{"mode":"int-developer"}`
	tmpFile, err := os.CreateTemp(t.TempDir(), "new_mode_*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	if _, err := tmpFile.WriteString(newContent); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	tmpFile.Close()

	if err := DebugfsWriteFile(image, tmpFile.Name(), "/jibo/mode.json"); err != nil {
		t.Fatalf("DebugfsWriteFile failed: %v", err)
	}

	// This is the second bug's exact symptom: the file existing in `ls`
	// output is a distinct check from `cat` succeeding, because the bug
	// was specifically about the directory link, not the inode itself.
	lsOut, err := DebugfsLs(image, "/jibo")
	if err != nil {
		t.Fatalf("DebugfsLs failed: %v", err)
	}
	if !strings.Contains(lsOut, "mode.json") {
		t.Errorf("mode.json missing from `ls /jibo` after write — this is exactly the silent directory-unlink bug found on real hardware. Listing:\n%s", lsOut)
	}

	got, err := DebugfsCat(image, "/jibo/mode.json")
	if err != nil {
		t.Fatalf("DebugfsCat failed: %v", err)
	}
	if got != newContent {
		t.Errorf("content after write = %q, want %q (this is the exact failure mode found on real hardware — the write silently no-opped)", got, newContent)
	}

	statOut, err := DebugfsStat(image, "/jibo/mode.json")
	if err != nil {
		t.Fatalf("DebugfsStat failed: %v", err)
	}
	if !containsAll(statOut, "Type: regular", "Mode:  0644") {
		t.Errorf("unexpected stat output, want regular type + mode 0644, got: %s", statOut)
	}
}

func TestDebugfsCat_NoBannerLeaksIntoContent(t *testing.T) {
	// Regression test for the second bug found alongside the first:
	// CombinedOutput() merged debugfs's stderr version banner
	// ("debugfs 1.44.5 (...)") into what should be pure file content,
	// breaking exact-string verification in CmdEdit.
	image := buildTestExt4Image(t, "/plain.txt", "hello world")

	got, err := DebugfsCat(image, "/plain.txt")
	if err != nil {
		t.Fatalf("DebugfsCat failed: %v", err)
	}
	if got != "hello world" {
		t.Errorf("DebugfsCat returned %q, want exactly %q (no banner text should be present)", got, "hello world")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}
