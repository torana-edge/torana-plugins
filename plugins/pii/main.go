// pii scans tool results (grep/bash/etc. output) before they are forwarded to
// the cloud upstream. A deterministic regex pre-filter catches high-precision
// categories; anything else is sent to a configured LOCAL model for contextual
// detection. If PII is found the request is vetoed (env.block_request) with an
// actionable, value-free error so the upstream model can adjust next turn.
//
// # v2 semantics
//
//   - Structured content is COMPLETE or explicitly unscannable: the scan
//     composes scalar Content with every wire-order text part of
//     ContentPartsJson (newline-separated, stable line numbers); malformed
//     JSON, a non-array value, malformed text parts, and any provider-visible
//     part type the text scanner cannot inspect are SCAN FAILURES driven by
//     on_error — never clean, never cached. An empty but valid collection is
//     distinct from unsupported content.
//   - Blocking is an ATTRIBUTED SIDE EFFECT: the verdict goes through
//     sdk.BlockRequest and the hook returns PassRequest — no content
//     replacement, no write grant. The host short-circuits downstream plugins
//     and never reaches upstream.
//   - max_scan_bytes is a BYTE budget with rune-safe boundary repair (the
//     old "chars" name was a lie); zero is unbounded.
//   - provider and model are a PAIR: both absent = regex-only; both present =
//     regex + model scan; exactly one present is an invalid scanner
//     configuration driven through on_error with NO offload call.
//   - Cache reads follow the approved classes: NOT_FOUND and present-empty
//     rescan; advisory refusals rescan; contract refusals and malformed
//     frames error the hook. Cache writes are best-effort.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

const cleanCachePrefix = "pii_clean:v2"

type piiConfig struct {
	Provider     string   `json:"provider"`       // local-model provider (required to enable the model scan)
	Model        string   `json:"model"`          // model name for the scan
	Tools        []string `json:"tools"`          // tool-name allowlist; empty or ["*"] = all tool results
	OnError      string   `json:"on_error"`       // "block" (default, fail-closed) | "allow" (fail-open)
	MaxScanBytes int      `json:"max_scan_bytes"` // cap on model-scan input bytes; 0 = unbounded
}

var (
	cfgOnce sync.Once
	cfg     piiConfig
)

// parseConfig is the pure config decoder; loadConfig installs its result into
// the process-global state exactly once. The host validates config against
// schema.json at write time.
func parseConfig(raw string) piiConfig {
	c := piiConfig{OnError: "block"}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	if c.OnError == "" {
		c.OnError = "block"
	}
	return c
}

func loadConfig() {
	cfgOnce.Do(func() { cfg = parseConfig(sdk.PluginConfig()) })
}

// resetConfigForTest restores every config global so a test row can install a
// fresh config. Production never calls it; the once-only loader is unchanged.
func resetConfigForTest() {
	cfgOnce = sync.Once{}
	cfg = piiConfig{}
}

// scannerPairValid reports whether the provider/model configuration is a
// coherent pair: both absent (regex-only) or both present. Exactly one
// present is an invalid scanner configuration.
func (c piiConfig) scannerPairValid() bool {
	return (c.Provider == "") == (c.Model == "")
}

