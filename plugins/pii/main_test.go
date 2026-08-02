package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

// ==========================================================================
// Shared fixtures
// ==========================================================================

func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	resetConfigForTest()
	return sdktest.New(t)
}

func toolMsg(id, name, content string, parts []byte) *pbv2.Message {
	return &pbv2.Message{Role: "tool", ToolCallId: id, ToolName: name, Content: content, ContentPartsJson: parts}
}

func textPart(s string) string {
	b, _ := json.Marshal(map[string]any{"type": "text", "text": s})
	return string(b)
}

func imagePart() string { return `{"type":"image","source":{"type":"base64","data":"x"}}` }

func offloadStub(completion string) func(string) (string, error) {
	return func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"completion":` + jsonEncode(completion) + `}`)), nil
	}
}

func jsonEncode(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func countCommand(h *sdktest.Harness, cmd string) int {
	n := 0
	for _, c := range h.Calls() {
		if c.Command == cmd {
			n++
		}
	}
	return n
}

func reqWith(msgs ...*pbv2.Message) *pbv2.ChatRequest {
	return &pbv2.ChatRequest{Messages: msgs}
}

// assertBlocked asserts EXACTLY one block verdict with status 422, the given
// code, and a message free of the given secrets.
func assertBlocked(t *testing.T, h *sdktest.Harness, code string, secrets ...string) {
	t.Helper()
	blocks := h.BlockCalls()
	if len(blocks) != 1 {
		t.Fatalf("expected exactly one block verdict, got %d", len(blocks))
	}
	args := sdktest.DecodeBlockArgs(t, blocks[0].Args)
	if args.Status != 422 {
		t.Fatalf("block status=%d, want 422", args.Status)
	}
	if args.Code != code {
		t.Fatalf("block code=%q, want %q", args.Code, code)
	}
	for _, secret := range secrets {
		if strings.Contains(args.Message, secret) {
			t.Fatalf("block message must be value-free, leaked %q: %q", secret, args.Message)
		}
	}
}

// ==========================================================================
// P1 — extraction
// ==========================================================================

func TestExtractScannableTable(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		parts    string
		wantText string
		complete bool
	}{
		{"scalar only", "line one\nline two", "", "line one\nline two", true},
		{"parts only", "", "[" + textPart("part a") + "," + textPart("part b") + "]", "part a\npart b", true},
		{"both scalar and parts", "scalar", "[" + textPart("part") + "]", "scalar\npart", true},
		{"multiple parts stable lines", "", "[" + textPart("first") + "," + textPart("second") + "]", "first\nsecond", true},
		{"valid empty collection", "", "[]", "", true},
		{"empty text parts", "", "[" + textPart("") + "]", "", true},
		{"malformed JSON", "", "not json", "", false},
		{"malformed JSON retains scalar", "scalar", "not json", "scalar", false},
		{"non-array top level", "", `{"type":"text","text":"x"}`, "", false},
		{"top-level null", "", "null", "", false},
		{"malformed part object", "scalar", `[{"type":"text"`, "scalar", false},
		{"malformed text (non-string)", "", `[{"type":"text","text":42}]`, "", false},
		{"missing text field", "", `[{"type":"text"}]`, "", false},
		{"text null", "", `[{"type":"text","text":null}]`, "", false},
		{"image part", "", "[" + imagePart() + "]", "", false},
		{"image after text retains text", "", "[" + textPart("kept") + "," + imagePart() + "]", "kept", false},
		{"text after image retained", "", "[" + imagePart() + "," + textPart("kept") + "]", "kept", false},
		{"leading empty text part", "", "[" + textPart("") + "," + textPart("x") + "]", "\nx", true},
		{"middle empty text part", "", "[" + textPart("a") + "," + textPart("") + "," + textPart("b") + "]", "a\n\nb", true},
		{"consecutive empty text parts", "", "[" + textPart("") + "," + textPart("") + "," + textPart("x") + "]", "\n\nx", true},
		{"empty part before unsupported", "", "[" + textPart("") + "," + imagePart() + "," + textPart("x") + "]", "\nx", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := toolMsg("c1", "read", tc.content, []byte(tc.parts))
			got := extractScannable(msg)
			if got.complete != tc.complete {
				t.Fatalf("complete=%v, want %v", got.complete, tc.complete)
			}
			if got.text != tc.wantText {
				t.Fatalf("text=%q, want %q", got.text, tc.wantText)
			}
		})
	}
}

// TestKnownPIIBlocksDespiteUnsupportedPart — finding 1: a deterministic PII
// fact in retained text blocks as pii_detected even when an unsupported part
// makes the extraction incomplete, under BOTH on_error modes.
func TestKnownPIIBlocksDespiteUnsupportedPart(t *testing.T) {
	content := "contact victim@example.com"
	for name, onError := range map[string]string{"block": "block", "allow": "allow"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"on_error":"` + onError + `"}`)
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", content, []byte("["+imagePart()+"]"))))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			assertBlocked(t, h, "pii_detected", "victim@example.com")
		})
	}

	// Text part with PII BEFORE an unsupported part.
	h := newHarness(t)
	h.SetConfig(`{"on_error":"allow"}`)
	parts := "[" + textPart("ssn 123-45-6789") + "," + imagePart() + "]"
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "", []byte(parts))))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	assertBlocked(t, h, "pii_detected", "123-45-6789")

	// Unsupported part BEFORE a text part with PII.
	h2 := newHarness(t)
	h2.SetConfig(`{"on_error":"allow"}`)
	parts2 := "[" + imagePart() + "," + textPart("key AKIA1234567890ABCDEF") + "]"
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", "", []byte(parts2))))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("err=%v", res2.Err)
	}
	assertBlocked(t, h2, "pii_detected", "AKIA1234567890ABCDEF")
}

