package main

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// shofel2_t124's argument parsing uses sscanf("%x", ...) for sector numbers
// — hex, not decimal. This was a real bug found and fixed in jibo-unlock.sh;
// codifying it here as a single formatting function so it can never regress.
// (Confirmed against exploit/shofel2_t124.c directly, not inferred from docs.)
func hexArg(n uint64) string {
	return fmt.Sprintf("0x%x", n)
}

// ProgressInfo captures one parsed "Progress: X / Y MB (Z%) [Ws]" line from
// shofel2_t124's EMMC_READ/EMMC_WRITE output.
type ProgressInfo struct {
	MBDone   float64
	MBTotal  float64
	Percent  float64
	ElapsedS int
}

var progressRe = regexp.MustCompile(`Progress:\s*([\d.]+)\s*/\s*([\d.]+)\s*MB\s*\(([\d.]+)%\)\s*\[(\d+)s\]`)
var errorRe = regexp.MustCompile(`^Error:\s*(.+)$`)

// LineEvent is what a single line (shofel2_t124 delimits progress updates
// with \r, not \n, so a custom scanner splits on either) means for the
// caller's state machine.
type LineEvent struct {
	Raw           string
	WaitingForRCM bool
	Connected     bool
	Progress      *ProgressInfo
	ErrorMsg      string
	Complete      bool // "Read complete." / "Write complete." / "Erase complete." etc.
}

func parseShofelLine(line string) LineEvent {
	ev := LineEvent{Raw: line}
	trimmed := strings.TrimSpace(line)

	switch {
	case strings.Contains(trimmed, "Waiting T124 to enter RCM mode"):
		ev.WaitingForRCM = true
	case strings.Contains(trimmed, "K1 in RCM mode connected."):
		ev.Connected = true
	case strings.Contains(trimmed, "complete."):
		ev.Complete = true
	}

	if m := progressRe.FindStringSubmatch(trimmed); m != nil {
		mbDone, _ := strconv.ParseFloat(m[1], 64)
		mbTotal, _ := strconv.ParseFloat(m[2], 64)
		pct, _ := strconv.ParseFloat(m[3], 64)
		elapsed, _ := strconv.Atoi(m[4])
		ev.Progress = &ProgressInfo{MBDone: mbDone, MBTotal: mbTotal, Percent: pct, ElapsedS: elapsed}
	}

	if m := errorRe.FindStringSubmatch(trimmed); m != nil {
		ev.ErrorMsg = m[1]
	}

	return ev
}

// crOrLFSplit is a bufio.SplitFunc that treats \r and \n both as line
// terminators. shofel2_t124 uses \r for in-place progress updates, which a
// plain bufio.Scanner (splits on \n only) would otherwise buffer for
// minutes before ever emitting a line.
func crOrLFSplit(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// RunShofel2 runs the built shofel2_t124 binary with the given args,
// streaming stdout line-by-line through onEvent as it's produced (so a
// caller can detect "waiting for RCM" and report it back to a human in
// real time, rather than only finding out after the process exits).
//
// Returns the full captured output and the process's error (nil on exit 0).
func RunShofel2(cfg *Config, args []string, onEvent func(LineEvent)) (string, error) {
	cmd := exec.Command(cfg.BinaryPath, args...)
	// shofel2_t124 opens its ARM payload files (emmc_server.bin etc.) by
	// relative path — confirmed by live testing against real hardware,
	// where running it with an unrelated CWD produced "Error: Couldn't
	// open the payload file: emmc_server.bin." despite the binary and its
	// payloads sitting right next to each other in RepoDir. The RCM
	// handshake itself still proceeded (misleadingly), so this failure
	// mode is easy to miss without checking exit status carefully.
	cmd.Dir = cfg.RepoDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = cmd.Stdout // shofel2_t124 mixes progress into stdout; capture both together in order isn't guaranteed across pipes, so mirror stderr into a second reader is unnecessary here — errors of interest are printed to stdout per the source.

	if err := cmd.Start(); err != nil {
		return "", err
	}

	var full strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	scanner.Split(crOrLFSplit)

	for scanner.Scan() {
		line := scanner.Text()
		full.WriteString(line)
		full.WriteString("\n")
		if onEvent != nil {
			onEvent(parseShofelLine(line))
		}
	}

	waitErr := cmd.Wait()
	return full.String(), waitErr
}
