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
	} {
		t.Run(name, func(t *testing.T) {
			token, ok := parseBearer(tc.header)
			if ok != tc.ok || token != tc.token {
				t.Fatalf("parseBearer(%q) = (%q, %v), want (%q, %v)", tc.header, token, ok, tc.token, tc.ok)
			}
		})
	}
}

// TestComposeIdentityCollisions — the composed identity is length-framed with
// all three positions ALWAYS represented, so delimiter, NUL, omission, and
// field-swapping collisions are distinct; the verified-token digest is
// domain-separated from the profile identity and never exposes the token.
func TestComposeIdentityCollisions(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"equal profiles collapse", "auth-identity-v2", "auth-identity-v2"},
		{"field swap differs", "auth-identity-v2", "auth-identity-v2"},
		{"omission differs", "auth-identity-v2", "auth-identity-v2"},
		{"NUL content differs", "auth-identity-v2", "auth-identity-v2"},
		{"delimiter content differs", "auth-identity-v2", "auth-identity-v2"},
		{"empty profile is not the host fallback", "auth-verified-key-v2", "auth-identity-v2"},
	}
	profiles := [][]string{
		{"t1", "tm1", "u1"},
		{"t1", "u1", "tm1"},
		{"t1", "tm1", ""},
		{"t1\u0000tm1", "", ""},
		{"t1|tm1", "", ""},
		{"", "", ""},
	}
	keys := make([]string, len(profiles))
	for i, p := range profiles {
		keys[i] = composeIdentity(VerifyResponse{TenantID: p[0], TeamID: p[1], UserID: p[2]}, "tok-1")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ia, ib int
			switch tc.a {
			case "auth-identity-v2":
				ia = 0
			case "auth-verified-key-v2":
				ia = 5
			}
			switch tc.b {
			case "auth-identity-v2":
				ib = 1
			case "auth-verified-key-v2":
				ib = 5
			}
			if keys[ia] == keys[ib] {
				t.Fatalf("collision: %q == %q", keys[ia], keys[ib])
			}
		})
	}
	// The verified-key digest is ContentAddressedCacheKey of the verified
	// token, never the token itself.
	digest := keys[5]
	if digest != sdk.ContentAddressedCacheKey(verifiedKeyNamespace, "tok-1") {
		t.Fatalf("verified digest = %q", digest)
	}
	if strings.Contains(digest, "tok-1") {
		t.Fatal("the verified token leaks into the identity")
	}
	// A different token must not collapse onto the same digest.
	other := composeIdentity(VerifyResponse{}, "tok-2")
	if other == digest {
		t.Fatal("two tokens collapse onto one digest")
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
	} {
		t.Run("accept "+name, func(t *testing.T) {
			if _, err := decodeVerifyResponse([]byte(raw)); err != nil {
				t.Fatalf("must be accepted: %v", err)
			}
		})
	}
	for name, raw := range map[string]string{
		"missing status":     `{}`,
		"unknown member":     `{"status":"ok","extra":1}`,
		"duplicate status":   `{"status":"ok","status":"ok"}`,
		"null status":        `{"status":null}`,
		"message on ok":      `{"status":"ok","message":"hi"}`,
		"tenant on rejected": `{"status":"rejected","tenant_id":"t"}`,
		"team on rejected":   `{"status":"rejected","team_id":"tm"}`,
		"user on rejected":   `{"status":"rejected","user_id":"u"}`,
		"message 1025 bytes": `{"status":"rejected","message":"` + strings.Repeat("x", 1025) + `"}`,
		"trailing JSON":      `{"status":"ok"} {}`,
		"not an object":      `["ok"]`,
		"null envelope":      `null`,
		"status wrong type":  `{"status":7}`,
		"message wrong type": `{"status":"rejected","message":7}`,
		"tenant wrong type":  `{"status":"ok","tenant_id":7}`,
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

// TestDomainRejectionIsTerminal — a value-arm `rejected` produces no verdict
// and passes; nothing else may resolve (there is no lower-precedence
// candidate), so the diagnostic message never surfaces anywhere.
func TestDomainRejectionIsTerminal(t *testing.T) {
	h := newHarness(t)
	stubVerify(t, h, func(token string) (string, bool) {
		return valueReply(`{"status":"rejected","message":"revoked at 2026-08-02"}`), true
	})
	res := h.BeforeRequest(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-torana-revoked"}))
	if !res.PassedThrough || res.Err != nil {
		t.Fatalf("domain rejection must pass without error, err=%v", res.Err)
	}
	if ids := identityCalls(t, h); len(ids) != 0 {
		t.Fatalf("a revoked key produced an identity: %v", ids)
	}
	if logs := h.Logs(); len(logs) != 0 {
		t.Fatalf("the diagnostic message must never be logged: %+v", logs)
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
