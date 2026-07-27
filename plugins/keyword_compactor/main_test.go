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

// TestTruncateHeadTailBudget pins the contract: n is the TOTAL output budget,
// notice included.
//
// It used to disagree with itself — the guard fired at n*2 while the body kept
// n — and the notice was appended after the halves had already consumed the
// whole budget, so near the boundary "truncating" produced a longer string than
// it was given.
//
// The current single caller only ever passes content well above n, so these
// sizes are not reachable through it today. They are the helper's contract, and
// the helper is exported to the package as a general budget-capping utility.
func TestTruncateHeadTailBudget(t *testing.T) {
	const budget = 2000
	for _, size := range []int{1999, 2000, 2001, 2042, 3000, 4000, 4001, 10000} {
		content := strings.Repeat("a", size)
		got := truncateHeadTail(content, budget)

		if size <= budget {
			if got != content {
				t.Errorf("size=%d: content within budget was modified", size)
			}
			continue
		}

		// The contract, and the bug: the result must never exceed the budget.
		if len(got) > budget {
			t.Errorf("size=%d: truncation produced %d bytes, which is OVER the %d-byte budget",
				size, len(got), budget)
		}
		// ...and it must not be so conservative that it throws the budget away.
		if len(got) < budget/2 {
			t.Errorf("size=%d: kept only %d bytes of a %d-byte budget", size, len(got), budget)
		}
		if !strings.Contains(got, "truncated by Torana") {
			t.Errorf("size=%d: no truncation notice in the result", size)
		}
	}
}

// A truncation must never return more than it was given, at any budget. This is
// the property the old code broke: for inputs just over the budget, the notice
// pushed the result past the input length.
func TestTruncationNeverGrowsTheInput(t *testing.T) {
	for _, budget := range []int{50, 100, 2000} {
		for _, size := range []int{budget - 1, budget, budget + 1, budget + 10, budget * 3} {
			if size < 1 {
				continue
			}
			content := strings.Repeat("a", size)
			got := truncateHeadTail(content, budget)
			if len(got) > len(content) {
				t.Errorf("budget=%d size=%d: truncation grew the input to %d bytes",
					budget, size, len(got))
			}
			if len(got) > budget && len(content) > budget {
				t.Errorf("budget=%d size=%d: result %d exceeds the budget", budget, size, len(got))
			}
		}
	}
}

// A non-positive budget must not panic. truncHead/truncTail would reach s[:n]
// with a negative n, and a panic inside a plugin traps the whole request.
func TestNonPositiveBudgetsAreSafe(t *testing.T) {
	for _, n := range []int{0, -1, -1000} {
		if got := truncateHeadTail("some content here", n); got != "" {
			t.Errorf("truncateHeadTail(n=%d) = %q, want empty", n, got)
		}
		if got := truncHead("abc", n); got != "" {
			t.Errorf("truncHead(n=%d) = %q", n, got)
		}
		if got := truncTail("abc", n); got != "" {
			t.Errorf("truncTail(n=%d) = %q", n, got)
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
