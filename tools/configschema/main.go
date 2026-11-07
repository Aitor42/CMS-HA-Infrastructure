// Command configschema generates schema/config.schema.json from the Go types in
// internal/config. The test in this package fails when the committed schema is
// stale, so adding a YAML field without regenerating it is a CI failure.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

const schemaPath = "schema/config.schema.json"

func main() {
	data, err := buildSchema()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configschema:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(schemaPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "configschema:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(schemaPath, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "configschema:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", schemaPath)
}

func buildSchema() ([]byte, error) {
	defs := make(map[string]any)
	root := objectSchema(reflect.TypeOf(config.Config{}), defs)
	doc := map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"title":       "CMS-HA Infrastructure configuration",
		"description": "Schema for config.yaml. Secrets may be plain text or age-encrypted ENC[...] values.",
		"$ref":        "#/$defs/Config",
		"$defs":       defs,
	}
	defs["Config"] = root
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func objectSchema(t reflect.Type, defs map[string]any) map[string]any {
	props := make(map[string]any)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, ok := yamlName(f)
		if !ok {
			continue
		}
		props[name] = fieldSchema(f.Type, defs)
	}
	return map[string]any{
		"type":                 "object",
		"title":                t.Name(),
		"properties":           props,
		"additionalProperties": false,
	}
}

func fieldSchema(t reflect.Type, defs map[string]any) any {
	if t == reflect.TypeOf(time.Duration(0)) {
		return map[string]any{
			"type":        []string{"string", "integer"},
			"description": "Go duration, for example 10s, 15m or 2h; an integer is interpreted as nanoseconds.",
		}
	}
	switch t.Kind() {
	case reflect.Struct:
		name := t.Name()
		if _, ok := defs[name]; !ok {
			// Reserve the slot first so a self-referential type cannot recurse.
			defs[name] = map[string]any{"type": "object"}
			defs[name] = objectSchema(t, defs)
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	case reflect.Pointer:
		return fieldSchema(t.Elem(), defs)
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": fieldSchema(t.Elem(), defs)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": fieldSchema(t.Elem(), defs)}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{}
	}
}

func yamlName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return strings.ToLower(f.Name), true
	}
	name := strings.Split(tag, ",")[0]
	if name == "" || name == "-" {
		return "", false
	}
	return name, true
}
