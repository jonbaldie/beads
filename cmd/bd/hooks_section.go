package main

import (
	"fmt"
	"strings"
)

// generateHookSection returns the marked section content for a given hook name.
// The section is self-contained: it checks for bd availability, runs the hook
// via 'bd hooks run', and propagates exit codes — without preventing any user
// content after the section from executing on success.
//
// Resilience (GH#2453, GH#2449):
//   - A compatible timeout helper applies a best-effort soft deadline.
//   - BEADS_HOOK_TIMEOUT accepts positive whole seconds only.
//   - Helper argv is separated with -- so user input cannot become an option.
//   - If no compatible helper exists, the direct fallback is explicitly
//     unbounded rather than silently pretending to enforce a deadline.
//   - Only GNU coreutils timeout implementations are selected; Windows
//     timeout.exe has the same name but an incompatible command line (GH#5503).
//   - If the beads database is not initialized (exit code 3), the hook exits
//     successfully with a warning so that git operations are not blocked.
func generateHookSection(hookName string) string {
	return hookSectionBeginLine() + "\n" +
		"# This section is managed by beads. Do not remove these markers.\n" +
		"if command -v bd >/dev/null 2>&1; then\n" +
		"  export BD_GIT_HOOK=1\n" +
		"  _bd_timeout=${BEADS_HOOK_TIMEOUT:-" + fmt.Sprintf("%d", hookTimeoutSeconds) + "}\n" +
		"  case \"$_bd_timeout\" in\n" +
		"    *[!0-9]*|'') _bd_timeout_invalid=1 ;;\n" +
		"    *[1-9]*) _bd_timeout_invalid=0 ;;\n" +
		"    *) _bd_timeout_invalid=1 ;;\n" +
		"  esac\n" +
		"  if [ \"$_bd_timeout_invalid\" -eq 1 ]; then\n" +
		"    echo >&2 \"beads: invalid BEADS_HOOK_TIMEOUT; using " + fmt.Sprintf("%d", hookTimeoutSeconds) + " seconds\"\n" +
		"    _bd_timeout=" + fmt.Sprintf("%d", hookTimeoutSeconds) + "\n" +
		"  fi\n" +
		"  _bd_timeout_backend=none\n" +
		"  _bd_timeout_command=\n" +
		"  for _bd_timeout_candidate in timeout gtimeout; do\n" +
		"    if command -v \"$_bd_timeout_candidate\" >/dev/null 2>&1; then\n" +
		"      if _bd_timeout_version=\"$(\"$_bd_timeout_candidate\" --version 2>/dev/null)\"; then\n" +
		"        case \"$_bd_timeout_version\" in\n" +
		"          \"timeout (GNU coreutils) \"*) _bd_timeout_command=$_bd_timeout_candidate; break ;;\n" +
		"        esac\n" +
		"      fi\n" +
		"    fi\n" +
		"  done\n" +
		"  if [ -n \"$_bd_timeout_command\" ]; then\n" +
		"    _bd_timeout_backend=coreutils\n" +
		"    if \"$_bd_timeout_command\" -- \"$_bd_timeout\" bd hooks run " + hookName + " \"$@\"; then\n" +
		"      _bd_exit=0\n" +
		"    else\n" +
		"      _bd_exit=$?\n" +
		"    fi\n" +
		"  elif command -v perl >/dev/null 2>&1; then\n" +
		"    _bd_timeout_backend=perl\n" +
		"    if perl -e 'alarm shift; exec @ARGV' -- \"$_bd_timeout\" bd hooks run " + hookName + " \"$@\"; then\n" +
		"      _bd_exit=0\n" +
		"    else\n" +
		"      _bd_exit=$?\n" +
		"    fi\n" +
		"  else\n" +
		"    echo >&2 \"beads: hook '" + hookName + "' running without timeout; install coreutils or perl to enable BEADS_HOOK_TIMEOUT\"\n" +
		"    if bd hooks run " + hookName + " \"$@\"; then\n" +
		"      _bd_exit=0\n" +
		"    else\n" +
		"      _bd_exit=$?\n" +
		"    fi\n" +
		"  fi\n" +
		"  if { [ \"$_bd_timeout_backend\" = coreutils ] && [ \"$_bd_exit\" -eq 124 ]; } || { [ \"$_bd_timeout_backend\" = perl ] && [ \"$_bd_exit\" -eq 142 ]; }; then\n" +
		"    echo >&2 \"beads: hook '" + hookName + "' timed out after ${_bd_timeout}s — continuing without beads\"\n" +
		"    _bd_exit=0\n" +
		"  fi\n" +
		"  if [ \"$_bd_exit\" -eq 3 ]; then\n" +
		"    echo >&2 \"beads: database not initialized — skipping hook '" + hookName + "'\"\n" +
		"    _bd_exit=0\n" +
		"  fi\n" +
		"  if [ \"$_bd_exit\" -ne 0 ]; then exit \"$_bd_exit\"; fi\n" +
		"fi\n" +
		hookSectionEndLine() + "\n"
}