// TestUnknownUnscannableContentFollowsOnError — incomplete extraction with NO
// deterministic finding: on_error block vetoes with pii_scan_failed, allow
// forwards, and nothing is cached or model-scanned.
func TestUnknownUnscannableContentFollowsOnError(t *testing.T) {
	for name, onError := range map[string]string{"block": "block", "allow": "allow"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"on_error":"` + onError + `"}`)
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "", []byte("["+imagePart()+"]"))))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			if onError == "block" {
				assertBlocked(t, h, "pii_scan_failed")
			} else if len(h.BlockCalls()) != 0 {
				t.Fatalf("allow must forward unknown unscannable content: %+v", h.BlockCalls())
			}
			if n := countCommand(h, "env.cache_set"); n != 0 {
				t.Fatalf("incomplete extractions must never be cached, got %d writes", n)
			}
			if n := countCommand(h, "torana_offload_completion"); n != 0 {
				t.Fatalf("incomplete extractions must never be model-scanned, got %d calls", n)
			}
		})
	}
}

// TestCacheKeyFramingIsUnambiguous — finding 5: the length-prefixed key must
// not collide for distinct identities joined with NULs.
func TestCacheKeyFramingIsUnambiguous(t *testing.T) {
	// The reviewer's reproduction: the joined identity (id + name) collides
	// under NUL framing — (id "a", name "b\x00c") and (id "a\x00b", name
	// "c") both join to "a\x00b\x00c". The length-prefixed key must not.
	a := toolMsg("a", "", "same", nil)
	b := toolMsg("a\x00b", "", "same", nil)
	if piiCleanCacheKey(a, "b\x00c") == piiCleanCacheKey(b, "c") {
		t.Fatal("NUL-join collision must not exist with length-prefixed framing")
	}
	partA, _ := json.Marshal(map[string]any{"type": "text", "text": "hello"})
	partB, _ := json.Marshal(map[string]any{"type": "text", "text": "hello!"})
	if piiCleanCacheKey(toolMsg("c1", "read", "", []byte("["+string(partA)+"]")), "read") ==
		piiCleanCacheKey(toolMsg("c1", "read", "", []byte("["+string(partB)+"]")), "read") {
		t.Fatal("changing only the structured bytes must change the cache key")
	}
}

// ==========================================================================
// Hook matrix
// ==========================================================================

// TestRegexCategoriesBlock — each deterministic category blocks with the
// pii_detected code and a value-free message naming the category and line.
func TestRegexCategoriesBlock(t *testing.T) {
	cases := []struct {
		name, content, wantType string
	}{
		{"email", "contact someone@example.com now", "email"},
		{"us ssn", "ssn: 123-45-6789", "us_ssn"},
		{"aws access key", "key AKIA1234567890ABCDEF", "aws_access_key"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----", "private_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", tc.content, nil)))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			assertBlocked(t, h, "pii_detected")
			args := sdktest.DecodeBlockArgs(t, h.BlockCalls()[0].Args)
			if !strings.Contains(args.Message, tc.wantType) {
				t.Fatalf("block must name the category: %q", args.Message)
			}
			if !strings.Contains(args.Message, "line 1") {
				t.Fatalf("block must carry the line number: %q", args.Message)
			}
		})
	}
}

// TestDuplicateToolCallIDsAmbiguous — finding 2: duplicated/reused IDs are
// ambiguous and err toward scanning, in either order and for same-name
// duplicates.
func TestDuplicateToolCallIDsAmbiguous(t *testing.T) {
	email := "contact someone@example.com"
	for name, mk := range map[string]func() *pbv2.ChatRequest{
		"read then excluded": func() *pbv2.ChatRequest {
			return &pbv2.ChatRequest{Messages: []*pbv2.Message{
				{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}},
				{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "same", Name: "excluded", ArgumentsJson: []byte(`{}`)}}},
				toolMsg("same", "", email, nil),
			}}
		},
		"excluded then read": func() *pbv2.ChatRequest {
			return &pbv2.ChatRequest{Messages: []*pbv2.Message{
				{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "same", Name: "excluded", ArgumentsJson: []byte(`{}`)}}},
				{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}},
				toolMsg("same", "", email, nil),
			}}
		},
		"same-name duplicates": func() *pbv2.ChatRequest {
			return &pbv2.ChatRequest{Messages: []*pbv2.Message{
				{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}},
				{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}},
				toolMsg("same", "", email, nil),
			}}
		},
		"reuse in a later message": func() *pbv2.ChatRequest {
			return &pbv2.ChatRequest{Messages: []*pbv2.Message{
				{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}},
				{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "same", Name: "excluded", ArgumentsJson: []byte(`{}`)}}},
				{Role: "user", Content: "later"},
				toolMsg("same", "", email, nil),
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"tools":["read"]}`)
			res := h.BeforeRequest(mk())
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			if len(h.BlockCalls()) != 1 {
				t.Fatal("an ambiguous id must err toward scanning")
			}
		})
	}

	// An explicit tool-result name remains authoritative.
	h := newHarness(t)
	h.SetConfig(`{"tools":["read"]}`)
	req := &pbv2.ChatRequest{Messages: []*pbv2.Message{
		{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}},
		{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "same", Name: "excluded", ArgumentsJson: []byte(`{}`)}}},
		toolMsg("same", "read", "contact someone@example.com", nil),
	}}
	res := h.BeforeRequest(req)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	if len(h.BlockCalls()) != 1 {
		t.Fatal("an explicit authoritative name must still scan")
	}
}

