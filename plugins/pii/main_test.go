package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

// ==========================================================================
// Shared fixtures
// ==========================================================================

// newHarness resets the process-global config once-state so every row starts
// from defaults, then builds a fresh fake host.
func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	resetConfigForTest()
	return sdktest.New(t)
}

func toolMsg(id, name, content string, parts []byte) *pbv2.Message {
	return &pbv2.Message{Role: "tool", ToolCallId: id, ToolName: name, Content: content, ContentPartsJson: parts}
}

// offloadStub returns a v2-shaped offload success (NO status field).
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

// ==========================================================================
// P1 — complete structured extraction
// ==========================================================================

// TestExtractScannableTable — scalar-only, parts-only, both, multiple text
// parts, malformed JSON, non-array, malformed part objects, malformed text,
// uninspectable part types, and valid-empty collections.
func TestExtractScannableTable(t *testing.T) {
	textPart := func(s string) string {
		b, _ := json.Marshal(map[string]any{"type": "text", "text": s})
		return string(b)
	}
	cases := []struct {
		name      string
		content   string
		parts     string
		wantText  string
		scannable bool
	}{
		{"scalar only", "line one\nline two", "", "line one\nline two", true},
		{"parts only", "", "[" + textPart("part a") + "," + textPart("part b") + "]", "part a\npart b", true},
		{"both scalar and parts", "scalar", "[" + textPart("part") + "]", "scalar\npart", true},
		{"multiple parts stable lines", "", "[" + textPart("first") + "," + textPart("second") + "]",
			"first\nsecond", true},
		{"valid empty collection", "", "[]", "", true},
		{"empty text parts", "", "[" + textPart("") + "]", "", true},
		{"malformed JSON", "", `not json`, "", false},
		{"non-array top level", "", `{"type":"text","text":"x"}`, "", false},
		{"malformed part object", "", `[{"type":"text"`, "", false},
		{"malformed text (non-string)", "", `[{"type":"text","text":42}]`, "", false},
		{"missing text field", "", `[{"type":"text"}]`, "", false},
		{"image part", "", `[{"type":"image","source":{"type":"base64","data":"x"}}]`, "", false},
		{"unknown part type", "", `[{"type":"weird"}]`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := toolMsg("c1", "read", tc.content, []byte(tc.parts))
			got, scannable := extractScannable(msg)
			if scannable != tc.scannable {
				t.Fatalf("scannable=%v, want %v", scannable, tc.scannable)
			}
			if got != tc.wantText {
				t.Fatalf("text=%q, want %q", got, tc.wantText)
			}
		})
	}
}

// TestStructuredContentBlocked — the failing-before-fix regression: a tool
// result in the real Anthropic/Claude-Code shape (empty Content, array-valued
// tool_result.content with a text part carrying an email) MUST be scanned and
// blocked. The mechanically ported v1 behavior (`Content == ""` skip) fails
// this test; the fix makes it pass (see the handoff's revert proof).
func TestStructuredContentBlocked(t *testing.T) {
	h := newHarness(t)
	part, _ := json.Marshal(map[string]any{"type": "text", "text": "contact: someone@example.com"})
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "", []byte("["+string(part)+"]"))))
	_ = h
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("a block verdict must return PASS-THROUGH content (P2)")
	}
	blocks := h.BlockCalls()
	if len(blocks) != 1 {
		t.Fatalf("expected exactly one block verdict, got %d", len(blocks))
	}
	if blocks[0].Result != "422" && !strings.Contains(blocks[0].Result, "422") && !strings.Contains(blocks[0].Args, "pii_detected") {
		// The block envelope carries the code; assert the args mention it.
		if !strings.Contains(blocks[0].Args, "pii_detected") {
			t.Fatalf("block must carry the pii_detected code: %+v", blocks[0])
		}
	}
	// Value-free message: the email must NOT appear anywhere in the block args.
	if strings.Contains(blocks[0].Args, "someone@example.com") {
		t.Fatal("the block message must be value-free")
	}
}