// injectHookSection merges the beads section into existing hook file content.
// If section markers are found, only the content between them is replaced.
// If broken markers exist (orphaned BEGIN, reversed order), the stale markers
// are removed before injecting the new section.
// If no markers are found, the section is appended.
func injectHookSection(existing, section string) string {
	return injectHookSectionWithDepth(existing, section, 0)
}

// maxInjectDepth guards against infinite recursion when cleaning broken markers.
const maxInjectDepth = 5

func appendHookSection(existing, section string) string {
	result := existing
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result + "\n" + section
}

func replaceHookSection(existing, section string, beginIdx, endIdx int) string {
	lineStart := strings.LastIndex(existing[:beginIdx], "\n")
	if lineStart == -1 {
		lineStart = 0
	} else {
		lineStart++ // skip the newline itself
	}

	// Find end of the end-marker line (including trailing newline)
	endOfEndMarker := endIdx + len(hookSectionEndPrefix)
	// Consume the rest of the end-marker line (e.g. " v0.58.0 ---\n")
	restAfterPrefix := existing[endOfEndMarker:]
	if nlIdx := strings.Index(restAfterPrefix, "\n"); nlIdx != -1 {
		endOfEndMarker += nlIdx + 1
	} else {
		endOfEndMarker = len(existing)
	}

	return existing[:lineStart] + section + existing[endOfEndMarker:]
}

func injectHookSectionWithDepth(existing, section string, depth int) string {
	if depth > maxInjectDepth {
		// Safety: too many recursive cleanups — append as fallback
		return appendHookSection(existing, section)
	}

	beginIdx := strings.Index(existing, hookSectionBeginPrefix)
	endIdx := strings.Index(existing, hookSectionEndPrefix)
	if beginIdx != -1 && endIdx != -1 && beginIdx < endIdx {
		// Case 1: valid BEGIN...END pair — replace between markers
		return replaceHookSection(existing, section, beginIdx, endIdx)
	}
	if beginIdx != -1 {
		// Case 2: broken markers — orphaned BEGIN (no END) or reversed (END before BEGIN).
		// Remove the orphaned/stale block, then recurse to handle remaining markers.
		cleaned := removeOrphanedBeginBlock(existing, beginIdx)
		return injectHookSectionWithDepth(cleaned, section, depth+1)
	}
	if endIdx != -1 {
		// Case 2b: orphaned END without BEGIN — remove the stale END line
		cleaned := removeMarkerLine(existing, endIdx, hookSectionEndPrefix)
		return injectHookSectionWithDepth(cleaned, section, depth+1)
	}

	// Case 3: no markers. If the existing hook ends in an exec-replacing
	// block (e.g. the templated hook produced by `pre-commit init-templatedir`,
	// which ends with `exec "$INSTALL_PYTHON" -mpre_commit ...`), appending
	// at the bottom would make the bd section unreachable. Detect that
	// pattern and inject above the exec block instead. (GH#3537)
	if injectAt := findExecBlockInjectionPoint(existing); injectAt >= 0 {
		return existing[:injectAt] + section + "\n" + existing[injectAt:]
	}

	// Case 3 fallback: no markers, no trailing exec — append at end.
	return appendHookSection(existing, section)
}

// findExecBlockInjectionPoint inspects the tail of a hook file. If it ends in
// an exec-replacing chain (a final `exec <cmd>` reachable from the bottom of
// the file, possibly inside an `if`/`elif`/`else`/`fi` ladder whose other
// branches only `echo`/`exit`), returns the byte offset where the bd section
// should be injected — i.e. just above the start of the enclosing control
// structure (or above the bare `exec` line if there is none). Returns -1 when
// the file does not end in such a pattern; callers should fall back to
// appending at the bottom.
//
// Motivation: appending below a terminating `exec` makes the appended content
// unreachable, because `exec` replaces the running shell process. (GH#3537)
//
// Limitations (the function uses line-based heuristics, not a shell parser):
//   - A heredoc body containing a literal line that starts with `exec` is
//     treated as code, not data. In practice this is harmless because the
//     terminator line (e.g. `EOF`) is then classified as non-filler and the
//     scan returns -1, but a contrived terminator name could fool it.
//   - A trailing comment on an `exec` line (e.g. `exec /bin/foo  # disabled`)
//     is treated as a live `exec` statement. Use a separate comment line if
//     intent is to disable it.
//   - Two disjoint `if/exec` blocks separated by real code: the scan only
//     considers the LAST one; the real code in the middle correctly causes
//     the scan to return -1 and the caller falls back to append.
func findExecBlockInjectionPoint(content string) int {
	// Trim a trailing newline so strings.Split doesn't produce an empty
	// sentinel as the last element. The scan then sees lines exactly as
	// they appear in the file with no off-by-one ambiguity.
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")

	lastExecLine := findLastExecLine(lines)
	if lastExecLine == -1 {
		return -1
	}

	blockStartLine := findExecBlockStartLine(lines, lastExecLine)
	return execBlockOffset(lines, blockStartLine)
}

