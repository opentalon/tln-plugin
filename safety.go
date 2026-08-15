package main

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
	reConnector = regexp.MustCompile(`(?m)\bconnector\s+"`)
	reEnv       = regexp.MustCompile(`(?m)\benv\s+"`)
	reIOServer  = regexp.MustCompile(`(?m)\b(?:tool|mcp)\s+"io"`)
)

// guardUnsafeSource rejects Tln source that uses a capability unsafe for
// plugin-executed (LLM-authored) workflows. It is intentionally conservative:
// over-rejection is safe, under-rejection is not.
func guardUnsafeSource(src string) error {
	switch {
	case reConnector.MatchString(src):
		return fmt.Errorf("tln-plugin: `connector` blocks are not allowed in plugin-executed workflows — external access is host-configured, not defined in workflow source")
	case reEnv.MatchString(src):
		return fmt.Errorf("tln-plugin: `env` is not allowed in plugin-executed workflows — host environment/secrets are never readable from workflow source")
	case reIOServer.MatchString(src):
		return fmt.Errorf("tln-plugin: the `io` tool server is not allowed in plugin-executed workflows — no host filesystem/stdio access; use a host-configured MCP server")
	}
	return nil
}
