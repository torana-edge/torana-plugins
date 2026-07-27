package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/torana-edge/torana-plugin-sdk/pb"
	"google.golang.org/protobuf/proto"
)

// The bug this file exists for: truncateHeadTail sliced by byte index, so a cut
// landing mid-rune produced invalid UTF-8. The result is assigned to
// Message.Content, the SDK marshals it into a proto3 string field, protobuf-go
// enforces UTF-8 on proto3 strings and returns an error, and the SDK panics —
// trapping the plugin for the whole request. Any non-ASCII tool output was
// enough.
//
// A utf8.ValidString assertion alone does not reproduce that; the marshal does.
// So the round-trip test below is the one that matters, and the rest guard the
// budget arithmetic around it.

// multibyte builds a string of n bytes made entirely of 3-byte runes, so almost
// every byte index is a mid-rune index.
func multibyte(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString("日") // 3 bytes
	}
	return b.String()
}

// TestTruncatedContentSurvivesProtoMarshal is the regression test for the trap.
// It asserts the exact thing that used to blow up: the truncated string going
// into a proto3 string field.
func TestTruncatedContentSurvivesProtoMarshal(t *testing.T) {
	// Sweep budgets so the cut lands at every offset modulo the 3-byte rune
	// width — one fixed budget could sit on a boundary by luck and pass.
	for n := 100; n <= 120; n++ {
		got := truncateHeadTail(multibyte(6000), n)

		if !utf8.ValidString(got) {
			t.Fatalf("n=%d: truncation produced invalid UTF-8", n)
		}
		msg := &pb.Message{Role: "tool", Content: got}
		if _, err := proto.Marshal(msg); err != nil {
			t.Fatalf("n=%d: proto.Marshal rejected the truncated content: %v", n, err)
		}
	}
}

// TestTruncateHeadTailBudget pins the threshold. It used to disagree with
// itself: the guard fired at n*2 while the body kept n, so content between n
// and 2n passed through untouched and anything larger was cut to half the
// requested size. n is the total budget.
func TestTruncateHeadTailBudget(t *testing.T) {
	for _, tc := range []struct {
		size      int
		truncated bool
	}{
		{1999, false},
		{2000, false}, // exactly the budget — nothing to remove
		{2001, true},  // one byte over: used to pass through untouched
		{4000, true},  // used to pass through untouched
		{4001, true},
	} {
		content := strings.Repeat("a", tc.size)
		got := truncateHeadTail(content, 2000)

		if truncated := got != content; truncated != tc.truncated {
			t.Errorf("size=%d: truncated=%v, want %v", tc.size, truncated, tc.truncated)
		}
		if !tc.truncated {
			continue
		}
		// The kept payload is the budget; the notice itself is overhead.
		head, tail, ok := strings.Cut(got, "\n\n... [")
		if !ok {
			t.Fatalf("size=%d: no truncation notice in %q", tc.size, got)
		}
		_, tail, _ = strings.Cut(tail, "] ...\n\n")
		if kept := len(head) + len(tail); kept != 2000 {
			t.Errorf("size=%d: kept %d bytes, want the full 2000-byte budget", tc.size, kept)
		}
	}
}

// TestTruncationNoticeCountsAccurately guards the number in the notice. It is
// computed from what was actually kept, so it stays exact even when backing off
// to a rune boundary shortens a half.
func TestTruncationNoticeCountsAccurately(t *testing.T) {
	content := multibyte(6000)
	got := truncateHeadTail(content, 101) // odd budget, 3-byte runes

	_, rest, ok := strings.Cut(got, "\n\n... [")
	if !ok {
		t.Fatalf("no truncation notice in %q", got)
	}
	claimed, _, _ := strings.Cut(rest, " bytes truncated by Torana]")

	head, tail, _ := strings.Cut(got, "\n\n... [")
	_, tail, _ = strings.Cut(tail, "] ...\n\n")
	want := itoa(len(content) - len(head) - len(tail))

	if claimed != want {
		t.Errorf("notice claims %s bytes truncated, actually removed %s", claimed, want)
	}
}

// TestTruncHeadTailStayWithinBudget asserts the halves never exceed what they
// were asked for — backing off to a rune boundary must shorten, never extend.
func TestTruncHeadTailStayWithinBudget(t *testing.T) {
	s := multibyte(300)
	for n := 0; n <= 40; n++ {
		if head := truncHead(s, n); len(head) > n || !utf8.ValidString(head) {
			t.Errorf("truncHead(n=%d): len=%d valid=%v", n, len(head), utf8.ValidString(head))
		}
		if tail := truncTail(s, n); len(tail) > n || !utf8.ValidString(tail) {
			t.Errorf("truncTail(n=%d): len=%d valid=%v", n, len(tail), utf8.ValidString(tail))
		}
	}
	// A budget at or beyond the input returns it whole.
	if got := truncHead(s, len(s)+10); got != s {
		t.Error("truncHead should return the input unchanged when the budget exceeds it")
	}
	if got := truncTail(s, len(s)+10); got != s {
		t.Error("truncTail should return the input unchanged when the budget exceeds it")
	}
}
