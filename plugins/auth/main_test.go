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
		// ...and it must not SUPPRESS the header that is an identity. This is
		// the shape that matters in practice: a harness sends its provider key
		// and Torana's user header on the same request.
		"upstream provider key does not mask the user header": {
			map[string]any{"X-Api-Key": "sk-proj-openai-key", "X-Torana-User": "alice"},
			credentialTrustedUser, "alice",
		},
		// The commonest shape of all, and the one the previous fix missed: an
		// OpenAI-shaped client puts its provider key in Authorization, not
		// X-Api-Key. Treating every bearer token as an identity claim meant
		// this yielded nothing.
		"provider key in Authorization does not mask the user header": {
			map[string]any{"Authorization": "Bearer sk-proj-openai-key", "X-Torana-User": "alice"},
			credentialTrustedUser, "alice",
		},
		"opaque bearer token is not an identity": {
			map[string]any{"Authorization": "Bearer opaque-secret"},
			credentialNone, "",
		},
		// A torana-issued key is verifiable wherever it was sent.
		"virtual key in Authorization is still a virtual key": {
			map[string]any{"Authorization": "Bearer sk-torana-123"},
			credentialVirtualKey, "sk-torana-123",
		},
		"empty api key does not mask the user header": {
			map[string]any{"X-Api-Key": "", "X-Torana-User": "alice"},
			credentialTrustedUser, "alice",
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
	// Most verifiable wins. A torana-issued key can be checked with the host;
	// a JWT cannot be checked at all in this edition, so it does not outrank
	// one just for arriving in Authorization.
	got := readCredential(map[string]any{
		"Authorization": "Bearer head.payload.sig",
		"X-Api-Key":     "sk-torana-123",
		"X-Torana-User": "alice",
	})
	if got.kind != credentialVirtualKey {
		t.Errorf("a verifiable torana key should beat an unverifiable JWT, got %v", got.kind)
	}

	got = readCredential(map[string]any{
		"X-Api-Key":     "sk-torana-123",
		"X-Torana-User": "alice",
	})
	if got.kind != credentialVirtualKey {
		t.Errorf("a verifiable key should beat the trusted-user header, got %v", got.kind)
	}

	// A JWT is terminal even though this edition cannot verify it: falling
	// through to an UNVERIFIED header when a real identity claim was presented
	// would be a downgrade.
	got = readCredential(map[string]any{
		"Authorization": "Bearer head.payload.sig",
		"X-Torana-User": "alice",
	})
	if got.kind != credentialJWT {
		t.Errorf("a JWT must not fall through to the trusted-user header, got %v", got.kind)
	}

	// But an opaque provider secret is not an identity claim, so it must not
	// suppress one.
	got = readCredential(map[string]any{
		"Authorization": "Bearer sk-proj-openai",
		"X-Torana-User": "alice",
	})
	if got.kind != credentialTrustedUser {
		t.Errorf("a provider key in Authorization must not mask the user header, got %v", got.kind)
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

// TestTrustedUserCarriesItsScope — the host exposes X-Torana-Team and
// X-Torana-Tenant through the same allowlist as X-Torana-User, and reading only
// the user silently dropped the scope a caller had explicitly set.
func TestTrustedUserCarriesItsScope(t *testing.T) {
	cred := readCredential(map[string]any{
		"X-Torana-User":   "alice",
		"X-Torana-Team":   "platform",
		"X-Torana-Tenant": "acme",
	})
	id, ok := resolveIdentity(cred, nil)
	if !ok {
		t.Fatal("a trusted-user assertion should resolve")
	}
	if id.userID != "alice" || id.teamID != "platform" || id.tenantID != "acme" {
		t.Errorf("scope dropped: %+v", id)
	}
}

// Without a tenant header the default still applies, so an unscoped assertion
// keeps working.
func TestTrustedUserWithoutScopeUsesTheDefaultTenant(t *testing.T) {
	cred := readCredential(map[string]any{"X-Torana-User": "alice"})
	id, ok := resolveIdentity(cred, nil)
	if !ok || id.tenantID != "default-tenant" || id.userID != "alice" {
		t.Errorf("got %+v ok=%v", id, ok)
	}
	if id.teamID != "" {
		t.Errorf("invented a team: %q", id.teamID)
	}
}
