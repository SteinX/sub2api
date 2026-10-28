package devin

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

// NormalizeToolSchemas runs after model assignment so routed Gemini requests
// receive the same scalar-type schema form as direct requests. Other model
// families retain their original JSON Schema.
func NormalizeToolSchemas(request *pb.GetChatMessageRequest) error {
	model := strings.ToLower(request.ChatModelUid)
	if !strings.Contains(model, "gemini") {
		return nil
	}
	for _, tool := range request.Tools {
		var schema map[string]any
		decoder := json.NewDecoder(strings.NewReader(tool.JsonSchemaString))
		decoder.UseNumber()
		if !json.Valid([]byte(tool.JsonSchemaString)) || decoder.Decode(&schema) != nil || schema == nil {
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
		var branches []map[string]any
		for _, value := range types {
			if value == "null" {
				schema["nullable"] = true
			} else if typ, ok := value.(string); ok {
				branches = append(branches, map[string]any{"type": typ})
			}
		}
		delete(schema, "type")
		if len(branches) == 1 {
			schema["type"] = branches[0]["type"]
		} else if len(branches) > 1 {
			// Move type-specific constraints into the applicable branch rather
			// than leaving e.g. items beside a string branch.
			for _, branch := range branches {
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
