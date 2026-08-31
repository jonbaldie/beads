package main

import (
	"os"
	"path/filepath"
	"strings"
)

// beadsIntegrationMarker is the substring present in agent instruction files
// (AGENTS.md / CLAUDE.md) that bd manages via its integration block.
const beadsIntegrationMarker = "BEGIN BEADS INTEGRATION"

// primeDivergenceReminder returns a one-line reminder when both AGENTS.md and
// CLAUDE.md in workspaceDir exist as independent regular files (not symlinks,
// not sharing an inode) that each contain the bd integration marker.
//
// In every other case it returns the empty string and performs no work beyond
// a couple of cheap Lstat calls, so the common case (the condition does not
// hold) adds zero output and negligible cost. workspaceDir may be "" to mean
// the current working directory.
func primeDivergenceReminder(workspaceDir string) string {
	if workspaceDir == "" {
		workspaceDir = "."
	}

	agentsPath := filepath.Join(workspaceDir, "AGENTS.md")
	claudePath := filepath.Join(workspaceDir, "CLAUDE.md")

	if !independentInstructionFiles(agentsPath, claudePath) {
		return ""
	}
	if !instructionFilesContainMarker(agentsPath, claudePath) {
		return ""
	}

	return "\n> **Note**: AGENTS.md and CLAUDE.md are independent files (not symlinked and not sharing an inode). Mirror substantive edits across both, or symlink one to the other.\n"
}

func independentInstructionFiles(agentsPath, claudePath string) bool {
	// Lstat (not Stat) so symlinks are detected rather than followed.
	agentsInfo, err := os.Lstat(agentsPath)
	if err != nil {
		return false
	}
	claudeInfo, err := os.Lstat(claudePath)
	if err != nil {
		return false
	}
	// Both must be regular, independent files. Since neither is a symlink,
	// the Lstat results carry the identity os.SameFile compares.
	if !regularInstructionFile(agentsInfo) || !regularInstructionFile(claudeInfo) {
		return false
	}
	return !os.SameFile(agentsInfo, claudeInfo)
}

func regularInstructionFile(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func instructionFilesContainMarker(agentsPath, claudePath string) bool {
	return fileContainsMarker(agentsPath) && fileContainsMarker(claudePath)
}

// fileContainsMarker reports whether the file at path contains the bd
// integration marker. Any read error yields false (treated as "no marker").
func fileContainsMarker(path string) bool {
	// #nosec G304 -- path is composed from a caller-controlled workspace dir
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), beadsIntegrationMarker)
}