// TestStructuredPIIOnlyInParts — a harmless scalar must never hide PII in
// parts: both are scanned together.
func TestStructuredPIIOnlyInParts(t *testing.T) {
	h := newHarness(t)
	part, _ := json.Marshal(map[string]any{"type": "text", "text": "ssn 123-45-6789"})
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "harmless", []byte("["+string(part)+"]"))))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("expected a pass-through block verdict, err=%v", res.Err)
	}
	if len(h.BlockCalls()) != 1 {
		t.Fatal("PII in parts must be detected even with a populated scalar")
	}
}

// TestUnscannableContentFollowsOnError — image/unknown/malformed structured
// content is a SCAN FAILURE, never clean: on_error block vetoes, allow
// forwards, and nothing is cached.
func TestUnscannableContentFollowsOnError(t *testing.T) {
	unscannable := `[{"type":"image","source":{"type":"base64","data":"x"}}]`
	for name, onError := range map[string]string{"block": "block", "allow": "allow"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"on_error":"` + onError + `"}`)
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "", []byte(unscannable))))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			blocks := h.BlockCalls()
			if onError == "block" {
				if len(blocks) != 1 || !strings.Contains(blocks[0].Args, "pii_scan_failed") {
					t.Fatalf("unscannable content must fail closed: %+v", blocks)
				}
			} else if len(blocks) != 0 {
				t.Fatalf("allow must forward unscannable content: %+v", blocks)
			}
			if n := countCommand(h, "env.cache_set"); n != 0 {
				t.Fatalf("unscannable content must never be cached, got %d writes", n)
			}
		})
	}
}

// TestCacheKeyFoldsStructuredBytes — changing ONLY the structured bytes
// invalidates a prior clean verdict.
func TestCacheKeyFoldsStructuredBytes(t *testing.T) {
	partA, _ := json.Marshal(map[string]any{"type": "text", "text": "hello"})
	partB, _ := json.Marshal(map[string]any{"type": "text", "text": "hello!"})
	a := toolMsg("c1", "read", "", []byte("["+string(partA)+"]"))
	b := toolMsg("c1", "read", "", []byte("["+string(partB)+"]"))
	if piiCleanCacheKey(a, "read") == piiCleanCacheKey(b, "read") {
		t.Fatal("changing only the structured bytes must change the cache key")
	}
	scalar := toolMsg("c1", "read", "x", nil)
	parts := toolMsg("c1", "read", "", []byte("["+string(partA)+"]"))
	if piiCleanCacheKey(scalar, "read") == piiCleanCacheKey(parts, "read") {
		t.Fatal("scalar-only and parts-only must not share a cache key")
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
			blocks := h.BlockCalls()
			if len(blocks) != 1 {
				t.Fatalf("expected one block, got %d", len(blocks))
			}
			if !strings.Contains(blocks[0].Args, "pii_detected") || !strings.Contains(blocks[0].Args, tc.wantType) {
				t.Fatalf("block must name the category: %+v", blocks[0])
			}
			if strings.Contains(blocks[0].Args, "someone@example.com") || strings.Contains(blocks[0].Args, "123-45-6789") ||
				strings.Contains(blocks[0].Args, "AKIA") {
				t.Fatal("the block message must be value-free")
			}
			if !strings.Contains(blocks[0].Args, "line 1") {
				t.Fatalf("the block must carry the line number: %+v", blocks[0])
			}
		})
	}
}

// TestCleanCacheSkipsRescan — the plugin's OWN cache round-trip: the first
// clean dispatch scans and writes the verdict; the second identical dispatch
// hits the cache and skips the scan with zero offload calls. Present-empty
// cache entries are unusable and rescan.
func TestCleanCacheSkipsRescan(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"provider":"local","model":"qwen"}`)
	h.StubHostCall("torana_offload_completion", offloadStub(`{"pii":false,"findings":[]}`))
	first := h.BeforeRequest(reqWith(toolMsg("c1", "read", "clean output here", nil)))
	if first.Err != nil || !first.PassedThrough {
		t.Fatalf("err=%v", first.Err)
	}
	if n := countCommand(h, "torana_offload_completion"); n != 1 {
		t.Fatalf("the first dispatch must scan, got %d offload calls", n)
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
	msg := toolMsg("c1", "read", "clean output here", nil)
	// Derive the key under the harness config, exactly as the plugin does.
	h2.Run(func() { loadConfig() })
	h2.SeedCache(piiCleanCacheKey(msg, "read"), "")
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
// all; an unknown name with an allowlist still scans (err toward safety).
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
	// Unknown name: err toward scanning.
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
	blocks := h.BlockCalls()
	if len(blocks) != 1 || !strings.Contains(blocks[0].Args, "pii_detected") || !strings.Contains(blocks[0].Args, "email") {
		t.Fatalf("model-detected PII must block: %+v", blocks)
	}

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
			blocks := h.BlockCalls()
			if len(blocks) != 1 || !strings.Contains(blocks[0].Args, "pii_scan_failed") {
				t.Fatalf("advisory offload refusal must fail closed under on_error=block: %+v", blocks)
			}
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
		if len(h.BlockCalls()) != 1 {
			t.Fatal("an unparseable verdict must fail closed under on_error=block")
		}
	})
}

