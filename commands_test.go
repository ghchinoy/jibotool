package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// runCmdForTest invokes a Cmd* function with a Reporter backed by a temp
// job directory, then reads back and decodes the resulting status.json —
// exercising the real Reporter/Done() code path rather than just the
// lower-level helpers.
func runCmdForTest(t *testing.T, fn func(*Config, *Reporter, []string), args []string) map[string]any {
	t.Helper()
	jobDir := t.TempDir()
	r := NewReporter("test", jobDir)
	fn(&Config{}, r, args)

	b, err := os.ReadFile(filepath.Join(jobDir, "status.json"))
	if err != nil {
		t.Fatalf("failed to read status.json: %v", err)
	}
	var status map[string]any
	if err := json.Unmarshal(b, &status); err != nil {
		t.Fatalf("failed to parse status.json: %v", err)
	}
	return status
}

func TestCmdPatchFile_RedactsContentByDefault(t *testing.T) {
	image := buildTestExt4Image(t, "/etc/wpa_supplicant.conf", `network={ssid="OldNetwork" psk="oldsecret"}`)

	newContent := `network={ssid="NewNetwork" psk="newsecret"}`
	contentFile := filepath.Join(t.TempDir(), "new_wpa.conf")
	if err := os.WriteFile(contentFile, []byte(newContent), 0o644); err != nil {
		t.Fatalf("failed to write content file: %v", err)
	}

	status := runCmdForTest(t, CmdPatchFile, []string{image, "/etc/wpa_supplicant.conf", contentFile})

	if ok, _ := status["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got status: %+v", status)
	}
	data, _ := status["data"].(map[string]any)
	if data == nil {
		t.Fatalf("expected data field, got status: %+v", status)
	}
	if verified, _ := data["verified"].(bool); !verified {
		t.Errorf("expected verified=true, got %+v", data)
	}
	// This is the important assertion: neither the old nor new secret
	// should appear anywhere in status.json by default, since that file
	// persists on disk and could be pulled/viewed independently of the
	// command that produced it.
	rendered, _ := json.Marshal(status)
	if contains(string(rendered), "oldsecret") || contains(string(rendered), "newsecret") {
		t.Errorf("secret content leaked into status.json without --show-diff: %s", rendered)
	}
	if _, present := data["before"]; present {
		t.Errorf("expected 'before' field to be omitted by default, got: %+v", data)
	}
	if _, present := data["after"]; present {
		t.Errorf("expected 'after' field to be omitted by default, got: %+v", data)
	}
	if beforeLen, _ := data["before_len"].(float64); beforeLen == 0 {
		t.Errorf("expected before_len to reflect original content length, got %v", data["before_len"])
	}
}

func TestCmdPatchFile_ShowDiffRevealsContent(t *testing.T) {
	image := buildTestExt4Image(t, "/plain.txt", "old content")

	contentFile := filepath.Join(t.TempDir(), "new.txt")
	if err := os.WriteFile(contentFile, []byte("new content"), 0o644); err != nil {
		t.Fatalf("failed to write content file: %v", err)
	}

	status := runCmdForTest(t, CmdPatchFile, []string{image, "/plain.txt", contentFile, "--show-diff"})

	data, _ := status["data"].(map[string]any)
	if data["before"] != "old content" {
		t.Errorf("before = %v, want %q", data["before"], "old content")
	}
	if data["after"] != "new content" {
		t.Errorf("after = %v, want %q", data["after"], "new content")
	}
}

func TestCmdPatchFile_UsageErrorOnMissingArgs(t *testing.T) {
	status := runCmdForTest(t, CmdPatchFile, []string{"image.img", "/only/one/more"})
	if ok, _ := status["ok"].(bool); ok {
		t.Error("expected ok=false for missing arguments")
	}
	errMsg, _ := status["error"].(string)
	if !contains(errMsg, "usage:") {
		t.Errorf("expected usage error message, got: %s", errMsg)
	}
}
