package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/memoryops"
)

// outputPrimeContext outputs workflow context in markdown format
func outputPrimeContext(w io.Writer, mcpMode bool, stealthMode bool) error {
	return outputPrimeContextWithOptions(w, mcpMode, stealthMode, false)
}

func outputPrimeContextWithOptions(w io.Writer, mcpMode bool, stealthMode bool, memoriesOnly bool) error {
	return outputPrimeContextWithPrimeOptions(w, mcpMode, stealthMode, primeOptions{memoriesOnly: memoriesOnly})
}

func outputPrimeContextWithPrimeOptions(w io.Writer, mcpMode bool, stealthMode bool, opts primeOptions) error {
	if opts.memoriesOnly {
		return outputMemoriesOnlyContextWithOptions(w, opts)
	}
	if mcpMode {
		return outputMCPContextWithOptions(w, stealthMode, opts)
	}
	return outputCLIContextWithOptions(w, stealthMode, opts)
}

func outputMemoriesOnlyContext(w io.Writer) error {
	return outputMemoriesOnlyContextWithOptions(w, primeOptions{})
}

func outputMemoriesOnlyContextWithOptions(w io.Writer, opts primeOptions) error {
	_, _ = fmt.Fprint(w, primeTruncationDirective)
	if mem := formatMemoriesForPrimeWithOptions(false, opts); mem != "" {
		_, _ = fmt.Fprint(w, mem)
		return nil
	}
	_, _ = fmt.Fprint(w, "# Beads Persistent Memories\n\nNo memories stored. Use `bd remember \"insight\"` to add one.\n")
	return nil
}

// formatMemoriesForPrime reads the memory plane with configuration-derived
// caps. Command invocations use formatMemoriesForPrimeWithOptions so explicit
// flags remain local to the command execution.
func formatMemoriesForPrime(compact bool) string {
	return formatMemoriesForPrimeWithOptions(compact, primeOptions{})
}

func formatMemoriesForPrimeWithOptions(compact bool, opts primeOptions) string {
	// bd-mm8wf: in a proxied-server workspace the memory read must ride the
	// proxied plane (UOW provider), never ensureStoreActiveForPrime — the
	// lazy direct-store open is the same seam class bd-m7zzd closed in
	// relate.go and human.go, here in a read-only limb. The proxied dual
	// preserves prime's silent-skip and timeout-banner contracts.
	if usesProxiedServer() {
		return formatMemoriesForPrimeProxiedWithOptions(compact, opts)
	}

	// Try to initialize store if not already active (prime may run before other commands)
	if getStore() == nil {
		timeout := primeStoreTimeout()
		ctx := context.Background()
		var cancel context.CancelFunc
		if timeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		if err := ensureStoreActiveForPrime(ctx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return formatPrimeMemoryTimeout(compact, timeout)
			}
			return "" // Silently skip — store unavailable
		}
	}
	if getStore() == nil {
		return ""
	}
	memories, err := getStore().Memories()
	if err != nil {
		return ""
	}
	result, err := memories.List(context.Background(), memoryops.ListRequest{})
	if err != nil {
		return ""
	}
	return renderPrimeMemoryPlaneWithOptions(result.Memories, compact, opts)
}

const primeTruncationDirective = "[bd prime] If this output is truncated by your host, read the full persisted hook output before continuing; it may contain project memories and session rules not visible in the preview.\n\n"

// renderPrimeMemoryPlane renders the memory plane for injection — the shared
// tail of the classic and proxied (bd-mm8wf) memory-read paths, so the two
// cannot drift in what a memory looks like once fetched.
//
// It takes the PLANE, not a config map: which rows are memories is
// memoryops.Memories.List's answer now, on both routes, which is what stopped
// prime from being a fifth front door with its own copy of the kv.memory.
// prefix rule.
func renderPrimeMemoryPlane(memories map[string]string, compact bool) string {
	return renderPrimeMemoryPlaneWithOptions(memories, compact, primeOptions{})
}

func renderPrimeMemoryPlaneWithOptions(memories map[string]string, compact bool, opts primeOptions) string {
	if len(memories) == 0 {
		return ""
	}
	maxCount, maxChars := primeMemoryCapsForOptions(opts)
	return renderPrimeMemories(memories, compact, maxCount, maxChars)
}

// primeConfigInt reads an integer config key (stubbable for tests).
var primeConfigInt = func(key string) int {
	return config.GetInt(key)
}

// primeMemoryCaps resolves the memory-injection caps. An explicitly passed
// flag wins, including an explicit 0 meaning "force unlimited"; otherwise the
// prime.max-memories / prime.max-memory-chars config keys apply. 0 or unset
// means uncapped.
func primeMemoryCaps() (maxCount, maxChars int) {
	return primeMemoryCapsForOptions(primeOptions{})
}

func primeMemoryCapsForOptions(opts primeOptions) (maxCount, maxChars int) {
	maxCount = opts.maxMemories
	if !opts.maxMemoriesSet && maxCount == 0 {
		maxCount = primeConfigInt("prime.max-memories")
	}
	maxChars = opts.maxMemoryChars
	if !opts.maxMemoryCharsSet && maxChars == 0 {
		maxChars = primeConfigInt("prime.max-memory-chars")
	}
	if maxCount < 0 {
		maxCount = 0
	}
	if maxChars < 0 {
		maxChars = 0
	}
	return maxCount, maxChars
}

