package main

import (
	"testing"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

// ==========================================================================
// Pure helpers (ported from v1; the response surface is now first-class v2
// ChatResponse fields instead of ToranaMeta._response parsing).
// ==========================================================================

func TestStatusClass(t *testing.T) {
	for status, want := range map[int32]string{
		200: "2xx", 201: "2xx", 299: "2xx",
		301: "3xx",
		400: "4xx", 429: "4xx",
		500: "5xx", 503: "5xx",
	} {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

// A status Torana never observed is 0, and bucketing that as "2xx" would make
// unreported outcomes indistinguishable from successes.
func TestStatusClassDoesNotInventSuccess(t *testing.T) {
	for _, status := range []int32{0, -1, 199} {
		if got := statusClass(status); got == "2xx" {
			t.Errorf("statusClass(%d) = %q; an unobserved status must not read as success", status, got)
		}
	}
}

func TestWithLabelDoesNotMutateTheBase(t *testing.T) {
	base := map[string]string{"model": "gpt-4", "status_class": "5xx"}

	input := withLabel(base, "direction", "input")
	output := withLabel(base, "direction", "output")

	if _, leaked := base["direction"]; leaked {
		t.Error("withLabel mutated the base map; direction would leak onto other series")
	}
	if input["direction"] != "input" || output["direction"] != "output" {
		t.Errorf("labels not applied: input=%v output=%v", input, output)
	}
	for _, m := range []map[string]string{input, output} {
		if m["model"] != "gpt-4" || m["status_class"] != "5xx" {
			t.Errorf("base labels not carried through: %v", m)
		}
	}
}

func findEmissions(t *testing.T, out []emission, name string) []emission {
	t.Helper()
	var found []emission
	for _, m := range out {
		if m.Name == name {
			found = append(found, m)
		}
	}
	if len(found) == 0 {
		t.Fatalf("no %s emission in %+v", name, out)
	}
	return found
}

// resp builds a ChatResponse with the given facts.
func resp(model string, status int32, durationMs int64, input, output int32) *pbv2.ChatResponse {
	return &pbv2.ChatResponse{
		Model:          model,
		UpstreamStatus: status,
		DurationMs:     durationMs,
		Usage:          &pbv2.Usage{InputTokens: input, OutputTokens: output},
	}
}

// TestEveryResponseSeriesCarriesStatusClass — the v1 regression: status_class
// on EVERY series when a status is observed.
func TestEveryResponseSeriesCarriesStatusClass(t *testing.T) {
	out := responseMetrics(resp("gpt-4", 503, 1234, 100, 20))
	if len(out) != 4 {
		t.Fatalf("expected 4 series (responses, duration, 2 token directions), got %d: %+v", len(out), out)
	}
	for _, m := range out {
		if got := m.Labels["status_class"]; got != "5xx" {
			t.Errorf("%s (direction=%q) has status_class=%q, want 5xx", m.Name, m.Labels["direction"], got)
		}
		if m.Labels["model"] != "gpt-4" {
			t.Errorf("%s lost the model label: %v", m.Name, m.Labels)
		}
	}
}

func TestTokenDirectionsDoNotLeak(t *testing.T) {
	out := responseMetrics(resp("gpt-4", 200, 0, 100, 20))
	tokens := findEmissions(t, out, "torana_plugin_tokens")
	if len(tokens) != 2 {
		t.Fatalf("expected 2 token series, got %d", len(tokens))
	}
	seen := map[string]float64{}
	for _, m := range tokens {
		seen[m.Labels["direction"]] = m.Value
	}
	if seen["input"] != 100 || seen["output"] != 20 {
		t.Errorf("token values/directions wrong: %v", seen)
	}
	for _, m := range findEmissions(t, out, "torana_plugin_request_duration_ms") {
		if _, leaked := m.Labels["direction"]; leaked {
			t.Errorf("direction leaked onto the duration series: %v", m.Labels)
		}
	}
}

// TestObservationalStreamShape — the real host dispatches after-response for
// streams with mutable=false and Message==nil but with completed facts. The
// metrics must not require Message.
func TestObservationalStreamShape(t *testing.T) {
	stream := resp("claude-sonnet-4", 200, 987, 500, 60)
	stream.Message = nil // stream-shaped: no assistant message
	out := responseMetrics(stream)
	if len(out) != 4 {
		t.Fatalf("stream-shaped dispatch must emit the same 4 series, got %d", len(out))
	}
	for _, m := range out {
		if m.Labels["status_class"] != "2xx" {
			t.Errorf("stream series missing the observed class: %v", m.Labels)
		}
	}
}

// TestUpstreamErrorShape — upstream-error dispatches carry facts and a >=400
// status; the class must be on every series.
func TestUpstreamErrorShape(t *testing.T) {
	out := responseMetrics(resp("claude-sonnet-4", 429, 321, 0, 0))
	for _, m := range out {
		if m.Labels["status_class"] != "4xx" {
			t.Errorf("error series missing the observed class: %v", m.Labels)
		}
	}
}

// TestStatusZeroEmitsFactsWithoutClass — UpstreamStatus == 0 is unobserved:
// genuinely present facts (response happened, duration, usage) are emitted,
// but NO status_class appears on ANY series.
func TestStatusZeroEmitsFactsWithoutClass(t *testing.T) {
	out := responseMetrics(resp("gpt-4", 0, 55, 30, 5))
	if len(out) != 4 {
		t.Fatalf("facts must still be emitted for an unobserved status, got %d", len(out))
	}
	for _, m := range out {
		if _, claimed := m.Labels["status_class"]; claimed {
			t.Errorf("status 0 must not claim a class: %v", m.Labels)
		}
	}
	if got := findEmissions(t, out, "torana_plugin_request_duration_ms")[0].Value; got != 55 {
		t.Errorf("duration fact lost: %v", got)
	}
}

// A nil response (no facts at all) gets one honest series and no status_class.
func TestResponseWithoutFactsEmitsOneUnlabelledSeries(t *testing.T) {
	out := responseMetrics(nil)
	if len(out) != 1 || out[0].Name != "torana_plugin_responses_total" {
		t.Fatalf("expected exactly the responses counter, got %+v", out)
	}
	if _, present := out[0].Labels["status_class"]; present {
		t.Errorf("no status was observed, so none should be claimed: %v", out[0].Labels)
	}
}

// Zero token counts are absent rather than reported as zero.
func TestZeroTokenCountsAreNotEmitted(t *testing.T) {
	out := responseMetrics(resp("gpt-4", 200, 0, 0, 0))
	for _, m := range out {
		if m.Name == "torana_plugin_tokens" {
			t.Errorf("emitted a token series with no usage reported: %+v", m)
		}
	}
}

// ==========================================================================
// Hook-level (sdktest)
// ==========================================================================

// TestRequestShapeMetricsAndPassThrough — the request hook emits shape metrics
// and always passes through.
func TestRequestShapeMetricsAndPassThrough(t *testing.T) {
	h := sdktest.New(t)
	res := h.BeforeRequest(&pbv2.ChatRequest{Model: "gpt-4", Messages: []*pbv2.Message{{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}}}})
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("otel must pass requests through, err=%v", res.Err)
	}
	seen := map[string]bool{}
	for _, m := range h.Metrics() {
		seen[m.Name] = true
		if m.Labels["model"] != "gpt-4" {
			t.Errorf("%s missing the model label: %v", m.Name, m.Labels)
		}
	}
	for _, want := range []string{"torana_plugin_requests_total", "torana_plugin_request_messages", "torana_plugin_request_tools"} {
		if !seen[want] {
			t.Errorf("missing request metric %s", want)
		}
	}
}

// TestResponseHookEmitsFactsForMutableAndObservational — the after-response
// hook emits the same factual series for mutable=true and stream/error-shaped
// mutable=false dispatches.
func TestResponseHookEmitsFactsForMutableAndObservational(t *testing.T) {
	for name, mk := range map[string]func() *pbv2.ChatResponse{
		"mutable json":  func() *pbv2.ChatResponse { return resp("gpt-4", 200, 100, 10, 5) },
		"stream shaped": func() *pbv2.ChatResponse { r := resp("gpt-4", 200, 200, 20, 6); r.Message = nil; return r },
		"error shaped":  func() *pbv2.ChatResponse { return resp("gpt-4", 502, 300, 0, 0) },
	} {
		t.Run(name, func(t *testing.T) {
			h := sdktest.New(t)
			res := h.AfterResponse(mk(), false)
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("otel must pass responses through, err=%v", res.Err)
			}
			found := false
			for _, m := range h.Metrics() {
				if m.Name == "torana_plugin_request_duration_ms" {
					found = true
				}
			}
			if !found {
				t.Fatal("response metrics not emitted")
			}
		})
	}
}

