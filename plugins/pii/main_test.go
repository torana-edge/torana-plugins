package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

// ==========================================================================
// Shared fixtures
// ==========================================================================

func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	resetConfigForTest()
	return sdktest.New(t)
}

// toolMsg builds a tool-role message whose tool-result block carries the
// given ordered content arms.
func toolMsg(id, name string, arms ...*pbv1.ToolResultContentBlock) *pbv1.Message {
	return &pbv1.Message{Role: "tool", Blocks: []*pbv1.RequestBlock{{
		Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{
			ToolCallId: id, ToolName: name, Content: arms,
		}},
	}}}
}

func textArm(s string) *pbv1.ToolResultContentBlock {
	return &pbv1.ToolResultContentBlock{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: s}}}
}

// unknownArm is the ordered analog of the flat image part: a
// provider-visible arm the text scanner cannot inspect.
func unknownArm() *pbv1.ToolResultContentBlock {
	return &pbv1.ToolResultContentBlock{Kind: &pbv1.ToolResultContentBlock_Unknown{Unknown: &pbv1.ToolResultUnknownBlock{Kind: "image", PayloadJson: []byte(`{"source":{"type":"base64","data":"x"}}`)}}}
}

func markerArm() *pbv1.ToolResultContentBlock {
	return &pbv1.ToolResultContentBlock{Kind: &pbv1.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pbv1.ToolResultCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}}
}

func modelStub(content string) func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
	return func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
		return modelResult(content), nil, nil
	}
}

func modelResult(content string) *pbv1.ModelCompleteResult {
	return &pbv1.ModelCompleteResult{Message: &pbv1.ResponseMessage{Blocks: []*pbv1.ResponseBlock{{Kind: &pbv1.ResponseBlock_Text{Text: &pbv1.ResponseTextBlock{Text: content}}}}}, FinishReason: "stop"}
}

