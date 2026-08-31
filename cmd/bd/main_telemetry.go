package main

import (
	"strings"

	"github.com/spf13/cobra"
)

// secretFlagNames are long flag names whose entire value is an opaque credential
// that must never reach the bd.args telemetry span. The flag's value is redacted
// wholesale. Only federation add-peer's --password currently qualifies. Its shorthand (-p) is
// resolved per command via secretFlagTokens so the same letter bound to
// --priority/--prefix/--parallel on other commands is never redacted.
var secretFlagNames = map[string]bool{"password": true}

// secretFlagTokens returns the concrete --long and -short flag tokens that carry a
// secret value for cmd. Resolving against the running command is what makes the
// redaction "by flag identity": -p is treated as secret only on the command that
// actually binds it to a secret flag (federation add-peer), not on the many
// commands that bind -p to a non-secret option.
func secretFlagTokens(cmd *cobra.Command) map[string]bool {
	tokens := make(map[string]bool)
	if cmd == nil {
		return tokens
	}
	for name := range secretFlagNames {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			continue
		}
		tokens["--"+f.Name] = true
		if f.Shorthand != "" {
			tokens["-"+f.Shorthand] = true
		}
	}
	return tokens
}

// scrubArgsForTelemetry joins argv for the bd.args span attribute with any
// credential-bearing values redacted. A secretFlags token's value is redacted
// wholesale across the `--password <v>`,
// `--password=<v>`, `-p <v>`, `-p=<v>`, and `-p<v>` spellings pflag accepts. Every
// other arg gets a conservative DSN/userinfo scrub as defense in depth so a
// positional connection string cannot leak a password.
func scrubArgsForTelemetry(argv []string, secretFlags map[string]bool) string {
	parts := make([]string, len(argv))
	redactNext := false
	for i, a := range argv {
		parts[i], redactNext = scrubTelemetryArg(argv, i, a, secretFlags, redactNext)
	}
	return strings.Join(parts, " ")
}

func scrubTelemetryArg(argv []string, index int, arg string, secretFlags map[string]bool, redactNext bool) (string, bool) {
	if redactNext {
		return "xxxxx", false
	}
	if scrubbed, ok := scrubTelemetryEqualsArg(arg, secretFlags); ok {
		return scrubbed, false
	}
	if index > 0 && secretFlags[argv[index-1]] {
		// <secret> following a bare --password / -p token.
		return "xxxxx", false
	}
	if short, ok := secretShorthandPrefix(arg, secretFlags); ok {
		// -p<secret> — pflag's concatenated shorthand spelling.
		return short + "xxxxx", false
	}
	if secretShorthandTakesSeparateValue(arg, secretFlags) {
		// -qp <secret> — a boolean shorthand cluster ending in the
		// value-taking secret shorthand, with its value in the next token.
		return arg, true
	}
	return scrubUserinfoPassword(scrubPotentialDSNPasswords(arg)), false
}

func scrubTelemetryEqualsArg(arg string, secretFlags map[string]bool) (string, bool) {
	name, value, ok := strings.Cut(arg, "=")
	if !ok {
		return "", false
	}
	if secretFlags[name] {
		// --password=<secret> / -p=<secret> — redact the whole value.
		return name + "=xxxxx", true
	}
	if !strings.HasPrefix(name, "-") {
		return "", false
	}
	scrubbed := scrubUserinfoPassword(scrubPotentialDSNPasswords(value))
	if scrubbed == value {
		return "", false
	}
	// Preserve an arbitrary flag name while parsing its equals-value as a
	// possible DSN. Passing the whole token to url.Parse would treat the flag
	// prefix as the URL scheme and miss query credentials.
	return name + "=" + scrubbed, true
}

// secretShorthandPrefix reports whether a is pflag's concatenated secret-shorthand
// spelling, returning the "-x...-p" prefix to preserve. Long flags cannot concatenate
// a value, so only -X<value> shorthands are matched.
//
// pflag also accepts a CLUSTER of boolean shorthands ending in a value-taking
// shorthand: given boolean flags -q/-v and value flag -p, "-qpSECRET" parses as -q
// followed by -p SECRET, and "-vpSECRET" parses as -v followed by -p SECRET — but the
// raw token still reaches telemetry as one string. Walk the leading run of letters in
// a; the first letter whose "-x" token is a registered secret shorthand ends the
// cluster, and everything after it is that flag's value, regardless of how many
// boolean shorthands preceded it. This mirrors pflag's own grammar (a cluster is zero
// or more boolean shorthands followed by one value-taking shorthand) without needing
// the running command's flag set here: it is conservative in the safe direction,
// since treating a longer prefix as consumed by the secret shorthand only ever
// over-redacts, never under-redacts.
func secretShorthandPrefix(a string, secretFlags map[string]bool) (string, bool) {
	if !isSecretShorthandCandidate(a) {
		return "", false
	}
	lastIndex := len(a) - 1
	for i := 1; i <= lastIndex; i++ {
		c := a[i]
		if !isSecretShorthandLetter(c) {
			return "", false
		}
		if secretFlags["-"+string(c)] {
			if i == lastIndex {
				return "", false // no value follows; not the concatenated spelling
			}
			return a[:i+1], true
		}
	}
	return "", false
}

// secretShorthandTakesSeparateValue recognizes a boolean-shorthand cluster that
// ends in a registered secret shorthand with no attached value. For example,
// pflag parses "-qp secret" as -q followed by -p=secret.
func secretShorthandTakesSeparateValue(a string, secretFlags map[string]bool) bool {
	if !isSecretShorthandCandidate(a) {
		return false
	}
	lastIndex := len(a) - 1
	for i := 1; i <= lastIndex; i++ {
		c := a[i]
		if !isSecretShorthandLetter(c) {
			return false
		}
		if secretFlags["-"+string(c)] {
			return i == lastIndex
		}
	}
	return false
}

func isSecretShorthandCandidate(a string) bool {
	return len(a) >= 3 && a[0] == '-' && a[1] != '-'
}

func isSecretShorthandLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// scrubUserinfoPassword redacts the password in a URL/DSN userinfo section
// (postgres://user:PASS@host or user:PASS@tcp(...)); args without a user:pass@
// userinfo pass through unchanged, so ordinary text is never mangled.
func scrubUserinfoPassword(a string) string {
	at := strings.LastIndexByte(a, '@')
	if at < 0 {
		return a
	}
	head := a[:at]
	start := 0
	if s := strings.LastIndex(head, "//"); s >= 0 {
		start = s + 2 // userinfo begins after the scheme's "//"
	}
	colon := strings.IndexByte(head[start:], ':')
	if colon < 0 {
		return a // no "user:pass" userinfo, nothing to redact
	}
	return head[:start+colon+1] + "xxxxx" + a[at:]
}