// TestCleanCacheSkipsRescan — the plugin's own cache round-trip.
func TestCleanCacheSkipsRescan(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"provider":"local","model":"qwen"}`)
	h.StubHostCall("torana_offload_completion", offloadStub(`{"pii":false,"findings":[]}`))
	first := h.BeforeRequest(reqWith(toolMsg("c1", "read", "clean output here", nil)))
	if first.Err != nil || !first.PassedThrough {
		t.Fatalf("err=%v", first.Err)
	}
	second := h.BeforeRequest(reqWith(toolMsg("c1", "read", "clean output here", nil)))
	if second.Err != nil || !second.PassedThrough {
		t.Fatalf("err=%v", second.Err)
	}
	if n := countCommand(h, "torana_offload_completion"); n != 1 {
		t.Fatalf("a cached clean verdict must skip the scan, got %d offload calls", n)
	}

	// Present-empty: unusable, rescan.
	h2 := newHarness(t)
	h2.SetConfig(`{"provider":"local","model":"qwen"}`)
	h2.Run(func() { loadConfig() })
	h2.SeedCache(piiCleanCacheKey(toolMsg("c1", "read", "clean output here", nil), "read"), "")
	h2.StubHostCall("torana_offload_completion", offloadStub(`{"pii":false,"findings":[]}`))
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", "clean output here", nil)))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("err=%v", res2.Err)
	}
	if n := countCommand(h2, "torana_offload_completion"); n != 1 {
		t.Fatalf("present-empty must rescan, got %d offload calls", n)
	}
}

// TestCacheRefusalClasses — advisory cache refusals decline to a scan the
// plugin can still do; contract refusals and malformed frames error the hook.
func TestCacheRefusalClasses(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("env.cache_get", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no cache"), nil
	})
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "no pii here", nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("advisory cache refusal must still scan, err=%v", res.Err)
	}
	if len(h.BlockCalls()) != 0 {
		t.Fatal("clean regex-only content must not block")
	}

	h2 := newHarness(t)
	h2.DenyPermission("env.cache_get")
	if res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", "no pii", nil))); res2.Err == nil {
		t.Fatal("a contract cache refusal must error the hook")
	}

	h3 := newHarness(t)
	h3.StubHostCall("env.cache_get", func(string) (string, error) {
		return "not a frame", nil
	})
	if res3 := h3.BeforeRequest(reqWith(toolMsg("c1", "read", "no pii", nil))); res3.Err == nil {
		t.Fatal("a malformed cache frame must error the hook")
	}
}

// TestAllowlistSemantics — ["read"] scans only read; ["*"] and empty scan
// all; an unknown name with an allowlist still scans.
func TestAllowlistSemantics(t *testing.T) {
	email := "contact someone@example.com"
	h := newHarness(t)
	h.SetConfig(`{"tools":["read"]}`)
	h.BeforeRequest(reqWith(toolMsg("c1", "grep", email, nil)))
	if len(h.BlockCalls()) != 0 {
		t.Fatal("grep must not be scanned under the read-only allowlist")
	}
	h.BeforeRequest(reqWith(toolMsg("c2", "read", email, nil)))
	if len(h.BlockCalls()) != 1 {
		t.Fatal("read must be scanned under the allowlist")
	}

	h2 := newHarness(t)
	h2.SetConfig(`{"tools":["*"]}`)
	h2.BeforeRequest(reqWith(toolMsg("c1", "anything", email, nil)))
	if len(h2.BlockCalls()) != 1 {
		t.Fatal("* must scan every tool")
	}

	h3 := newHarness(t)
	h3.SetConfig(`{"tools":["read"]}`)
	h3.BeforeRequest(reqWith(toolMsg("c1", "", email, nil)))
	if len(h3.BlockCalls()) != 1 {
		t.Fatal("an unknown tool name with an allowlist must still scan")
	}
}

// TestModelScanHappyPath — a model verdict of PII blocks; a clean verdict
// caches and passes.
func TestModelScanHappyPath(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"provider":"local","model":"qwen"}`)
	h.StubHostCall("torana_offload_completion", offloadStub(`{"pii":true,"findings":[{"type":"email","line":3}]}`))
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "line1\nline2\nline3", nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	assertBlocked(t, h, "pii_detected")

	h2 := newHarness(t)
	h2.SetConfig(`{"provider":"local","model":"qwen"}`)
	h2.StubHostCall("torana_offload_completion", offloadStub(`{"pii":false,"findings":[]}`))
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", "clean text", nil)))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("err=%v", res2.Err)
	}
	if len(h2.BlockCalls()) != 0 {
		t.Fatal("a clean model verdict must not block")
	}
	if n := countCommand(h2, "env.cache_set"); n != 1 {
		t.Fatalf("a clean verdict must be cached, got %d writes", n)
	}
}

