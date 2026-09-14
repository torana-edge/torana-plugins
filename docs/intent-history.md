# Intent history identity

Captured history restoration requires the same host conversation ID, tool call
ID, tool name, and inputs. This deliberately retires the Claude Code ID-remapping
bridge: remapped calls use the configured heuristic fill (or remain untouched
with `fill: off`). Equal arguments alone cannot safely identify an occurrence.
Torana derives conversation identity from the leading system prompt and first
user message; changes to that prefix can rotate the identity and prevent old
captures from restoring. Missing/malformed identity and lookup misses produce
info diagnostics. Request-scoped capture-context failures are best-effort and
do not block schema injection or response stripping.
