package devin

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

// NormalizeToolSchemas runs after AssignModel so Fusion models routed to Google
// receive the same scalar-type schema form as direct Gemini models. Other model
// families retain their original JSON Schema.
func NormalizeToolSchemas(request *pb.GetChatMessageRequest) error {
	model := strings.ToLower(request.ChatModelUid)
	if !strings.Contains(model, "gemini") {
		return nil
	}
	for _, tool := range request.Tools {
		var schema map[string]any
		if err := json.Unmarshal([]byte(tool.JsonSchemaString), &schema); err != nil || schema == nil {
			return fmt.Errorf("invalid schema for tool %q", tool.Name)
		}
		normalizeGoogleSchemaTypes(schema)
		body, err := json.Marshal(schema)
		if err != nil {
			return err
		}
		tool.JsonSchemaString = string(body)
	}
	return nil
}

func normalizeGoogleSchemaTypes(schema map[string]any) {
	// Only schema locations are visited: a property named "type" or "items",
	// and objects inside enum/default values, are user data.
	if properties, ok := schema["properties"].(map[string]any); ok {
		for _, value := range properties {
			if child, ok := value.(map[string]any); ok {
				normalizeGoogleSchemaTypes(child)
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if child, ok := schema[key].(map[string]any); ok {
			normalizeGoogleSchemaTypes(child)
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if children, ok := schema[key].([]any); ok {
			for _, value := range children {
				if child, ok := value.(map[string]any); ok {
					normalizeGoogleSchemaTypes(child)
				}
			}
		}
	}
	if constant, present := schema["const"]; present {
		schema["enum"] = []any{constant}
		delete(schema, "const")
	}
	if types, ok := schema["type"].([]any); ok {
		var branches []any
		for _, value := range types {
			if value == "null" {
				schema["nullable"] = true
			} else if typ, ok := value.(string); ok {
				branches = append(branches, map[string]any{"type": typ})
			}
		}
		delete(schema, "type")
		if len(branches) == 1 {
			schema["type"] = branches[0].(map[string]any)["type"]
		} else if len(branches) > 1 {
			// Move type-specific constraints into the applicable branch rather
			// than leaving e.g. items beside a string branch.
			for _, value := range branches {
				branch := value.(map[string]any)
				var keys []string
				switch branch["type"] {
				case "object":
					keys = []string{"properties", "required", "additionalProperties"}
				case "array":
					keys = []string{"items", "minItems", "maxItems", "uniqueItems"}
				case "string":
					keys = []string{"pattern", "format", "minLength", "maxLength"}
				case "number", "integer":
					keys = []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"}
				}
				for _, key := range keys {
					if constraint, present := schema[key]; present {
						branch[key] = constraint
					}
				}
				normalizeGoogleSchemaTypes(branch)
			}
			for _, key := range []string{"properties", "required", "additionalProperties", "items", "minItems", "maxItems", "uniqueItems", "pattern", "format", "minLength", "maxLength", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
				delete(schema, key)
			}
			// Preserve an existing anyOf as a conjunction instead of replacing it.
			if existing, present := schema["anyOf"]; present {
				all, _ := schema["allOf"].([]any)
				schema["allOf"] = append(all, map[string]any{"anyOf": existing}, map[string]any{"anyOf": branches})
				delete(schema, "anyOf")
			} else {
				schema["anyOf"] = branches
			}
		} else {
			schema["type"] = "null"
		}
	}
	if schema["type"] == "object" && schema["properties"] == nil {
		schema["properties"] = map[string]any{}
	}
}
