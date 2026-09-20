// The intent plugin captures WHY the model makes each tool call.
//
// Request side, it teaches the convention: every tool schema gains a required
// "i" property ("what question are you answering?"), reinforced by a system
// prompt addendum that embeds a one-line example transcript. No synthetic
// messages are injected — a fake conversation is indistinguishable from real
// history and measurably contaminates behavior (verbatim intent leaks,
// topic-anchored refusals; see the Jul 16 experiments). It also keeps the
// model's own history consistent: prior tool calls get their captured "i"
// restored (rehydration), and never-captured ones get a heuristic fill —
// without this the model imitates its "i"-stripped history and emission
// collapses per tool (see rehydrateHistoryIntents). Response side, it
// extracts the "i" value from the streamed tool call into the shared
// cross-request cache (bound to conversation, call ID, and inputs), and
// strips "i" back off so the agent harness never sees it.
//
// It exists as its own plugin so the compactors are independent consumers:
// run "intent" plus EITHER keyword_compactor (deterministic, local) OR
// compactor (operator-bound model summarization) — both read the same intent cache.
//
// The response side covers BOTH answer shapes, because the request side
// injects "i" into both:
//
//   - streamed: the SDK's StreamHandler buffers tool-call fragments host-side
//     (meta_append, under env.meta_set) and presents them to OnToolCall as one
//     complete call. Start/deltas are suppressed and an equivalent assembled
//     start+delta+stop is emitted at block completion. Callback errors are
//     consumed by StreamHandler for fail-open re-emission of the original
//     block — a streamed response must never be truncated by a plugin failure.
//   - non-streamed: the after-response hook rewrites the ordered response
//     blocks in place under the host's replacement rules.
//
// Both call captureAndStrip, so the capture protocol and the native-"i" rule
// have exactly one implementation.
//
// Captured intent is never written to the log. It is the user's task in the
// model's own words, and the plugin's contract is to move it into the cache
// the compactors read; diagnostics stay content-free.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

const (
	intentField    = "i"
	intentCacheKey = "intent"

	// intentDescription is the example-carrying "i" description. It measured
	// markedly better than an abstract instruction (75% vs 54% goal-tied
	// intents in the Jul 16 experiments).
	intentDescription = "the underlying question this call helps answer, NOT the action taken. " +
		"Good: 'where is the user locale mapped to a currency, to find the EU bug'. " +
		"Bad: 'reading currency.ts'."
)

// fillMode controls what happens to a history tool call whose intent was never
// captured (the model organically omitted "i" — nothing to rehydrate):
// "heuristic" (default) fills it with a template derived from the call's own
// arguments and the current task; "off" leaves it untouched. Loaded once from
// plugins.config.intent.
var (
	cfgMu     sync.Mutex
	cfgLoaded bool
	fillMode  = "heuristic"
)

// parseConfig is the pure config decoder. loadConfig publishes its result
// after the first successful host read and retries refused/failed reads. The
// host validates config against schema.json at write time.
func parseConfig(raw string) (fill string) {
	if raw == "" {
		return "heuristic"
	}
	var c struct {
		Fill string `json:"fill"`
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return "heuristic"
	}
	if c.Fill != "" {
		return c.Fill
	}
	return "heuristic"
}

func loadConfig() error {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if cfgLoaded {
		return nil
	}
	raw, err := sdk.PluginConfig()
	if err != nil {
		return fmt.Errorf("intent: load plugin config: %w", err)
	}
	fillMode = parseConfig(raw)
	cfgLoaded = true
	return nil
}

// resetConfigForTest restores every config global so a test row can install a
// fresh config. Production never calls it.
func resetConfigForTest() {
	cfgLoaded = false
	fillMode = "heuristic"
}

