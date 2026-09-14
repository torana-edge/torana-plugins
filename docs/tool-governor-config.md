# Tool governor configuration

The governor reads config through `PluginConfigStrict`, retaining SDK refusal
and malformed-envelope distinctions. A successful empty value means unset
configuration and becomes `{}`; like an explicit empty object, it leaves the
tool inventory unchanged. Whitespace-only or malformed policy JSON is invalid.
Denied calls, unavailable calls, missing envelopes, and transport failures fail
the hook rather than advertising unrestricted tools or reusing an old policy.

The exact returned config string still keys the per-instance parsed-policy
cache, and each hook reads the current config so changes take effect immediately.

This follow-through requires SDK #79. Its provisional SDK pin must be replaced
with a published release before landing. Coordinate with Plugins #51 so both
consumers use a common SDK release containing #78 and #79 when merged together.
