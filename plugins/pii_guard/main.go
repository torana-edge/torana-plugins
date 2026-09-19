// pii_guard blocks high-confidence PII and secret patterns in tool results
// before those results are forwarded to the configured model provider. It is
// deliberately deterministic: no model service, network call, or clean-verdict
// cache is involved.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

type guardConfig struct {
	Tools []string `json:"tools"` // empty or ["*"] scans every tool result
}

var (
	configMu     sync.Mutex
	configLoaded bool
	config       guardConfig
)

func loadConfig() error {
	configMu.Lock()
	defer configMu.Unlock()
	if configLoaded {
		return nil
	}
	raw, err := sdk.PluginConfig()
	if err != nil {
		return fmt.Errorf("pii_guard: load plugin config: %w", err)
	}
	var next guardConfig
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &next) // host validates schema on write
	}
	config = next
	configLoaded = true
	return nil
}

func resetConfigForTest() {
	configMu.Lock()
	defer configMu.Unlock()
	configLoaded = false
	config = guardConfig{}
}

type sensitivePattern struct {
	category        string
	requiredLiteral string
	re              *regexp.Regexp
}

// Patterns are intentionally high precision. An unmatched value is not a
// declaration that the tool result is clean; use the model-backed pii plugin
// when contextual coverage is required.
var sensitivePatterns = []sensitivePattern{
	{"email", "@", regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)},
	{"us_ssn", "-", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{"aws_access_key", "AKIA", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"private_key", "-----BEGIN ", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"api_key", "sk-", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|ant-)?[A-Za-z0-9_-]{20,}\b`)},
	{"api_key", "sk_", regexp.MustCompile(`\bsk_(?:live|test)_[A-Za-z0-9_]{16,}\b`)},
	{"api_key", "rk_", regexp.MustCompile(`\brk_(?:live|test)_[A-Za-z0-9_]{16,}\b`)},
	{"access_token", "gh", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{20,255})\b`)},
}

type finding struct {
	category string
	line     int
}

const maxReportedFindings = 20

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv1.ChatRequest) (sdk.RequestResult, error) {
		if err := loadConfig(); err != nil {
			return sdk.RequestResult{}, err
		}

		nameByID := map[string]string{}
		ambiguousID := map[string]bool{}
		for _, message := range req.Messages {
			for _, call := range sdk.ToolCalls(message) {
				if _, exists := nameByID[call.Id]; exists {
					ambiguousID[call.Id] = true
				} else {
					nameByID[call.Id] = call.Name
				}
			}
		}

		mutated := false
		replayed := map[[2]int]bool{}
		for messageIndex, message := range req.Messages {
			for _, result := range sdk.ToolResults(message) {
				didReplay, err := replayPrior(ctx, message, result)
				if err != nil {
					return sdk.RequestResult{}, fmt.Errorf("pii_guard: replay protected result: %w", err)
				}
				replayed[[2]int{messageIndex, result.Block}] = didReplay
				mutated = mutated || didReplay
			}
		}

		latest := trailingToolResultMessages(req.Messages)
		for messageIndex, message := range req.Messages {
			if !latest[messageIndex] {
				continue
			}
			for _, result := range sdk.ToolResults(message) {
				if replayed[[2]int{messageIndex, result.Block}] {
					continue
				}
				toolName := result.ToolName
				if toolName == "" {
					toolName = nameByID[result.ToolCallId]
					if ambiguousID[result.ToolCallId] {
						toolName = ""
					}
				}
				if !toolAllowed(toolName) {
					continue
				}
				findings := scanToolResult(result)
				if len(findings) == 0 {
					continue
				}
				if err := replaceAndRemember(ctx, message, result, blockMessage(toolName, findings)); err != nil {
					return sdk.RequestResult{}, fmt.Errorf("pii_guard: replace detected content: %w", err)
				}
				mutated = true
			}
		}
		if mutated {
			return sdk.ReplaceRequest(req), nil
		}
		return sdk.PassRequest(), nil
	})
}

func toolAllowed(name string) bool {
	if len(config.Tools) == 0 {
		return true
	}
	for _, configured := range config.Tools {
		if configured == "*" || strings.EqualFold(configured, name) {
			return true
		}
	}
	return name == "" // unknown tool names err toward scanning
}

func scanToolResult(result sdk.ToolResultView) []finding {
	var findings []finding
	line := 0
	for _, content := range result.Content {
		if content.UnknownKind != "" || len(content.CacheMarker) > 0 {
			continue
		}
		for textLine := range strings.SplitSeq(content.Text, "\n") {
			line++
			for _, pattern := range sensitivePatterns {
				if !strings.Contains(textLine, pattern.requiredLiteral) || !pattern.re.MatchString(textLine) {
					continue
				}
				findings = appendUnique(findings, finding{category: pattern.category, line: line})
				if len(findings) > maxReportedFindings {
					return findings
				}
			}
		}
	}
	return findings
}

func appendUnique(findings []finding, next finding) []finding {
	for _, existing := range findings {
		if existing == next {
			return findings
		}
	}
	return append(findings, next)
}

var safeToolName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func blockMessage(toolName string, findings []finding) string {
	n := min(len(findings), maxReportedFindings)
	parts := make([]string, 0, n)
	for _, item := range findings[:n] {
		parts = append(parts, fmt.Sprintf("%s (line %d)", item.category, item.line))
	}
	label := "a tool result"
	if len(toolName) <= 64 && safeToolName.MatchString(toolName) {
		label = fmt.Sprintf("`%s` output", toolName)
	}
	message := fmt.Sprintf("Sensitive output withheld before it reached the model. pii_guard detected %s in %s. Continue without the sensitive value; request a narrower read or skip the affected lines.", strings.Join(parts, ", "), label)
	if len(findings) > maxReportedFindings {
		message += " Additional findings omitted."
	}
	return message
}