func init() {
	// ── Request side: teach the "i" convention ──────────────────────
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv1.ChatRequest) (sdk.RequestResult, error) {
		if !hasFunctionTool(req) {
			return sdk.PassRequest(), nil
		}
		// Resolve configuration before metadata writes or in-memory mutation.
		// A refusal must leave this invocation with no observable side effects;
		// loadConfig caches only successful reads, so a later request can retry.
		if err := loadConfig(); err != nil {
			return sdk.RequestResult{}, err
		}
		if err := sdk.MetaSet("intent:conversation", conversationID(req)); err != nil {
			sdk.Log(fmt.Sprintf("intent: capture context unavailable: %v", err), sdk.LogLevelInfo)
		}
		modified, err := injectIntentSchema(req)
		if err != nil {
			return sdk.RequestResult{}, err
		}
		promptChanged, err := injectSystemPrompt(req)
		if err != nil {
			return sdk.RequestResult{}, err
		}
		modified = promptChanged || modified
		// Re-hydrate "i" onto the model's PRIOR tool calls in history. We
		// strip "i" before returning to the harness, so the harness replays
		// the model's own tool calls without it — and the model imitates that
		// stripped history, dropping "i" on new calls within a few turns
		// (measured: ~96% single-turn capture collapses to ~18% multi-turn).
		// Restoring "i" here (from the cache we populated when it was first
		// emitted) shows the model a consistent history, so it keeps emitting.
		rehydrated, err := rehydrateHistoryIntents(req)
		if err != nil {
			return sdk.RequestResult{}, err
		}
		modified = rehydrated || modified
		if !modified {
			return sdk.PassRequest(), nil
		}
		return sdk.ReplaceRequest(req), nil
	})

	// ── Response side: extract intent from the assembled tool call ──
	//
	// StreamHandler buffers start/deltas host-side and presents the complete
	// call to OnToolCall; it re-emits the assembled start+delta+stop itself
	// (preserving the ToolCallRef, clearing a bound signature only when the
	// arguments actually change, and re-emitting the original block if the
	// callback errors — fail-open, because earlier fragments were already
	// suppressed and a truncated call would be executed by the agent).
	handler := sdk.NewStreamHandler()
	handler.OnToolCall(func(ctx context.Context, call sdk.ToolCall) (sdk.ToolCallAction, error) {
		return handleToolCall(call)
	})
	handler.Register()

	// ── Response side: the same capture and strip, not streamed ───────
	//
	// The request side injects "i" into every tool schema whether or not the
	// caller asked for a stream, so a non-streamed answer arrives carrying
	// the field Torana added. Without this hook the harness receives it: an
	// argument the tool never declared, which strict tools reject and lax
	// tools act on, and the intent is never captured for the compactors.
	//
	// The host contract for replace_response decides the mechanics:
	//
	//   - observational dispatches (a streamed response — already handled by
	//     the stream hook — or an upstream error) carry no body to rewrite
	//     and are passed untouched;
	//   - blocks keep their cardinality, arm and position, and tool identity
	//     is host-owned: only arguments_json is rewritten, in place;
	//   - clearing the provider signature is the prescribed response to
	//     changing the content it covers, so a stripped call drops its token
	//     and an untouched call keeps it (dropping provenance over unchanged
	//     content is rejected as forgery).
	sdk.OnAfterResponse(func(ctx context.Context, resp *pbv1.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		if !mutable || resp.GetMessage() == nil {
			return sdk.PassResponse(), nil
		}
		// Mutate a CLONE so a failure part-way through cannot leave the
		// accepted response half-stripped.
		out, ok := proto.Clone(resp).(*pbv1.ChatResponse)
		if !ok {
			return sdk.ResponseResult{}, fmt.Errorf("intent: response clone")
		}
		changed := false
		for _, block := range out.Message.Blocks {
			call := block.GetToolCall()
			if call == nil {
				continue
			}
			stripped, did, err := captureAndStrip(call.Name, call.Id, string(call.ArgumentsJson))
			if err != nil {
				return sdk.ResponseResult{}, err
			}
			if !did {
				continue
			}
			call.ArgumentsJson = []byte(stripped)
			call.Signature = ""
			changed = true
		}
		if !changed {
			return sdk.PassResponse(), nil
		}
		return sdk.ReplaceResponse(out), nil
	})
}

