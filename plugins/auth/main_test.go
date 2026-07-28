package main

import "testing"

// First tests for auth. It had none anywhere, and the logic was inline in the
// hook closure so there was no seam to test through.
//
// plugin.json is explicit that this is a reference for the capability surface
// and not an access control. That makes tests MORE important, not less: people
// copy references.

// first returns the highest-precedence candidate, or a zero credential when
// there is none.
func first(headers map[string]any) credential {
	if c := readCredential(headers); len(c) > 0 {
		return c[0]
	}
	return credential{}
}

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
		"virtual key in Authorization is still a virtual key": {
			map[string]any{"Authorization": "Bearer sk-torana-123"},
			credentialVirtualKey, "sk-torana-123",
		},
		"trusted user header": {
			map[string]any{"X-Torana-User": "alice"},
			credentialTrustedUser, "alice",
		},
		// A provider key is not an identity. Treating it as one would attach an
		// identity Torana never verified, from a secret that says nothing about
		// who the caller is.
		"upstream provider key is not a credential": {
			map[string]any{"X-Api-Key": "sk-proj-openai-key"},
			credentialNone, "",
		},
		"opaque bearer token is not an identity": {
			map[string]any{"Authorization": "Bearer opaque-secret"},
			credentialNone, "",
		},
		"upstream provider key does not mask the user header": {
			map[string]any{"X-Api-Key": "sk-proj-openai-key", "X-Torana-User": "alice"},
			credentialTrustedUser, "alice",
		},
		"provider key in Authorization does not mask the user header": {
			map[string]any{"Authorization": "Bearer sk-proj-openai-key", "X-Torana-User": "alice"},
			credentialTrustedUser, "alice",
		},
		"no headers":                   {map[string]any{}, credentialNone, ""},
		"empty api key":                {map[string]any{"X-Api-Key": ""}, credentialNone, ""},
		"empty trusted user":           {map[string]any{"X-Torana-User": ""}, credentialNone, ""},
		"authorization without Bearer": {map[string]any{"Authorization": "Basic dXNlcjpwdw=="}, credentialNone, ""},
		"non-string header value":      {map[string]any{"X-Api-Key": 42}, credentialNone, ""},
		"empty api key does not mask the user header": {
			map[string]any{"X-Api-Key": "", "X-Torana-User": "alice"},
			credentialTrustedUser, "alice",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := first(tc.headers)
			if got.kind != tc.wantKind || got.token != tc.wantTok {
				t.Errorf("got kind=%v token=%q, want kind=%v token=%q",
					got.kind, got.token, tc.wantKind, tc.wantTok)
			}
		})
	}
}

// TestJWTDoesNotMaskAnIdentityItCannotVerify — a JWT-shaped bearer may be an
// identity claim OR an upstream secret (a Google identity token, a
// service-account assertion), and the header cannot tell them apart. This
// edition cannot verify either, so stopping at the JWT would discard an
// identity the caller did supply.
func TestJWTDoesNotMaskAnIdentityItCannotVerify(t *testing.T) {
	candidates := readCredential(map[string]any{
		"Authorization": "Bearer head.payload.sig",
		"X-Torana-User": "alice",
	})
	if len(candidates) != 2 {
		t.Fatalf("expected the JWT and the user header as candidates, got %+v", candidates)
	}
	if candidates[0].kind != credentialJWT {
		t.Errorf("the JWT should be tried first, got %v", candidates[0].kind)
	}
	if candidates[1].kind != credentialTrustedUser || candidates[1].token != "alice" {
		t.Errorf("the user header should remain available as a fallback, got %+v", candidates[1])
	}

	// And the JWT itself still does not resolve here.
	if _, outcome := resolveIdentity(candidates[0], nil); outcome == resolveOK {
		t.Error("a JWT must not resolve without verification")
	}
}

