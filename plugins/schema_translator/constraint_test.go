package main

import (
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// Converting a map to rows must not erase validation that the rows cannot
// express. The entire unsupported subtree stays byte-identical instead.
func TestConstrainedMapSchemasStayExact(t *testing.T) {
	for name, extra := range map[string]string{
		"enum":           `"enum":[{"prod":"on"}],`,
		"const":          `"const":{"prod":"on"},`,
		"key pattern":    `"propertyNames":{"pattern":"^[A-Z]+$"},`,
		"pattern values": `"patternProperties":{"^secret":{"type":"string"}},`,
		"minimum count":  `"minProperties":2,`,
		"maximum count":  `"maxProperties":4,`,
		"required key":   `"required":["prod"],`,
		"reference":      `"$ref":"#/$defs/map",`,
		"conditional":    `"if":{"required":["prod"]},"then":{"required":["audit"]},`,
		"union":          `"oneOf":[{"required":["prod"]},{"required":["test"]}],`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := `{"type":"object","additionalProperties":false,"properties":{"env":{"type":"object",` + extra + `"additionalProperties":{"type":"string"}}}}`
			reg, tools, changed := translateTools([]*pb.ToolDef{{Name: "run", ParametersJson: []byte(raw)}})
			if changed || string(tools[0].ParametersJson) != raw || len(reg.tools) != 0 {
				t.Fatalf("unsupported constrained map was changed: %s; registry=%v", tools[0].ParametersJson, reg.tools)
			}
		})
	}
}

func TestApplicatorSubtreesStayExact(t *testing.T) {
	for name, raw := range map[string]string{
		"parent enum":        `{"type":"object","additionalProperties":false,"enum":[{"env":{"prod":"on"}}],"properties":{"env":{"type":"object","additionalProperties":true}}}`,
		"parent conditional": `{"type":"object","additionalProperties":false,"if":{"required":["env"]},"then":{"properties":{"env":{"required":["prod"]}}},"properties":{"env":{"type":"object","additionalProperties":true}}}`,
		"union map":          `{"type":"object","additionalProperties":false,"properties":{"env":{"type":["object","null"],"additionalProperties":true}}}`,
		"non-object AP":      `{"type":"object","additionalProperties":false,"properties":{"env":{"type":"string","additionalProperties":true}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			reg, tools, changed := translateTools([]*pb.ToolDef{{Name: "run", ParametersJson: []byte(raw)}})
			if changed || string(tools[0].ParametersJson) != raw || len(reg.tools) != 0 {
				t.Fatalf("unsupported subtree was changed: %s; registry=%v", tools[0].ParametersJson, reg.tools)
			}
		})
	}
}