func TestStrictModelTextRejectsUnsupportedBlocks(t *testing.T) {
	tool := &pbv1.ResponseBlock{Kind: &pbv1.ResponseBlock_ToolCall{ToolCall: &pbv1.ToolCall{Name: "read", ArgumentsJson: []byte(`{}`)}}}
	mixed := &pbv1.ModelCompleteResult{Message: &pbv1.ResponseMessage{Blocks: []*pbv1.ResponseBlock{
		{Kind: &pbv1.ResponseBlock_Text{Text: &pbv1.ResponseTextBlock{Text: `{"pii":false}`}}}, tool,
	}}}
	if _, err := strictModelText(mixed); err == nil {
		t.Fatal("mixed text/tool scanner response must be rejected rather than treated as clean")
	}
	if _, err := strictModelText(&pbv1.ModelCompleteResult{Message: &pbv1.ResponseMessage{Blocks: []*pbv1.ResponseBlock{tool}}}); err == nil {
		t.Fatal("tool-only scanner response must be rejected")
	}
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

func requestCompleted(result sdktest.RequestResult) bool {
	return result.Err == nil && (result.PassedThrough || result.Request != nil)
}

func reqWith(msgs ...*pbv1.Message) *pbv1.ChatRequest {
	return &pbv1.ChatRequest{Messages: msgs}
}

func protectedMessage(t *testing.T, h *sdktest.Harness) (string, bool) {
	t.Helper()
	for i := len(h.Calls()) - 1; i >= 0; i-- {
		call := h.Calls()[i]
		if call.Command != "env.state_compare_and_set" {
			continue
		}
		var args pbv1.StateCompareAndSetArgs
		if err := proto.Unmarshal([]byte(call.Args), &args); err != nil {
			t.Fatalf("decode state compare-and-set: %v", err)
		}
		var record replayRecord
		if err := json.Unmarshal([]byte(args.Value), &record); err != nil {
			t.Fatalf("decode replay record: %v", err)
		}
		return record.Replacement, true
	}
	return "", false
}

// assertBlocked retains the old test name while asserting the new behavior:
// the request passes with a persisted, value-free replacement.
func assertBlocked(t *testing.T, h *sdktest.Harness, code string, secrets ...string) {
	t.Helper()
	message, ok := protectedMessage(t, h)
	if !ok {
		t.Fatalf("expected a persisted replacement for %s", code)
	}
	for _, secret := range secrets {
		if strings.Contains(message, secret) {
			t.Fatalf("replacement must be value-free, leaked %q: %q", secret, message)
		}
	}
}

// ==========================================================================
// P1 — extraction
// ==========================================================================

func TestExtractScannableTable(t *testing.T) {
	cases := []struct {
		name     string
		arms     []*pbv1.ToolResultContentBlock
		wantText string
		complete bool
	}{
		{"single text arm", []*pbv1.ToolResultContentBlock{textArm("line one\nline two")}, "line one\nline two", true},
		{"two text arms", []*pbv1.ToolResultContentBlock{textArm("part a"), textArm("part b")}, "part a\npart b", true},
		{"multiple text arms stable lines", []*pbv1.ToolResultContentBlock{textArm("first"), textArm("second")}, "first\nsecond", true},
		{"valid empty collection", nil, "", true},
		{"explicit empty text arm", []*pbv1.ToolResultContentBlock{textArm("")}, "", true},
		{"unknown arm", []*pbv1.ToolResultContentBlock{unknownArm()}, "", false},
		{"unknown after text retains text", []*pbv1.ToolResultContentBlock{textArm("kept"), unknownArm()}, "kept", false},
		{"text after unknown retained", []*pbv1.ToolResultContentBlock{unknownArm(), textArm("kept")}, "kept", false},
		{"leading empty text arm", []*pbv1.ToolResultContentBlock{textArm(""), textArm("x")}, "\nx", true},
		{"middle empty text arm", []*pbv1.ToolResultContentBlock{textArm("a"), textArm(""), textArm("b")}, "a\n\nb", true},
		{"consecutive empty text arms", []*pbv1.ToolResultContentBlock{textArm(""), textArm(""), textArm("x")}, "\n\nx", true},
		{"empty arm before unknown", []*pbv1.ToolResultContentBlock{textArm(""), unknownArm(), textArm("x")}, "\nx", false},
		// Cache-marker arms are the plugin's own carriers: skipped without
		// affecting completeness (never provider content).
		{"marker arm skipped", []*pbv1.ToolResultContentBlock{textArm("a"), markerArm(), textArm("b")}, "a\nb", true},
		{"marker-only is a valid empty result", []*pbv1.ToolResultContentBlock{markerArm()}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := toolMsg("c1", "read", tc.arms...)
			got := extractScannable(sdk.ToolResults(msg)[0])
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
	content := "key sk_test_torana_demo_not_a_real_key_123"
	for name, onError := range map[string]string{"block": "block", "allow": "allow"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"on_error":"` + onError + `"}`)
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm(content), unknownArm())))
			if !requestCompleted(res) {
				t.Fatalf("err=%v", res.Err)
			}
			assertBlocked(t, h, "pii_detected", "sk_test_torana_demo_not_a_real_key_123")
		})
	}

	// Text arm with PII BEFORE an unsupported arm.
	h := newHarness(t)
	h.SetConfig(`{"on_error":"allow"}`)
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("ssn 123-45-6789"), unknownArm())))
	if !requestCompleted(res) {
		t.Fatalf("err=%v", res.Err)
	}
	assertBlocked(t, h, "pii_detected", "123-45-6789")

	// Unsupported arm BEFORE a text arm with PII.
	h2 := newHarness(t)
	h2.SetConfig(`{"on_error":"allow"}`)
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", unknownArm(), textArm("key AKIA1234567890ABCDEF"))))
	if !requestCompleted(res2) {
		t.Fatalf("err=%v", res2.Err)
	}
	assertBlocked(t, h2, "pii_detected", "AKIA1234567890ABCDEF")
}

func TestFreeformToolOutputIsScanned(t *testing.T) {
	h := newHarness(t)
	msg := toolMsg("call_1", "exec", textArm("key sk_test_torana_demo_not_a_real_key_123"))
	msg.Blocks[0].GetToolResult().InvocationKind = pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM
	res := h.BeforeRequest(reqWith(msg))
	if !requestCompleted(res) {
		t.Fatalf("err=%v passed=%v", res.Err, res.PassedThrough)
	}
	assertBlocked(t, h, "pii_detected", "sk_test_torana_demo_not_a_real_key_123")
}

// TestUnknownUnscannableContentFollowsOnError — incomplete extraction with NO
// deterministic finding: on_error block vetoes with pii_scan_failed, allow
// forwards, and nothing is cached or model-scanned.
func TestUnknownUnscannableContentFollowsOnError(t *testing.T) {
	for name, onError := range map[string]string{"block": "block", "allow": "allow"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"on_error":"` + onError + `"}`)
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", unknownArm())))
			if !requestCompleted(res) {
				t.Fatalf("err=%v", res.Err)
			}
			if onError == "block" {
				assertBlocked(t, h, "pii_scan_failed")
			} else if _, protected := protectedMessage(t, h); protected {
				t.Fatal("allow must forward unknown unscannable content")
			}
			if n := countCommand(h, "env.cache_set"); n != 0 {
				t.Fatalf("incomplete extractions must never be cached, got %d writes", n)
			}
			if n := countCommand(h, "env.model_complete"); n != 0 {
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
	a := sdk.ToolResults(toolMsg("a", "", textArm("same")))[0]
	b := sdk.ToolResults(toolMsg("a\x00b", "", textArm("same")))[0]
	if piiCleanCacheKey(a, "b\x00c") == piiCleanCacheKey(b, "c") {
		t.Fatal("NUL-join collision must not exist with length-prefixed framing")
	}
	// Changing only the scannable text must change the key.
	textA := sdk.ToolResults(toolMsg("c1", "read", textArm("hello")))[0]
	textB := sdk.ToolResults(toolMsg("c1", "read", textArm("hello!")))[0]
	if piiCleanCacheKey(textA, "read") == piiCleanCacheKey(textB, "read") {
		t.Fatal("changing only the scannable text must change the cache key")
	}
	// An added cache-marker arm is the plugin's own carrier: the clean
	// verdict depends only on the composed text, so the key is unchanged.
	withMarker := sdk.ToolResults(toolMsg("c1", "read", textArm("hello"), markerArm()))[0]
	if piiCleanCacheKey(withMarker, "read") != piiCleanCacheKey(textA, "read") {
		t.Fatal("a cache-marker arm must not change the clean verdict key")
	}
}

// ==========================================================================
// Hook matrix
// ==========================================================================

// TestUserRoleResultIsACandidate — the ordered seam: EVERY message's
// tool-result blocks are candidates (no role gate), so a user-role result
// carrying PII must block exactly like a tool-role one.
func TestUserRoleResultIsACandidate(t *testing.T) {
	msg := &pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{
		{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "u"}}},
		{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{
			ToolCallId: "c1", ToolName: "read",
			Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: "key sk_test_torana_demo_not_a_real_key_123"}}}},
		}}},
	}}
	h := newHarness(t)
	res := h.BeforeRequest(reqWith(msg))
	if !requestCompleted(res) {
		t.Fatalf("err=%v", res.Err)
	}
	assertBlocked(t, h, "pii_detected", "sk_test_torana_demo_not_a_real_key_123")
}

