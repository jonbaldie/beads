// Package query implements a simple query language for filtering beads.
//
// The query language supports:
//   - Field comparisons: status=open, priority>1, updated>7d
//   - Boolean operators: AND, OR, NOT
//   - Parentheses for grouping: (status=open OR status=blocked) AND priority<2
//   - Date-relative expressions: updated>7d, created<30d
//
// Example queries:
//   - status=open AND priority>1
//   - (status=open OR status=blocked) AND updated>7d
//   - NOT status=closed
//   - type=bug AND priority=0
package query

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TokenType represents the type of a lexer token.
type TokenType int

const (
	TokenEOF       TokenType = iota
	TokenIdent               // field names, values
	TokenString              // quoted strings
	TokenNumber              // numeric values
	TokenDuration            // duration values like 7d, 24h
	TokenEquals              // =
	TokenNotEquals           // !=
	TokenLess                // <
	TokenLessEq              // <=
	TokenGreater             // >
	TokenGreaterEq           // >=
	TokenAnd                 // AND
	TokenOr                  // OR
	TokenNot                 // NOT
	TokenLParen              // (
	TokenRParen              // )
	TokenComma               // , (for lists)
)

// String returns the string representation of a TokenType.
func (t TokenType) String() string {
	names := [...]string{
		"EOF", "IDENT", "STRING", "NUMBER", "DURATION", "=", "!=", "<", "<=", ">", ">=",
		"AND", "OR", "NOT", "(", ")", ",",
	}
	if t >= TokenEOF && int(t) < len(names) {
		return names[t]
	}
	return fmt.Sprintf("UNKNOWN(%d)", t)
}

// Token represents a single token from the lexer.
type Token struct {
	Type  TokenType
	Value string
	Pos   int // Position in input string
}

// Lexer tokenizes a query string.
type Lexer struct {
	input string
	pos   int
	width int // width of last rune read
}

// NewLexer creates a new Lexer for the given input string.
func NewLexer(input string) *Lexer {
	return &Lexer{input: input}
}

// next returns the next rune and advances position.
//
// input is decoded as UTF-8 (not raw bytes): treating each byte as its own
// rune corrupts any multi-byte sequence once it round-trips through
// strings.Builder.WriteRune (e.g. "café" becomes "cafÃ©").
func (l *Lexer) next() rune {
	if l.pos >= len(l.input) {
		l.width = 0
		return 0
	}
	r, w := utf8.DecodeRuneInString(l.input[l.pos:])
	l.width = w
	l.pos += l.width
	return r
}

// peek returns the next rune without advancing.
func (l *Lexer) peek() rune {
	if l.pos >= len(l.input) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(l.input[l.pos:])
	return r
}

// backup steps back one rune.
func (l *Lexer) backup() {
	l.pos -= l.width
}

// skipWhitespace skips whitespace characters.
func (l *Lexer) skipWhitespace() {
	for {
		r := l.next()
		if r == 0 || !unicode.IsSpace(r) {
			l.backup()
			return
		}
	}
}

// NextToken returns the next token from the input.
func (l *Lexer) NextToken() (Token, error) {
	l.skipWhitespace()
	startPos := l.pos
	r := l.next()
	if r == 0 {
		return Token{Type: TokenEOF, Pos: startPos}, nil
	}
	return lexRune(l, r, startPos)
}

func lexRune(l *Lexer, r rune, startPos int) (Token, error) {
	switch r {
	case '(':
		return Token{Type: TokenLParen, Value: "(", Pos: startPos}, nil
	case ')':
		return Token{Type: TokenRParen, Value: ")", Pos: startPos}, nil
	case ',':
		return Token{Type: TokenComma, Value: ",", Pos: startPos}, nil
	case '=':
		return Token{Type: TokenEquals, Value: "=", Pos: startPos}, nil
	case '!', '<', '>':
		return lexComparison(l, r, startPos)
	case '"', '\'':
		return l.readString(r, startPos)
	default:
		return lexWord(l, r, startPos)
	}
}

func lexWord(l *Lexer, r rune, startPos int) (Token, error) {
	l.backup()
	if unicode.IsDigit(r) || r == '-' || r == '+' {
		return l.readNumberOrDuration(startPos)
	}
	if isIdentStart(r) {
		return l.readIdent(startPos)
	}
	l.next()
	return Token{}, fmt.Errorf("unexpected character %q at position %d", r, startPos)
}

func lexComparison(l *Lexer, r rune, startPos int) (Token, error) {
	hasEquals := l.peek() == '='
	if hasEquals {
		l.next()
	}
	switch r {
	case '!':
		if hasEquals {
			return Token{Type: TokenNotEquals, Value: "!=", Pos: startPos}, nil
		}
		return Token{}, fmt.Errorf("unexpected character '!' at position %d (did you mean '!=' or 'NOT'?)", startPos)
	case '<':
		if hasEquals {
			return Token{Type: TokenLessEq, Value: "<=", Pos: startPos}, nil
		}
		return Token{Type: TokenLess, Value: "<", Pos: startPos}, nil
	default:
		if hasEquals {
			return Token{Type: TokenGreaterEq, Value: ">=", Pos: startPos}, nil
		}
		return Token{Type: TokenGreater, Value: ">", Pos: startPos}, nil
	}
}

