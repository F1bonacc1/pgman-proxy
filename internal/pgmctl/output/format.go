package output

import (
	"encoding/json"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Format is the value of the global -o/--output flag.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
	FormatWide  Format = "wide"
)

// ParseFormat parses the flag value; empty defaults to table.
func ParseFormat(s string) (Format, error) {
	switch s {
	case "", "table":
		return FormatTable, nil
	case "json":
		return FormatJSON, nil
	case "yaml":
		return FormatYAML, nil
	case "wide":
		return FormatWide, nil
	default:
		return "", fmt.Errorf("unknown output format %q (valid: table, json, yaml, wide)", s)
	}
}

// VersionedDoc is the schema-versioned envelope every non-table
// output wraps its payload in (FR-038). Downstream automation MAY
// pin to `apiVersion: pgmctl/v1` and reject unknown majors.
type VersionedDoc struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	Payload    any    `json:"-" yaml:"-"`
}

// EmitJSON writes the payload in pgmctl/v1 form to w.
func EmitJSON(w io.Writer, kind string, payload any) error {
	doc := versionedJSON{APIVersion: "pgmctl/v1", Kind: kind, Payload: payload}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// EmitYAML writes the payload in pgmctl/v1 form to w.
func EmitYAML(w io.Writer, kind string, payload any) error {
	doc := versionedYAML{APIVersion: "pgmctl/v1", Kind: kind, Payload: payload}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(doc)
}

// Two parallel struct types so we don't smuggle the `Payload` tag
// across formats — JSON and YAML have different layout expectations
// for inline payloads, but in pgmctl/v1 the payload always lives
// alongside apiVersion + kind at the document root.
type versionedJSON struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Payload    any    `json:"payload"`
}

type versionedYAML struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Payload    any    `yaml:"payload"`
}