// High-precision patterns — deterministic, no model call, exact line numbers,
// and they still catch obvious PII when the local model is unavailable.
var piiPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"email", regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)},
	{"us_ssn", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{"aws_access_key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
}

type finding struct {
	Type string
	Line int
}

// textPartType is the only ContentPartsJson part type the text scanner can
// inspect. Anything else that is provider-visible is unscannable.
const textPartType = "text"

// extraction is the result of composing a tool message's scannable content.
type extraction struct {
	// text is every INSPECTABLE string: the scalar Content plus all valid
	// text parts in wire order, newline-separated so line numbers are stable.
	// A deterministic regex run over this text can PROVE PII even when the
	// extraction is incomplete.
	text string
	// complete is false when some provider-visible content could not be
	// inspected (malformed JSON, a non-array top-level value, JSON null,
	// malformed or non-text parts). Incomplete extractions are NEVER
	// model-scanned and NEVER cached: on_error governs them after the
	// deterministic scan of the retained text.
	complete bool
}

// extractScannable composes the scannable text of a tool message. When both
// Content and ContentPartsJson are populated, BOTH are included — a harmless
// scalar must never hide PII in parts. Within a syntactically valid array,
// bad/unsupported elements are skipped so valid text ELSEWHERE is retained;
// malformed top-level JSON still retains the scalar Content (both are
// incomplete, never clean).
func extractScannable(msg *pbv2.Message) extraction {
	if len(msg.ContentPartsJson) == 0 {
		return extraction{text: msg.Content, complete: true}
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(msg.ContentPartsJson, &raw); err != nil {
		// Malformed top-level JSON (or JSON null / a non-array): retain the
		// scalar, incomplete.
		return extraction{text: msg.Content, complete: false}
	}
	if raw == nil {
		// json.Unmarshal("null", &raw) succeeds with a nil slice: top-level
		// null is not a valid array.
		return extraction{text: msg.Content, complete: false}
	}
	text := msg.Content
	complete := true
	for _, r := range raw {
		var part struct {
			Type string          `json:"type"`
			Text json.RawMessage `json:"text"`
		}
		if err := json.Unmarshal(r, &part); err != nil {
			complete = false // malformed part object: un-inspectable, continue
			continue
		}
		if part.Type != textPartType {
			complete = false // provider-visible part the text scanner cannot inspect
			continue
		}
		if len(part.Text) == 0 || string(part.Text) == "null" {
			complete = false // missing or null text: not a string, continue
			continue
		}
		var textVal string
		if err := json.Unmarshal(part.Text, &textVal); err != nil {
			complete = false // malformed text part (non-string), continue
			continue
		}
		if text != "" {
			text += "\n"
		}
		text += textVal
	}
	return extraction{text: text, complete: complete}
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		loadConfig()

		// tool_call_id → tool name, so the allowlist can be applied even when
		// the tool-result message itself doesn't carry the name. A duplicated
		// or REUSED id is AMBIGUOUS regardless of name or order — a result
		// whose id appears more than once resolves to UNKNOWN, and the
		// unknown-name rule then errs toward scanning. An explicit
		// tool-result name stays authoritative.
		nameByID := map[string]string{}
		ambiguousID := map[string]bool{}
		for _, m := range req.Messages {
			for _, tc := range m.ToolCalls {
				if _, seen := nameByID[tc.Id]; seen {
					ambiguousID[tc.Id] = true
				} else {
					nameByID[tc.Id] = tc.Name
				}
			}
		}

		for _, msg := range req.Messages {
			if msg.Role != "tool" {
				continue
			}
			toolName := msg.ToolName
			if toolName == "" {
				toolName = nameByID[msg.ToolCallId]
				if ambiguousID[msg.ToolCallId] {
					toolName = "" // ambiguous id: err toward scanning via the unknown rule
				}
			}
			if !toolAllowed(toolName) {
				continue
			}
			ex := extractScannable(msg)

			// The deterministic scan runs FIRST over ALL retained text: a PII
			// fact Torana already detected blocks as pii_detected even when
			// the extraction is incomplete or the provider/model pair is
			// misconfigured — on_error governs the UNAVAILABLE contextual
			// scan, never a deterministic finding already made.
			if f := regexScan(ex.text); len(f) > 0 {
				sdk.BlockRequest(422, "pii_detected", blockMessage(toolName, f))
				return sdk.PassRequest(), nil
			}
			if !ex.complete {
				// Incomplete extraction: never model-scanned, never cached;
				// on_error governs the uninspectable remainder.
				if failClosed() {
					sdk.BlockRequest(422, "pii_scan_failed",
						fmt.Sprintf("PII scan unavailable for %s; request blocked (fail-closed). Retry, or set pii.on_error=\"allow\" to forward unscanned.",
							toolLabel(toolName)))
					return sdk.PassRequest(), nil
				}
				continue
			}
			if ex.text == "" {
				continue // a valid empty tool result: nothing to scan, nothing to cache
			}
			// Skip results cleared on a prior turn (avoids re-scanning history).
			cacheKey := piiCleanCacheKey(msg, toolName)
			cached, herr, err := sdk.CacheGet(cacheKey)
			if err != nil {
				return sdk.RequestResult{}, err
			}
			if herr != nil && !sdk.IsNotFound(herr) {
				if herr.Code == pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED || herr.Code == pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE {
					// Advisory: decline the cache, still scan.
				} else {
					return sdk.RequestResult{}, fmt.Errorf("pii: cache_get refused: %s", herr.Message)
				}
			} else if herr == nil && cached != "" {
				continue
			}

			findings, err := scan(ex.text, toolName)
			if err != nil {
				var sf *scannerFailure
				if !errors.As(err, &sf) {
					// Contract refusal, malformed frame, or protocol defect:
					// error the hook regardless of on_error.
					return sdk.RequestResult{}, err
				}
				// Scanner failure. Fail-closed by default.
				if cfg.OnError == "allow" {
					continue
				}
				sdk.BlockRequest(422, "pii_scan_failed",
					fmt.Sprintf("PII scan unavailable for %s; request blocked (fail-closed). Retry, or set pii.on_error=\"allow\" to forward unscanned.",
						toolLabel(toolName)))
				return sdk.PassRequest(), nil
			}
			if len(findings) > 0 {
				sdk.BlockRequest(422, "pii_detected", blockMessage(toolName, findings))
				return sdk.PassRequest(), nil
			}
			// Complete extraction was scannable and clean: cache the verdict.
			_, _ = sdk.CacheSet(cacheKey, "1")
		}
		return sdk.PassRequest(), nil
	})
}