// readString reads a quoted string.
func (l *Lexer) readString(quote rune, startPos int) (Token, error) {
	var sb strings.Builder
	for {
		r := l.next()
		if r == 0 {
			return Token{}, fmt.Errorf("unterminated string starting at position %d", startPos)
		}
		if r == quote {
			return Token{Type: TokenString, Value: sb.String(), Pos: startPos}, nil
		}
		if r == '\\' {
			escaped, ok := escapedRune(l.next())
			if !ok {
				return Token{}, fmt.Errorf("unterminated escape sequence at position %d", l.pos-1)
			}
			sb.WriteRune(escaped)
		} else {
			sb.WriteRune(r)
		}
	}
}

func escapedRune(r rune) (rune, bool) {
	switch r {
	case 'n':
		return '\n', true
	case 't':
		return '\t', true
	case 0:
		return 0, false
	default:
		return r, true
	}
}

// readNumberOrDuration reads a number or duration (e.g., 7d, 24h).
//
// Unsigned digit-led tokens that continue into identifier characters
// (e.g. "1-alpha", "42day-sla", "9.3.1") are re-lexed as identifiers so
// they can stand in unquoted on the value side of comparisons like
// "label=1-alpha". Signed forms ("-3-foo") still error — the user has
// to quote them.
func (l *Lexer) readNumberOrDuration(startPos int) (Token, error) {
	value, r, hadSign, err := readNumberRun(l)
	if err != nil {
		return Token{}, err
	}

	// Check for duration suffix. Only commit to a duration when the suffix
	// stands alone — if more identifier characters follow (e.g. "7day"),
	// fall through to the identifier-fallback below.
	if r != 0 && isDurationSuffix(r) && !isIdentChar(l.peek()) {
		return Token{Type: TokenDuration, Value: value + string(r), Pos: startPos}, nil
	}

	// Identifier-continuation fallback: an unsigned digit-led run that
	// butts against more identifier characters is an identifier, not a
	// number. Restart the lex at the original position and read it as an
	// identifier so "1-alpha", "2bravo", "42d-sla" all tokenize as one
	// TokenIdent.
	if !hadSign && r != 0 && isIdentChar(r) {
		l.pos = startPos
		return l.readIdent(startPos)
	}

	// Not a duration suffix and not an identifier — back up the lookahead
	// rune.
	if r != 0 {
		l.backup()
	}

	return Token{Type: TokenNumber, Value: value, Pos: startPos}, nil
}

func readNumberRun(l *Lexer) (string, rune, bool, error) {
	var value strings.Builder
	r := l.next()
	hadSign := r == '-' || r == '+'
	if hadSign {
		value.WriteRune(r)
		r = l.next()
	}
	if !unicode.IsDigit(r) {
		l.backup()
		return "", 0, false, fmt.Errorf("expected digit at position %d", l.pos)
	}
	value.WriteRune(r)
	for {
		r = l.next()
		if !unicode.IsDigit(r) {
			return value.String(), r, hadSign, nil
		}
		value.WriteRune(r)
	}
}

// readIdent reads an identifier or keyword.
func (l *Lexer) readIdent(startPos int) (Token, error) {
	var sb strings.Builder

	for {
		r := l.next()
		if r == 0 || !isIdentChar(r) {
			l.backup()
			break
		}
		sb.WriteRune(r)
	}

	value := sb.String()
	upper := strings.ToUpper(value)

	// Check for keywords
	switch upper {
	case "AND":
		return Token{Type: TokenAnd, Value: value, Pos: startPos}, nil
	case "OR":
		return Token{Type: TokenOr, Value: value, Pos: startPos}, nil
	case "NOT":
		return Token{Type: TokenNot, Value: value, Pos: startPos}, nil
	default:
		return Token{Type: TokenIdent, Value: value, Pos: startPos}, nil
	}
}

// Tokenize returns all tokens from the input.
func (l *Lexer) Tokenize() ([]Token, error) {
	var tokens []Token
	for {
		tok, err := l.NextToken()
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, tok)
		if tok.Type == TokenEOF {
			break
		}
	}
	return tokens, nil
}

// isIdentStart returns true if r can start an identifier.
func isIdentStart(r rune) bool {
	return unicode.IsLetter(r) || r == '_'
}

// isIdentChar returns true if r can be part of an identifier.
// Colons are allowed so that namespaced labels (e.g., gt:merge-request) can
// be used unquoted in query expressions like "label=gt:merge-request".
// Slashes are allowed so that path-style metadata keys (e.g., jira/sprint)
// can be used in query expressions like "metadata.jira/sprint=42".
func isIdentChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == ':' || r == '/'
}

// isDurationSuffix returns true if r is a valid duration suffix.
func isDurationSuffix(r rune) bool {
	switch r {
	case 'h', 'd', 'w', 'm', 'y', 'H', 'D', 'W', 'M', 'Y':
		return true
	default:
		return false
	}
}
