package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// ==========================================================================
// Identity resolution
// ==========================================================================
//
// This plugin is the only identity source. Identity flows through the
// attributed env.set_identity verdict, which changes the actual rate-limit
// key; ToranaMeta is host-owned. Consequences, each pinned by a matrix row:
//
//   - Caller-controlled X-Torana-* headers are NOT identity candidates. Edge
//     forwards them without a trusted-proxy boundary, so a caller could rotate
//     them and evade identity-based limits. Unverified headers never produce a
//     verdict.
//   - JWT-shaped and provider-key bearers are secrets for the upstream, not
//     identities this edition can verify; they never resolve either.
//   - The ONLY resolvable credential is a torana-issued virtual key, verified
//     through the host's verify_virtual_key feature. A verified key with a
//     profile composes a collision-proof identity; a verified key with an
//     EMPTY profile composes a digest of the verified token so per-key rate
//     limiting survives without inventing profile fields.
//
// The request hook returns PassRequest ALWAYS: identity is a side channel
// (the verdict), never a request mutation.

// virtualKeyPrefix marks a Torana-issued key, which the host can verify.
const virtualKeyPrefix = "sk-torana-"

// maxVerifyMessageBytes bounds the optional diagnostic message on a `rejected`
// response at 1024 decoded UTF-8 bytes. Above the bound is a protocol error;
// the message is NEVER placed in a verdict, an error returned to the caller,
// an identity, a metric, or a log.
const maxVerifyMessageBytes = 1024

// identity namespaces for ContentAddressedCacheKey. The key is length-framed
// by the SDK, so delimiter, NUL, omission, and field-swapping collisions are
// distinct by construction; the namespace keeps the two composition kinds
// domain-separated.
const (
	identityNamespace    = "auth-identity-v2"
	verifiedKeyNamespace = "auth-verified-key-v2"
)

