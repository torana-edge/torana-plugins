package main

import (
	"context"

	"encoding/json"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

type VerifyResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	TenantID string `json:"tenant_id,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if req.ToranaMetaJson == nil {
			req.ToranaMetaJson = []byte(`{}`)
		}

		var meta map[string]any
		if err := json.Unmarshal(req.ToranaMetaJson, &meta); err != nil {
			return nil, nil // skip on err
		}

		headers, ok := meta["_request_headers"].(map[string]any)
		if !ok {
			return nil, nil
		}

		cred := readCredential(headers)
		if cred.kind == credentialNone {
			return nil, nil
		}
		if cred.kind == credentialJWT {
			// Enterprise auth is not available in the open-source edition.
			// Real JWT verification lives in torana-edge/private-nucleus.
			return nil, nil
		}

		identity, ok := resolveIdentity(cred, verifyVirtualKey)
		if !ok {
			return nil, nil
		}
		applyIdentity(meta, identity)

		metaBytes, err := json.Marshal(meta)
		if err != nil {
			return nil, nil
		}
		req.ToranaMetaJson = metaBytes
		return req, nil
	})
}

// The parsing and normalization below are separated from the hook so they can
// be tested. They were inline in the closure, which is why this plugin had no
// tests at all: there was no seam to test through, and a plugin nobody can test
// is a poor reference for the capability surface — which is the only thing this
// plugin is for. See plugin.json: it is deliberately NOT an access control.

type credentialKind int

const (
	credentialNone credentialKind = iota
	credentialJWT
	credentialVirtualKey
	credentialTrustedUser
)

// virtualKeyPrefix marks a Torana-issued key, which the host can verify.
const virtualKeyPrefix = "sk-torana-"

type credential struct {
	kind  credentialKind
	token string
}

type identity struct {
	tenantID string
	teamID   string
	userID   string
}

// readCredential classifies the inbound credential. Headers arrive canonicalized
// (net/http MIME form) through the env.request_headers grant allowlist, so the
// casing here is the casing the host produces.
//
// Precedence, in order:
//
//  1. Authorization: Bearer — a real credential. Terminal: it cannot be
//     verified in this edition, and falling through to an UNVERIFIED header
//     when a verifiable credential was presented would be a downgrade.
//  2. X-Api-Key beginning sk-torana- — a virtual key the host can verify.
//  3. X-Torana-User — a trusted-header identity assertion with no verification
//     at all, so it is last.
//
// An X-Api-Key that Torana did not issue is NOT a credential here and does not
// stop the search. It is a secret for the upstream provider and says nothing
// about who the caller is — so letting it suppress the one header that does
// would mask the identity for exactly the requests that carry both. That is
// what the original else-if chain did, and the reason it looked deliberate is
// that nothing wrote down which of these are identities and which are secrets.
func readCredential(headers map[string]any) credential {
	if authHeader, ok := headers["Authorization"].(string); ok && strings.HasPrefix(authHeader, "Bearer ") {
		return credential{kind: credentialJWT, token: strings.TrimPrefix(authHeader, "Bearer ")}
	}
	if apiKey, ok := headers["X-Api-Key"].(string); ok && strings.HasPrefix(apiKey, virtualKeyPrefix) {
		return credential{kind: credentialVirtualKey, token: apiKey}
	}
	if toranaUser, ok := headers["X-Torana-User"].(string); ok && toranaUser != "" {
		return credential{kind: credentialTrustedUser, token: toranaUser}
	}
	return credential{}
}

// resolveIdentity turns a credential into an identity, returning false when it
// cannot be established. verify is injected so the host call can be substituted
// in tests.
func resolveIdentity(cred credential, verify func(token string) (VerifyResponse, bool)) (identity, bool) {
	switch cred.kind {
	case credentialTrustedUser:
		return identity{tenantID: "default-tenant", userID: cred.token}, true
	case credentialVirtualKey:
		res, ok := verify(cred.token)
		if !ok || res.Status != "ok" {
			// An unverifiable key must not yield an identity. Falling through
			// with a partial one would attach an empty tenant to the request.
			return identity{}, false
		}
		return identity{tenantID: res.TenantID, teamID: res.TeamID, userID: res.UserID}, true
	default:
		return identity{}, false
	}
}

// applyIdentity writes the resolved identity into ToranaMeta, omitting fields
// the verifier did not supply rather than writing empty strings.
func applyIdentity(meta map[string]any, id identity) {
	if id.tenantID != "" {
		meta["tenant_id"] = id.tenantID
	}
	if id.teamID != "" {
		meta["team_id"] = id.teamID
	}
	if id.userID != "" {
		meta["user_id"] = id.userID
	}
}

// verifyVirtualKey asks the host to validate a Torana-issued key. The grant is
// named env.host_call.verify_virtual_key in plugin.json, which is what permits
// this HostCall("verify_virtual_key", ...).
func verifyVirtualKey(token string) (VerifyResponse, bool) {
	resStr, err := sdk.HostCall("verify_virtual_key", token)
	if err != nil || resStr == "" {
		return VerifyResponse{}, false
	}
	var res VerifyResponse
	if err := json.Unmarshal([]byte(resStr), &res); err != nil {
		return VerifyResponse{}, false
	}
	return res, true
}