// TestRegexCategoriesBlock — each deterministic category blocks with the
// pii_detected code and a value-free message naming the category and line.
func TestRegexCategoriesBlock(t *testing.T) {
	cases := []struct {
		name, content, wantType string
	}{
		{"us ssn", "ssn: 123-45-6789", "us_ssn"},
		{"aws access key", "key AKIA1234567890ABCDEF", "aws_access_key"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----", "private_key"},
		{"dash api key", "key sk-proj-abcdefghijklmnopqrstuvwxyz123456", "api_key"},
		{"underscore api key", "key sk_test_torana_demo_not_a_real_key_123", "api_key"},
		{"restricted api key", "key rk_live_torana_demo_not_a_real_key_123", "api_key"},
		{"github token", "token ghp_abcdefghijklmnopqrstuvwxyz0123456789", "access_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm(tc.content))))
			if !requestCompleted(res) {
				t.Fatalf("err=%v", res.Err)
			}
			assertBlocked(t, h, "pii_detected")
			message, _ := protectedMessage(t, h)
			if !strings.Contains(message, tc.wantType) {
				t.Fatalf("replacement must name the category: %q", message)
			}
			if !strings.Contains(message, "line 1") {
				t.Fatalf("replacement must carry the line number: %q", message)
			}
		})
	}
}

func TestEmailUsesContextualScanner(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{}`)
	h.StubModelComplete(modelStub(`{"pii":false,"findings":[]}`))
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("commit author someone@example.com"))))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("public email should follow the contextual scanner verdict: %+v", res)
	}
	if n := countCommand(h, "env.model_complete"); n != 1 {
		t.Fatalf("email triggered %d contextual scans, want 1", n)
	}
}

func TestRegexRequiredLiteralPrefilterMatchesReference(t *testing.T) {
	cases := []string{
		"plain output without trigger bytes",
		"contains @ but not an email",
		"hyphenated-but-not-an-ssn",
		"AKIA-short",
		"-----BEGIN but not a private key",
		"sk-short",
		"sk_test_short",
		"rk_live_short",
		"ghp_short",
		"contact someone@example.com now",
		"ssn: 123-45-6789",
		"key AKIA1234567890ABCDEF",
		"-----BEGIN RSA PRIVATE KEY-----",
		"someone@example.com and 123-45-6789\nAKIA1234567890ABCDEF\n-----BEGIN PRIVATE KEY-----",
		"first@example.com second@example.com",
		"wordAKIA1234567890ABCDEFword",
	}
	for _, input := range cases {
		if got, want := regexScan(input), regexScanWithoutPrefilter(input); !reflect.DeepEqual(got, want) {
			t.Fatalf("input %q: filtered=%+v reference=%+v", input, got, want)
		}
	}
}

func FuzzRegexRequiredLiteralPrefilterMatchesReference(f *testing.F) {
	for _, seed := range []string{
		"plain",
		"someone@example.com",
		"123-45-6789",
		"AKIA1234567890ABCDEF",
		"-----BEGIN EC PRIVATE KEY-----",
		"@-AKIA-----BEGIN \n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if got, want := regexScan(input), regexScanWithoutPrefilter(input); !reflect.DeepEqual(got, want) {
			t.Fatalf("filtered=%+v reference=%+v", got, want)
		}
	})
}

func regexScanWithoutPrefilter(content string) []finding {
	var out []finding
	seen := map[string]bool{}
	lineNo := 0
	for line := range strings.SplitSeq(content, "\n") {
		lineNo++
		for _, p := range piiPatterns {
			if !p.re.MatchString(line) {
				continue
			}
			key := p.name + ":" + strconv.Itoa(lineNo)
			if seen[key] {
				continue
			}
			if len(out) >= maxReportedFindings {
				return append(out, finding{Type: "overflow", Line: 0})
			}
			seen[key] = true
			out = append(out, finding{Type: p.name, Line: lineNo})
		}
	}
	return out
}

// TestDuplicateToolCallIDsAmbiguous — finding 2: duplicated/reused IDs are
// ambiguous and err toward scanning, in either order and for same-name
// duplicates.
func TestDuplicateToolCallIDsAmbiguous(t *testing.T) {
	sensitive := "key sk_test_torana_demo_not_a_real_key_123"
	for name, mk := range map[string]func() *pbv1.ChatRequest{
		"read then excluded": func() *pbv1.ChatRequest {
			return &pbv1.ChatRequest{Messages: []*pbv1.Message{
				{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}}}},
				{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "same", Name: "excluded", ArgumentsJson: []byte(`{}`)}}}}},
				toolMsg("same", "", textArm(sensitive)),
			}}
		},
		"excluded then read": func() *pbv1.ChatRequest {
			return &pbv1.ChatRequest{Messages: []*pbv1.Message{
				{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "same", Name: "excluded", ArgumentsJson: []byte(`{}`)}}}}},
				{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}}}},
				toolMsg("same", "", textArm(sensitive)),
			}}
		},
		"same-name duplicates": func() *pbv1.ChatRequest {
			return &pbv1.ChatRequest{Messages: []*pbv1.Message{
				{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}}}},
				{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}}}},
				toolMsg("same", "", textArm(sensitive)),
			}}
		},
		"reuse in a later message": func() *pbv1.ChatRequest {
			return &pbv1.ChatRequest{Messages: []*pbv1.Message{
				{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}}}},
				{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "same", Name: "excluded", ArgumentsJson: []byte(`{}`)}}}}},
				{Role: "user", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "later"}}}}},
				toolMsg("same", "", textArm(sensitive)),
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"tools":["read"]}`)
			res := h.BeforeRequest(mk())
			if !requestCompleted(res) {
				t.Fatalf("err=%v", res.Err)
			}
			if _, ok := protectedMessage(t, h); !ok {
				t.Fatal("an ambiguous id must err toward scanning")
			}
		})
	}

	// An explicit tool-result name remains authoritative.
	h := newHarness(t)
	h.SetConfig(`{"tools":["read"]}`)
	req := &pbv1.ChatRequest{Messages: []*pbv1.Message{
		{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "same", Name: "read", ArgumentsJson: []byte(`{}`)}}}}},
		{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "same", Name: "excluded", ArgumentsJson: []byte(`{}`)}}}}},
		toolMsg("same", "read", textArm("key sk_test_torana_demo_not_a_real_key_123")),
	}}
	res := h.BeforeRequest(req)
	if !requestCompleted(res) {
		t.Fatalf("err=%v", res.Err)
	}
	if _, ok := protectedMessage(t, h); !ok {
		t.Fatal("an explicit authoritative name must still scan")
	}
}