func failClosed() bool { return cfg.OnError != "allow" }

// scannerFailure marks a SCANNER failure — advisory refusals, an invalid
// provider/model pair, or an unparseable model verdict — which the plugin's
// own on_error policy governs. Anything else (contract refusals, malformed
// frames, protocol defects) is a plain error and errors the hook regardless
// of on_error.
type scannerFailure struct{ msg string }

func (e *scannerFailure) Error() string { return e.msg }

func piiCleanCacheKey(msg *pbv2.Message, toolName string) string {
	policy, _ := json.Marshal(struct {
		Version      int      `json:"version"`
		Provider     string   `json:"provider"`
		Model        string   `json:"model"`
		Tools        []string `json:"tools"`
		OnError      string   `json:"on_error"`
		MaxScanBytes int      `json:"max_scan_bytes"`
	}{
		Version:      3,
		Provider:     cfg.Provider,
		Model:        cfg.Model,
		Tools:        cfg.Tools,
		OnError:      cfg.OnError,
		MaxScanBytes: cfg.MaxScanBytes,
	})
	// ContentAddressedCacheKey length-prefixes every input, so arbitrary
	// strings (ids, names, content) cannot be joined ambiguously.
	return sdk.ContentAddressedCacheKey(cleanCachePrefix,
		msg.ToolCallId, toolName, msg.Content, string(msg.ContentPartsJson), string(policy))
}

func toolAllowed(name string) bool {
	if len(cfg.Tools) == 0 {
		return true // scan every tool result by default
	}
	for _, t := range cfg.Tools {
		if t == "*" || strings.EqualFold(t, name) {
			return true
		}
	}
	// Unknown tool name with an allowlist set: err toward scanning (don't miss PII).
	return name == ""
}

