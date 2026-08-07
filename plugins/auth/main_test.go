package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

// ==========================================================================
// Unit seams
// ==========================================================================

func TestParseBearerGrammar(t *testing.T) {
	for name, tc := range map[string]struct {
		header string
		token  string
		ok     bool
	}{
		"exact":               {"Bearer sk-torana-abc", "sk-torana-abc", true},
		"lowercase scheme":    {"bearer sk-torana-abc", "sk-torana-abc", true},
		"mixed case scheme":   {"BeArEr sk-torana-abc", "sk-torana-abc", true},
		"two spaces":          {"Bearer  sk-torana-abc", "sk-torana-abc", true},
		"HTAB separator":      {"Bearer\tsk-torana-abc", "sk-torana-abc", true},
		"mixed separators":    {"Bearer \tsk-torana-abc", "sk-torana-abc", true},
		"bytes unchanged":     {"Bearer sk-torana-ABC-123", "sk-torana-ABC-123", true},
		"no scheme":           {"", "", false},
		"no separator":        {"Bearer", "", false},
		"empty credential":    {"Bearer ", "", false},
		"internal whitespace": {"Bearer sk-torana-abc extra", "", false},
		"trailing whitespace": {"Bearer sk-torana-abc ", "", false},
		"trailing HTAB":       {"Bearer sk-torana-abc\t", "", false},
		"not a bearer":        {"Basic abc", "", false},
		"scheme only space":   {"Basic ", "", false},
		"CR inside":           {"Bearer sk-torana-\rabc", "", false},
		"LF inside":           {"Bearer sk-torana-\nabc", "", false},
		"VT inside":           {"Bearer sk-torana-\vabc", "", false},
		"FF inside":           {"Bearer sk-torana-\fabc", "", false},
		"CR as separator":     {"Bearer\rsk-torana-abc", "", false},
		"LF as separator":     {"Bearer\nsk-torana-abc", "", false},
		"VT as separator":     {"Bearer\vsk-torana-abc", "", false},
		"FF as separator":     {"Bearer\fsk-torana-abc", "", false},
		"NUL inside":          {"Bearer sk-torana-\x00abc", "", false},
		"DEL inside":          {"Bearer sk-torana-\x7fabc", "", false},
		"CR after separator":  {"Bearer \rsk-torana-abc", "", false},
		"tab then CR":         {"Bearer \tsk-torana-abc\r", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			token, ok := parseBearer(tc.header)
			if ok != tc.ok || token != tc.token {
				t.Fatalf("parseBearer(%q) = (%q, %v), want (%q, %v)", tc.header, token, ok, tc.token, tc.ok)
			}
		})
	}
}

// TestComposeIdentityCollisions — explicit table of (response, token) inputs
// with an equal expectation per row: identical inputs are equal; each field
// swap differs; omitted-versus-empty position framing differs; delimiter and
// NUL ambiguity pairs differ; the profile identity and the verified-token
// digest are domain-separated; distinct tokens never collapse.
func TestComposeIdentityCollisions(t *testing.T) {
	resp := func(tenant, team, user string) VerifyResponse {
		return VerifyResponse{TenantID: tenant, TeamID: team, UserID: user}
	}
	cases := []struct {
		name  string
		a     VerifyResponse
		aTok  string
		b     VerifyResponse
		bTok  string
		equal bool
	}{
		{"identical input", resp("t", "tm", "u"), "tok-1", resp("t", "tm", "u"), "tok-1", true},
		{"identical empty profile", VerifyResponse{}, "tok-1", VerifyResponse{}, "tok-1", true},
		{"swap tenant/team", resp("t", "tm", "u"), "tok-1", resp("tm", "t", "u"), "tok-1", false},
		{"swap tenant/user", resp("t", "tm", "u"), "tok-1", resp("u", "tm", "t"), "tok-1", false},
		{"swap team/user", resp("t", "tm", "u"), "tok-1", resp("t", "u", "tm"), "tok-1", false},
		{"omitted vs empty user", resp("t", "tm", "u"), "tok-1", resp("t", "tm", ""), "tok-1", false},
		{"omitted vs empty team", resp("t", "tm", ""), "tok-1", resp("t", "", ""), "tok-1", false},
		{"delimiter ambiguity", resp("a|b", "", ""), "tok-1", resp("a", "b", ""), "tok-1", false},
		{"NUL ambiguity", resp("a\x00b", "", ""), "tok-1", resp("a", "b", ""), "tok-1", false},
		{"NUL vs delimiter", resp("a\x00b", "", ""), "tok-1", resp("a|b", "", ""), "tok-1", false},
		{"profile vs token namespace", resp("t", "tm", "u"), "tok-1", VerifyResponse{}, "tok-1", false},
		{"distinct tokens", VerifyResponse{}, "tok-1", VerifyResponse{}, "tok-2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotA := composeIdentity(tc.a, tc.aTok)
			gotB := composeIdentity(tc.b, tc.bTok)
			if (gotA == gotB) != tc.equal {
				t.Fatalf("composeIdentity equal=%v, want %v\n  a: %q\n  b: %q", gotA == gotB, tc.equal, gotA, gotB)
			}
		})
	}
	// The verified-token digest is ContentAddressedCacheKey of the verified
	// token, never the token itself.
	digest := composeIdentity(VerifyResponse{}, "tok-1")
	if digest != sdk.ContentAddressedCacheKey(verifiedKeyNamespace, "tok-1") {
		t.Fatalf("verified digest = %q", digest)
	}
	if strings.Contains(digest, "tok-1") {
		t.Fatal("the verified token leaks into the identity")
	}
}