// handleToolCall extracts and caches the intent from one assembled STREAMED
// tool call, then strips "i" (unless the tool natively declares it) so the
// harness never sees the field Torana injected.
//
// Pass-through is SEMANTIC: whenever the plugin does not actually delete a
// field, the ORIGINAL argument bytes and the bound signature must travel
// unchanged. JSON formatting or key order in the model's output is never a
// reason to rewrite the block.
func handleToolCall(call sdk.ToolCall) (sdk.ToolCallAction, error) {
	// The "i" convention belongs to JSON-object function arguments. A
	// provider-native free-form payload is opaque text and must never be
	// parsed, cached, stripped, or converted into a function call.
	//
	// The non-streaming path has no counterpart to this gate: a response
	// tool call carries arguments_json and no invocation kind.
	if call.InvocationKind != pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FUNCTION {
		return sdk.PassToolCall(), nil
	}
	stripped, changed, err := captureAndStrip(call.Name, call.ID, call.Arguments)
	if err != nil {
		return sdk.ToolCallAction{}, err
	}
	if !changed {
		return sdk.PassToolCall(), nil
	}
	return sdk.ReplaceToolArguments(stripped), nil
}

// captureAndStrip is the ONE implementation of the response-side convention,
// shared by the streamed and non-streamed paths so the two cannot drift: the
// capture protocol, the observability, the native-"i" rule and the
// pass-through rule are decided in exactly one place.
//
// It returns the replacement arguments and whether they actually differ. A
// false `changed` means the caller must emit the ORIGINAL bytes — not a
// re-marshal of an equal object — so a bound provider signature stays valid.
// An error is a contract failure the caller turns into its path's fail-open
// (stream re-emission, or a hook error the host resolves by failure_mode).
func captureAndStrip(name, id, arguments string) (string, bool, error) {
	// Parse regardless of leading whitespace (the JSON decoder accepts it);
	// invalid, non-object, and "null" arguments (args stays nil) are not
	// representable and pass the exact bytes.
	args, err := decodeJSONObject([]byte(arguments))
	if err != nil || args == nil {
		return arguments, false, nil
	}

	// Extract and cache intent. Phase 0 observability: count how often the
	// model actually follows the convention, per tool. FIELD PRESENCE and
	// CAPTURE VALIDITY are independent facts: only a non-empty string is a
	// usable intent, but ANY present "i" key is Torana's injected field
	// unless the tool natively declares it (decided by the hadI marker).
	labels := map[string]string{"tool": name}
	rawI, hasIntent := args[intentField]
	intent, usable := rawI.(string)
	if usable && intent != "" {
		// Keep the shared compactor protocol, but restore history only from
		// a conversation- and occurrence-bound private entry. Equal tool
		// arguments do not imply equal intent. Remapped IDs safely take the
		// request side's configured heuristic/off behavior.
		//
		// CacheSet is best-effort: a refusal affects FUTURE compaction, not
		// the validity of this response, so it is logged and the current
		// tool call still completes. The host records the refusal itself.
		if err := sdk.SharedCacheSet(intentCacheKey+":"+id, intent); err != nil {
			sdk.Log(fmt.Sprintf("intent: cache_set %s:%s refused: %v", intentCacheKey, id, err), sdk.LogLevelInfo)
		}
		conversation, _, err := sdk.MetaGet("intent:conversation")
		if err == nil {
			if key := occurrenceKey(conversation, id, name, args); key != "" {
				if err := sdk.CacheSet(key, intent); err != nil {
					sdk.Log(fmt.Sprintf("intent: cache_set occurrence refused: %v", err), sdk.LogLevelInfo)
				}
			} else {
				reason := "missing_call_id"
				if conversation == "" {
					reason = "missing_conversation"
				}
				sdk.Log("intent: occurrence capture skipped: "+reason, sdk.LogLevelInfo)
			}
		} else {
			sdk.Log("intent: occurrence capture skipped: context_lookup_failed", sdk.LogLevelInfo)
		}
		sdk.EmitMetric("torana_intent_captured_total", sdk.MetricCounter, 1, labels)
		// CONTENT-FREE diagnostics only. The captured value is the user's
		// task in the model's words — file paths, product names, customer
		// context — and a debug line is still a log line: it lands in the
		// host log file and in anything shipping those logs onward. The
		// plugin's job is to move intent into the cache the compactors read,
		// not to publish it. Length is the one fact worth keeping here: it
		// separates "the model emitted a real intent" from a one-word
		// placeholder without reproducing either.
		sdk.Log(fmt.Sprintf("intent[%s %s]: captured %d runes", name, id, utf8.RuneCountInString(intent)), sdk.LogLevelDebug)
	} else {
		sdk.EmitMetric("torana_intent_absent_total", sdk.MetricCounter, 1, labels)
		sdk.Log(fmt.Sprintf("intent[%s %s]: ABSENT", name, id), sdk.LogLevelDebug)
	}

	// No "i" key at all: absent observability already emitted, exact pass
	// with NO marker lookup, marshal, or signature change.
	if !hasIntent {
		return arguments, false, nil
	}

	// Any present "i" value (string, empty, number, object, boolean, null)
	// is Torana's injected field unless the tool natively declares it. A
	// refusal to READ the hadI marker is a protocol failure (the key is only
	// written by this plugin's request side): log and return an error so the
	// caller re-emits the original call — never a guess about whether to
	// strip.
	hadI := ""
	if name != "" {
		var err error
		hadI, _, err = sdk.MetaGet("hadI:" + name)
		if err != nil {
			sdk.Log(fmt.Sprintf("intent: hadI meta_get refused: %v", err), sdk.LogLevelInfo)
			return arguments, false, fmt.Errorf("intent: hadI meta_get failed: %v", err)
		}
	}
	if hadI == "true" {
		// Native field: the original call (with "i" of ANY value and its
		// signature) passes byte-identical.
		return arguments, false, nil
	}

	// Injected "i" of ANY type/value: delete it and marshal the changed
	// object. The arguments changed, so the caller clears the bound
	// signature.
	delete(args, intentField)
	modifiedJSON, err := json.Marshal(args)
	if err != nil {
		return arguments, false, nil
	}
	return string(modifiedJSON), true, nil
}

