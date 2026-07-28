package main

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

// otel emits request-shape metrics on the way in and, on the way out, the
// per-request signals the host exposes via ToranaMeta._response: latency,
// upstream status class, and provider-reported token usage. Core ops metrics
// the host can observe more reliably (every response, including vetoes) are
// also emitted host-side (see internal/metrics); the plugin-side series exist
// so operators can slice by whatever labels plugins add.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		labels := map[string]string{"model": req.Model}
		sdk.EmitMetric("torana_plugin_requests_total", sdk.MetricCounter, 1, labels)
		sdk.EmitMetric("torana_plugin_request_messages", sdk.MetricHistogram, float64(len(req.Messages)), labels)
		sdk.EmitMetric("torana_plugin_request_tools", sdk.MetricHistogram, float64(len(req.Tools)), labels)
		return nil, nil
	})

	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatRequest) (*pb.ChatRequest, error) {
		for _, m := range responseMetrics(resp.Model, parseResponseMeta(resp.ToranaMetaJson)) {
			sdk.EmitMetric(m.Name, m.Kind, m.Value, m.Labels)
		}
		return nil, nil
	})

	// Serve a tiny status page at /_torana/plugin/otel/.
	// This demonstrates the run_on_http_request ABI: the page is intentionally
	// minimal — a proof of the per-plugin HTTP namespace, not a real dashboard.
	sdk.OnHTTPRequest(func(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error) {
		if req.Path == "/agent/status" {
			hdrsJSON, _ := json.Marshal(map[string][]string{
				"Content-Type": {"application/json"},
			})
			return &pb.HttpResponse{
				Status:      200,
				HeadersJson: hdrsJSON,
				Body:        []byte(`{"plugin":"otel","status":"ready","capabilities":["request_metrics","response_metrics","token_metrics"]}`),
				Handled:     true,
			}, nil
		}
		body := []byte(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Torana otel plugin</title></head>
<body><h1>Torana otel plugin</h1></body>
</html>`)
		hdrsJSON, _ := json.Marshal(map[string][]string{
			"Content-Type": {"text/html; charset=utf-8"},
		})
		return &pb.HttpResponse{
			Status:      200,
			HeadersJson: hdrsJSON,
			Body:        body,
			Handled:     true,
		}, nil
	})
}

func statusClass(status int) string {
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

// responseMeta is the slice of ToranaMeta._response these metrics are built
// from.
type responseMeta struct {
	DurationMs     float64 `json:"duration_ms"`
	UpstreamStatus int     `json:"upstream_status"`
	Usage          struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func parseResponseMeta(raw []byte) *responseMeta {
	if len(raw) == 0 {
		return nil
	}
	var meta struct {
		Response *responseMeta `json:"_response"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil
	}
	return meta.Response
}

// emission is one metric series, built but not yet sent.
type emission struct {
	Name   string
	Kind   int32
	Value  float64
	Labels map[string]string
}

// responseMetrics builds every series for a completed response.
//
// It is separate from the hook so the LABELS can be tested. sdk.EmitMetric is a
// host call, so anything assembled inline in the closure is unobservable from a
// test — which is why the bug this fixes (status_class computed and then
// dropped from the duration and token series) survived with a green suite, and
// why the first attempt at a test for it also passed against the broken code.
func responseMetrics(model string, r *responseMeta) []emission {
	labels := map[string]string{"model": model}

	// No per-response metadata: the only honest series is that a response
	// happened. It carries no status_class because none was observed —
	// labelling it "2xx" would invent a measurement.
	if r == nil {
		return []emission{{"torana_plugin_responses_total", sdk.MetricCounter, 1, labels}}
	}

	labels["status_class"] = statusClass(r.UpstreamStatus)

	// status_class belongs on EVERY series, not just the counter. The duration
	// and token metrics used to build fresh label maps holding only the model,
	// so latency and token spend could not be split by outcome — "how slow are
	// the 5xx?" and "how many tokens did failed requests burn?" are the first
	// two questions anyone asks of these, and neither was answerable.
	out := []emission{
		{"torana_plugin_responses_total", sdk.MetricCounter, 1, labels},
		{"torana_plugin_request_duration_ms", sdk.MetricHistogram, r.DurationMs, labels},
	}
	if r.Usage.InputTokens > 0 {
		out = append(out, emission{"torana_plugin_tokens", sdk.MetricCounter,
			float64(r.Usage.InputTokens), withLabel(labels, "direction", "input")})
	}
	if r.Usage.OutputTokens > 0 {
		out = append(out, emission{"torana_plugin_tokens", sdk.MetricCounter,
			float64(r.Usage.OutputTokens), withLabel(labels, "direction", "output")})
	}
	return out
}
