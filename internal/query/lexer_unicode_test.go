package query

import "testing"

// Regression test for a bug where the lexer read input one byte at a time
// and cast each byte directly to rune (see Lexer.next), instead of decoding
// UTF-8. Any WriteRune call downstream (readString, readIdent) then
// re-encoded that raw byte as its own UTF-8 code point, corrupting every
// multi-byte UTF-8 sequence into mojibake, e.g. "café" -> "cafÃ©". This
// silently broke bd queries against non-ASCII field values (labels,
// assignees, metadata, etc.) since the corrupted value is what reached the
// evaluator's comparison logic.
func TestLexerHandlesNonASCIIStringValue(t *testing.T) {
	const input = `label="café"`
	lex := NewLexer(input)

	tok, err := lex.NextToken() // "label"
	if err != nil || tok.Type != TokenIdent || tok.Value != "label" {
		t.Fatalf("unexpected first token: %+v err=%v", tok, err)
	}

	tok, err = lex.NextToken() // "="
	if err != nil || tok.Type != TokenEquals {
		t.Fatalf("unexpected second token: %+v err=%v", tok, err)
	}

	tok, err = lex.NextToken() // "café"
	if err != nil {
		t.Fatalf("unexpected lexer error: %v", err)
	}
	if tok.Type != TokenString {
		t.Fatalf("expected TokenString, got %v", tok.Type)
	}
	if tok.Value != "café" {
		t.Fatalf("lexer corrupted non-ASCII string: got %q (bytes %x), want %q (bytes %x)",
			tok.Value, []byte(tok.Value), "café", []byte("café"))
	}
}

// Same corruption applied to unquoted identifiers/values (readIdent shares
// the same next()/WriteRune path as readString).
func TestLexerHandlesNonASCIIIdent(t *testing.T) {
	const input = `assignee=日本語`
	lex := NewLexer(input)

	tok, err := lex.NextToken() // "assignee"
	if err != nil || tok.Value != "assignee" {
		t.Fatalf("unexpected first token: %+v err=%v", tok, err)
	}

	tok, err = lex.NextToken() // "="
	if err != nil || tok.Type != TokenEquals {
		t.Fatalf("unexpected second token: %+v err=%v", tok, err)
	}

	tok, err = lex.NextToken() // "日本語"
	if err != nil {
		t.Fatalf("unexpected lexer error: %v", err)
	}
	if tok.Value != "日本語" {
		t.Fatalf("lexer corrupted non-ASCII ident: got %q (bytes %x), want %q (bytes %x)",
			tok.Value, []byte(tok.Value), "日本語", []byte("日本語"))
	}
}

// End-to-end seam: the query language's Parse entry point, as used by bd's
// query/filter commands, must preserve non-ASCII comparison values.
func TestParsePreservesNonASCIIValue(t *testing.T) {
	node, err := Parse(`label="café"`)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	cmp, ok := node.(*ComparisonNode)
	if !ok {
		t.Fatalf("expected *ComparisonNode, got %T", node)
	}
	if cmp.Value != "café" {
		t.Fatalf("parsed comparison value corrupted: got %q, want %q", cmp.Value, "café")
	}
}
