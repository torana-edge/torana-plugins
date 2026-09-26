package main

import (
	"context"
	"encoding/json"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func init() {
	sdk.OnAfterResponse(func(_ context.Context, response *pbv1.ChatResponse, _ bool) (sdk.ResponseResult, error) {
		key, found, err := sdk.MetaGet("decision_router_state_key")
		if err != nil {
			return sdk.ResponseResult{}, err
		}
		if !found || key == "" || response == nil {
			return sdk.PassResponse(), nil
		}
		for attempt := 0; attempt < 3; attempt++ {
			stored, exists, err := sdk.StateGetVersioned(key)
			if err != nil {
				return sdk.ResponseResult{}, err
			}
			if !exists {
				return sdk.PassResponse(), nil
			}
			var state shadowState
			if err := json.Unmarshal([]byte(stored.Value), &state); err != nil {
				return sdk.ResponseResult{}, err
			}
			if response.Usage != nil {
				// Usage updates do not imply that a requested route was applied.
				// input_tokens is total input, including cache reads/writes where
				// reported; adding those again would overestimate switch cost.
				if response.Usage.InputTokens > 0 {
					state.ContextTokens = int64(response.Usage.InputTokens)
				}
				state.AvgOutputTokens = (state.AvgOutputTokens*float64(state.ResponseCount) + float64(response.Usage.OutputTokens)) / float64(state.ResponseCount+1)
				state.ResponseCount++
			}
			if strings.EqualFold(response.FinishReason, "length") || strings.EqualFold(response.FinishReason, "max_tokens") {
				state.MaxTokensFinishes++
			}
			version := stored.Version
			reconcileAppliedRoute(response, &state)
			applied, err := saveShadowState(key, state, &version)
			if err != nil {
				return sdk.ResponseResult{}, err
			}
			if applied {
				return sdk.PassResponse(), nil
			}
		}
		emit("shadow_state_conflict", "")
		return sdk.PassResponse(), nil
	})
}
