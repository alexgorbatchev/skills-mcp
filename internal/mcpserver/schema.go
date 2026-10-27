package mcpserver

import "github.com/google/jsonschema-go/jsonschema"

// object builds an object schema that rejects unknown properties. Each call
// allocates its own false schema: a schema must form a tree.
func object(required []string, props map[string]*jsonschema.Schema) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object", Properties: props, Required: required,
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
}

func str(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: description}
}

func nonNegative(description string) *jsonschema.Schema {
	zero := 0.0
	return &jsonschema.Schema{Type: "integer", Minimum: &zero, Description: description}
}

func boolean(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: description}
}

func ptr[T any](v T) *T { return &v }
