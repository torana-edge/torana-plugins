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
// cross-request cache (keyed by tool_call_id AND by tool name+args), and
// strips "i" back off so the agent harness never sees it.
//
// It exists as its own plugin so the compactors are independent consumers:
// run "intent" plus EITHER keyword_compactor (deterministic, local) OR
// compactor (cheap-model offload) — both read the same intent cache.
//
// The response side runs on the SDK's StreamHandler: tool-call fragments are
// buffered host-side (meta_append, under env.meta_set) and presented to
// OnToolCall as one complete call. This is a deliberate timing change from
// v1, which hand-rolled its own fragment maps: start/deltas are suppressed
// and an equivalent assembled start+delta+stop is emitted at block
// completion. Callback errors are consumed by StreamHandler for fail-open
// re-emission of the original block — a streamed response must never be
// truncated by a plugin failure.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
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
	cfgOnce  sync.Once
	fillMode = "heuristic"
)

// parseConfig is the pure config decoder; loadConfig installs its result into
// the process-global state exactly once. The host validates config against
// schema.json at write time, so an unmarshal failure here is unreachable in
// practice and falls back to defaults, matching v1.
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

func loadConfig() {
	cfgOnce.Do(func() { fillMode = parseConfig(sdk.PluginConfig()) })
}

// resetConfigForTest restores every config global so a test row can install a
// fresh config. Production never calls it; the once-only loader is unchanged.
func resetConfigForTest() {
	cfgOnce = sync.Once{}
	fillMode = "heuristic"
}

func init() {
	// ── Request side: teach the "i" convention ──────────────────────
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		if len(req.Tools) == 0 {
			return sdk.PassRequest(), nil
		}
		modified, err := injectIntentSchema(req)
		if err != nil {
			return sdk.RequestResult{}, err
		}
		modified = injectSystemPrompt(req) || modified
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
}