// TestModelVerdictShapeValidation — finding 3: pii must be present, non-null,
// boolean; findings must be an array; contradictory shapes are scanner
// failures governed by on_error, never clean, never cached.
func TestModelVerdictShapeValidation(t *testing.T) {
	completions := map[string]string{
		"missing pii":                       `{}`,
		"null pii":                          `{"pii":null,"findings":[]}`,
		"string pii":                        `{"pii":"yes","findings":[]}`,
		"number pii":                        `{"pii":1,"findings":[]}`,
		"null findings":                     `{"pii":false,"findings":null}`,
		"object findings":                   `{"pii":false,"findings":{"type":"email"}}`,
		"string findings":                   `{"pii":false,"findings":"none"}`,
		"contradictory false with findings": `{"pii":false,"findings":[{"type":"email","line":1}]}`,
	}
	for name, completion := range completions {
		for mode, onError := range map[string]string{"block": "block", "allow": "allow"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				h := newHarness(t)
				h.SetConfig(`{"provider":"local","model":"qwen","on_error":"` + onError + `"}`)
				h.StubHostCall("torana_offload_completion", offloadStub(completion))
				res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil)))
				if res.Err != nil || !res.PassedThrough {
					t.Fatalf("err=%v", res.Err)
				}
				if onError == "block" {
					assertBlocked(t, h, "pii_scan_failed")
				} else if len(h.BlockCalls()) != 0 {
					t.Fatalf("allow must forward a malformed verdict: %+v", h.BlockCalls())
				}
				if n := countCommand(h, "env.cache_set"); n != 0 {
					t.Fatalf("a malformed verdict must never be cached, got %d writes", n)
				}
			})
		}
	}
}

// TestModelCategoryNormalization — finding 4: a model-controlled category is
// never echoed verbatim; unknown values map to unspecified, and the decoded
// block message cannot contain the echoed secret.
func TestModelCategoryNormalization(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"provider":"local","model":"qwen"}`)
	secret := "victim@example.com"
	h.StubHostCall("torana_offload_completion", offloadStub(
		`{"pii":true,"findings":[{"type":"`+secret+`","line":1}]}`))
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	args := sdktest.DecodeBlockArgs(t, h.BlockCalls()[0].Args)
	if strings.Contains(args.Message, secret) {
		t.Fatalf("the model-controlled category was echoed: %q", args.Message)
	}
	if !strings.Contains(args.Message, "unspecified") {
		t.Fatalf("an unknown category must map to unspecified: %q", args.Message)
	}

	// Aliases normalize to the documented set.
	if got := normalizeCategory("SSN"); got != "us_ssn" {
		t.Fatalf("alias SSN -> %q, want us_ssn", got)
	}
	if got := normalizeCategory("api key"); got != "api_key" {
		t.Fatalf("alias api key -> %q, want api_key", got)
	}
	if got := normalizeCategory("email"); got != "email" {
		t.Fatalf("known category -> %q, want email", got)
	}

	// Line numbers are clamped before display.
	h2 := newHarness(t)
	h2.SetConfig(`{"provider":"local","model":"qwen"}`)
	h2.StubHostCall("torana_offload_completion", offloadStub(`{"pii":true,"findings":[{"type":"email","line":-7}]}`))
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil)))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("err=%v", res2.Err)
	}
	args2 := sdktest.DecodeBlockArgs(t, h2.BlockCalls()[0].Args)
	if strings.Contains(args2.Message, "-7") {
		t.Fatalf("a negative line must be clamped: %q", args2.Message)
	}
}

// TestToolLabelSafety — the tool name is displayed only after conservative
// validation; the raw tool-call id is never included.
func TestToolLabelSafety(t *testing.T) {
	if got := toolLabel("read"); got != "`read` output" {
		t.Fatalf("safe name: %q", got)
	}
	for _, bad := range []string{"", "x\nsecret", "a b c", strings.Repeat("x", 65), "read;rm"} {
		if got := toolLabel(bad); got != "a tool result" {
			t.Fatalf("unsafe name %q displayed as %q", bad, got)
		}
	}
}

// TestModelScanRefusalClasses — advisory offload refusals are a scanner
// failure governed by on_error; contract refusals and malformed frames error
// the hook regardless of on_error.
func TestModelScanRefusalClasses(t *testing.T) {
	for _, code := range []pbv2.ErrorCode{
		pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
		pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE,
	} {
		t.Run("advisory/"+code.String(), func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"provider":"local","model":"qwen","on_error":"block"}`)
			h.StubHostCall("torana_offload_completion", func(string) (string, error) {
				return sdktest.HostResultError(code, "stub"), nil
			})
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil)))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			assertBlocked(t, h, "pii_scan_failed")
		})
	}

	t.Run("advisory allow", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(`{"provider":"local","model":"qwen","on_error":"allow"}`)
		h.StubHostCall("torana_offload_completion", func(string) (string, error) {
			return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, "stub"), nil
		})
		res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil)))
		if res.Err != nil || !res.PassedThrough {
			t.Fatalf("err=%v", res.Err)
		}
		if len(h.BlockCalls()) != 0 {
			t.Fatal("allow must forward on an advisory offload refusal")
		}
	})

	for _, code := range []pbv2.ErrorCode{
		pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
		pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
		pbv2.ErrorCode_ERROR_CODE_INTERNAL,
	} {
		t.Run("contract/"+code.String(), func(t *testing.T) {
			for _, onError := range []string{"block", "allow"} {
				h := newHarness(t)
				h.SetConfig(`{"provider":"local","model":"qwen","on_error":"` + onError + `"}`)
				h.StubHostCall("torana_offload_completion", func(string) (string, error) {
					return sdktest.HostResultError(code, "stub"), nil
				})
				if res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil))); res.Err == nil {
					t.Fatalf("contract refusal must error the hook regardless of on_error=%s", onError)
				}
			}
		})
	}

	t.Run("malformed frame", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(`{"provider":"local","model":"qwen","on_error":"allow"}`)
		h.StubHostCall("torana_offload_completion", func(string) (string, error) {
			return "not a frame", nil
		})
		if res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil))); res.Err == nil {
			t.Fatal("a malformed offload frame must error the hook")
		}
	})

	t.Run("unparseable verdict", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(`{"provider":"local","model":"qwen","on_error":"block"}`)
		h.StubHostCall("torana_offload_completion", offloadStub(`no json here`))
		res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil)))
		if res.Err != nil || !res.PassedThrough {
			t.Fatalf("err=%v", res.Err)
		}
		assertBlocked(t, h, "pii_scan_failed")
	})
}

