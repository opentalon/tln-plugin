package runtime

import (
	"strings"
	"testing"
)

func TestGuardUnsafeSource_Rejects(t *testing.T) {
	cases := map[string]struct {
		src  string
		want string // substring expected in the rejection
	}{
		"connector block": {
			src:  `connector "inventory" via mcp { endpoint env "X" }`,
			want: "connector",
		},
		"env value": {
			src:  `connector "x" via mcp { bearer env "SECRET" }`,
			want: "connector", // connector is caught first, which is also correct
		},
		"env inside a workflow arg": {
			src:  `workflow "w" { step "s" { tool "svc" "op" { token env "SECRET" } } }`,
			want: "env",
		},
		"io tool server": {
			src:  `workflow "w" { step "s" { tool "io" "writeln" { text "hi" } } }`,
			want: "io",
		},
		"legacy mcp io verb": {
			src:  `workflow "w" { step "s" { mcp "io" "writeln" { text "hi" } } }`,
			want: "io",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := guardUnsafeSource(c.src, nil)
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("rejection %q should mention %q", err.Error(), c.want)
			}
		})
	}
}

func TestGuardUnsafeSource_AllowsHostMediatedTools(t *testing.T) {
	// The legitimate shape: reach a host-configured MCP server by name. No
	// connector, no env, no io — must pass the guard untouched.
	ok := []string{
		`workflow "w" { step "s" { tool "inventory" "list_items" { query "x" } } }`,
		`detect "d" { for records where type == "vehicle" flag matching items }`,
		// "environment" as a substring of an attribute name must not trip \benv\b.
		`detect "e" { for records where attr "environment" == "prod" flag matching items }`,
	}
	for _, src := range ok {
		if err := guardUnsafeSource(src, nil); err != nil {
			t.Errorf("benign source rejected: %v\nsrc: %s", err, src)
		}
	}
}
