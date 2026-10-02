package main

import (
	"encoding/json"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/strictjson"
)

// This is the intent/shared/v3 producer-consumer contract. Keep its golden
// tests in intent, compactor and keyword_compactor identical. The shared
// cache is global: a call ID alone is not an occurrence or a task identity.
func sharedIntentKey(conversation, id, name, arguments string) string {
	if conversation == "" || id == "" || name == "" {
		return ""
	}
	args, err := strictjson.DecodeObject([]byte(arguments))
	if err != nil || args == nil {
		return ""
	}
	delete(args, "i")
	normalized, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return sdk.ContentAddressedCacheKey("intent/shared/v3", conversation, id, name, string(normalized))
}
