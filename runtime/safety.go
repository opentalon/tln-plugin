package runtime

import (
	"fmt"
	"regexp"
)

// This plugin executes Tln that is frequently authored by an LLM. That makes a
// handful of language capabilities too dangerous to allow here, because a
// prompt-injected or hallucinated workflow could use them to reach past the
// host's control:
//
//   - `connector` defines external access (endpoints, credentials) from source.
//   - `env "VAR"` reads host environment variables (secrets).
//   - the built-in `io` tool server touches the host filesystem / stdio.
//
// Legitimate workflows never need any of these: they reach the MCP servers the
// host has configured by name — `tool "<server>" "<tool>" { … }` — and every
// such call is routed back through the host's own policy / credential /
// observability path (see handler.go's tlnCaller). So we fail closed: any
// source using a forbidden capability is rejected before it is compiled or run.
var (
	// A connector declaration, capturing the optional `via <name>` target so
	// the guard can allow connectors that delegate to a trusted, compiled-in
	// bundle plugin (e.g. `connector "solver" via asp`) while still rejecting
	// inline external connectors defined from source.
	reConnector = regexp.MustCompile(`(?m)\bconnector\s+"[^"]+"\s*(?:via\s+([A-Za-z_]\w*))?`)
	reEnv       = regexp.MustCompile(`(?m)\benv\s+"`)
	reIOServer  = regexp.MustCompile(`(?m)\b(?:tool|mcp)\s+"io"`)
)

// guardUnsafeSource rejects Tln source that uses a capability unsafe for
// plugin-executed (LLM-authored) workflows. It is intentionally conservative:
// over-rejection is safe, under-rejection is not. allowedConnectors is the set
// of bundle-plugin names (from Serve) a `connector "…" via <name>` may delegate
// to; nil means no bundle plugins, so every connector is rejected.
func guardUnsafeSource(src string, allowedConnectors map[string]bool) error {
	for _, m := range reConnector.FindAllStringSubmatch(src, -1) {
		if via := m[1]; via == "" || !allowedConnectors[via] {
			return fmt.Errorf("tln-plugin: `connector` blocks are not allowed in plugin-executed workflows unless they delegate to a compiled-in bundle plugin via `via <name>` — external access is host-configured, not defined in workflow source")
		}
	}
	switch {
	case reEnv.MatchString(src):
		return fmt.Errorf("tln-plugin: `env` is not allowed in plugin-executed workflows — host environment/secrets are never readable from workflow source")
	case reIOServer.MatchString(src):
		return fmt.Errorf("tln-plugin: the `io` tool server is not allowed in plugin-executed workflows — no host filesystem/stdio access; use a host-configured MCP server")
	}
	return nil
}