// TestCleanCacheSkipsRescan — the plugin's own cache round-trip.
func TestCleanCacheSkipsRescan(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{}`)
	h.StubModelComplete(modelStub(`{"pii":false,"findings":[]}`))
	first := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("clean output here"))))
	if first.Err != nil || !first.PassedThrough {
		t.Fatalf("err=%v", first.Err)
	}
	second := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("clean output here"))))
	if second.Err != nil || !second.PassedThrough {
		t.Fatalf("err=%v", second.Err)
	}
	if n := countCommand(h, "env.model_complete"); n != 1 {
		t.Fatalf("a cached clean verdict must skip the scan, got %d model calls", n)
	}

	// Present-empty: unusable, rescan.
	h2 := newHarness(t)
	h2.SetConfig(`{}`)
	h2.Run(func() { loadConfig() })
	h2.SeedCache(piiCleanCacheKey(sdk.ToolResults(toolMsg("c1", "read", textArm("clean output here")))[0], "read"), "")
	h2.StubModelComplete(modelStub(`{"pii":false,"findings":[]}`))
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("clean output here"))))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("err=%v", res2.Err)
	}
	if n := countCommand(h2, "env.model_complete"); n != 1 {
		t.Fatalf("present-empty must rescan, got %d model calls", n)
	}
}

// TestCacheRefusalClasses — advisory cache refusals decline to a scan the
// plugin can still do; contract refusals and malformed frames error the hook.
func TestCacheRefusalClasses(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("env.cache_get", func(string) (string, error) {
		return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no cache"), nil
	})
	h.StubModelComplete(modelStub(`{"pii":false,"findings":[]}`))
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("no pii here"))))
	if !requestCompleted(res) {
		t.Fatalf("advisory cache refusal must still scan, err=%v", res.Err)
	}
	if len(h.BlockCalls()) != 0 {
		t.Fatal("clean regex-only content must not block")
	}

	h2 := newHarness(t)
	h2.DenyPermission("env.cache_get")
	if res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("no pii")))); res2.Err == nil {
		t.Fatal("a contract cache refusal must error the hook")
	}

	h3 := newHarness(t)
	h3.StubHostCall("env.cache_get", func(string) (string, error) {
		return "not a frame", nil
	})
	if res3 := h3.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("no pii")))); res3.Err == nil {
		t.Fatal("a malformed cache frame must error the hook")
	}
}

func TestDeniedReplayWriteCannotReturnSuccess(t *testing.T) {
	h := newHarness(t)
	h.DenyPermission("env.state_compare_and_set")
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("key sk_test_torana_demo_not_a_real_key_123"))))
	if res.Err == nil || res.PassedThrough {
		t.Fatalf("denied replay write must fail the hook, passed=%v err=%v", res.PassedThrough, res.Err)
	}
}

// TestAllowlistSemantics — ["read"] scans only read; ["*"] and empty scan
// all; an unknown name with an allowlist still scans.
func TestAllowlistSemantics(t *testing.T) {
	sensitive := "key sk_test_torana_demo_not_a_real_key_123"
	h := newHarness(t)
	h.SetConfig(`{"tools":["read"]}`)
	h.BeforeRequest(reqWith(toolMsg("c1", "grep", textArm(sensitive))))
	if _, ok := protectedMessage(t, h); ok {
		t.Fatal("grep must not be scanned under the read-only allowlist")
	}
	h.BeforeRequest(reqWith(toolMsg("c2", "read", textArm(sensitive))))
	if _, ok := protectedMessage(t, h); !ok {
		t.Fatal("read must be scanned under the allowlist")
	}

	h2 := newHarness(t)
	h2.SetConfig(`{"tools":["*"]}`)
	h2.BeforeRequest(reqWith(toolMsg("c1", "anything", textArm(sensitive))))
	if _, ok := protectedMessage(t, h2); !ok {
		t.Fatal("* must scan every tool")
	}

	h3 := newHarness(t)
	h3.SetConfig(`{"tools":["read"]}`)
	h3.BeforeRequest(reqWith(toolMsg("c1", "", textArm(sensitive))))
	if _, ok := protectedMessage(t, h3); !ok {
		t.Fatal("an unknown tool name with an allowlist must still scan")
	}
}

// TestModelScanHappyPath — a model verdict of PII blocks; a clean verdict
// caches and passes.
func TestModelScanHappyPath(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{}`)
	h.StubModelComplete(modelStub(`{"pii":true,"findings":[{"type":"email","line":3}]}`))
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("line1\nline2\nline3"))))
	if !requestCompleted(res) {
		t.Fatalf("err=%v", res.Err)
	}
	assertBlocked(t, h, "pii_detected")

	h2 := newHarness(t)
	h2.SetConfig(`{}`)
	h2.StubModelComplete(modelStub(`{"pii":false,"findings":[]}`))
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("clean text"))))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("err=%v", res2.Err)
	}
	if _, ok := protectedMessage(t, h2); ok {
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
				h.SetConfig(`{"on_error":"` + onError + `"}`)
				h.StubModelComplete(modelStub(completion))
				res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("text"))))
				if !requestCompleted(res) {
					t.Fatalf("err=%v", res.Err)
				}
				if onError == "block" {
					assertBlocked(t, h, "pii_scan_failed")
				} else if _, protected := protectedMessage(t, h); protected {
					t.Fatal("allow must forward a malformed verdict")
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
	h.SetConfig(`{}`)
	secret := "victim@example.com"
	h.StubModelComplete(modelStub(
		`{"pii":true,"findings":[{"type":"` + secret + `","line":1}]}`))
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("text"))))
	if !requestCompleted(res) {
		t.Fatalf("err=%v", res.Err)
	}
	message, _ := protectedMessage(t, h)
	if strings.Contains(message, secret) {
		t.Fatalf("the model-controlled category was echoed: %q", message)
	}
	if !strings.Contains(message, "unspecified") {
		t.Fatalf("an unknown category must map to unspecified: %q", message)
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
	h2.SetConfig(`{}`)
	h2.StubModelComplete(modelStub(`{"pii":true,"findings":[{"type":"email","line":-7}]}`))
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("text"))))
	if !requestCompleted(res2) {
		t.Fatalf("err=%v", res2.Err)
	}
	message2, _ := protectedMessage(t, h2)
	if strings.Contains(message2, "-7") {
		t.Fatalf("a negative line must be clamped: %q", message2)
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

// TestModelScanRefusalClasses — advisory model-service refusals are a scanner
// failure governed by on_error; contract refusals and malformed frames error
// the hook regardless of on_error.
func TestModelScanRefusalClasses(t *testing.T) {
	for _, code := range []pbv1.ErrorCode{
		pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
		pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE,
	} {
		t.Run("advisory/"+code.String(), func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"on_error":"block"}`)
			h.StubModelComplete(func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
				return nil, &pbv1.HostError{Code: code, Message: "stub"}, nil
			})
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("text"))))
			if !requestCompleted(res) {
				t.Fatalf("err=%v", res.Err)
			}
			assertBlocked(t, h, "pii_scan_failed")
		})
	}

	t.Run("advisory allow", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(`{"on_error":"allow"}`)
		h.StubModelComplete(func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
			return nil, &pbv1.HostError{Code: pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, Message: "stub"}, nil
		})
		res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("text"))))
		if !requestCompleted(res) {
			t.Fatalf("err=%v", res.Err)
		}
		if _, ok := protectedMessage(t, h); ok {
			t.Fatal("allow must forward on an advisory model-service refusal")
		}
	})

	for _, code := range []pbv1.ErrorCode{
		pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
		pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
		pbv1.ErrorCode_ERROR_CODE_INTERNAL,
	} {
		t.Run("contract/"+code.String(), func(t *testing.T) {
			for _, onError := range []string{"block", "allow"} {
				h := newHarness(t)
				h.SetConfig(`{"on_error":"` + onError + `"}`)
				h.StubModelComplete(func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
					return nil, &pbv1.HostError{Code: code, Message: "stub"}, nil
				})
				if res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("text")))); res.Err == nil {
					t.Fatalf("contract refusal must error the hook regardless of on_error=%s", onError)
				}
			}
		})
	}

	t.Run("malformed frame", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(`{"on_error":"allow"}`)
		h.StubHostCall("env.model_complete", func(string) (string, error) {
			return "not a frame", nil
		})
		if res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("text")))); res.Err == nil {
			t.Fatal("a malformed model-service frame must error the hook")
		}
	})

	t.Run("unparseable verdict", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(`{"on_error":"block"}`)
		h.StubModelComplete(modelStub(`no json here`))
		res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("text"))))
		if !requestCompleted(res) {
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
// BYTE length. A clean prefix verdict remains incomplete for the full result:
// on_error governs it and it is never cached as clean.
func TestMaxScanBytesTruncation(t *testing.T) {
	for _, onError := range []string{"block", "allow"} {
		t.Run(onError, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"max_scan_bytes":100,"on_error":"` + onError + `"}`)
			var request *pbv1.ModelCompleteArgs
			h.StubModelComplete(func(args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
				request = args
				return modelResult(`{"pii":false,"findings":[]}`), nil, nil
			})
			content := strings.Repeat("日本語", 500)
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm(content))))
			if !requestCompleted(res) {
				t.Fatalf("err=%v", res.Err)
			}
			if request == nil || len(request.Messages) != 2 {
				t.Fatalf("model request = %+v, want two messages", request)
			}
			const marker = "Output to scan:\n"
			idx := strings.Index(sdk.Text(request.Messages[1]), marker)
			if idx < 0 {
				t.Fatalf("model user message missing scan marker: %q", sdk.Text(request.Messages[1]))
			}
			scanned := sdk.Text(request.Messages[1])[idx+len(marker):]
			if len(scanned) > 100 {
				t.Fatalf("scanned bytes=%d exceed the 100-byte budget", len(scanned))
			}
			if !utf8.ValidString(scanned) {
				t.Fatal("truncation split a rune")
			}
			if n := countCommand(h, "env.cache_set"); n != 0 {
				t.Fatalf("incomplete scan wrote %d clean cache entries", n)
			}
			_, blocked := protectedMessage(t, h)
			if want := onError == "block"; blocked != want {
				t.Fatalf("blocked=%v, want %v for on_error=%s", blocked, want, onError)
			}
		})
	}
}

