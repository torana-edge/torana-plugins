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
// v2 identity resolution (approved batch-5 contract)
// ==========================================================================
//
// This plugin is the ONLY identity source in v2. The v1 writes of
// tenant_id/team_id/user_id into ToranaMeta are GONE — those fields are
// host-owned, and identity now flows through the attributed env.set_identity
// verdict, which changes the ACTUAL rate-limit key. Consequences, each pinned
// by a matrix row:
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

// VerifyResponse is the strictly validated v2 response to verify_virtual_key.
// The wire grammar (section 8A of the checkpoint):
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
			// source — surface it so failure_mode applies.
			return sdk.RequestResult{}, err
		}
		switch outcome {
		case verifyOK:
			sdk.SetIdentity(id)
			return sdk.PassRequest(), nil
		default:
			// verifyRejected (terminal domain refusal) and verifyNoIdentity
			// (advisory) both pass without a verdict.
			return sdk.PassRequest(), nil
		}
	})
}

// requestHeaders extracts the allowlisted request headers the host injects
// into ToranaMeta under _request_headers (only when the env.request_headers
// grant is held). Malformed host-provided metadata is a protocol defect.
func requestHeaders(req *pbv2.ChatRequest) (map[string]any, error) {
	if len(req.ToranaMetaJson) == 0 {
		return nil, nil
	}
	var meta map[string]any
	if err := json.Unmarshal(req.ToranaMetaJson, &meta); err != nil {
		return nil, fmt.Errorf("auth: malformed ToranaMetaJson: %w", err)
	}
	headers, _ := meta["_request_headers"].(map[string]any)
	return headers, nil
}

// virtualKeyCandidate picks the single resolvable credential:
//
//   - an Authorization bearer that parses under the exact Bearer grammar and
//     carries a virtual key WINS over X-Api-Key (deterministic; verification
//     happens exactly once, and same-token duplication still verifies once);
//   - otherwise an X-Api-Key virtual key;
//   - otherwise nothing. Unverified JWT-shaped bearers, provider keys, and
//     X-Torana-* headers are NOT candidates.
func virtualKeyCandidate(headers map[string]any) (string, bool) {
	if auth, ok := headers["Authorization"].(string); ok {
		if token, ok := parseBearer(auth); ok && strings.HasPrefix(token, virtualKeyPrefix) {
			return token, true
		}
	}
	if apiKey, ok := headers["X-Api-Key"].(string); ok && strings.HasPrefix(apiKey, virtualKeyPrefix) {
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
	for i < len(authHeader) && !isSpace(authHeader[i]) {
		i++
	}
	if i == 0 {
		return "", false
	}
	if !strings.EqualFold(authHeader[:i], "Bearer") {
		return "", false
	}
	j := i
	for j < len(authHeader) && isSpace(authHeader[j]) {
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
		if isSpace(cred[k]) {
			return "", false // internal or trailing whitespace
		}
	}
	return cred, true
}

// isSpace reports ASCII whitespace: SP (0x20) and HTAB (0x09) are the only
// legal header separators; CR/LF cannot legally appear inside a header value
// and are treated as malformed rather than passed through.
func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
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
			// response, never by HostError.
			return "", verifyNoIdentity, fmt.Errorf("auth: verify_virtual_key refused (%s): %s", herr.Code, herr.Message)
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
