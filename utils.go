package main

import (
	"bytes"
	"strings"
)

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func trimNL(s string) string {
	return strings.TrimRight(s, "\r\n")
}

func fixedReader(s string) *bytes.Reader {
	return bytes.NewReader([]byte(s))
}