// TestDecodeVerifyResponseGrammar pins the two-status response grammar:
// status required; message forbidden on ok; profile fields forbidden on
// rejected; message bounded at 1024 decoded bytes; unknown/duplicate members,
// nulls, and trailing JSON rejected. (An unknown but well-typed status value
// is rejected one level up in verifyVirtualKey — see
// TestProtocolViolationsAreHookErrors.)
func TestDecodeVerifyResponseGrammar(t *testing.T) {
	// Accepted shapes.
	for name, raw := range map[string]string{
		"ok with full profile":     `{"status":"ok","tenant_id":"t","team_id":"tm","user_id":"u"}`,
		"ok with empty profile":    `{"status":"ok"}`,
		"ok with empty strings":    `{"status":"ok","tenant_id":"","team_id":"","user_id":""}`,
		"rejected without message": `{"status":"rejected"}`,
		"rejected with empty msg":  `{"status":"rejected","message":""}`,
		"rejected 1024-byte msg":   `{"status":"rejected","message":"` + strings.Repeat("x", 1024) + `"}`,
		"valid surrogate pair":     `{"status":"ok","tenant_id":"\ud83d\ude00"}`,
		"literal escaped slash":    `{"status":"ok","tenant_id":"\\ud800"}`,
		"literal U+FFFD":           `{"status":"ok","tenant_id":"` + "\uFFFD" + `"}`,
	} {
		t.Run("accept "+name, func(t *testing.T) {
			if _, err := decodeVerifyResponse([]byte(raw)); err != nil {
				t.Fatalf("must be accepted: %v", err)
			}
		})
	}
	for name, raw := range map[string]string{
		"missing status":        `{}`,
		"unknown member":        `{"status":"ok","extra":1}`,
		"duplicate status":      `{"status":"ok","status":"ok"}`,
		"null status":           `{"status":null}`,
		"message on ok":         `{"status":"ok","message":"hi"}`,
		"tenant on rejected":    `{"status":"rejected","tenant_id":"t"}`,
		"team on rejected":      `{"status":"rejected","team_id":"tm"}`,
		"user on rejected":      `{"status":"rejected","user_id":"u"}`,
		"message 1025 bytes":    `{"status":"rejected","message":"` + strings.Repeat("x", 1025) + `"}`,
		"trailing JSON":         `{"status":"ok"} {}`,
		"not an object":         `["ok"]`,
		"null envelope":         `null`,
		"status wrong type":     `{"status":7}`,
		"message wrong type":    `{"status":"rejected","message":7}`,
		"tenant wrong type":     `{"status":"ok","tenant_id":7}`,
		"invalid UTF-8 tenant":  "{\"status\":\"ok\",\"tenant_id\":\"t\xff\"}",
		"invalid UTF-8 user":    "{\"status\":\"ok\",\"user_id\":\"u\x00\"}",
		"invalid UTF-8 message": "{\"status\":\"rejected\",\"message\":\"m\xff\"}",
		"lone high surrogate":   `{"status":"ok","tenant_id":"\ud800"}`,
		"lone low surrogate":    `{"status":"ok","tenant_id":"\udc00"}`,
		"distinct lone highs":   `{"status":"ok","tenant_id":"\ud801"}`,
	} {
		t.Run("reject "+name, func(t *testing.T) {
			if _, err := decodeVerifyResponse([]byte(raw)); err == nil {
				t.Fatalf("must be rejected: %s", raw)
			}
		})
	}
}