// TestScannerModelServiceContract pins the provider-neutral request. Provider,
// model, URL, and credentials are operator-owned binding data and never enter
// plugin configuration or the guest request.
func TestScannerModelServiceContract(t *testing.T) {
	h := newHarness(t)
	var got *pbv1.ModelCompleteArgs
	h.StubModelComplete(func(args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
		got = args
		return modelResult(`{"pii":false,"findings":[]}`), nil, nil
	})
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("clean text"))))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	if got == nil || got.Service != "scanner" || len(got.Messages) != 2 {
		t.Fatalf("model request = %+v", got)
	}
	if got.Messages[0].Role != "system" || sdk.Text(got.Messages[0]) != piiSystemPrompt {
		t.Fatalf("system message = %+v", got.Messages[0])
	}
	if got.Messages[1].Role != "user" || !strings.Contains(sdk.Text(got.Messages[1]), "clean text") {
		t.Fatalf("user message = %+v", got.Messages[1])
	}
	if got.MaxTokens == nil || *got.MaxTokens != 512 || got.Temperature == nil || *got.Temperature != 0 {
		t.Fatalf("model controls = %+v", got)
	}

	// The deterministic scanner remains first and blocks without invoking the
	// bound model service when it already has a conclusive finding.
	h2 := newHarness(t)
	h2.StubModelComplete(modelStub(`{"pii":false,"findings":[]}`))
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("key sk_test_torana_demo_not_a_real_key_123"))))
	if !requestCompleted(res2) {
		t.Fatalf("err=%v", res2.Err)
	}
	assertBlocked(t, h2, "pii_detected", "sk_test_torana_demo_not_a_real_key_123")
	if n := countCommand(h2, "env.model_complete"); n != 0 {
		t.Fatalf("regex finding made %d model calls, want zero", n)
	}
}

