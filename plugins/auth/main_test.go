package main

import "testing"

// First tests for auth. It had none anywhere, and the logic was inline in the
// hook closure so there was no seam to test through.
//
// plugin.json is explicit that this is a reference for the capability surface
// and not an access control. That makes tests MORE important, not less: people
// copy references.

func TestReadCredentialClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		headers  map[string]any
		wantKind credentialKind
		wantTok  string
	}{
		"bearer token is a JWT": {
			map[string]any{"Authorization": "Bearer abc.def.ghi"},
			credentialJWT, "abc.def.ghi",
		},
		"torana-issued key is a virtual key": {
			map[string]any{"X-Api-Key": "sk-torana-123"},
			credentialVirtualKey, "sk-torana-123",
		},
		"trusted user header": {
			map[string]any{"X-Torana-User": "alice"},
			credentialTrustedUser, "alice",
		},
		// A provider key is not an identity. Treating it as one would attach
		// an identity Torana never verified, derived from a secret that says
		// nothing about who the caller is.
		"upstream provider key is not a credential": {
			map[string]any{"X-Api-Key": "sk-proj-openai-key"},
			credentialNone, "",
		},
		"no headers":                   {map[string]any{}, credentialNone, ""},
		"empty api key":                {map[string]any{"X-Api-Key": ""}, credentialNone, ""},
		"empty trusted user":           {map[string]any{"X-Torana-User": ""}, credentialNone, ""},
		"authorization without Bearer": {map[string]any{"Authorization": "Basic dXNlcjpwdw=="}, credentialNone, ""},
		"non-string header value":      {map[string]any{"X-Api-Key": 42}, credentialNone, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := readCredential(tc.headers)
			if got.kind != tc.wantKind || got.token != tc.wantTok {
				t.Errorf("got kind=%v token=%q, want kind=%v token=%q",
					got.kind, got.token, tc.wantKind, tc.wantTok)
			}
		})
	}
}

// A verifiable credential must win over an unverified assertion.
func TestReadCredentialPrefersVerifiableHeaders(t *testing.T) {
	got := readCredential(map[string]any{
		"Authorization": "Bearer jwt",
		"X-Api-Key":     "sk-torana-123",
		"X-Torana-User": "alice",
	})
	if got.kind != credentialJWT {
		t.Errorf("Authorization should win, got %v", got.kind)
	}

	got = readCredential(map[string]any{
		"X-Api-Key":     "sk-torana-123",
		"X-Torana-User": "alice",
	})
	if got.kind != credentialVirtualKey {
		t.Errorf("a verifiable key should beat the trusted-user header, got %v", got.kind)
	}
}

// TestUnverifiableKeyYieldsNoIdentity is the security-relevant one: a key the
// host could not verify must produce nothing. Falling through with a partial
// identity would attach an empty tenant to the request.
func TestUnverifiableKeyYieldsNoIdentity(t *testing.T) {
	for name, verify := range map[string]func(string) (VerifyResponse, bool){
		"host call failed":  func(string) (VerifyResponse, bool) { return VerifyResponse{}, false },
		"status not ok":     func(string) (VerifyResponse, bool) { return VerifyResponse{Status: "error"}, true },
		"empty status":      func(string) (VerifyResponse, bool) { return VerifyResponse{}, true },
		"rejected with msg": func(string) (VerifyResponse, bool) { return VerifyResponse{Status: "error", Message: "revoked"}, true },
	} {
		t.Run(name, func(t *testing.T) {
			id, ok := resolveIdentity(credential{kind: credentialVirtualKey, token: "sk-torana-x"}, verify)
			if ok {
				t.Errorf("an unverifiable key produced an identity: %+v", id)
			}
		})
	}
}

func TestVerifiedKeyYieldsTheHostsIdentity(t *testing.T) {
	id, ok := resolveIdentity(
		credential{kind: credentialVirtualKey, token: "sk-torana-x"},
		func(string) (VerifyResponse, bool) {
			return VerifyResponse{Status: "ok", TenantID: "acme", TeamID: "platform", UserID: "alice"}, true
		})
	if !ok {
		t.Fatal("a verified key should yield an identity")
	}
	if id.tenantID != "acme" || id.teamID != "platform" || id.userID != "alice" {
		t.Errorf("identity = %+v", id)
	}
}

// The verifier must be the source of identity — resolveIdentity must not
// substitute anything of its own for a field the host left empty.
func TestVerifiedKeyDoesNotInventMissingFields(t *testing.T) {
	id, ok := resolveIdentity(
		credential{kind: credentialVirtualKey, token: "sk-torana-x"},
		func(string) (VerifyResponse, bool) {
			return VerifyResponse{Status: "ok", TenantID: "acme"}, true
		})
	if !ok {
		t.Fatal("expected an identity")
	}
	if id.teamID != "" || id.userID != "" {
		t.Errorf("invented identity fields the verifier did not supply: %+v", id)
	}
}

func TestApplyIdentityOmitsEmptyFields(t *testing.T) {
	meta := map[string]any{"existing": "value"}
	applyIdentity(meta, identity{tenantID: "acme"})

	if meta["tenant_id"] != "acme" {
		t.Errorf("tenant_id not written: %v", meta)
	}
	// Writing empty strings would make "verified but no team" indistinguishable
	// from "team is the empty string" for anything reading ToranaMeta.
	for _, key := range []string{"team_id", "user_id"} {
		if _, present := meta[key]; present {
			t.Errorf("%s written as an empty value: %v", key, meta)
		}
	}
	if meta["existing"] != "value" {
		t.Error("applyIdentity clobbered unrelated meta")
	}
}

func TestJWTIsNotResolvedInTheOpenSourceEdition(t *testing.T) {
	// Real verification lives in private-nucleus. Resolving one here would mean
	// trusting an unverified token.
	if _, ok := resolveIdentity(credential{kind: credentialJWT, token: "jwt"}, nil); ok {
		t.Error("a JWT must not resolve to an identity without verification")
	}
}