// renderPrimeMemories formats memories for injection, applying the given
// caps. maxCount bounds how many memories are emitted; maxChars bounds the
// total bytes of the emitted memory entries (the section header and elision
// banner are not counted against this budget). Both are 0 when uncapped.
// Caps apply at whole-memory boundaries and at least one memory is always
// emitted, so a single oversized memory can exceed maxChars rather than
// vanish. Keys are emitted in sorted order (the memory store keeps no
// timestamps, so alphabetical is the only stable order available); when
// entries are elided a banner ahead of the entries says how many and how to
// reach the rest, so a capped prime never silently drops context. The banner
// names only the cap that actually fired.
func renderPrimeMemories(memories map[string]string, compact bool, maxCount, maxChars int) string {
	keys := sortedKeys(memories)
	entries, countCapHit, charCapHit := collectPrimeMemoryEntries(memories, keys, compact, maxCount, maxChars)
	elided := len(keys) - len(entries)
	note := primeMemoryCapNoteForHits(countCapHit, charCapHit, maxCount, maxChars)
	var sb strings.Builder
	sb.WriteString(primeMemoryIntro(compact, len(entries), len(keys), elided, note))
	for _, entry := range entries {
		sb.WriteString(entry)
	}
	return sb.String()
}

func collectPrimeMemoryEntries(memories map[string]string, keys []string, compact bool, maxCount, maxChars int) ([]string, bool, bool) {
	entries := make([]string, 0, len(keys))
	used := 0
	var countCapHit, charCapHit bool
	for _, key := range keys {
		if primeMemoryCountCapReached(len(entries), maxCount) {
			countCapHit = true
			break
		}
		entry := formatPrimeMemoryEntry(key, memories[key], compact)
		if primeMemoryCharCapReached(len(entries), used, len(entry), maxChars) {
			charCapHit = true
			break
		}
		entries = append(entries, entry)
		used += len(entry)
	}
	return entries, countCapHit, charCapHit
}

func primeMemoryCountCapReached(entryCount, maxCount int) bool {
	return maxCount > 0 && entryCount >= maxCount
}

func formatPrimeMemoryEntry(key, memory string, compact bool) string {
	if compact {
		memory = strings.ReplaceAll(memory, "\n", " ")
		memory = truncate(memory, 150)
		return fmt.Sprintf("- **%s**: %s\n", key, memory)
	}
	return fmt.Sprintf("### %s\n%s\n\n", key, memory)
}

func primeMemoryCharCapReached(entryCount, used, entryLength, maxChars int) bool {
	return maxChars > 0 && entryCount > 0 && used+entryLength > maxChars
}

func primeMemoryCapNoteForHits(countCapHit, charCapHit bool, maxCount, maxChars int) string {
	var noteCount, noteChars int
	if countCapHit {
		noteCount = maxCount
	}
	if charCapHit {
		noteChars = maxChars
	}
	return primeMemoryCapNote(noteCount, noteChars)
}

func primeMemoryIntro(compact bool, entryCount, totalCount, elided int, note string) string {
	if compact {
		return compactPrimeMemoryIntro(entryCount, totalCount, elided, note)
	}
	return fullPrimeMemoryIntro(entryCount, totalCount, elided, note)
}

func compactPrimeMemoryIntro(entryCount, totalCount, elided int, note string) string {
	if elided == 0 {
		return "\n## Memories\n"
	}
	return fmt.Sprintf("\n## Memories (showing %d of %d)\n- %d more not shown (%s); browse with `bd memories <keyword>`\n", entryCount, totalCount, elided, note)
}

func fullPrimeMemoryIntro(entryCount, totalCount, elided int, note string) string {
	var header string
	if elided > 0 {
		header = fmt.Sprintf("\n## Persistent Memories (showing %d of %d, alphabetical)\n\n", entryCount, totalCount)
	} else {
		header = fmt.Sprintf("\n## Persistent Memories (%d)\n\n", totalCount)
	}
	header += "Stored via `bd remember`. Update in place with `bd remember --key <key> \"new content\"`. Search with `bd memories <keyword>`. Remove with `bd forget <key>`.\n\n"
	if elided > 0 {
		header += fmt.Sprintf("> %d more memories are not shown here (%s). Browse the full set with `bd memories <keyword>` or recall one with `bd remember <key>`.\n\n", elided, note)
	}
	return header
}

// primeMemoryCapNote names the active cap(s) for the elision banner.
func primeMemoryCapNote(maxCount, maxChars int) string {
	var parts []string
	if maxCount > 0 {
		parts = append(parts, fmt.Sprintf("max-memories=%d", maxCount))
	}
	if maxChars > 0 {
		parts = append(parts, fmt.Sprintf("max-memory-chars=%d", maxChars))
	}
	return "capped by " + strings.Join(parts, ", ")
}

func formatPrimeMemoryTimeout(compact bool, timeout time.Duration) string {
	if timeout <= 0 {
		timeout = primeStoreTimeoutDefault
	}
	msg := fmt.Sprintf("Skipped: timed out after %s opening beads storage. Another bd process or stale storage lock may be blocking memory injection; run `bd doctor` and stop stuck bd processes before retrying.", timeout.Round(time.Millisecond))
	if compact {
		return "\n## Memories\n- " + msg + "\n"
	}
	return "\n## Persistent Memories\n\n" + msg + "\n"
}