// TestBlockReturnsPassAndNoWriteGrant — P2: every block row returns
// The guard changes the request in place and still permits upstream recovery.
func TestReplacementReturnsRequest(t *testing.T) {
	h := newHarness(t)
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("key sk_test_torana_demo_not_a_real_key_123"))))
	if res.Err != nil || res.PassedThrough || res.Request == nil {
		t.Fatalf("a protected result must return replace_request, result=%+v", res)
	}
	if _, ok := protectedMessage(t, h); !ok {
		t.Fatal("expected a persisted replacement")
	}
}

func TestNoUnauthorizedCalls(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{}`)
	h.StubModelComplete(modelStub(`{"pii":false,"findings":[]}`))
	h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("clean"))))
	allowed := map[string]bool{
		"env.plugin_config":            true,
		"env.cache_get":                true,
		"env.cache_set":                true,
		"env.state_get":                true,
		"env.state_get_versioned":      true,
		"env.state_set":                true,
		"env.state_compare_and_delete": true,
		"env.model_complete":           true,
	}
	for _, c := range h.Calls() {
		if !allowed[c.Command] {
			t.Errorf("host call outside the declared permission set: %s", c.Command)
		}
	}
}

// TestSchemaDefaultsMatchRuntimeDefaults pins the user-owned policy surface.
// Provider, model, URL, credentials, and service budgets belong to the
// operator binding declared by plugin.json, not this configuration schema.
func TestSchemaDefaultsMatchRuntimeDefaults(t *testing.T) {
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatalf("read schema.json: %v", err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
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
	for _, hostOwned := range []string{"provider", "model", "url", "credential"} {
		if _, ok := schema.Properties[hostOwned]; ok {
			t.Fatalf("host-owned binding field %q leaked into plugin config", hostOwned)
		}
	}
	rt := parseConfig("")
	if rt.OnError != "block" || rt.MaxScanBytes != 0 {
		t.Fatalf("runtime defaults %+v do not match the schema", rt)
	}
}

// TestConfigResetPinsIsolation — contradictory configs across sequential
// rows, using an UNSCANNABLE structured value so leaked config fails the
// test (a plain email regex hit would block identically under both modes).
func TestConfigResetPinsIsolation(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"on_error":"block"}`)
	h.BeforeRequest(reqWith(toolMsg("c1", "read", unknownArm())))
	if _, ok := protectedMessage(t, h); !ok {
		t.Fatal("row 1 must fail closed on unscannable content")
	}
	h2 := newHarness(t)
	h2.SetConfig(`{"on_error":"allow"}`)
	h2.BeforeRequest(reqWith(toolMsg("c2", "read", unknownArm())))
	if _, ok := protectedMessage(t, h2); ok {
		t.Fatal("row 2 leaked row 1's fail-closed policy")
	}
}

