package main

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// buildFakeGPT constructs a minimal synthetic GPT sector dump for testing
// ParseGPT without needing real hardware or a real disk image.
func buildFakeGPT(t *testing.T, index int, name string, startLBA, endLBA uint64) []byte {
	t.Helper()
	buf := make([]byte, gptEntriesOffset+(index+1)*gptEntrySize)
	copy(buf[gptHeaderOffset:], []byte(gptSignature))

	entryOff := gptEntriesOffset + index*gptEntrySize
	// A real GPT entry's first 16 bytes are the partition type GUID.
	// FindPartitionBySize treats an all-zero type GUID as "unused slot" —
	// matching real GPT semantics — so a fake entry needs a non-zero one
	// here or it gets (correctly) skipped as empty.
	for i := 0; i < 16; i++ {
		buf[entryOff+i] = 0xAB
	}
	binary.LittleEndian.PutUint64(buf[entryOff+32:], startLBA)
	binary.LittleEndian.PutUint64(buf[entryOff+40:], endLBA)

	u16 := utf16.Encode([]rune(name))
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(buf[entryOff+56+i*2:], v)
	}
	return buf
}

func TestParseGPT_HappyPath(t *testing.T) {
	data := buildFakeGPT(t, 4, "var", 2103296, 3151871)
	part, err := ParseGPT(data, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if part.Name != "var" {
		t.Errorf("Name = %q, want %q", part.Name, "var")
	}
	if part.StartLBA != 2103296 {
		t.Errorf("StartLBA = %d, want %d", part.StartLBA, 2103296)
	}
	wantSize := uint64(3151871 - 2103296 + 1)
	if part.SizeSectors != wantSize {
		t.Errorf("SizeSectors = %d, want %d", part.SizeSectors, wantSize)
	}
	// The bug this whole project found: sector args must round-trip as hex.
	if hexArg(part.StartLBA) != "0x201800" {
		t.Errorf("hexArg(StartLBA) = %s, want 0x201800", hexArg(part.StartLBA))
	}
}

func TestParseGPT_MissingSignature(t *testing.T) {
	data := make([]byte, gptEntriesOffset+5*gptEntrySize) // all zeros, no "EFI PART"
	_, err := ParseGPT(data, 4)
	if err == nil {
		t.Fatal("expected error for missing GPT signature, got nil")
	}
}

func TestParseGPT_TruncatedDump(t *testing.T) {
	data := buildFakeGPT(t, 4, "var", 100, 200)
	truncated := data[:len(data)-10]
	_, err := ParseGPT(truncated, 4)
	if err == nil {
		t.Fatal("expected error for truncated dump, got nil")
	}
}

func TestParseGPT_ImplausibleEntry(t *testing.T) {
	data := buildFakeGPT(t, 4, "var", 5000, 100) // end < start
	_, err := ParseGPT(data, 4)
	if err == nil {
		t.Fatal("expected error for end LBA < start LBA, got nil")
	}
}

func TestFindPartitionBySize_FindsMatchAtWrongIndex(t *testing.T) {
	// Simulate the exact scenario the fallback exists for: the /var-sized
	// partition isn't at the expected index 4, it's at index 6 instead.
	sizeSectors := uint64(500 * 1024 * 1024 / sectorSize) // 500MB
	data := buildFakeGPT(t, 6, "var", 1000, 1000+sizeSectors-1)

	minSec := uint64(450) * 1024 * 1024 / sectorSize
	maxSec := uint64(550) * 1024 * 1024 / sectorSize

	part, err := FindPartitionBySize(data, minSec, maxSec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if part.Index != 6 {
		t.Errorf("Index = %d, want 6", part.Index)
	}
	if part.Name != "var" {
		t.Errorf("Name = %q, want %q", part.Name, "var")
	}
}

func TestFindPartitionBySize_NoMatch(t *testing.T) {
	data := buildFakeGPT(t, 4, "var", 1000, 1999) // ~500KB, way outside a 450-550MB window
	minSec := uint64(450) * 1024 * 1024 / sectorSize
	maxSec := uint64(550) * 1024 * 1024 / sectorSize
	_, err := FindPartitionBySize(data, minSec, maxSec)
	if err == nil {
		t.Fatal("expected no-match error, got nil")
	}
}

func TestFindPartitionBySize_MissingSignature(t *testing.T) {
	data := make([]byte, gptEntriesOffset+5*gptEntrySize)
	_, err := FindPartitionBySize(data, 0, 1000000)
	if err == nil {
		t.Fatal("expected error for missing GPT signature, got nil")
	}
}