// scan runs the local-model path. The deterministic pre-filter already ran
// over the retained text in the hook (before completeness and pair checks),
// so a deterministic finding can never be demoted by on_error or a
// misconfigured pair.
func scan(content, toolName string) ([]finding, error) {
	if !cfg.scannerPairValid() {
		// Exactly one of provider/model: an invalid scanner configuration,
		// driven through on_error — with NO offload call.
		return nil, &scannerFailure{"pii scanner configuration invalid: set both provider and model, or neither"}
	}
	if cfg.Provider == "" {
		// No local model configured: regex-only mode. Nothing more to check.
		return nil, nil
	}
	return modelScan(content, toolName)
}

func regexScan(content string) []finding {
	var out []finding
	seen := map[string]bool{}
	for i, line := range strings.Split(content, "\n") {
		for _, p := range piiPatterns {
			if p.re.MatchString(line) {
				key := fmt.Sprintf("%s:%d", p.name, i+1)
				if !seen[key] {
					seen[key] = true
					out = append(out, finding{Type: p.name, Line: i + 1})
				}
			}
		}
	}
	return out
}

const piiSystemPrompt = `You are a PII detector. Examine the tool output and decide whether it contains personally identifiable information or secrets: emails, phone numbers, physical addresses, government IDs (e.g. SSNs), credit-card or bank numbers, API keys, passwords, private keys, or access tokens.

Respond with ONLY a JSON object and no other text:
{"pii": true|false, "findings": [{"type": "<category>", "line": <1-based line number>}]}

Never include the actual PII values — only the category and line number. If there is no PII, respond {"pii": false, "findings": []}.`

func modelScan(content, toolName string) ([]finding, error) {
	scanContent := content
	if cfg.MaxScanBytes > 0 && len(scanContent) > cfg.MaxScanBytes {
		// Byte budget with rune-safe boundary repair: a mid-rune cut would
		// silently corrupt the last character of the text a PII detector is
		// about to read.
		scanContent = truncHead(scanContent, cfg.MaxScanBytes)
	}
	payload, _ := json.Marshal(map[string]any{
		"provider":      cfg.Provider,
		"model":         cfg.Model,
		"system_prompt": piiSystemPrompt,
		"user_prompt":   "Tool: " + toolName + "\n\nOutput to scan:\n" + scanContent,
	})
	res, herr, err := sdk.HostCallExtension("torana_offload_completion", payload)
	if err != nil {
		// Malformed frame / transport / protocol defect: hook error.
		return nil, err
	}
	if herr != nil {
		// Advisory refusals are a scanner failure (on_error decides);
		// contract refusals are the caller's/host's defect — the hook errors
		// regardless of on_error.
		switch herr.Code {
		case pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE:
			return nil, &scannerFailure{"pii scan failed: " + herr.Message}
		default:
			return nil, fmt.Errorf("pii offload refused: %s", herr.Message)
		}
	}
	// The v2 offload result carries NO status field; refusals arrive only in
	// the framed error arm. An undecodable value arm is a protocol defect.
	var resp struct {
		Completion string `json:"completion"`
	}
	if json.Unmarshal(res, &resp) != nil {
		return nil, fmt.Errorf("pii scan: unparseable offload reply")
	}
	// The verdict SHAPE is validated explicitly: pii must be present,
	// non-null, and boolean; findings must be a documented array (or absent).
	// Malformed or contradictory shapes are scanner failures governed by
	// on_error — never clean, never cached.
	var verdict struct {
		PII      json.RawMessage `json:"pii"`
		Findings json.RawMessage `json:"findings"`
	}
	if json.Unmarshal([]byte(extractJSON(resp.Completion)), &verdict) != nil {
		return nil, &scannerFailure{"pii scan: unparseable verdict"}
	}
	if len(verdict.PII) == 0 || string(verdict.PII) == "null" {
		return nil, &scannerFailure{"pii scan: verdict missing or null pii"}
	}
	var pii bool
	if err := json.Unmarshal(verdict.PII, &pii); err != nil {
		return nil, &scannerFailure{"pii scan: verdict pii is not a boolean"}
	}
	var findings []struct {
		Type string `json:"type"`
		Line int    `json:"line"`
	}
	if len(verdict.Findings) > 0 {
		if string(verdict.Findings) == "null" {
			return nil, &scannerFailure{"pii scan: verdict findings is null, not an array"}
		}
		if err := json.Unmarshal(verdict.Findings, &findings); err != nil {
			return nil, &scannerFailure{"pii scan: verdict findings is not an array"}
		}
	}
	if !pii && len(findings) > 0 {
		return nil, &scannerFailure{"pii scan: contradictory verdict (pii false with findings)"}
	}
	if !pii {
		return nil, nil
	}
	out := make([]finding, 0, len(findings))
	for _, f := range findings {
		out = append(out, finding{Type: f.Type, Line: f.Line})
	}
	if len(out) == 0 {
		out = append(out, finding{Type: "unspecified"})
	}
	return out, nil
}

