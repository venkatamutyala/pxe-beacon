package httpd

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/venkatamutyala/pxe-beacon/pkg/pxebeacon"
	"gopkg.in/yaml.v3"
)

// TestOpenAPI_SchemasMatchStructs is the v0.9.1 drift guard. Now that
// the wire types live in pkg/pxebeacon AND the OpenAPI spec is hand-
// written, the two are independent sources of truth for the same wire
// contract — and would silently diverge. This test fails the build if
// a pxebeacon struct's JSON field set stops matching its OpenAPI
// component schema's `properties`, in either direction.
//
// (Inline response bodies like ListResponse aren't named component
// schemas in the spec, so they're not covered here — only the named
// components.schemas entries.)
func TestOpenAPI_SchemasMatchStructs(t *testing.T) {
	// Parse the embedded spec's component schemas.
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(openAPISpec, &doc); err != nil {
		t.Fatalf("openapi.yaml parse: %v", err)
	}

	// struct → schema name (most are 1:1; APIError is documented as "Error").
	cases := []struct {
		schema string
		typ    reflect.Type
	}{
		{"Desired", reflect.TypeOf(pxebeacon.Desired{})},
		{"Observed", reflect.TypeOf(pxebeacon.Observed{})},
		{"Machine", reflect.TypeOf(pxebeacon.Machine{})},
		{"Intent", reflect.TypeOf(pxebeacon.Intent{})},
		{"MachineConfig", reflect.TypeOf(pxebeacon.MachineConfig{})},
		{"Error", reflect.TypeOf(pxebeacon.APIError{})},
	}

	for _, c := range cases {
		t.Run(c.schema, func(t *testing.T) {
			schema, ok := doc.Components.Schemas[c.schema]
			if !ok {
				t.Fatalf("openapi.yaml has no components.schemas.%s", c.schema)
			}
			specFields := keys(schema.Properties)
			structFields := jsonFields(c.typ)
			if !equalSets(specFields, structFields) {
				t.Errorf("field-set drift between pkg/pxebeacon.%s and openapi schema %q\n  struct: %v\n  spec:   %v",
					c.typ.Name(), c.schema, structFields, specFields)
			}
		})
	}
}

// jsonFields returns the JSON field names of a struct (tag name minus
// options; skips "-" and embedded-without-tag).
func jsonFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
