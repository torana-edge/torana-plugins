package main

import "testing"

// status_class used to be computed and then dropped: the duration and token
// metrics built fresh label maps holding only the model. Latency and token
// spend could not be split by outcome, which is the first thing anyone asks of
// these metrics.

func TestStatusClass(t *testing.T) {
	for status, want := range map[int]string{
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
	for _, status := range []int{0, -1, 199} {
		if got := statusClass(status); got == "2xx" {
			t.Errorf("statusClass(%d) = %q; an unobserved status must not read as success", status, got)
		}
	}
}

func TestWithLabelDoesNotMutateTheBase(t *testing.T) {
	base := map[string]string{"model": "gpt-4", "status_class": "5xx"}

	input := withLabel(base, "direction", "input")
	output := withLabel(base, "direction", "output")

	// The base is emitted with several series. Mutating it would leak
	// "direction" onto every metric emitted after the first token count.
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

// The bug this file exists for: status_class was computed, attached to the
// responses_total labels, and then dropped — the duration and token metrics
// built fresh maps holding only the model.
//
// The first version of these tests could not catch it. sdk.EmitMetric is a host
// call, so labels assembled inline in the hook closure are unobservable from a
// test; reverting the entire fix left the suite green. responseMetrics exists so
// the labels are a value that can be inspected.

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

// TestEveryResponseSeriesCarriesStatusClass is the regression test.
func TestEveryResponseSeriesCarriesStatusClass(t *testing.T) {
	meta := &responseMeta{DurationMs: 1234, UpstreamStatus: 503}
	meta.Usage.InputTokens = 100
	meta.Usage.OutputTokens = 20

	out := responseMetrics("gpt-4", meta)

	if len(out) != 4 {
		t.Fatalf("expected 4 series (responses, duration, 2 token directions), got %d: %+v", len(out), out)
	}
	for _, m := range out {
		if got := m.Labels["status_class"]; got != "5xx" {
			t.Errorf("%s (direction=%q) has status_class=%q, want 5xx — "+
				"without it, latency and token spend cannot be split by outcome",
				m.Name, m.Labels["direction"], got)
		}
		if m.Labels["model"] != "gpt-4" {
			t.Errorf("%s lost the model label: %v", m.Name, m.Labels)
		}
	}
}

// The token series must be distinguishable, and adding direction to one must
// not leak into the other or into the shared base.
func TestTokenDirectionsDoNotLeak(t *testing.T) {
	meta := &responseMeta{UpstreamStatus: 200}
	meta.Usage.InputTokens = 100
	meta.Usage.OutputTokens = 20

	out := responseMetrics("gpt-4", meta)
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

// A response with no metadata gets one honest series and NO status_class:
// labelling an unobserved outcome "2xx" would invent a measurement.
func TestResponseWithoutMetaEmitsOneUnlabelledSeries(t *testing.T) {
	out := responseMetrics("gpt-4", nil)
	if len(out) != 1 || out[0].Name != "torana_plugin_responses_total" {
		t.Fatalf("expected exactly the responses counter, got %+v", out)
	}
	if _, present := out[0].Labels["status_class"]; present {
		t.Errorf("no status was observed, so none should be claimed: %v", out[0].Labels)
	}
}

// Zero token counts are absent rather than reported as zero — a zero-token
// response is a measurement nobody made.
func TestZeroTokenCountsAreNotEmitted(t *testing.T) {
	out := responseMetrics("gpt-4", &responseMeta{UpstreamStatus: 200})
	for _, m := range out {
		if m.Name == "torana_plugin_tokens" {
			t.Errorf("emitted a token series with no usage reported: %+v", m)
		}
	}
}

func TestParseResponseMeta(t *testing.T) {
	if got := parseResponseMeta(nil); got != nil {
		t.Errorf("nil meta should parse to nil, got %+v", got)
	}
	if got := parseResponseMeta([]byte(`{"other":"field"}`)); got != nil {
		t.Errorf("meta without _response should be nil, got %+v", got)
	}
	if got := parseResponseMeta([]byte(`not json`)); got != nil {
		t.Errorf("unparseable meta should be nil, got %+v", got)
	}
	got := parseResponseMeta([]byte(`{"_response":{"duration_ms":42,"upstream_status":429,"usage":{"input_tokens":7}}}`))
	if got == nil || got.UpstreamStatus != 429 || got.DurationMs != 42 || got.Usage.InputTokens != 7 {
		t.Errorf("meta misparsed: %+v", got)
	}
}