// ==========================================================================
// History re-hydration
// ==========================================================================

// rehydrateHistoryIntents restores the "i" field onto the model's prior
// assistant tool calls in the conversation history, reading each intent back
// from the cross-request cache. This counters the model imitating its own
// "i"-stripped history. Calls whose intent was never captured are FILLED with
// a derived heuristic (unless fill is "off"): history "i" values act as
// few-shot examples, so a single "i"-less call becomes a self-reinforcing
// per-tool precedent (measured: one organic miss collapsed that tool's
// emission to 0 for the rest of the session), while presence — even a
// mediocre fill among real intents — sustains near-100% emission without
// dragging new-call quality down to the fill's level. A constant placeholder
// is NOT safe: models copy the literal value into new calls. Trailing
// reminder messages recovered only ~70% in the same experiments and add
// contamination surface — kept as a fallback idea, not implemented.
//
// Cache semantics are the request-side contract: NOT_FOUND and present-empty
// are both unusable (the fill path, which is never cached); any other refusal
// or a malformed reply is a contract/configuration defect and returns an
// error so failure_mode applies and the host records the failure.
func rehydrateHistoryIntents(req *pbv1.ChatRequest) (bool, error) {
	if err := loadConfig(); err != nil {
		return false, err
	}
	restored, filled, present := 0, 0, 0
	conversation, identityReason := conversationIdentity(req)
	missingID, lookupMiss := 0, 0
	defer func() {
		if identityReason != "" {
			sdk.Log("intent: history restoration unavailable: "+identityReason, sdk.LogLevelInfo)
		}
		if missingID > 0 || lookupMiss > 0 {
			sdk.Log(fmt.Sprintf("intent: history occurrence unavailable: missing_call_id=%d lookup_miss=%d (uncaptured, expired, or remapped identity); using configured fill", missingID, lookupMiss), sdk.LogLevelInfo)
		}
	}()
	modified := false
	for _, msg := range req.Messages {
		// The semantic scope is ASSISTANT HISTORY (past tool-use turns), so
		// the role gate is preserved; the calls themselves are the ordered
		// tool-use blocks.
		if msg.Role != "assistant" || len(msg.Blocks) == 0 {
			continue
		}
		calls := sdk.ToolCalls(msg)
		if len(calls) == 0 {
			continue
		}
		for _, tc := range calls {
			if tc.InvocationKind != pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FUNCTION {
				continue
			}
			var args map[string]any
			if len(tc.Arguments) == 0 {
				args = map[string]any{}
			} else if decoded, err := decodeJSONObject(tc.Arguments); err != nil || decoded == nil {
				// Unrepresentable history arguments: null (which decodes as a
				// nil map), arrays, scalars, malformed JSON. Leave them
				// unchanged — assigning into a nil map would panic.
				continue
			} else {
				args = decoded
			}
			if _, ok := args[intentField]; ok {
				present++
				continue // already carries "i"
			}
			// Missing/remapped identity declines to heuristic/off. A
			// tool-arguments-only fallback can rewrite unrelated old history.
			key := occurrenceKey(conversation, tc.Id, tc.Name, args)
			if tc.Id == "" {
				missingID++
			}
			intent := ""
			if key != "" {
				var err error
				intent, _, err = sdk.CacheGet(key)
				if err != nil {
					return false, fmt.Errorf("intent: cache_get %q: %w", key, err)
				}
			}
			if intent != "" {
				// Publish the verified occurrence's captured intent for compactors.
				if err := sdk.SharedCacheSet(intentCacheKey+":"+tc.Id, intent); err != nil {
					return false, fmt.Errorf("intent: cache_set %s:%s refused: %v", intentCacheKey, tc.Id, err)
				}
				restored++
			} else {
				if key != "" {
					lookupMiss++
				}
				if fillMode == "off" {
					continue
				}
				// Filled values are injected into history only — never cached
				// and never bridged: the intent cache stays real-captured-only
				// so compaction quality is driven by real intents.
				intent = heuristicFill(tc.Name, args)
				sdk.EmitMetric("torana_intent_filled_total", sdk.MetricCounter, 1, map[string]string{"tool": tc.Name})
				// Content-free: the fill is DERIVED from the call's own
				// arguments (a path, a query, a command line), so logging it
				// would leak the same workflow context as logging a captured
				// intent.
				sdk.Log(fmt.Sprintf("intent-fill[%s %s]: filled %d runes", tc.Name, tc.Id, utf8.RuneCountInString(intent)), sdk.LogLevelDebug)
				filled++
			}
			args[intentField] = intent
			if b, err := json.Marshal(args); err == nil {
				// The view is a COPY and the mutation must be
				// PROVENANCE-AWARE: ReplaceToolCall targets the real block
				// (position-addressed by the view), clears the call-bound
				// signature token on a REAL change, and preserves it on a
				// byte-identical no-op. The intent field was absent before,
				// so the re-marshal always differs — a real change.
				if err := sdk.ReplaceToolCall(msg, tc.Block, sdk.ToolCallInput{
					Id: tc.Id, Name: tc.Name, Arguments: b,
				}); err != nil {
					return false, fmt.Errorf("intent: replace history tool call: %w", err)
				}
				modified = true
			}
		}
	}
	if restored+filled > 0 {
		sdk.Log(fmt.Sprintf("rehydrate: %d restored, %d filled, %d already present", restored, filled, present), sdk.LogLevelDebug)
	}
	return modified, nil
}