// ==========================================================================
// Hook-level matrix (sdktest; the plugin registers in init(), so every
// dispatch exercises the real hook)
// ==========================================================================

func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	return sdktest.New(t)
}

func reqWithHeaders(headers map[string]string) *pbv2.ChatRequest {
	meta := map[string]any{"_request_headers": map[string]any{}}
	hm := meta["_request_headers"].(map[string]any)
	for k, v := range headers {
		hm[k] = v
	}
	raw, _ := json.Marshal(meta)
	return &pbv2.ChatRequest{ToranaMetaJson: raw}
}

// stubVerify installs a verify_virtual_key backend. respond receives the
// decoded {"key":...} token and returns a framed response (or ok=false to
// fail the call).
func stubVerify(t *testing.T, h *sdktest.Harness, respond func(token string) (string, bool)) {
	t.Helper()
	h.StubHostCall("verify_virtual_key", func(args string) (string, error) {
		var req struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(args), &req); err != nil {
			t.Fatalf("verify request is not {\"key\":...}: %v (%q)", err, args)
		}
		if res, ok := respond(req.Key); ok {
			return res, nil
		}
		return "", errors.New("unexpected verify call")
	})
}

func valueReply(body string) string {
	return sdktest.HostResultValue([]byte(body))
}

func okReply(profile string) string {
	return valueReply(`{"status":"ok"` + profile + `}`)
}

// identityCalls returns the identities the plugin sent through env.set_identity.
func identityCalls(t *testing.T, h *sdktest.Harness) []string {
	t.Helper()
	var out []string
	for _, c := range h.Calls() {
		if c.Command != "env.set_identity" {
			continue
		}
		var a pbv2.SetIdentityArgs
		if err := proto.Unmarshal([]byte(c.Args), &a); err != nil {
			t.Fatalf("set_identity args: %v", err)
		}
		out = append(out, a.Identity)
	}
	return out
}

// verifyKeys returns the tokens the plugin submitted, in order.
func verifyKeys(t *testing.T, h *sdktest.Harness) []string {
	t.Helper()
	var out []string
	for _, c := range h.Calls() {
		if c.Command != "verify_virtual_key" {
			continue
		}
		var req struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(c.Args), &req); err != nil {
			t.Fatalf("verify_virtual_key args: %v", err)
		}
		out = append(out, req.Key)
	}
	return out
}

func wantIdentity(want string) func(string) (string, bool) {
	return func(token string) (string, bool) {
		return okReply(`,"tenant_id":"t","team_id":"tm","user_id":"u"`), true
	}
}

// TestNoCredentialsNoVerdict — no headers, or headers that are not
// resolvable credentials, produce no verify call and no set_identity verdict.
func TestNoCredentialsNoVerdict(t *testing.T) {
	for name, headers := range map[string]map[string]string{
		"no headers at all":      {},
		"no request headers key": nil,
		"provider key only":      {"Authorization": "Bearer sk-proj-123"},
		"JWT shaped bearer":      {"Authorization": "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.sig"},
		"basic auth":             {"Authorization": "Basic dXNlcjpwYXNz"},
		"trusted headers":        {"X-Torana-User": "alice", "X-Torana-Team": "team", "X-Torana-Tenant": "tenant"},
		"trusted plus JWT":       {"Authorization": "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.sig", "X-Torana-User": "alice"},
		"trusted plus key":       {"X-Torana-User": "alice", "X-Api-Key": "sk-proj-zzz"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			stubVerify(t, h, func(string) (string, bool) {
				t.Fatal("verify must not be called")
				return "", false
			})
			res := h.BeforeRequest(reqWithHeaders(headers))
			if !res.PassedThrough || res.Err != nil {
				t.Fatalf("expected pass-through, err=%v", res.Err)
			}
			if ids := identityCalls(t, h); len(ids) != 0 {
				t.Fatalf("unverified input produced an identity: %v", ids)
			}
		})
	}
}