// VerifyResponse is the strictly validated response to verify_virtual_key.
// The wire grammar is:
//
//   - exactly two case-sensitive statuses: "ok" and "rejected";
//   - status is REQUIRED; unknown statuses, unknown members, duplicate
//     members, nulls, and trailing JSON are protocol errors;
//   - `rejected` FORBIDS tenant_id/team_id/user_id and allows an optional
//     bounded `message` as diagnostic data only (never reflected to the
//     caller); the rejection is a value-arm result and is terminal;
//   - `ok` uses the profile identity when ANY profile field is non-empty and
//     the verified-token digest only when all three are empty; `message` is
//     FORBIDDEN on `ok`.
type VerifyResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	TenantID string `json:"tenant_id,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
}

// verifyOutcome says what happened to a credential. Advisory host failure is
// distinct from a domain answer: the verifier being unwired (NOT_CONFIGURED)
// or unavailable means NO identity is possible (this plugin is the only
// source), while a domain `rejected` is a statement ABOUT THE REQUEST.
type verifyOutcome int

const (
	// verifyNoIdentity: advisory (NOT_CONFIGURED/UNAVAILABLE). No verdict.
	verifyNoIdentity verifyOutcome = iota
	// verifyOK: the host answered `ok`; the identity is established.
	verifyOK
	// verifyRejected: the host answered `rejected` — terminal, nothing else
	// may resolve (there is no lower-precedence candidate anyway).
	verifyRejected
)

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		headers, err := requestHeaders(req)
		if err != nil {
			return sdk.RequestResult{}, err
		}
		token, have := virtualKeyCandidate(headers)
		if !have {
			// No resolvable credential: no identity. The host falls back to
			// its own Authorization-header handling.
			return sdk.PassRequest(), nil
		}
		id, outcome, err := verifyVirtualKey(token)
		if err != nil {
			// Transport/protocol failure or a contract-class host refusal:
			// the identity cannot be established, and this plugin is the only
			// source — surface it so failure_mode applies. This REFERENCE plugin
			// deliberately ships failure_mode=pass because it is not access
			// control. A production auth plugin must choose block semantics here.
			return sdk.RequestResult{}, err
		}
		switch outcome {
		case verifyOK:
			sdk.SetIdentity(id)
			return sdk.PassRequest(), nil
		case verifyRejected:
			// A domain rejection is an authoritative answer about the presented
			// credential. Falling back to the operator's provider credential would
			// turn an explicitly revoked/invalid Torana key into authenticated
			// access. Keep the verifier's optional diagnostic private and return a
			// stable, value-free denial.
			sdk.BlockRequest(401, "virtual_key_rejected", "The Torana virtual key was rejected.")
			return sdk.PassRequest(), nil
		default: // verifyNoIdentity: advisory unwired/unavailable verifier.
			return sdk.PassRequest(), nil
		}
	})
}

// requestHeaders extracts the allowlisted request headers the host injects
// into ToranaMeta under _request_headers (only when the env.request_headers
// grant is held). Malformed host-provided metadata — including textually
// invalid JSON — is a protocol defect, and the decode is lossless
// (decodeJSONObject) so no header byte is normalized before the token
// grammar sees it.
func requestHeaders(req *pbv2.ChatRequest) (map[string]any, error) {
	if len(req.ToranaMetaJson) == 0 {
		return nil, nil
	}
	meta, err := decodeJSONObject(req.ToranaMetaJson)
	if err != nil {
		return nil, fmt.Errorf("auth: malformed ToranaMetaJson: %w", err)
	}
	if meta == nil {
		return nil, nil
	}
	headers, _ := meta["_request_headers"].(map[string]any)
	return headers, nil
}

// validVirtualKey is the ONE virtual-key validator shared by both header
// sources. The v2 token grammar is explicitly ASCII: the prefix "sk-torana-"
// followed by at least one printable ASCII byte (0x21..0x7e), with no
// controls, whitespace, DEL, non-ASCII, or empty suffix. ASCII is the
// normative token grammar for a simple, interoperable, byte-stable
// credential contract: valid non-ASCII Unicode is preserved by JSON
// encoding, but INVALID UTF-8 is not (encoding/json substitutes U+FFFD), and
// the host injects headers through JSON metadata before this plugin sees
// them. A printable-ASCII contract keeps the token byte-stable end to end
// while staying flexible about the private verifier's punctuation/alphabet.
func validVirtualKey(token string) bool {
	if !strings.HasPrefix(token, virtualKeyPrefix) {
		return false
	}
	rest := token[len(virtualKeyPrefix):]
	if rest == "" {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] < 0x21 || rest[i] > 0x7e {
			return false
		}
	}
	return true
}

// virtualKeyCandidate picks the single resolvable credential:
//
//   - an Authorization bearer that parses under the exact Bearer grammar and
//     carries a valid virtual key WINS over X-Api-Key (deterministic;
//     verification happens exactly once, and same-token duplication still
//     verifies once);
//   - otherwise an X-Api-Key virtual key (same validator);
//   - otherwise nothing. Unverified JWT-shaped bearers, provider keys, and
//     X-Torana-* headers are NOT candidates.
func virtualKeyCandidate(headers map[string]any) (string, bool) {
	if auth, ok := headers["Authorization"].(string); ok {
		if token, ok := parseBearer(auth); ok && validVirtualKey(token) {
			return token, true
		}
	}
	if apiKey, ok := headers["X-Api-Key"].(string); ok && validVirtualKey(apiKey) {
		return apiKey, true
	}
	return "", false
}

// parseBearer implements the exact Bearer grammar: a case-insensitive scheme,
// one or more ASCII SP/HTAB separators, then one non-empty credential with no
// internal or trailing ASCII whitespace. The credential bytes pass through
// UNCHANGED. Malformed syntax is NOT a credential.
func parseBearer(authHeader string) (string, bool) {
	i := 0
	for i < len(authHeader) && !isBearerSeparator(authHeader[i]) {
		i++
	}
	if i == 0 {
		return "", false
	}
	if !strings.EqualFold(authHeader[:i], "Bearer") {
		return "", false
	}
	j := i
	for j < len(authHeader) && isBearerSeparator(authHeader[j]) {
		j++
	}
	if j == i {
		return "", false // scheme without a separator is not a credential
	}
	cred := authHeader[j:]
	if cred == "" {
		return "", false
	}
	for k := 0; k < len(cred); k++ {
		if isForbiddenCredentialByte(cred[k]) {
			return "", false // internal or trailing whitespace / control
		}
	}
	return cred, true
}

// isBearerSeparator is the ONLY whitespace the Bearer grammar permits between
// the scheme and the credential: ASCII SP (0x20) and HTAB (0x09). CR, LF, and
// any other control are NOT separators — a header like "Bearer\rsk-torana-x"
// fails the scheme match outright (review round-1 F6).
func isBearerSeparator(b byte) bool {
	return b == ' ' || b == '\t'
}

// isForbiddenCredentialByte reports any ASCII control (C0: 0x00-0x1F, DEL:
// 0x7F) or SP. The contract permits one non-empty credential with no internal
// or trailing ASCII whitespace; every one of these bytes is rejected.
func isForbiddenCredentialByte(b byte) bool {
	return b <= 0x20 || b == 0x7f
}

// verifyVirtualKey asks the host to validate a Torana-issued key. The request
// is the typed JSON envelope {"key":"<token>"}; the response is strictly
// validated. Requires the env.host_call.verify_virtual_key grant.
func verifyVirtualKey(token string) (string, verifyOutcome, error) {
	payload, err := json.Marshal(map[string]string{"key": token})
	if err != nil {
		return "", verifyNoIdentity, fmt.Errorf("auth: cannot encode verify request: %w", err)
	}
	res, herr, err := sdk.HostCallExtension("verify_virtual_key", payload)
	if err != nil {
		return "", verifyNoIdentity, fmt.Errorf("auth: verify_virtual_key call failed: %w", err)
	}
	if herr != nil {
		switch herr.Code {
		case pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE:
			// The verifier is unwired or temporarily unavailable: advisory.
			// No identity is possible — this plugin is the only source.
			return "", verifyNoIdentity, nil
		default:
			// NOT_FOUND, PERMISSION_DENIED, INVALID_ARGUMENT, INTERNAL: a
			// contract/protocol failure, not a domain answer. Revoked or
			// unknown credentials are expressed by the normal domain
			// response, never by HostError. The host message is NEVER
			// interpolated: a private verifier could embed the token or
			// tenant data in it, and Edge captures hook errors (review
			// round-1 F7). The error is classified by code only.
			return "", verifyNoIdentity, fmt.Errorf("auth: verify_virtual_key refused (code=%s)", herr.Code)
		}
	}

	resp, err := decodeVerifyResponse(res)
	if err != nil {
		return "", verifyNoIdentity, fmt.Errorf("auth: invalid verify_virtual_key response: %w", err)
	}

	switch resp.Status {
	case "ok":
		return composeIdentity(resp, token), verifyOK, nil
	case "rejected":
		// Value-arm terminal refusal. The diagnostic message stays in this
		// function: never in a verdict, an error to the caller, an identity,
		// a metric, or a log.
		return "", verifyRejected, nil
	default:
		return "", verifyNoIdentity, fmt.Errorf("auth: unknown verify status %q", resp.Status)
	}
}

// decodeVerifyResponse strictly validates the response envelope:
//
//   - exactly the known members; duplicate keys, nulls, unknown members, and
//     trailing JSON are rejected;
//   - status required, exactly "ok" or "rejected";
//   - `ok`: message FORBIDDEN; tenant/team/user optional;
//   - `rejected`: tenant/team/user FORBIDDEN; message optional, bounded at
//     maxVerifyMessageBytes decoded UTF-8 bytes.
func decodeVerifyResponse(res []byte) (VerifyResponse, error) {
	raw, err := decodeObjectStrict(res, map[string]bool{
		"status": true, "message": true,
		"tenant_id": true, "team_id": true, "user_id": true,
	})
	if err != nil {
		return VerifyResponse{}, err
	}
	statusRaw, ok := raw["status"]
	if !ok {
		return VerifyResponse{}, fmt.Errorf("missing required member %q", "status")
	}
	var status string
	if err := json.Unmarshal(statusRaw, &status); err != nil {
		return VerifyResponse{}, fmt.Errorf("status must be a string")
	}

	var resp VerifyResponse
	if rawMessage, present := raw["message"]; present {
		if status == "ok" {
			return VerifyResponse{}, fmt.Errorf("message is forbidden on status %q", status)
		}
		var msg string
		if err := json.Unmarshal(rawMessage, &msg); err != nil {
			return VerifyResponse{}, fmt.Errorf("message must be a string")
		}
		if len(msg) > maxVerifyMessageBytes {
			return VerifyResponse{}, fmt.Errorf("message exceeds %d bytes", maxVerifyMessageBytes)
		}
		resp.Message = msg
	}
	for member, target := range map[string]*string{
		"tenant_id": &resp.TenantID, "team_id": &resp.TeamID, "user_id": &resp.UserID,
	} {
		if rawMember, present := raw[member]; present {
			if status == "rejected" {
				return VerifyResponse{}, fmt.Errorf("%s is forbidden on status %q", member, status)
			}
			if err := json.Unmarshal(rawMember, target); err != nil {
				return VerifyResponse{}, fmt.Errorf("%s must be a string", member)
			}
		}
	}
	resp.Status = status
	return resp, nil
}

// composeIdentity builds the collision-proof identity for a verified key.
// All three profile positions are ALWAYS represented (empty strings included)
// and the key is length-framed, so delimiter, NUL, omission, and field-swap
// collisions are distinct. A verified key with an empty profile composes a
// domain-separated digest of the VERIFIED TOKEN — never the token itself, and
// never the empty host fallback — so per-key rate limiting survives.
func composeIdentity(resp VerifyResponse, token string) string {
	if resp.TenantID != "" || resp.TeamID != "" || resp.UserID != "" {
		return sdk.ContentAddressedCacheKey(identityNamespace, resp.TenantID, resp.TeamID, resp.UserID)
	}
	return sdk.ContentAddressedCacheKey(verifiedKeyNamespace, token)
}