// extractJSON pulls the first complete {...} object out of a model reply that
// may be wrapped in prose or code fences.
//
// Brace counting, skipping string literals so a "{" inside a value does not
// shift the depth, and their escapes so a \" does not end the string early.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return s
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	// Unbalanced: hand back what follows the first brace and let the caller's
	// json.Unmarshal report it, rather than inventing a closing brace.
	return s[start:]
}

// knownCategories is the fixed, documented set of useful finding categories.
// A model-controlled category is NEVER echoed verbatim (the prompt asking the
// model not to echo secrets is not a security boundary); every unknown value
// maps to "unspecified".
var knownCategories = map[string]bool{
	"email": true, "phone": true, "address": true, "government_id": true,
	"ssn": true, "us_ssn": true, "credit_card": true, "bank_number": true,
	"api_key": true, "password": true, "private_key": true, "access_token": true,
	"aws_access_key": true, "unspecified": true,
}

// normalizeCategory maps a model-supplied category to the safe documented set.
func normalizeCategory(cat string) string {
	lower := strings.ToLower(strings.TrimSpace(cat))
	aliases := map[string]string{
		"ssn":           "us_ssn",
		"aws_key":       "aws_access_key",
		"aws key":       "aws_access_key",
		"api key":       "api_key",
		"api-key":       "api_key",
		"accesskey":     "access_token",
		"phone number":  "phone",
		"email address": "email",
	}
	if canon, ok := aliases[lower]; ok {
		return canon
	}
	if knownCategories[lower] {
		return lower
	}
	return "unspecified"
}

func blockMessage(toolName string, findings []finding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		cat := normalizeCategory(f.Type)
		line := f.Line
		if line < 1 || line > 1_000_000 {
			line = 0 // clamp: only display sane 1-based line numbers
		}
		if line > 0 {
			parts = append(parts, fmt.Sprintf("%s (line %d)", cat, line))
		} else {
			parts = append(parts, cat)
		}
	}
	return fmt.Sprintf(
		"Blocked: PII detected in %s and NOT sent upstream. Found: %s. "+
			"Do not resend this content; reformulate to exclude or redact these values before returning the tool result.",
		toolLabel(toolName), strings.Join(parts, ", "))
}

// toolLabel displays a tool name ONLY after conservative validation: a short
// identifier of word characters, dots, dashes, and underscores. Anything else
// (or an unknown/empty name) reads as "a tool result". The raw tool-call ID
// is never included merely for diagnostics.
func toolLabel(toolName string) string {
	if len(toolName) > 0 && len(toolName) <= 64 && safeNameRe.MatchString(toolName) {
		return fmt.Sprintf("`%s` output", toolName)
	}
	return "a tool result"
}

var safeNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// truncHead returns the longest prefix of s that is at most n bytes and does
// not split a rune.
func truncHead(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
