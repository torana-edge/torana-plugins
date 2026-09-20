# Reference only — not an authentication boundary

**This is work in progress and reference code.** It is a worked example of
Torana's identity and request-blocking capabilities, not a plugin to deploy.
It is excluded from the public plugin listing, and it fails open: use it to
read, not to protect anything.

[Why it is not listed](../../README.md#the-auth-reference-is-not-a-listed-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

## Do not use it as access control

Its `failure_mode` is `pass`, deliberately. Only an explicit `rejected` answer
from the host's verifier blocks a request; every other outcome lets the
request through:

- the verifier is unwired (`NOT_CONFIGURED`) or unavailable → no identity, the
  request continues;
- the call fails, is denied, or answers something this plugin cannot parse →
  the hook errors, and `failure_mode: pass` continues the request;
- the caller presents no Torana virtual key at all → the request continues on
  the host's own `Authorization` handling.

An access-control plugin must choose block semantics for those cases. This one
does not, so anything it appears to protect is protected only while the
verifier happens to answer.

## What it actually does today

One hook, `run_before_request`, and it never modifies the request — identity
travels as an attributed `env.set_identity` verdict, which changes the
rate-limit key the host applies.

1. **Finds one candidate credential.** From the host-injected allowlisted
   headers: an `Authorization: Bearer <token>` under the exact Bearer grammar
   wins over `X-Api-Key`; both must carry a `sk-torana-` prefixed
   printable-ASCII token. Nothing else is a candidate — caller-controlled
   `X-Torana-*` headers are not identity (a caller could rotate them to evade
   identity-based limits), and JWT-shaped or provider-key bearers are secrets
   for the upstream, not identities this edition can verify.
2. **Asks the host to verify it,** through the `verify_virtual_key` extension
   call, and strictly validates the reply: exactly the statuses `ok` and
   `rejected`, no unknown or duplicate members, no trailing JSON.
3. **On `ok`, sets an identity.** A non-empty profile composes a
   collision-proof identity from tenant/team/user; an empty profile composes a
   domain-separated digest of the verified token, so per-key rate limiting
   still works. The raw token is never used as an identity.
4. **On `rejected`, emits the example 401** (`virtual_key_rejected`) rather
   than falling back to the operator's provider credential, which would turn a
   revoked key into authenticated access. The verifier's optional diagnostic
   message is never reflected to the caller, logged, or put in an identity.

Everything else passes.

## Why it is not listed

It is in the repository as an executable reference for the capability surface
— `env.request_headers`, `env.host_call.verify_virtual_key`,
`env.set_identity`, `env.block_request` — and it stays in the release
inventory so the example keeps compiling against the current ABI. It is not
published to the plugin registry, has no setup guide, and should not be
installed as though it were one.

If you need real authentication at the edge, write a plugin that chooses
`failure_mode: block` and decides explicitly what every non-answer means.
