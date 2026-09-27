package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/strictjson"
)

type explanationFinding struct {
	Category string `json:"category"`
	Line     int    `json:"line"`
}

type explanation struct {
	Found     bool                 `json:"found"`
	Findings  []explanationFinding `json:"findings"`
	Truncated bool                 `json:"truncated"`
}

func explanationKey(conversation string) string {
	sum := sha256.Sum256([]byte("torana/pii-guard/explanation/v1\x00" + conversation))
	return "explanation/v1/" + hex.EncodeToString(sum[:])
}

// Only fresh replacements update this record. Historical replay must not make
// an old detection appear to be the latest, or change the conversation bytes.
func rememberExplanation(ctx context.Context, findings []finding) error {
	execution := sdk.Execution(ctx)
	if execution == nil || execution.GetConversationId() == "" {
		return errors.New("stable conversation identity is unavailable")
	}
	record := explanation{Found: true, Findings: make([]explanationFinding, 0, maxReportedFindings), Truncated: len(findings) > maxReportedFindings}
	for _, item := range findings {
		if len(record.Findings) == maxReportedFindings {
			break
		}
		record.Findings = append(record.Findings, explanationFinding{Category: item.category, Line: item.line})
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return sdk.StateSet(explanationKey(execution.GetConversationId()), string(raw))
}

func validExplanation(record explanation) bool {
	if !record.Found || len(record.Findings) == 0 || len(record.Findings) > maxReportedFindings {
		return false
	}
	for _, item := range record.Findings {
		if item.Line <= 0 {
			return false
		}
		switch item.Category {
		case "us_ssn", "aws_access_key", "private_key", "api_key", "access_token":
		default:
			return false
		}
	}
	return true
}

func init() {
	sdk.OnHTTPRequest(func(_ context.Context, req *pb.HttpRequest) (sdk.HTTPResult, error) {
		if req == nil || req.Method != "GET" || req.Path != "/agent/redaction/explain-last" {
			return sdk.PassHTTP(), nil
		}
		binding, present, err := sdk.HTTPConversation(req)
		if err != nil {
			return sdk.PassHTTP(), err
		}
		if !present || !binding.Bound {
			return sdk.ServeHTTP(&pb.HttpResponse{Status: 409, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: []byte(`{"error":"unbound_conversation"}`)}), nil
		}
		raw, found, err := sdk.StateGet(explanationKey(binding.ConversationID))
		if err != nil {
			return sdk.PassHTTP(), err
		}
		record := explanation{Findings: []explanationFinding{}}
		if found {
			object, err := strictjson.DecodeObject([]byte(raw))
			if err != nil || object == nil || json.Unmarshal([]byte(raw), &record) != nil || !validExplanation(record) {
				return sdk.PassHTTP(), errors.New("pii_guard: invalid explanation record")
			}
		}
		body, err := json.Marshal(record)
		if err != nil {
			return sdk.PassHTTP(), err
		}
		return sdk.ServeHTTP(&pb.HttpResponse{Status: 200, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body}), nil
	})
}