// heuristicFill derives a stand-in intent for a history tool call whose real
// intent was never captured. Its only job is presence — preventing an
// "i"-less precedent — but it carries the call's primary argument so it reads
// as a plausible (if mediocre) example rather than a literal token the model
// might copy verbatim.
//
// PROMPT-CACHE COMPLIANCE: the fill MUST be a pure function of (tool name,
// args). It previously also mixed in a snippet of the latest user message,
// which changes every turn — so the same historical call re-serialized to
// different bytes each request, busting the provider prompt cache (OpenAI
// exact-prefix, Anthropic breakpoint hash) from that message onward. Do not
// reintroduce any per-request input here.
func heuristicFill(name string, args map[string]any) string {
	subject := name
	keys := make([]string, 0, len(args))
	for k := range args {
		if k != intentField {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := args[k].(string); ok && s != "" {
			subject = truncateRunes(s, 80)
			break
		}
	}
	return "what " + subject + " shows"
}

// truncateRunes shortens s to at most n runes, never splitting a rune.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// contentKey derives an opaque cache key from a tool call's name and arguments,
// excluding "i". Go's json.Marshal sorts map keys, so the encoding is
// canonical before ContentAddressedCacheKey hashes it: the response side
// (which strips "i") and the request side (where "i" is already absent)
// produce the same key for the same logical call.
// This is only the input-binding component of an occurrence key; it is never
// sufficient by itself to identify an intent.
func contentKey(name string, args map[string]any) string {
	cp := make(map[string]any, len(args))
	for k, v := range args {
		if k == intentField {
			continue
		}
		cp[k] = v
	}
	// Encode as a JSON array to keep the name/map boundary explicit, then hash
	// the canonical bytes. Raw arguments can contain paths, commands, source,
	// and user data; they must not become Redis key names or defeat local-cache
	// size accounting.
	b, _ := json.Marshal([]any{name, cp})
	return sdk.ContentAddressedCacheKey("intent/content/v1", string(b))
}

func conversationID(req *pbv1.ChatRequest) string {
	id, _ := conversationIdentity(req)
	return id
}

func conversationIdentity(req *pbv1.ChatRequest) (string, string) {
	if req == nil || len(req.ToranaMetaJson) == 0 {
		return "", "missing_conversation"
	}
	var meta struct {
		ConversationID string `json:"_conversation_id"`
	}
	if json.Unmarshal(req.ToranaMetaJson, &meta) != nil {
		return "", "malformed_torana_meta"
	}
	if meta.ConversationID == "" {
		return "", "missing_conversation"
	}
	return meta.ConversationID, ""
}

// Without conversation and call identity a cached value cannot safely be
// attributed to this historical occurrence. Bind inputs too, so reusing an ID
// for a different tool or arguments cannot restore an unrelated intent.
// The host conversation label hashes the leading system messages and first user
// message; changing that prefix rotates the key. Model/tool-definition changes
// alone are excluded (unless the harness renders them into the system prompt).
// Equal prefixes can share a label, so retained call identity is also essential.
func occurrenceKey(conversation, id, name string, args map[string]any) string {
	if conversation == "" || id == "" {
		return ""
	}
	return sdk.ContentAddressedCacheKey("intent/occurrence/v2", conversation, id, contentKey(name, args))
}

// decodeJSONObject preserves every JSON number lexeme as json.Number. These
// objects are later re-encoded after adding or removing the intent field, so a
// float64 decode would silently change large integer tool arguments and schema
// constraints. It also rejects trailing documents like json.Unmarshal does.
func decodeJSONObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON document")
		}
		return nil, err
	}
	return out, nil
}