// A verifiable credential must win over an unverified assertion.
func TestReadCredentialPrefersVerifiableHeaders(t *testing.T) {
	// Most verifiable wins. A torana-issued key can be checked with the host;
	// a JWT cannot be checked at all in this edition, so it does not outrank
	// one just for arriving in Authorization.
	got := first(map[string]any{
		"Authorization": "Bearer head.payload.sig",
		"X-Api-Key":     "sk-torana-123",
		"X-Torana-User": "alice",
	})
	if got.kind != credentialVirtualKey {
		t.Errorf("a verifiable torana key should beat an unverifiable JWT, got %v", got.kind)
	}

	got = first(map[string]any{
		"X-Api-Key":     "sk-torana-123",
		"X-Torana-User": "alice",
	})
	if got.kind != credentialVirtualKey {
		t.Errorf("a verifiable key should beat the trusted-user header, got %v", got.kind)
	}

	// A JWT is TRIED first, but it is not terminal — see
	// TestJWTDoesNotMaskAnIdentityItCannotVerify. This asserts the ordering
	// only: the identity claim gets first refusal, and the trusted header is
	// the fallback when it does not resolve.
	got = first(map[string]any{
		"Authorization": "Bearer head.payload.sig",
		"X-Torana-User": "alice",
	})
	if got.kind != credentialJWT {
		t.Errorf("a JWT must not fall through to the trusted-user header, got %v", got.kind)
	}

	// But an opaque provider secret is not an identity claim, so it must not
	// suppress one.
	got = first(map[string]any{
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
			id, outcome := resolveIdentity(credential{kind: credentialVirtualKey, token: "sk-torana-x"}, verify)
			if outcome == resolveOK {
				t.Errorf("an unverifiable key produced an identity: %+v", id)
			}
		})
	}
}

func TestVerifiedKeyYieldsTheHostsIdentity(t *testing.T) {
	id, outcome := resolveIdentity(
		credential{kind: credentialVirtualKey, token: "sk-torana-x"},
		func(string) (VerifyResponse, bool) {
			return VerifyResponse{Status: "ok", TenantID: "acme", TeamID: "platform", UserID: "alice"}, true
		})
	if outcome != resolveOK {
		t.Fatal("a verified key should yield an identity")
	}
	if id.tenantID != "acme" || id.teamID != "platform" || id.userID != "alice" {
		t.Errorf("identity = %+v", id)
	}
}