// TestVirtualKeyInAuthorization — a verified virtual key in Authorization
// yields the exact composed identity with all three positions represented.
func TestVirtualKeyInAuthorization(t *testing.T) {
	h := newHarness(t)
	stubVerify(t, h, func(token string) (string, bool) {
		if token != "sk-torana-abc" {
			t.Fatalf("verify called with %q, want the exact Authorization token bytes", token)
		}
		return okReply(`,"tenant_id":"t","team_id":"tm","user_id":"u"`), true
	})
	res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-abc"}))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	ids := identityCalls(t, h)
	if len(ids) != 1 {
		t.Fatalf("want exactly one verdict, got %v", ids)
	}
	want := sdk.ContentAddressedCacheKey(identityNamespace, "t", "tm", "u")
	if ids[0] != want {
		t.Fatalf("identity = %q, want %q", ids[0], want)
	}
}

// TestVirtualKeyInXApiKey — the same flow through X-Api-Key.
func TestVirtualKeyInXApiKey(t *testing.T) {
	h := newHarness(t)
	stubVerify(t, h, func(token string) (string, bool) {
		if token != "sk-torana-xyz" {
			t.Fatalf("verify called with %q", token)
		}
		return okReply(`,"tenant_id":"t","team_id":"tm","user_id":"u"`), true
	})
	res := h.BeforeRequest(reqWithHeaders(map[string]string{"X-Api-Key": "sk-torana-xyz"}))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if ids := identityCalls(t, h); len(ids) != 1 {
		t.Fatalf("want exactly one verdict, got %v", ids)
	}
}

// TestVerifiedEmptyProfileUsesTokenDigest — a verified key with an empty
// profile composes the verified-token digest: per-key rate limiting survives
// and the token never appears in the identity.
func TestVerifiedEmptyProfileUsesTokenDigest(t *testing.T) {
	h := newHarness(t)
	stubVerify(t, h, func(token string) (string, bool) {
		return okReply(""), true
	})
	res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-key1"}))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	ids := identityCalls(t, h)
	if len(ids) != 1 {
		t.Fatalf("want exactly one verdict, got %v", ids)
	}
	want := sdk.ContentAddressedCacheKey(verifiedKeyNamespace, "sk-torana-key1")
	if ids[0] != want {
		t.Fatalf("identity = %q, want %q", ids[0], want)
	}
	if strings.Contains(ids[0], "sk-torana-key1") {
		t.Fatal("the verified token leaks into the identity")
	}
}

// TestDomainRejectionIsTerminal — a value-arm `rejected` produces one
// attributed, value-free 401 block. It must never fall back to the operator's
// provider credential, and the verifier diagnostic never reaches the caller.
func TestDomainRejectionIsTerminal(t *testing.T) {
	h := newHarness(t)
	stubVerify(t, h, func(token string) (string, bool) {
		return valueReply(`{"status":"rejected","message":"revoked at 2026-08-02"}`), true
	})
	res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-revoked"}))
	if !res.PassedThrough || res.Err != nil {
		t.Fatalf("block verdict is a side effect followed by pass, err=%v", res.Err)
	}
	if ids := identityCalls(t, h); len(ids) != 0 {
		t.Fatalf("a revoked key produced an identity: %v", ids)
	}
	if logs := h.Logs(); len(logs) != 0 {
		t.Fatalf("the diagnostic message must never be logged: %+v", logs)
	}
	if got := h.BlockCalls(); len(got) != 1 {
		t.Fatalf("block calls = %d, want 1", len(got))
	} else {
		args := sdktest.DecodeBlockArgs(t, got[0].Args)
		if args.Status != 401 || args.Code != "virtual_key_rejected" ||
			args.Message != "The Torana virtual key was rejected." {
			t.Fatalf("block args = %+v", args)
		}
		if strings.Contains(args.Message, "revoked") || strings.Contains(args.Message, "2026") {
			t.Fatalf("verifier diagnostic leaked into block: %q", args.Message)
		}
	}
}

