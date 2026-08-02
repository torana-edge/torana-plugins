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
	"crypto/sha256"
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

// extractScannable composes the scannable text of a tool message: scalar
// Content first, then every text part of ContentPartsJson in wire order, each
// separated by a newline so line numbers are stable across shapes. When both
// are populated, BOTH are included — a harmless scalar must never hide PII in
// parts.
//
// scannable is false — and the result must be treated as a SCAN FAILURE, not
// a clean verdict — when the structured content cannot be completely
// inspected: malformed JSON, a non-array top-level value, a malformed text
// part, or any provider-visible part type the text scanner cannot inspect.
// A valid empty collection (no provider-visible content) is scannable with
// empty text.
func extractScannable(msg *pbv2.Message) (text string, scannable bool) {
	var parts []string
	if len(msg.ContentPartsJson) == 0 {
		return msg.Content, true
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(msg.ContentPartsJson, &raw); err != nil {
		return "", false // malformed JSON or non-array
	}
	for _, r := range raw {
		var part struct {
			Type string          `json:"type"`
			Text json.RawMessage `json:"text"`
		}
		if err := json.Unmarshal(r, &part); err != nil {
			return "", false // malformed part object
		}
		if part.Type != textPartType {
			return "", false // provider-visible part the text scanner cannot inspect
		}
		var textVal string
		if err := json.Unmarshal(part.Text, &textVal); err != nil {
			return "", false // malformed text part (missing or non-string text)
		}
		parts = append(parts, textVal)
	}
	if msg.Content != "" {
		parts = append([]string{msg.Content}, parts...)
	}
	return strings.Join(parts, "\n"), true
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		loadConfig()

		// tool_call_id → tool name, so the allowlist can be applied even when
		// the tool-result message itself doesn't carry the name.
		nameByID := map[string]string{}
		for _, m := range req.Messages {
			for _, tc := range m.ToolCalls {
				nameByID[tc.Id] = tc.Name
			}
		}

		for _, msg := range req.Messages {
			if msg.Role != "tool" {
				continue
			}
			toolName := msg.ToolName
			if toolName == "" {
				toolName = nameByID[msg.ToolCallId]
			}
			if !toolAllowed(toolName) {
				continue
			}
			// Complete extraction FIRST: unscannable content is a scan
			// failure, never a clean verdict, never cached.
			content, scannable := extractScannable(msg)
			if !scannable {
				if failClosed() {
					sdk.BlockRequest(422, "pii_scan_failed",
						fmt.Sprintf("PII scan unavailable for %s; request blocked (fail-closed). Retry, or set pii.on_error=\"allow\" to forward unscanned.",
							toolLabel(toolName, msg.ToolCallId)))
					return sdk.PassRequest(), nil
				}
				continue
			}
			if content == "" {
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

			findings, err := scan(content, toolName)
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
						toolLabel(toolName, msg.ToolCallId)))
				return sdk.PassRequest(), nil
			}
			if len(findings) > 0 {
				sdk.BlockRequest(422, "pii_detected", blockMessage(toolName, msg.ToolCallId, findings))
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
	digest := sha256.New()
	_, _ = digest.Write([]byte(msg.ToolCallId))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(toolName))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(msg.Content)) // EXACT scalar content
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(msg.ContentPartsJson) // EXACT structured bytes
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(policy)
	return fmt.Sprintf("%s:%x", cleanCachePrefix, digest.Sum(nil))
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

// scan runs the deterministic pre-filter first, then the local model.
func scan(content, toolName string) ([]finding, error) {
	if f := regexScan(content); len(f) > 0 {
		return f, nil
	}
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
	var verdict struct {
		PII      bool `json:"pii"`
		Findings []struct {
			Type string `json:"type"`
			Line int    `json:"line"`
		} `json:"findings"`
	}
	if json.Unmarshal([]byte(extractJSON(resp.Completion)), &verdict) != nil {
		return nil, &scannerFailure{"pii scan: unparseable verdict"}
	}
	if !verdict.PII {
		return nil, nil
	}
	out := make([]finding, 0, len(verdict.Findings))
	for _, f := range verdict.Findings {
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

func blockMessage(toolName, id string, findings []finding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		if f.Line > 0 {
			parts = append(parts, fmt.Sprintf("%s (line %d)", f.Type, f.Line))
		} else {
			parts = append(parts, f.Type)
		}
	}
	return fmt.Sprintf(
		"Blocked: PII detected in %s and NOT sent upstream. Found: %s. "+
			"Do not resend this content; reformulate to exclude or redact these values before returning the tool result.",
		toolLabel(toolName, id), strings.Join(parts, ", "))
}

func toolLabel(toolName, id string) string {
	switch {
	case toolName != "" && id != "":
		return fmt.Sprintf("`%s` output (%s)", toolName, id)
	case toolName != "":
		return fmt.Sprintf("`%s` output", toolName)
	case id != "":
		return fmt.Sprintf("tool result (%s)", id)
	default:
		return "a tool result"
	}
}

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