func findLastExecLine(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if isExecLine(trimmed) {
			return i
		}
		if !isAllowedAfterExec(trimmed) {
			// Found non-trivial code at the tail that isn't part of an
			// exec-terminated chain — not a pattern we should rewrite.
			return -1
		}
	}
	return -1
}

func isExecBlockContinuation(trimmed string) bool {
	return strings.HasPrefix(trimmed, "elif ") || trimmed == "else" ||
		strings.HasPrefix(trimmed, "else ") || trimmed == "then"
}

func findExecBlockStartLine(lines []string, lastExecLine int) int {
	blockStartLine := lastExecLine
	for i := lastExecLine - 1; i >= 0; i-- {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || isExecBlockContinuation(trimmed) {
			continue
		}
		if line != trimmed { // indented body of an enclosing block
			continue
		}
		// Column-0 line that isn't a continuation. If it opens an if-block,
		// that's the start of our construct.
		if strings.HasPrefix(trimmed, "if ") {
			blockStartLine = i
		}
		break
	}
	return blockStartLine
}

func execBlockOffset(lines []string, blockStartLine int) int {
	offset := 0
	for i := 0; i < blockStartLine; i++ {
		offset += len(lines[i]) + 1 // +1 for the '\n' that strings.Split removed
	}
	return offset
}

// isExecLine reports whether trimmed is an effective `exec <cmd>` statement.
func isExecLine(trimmed string) bool {
	return trimmed == "exec" || strings.HasPrefix(trimmed, "exec ") ||
		strings.HasPrefix(trimmed, "exec\t")
}

// isAllowedAfterExec reports whether a trailing line in an exec-terminated
// chain is harmless filler — control-flow closers, alternative branches,
// fallback exits, comments, and blanks. Anything else means the file does
// not strictly terminate via exec, so we should not rewrite it.
func isAllowedAfterExec(trimmed string) bool {
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return true
	}
	switch trimmed {
	case "fi", "else", "done", "esac", "}", ";;":
		return true
	}
	if strings.HasPrefix(trimmed, "elif ") || strings.HasPrefix(trimmed, "else ") {
		return true
	}
	if strings.HasPrefix(trimmed, "exit") {
		return true
	}
	if strings.HasPrefix(trimmed, "echo ") || strings.HasPrefix(trimmed, "echo\t") {
		// pre-commit's else branch prints a hint before exit 1.
		return true
	}
	return false
}

// removeOrphanedBeginBlock removes an orphaned BEGIN block starting at beginIdx.
// Scans forward from the BEGIN line to the next blank line, next BEGIN marker, or EOF.
func removeOrphanedBeginBlock(content string, beginIdx int) string {
	lineStart := strings.LastIndex(content[:beginIdx], "\n")
	if lineStart == -1 {
		lineStart = 0
	} else {
		lineStart++ // skip the newline itself
	}

	afterBegin := content[beginIdx:]
	blockEnd := len(content)

	lines := strings.SplitAfter(afterBegin, "\n")
	scanned := beginIdx
	for i, line := range lines {
		if i == 0 {
			// Skip the BEGIN line itself
			scanned += len(line)
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			// Blank line — end of orphaned block (include the blank line)
			blockEnd = scanned + len(line)
			break
		}
		if strings.Contains(line, hookSectionBeginPrefix) {
			// Next BEGIN marker — end before this line
			blockEnd = scanned
			break
		}
		scanned += len(line)
	}

	return content[:lineStart] + content[blockEnd:]
}

// removeMarkerLine removes a single marker line from content.
func removeMarkerLine(content string, markerIdx int, markerPrefix string) string {
	lineStart := strings.LastIndex(content[:markerIdx], "\n")
	if lineStart == -1 {
		lineStart = 0
	} else {
		lineStart++ // skip the newline itself
	}

	lineEnd := markerIdx + len(markerPrefix)
	restAfterPrefix := content[lineEnd:]
	if nlIdx := strings.Index(restAfterPrefix, "\n"); nlIdx != -1 {
		lineEnd += nlIdx + 1
	} else {
		lineEnd = len(content)
	}

	return content[:lineStart] + content[lineEnd:]
}