func TestDeterminismOverIdenticalRequests(t *testing.T) {
	h := newHarness(t)
	h.StubModelComplete(modelStub(`{"pii":false,"findings":[]}`))
	r1 := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("clean"))))
	r2 := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("clean"))))
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
// text ARM must keep its newline, so a later finding reports line 2, not
// line 1.
func TestEmptyPartPreservesLineBoundary(t *testing.T) {
	h := newHarness(t)
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm(""), textArm("key sk_test_torana_demo_not_a_real_key_123"))))
	if !requestCompleted(res) {
		t.Fatalf("err=%v", res.Err)
	}
	assertBlocked(t, h, "pii_detected", "sk_test_torana_demo_not_a_real_key_123")
	message, _ := protectedMessage(t, h)
	if !strings.Contains(message, "line 2") {
		t.Fatalf("the empty leading part must push the finding to line 2: %q", message)
	}
}

// TestRegexFindingCapAndMessageBound — a request producing more findings than
// the cap renders exactly the cap, flags overflow, and keeps the message
// bounded and deterministic.
func TestRegexFindingCapAndMessageBound(t *testing.T) {
	content := ""
	for i := 0; i < 100; i++ {
		content += "key sk_test_torana_demo_not_a_real_key_123\n"
	}
	h := newHarness(t)
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm(content))))
	if !requestCompleted(res) {
		t.Fatalf("err=%v", res.Err)
	}
	message, _ := protectedMessage(t, h)
	if !strings.Contains(message, "Additional findings omitted") {
		t.Fatalf("overflow note missing: %q", message)
	}
	if len(message) > 4096 {
		t.Fatalf("replacement message unbounded: %d bytes", len(message))
	}
	// Deterministic ordering: findings render in line order.
	first := strings.Index(message, "line 1")
	second := strings.Index(message, "line 2")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("findings out of order: %q", message)
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
	h.SetConfig(`{}`)
	h.StubModelComplete(modelStub(`{"pii":true,"findings":[` + findings + `]}`))
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("one line"))))
	if !requestCompleted(res) {
		t.Fatalf("err=%v", res.Err)
	}
	message, _ := protectedMessage(t, h)
	if !strings.Contains(message, "Additional findings omitted") {
		t.Fatalf("overflow note missing: %q", message)
	}
	if len(message) > 4096 {
		t.Fatalf("replacement message unbounded: %d bytes", len(message))
	}

	// A one-line input with model lines 2 and 999999: both implausible and
	// omitted (the category renders without a bogus line).
	h2 := newHarness(t)
	h2.SetConfig(`{}`)
	h2.StubModelComplete(modelStub(
		`{"pii":true,"findings":[{"type":"email","line":2},{"type":"email","line":999999}]}`))
	res2 := h2.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("one line"))))
	if !requestCompleted(res2) {
		t.Fatalf("err=%v", res2.Err)
	}
	message2, _ := protectedMessage(t, h2)
	if strings.Contains(message2, "line 2") || strings.Contains(message2, "999999") {
		t.Fatalf("implausible lines must be omitted: %q", message2)
	}
	if !strings.Contains(message2, "email") {
		t.Fatalf("the category must still render: %q", message2)
	}
}

