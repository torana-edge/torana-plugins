package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ==========================================================================
// Strict JSON decoding shared by the registry envelope and the auth verify
// response (each plugin carries its own copy; plugins are separate modules).
// ==========================================================================

// decodeObjectStrict decodes data as a JSON OBJECT, rejecting:
//   - duplicate keys at ANY nesting level (a repeated tool name in a registry,
//     a repeated status member in a verify response, ...);
//   - unknown members (anything outside known);
//   - null members;
//   - trailing data after the value.
//
// Member PRESENCE is preserved: the returned map contains exactly the members
// that were written, so a missing member is distinguishable from an empty
// string or false value.
func decodeObjectStrict(data []byte, known map[string]bool) (map[string]json.RawMessage, error) {
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	for member, value := range raw {
		if !known[member] {
			return nil, fmt.Errorf("unknown member %q", member)
		}
		if string(value) == "null" {
			return nil, fmt.Errorf("member %q must not be null", member)
		}
	}
	return raw, nil
}

// rejectDuplicateKeys walks the raw JSON and rejects any object that repeats a
// key, at every nesting level.
func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return walkNoDups(dec)
}

func walkNoDups(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return walkValueNoDups(dec, tok)
}

func walkValueNoDups(dec *json.Decoder, tok json.Token) error {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyTok.(string)
				if !ok {
					return fmt.Errorf("object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate JSON member %q", key)
				}
				seen[key] = true
				if err := walkNoDups(dec); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil {
				return err
			}
			if end != json.Delim('}') {
				return fmt.Errorf("object not closed")
			}
		case '[':
			for dec.More() {
				if err := walkNoDups(dec); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return fmt.Errorf("array not closed")
			}
		default:
			return fmt.Errorf("unexpected delimiter %v", t)
		}
	case nil:
		return fmt.Errorf("top-level null is not a valid object")
	default:
		// scalar: nothing to check
	}
	return nil
}