// TestProtocolViolationsAreHookErrors — missing/unknown status, message on ok,
// profile on rejected, unknown/duplicate members, over-bound message, and
// malformed frames are hook errors, never rejections and never advisory
// absence.
func TestProtocolViolationsAreHookErrors(t *testing.T) {
	for name, body := range map[string]string{
		"missing status":     `{}`,
		"unknown status":     `{"status":"maybe"}`,
		"message on ok":      `{"status":"ok","message":"hi"}`,
		"tenant on rejected": `{"status":"rejected","tenant_id":"t"}`,
		"unknown member":     `{"status":"ok","extra":1}`,
		"duplicate status":   `{"status":"ok","status":"ok"}`,
		"trailing JSON":      `{"status":"ok"} {}`,
		"message 1025 bytes": `{"status":"rejected","message":"` + strings.Repeat("x", 1025) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			stubVerify(t, h, func(string) (string, bool) {
				return valueReply(body), true
			})
			res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-abc"}))
			if res.Err == nil {
				t.Fatal("protocol violation must be a hook error")
			}
			if strings.Contains(res.Err.Error(), "revoked at") || strings.Contains(res.Err.Error(), strings.Repeat("x", 8)) {
				t.Fatalf("the diagnostic message leaked into the error: %v", res.Err)
			}
			if ids := identityCalls(t, h); len(ids) != 0 {
				t.Fatalf("a protocol violation produced an identity: %v", ids)
			}
		})
	}
}

// TestMalformedFrameIsHookError — a reply that is not a framed HostCallResult
// is a transport/protocol failure, not a domain answer.
func TestMalformedFrameIsHookError(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("verify_virtual_key", func(args string) (string, error) {
		return "not a framed reply", nil
	})
	res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-abc"}))
	if res.Err == nil {
		t.Fatal("a malformed frame must be a hook error")
	}
}

// TestHostErrorTable — NOT_CONFIGURED/UNAVAILABLE are advisory (no verdict,
// pass); every other code is a contract failure (hook error).
func TestHostErrorTable(t *testing.T) {
	advisory := map[string]pbv2.ErrorCode{
		"NOT_CONFIGURED": pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
		"UNAVAILABLE":    pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE,
	}
	for name, code := range advisory {
		t.Run(name+" advisory", func(t *testing.T) {
			h := newHarness(t)
			h.StubHostCall("verify_virtual_key", func(args string) (string, error) {
				return sdktest.HostResultError(code, "verifier unwired"), nil
			})
			res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-abc"}))
			if !res.PassedThrough || res.Err != nil {
				t.Fatalf("advisory refusal must pass without a verdict, err=%v", res.Err)
			}
			if ids := identityCalls(t, h); len(ids) != 0 {
				t.Fatalf("advisory refusal produced an identity: %v", ids)
			}
		})
	}
	contract := map[string]pbv2.ErrorCode{
		"NOT_FOUND":         pbv2.ErrorCode_ERROR_CODE_NOT_FOUND,
		"PERMISSION_DENIED": pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
		"INVALID_ARGUMENT":  pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
		"INTERNAL":          pbv2.ErrorCode_ERROR_CODE_INTERNAL,
	}
	for name, code := range contract {
		t.Run(name+" contract", func(t *testing.T) {
			h := newHarness(t)
			h.StubHostCall("verify_virtual_key", func(args string) (string, error) {
				return sdktest.HostResultError(code, "boom"), nil
			})
			res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-abc"}))
			if res.Err == nil {
				t.Fatal("a contract-class refusal must be a hook error")
			}
			if ids := identityCalls(t, h); len(ids) != 0 {
				t.Fatalf("a contract refusal produced an identity: %v", ids)
			}
		})
	}
}

// TestPrecedenceAuthorizationWins — a virtual key in Authorization beats one
// in X-Api-Key, and verification happens EXACTLY ONCE (same-token duplication
// included).
func TestPrecedenceAuthorizationWins(t *testing.T) {
	for name, headers := range map[string]map[string]string{
		"conflicting keys": {
			"Authorization": "Bearer sk-torana-auth",
			"X-Api-Key":     "sk-torana-apikey",
		},
		"same token duplicated": {
			"Authorization": "Bearer sk-torana-same",
			"X-Api-Key":     "sk-torana-same",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			stubVerify(t, h, wantIdentity(""))
			res := h.BeforeRequest(reqWithHeaders(headers))
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			keys := verifyKeys(t, h)
			if len(keys) != 1 {
				t.Fatalf("verification must happen exactly once, got %v", keys)
			}
			if keys[0] != "sk-torana-auth" && keys[0] != "sk-torana-same" {
				t.Fatalf("the Authorization key must win, got %q", keys[0])
			}
		})
	}
}