// ==========================================================================
// Schema injection
// ==========================================================================

func injectIntentSchema(req *pbv1.ChatRequest) (bool, error) {
	modified := false
	for _, tool := range req.Tools {
		if len(tool.ParametersJson) == 0 {
			continue
		}
		params, err := decodeJSONObject(tool.ParametersJson)
		if err != nil {
			continue
		}
		if params["type"] == nil {
			params["type"] = "object"
		}
		props, _ := params["properties"].(map[string]any)
		// Whether the tool NAMED any arguments, recorded before we create the
		// map. It decides whether closing the schema is safe.
		//
		// len > 0, not != nil: in JSON Schema {"type":"object"} and
		// {"type":"object","properties":{}} are the same schema — both permit
		// any property — so treating the second as "the author named their
		// arguments" would close a tool that accepts anything, leaving one
		// that accepts nothing but "i". That is the exact failure this guard
		// exists to prevent, reached through the guard itself.
		declaredProps := len(props) > 0
		if props == nil {
			props = make(map[string]any)
			params["properties"] = props
		}

		// A tool that natively declares "i" (omp's tools do — the harness
		// adopted the intent field itself) keeps its structural contract:
		// required/optionality and additionalProperties are never touched,
		// and the response side never strips the value (that's what hadI
		// records). Only the DESCRIPTION is upgraded to the example-carrying
		// form — advisory prose, not contract, and measured markedly better
		// at producing goal-tied intents (omp's native "concise intent"
		// yielded action-labels like "Map repo structure", which starve the
		// compactors' keyword extraction).
		if existing, exists := props[intentField]; exists {
			// A refusal to record the marker is a request-side contract
			// defect: fail the hook so failure_mode applies rather than
			// stripping "i" on the response side of a tool we promised to
			// preserve.
			if err := sdk.MetaSet("hadI:"+tool.Name, "true"); err != nil {
				return false, fmt.Errorf("intent: hadI meta_set refused for %s: %v", tool.Name, err)
			}
			if m, ok := existing.(map[string]any); ok {
				m["description"] = intentDescription
			}
		} else {
			props[intentField] = map[string]any{
				"type":        "string",
				"description": intentDescription,
			}

			required, _ := params["required"].([]any)
			found := false
			for _, r := range required {
				if s, ok := r.(string); ok && s == intentField {
					found = true
					break
				}
			}
			if !found {
				params["required"] = append(required, intentField)
			}

			// Closing the schema is only this plugin's call when the tool
			// already described its arguments by name and did not ask to stay
			// open. Two cases where it is not:
			//
			//   - the author declared an open map, deliberately accepting
			//     free-form keys. Closing it is strictly stricter than written.
			//   - the tool declared no properties at all, so it accepted
			//     anything. Closing it after adding "i" leaves a tool that
			//     accepts nothing BUT "i" — which breaks the tool outright.
			//
			// Injecting the field stays fine in both cases; forbidding
			// everything else does not.
			openMap := false
			switch ap := params["additionalProperties"].(type) {
			case bool:
				openMap = ap
			case map[string]any:
				openMap = true
			}
			if declaredProps && !openMap {
				params["additionalProperties"] = false
			}
		}

		newJSON, err := json.Marshal(params)
		if err == nil && string(newJSON) != string(tool.ParametersJson) {
			tool.ParametersJson = newJSON
			modified = true
		}
	}
	return modified, nil
}

