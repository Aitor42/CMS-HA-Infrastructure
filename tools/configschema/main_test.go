package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommittedSchemaIsCurrent(t *testing.T) {
	want, err := buildSchema()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	path := filepath.Join("..", "..", schemaPath)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed schema: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is stale; run `go run ./tools/configschema` and commit the result", schemaPath)
	}
}
