package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// Advice deliberately never routes. The user can switch in their harness;
// subsequent requests reconcile that choice through the observed client model.
func publishAdvice(key, policyHash string, ladder shadowLadder, state shadowState, decision policyDecision) {
	i := shadowStepIndex(ladder, decision.Target)
	if i < 0 {
		return
	}
	step := ladder.Steps[i]
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s/%s", key, policyHash, state.Step, step.ID)))
	harnessModel := step.Model
	if len(step.Aliases) > 0 {
		harnessModel = step.Aliases[0]
	}
	body := fmt.Sprintf("%s. Estimated cache rebuild: $%.4f; per-turn cost change: $%+.4f. Switch in your harness if useful; Torana will keep your current route until you do.", adviceText(step.Description, 120), decision.RebuildUSD, decision.PerTurnDelta)
	if decision.PaybackTurns > 0 {
		body += fmt.Sprintf(" Estimated payback: %.1f turns.", decision.PaybackTurns)
	}
	if decision.Class == "effort_change" {
		body += " This changes reasoning effort and may invalidate the prompt cache."
	}
	_, err := sdk.Suggest(&pbv1.SuggestArgs{
		Kind: decision.Class, DedupeKey: hex.EncodeToString(digest[:]),
		Title: adviceText("Consider "+step.Model, 120), Body: adviceText(body, 600), CostUsd: &decision.RebuildUSD,
		HarnessTargetModel: &harnessModel, ExpiresAfterUserTurns: 3,
	})
	if err != nil {
		emit("suggest_failed", pricingLookupReason(err))
	} else {
		emit("suggested", step.ID)
	}
}

func adviceText(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	last := 0
	for i := range s {
		if i > limit-3 {
			return s[:last] + "..."
		}
		last = i
	}
	return s
}