func hasFunctionTool(req *pbv1.ChatRequest) bool {
	if req == nil {
		return false
	}
	for _, tool := range req.Tools {
		if tool != nil && tool.InvocationKind == pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FUNCTION {
			return true
		}
	}
	return false
}

// injectSystemPrompt appends the "i" convention with a one-line example
// TRANSCRIPT embedded in the system prompt — the winning strategy from the
// Jul 16 experiments: it matches few-shot messages on intent quality with
// zero conversation contamination and no per-request message overhead.
// addendum is the "i" convention text appended to the system prompt (see
// injectSystemPrompt for the strategy notes). Package-level so the tests pin
// the exact production bytes.
const addendum = "\n\nEvery tool call has an \"i\" field: the underlying question the call " +
	"helps answer, never the action taken. Example of a good call:\n" +
	"  read_file(path=\"src/pricing.ts\", i=\"Which table maps locale to currency, to find why EU shows USD\")\n" +
	"Example of a BAD value: i=\"reading pricing.ts\" (action description — discarded)."

func injectSystemPrompt(req *pbv1.ChatRequest) (bool, error) {
	for _, msg := range req.Messages {
		if msg.Role != "system" {
			continue
		}
		// Append to the LAST text block of the system prompt via the
		// PROVENANCE-AWARE helper: a real change clears the text block's
		// signature AND any trailing-signature carrier whose covered content
		// changed; a byte-identical no-op preserves every token.
		for i := len(msg.Blocks) - 1; i >= 0; i-- {
			if t := msg.Blocks[i].GetText(); t != nil {
				if err := sdk.SetTextAt(msg, i, t.Text+addendum); err != nil {
					return false, fmt.Errorf("intent: system prompt append: %w", err)
				}
				return true, nil
			}
		}
		// A system message with NO text block: the SDK's valid no-text path
		// appends one at the end (removing a final trailing carrier first —
		// content appended after the token's covered scope is stale).
		if err := sdk.ReplaceAllText(msg, addendum); err != nil {
			return false, fmt.Errorf("intent: system prompt no-text append: %w", err)
		}
		return true, nil
	}
	req.Messages = append([]*pbv1.Message{{
		Role: "system",
		Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{
			Text: &pbv1.RequestTextBlock{Text: "[SYSTEM]" + addendum},
		}}},
	}}, req.Messages...)
	return true, nil
}