// TestJWTDoesNotMaskXApiKey — an unverifiable JWT in Authorization is not a
// credential; a virtual key in X-Api-Key still resolves.
func TestJWTDoesNotMaskXApiKey(t *testing.T) {
	h := newHarness(t)
	stubVerify(t, h, func(token string) (string, bool) {
		if token != "sk-torana-apikey" {
			t.Fatalf("verify called with %q, want the X-Api-Key token", token)
		}
		return okReply(`,"tenant_id":"t","team_id":"tm","user_id":"u"`), true
	})
	res := h.BeforeRequest(reqWithHeaders(map[string]string{
		"Authorization": "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.sig",
		"X-Api-Key":     "sk-torana-apikey",
	}))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if ids := identityCalls(t, h); len(ids) != 1 {
		t.Fatalf("want exactly one verdict, got %v", ids)
	}
}

// TestDeterminism — identical headers produce identical verdict arguments.
func TestDeterminism(t *testing.T) {
	run := func() string {
		h := newHarness(t)
		stubVerify(t, h, func(token string) (string, bool) {
			return okReply(`,"tenant_id":"t","team_id":"tm","user_id":"u"`), true
		})
		res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-abc"}))
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		ids := identityCalls(t, h)
		if len(ids) != 1 {
			t.Fatal("expected one verdict")
		}
		return ids[0]
	}
	first := run()
	for i := 0; i < 10; i++ {
		if got := run(); got != first {
			t.Fatalf("identity differs on iteration %d: %q vs %q", i, got, first)
		}
	}
}

// TestNoUnauthorizedCalls — the plugin's only host traffic is the verify
// extension call and the set_identity verdict.
func TestNoUnauthorizedCalls(t *testing.T) {
	h := newHarness(t)
	stubVerify(t, h, func(token string) (string, bool) {
		return okReply(`,"tenant_id":"t","team_id":"tm","user_id":"u"`), true
	})
	h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-abc"}))
	for _, c := range h.Calls() {
		switch c.Command {
		case "verify_virtual_key", "env.set_identity":
		default:
			t.Errorf("unexpected host call: %s", c.Command)
		}
	}
}

// TestHostErrorMessageNeverLeaks — a private verifier can embed the token,
// an upstream reason, or tenant data in a contract HostError's message; Edge
// captures hook errors. The hook error must be classified by code only, and
// the message must appear in no error, log, verdict, or metric (F7).
func TestHostErrorMessageNeverLeaks(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("verify_virtual_key", func(args string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_INTERNAL,
			"upstream denied token=sk-torana-secret123 for tenant acme"), nil
	})
	res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-secret123"}))
	if res.Err == nil {
		t.Fatal("a contract HostError must be a hook error")
	}
	if strings.Contains(res.Err.Error(), "sk-torana-secret123") ||
		strings.Contains(res.Err.Error(), "acme") ||
		strings.Contains(res.Err.Error(), "upstream denied") {
		t.Fatalf("the host message leaked into the hook error: %v", res.Err)
	}
	if ids := identityCalls(t, h); len(ids) != 0 {
		t.Fatalf("a contract refusal produced a verdict: %v", ids)
	}
	if logs := h.Logs(); len(logs) != 0 {
		t.Fatalf("the host message leaked into logs: %+v", logs)
	}
	if metrics := h.Metrics(); len(metrics) != 0 {
		t.Fatalf("the host message leaked into metrics: %+v", metrics)
	}
}

// TestFailureModeScope — the unit proof is that contract/protocol failures
// RETURN a hook error; host application of failure_mode: pass is the generic
// Edge invariant covered by existing edge fixtures, not asserted here.
func TestFailureModeScope(t *testing.T) {
	h := newHarness(t)
	h.DenyPermission("verify_virtual_key")
	res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-abc"}))
	if res.Err == nil {
		t.Fatal("a permission-denied verify call must surface as a hook error")
	}
}

// TestMalformedToranaMetaIsHookError — the host's own metadata is a protocol
// surface; garbage in it is a defect, not a silent pass.
func TestMalformedToranaMetaIsHookError(t *testing.T) {
	h := newHarness(t)
	res := h.BeforeRequest(&pbv2.ChatRequest{ToranaMetaJson: []byte(`{not json`)})
	if res.Err == nil {
		t.Fatal("malformed ToranaMetaJson must be a hook error")
	}
}