// TestHTTPRoutes — / and /agent/status are served; unknown paths pass to the
// host (404).
func TestHTTPRoutes(t *testing.T) {
	h := sdktest.New(t)
	ok := h.HTTPRequest(&pbv2.HttpRequest{Method: "GET", Path: "/agent/status"})
	if ok.Err != nil || ok.Response == nil || ok.Response.Status != 200 {
		t.Fatalf("agent/status must be served, got %+v", ok.Response)
	}
	root := h.HTTPRequest(&pbv2.HttpRequest{Method: "GET", Path: "/"})
	if root.Err != nil || root.Response == nil || root.Response.Status != 200 {
		t.Fatalf("/ must be served, got %+v", root.Response)
	}
	unknown := h.HTTPRequest(&pbv2.HttpRequest{Method: "GET", Path: "/nope"})
	if unknown.Err != nil || !unknown.PassedThrough {
		t.Fatalf("unknown paths must pass to the host 404, err=%v", unknown.Err)
	}
}

// TestNoUnauthorizedCalls — otel makes no host calls at all (no state, cache,
// pricing); metrics ride the dedicated emit path.
func TestNoUnauthorizedCalls(t *testing.T) {
	h := sdktest.New(t)
	h.BeforeRequest(&pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}}}})
	h.AfterResponse(resp("m", 200, 1, 2, 3), false)
	for _, c := range h.Calls() {
		t.Errorf("otel made a host call outside its grant set: %s", c.Command)
	}
	if len(h.Metrics()) == 0 {
		t.Fatal("otel emitted no metrics")
	}
}