// handleToolCall extracts and caches the intent from one assembled tool call,
// then strips "i" (unless the tool natively declares it) so the harness never
// sees the field Torana injected.
//
// Pass-through is SEMANTIC: whenever the plugin does not actually delete a
// field, the ORIGINAL argument bytes and the bound signature must travel
// unchanged. JSON formatting or key order in the model's output is never a
// reason to rewrite the block.
func handleToolCall(call sdk.ToolCall) (sdk.ToolCallAction, error) {
	// Parse regardless of leading whitespace (json.Unmarshal accepts it);
	// invalid, non-object, and "null" arguments (args stays nil) are not
	// representable and pass the exact bytes.
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || args == nil {
		return sdk.PassToolCall(), nil
	}

	// Extract and cache intent. Phase 0 observability: count how often the
	// model actually follows the convention, per tool. FIELD PRESENCE and
	// CAPTURE VALIDITY are independent facts: only a non-empty string is a
	// usable intent, but ANY present "i" key is Torana's injected field
	// unless the tool natively declares it (decided by the hadI marker).
	labels := map[string]string{"tool": call.Name}
	rawI, hasIntent := args[intentField]
	intent, usable := rawI.(string)
	if usable && intent != "" {
		// Key by tool_call_id (works when the harness echoes IDs, e.g. most
		// OpenAI clients) AND by tool name+args content. The content key
		// survives harnesses that reassign tool_call_ids across turns (Claude
		// Code does), which is the only key rehydration can rely on since the
		// response-stream ID never reappears in later request history.
		//
		// CacheSet is best-effort: a refusal affects FUTURE compaction, not
		// the validity of this response, so it is logged and the current
		// tool call still completes. The host records the refusal itself.
		if herr, err := sdk.CacheSet(intentCacheKey+":"+call.ID, intent); err != nil || herr != nil {
			sdk.Log(fmt.Sprintf("intent: cache_set %s:%s refused: %v %v", intentCacheKey, call.ID, herr, err), sdk.LogLevelInfo)
		}
		if herr, err := sdk.CacheSet(contentKey(call.Name, args), intent); err != nil || herr != nil {
			sdk.Log(fmt.Sprintf("intent: cache_set content key refused: %v %v", herr, err), sdk.LogLevelInfo)
		}
		sdk.EmitMetric("torana_intent_captured_total", sdk.MetricCounter, 1, labels)
		// Debug visibility for dogfooding: intent QUALITY (goal vs action
		// description) is only judgeable by reading the values.
		sdk.Log(fmt.Sprintf("intent[%s %s]: %s", call.Name, call.ID, truncateRunes(intent, 160)), sdk.LogLevelDebug)
	} else {
		sdk.EmitMetric("torana_intent_absent_total", sdk.MetricCounter, 1, labels)
		sdk.Log(fmt.Sprintf("intent[%s %s]: ABSENT", call.Name, call.ID), sdk.LogLevelDebug)
	}

	// No "i" key at all: absent observability already emitted, exact pass
	// with NO marker lookup, marshal, or signature change.
	if !hasIntent {
		return sdk.PassToolCall(), nil
	}

	// Any present "i" value (string, empty, number, object, boolean, null)
	// is Torana's injected field unless the tool natively declares it. A
	// refusal to READ the hadI marker is a protocol failure (the key is only
	// written by this plugin's request side): log and return an error so
	// StreamHandler re-emits the original block — never a guess about
	// whether to strip.
	hadI := ""
	if call.Name != "" {
		var herr *pbv2.HostError
		var err error
		hadI, herr, err = sdk.MetaGet("hadI:" + call.Name)
		if err != nil || (herr != nil && !sdk.IsNotFound(herr)) {
			sdk.Log(fmt.Sprintf("intent: hadI meta_get refused: %v %v", herr, err), sdk.LogLevelInfo)
			return sdk.ToolCallAction{}, fmt.Errorf("intent: hadI meta_get failed: %v %v", herr, err)
		}
	}
	if hadI == "true" {
		// Native field: the original block (with "i" of ANY value and its
		// signature) passes byte-identical.
		return sdk.PassToolCall(), nil
	}

	// Injected "i" of ANY type/value: delete it, marshal the changed
	// object, and replace — the arguments changed, so StreamHandler clears
	// the bound signature.
	delete(args, intentField)
	modifiedJSON, err := json.Marshal(args)
	if err != nil {
		return sdk.PassToolCall(), nil
	}
	return sdk.ReplaceToolArguments(string(modifiedJSON)), nil
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
func rehydrateHistoryIntents(req *pbv2.ChatRequest) (bool, error) {
	loadConfig()
	restored, filled, present := 0, 0, 0
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
			var args map[string]any
			if len(tc.Arguments) == 0 {
				args = map[string]any{}
			} else if json.Unmarshal(tc.Arguments, &args) != nil || args == nil {
				// Unrepresentable history arguments: null (which decodes as a
				// nil map), arrays, scalars, malformed JSON. Leave them
				// unchanged — assigning into a nil map would panic.
				continue
			}
			if _, ok := args[intentField]; ok {
				present++
				continue // already carries "i"
			}
			// Look up by the content key (tool name + args). This is the only
			// key that survives harnesses reassigning tool_call_ids across
			// turns — the response-stream ID we cached under never reappears
			// in later request history.
			intent, herr, err := sdk.CacheGet(contentKey(tc.Name, args))
			if err != nil {
				return false, fmt.Errorf("intent: cache_get %q: %w", contentKey(tc.Name, args), err)
			}
			if herr != nil && !sdk.IsNotFound(herr) {
				return false, fmt.Errorf("intent: cache_get %q refused: %s", contentKey(tc.Name, args), herr.Message)
			}
			if herr == nil && intent != "" {
				// Bridge the real intent to this request's own tool_call_id so
				// the compactors' intent:<tool_call_id> lookup (keyed off the
				// tool RESULT message) works on harnesses that reassign IDs.
				if herr, err := sdk.CacheSet(intentCacheKey+":"+tc.Id, intent); err != nil || herr != nil {
					return false, fmt.Errorf("intent: cache_set %s:%s refused: %v %v", intentCacheKey, tc.Id, herr, err)
				}
				restored++
			} else {
				if fillMode == "off" {
					continue
				}
				// Filled values are injected into history only — never cached
				// and never bridged: the intent cache stays real-captured-only
				// so compaction quality is driven by real intents.
				intent = heuristicFill(tc.Name, args)
				sdk.EmitMetric("torana_intent_filled_total", sdk.MetricCounter, 1, map[string]string{"tool": tc.Name})
				sdk.Log(fmt.Sprintf("intent-fill[%s %s]: %s", tc.Name, tc.Id, intent), sdk.LogLevelDebug)
				filled++
			}
			args[intentField] = intent
			if b, err := json.Marshal(args); err == nil {
				// The view is a COPY: the mutation must land on the actual
				// tool-use block (position-addressed by the view).
				msg.Blocks[tc.Block].GetToolUse().ArgumentsJson = b
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

// contentKey derives a cache key from a tool call's name and arguments,
// excluding "i". Go's json.Marshal sorts map keys, so the encoding is
// canonical: the response side (which strips "i") and the request side (where
// "i" is already absent) produce the same key for the same logical call.
// Collisions (same tool + args, different intent) resolve last-write-wins,
// which is acceptable for a hint.
func contentKey(name string, args map[string]any) string {
	cp := make(map[string]any, len(args))
	for k, v := range args {
		if k == intentField {
			continue
		}
		cp[k] = v
	}
	// Encode as a JSON array so the key stays JSON-safe (no control-char
	// separator that would break the cache_set payload) while remaining
	// canonical — Go sorts the inner map's keys.
	b, _ := json.Marshal([]any{name, cp})
	return "intentc:" + string(b)
}

// ==========================================================================
// Schema injection
// ==========================================================================

func injectIntentSchema(req *pbv2.ChatRequest) (bool, error) {
	modified := false
	for _, tool := range req.Tools {
		if len(tool.ParametersJson) == 0 {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(tool.ParametersJson, &params); err != nil {
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
			if herr, err := sdk.MetaSet("hadI:"+tool.Name, "true"); err != nil || herr != nil {
				return false, fmt.Errorf("intent: hadI meta_set refused for %s: %v %v", tool.Name, herr, err)
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

// injectSystemPrompt appends the "i" convention with a one-line example
// TRANSCRIPT embedded in the system prompt — the winning strategy from the
// Jul 16 experiments: it matches few-shot messages on intent quality with
// zero conversation contamination and no per-request message overhead.
func injectSystemPrompt(req *pbv2.ChatRequest) bool {
	const addendum = "\n\nEvery tool call has an \"i\" field: the underlying question the call " +
		"helps answer, never the action taken. Example of a good call:\n" +
		"  read_file(path=\"src/pricing.ts\", i=\"Which table maps locale to currency, to find why EU shows USD\")\n" +
		"Example of a BAD value: i=\"reading pricing.ts\" (action description — discarded)."
	for _, msg := range req.Messages {
		if msg.Role != "system" {
			continue
		}
		// Append to the LAST text block of the system prompt (the ordered
		// body keeps the prompt text in text blocks; the flat Content is
		// gone).
		for i := len(msg.Blocks) - 1; i >= 0; i-- {
			if t := msg.Blocks[i].GetText(); t != nil {
				t.Text += addendum
				return true
			}
		}
		msg.Blocks = append(msg.Blocks, &pbv2.RequestBlock{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: addendum}}})
		return true
	}
	req.Messages = append([]*pbv2.Message{{
		Role: "system",
		Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{
			Text: &pbv2.RequestTextBlock{Text: "[SYSTEM]" + addendum},
		}}},
	}}, req.Messages...)
	return true
}