func TestExtractJSONCases(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain {"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{`{"a":"{"} trailing`, `{"a":"{"}`},
		{`{"a":1} {"b":2}`, `{"a":1}`},
	}
	for _, tc := range cases {
		if got := extractJSON(tc.in); got != tc.want {
			t.Errorf("extractJSON(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMaxScanBytesTruncation — the byte budget is rune-safe and asserted by
// BYTE length; the stubbed offload payload never exceeds it.
func TestMaxScanBytesTruncation(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"provider":"local","model":"qwen","max_scan_bytes":100}`)
	var payload string
	h.StubHostCall("torana_offload_completion", func(args string) (string, error) {
		payload = args
		return sdktest.HostResultValue([]byte(`{"completion":"{\"pii\":false,\"findings\":[]}"}`)), nil
	})
	content := strings.Repeat("日本語", 500)
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", content, nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	idx := strings.Index(payload, "Output to scan:\\n")
	if idx < 0 {
		t.Fatal("offload payload missing the scan content")
	}
	scanned := payload[idx+len("Output to scan:\\n"):]
	scanned = strings.TrimSuffix(scanned, `"}`)
	var decoded string
	_ = json.Unmarshal([]byte(`"`+scanned+`"`), &decoded)
	if len(decoded) > 100 {
		t.Fatalf("scanned bytes=%d exceed the 100-byte budget", len(decoded))
	}
	if !utf8.ValidString(decoded) {
		t.Fatal("truncation split a rune")
	}
}

// TestScannerPairConfiguration — both-or-neither with ZERO offload calls for
// an invalid pair; a deterministic regex finding still blocks under a
// mispaired configuration (the safer ordering).
func TestScannerPairConfiguration(t *testing.T) {
	for name, cfg := range map[string]string{
		"provider only": `{"provider":"local"}`,
		"model only":    `{"model":"qwen"}`,
	} {
		t.Run(name+"/block", func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(cfg)
			h.StubHostCall("torana_offload_completion", offloadStub(`{"pii":false,"findings":[]}`))
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil)))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			if n := countCommand(h, "torana_offload_completion"); n != 0 {
				t.Fatalf("an invalid scanner pair must make ZERO offload calls, got %d", n)
			}
			assertBlocked(t, h, "pii_scan_failed")
		})
		t.Run(name+"/allow", func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(cfg + `,"on_error":"allow"`)
			h.StubHostCall("torana_offload_completion", offloadStub(`{"pii":false,"findings":[]}`))
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil)))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			if n := countCommand(h, "torana_offload_completion"); n != 0 {
				t.Fatalf("an invalid scanner pair must make ZERO offload calls, got %d", n)
			}
			if len(h.BlockCalls()) != 0 {
				t.Fatalf("allow must forward an invalid pair: %+v", h.BlockCalls())
			}
		})
	}

	// A deterministic regex finding blocks even when the pair is mispaired.
	h := newHarness(t)
	h.SetConfig(`{"provider":"local"}`)
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "contact someone@example.com", nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	assertBlocked(t, h, "pii_detected", "someone@example.com")

	// Both absent: regex-only works without any offload.
	h2 := newHarness(t)
	h2.StubHostCall("torana_offload_completion", offloadStub(`{"pii":true,"findings":[]}`))
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", "clean text", nil)))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("regex-only mode must work, err=%v", res2.Err)
	}
	if n := countCommand(h2, "torana_offload_completion"); n != 0 {
		t.Fatalf("regex-only mode must not call the model, got %d", n)
	}
}

