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
	// team and tenant accompany a trusted-user assertion. The host exposes
	// X-Torana-Team and X-Torana-Tenant through the same allowlist as
	// X-Torana-User, and reading only the user meant a caller that scoped its
	// request got the scope silently dropped.
	team   string
	tenant string
}

type identity struct {
	tenantID string
	teamID   string
	userID   string
}

// looksLikeJWT reports whether a bearer token is structurally a JWT: three
// non-empty dot-separated segments.
//
// Authorization carries two unrelated things. An OpenAI-shaped client puts its
// PROVIDER key there ("Bearer sk-proj-…"), which is a secret for the upstream
// and says nothing about who the caller is. A gateway deployment puts a JWT
// there, which is an identity claim this edition cannot verify.
//
// Treating every bearer token as the second kind is what made the previous fix
// miss the commonest case: a harness sending its provider key in Authorization
// with X-Torana-User alongside still got no identity, which is the exact
// scenario that fix was written for.
func looksLikeJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	return true
}

// readCredential classifies the inbound credential. Headers arrive canonicalized
// (net/http MIME form) through the env.request_headers grant allowlist, so the
// casing here is the casing the host produces.
//
// Precedence, most verifiable first:
//
//  1. A torana-issued key, in EITHER Authorization or X-Api-Key. The host can
//     verify it, so it wins wherever it was sent — a client that puts it in
//     Authorization is not doing anything unusual.
//  2. A structurally valid JWT in Authorization. A real identity claim, and
//     terminal: falling through to an unverified header when one was presented
//     would be a downgrade.
//  3. X-Torana-User — a trusted-header assertion with no verification at all,
//     so it comes last.
//
// Anything else, in either header, is a SECRET rather than an identity and does
// not stop the search. Letting a provider key suppress X-Torana-User would mask
// the identity on exactly the requests that carry both.
func readCredential(headers map[string]any) credential {
	bearer := ""
	if authHeader, ok := headers["Authorization"].(string); ok && strings.HasPrefix(authHeader, "Bearer ") {
		bearer = strings.TrimPrefix(authHeader, "Bearer ")
	}
	apiKey, _ := headers["X-Api-Key"].(string)

	for _, token := range []string{bearer, apiKey} {
		if strings.HasPrefix(token, virtualKeyPrefix) {
			return credential{kind: credentialVirtualKey, token: token}
		}
	}
	if looksLikeJWT(bearer) {
		return credential{kind: credentialJWT, token: bearer}
	}
	if toranaUser, ok := headers["X-Torana-User"].(string); ok && toranaUser != "" {
		team, _ := headers["X-Torana-Team"].(string)
		tenant, _ := headers["X-Torana-Tenant"].(string)
		return credential{kind: credentialTrustedUser, token: toranaUser, team: team, tenant: tenant}
	}
	return credential{}
}

// resolveIdentity turns a credential into an identity, returning false when it
// cannot be established. verify is injected so the host call can be substituted
// in tests.
func resolveIdentity(cred credential, verify func(token string) (VerifyResponse, bool)) (identity, bool) {
	switch cred.kind {
	case credentialTrustedUser:
		// The caller's own tenant when it sent one; the default otherwise, so
		// an unscoped assertion still resolves.
		tenant := cred.tenant
		if tenant == "" {
			tenant = "default-tenant"
		}
		return identity{tenantID: tenant, teamID: cred.team, userID: cred.token}, true
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
