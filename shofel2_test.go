package main

import "testing"

func TestHexArg(t *testing.T) {
	cases := map[uint64]string{
		0:       "0x0",
		34:      "0x22",
		2103296: "0x201800",
	}
	for in, want := range cases {
		if got := hexArg(in); got != want {
			t.Errorf("hexArg(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseShofelLine_Progress(t *testing.T) {
	line := "  Progress: 128.0 / 500.0 MB (25.6%) [42s]   "
	ev := parseShofelLine(line)
	if ev.Progress == nil {
		t.Fatal("expected progress to be parsed, got nil")
	}
	if ev.Progress.MBDone != 128.0 || ev.Progress.MBTotal != 500.0 {
		t.Errorf("got MBDone=%v MBTotal=%v", ev.Progress.MBDone, ev.Progress.MBTotal)
	}
	if ev.Progress.Percent != 25.6 {
		t.Errorf("got Percent=%v, want 25.6", ev.Progress.Percent)
	}
	if ev.Progress.ElapsedS != 42 {
		t.Errorf("got ElapsedS=%v, want 42", ev.Progress.ElapsedS)
	}
}

func TestParseShofelLine_WaitingForRCM(t *testing.T) {
	ev := parseShofelLine("Waiting T124 to enter RCM mode (ctrl-c to cancel). Note: root permission could be required.")
	if !ev.WaitingForRCM {
		t.Error("expected WaitingForRCM = true")
	}
}

func TestParseShofelLine_Connected(t *testing.T) {
	ev := parseShofelLine("K1 in RCM mode connected.")
	if !ev.Connected {
		t.Error("expected Connected = true")
	}
}

func TestParseShofelLine_Error(t *testing.T) {
	ev := parseShofelLine("Error: eMMC write failed with status 0xdead0007.")
	if ev.ErrorMsg == "" {
		t.Error("expected ErrorMsg to be captured")
	}
}

func TestIsHandshakeStageFailure_RetryableBeforeConnect(t *testing.T) {
	out := "Waiting T124 to enter RCM mode...\nError: Couldn't read Chip ID. Please reset T124 in RCM mode again.\n"
	if !isHandshakeStageFailure(out, false) {
		t.Error("expected handshake-stage failure to be retryable when Connected was never seen")
	}
}

func TestIsHandshakeStageFailure_NotRetryableAfterConnect(t *testing.T) {
	// Even though the output contains a matching signature substring, once
	// the handshake succeeded (connected=true), a later failure must NOT
	// be treated as retryable — this is the guard against silently
	// re-attempting a failed write.
	out := "K1 in RCM mode connected.\nError: eMMC write failed with status 0xdead0007.\n"
	if isHandshakeStageFailure(out, true) {
		t.Error("expected failure after Connected=true to NOT be retryable")
	}
}

func TestIsHandshakeStageFailure_UnrelatedErrorNotRetryable(t *testing.T) {
	out := "K1 in RCM mode connected.\nError: something else entirely.\n"
	if isHandshakeStageFailure(out, false) {
		t.Error("expected an error with no matching handshake signature to not be treated as a handshake failure")
	}
}

func TestCrOrLFSplit_HandlesCarriageReturnProgress(t *testing.T) {
	data := []byte("line one\r  Progress: 1 / 2 MB (50.0%) [1s]   \rline two\n")
	var tokens []string
	start := 0
	for start < len(data) {
		adv, tok, err := crOrLFSplit(data[start:], true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if adv == 0 {
			break
		}
		tokens = append(tokens, string(tok))
		start += adv
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens (split on \\r and \\n), got %d: %#v", len(tokens), tokens)
	}
	if tokens[0] != "line one" || tokens[2] != "line two" {
		t.Errorf("unexpected tokens: %#v", tokens)
	}
}