// The verifier must be the source of identity — resolveIdentity must not
// substitute anything of its own for a field the host left empty.
func TestVerifiedKeyDoesNotInventMissingFields(t *testing.T) {
	id, outcome := resolveIdentity(
		credential{kind: credentialVirtualKey, token: "sk-torana-x"},
		func(string) (VerifyResponse, bool) {
			return VerifyResponse{Status: "ok", TenantID: "acme"}, true
		})
	if outcome != resolveOK {
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
	if _, outcome := resolveIdentity(credential{kind: credentialJWT, token: "jwt"}, nil); outcome == resolveOK {
		t.Error("a JWT must not resolve to an identity without verification")
	}
}

// TestTrustedUserCarriesItsScope — the host exposes X-Torana-Team and
// X-Torana-Tenant through the same allowlist as X-Torana-User, and reading only
// the user silently dropped the scope a caller had explicitly set.
func TestTrustedUserCarriesItsScope(t *testing.T) {
	cred := first(map[string]any{
		"X-Torana-User":   "alice",
		"X-Torana-Team":   "platform",
		"X-Torana-Tenant": "acme",
	})
	id, outcome := resolveIdentity(cred, nil)
	if outcome != resolveOK {
		t.Fatal("a trusted-user assertion should resolve")
	}
	if id.userID != "alice" || id.teamID != "platform" || id.tenantID != "acme" {
		t.Errorf("scope dropped: %+v", id)
	}
}

// Without a tenant header the default still applies, so an unscoped assertion
// keeps working.
func TestTrustedUserWithoutScopeUsesTheDefaultTenant(t *testing.T) {
	cred := first(map[string]any{"X-Torana-User": "alice"})
	id, outcome := resolveIdentity(cred, nil)
	if outcome != resolveOK || id.tenantID != "default-tenant" || id.userID != "alice" {
		t.Errorf("got %+v outcome=%v", id, outcome)
	}
	if id.teamID != "" {
		t.Errorf("invented a team: %q", id.teamID)
	}
}

// TestHostRejectionIsNotOverriddenByTheTrustedHeader — "the host refused this
// credential" and "we learned nothing" are different answers, and only the
// second should let a lower-precedence candidate speak.
//
// Presenting a revoked virtual key alongside X-Torana-User must not quietly
// succeed on the header. It is not an escalation — the header works on its own
// without any key — but an explicit rejection silently becoming an acceptance
// is the wrong default in a file people copy.
func TestHostRejectionIsNotOverriddenByTheTrustedHeader(t *testing.T) {
	rejected := func(string) (VerifyResponse, bool) {
		return VerifyResponse{Status: "error", Message: "revoked"}, true
	}
	if _, outcome := resolveIdentity(credential{kind: credentialVirtualKey, token: "sk-torana-revoked"}, rejected); outcome != resolveRejected {
		t.Errorf("an explicitly refused key should report resolveRejected, got %v", outcome)
	}

	// A host call that FAILED is different: nothing was learned about the key,
	// so a later candidate may still resolve.
	unreachable := func(string) (VerifyResponse, bool) { return VerifyResponse{}, false }
	if _, outcome := resolveIdentity(credential{kind: credentialVirtualKey, token: "sk-torana-x"}, unreachable); outcome != resolveNoIdentity {
		t.Errorf("an unreachable verifier should report resolveNoIdentity, got %v", outcome)
	}
}

// The precedence LOOP, not resolveIdentity in isolation. Both bugs found in
// this file lived in the ordering — an unverifiable JWT masking a header the
// caller supplied, and a host rejection being overridden by that same header —
// and neither was reachable from a test that called resolveIdentity alone.
func TestIdentityFromHeadersPrecedence(t *testing.T) {
	ok := func(id VerifyResponse) func(string) (VerifyResponse, bool) {
		return func(string) (VerifyResponse, bool) { return id, true }
	}
	rejected := ok(VerifyResponse{Status: "error", Message: "revoked"})
	unreachable := func(string) (VerifyResponse, bool) { return VerifyResponse{}, false }
	verified := ok(VerifyResponse{Status: "ok", TenantID: "acme", UserID: "carol"})

	for name, tc := range map[string]struct {
		headers    map[string]any
		verify     func(string) (VerifyResponse, bool)
		wantOK     bool
		wantUserID string
	}{
		// The round-4 defect: a key the host REFUSED must not fall through.
		"revoked key does not fall through to the user header": {
			map[string]any{"X-Api-Key": "sk-torana-revoked", "X-Torana-User": "alice"},
			rejected, false, "",
		},
		// But a verifier that could not be reached taught us nothing, so the
		// next candidate may still speak.
		"unreachable verifier falls through": {
			map[string]any{"X-Api-Key": "sk-torana-x", "X-Torana-User": "alice"},
			unreachable, true, "alice",
		},
		// The round-3 defect: an unverifiable JWT must not mask the header.
		"jwt does not mask the user header": {
			map[string]any{"Authorization": "Bearer head.payload.sig", "X-Torana-User": "alice"},
			unreachable, true, "alice",
		},
		"provider key does not mask the user header": {
			map[string]any{"Authorization": "Bearer sk-proj-openai", "X-Torana-User": "alice"},
			unreachable, true, "alice",
		},
		"a verified key wins over the header": {
			map[string]any{"X-Api-Key": "sk-torana-good", "X-Torana-User": "alice"},
			verified, true, "carol",
		},
		"nothing at all": {map[string]any{}, unreachable, false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			id, got := identityFromHeaders(tc.headers, tc.verify)
			if got != tc.wantOK {
				t.Fatalf("resolved=%v, want %v (identity %+v)", got, tc.wantOK, id)
			}
			if got && id.userID != tc.wantUserID {
				t.Errorf("userID = %q, want %q", id.userID, tc.wantUserID)
			}
		})
	}
}