// TestCleanCacheKeyAuthoritativeInputs — every clean-cache input changes the
// key: tool-call ID, resolved name, exact scalar, exact structured bytes, and
// each policy field.
func TestCleanCacheKeyAuthoritativeInputs(t *testing.T) {
	msg := sdk.ToolResults(toolMsg("c1", "read", textArm("scalar")))[0]
	// Deterministic base under the DEFAULT config.
	baseHarness := newHarness(t)
	baseHarness.Run(func() { loadConfig() })
	base := piiCleanCacheKey(msg, "read")
	cases := []struct {
		name string
		key  func() string
	}{
		{"tool call id", func() string {
			return piiCleanCacheKey(sdk.ToolResults(toolMsg("c2", "read", textArm("scalar")))[0], "read")
		}},
		{"resolved name", func() string { return piiCleanCacheKey(msg, "other") }},
		{"exact scalar", func() string {
			return piiCleanCacheKey(sdk.ToolResults(toolMsg("c1", "read", textArm("scalar!")))[0], "read")
		}},
		{"composed text", func() string {
			return piiCleanCacheKey(sdk.ToolResults(toolMsg("c1", "read", textArm("x"), textArm("y")))[0], "read")
		}},
	}
	for _, tc := range cases {
		if tc.key() == base {
			t.Errorf("%s must change the cache key", tc.name)
		}
	}
	// Each user-owned policy field is authoritative: a non-default baseline,
	// and every row mutates exactly one field. The bound model coordinates are
	// host-owned and Edge scopes the private cache to the approved resources.
	baseline := `{"tools":["read"],"on_error":"allow","max_scan_bytes":123}`
	baseHarness.Run(func() { loadConfig() }) // ensure defaults first
	h0 := newHarness(t)
	h0.SetConfig(baseline)
	h0.Run(func() { loadConfig() })
	baseKey := piiCleanCacheKey(msg, "read")
	policyCases := []struct {
		name string
		cfg  string
	}{
		{"tools", `{"tools":["grep"],"on_error":"allow","max_scan_bytes":123}`},
		{"on_error", `{"tools":["read"],"on_error":"block","max_scan_bytes":123}`},
		{"max_scan_bytes", `{"tools":["read"],"on_error":"allow","max_scan_bytes":456}`},
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
	// Regex producer: one finding per line.
	regexContent := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString("key sk_test_torana_demo_not_a_real_key_123\n")
		}
		return b.String()
	}
	for _, n := range []int{19, 20, 21} {
		t.Run("regex/"+itoa(n), func(t *testing.T) {
			h := newHarness(t)
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm(regexContent(n)))))
			if !requestCompleted(res) {
				t.Fatalf("err=%v", res.Err)
			}
			message, _ := protectedMessage(t, h)
			note := strings.Contains(message, "Additional findings omitted")
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
			h.SetConfig(`{}`)
			h.StubModelComplete(modelStub(modelCompletion(n)))
			res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm("one line"))))
			if !requestCompleted(res) {
				t.Fatalf("err=%v", res.Err)
			}
			message, _ := protectedMessage(t, h)
			note := strings.Contains(message, "Additional findings omitted")
			if (n > 20) != note {
				t.Fatalf("n=%d: note present=%v, want %v", n, note, n > 20)
			}
		})
	}
}

// TestEmptyLineNumberingAfterSplitSeq — leading, middle, and trailing empty
// lines keep their positions with the allocation-free iterator.
func TestEmptyLineNumberingAfterSplitSeq(t *testing.T) {
	content := "\n\nkey sk_test_torana_demo_not_a_real_key_123\n\n"
	findings := regexScan(content)
	if len(findings) != 1 {
		t.Fatalf("findings=%d, want 1", len(findings))
	}
	if findings[0].Line != 3 {
		t.Fatalf("line=%d, want 3 (two leading empty lines)", findings[0].Line)
	}
	h := newHarness(t)
	res := h.BeforeRequest(reqWith(toolMsg("c1", "read", textArm(content))))
	if !requestCompleted(res) {
		t.Fatalf("err=%v", res.Err)
	}
	message, _ := protectedMessage(t, h)
	if !strings.Contains(message, "line 3") {
		t.Fatalf("hook-level line numbering wrong: %q", message)
	}
}

// BenchmarkRegexScanLargeSuffix — evidence that the bounded scan does not
// allocate per suffix line: the findings sit in the first cap+1 lines, and a
// large noise suffix follows. Allocation must not scale with the suffix.
func BenchmarkRegexScanLargeSuffix(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 21; i++ {
		sb.WriteString("key sk_test_torana_demo_not_a_real_key_123\n")
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

// BenchmarkRegexScanAgentToolResult mirrors the clean 16 KiB historical tool
// result used by Edge's retained plugin-chain and per-instance memory probes.
func BenchmarkRegexScanAgentToolResult(b *testing.B) {
	content := strings.Repeat("p", 16<<10)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := regexScan(content); len(out) != 0 {
			b.Fatalf("unexpected findings: %+v", out)
		}
	}
}