// TestBlockReturnsPassAndNoWriteGrant — P2: every block row returns
// pass-through content and the plugin never touches a write grant.
func TestBlockReturnsPassAndNoWriteGrant(t *testing.T) {
	h := newHarness(t)
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "contact someone@example.com", nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("a block verdict must be pass-through, err=%v", res.Err)
	}
	for _, c := range h.Calls() {
		if strings.HasPrefix(c.Command, "ir.") {
			t.Errorf("pii made a write-grant-class call: %s", c.Command)
		}
	}
}

func TestNoUnauthorizedCalls(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"provider":"local","model":"qwen"}`)
	h.StubHostCall("torana_offload_completion", offloadStub(`{"pii":false,"findings":[]}`))
	h.BeforeRequest(reqWith(toolMsg("c1", "read", "clean", nil)))
	allowed := map[string]bool{
		"env.plugin_config":         true,
		"env.cache_get":             true,
		"env.cache_set":             true,
		"env.block_request":         true,
		"torana_offload_completion": true,
	}
	for _, c := range h.Calls() {
		if !allowed[c.Command] {
			t.Errorf("host call outside the declared permission set: %s", c.Command)
		}
	}
}

// TestSchemaDefaultsMatchRuntimeDefaults — schema.json defaults and the pair
// constraint + byte-budget name.
func TestSchemaDefaultsMatchRuntimeDefaults(t *testing.T) {
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatalf("read schema.json: %v", err)
	}
	var schema struct {
		Properties        map[string]json.RawMessage `json:"properties"`
		DependentRequired map[string][]string        `json:"dependentRequired"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if _, legacy := schema.Properties["max_scan_chars"]; legacy {
		t.Fatal("legacy max_scan_chars must not exist")
	}
	var budget struct{ Default int }
	if err := json.Unmarshal(schema.Properties["max_scan_bytes"], &budget); err != nil || budget.Default != 0 {
		t.Fatalf("max_scan_bytes default must be 0: %+v", budget)
	}
	var onError struct{ Default string }
	if err := json.Unmarshal(schema.Properties["on_error"], &onError); err != nil || onError.Default != "block" {
		t.Fatalf("on_error default=%q, want block", onError.Default)
	}
	dep, ok := schema.DependentRequired["provider"]
	if !ok || len(dep) != 1 || dep[0] != "model" {
		t.Fatalf("provider must depend on model: %v", dep)
	}
	if _, ok := schema.DependentRequired["model"]; !ok {
		t.Fatal("model must depend on provider")
	}
	rt := parseConfig("")
	if rt.OnError != "block" || rt.Provider != "" || rt.Model != "" || rt.MaxScanBytes != 0 {
		t.Fatalf("runtime defaults %+v do not match the schema", rt)
	}
	if !rt.scannerPairValid() {
		t.Fatal("empty pair must be valid (regex-only)")
	}
}

