// otel emits request-shape metrics on the way in and, on the way out, the
// per-request signals the host exposes: latency, upstream status class, and
// provider-reported token usage. Core ops metrics the host can observe more
// reliably (every response, including vetoes) are also emitted host-side (see
// internal/metrics); the plugin-side series exist so operators can slice by
// whatever labels plugins add.
//
// The response hook is dispatched for mutable JSON responses, for
// OBSERVATIONAL stream-shaped dispatches (mutable=false, Message=nil, with
// completed status/duration/usage facts), and for upstream-error dispatches —
// the port reports the same factual metrics for all of them and never
// requires Message. UpstreamStatus == 0 means unobserved: genuinely present
// facts are still emitted, but no status_class is claimed on ANY series; a
// positive status adds the same class to every series.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		labels := map[string]string{"model_family": modelFamily(req.Model)}
		sdk.EmitMetric("torana_plugin_requests_total", sdk.MetricCounter, 1, labels)
		sdk.EmitMetric("torana_plugin_request_messages", sdk.MetricHistogram, float64(len(req.Messages)), labels)
		sdk.EmitMetric("torana_plugin_request_tools", sdk.MetricHistogram, float64(len(req.Tools)), labels)
		return sdk.PassRequest(), nil
	})

	sdk.OnAfterResponse(func(ctx context.Context, resp *pbv2.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		for _, m := range responseMetrics(resp) {
			sdk.EmitMetric(m.Name, m.Kind, m.Value, m.Labels)
		}
		return sdk.PassResponse(), nil
	})

	// Serve a tiny status page at /_torana/plugin/otel/.
	// This demonstrates the run_on_http_request ABI: the page is intentionally
	// minimal — a proof of the per-plugin HTTP namespace, not a real dashboard.
	sdk.OnHTTPRequest(func(ctx context.Context, req *pbv2.HttpRequest) (sdk.HTTPResult, error) {
		switch req.Path {
		case "/agent/status":
			hdrsJSON, _ := json.Marshal(map[string][]string{
				"Content-Type": {"application/json"},
			})
			return sdk.ServeHTTP(&pbv2.HttpResponse{
				Status:      200,
				HeadersJson: hdrsJSON,
				Body:        []byte(`{"plugin":"otel","status":"ready","capabilities":["request_metrics","response_metrics","token_metrics"]}`),
			}), nil
		case "/":
			body := []byte(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Torana otel plugin</title></head>
<body><h1>Torana otel plugin</h1></body>
</html>`)
			hdrsJSON, _ := json.Marshal(map[string][]string{
				"Content-Type": {"text/html; charset=utf-8"},
			})
			return sdk.ServeHTTP(&pbv2.HttpResponse{
				Status:      200,
				HeadersJson: hdrsJSON,
				Body:        body,
			}), nil
		default:
			// Unknown paths pass to the host, which answers 404.
			return sdk.PassHTTP(), nil
		}
	})
}

func statusClass(status int32) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return fmt.Sprintf("%d", status)
	}
}

// withLabel returns a copy of base with one extra label. A copy, because the
// base map is emitted with several series: mutating it would leak "direction"
// onto every metric emitted after the first token count.
func withLabel(base map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[key] = value
	return out
}

// emission is one metric series, built but not yet sent.
type emission struct {
	Name   string
	Kind   int32
	Value  float64
	Labels map[string]string
}

// responseMetrics builds every series for a completed response dispatch —
// mutable JSON, observational stream-shaped (Message == nil), and
// upstream-error shapes alike. statusClass is added to EVERY series only when
// a status was actually observed (UpstreamStatus > 0); status zero emits the
// genuinely present facts (a response happened, duration, usage) with NO
// status_class anywhere — labelling an unobserved outcome would invent a
// measurement.
func responseMetrics(resp *pbv2.ChatResponse) []emission {
	if resp == nil {
		// No response facts at all: the only honest series is that a response
		// happened, with no status_class.
		return []emission{{"torana_plugin_responses_total", sdk.MetricCounter, 1, map[string]string{}}}
	}

	labels := map[string]string{"model_family": modelFamily(resp.Model)}
	hasStatus := resp.UpstreamStatus > 0
	if hasStatus {
		labels["status_class"] = statusClass(resp.UpstreamStatus)
	}

	out := []emission{
		{"torana_plugin_responses_total", sdk.MetricCounter, 1, labels},
		{"torana_plugin_request_duration_ms", sdk.MetricHistogram, float64(resp.DurationMs), labels},
	}
	if resp.Usage != nil {
		if resp.Usage.InputTokens > 0 {
			out = append(out, emission{"torana_plugin_tokens", sdk.MetricCounter,
				float64(resp.Usage.InputTokens), withLabel(labels, "direction", "input")})
		}
		if resp.Usage.OutputTokens > 0 {
			out = append(out, emission{"torana_plugin_tokens", sdk.MetricCounter,
				float64(resp.Usage.OutputTokens), withLabel(labels, "direction", "output")})
		}
	}
	return out
}

// modelFamily keeps client-controlled model names out of metric labels. The
// returned vocabulary is finite, so sending a fresh model name per request
// cannot create an unbounded number of OTel series.
func modelFamily(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, family := range modelFamilies {
		for _, prefix := range family.prefixes {
			if strings.HasPrefix(model, prefix) {
				return family.name
			}
		}
	}
	return "other"
}

var modelFamilies = []struct {
	name     string
	prefixes []string
}{
	{name: "claude", prefixes: []string{"claude"}},
	{name: "openai", prefixes: []string{"gpt", "chatgpt", "o1", "o3", "o4"}},
	{name: "gemini", prefixes: []string{"gemini"}},
	{name: "deepseek", prefixes: []string{"deepseek"}},
	{name: "llama", prefixes: []string{"llama", "meta-llama"}},
	{name: "mistral", prefixes: []string{"mistral", "mixtral", "codestral"}},
	{name: "qwen", prefixes: []string{"qwen"}},
	{name: "command", prefixes: []string{"command", "cohere"}},
	{name: "grok", prefixes: []string{"grok"}},
}