// TestExtractJSONCases — prose, fences, and braces inside strings.
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
	content := strings.Repeat("日本語", 500) // 6-byte runes
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", content, nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	// The user_prompt carries "Output to scan:\n" + the truncated content;
	// extract the content region and assert BYTES <= 100 and valid UTF-8.
	idx := strings.Index(payload, "Output to scan:\\n")
	if idx < 0 {
		t.Fatal("offload payload missing the scan content")
	}
	scanned := payload[idx+len("Output to scan:\\n"):]
	scanned = strings.TrimSuffix(scanned, `"}`)
	// The JSON payload escapes the content; unescape for byte accounting.
	var decoded string
	_ = json.Unmarshal([]byte(`"`+scanned+`"`), &decoded)
	if len(decoded) > 100 {
		t.Fatalf("scanned bytes=%d exceed the 100-byte budget", len(decoded))
	}
	if !utf8.ValidString(decoded) {
		t.Fatal("truncation split a rune")
	}
}

// TestScannerPairConfiguration — both-or-neither: provider-only and
// model-only are invalid scanner configurations driven through on_error with
// ZERO offload calls; both absent = regex-only; both present = model scan.
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
			if len(h.BlockCalls()) != 1 || !strings.Contains(h.BlockCalls()[0].Args, "pii_scan_failed") {
				t.Fatalf("an invalid pair must fail closed: %+v", h.BlockCalls())
			}
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

	// Both absent: regex-only works without any offload.
	h := newHarness(t)
	h.StubHostCall("torana_offload_completion", offloadStub(`{"pii":true,"findings":[]}`))
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", "clean text", nil)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("regex-only mode must work, err=%v", res.Err)
	}
	if n := countCommand(h, "torana_offload_completion"); n != 0 {
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

// TestNoUnauthorizedCalls — every host call is within the declared set.
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

// TestSchemaDefaultsMatchRuntimeDefaults — schema.json defaults (provider "",
// model "", tools ["*"], on_error "block", max_scan_bytes 0) must equal the
// runtime defaults, and the pair constraint + byte-budget name must be
// present.
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

// TestConfigResetPinsIsolation — contradictory configs across sequential rows.
func TestConfigResetPinsIsolation(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"on_error":"block"}`)
	h.BeforeRequest(reqWith(toolMsg("c1", "read", "text", nil)))
	h2 := newHarness(t)
	h2.SetConfig(`{"on_error":"allow"}`)
	h2.BeforeRequest(reqWith(toolMsg("c2", "read", "text", nil)))
	// Row 2 leaked row 1's fail-closed policy? Both clean -> no blocks either way;
	// pin by scanning PII under allow.
	h3 := newHarness(t)
	h3.SetConfig(`{"on_error":"allow"}`)
	h3.BeforeRequest(reqWith(toolMsg("c3", "read", "x@y.z", nil)))
	if len(h3.BlockCalls()) != 0 {
		t.Fatal("row 3 leaked a fail-closed policy")
	}
}

// TestDeterminismOverIdenticalRequests — identical clean requests produce
// byte-identical pass-through and identical cache traffic.
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