// TestRequestIsNeverMutated — auth is a side-channel plugin: PassRequest with
// no request mutation, ever.
func TestRequestIsNeverMutated(t *testing.T) {
	h := newHarness(t)
	stubVerify(t, h, func(token string) (string, bool) {
		return okReply(`,"tenant_id":"t","team_id":"tm","user_id":"u"`), true
	})
	req := reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-abc"})
	res := h.BeforeRequest(req)
	if !res.PassedThrough || res.Err != nil {
		t.Fatalf("expected pure pass-through, err=%v", res.Err)
	}
	if res.Request != nil {
		t.Fatal("the request must never be replaced")
	}
}

// ==========================================================================
// Round-2 pins (lossless JSON / token boundaries)
// ==========================================================================

// TestValidVirtualKeyGrammar — the v2 virtual-key token is ASCII by contract:
// "sk-torana-" followed by at least one printable ASCII byte (0x21..0x7e).
// Controls, whitespace, DEL, non-ASCII, and an empty suffix are not tokens —
// so JSON transport of the token is lossless. Both header sources share this
// validator.
func TestValidVirtualKeyGrammar(t *testing.T) {
	cases := []struct {
		name  string
		token string
		ok    bool
	}{
		{"minimum suffix", "sk-torana-a", true},
		{"space boundary 0x20", "sk-torana- ", false},
		{"bang boundary 0x21", "sk-torana-!", true},
		{"tilde boundary 0x7e", "sk-torana-~", true},
		{"DEL boundary 0x7f", "sk-torana-\x7f", false},
		{"non-ASCII 0x80", "sk-torana-\x80", false},
		{"invalid UTF-8 0xff", "sk-torana-\xff", false},
		{"unicode char", "sk-torana-é", false},
		{"empty suffix", "sk-torana-", false},
		{"no prefix", "sk-proj-abc", false},
		{"control inside", "sk-torana-a\x01b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validVirtualKey(tc.token); got != tc.ok {
				t.Fatalf("validVirtualKey(%q) = %v, want %v", tc.token, got, tc.ok)
			}
		})
	}
}

// TestNonASCIIVirtualKeysNeverVerify — invalid-token bytes from either header
// source produce no verify call and no verdict.
func TestNonASCIIVirtualKeysNeverVerify(t *testing.T) {
	for name, headers := range map[string]map[string]string{
		"Authorization 0xff": {"Authorization": "Bearer sk-torana-\xff"},
		"X-Api-Key 0xff":     {"X-Api-Key": "sk-torana-\xff"},
		"Authorization 0x80": {"Authorization": "Bearer sk-torana-\x80"},
		"X-Api-Key unicode":  {"X-Api-Key": "sk-torana-é"},
		"empty suffix":       {"X-Api-Key": "sk-torana-"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			stubVerify(t, h, func(string) (string, bool) {
				t.Fatal("verify must not be called")
				return "", false
			})
			res := h.BeforeRequest(reqWithHeaders(headers))
			if !res.PassedThrough || res.Err != nil {
				t.Fatalf("expected pass-through, err=%v", res.Err)
			}
			if ids := identityCalls(t, h); len(ids) != 0 {
				t.Fatalf("an invalid token produced a verdict: %v", ids)
			}
			if keys := verifyKeys(t, h); len(keys) != 0 {
				t.Fatalf("an invalid token reached the verifier: %v", keys)
			}
		})
	}
}

// TestSameTokenThroughBothHeaders — the same accepted token arriving through
// both headers is still verified exactly once (Authorization wins).
func TestSameTokenThroughBothHeaders(t *testing.T) {
	h := newHarness(t)
	stubVerify(t, h, func(token string) (string, bool) {
		if token != "sk-torana-same~!x" {
			t.Fatalf("verify called with %q", token)
		}
		return okReply(`,"tenant_id":"t","team_id":"tm","user_id":"u"`), true
	})
	res := h.BeforeRequest(reqWithHeaders(map[string]string{
		"Authorization": "Bearer sk-torana-same~!x",
		"X-Api-Key":     "sk-torana-same~!x",
	}))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if keys := verifyKeys(t, h); len(keys) != 1 {
		t.Fatalf("verification must happen exactly once, got %v", keys)
	}
}
