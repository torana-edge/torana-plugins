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