// removeHookSection removes only the beads section from hook file content.
// Returns the content with the section removed, and true if a section was found.
// Handles valid BEGIN...END pairs, orphaned BEGIN, orphaned END, and reversed markers.
func removeValidHookSection(content string, beginIdx, endIdx int) string {
	lineStart := strings.LastIndex(content[:beginIdx], "\n")
	if lineStart == -1 {
		lineStart = 0
	} else {
		lineStart++
	}

	endOfSection := endIdx + len(hookSectionEndPrefix)
	restAfterPrefix := content[endOfSection:]
	if nlIdx := strings.Index(restAfterPrefix, "\n"); nlIdx != -1 {
		endOfSection += nlIdx + 1
	} else {
		endOfSection = len(content)
	}

	// Also consume a blank line before the section if present
	if lineStart >= 2 && content[lineStart-1] == '\n' && content[lineStart-2] == '\n' {
		lineStart--
	}

	return content[:lineStart] + content[endOfSection:]
}

func removeBrokenHookMarkers(content string, beginIdx, endIdx int) string {
	result := content
	if beginIdx != -1 {
		result = removeOrphanedBeginBlock(result, strings.Index(result, hookSectionBeginPrefix))
	}
	if endIdx != -1 {
		// Re-find END index in the (possibly modified) result
		if newEndIdx := strings.Index(result, hookSectionEndPrefix); newEndIdx != -1 {
			result = removeMarkerLine(result, newEndIdx, hookSectionEndPrefix)
		}
	}
	for strings.HasSuffix(result, "\n\n\n") {
		result = result[:len(result)-1]
	}
	return result
}

func removeHookSection(content string) (string, bool) {
	beginIdx := strings.Index(content, hookSectionBeginPrefix)
	endIdx := strings.Index(content, hookSectionEndPrefix)
	if beginIdx == -1 && endIdx == -1 {
		return content, false
	}
	if beginIdx != -1 && endIdx != -1 && beginIdx < endIdx {
		// Valid BEGIN...END pair — remove the whole section
		return removeValidHookSection(content, beginIdx, endIdx), true
	}
	// Broken markers: orphaned BEGIN, orphaned END, or reversed order.
	return removeBrokenHookMarkers(content, beginIdx, endIdx), true
}

// isOnlyShebangOrEmpty reports whether the given hook content consists of
// nothing meaningful — only an optional shebang line plus blank lines and
// comments. Used by shouldPreserveHookContent to decide, after stripping a
// BEADS INTEGRATION block, whether anything user-owned remains worth
// preserving.
//
// Note: non-shebang comment lines (e.g. `# preamble`) are intentionally
// treated as non-content. A file that's only a shebang plus a comment is
// classified empty and skipped — comments alone aren't user logic worth
// carrying forward to .beads/hooks/<name>. (GH#3536)
func isOnlyShebangOrEmpty(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#!") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return false
	}
	return true
}

// shouldPreserveHookContent decides what preservePreexistingHooks should do
// with one hook file's content. Returns (transformedContent, true) when the
// file should be preserved into the target directory (possibly with the bd
// section stripped or the husky helper-layout sanitized); returns
// ("", false) when preservation should skip this file because it's wholly
// bd-managed and contains nothing user-owned worth keeping.
//
// Decision rules (GH#3536):
//   - inlineHookMarker (the "# bd (beads)" tag from GH#1120) marks files
//     that were always wholly bd-owned one-liners — skip.
//   - hookSectionBeginPrefix marks files that were *user-owned* with bd's
//     block injected into them (the v0.49+ section-marker model). Strip
//     the bd block and preserve the remaining user content. If only a
//     shebang/blank/comments remain, treat as wholly bd-owned and skip.
//   - When fromHusky is true, sanitize the (possibly stripped) content so
//     it doesn't depend on husky's helper-layout being mirrored into the
//     target directory (GH#3132).
//
// The function is pure: no I/O, no global state.
func shouldPreserveHookContent(content string, fromHusky bool) (string, bool) {
	if strings.Contains(content, inlineHookMarker) {
		return "", false
	}
	if strings.Contains(content, hookSectionBeginPrefix) {
		stripped, _ := removeHookSection(content)
		if isOnlyShebangOrEmpty(stripped) {
			return "", false
		}
		// Normalize CRLF → LF on the preserved-and-stripped content so
		// Windows / autocrlf=true repos don't end up with `\r\n` line
		// endings in .beads/hooks/<name>. Mirrors the normalization that
		// injectHookSection does on its output (`hooks.go` ~line 622).
		content = strings.ReplaceAll(stripped, "\r\n", "\n")
	}
	if fromHusky {
		content = sanitizeHuskyHook(content)
	}
	return content, true
}

// HookStatus represents the status of a single git hook
type HookStatus struct {
	Name      string
	Installed bool
	Version   string
	IsShim    bool // true if this is a thin shim (version-agnostic)
	Outdated  bool
}