// TestConfigResetPinsIsolation — contradictory configs across sequential
// rows, using an UNSCANNABLE structured value so leaked config fails the
// test (a plain email regex hit would block identically under both modes).
func TestConfigResetPinsIsolation(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"on_error":"block"}`)
	h.BeforeRequest(reqWith(toolMsg("c1", "read", "", []byte("["+imagePart()+"]"))))
	if len(h.BlockCalls()) != 1 {
		t.Fatal("row 1 must fail closed on unscannable content")
	}
	h2 := newHarness(t)
	h2.SetConfig(`{"on_error":"allow"}`)
	h2.BeforeRequest(reqWith(toolMsg("c2", "read", "", []byte("["+imagePart()+"]"))))
	if len(h2.BlockCalls()) != 0 {
		t.Fatal("row 2 leaked row 1's fail-closed policy")
	}
}

func TestDeterminismOverIdenticalRequests(t *testing.T) {
	h := newHarness(t)
	r1 := h.BeforeRequest(reqWith(toolMsg("c1", "read", "clean", nil)))
	r2 := h.BeforeRequest(reqWith(toolMsg("c1", "read", "clean", nil)))
	if r1.Err != nil || r2.Err != nil || !r1.PassedThrough || !r2.PassedThrough {
		t.Fatalf("errors: %v %v", r1.Err, r2.Err)
	}
	b1, _ := json.Marshal(r1.Request)
	b2, _ := json.Marshal(r2.Request)
	if string(b1) != string(b2) {
		t.Fatal("identical requests produced different output")
	}
}

// TestEmptyPartPreservesLineBoundary — finding 1 (round 2): an empty first
// text part must keep its newline, so a later finding reports line 2, not
// line 1.
func TestEmptyPartPreservesLineBoundary(t *testing.T) {
	h := newHarness(t)
	parts := "[" + textPart("") + "," + textPart("contact victim@example.com") + "]"
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "", []byte(parts))))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	assertBlocked(t, h, "pii_detected", "victim@example.com")
	args := sdktest.DecodeBlockArgs(t, h.BlockCalls()[0].Args)
	if !strings.Contains(args.Message, "line 2") {
		t.Fatalf("the empty leading part must push the finding to line 2: %q", args.Message)
	}
}

// TestRegexFindingCapAndMessageBound — a request producing more findings than
// the cap renders exactly the cap, flags overflow, and keeps the message
// bounded and deterministic.
func TestRegexFindingCapAndMessageBound(t *testing.T) {
	content := ""
	for i := 0; i < 100; i++ {
		content += "line with someone" + itoa(i) + "@example.com\n"
	}
	h := newHarness(t)
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", content, nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	args := sdktest.DecodeBlockArgs(t, h.BlockCalls()[0].Args)
	if !strings.Contains(args.Message, "Additional findings omitted") {
		t.Fatalf("overflow note missing: %q", args.Message)
	}
	if len(args.Message) > 4096 {
		t.Fatalf("block message unbounded: %d bytes", len(args.Message))
	}
	// Deterministic ordering: findings render in line order.
	first := strings.Index(args.Message, "line 1")
	second := strings.Index(args.Message, "line 2")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("findings out of order: %q", args.Message)
	}
}

// TestModelFindingCapAndLineValidation — a hostile model reply with thousands
// of findings renders at most the cap; line numbers beyond the actual scanned
// text are omitted; the message stays bounded.
func TestModelFindingCapAndLineValidation(t *testing.T) {
	findings := ""
	for i := 0; i < 100; i++ {
		findings += `{"type":"email","line":1},`
	}
	findings = findings[:len(findings)-1]
	h := newHarness(t)
	h.SetConfig(`{"provider":"local","model":"qwen"}`)
	h.StubHostCall("torana_offload_completion", offloadStub(`{"pii":true,"findings":[`+findings+`]}`))
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "one line", nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	args := sdktest.DecodeBlockArgs(t, h.BlockCalls()[0].Args)
	if !strings.Contains(args.Message, "Additional findings omitted") {
		t.Fatalf("overflow note missing: %q", args.Message)
	}
	if len(args.Message) > 4096 {
		t.Fatalf("block message unbounded: %d bytes", len(args.Message))
	}

	// A one-line input with model lines 2 and 999999: both implausible and
	// omitted (the category renders without a bogus line).
	h2 := newHarness(t)
	h2.SetConfig(`{"provider":"local","model":"qwen"}`)
	h2.StubHostCall("torana_offload_completion", offloadStub(
		`{"pii":true,"findings":[{"type":"email","line":2},{"type":"email","line":999999}]}`))
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", "one line", nil)))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("err=%v", res2.Err)
	}
	args2 := sdktest.DecodeBlockArgs(t, h2.BlockCalls()[0].Args)
	if strings.Contains(args2.Message, "line 2") || strings.Contains(args2.Message, "999999") {
		t.Fatalf("implausible lines must be omitted: %q", args2.Message)
	}
	if !strings.Contains(args2.Message, "email") {
		t.Fatalf("the category must still render: %q", args2.Message)
	}
}

// TestCleanCacheKeyAuthoritativeInputs — every clean-cache input changes the
// key: tool-call ID, resolved name, exact scalar, exact structured bytes, and
// each policy field.
func TestCleanCacheKeyAuthoritativeInputs(t *testing.T) {
	msg := toolMsg("c1", "read", "scalar", nil)
	// Deterministic base under the DEFAULT config.
	baseHarness := newHarness(t)
	baseHarness.Run(func() { loadConfig() })
	base := piiCleanCacheKey(msg, "read")
	cases := []struct {
		name string
		key  func() string
	}{
		{"tool call id", func() string { return piiCleanCacheKey(toolMsg("c2", "read", "scalar", nil), "read") }},
		{"resolved name", func() string { return piiCleanCacheKey(msg, "other") }},
		{"exact scalar", func() string { return piiCleanCacheKey(toolMsg("c1", "read", "scalar!", nil), "read") }},
		{"structured bytes", func() string {
			return piiCleanCacheKey(toolMsg("c1", "read", "", []byte("["+textPart("x")+"]")), "read")
		}},
	}
	for _, tc := range cases {
		if tc.key() == base {
			t.Errorf("%s must change the cache key", tc.name)
		}
	}
	// Each policy field is authoritative: a NON-default baseline, and every
	// row mutates EXACTLY ONE field (the provider row must not change model
	// at the same time, or it would not independently prove provider
	// authority).
	baseline := `{"provider":"p","model":"m","tools":["read"],"on_error":"allow","max_scan_bytes":123}`
	baseHarness.Run(func() { loadConfig() }) // ensure defaults first
	h0 := newHarness(t)
	h0.SetConfig(baseline)
	h0.Run(func() { loadConfig() })
	baseKey := piiCleanCacheKey(msg, "read")
	policyCases := []struct {
		name string
		cfg  string
	}{
		{"provider", `{"provider":"p2","model":"m","tools":["read"],"on_error":"allow","max_scan_bytes":123}`},
		{"model", `{"provider":"p","model":"m2","tools":["read"],"on_error":"allow","max_scan_bytes":123}`},
		{"tools", `{"provider":"p","model":"m","tools":["grep"],"on_error":"allow","max_scan_bytes":123}`},
		{"on_error", `{"provider":"p","model":"m","tools":["read"],"on_error":"block","max_scan_bytes":123}`},
		{"max_scan_bytes", `{"provider":"p","model":"m","tools":["read"],"on_error":"allow","max_scan_bytes":456}`},
	}
	for _, tc := range policyCases {
		h := newHarness(t)
		h.SetConfig(tc.cfg)
		h.Run(func() { loadConfig() })
		if got := piiCleanCacheKey(msg, "read"); got == baseKey {
			t.Errorf("policy field %s must change the cache key", tc.name)
		}
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// TestBlockMessageDirectBound — the final boundedness proof calls
// blockMessage DIRECTLY with 100,000 findings and no caller-supplied flag:
// the message stays < 4 KiB, is deterministic, renders exactly 20 findings,
// and carries the omission note.
func TestBlockMessageDirectBound(t *testing.T) {
	findings := make([]finding, 100_000)
	for i := range findings {
		findings[i] = finding{Type: "email", Line: i + 1}
	}
	msg := blockMessage("read", findings)
	if len(msg) > 4096 {
		t.Fatalf("block message unbounded: %d bytes", len(msg))
	}
	if n := strings.Count(msg, "(line "); n != 20 {
		t.Fatalf("rendered %d findings, want exactly 20", n)
	}
	if !strings.Contains(msg, "Additional findings omitted") {
		t.Fatal("the omission note must be present")
	}
	if again := blockMessage("read", findings); again != msg {
		t.Fatal("block message must be deterministic")
	}
}

// TestFindingCapBoundaries — cap-1, cap, and cap+1 for BOTH producers: the
// note appears only past the cap, and the message stays bounded.
func TestFindingCapBoundaries(t *testing.T) {
	// Regex producer: one unique email per line.
	regexContent := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString("someone" + itoa(i) + "@example.com\n")
		}
		return b.String()
	}
	for _, n := range []int{19, 20, 21} {
		t.Run("regex/"+itoa(n), func(t *testing.T) {
			h := newHarness(t)
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", regexContent(n), nil)))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			args := sdktest.DecodeBlockArgs(t, h.BlockCalls()[0].Args)
			note := strings.Contains(args.Message, "Additional findings omitted")
			if (n > 20) != note {
				t.Fatalf("n=%d: note present=%v, want %v", n, note, n > 20)
			}
		})
	}

	// Model producer.
	modelCompletion := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"pii":true,"findings":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"type":"email","line":1}`)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	for _, n := range []int{19, 20, 21} {
		t.Run("model/"+itoa(n), func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"provider":"local","model":"qwen"}`)
			h.StubHostCall("torana_offload_completion", offloadStub(modelCompletion(n)))
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "one line", nil)))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			args := sdktest.DecodeBlockArgs(t, h.BlockCalls()[0].Args)
			note := strings.Contains(args.Message, "Additional findings omitted")
			if (n > 20) != note {
				t.Fatalf("n=%d: note present=%v, want %v", n, note, n > 20)
			}
		})
	}
}

// TestEmptyLineNumberingAfterSplitSeq — leading, middle, and trailing empty
// lines keep their positions with the allocation-free iterator.
func TestEmptyLineNumberingAfterSplitSeq(t *testing.T) {
	content := "\n\ncontact someone@example.com\n\n"
	findings := regexScan(content)
	if len(findings) != 1 {
		t.Fatalf("findings=%d, want 1", len(findings))
	}
	if findings[0].Line != 3 {
		t.Fatalf("line=%d, want 3 (two leading empty lines)", findings[0].Line)
	}
	h := newHarness(t)
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", content, nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	args := sdktest.DecodeBlockArgs(t, h.BlockCalls()[0].Args)
	if !strings.Contains(args.Message, "line 3") {
		t.Fatalf("hook-level line numbering wrong: %q", args.Message)
	}
}

// BenchmarkRegexScanLargeSuffix — evidence that the bounded scan does not
// allocate per suffix line: the findings sit in the first cap+1 lines, and a
// large noise suffix follows. Allocation must not scale with the suffix.
func BenchmarkRegexScanLargeSuffix(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 21; i++ {
		sb.WriteString("someone" + itoa(i) + "@example.com\n")
	}
	for i := 0; i < 100_000; i++ {
		sb.WriteString("noise line without matches\n")
	}
	content := sb.String()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := regexScan(content)
		if len(out) != 21 {
			b.Fatalf("len=%d, want 21 (cap+1 sentinel)", len(out))
		}
	}
}
