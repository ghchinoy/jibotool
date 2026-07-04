package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// GPT layout constants — fixed by the UEFI spec, not configurable:
// header at LBA 1, partition entry array starting at LBA 2, 128 bytes per
// entry. This replaces the Python heredoc that did the same parsing in
// jibo-unlock.sh, with the same signature-check safety behavior: if this
// isn't really a GPT disk, ParseGPT returns an error rather than guessing.
const (
	sectorSize       = 512
	gptHeaderOffset  = 1 * sectorSize
	gptEntriesOffset = 2 * sectorSize
	gptEntrySize     = 128
	gptSignature     = "EFI PART"
)

type GPTPartition struct {
	Index       int
	Name        string
	StartLBA    uint64
	EndLBA      uint64
	SizeSectors uint64
}

// ParseGPT reads the GPT header + one partition entry (by zero-based index)
// out of a raw sector dump (e.g. the first 34 sectors read via EMMC_READ).
func ParseGPT(data []byte, index int) (*GPTPartition, error) {
	needed := gptEntriesOffset + (index+1)*gptEntrySize
	if len(data) < needed {
		return nil, fmt.Errorf("dump too small (%d bytes) to contain partition entry %d (need %d bytes)", len(data), index, needed)
	}

	sig := data[gptHeaderOffset : gptHeaderOffset+8]
	if !bytes.Equal(sig, []byte(gptSignature)) {
		return nil, fmt.Errorf("GPT signature not found at LBA 1 (got %q) — this may not be a GPT disk; do not assume partition offsets without independent confirmation", sig)
	}

	off := gptEntriesOffset + index*gptEntrySize
	entry := data[off : off+gptEntrySize]

	startLBA := binary.LittleEndian.Uint64(entry[32:40])
	endLBA := binary.LittleEndian.Uint64(entry[40:48])
	name := decodeUTF16LEName(entry[56:128])

	if endLBA < startLBA {
		return nil, fmt.Errorf("implausible partition entry: end LBA %d < start LBA %d", endLBA, startLBA)
	}

	return &GPTPartition{
		Index:       index,
		Name:        name,
		StartLBA:    startLBA,
		EndLBA:      endLBA,
		SizeSectors: endLBA - startLBA + 1,
	}, nil
}

// maxGPTEntriesToScan is the UEFI-spec default partition entry count. The
// 34-sector GPT dump jibotool reads (MBR + header + up to 128 entries at
// 128 bytes each = 32 sectors) is sized to cover exactly this many entries.
const maxGPTEntriesToScan = 128

// FindPartitionBySize scans every partition entry in a GPT dump and returns
// the first one whose size falls within [minSectors, maxSectors].
//
// This exists because a fixed partition index isn't bulletproof: the
// Jibo Revival Group's JiboAutoMod tool (independently reviewed) uses the
// same primary heuristic we do — partition index 4 ("partition 5") — but
// falls back to exactly this kind of size-based scan (450-550MB) when that
// doesn't pan out, rather than trusting the fixed index unconditionally.
// This is that same fallback, translated into Go.
func FindPartitionBySize(data []byte, minSectors, maxSectors uint64) (*GPTPartition, error) {
	if len(data) < gptHeaderOffset+8 {
		return nil, fmt.Errorf("dump too small (%d bytes) to contain a GPT header", len(data))
	}
	sig := data[gptHeaderOffset : gptHeaderOffset+8]
	if !bytes.Equal(sig, []byte(gptSignature)) {
		return nil, fmt.Errorf("GPT signature not found at LBA 1 (got %q)", sig)
	}

	for i := 0; i < maxGPTEntriesToScan; i++ {
		off := gptEntriesOffset + i*gptEntrySize
		if off+gptEntrySize > len(data) {
			break
		}
		entry := data[off : off+gptEntrySize]
		if isZero(entry[0:16]) { // type GUID all-zero = unused entry slot
			continue
		}
		startLBA := binary.LittleEndian.Uint64(entry[32:40])
		endLBA := binary.LittleEndian.Uint64(entry[40:48])
		if endLBA < startLBA {
			continue
		}
		size := endLBA - startLBA + 1
		if size >= minSectors && size <= maxSectors {
			return &GPTPartition{
				Index:       i,
				Name:        decodeUTF16LEName(entry[56:128]),
				StartLBA:    startLBA,
				EndLBA:      endLBA,
				SizeSectors: size,
			}, nil
		}
	}
	return nil, fmt.Errorf("no partition found with size between %d and %d sectors (%.0f-%.0f MB)",
		minSectors, maxSectors,
		float64(minSectors)*sectorSize/1024/1024, float64(maxSectors)*sectorSize/1024/1024)
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func decodeUTF16LEName(b []byte) string {
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		v := uint16(b[i]) | uint16(b[i+1])<<8
		if v == 0 {
			break
		}
		u16 = append(u16, v)
	}
	return string(utf16.Decode(u16))
}
