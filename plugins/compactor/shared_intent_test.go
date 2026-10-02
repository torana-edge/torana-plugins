package main

import "testing"

// Every producer/consumer module pins the same protocol, without depending
// on another plugin binary being installed or trusting call IDs globally.
func TestSharedIntentContract(t *testing.T) {
	base := sharedIntentKey("conversation", "call", "read", `{"path":"server.go","n":9007199254740993}`)
	if base == "" {
		t.Fatal("valid occurrence lacks a key")
	}
	for _, args := range []string{`{"n":9007199254740993,"path":"server.go"}`, `{"path":"server.go","i":"inspect","n":9007199254740993}`} {
		if got := sharedIntentKey("conversation", "call", "read", args); got != base {
			t.Fatal("order or injected intent changed identity")
		}
	}
	for _, row := range [][4]string{
		{"another", "call", "read", `{"path":"server.go","n":9007199254740993}`},
		{"conversation", "another", "read", `{"path":"server.go","n":9007199254740993}`},
		{"conversation", "call", "write", `{"path":"server.go","n":9007199254740993}`},
		{"conversation", "call", "read", `{"path":"different.go","n":9007199254740993}`},
		{"conversation", "call", "read", `{"path":"server.go","n":9007199254740992}`},
	} {
		if got := sharedIntentKey(row[0], row[1], row[2], row[3]); got == "" || got == base {
			t.Fatal("distinct occurrence borrowed shared intent")
		}
	}
	for _, args := range []string{"null", "[]", "{", `{"path":"a","path":"b"}`} {
		if got := sharedIntentKey("conversation", "call", "read", args); got != "" {
			t.Fatal("ambiguous arguments admitted")
		}
	}
	for _, row := range [][4]string{{"", "call", "read", "{}"}, {"conversation", "", "read", "{}"}, {"conversation", "call", "", "{}"}} {
		if got := sharedIntentKey(row[0], row[1], row[2], row[3]); got != "" {
			t.Fatal("unbound occurrence admitted")
		}
	}
	if base != "intent/shared/v3:sha256:78ec2e143d23422c79c8bcbc739667cc44e6a2a45987fef76c17e14f4eaf57f0" {
		t.Fatalf("shared protocol drift: %s", base)
	}
}
