package main

import "path/filepath"

// Flags holds the subset of flags that can appear on any subcommand.
// Deliberately hand-rolled instead of the stdlib flag package: these need
// to be extractable from anywhere in the argument list while leaving
// command-specific positional args (image paths, the write confirm phrase)
// untouched and in order.
type Flags struct {
	workDir    string
	jobDir     string
	background bool
	fix        bool
	update     bool
}

func extractFlags(args []string) (Flags, []string) {
	var f Flags
	var remaining []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--workdir":
			if i+1 < len(args) {
				f.workDir = args[i+1]
				i++
			}
		case "--job-dir":
			if i+1 < len(args) {
				f.jobDir = args[i+1]
				i++
			}
		case "--background":
			f.background = true
		case "--fix":
			f.fix = true
		case "--update":
			f.update = true
		default:
			remaining = append(remaining, args[i])
		}
	}
	return f, remaining
}

func jobDirBase(jobDir string) string {
	return filepath.Base(jobDir)
}
