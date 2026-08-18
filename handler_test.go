package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/pkg/plugin"
)

// stubHost records every RunAction call and lets the test shape the
// response via the optional reply func. Implements plugin.HostCaller.
type stubHost struct {
	calls []hostCall
	reply func(plugin_, action string, args map[string]string) (plugin.CallResult, error)
}

type hostCall struct {
	Plugin string
	Action string
	Args   map[string]string
}

func (s *stubHost) RunAction(_ context.Context, plugin_, action string, args map[string]string) (plugin.CallResult, error) {
	s.calls = append(s.calls, hostCall{Plugin: plugin_, Action: action, Args: args})
	if s.reply != nil {
		return s.reply(plugin_, action, args)
	}
	return plugin.CallResult{Content: `{"ok":true}`}, nil
}

func TestConfigure_DefaultsToWorkflowOnly(t *testing.T) {
	h := &handler{}
	if err := h.Configure(""); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.cfg.DatalevinURL != "" {
		t.Errorf("DatalevinURL should default to empty, got %q", h.cfg.DatalevinURL)
	}
}

func TestConfigure_ParsesDatalevinURL(t *testing.T) {
	h := &handler{}
	if err := h.Configure(`{"datalevin_url":"http://dl.internal:8898"}`); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.cfg.DatalevinURL != "http://dl.internal:8898" {
		t.Errorf("DatalevinURL: %q", h.cfg.DatalevinURL)
	}
}

func TestConfigure_InvalidJSON(t *testing.T) {
	h := &handler{}
	if err := h.Configure(`{not json`); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestExecuteWithCallbacks_DetectRejectedWithoutDatalevin verifies the
// workflow-only fallback: with no datalevin_url configured, a program
// containing a detect block surfaces a clear error pointing at the
// missing config rather than panicking inside the executor.
func TestExecuteWithCallbacks_DetectRejectedWithoutDatalevin(t *testing.T) {
	src := `
detect "Low stock" {
  for records where type == "stock_item"
    and attr "current_stock" <= attr "minimum_amount"
  flag matching items
  label "{item.name}: low"
  priority HIGH
}`
	h := &handler{} // no DatalevinURL
	resp := h.ExecuteWithCallbacks(context.Background(),
		plugin.Request{ID: "c1", Action: "execute_workflow", Args: map[string]string{"workflow": src}},
		&stubHost{},
	)
	if resp.Error == "" {
		t.Fatal("expected error for detect program without datalevin_url")
	}
	if !strings.Contains(resp.Error, "FactStore") && !strings.Contains(resp.Error, "detect") {
		t.Errorf("error should reference the missing FactStore: %q", resp.Error)
	}
}

func TestCapabilities(t *testing.T) {
	h := &handler{}
	caps := h.Capabilities()
	if caps.Name != "tln-plugin" {
		t.Errorf("name: %q", caps.Name)
	}
	if !caps.SupportsCallbacks {
		t.Error("SupportsCallbacks must be true for this plugin")
	}
	got := map[string]bool{}
	for _, a := range caps.Actions {
		got[a.Name] = true
	}
	if !got["execute_workflow"] || !got["check"] {
		t.Errorf("expected execute_workflow and check actions, got %+v", caps.Actions)
	}
	if !strings.Contains(caps.SystemPromptAddition, "workflow") {
		t.Error("system_prompt_addition should reference the DSL")
	}
}

func TestExecuteCheck(t *testing.T) {
	h := &handler{}

	// Valid source → ok, no error, structured {"ok":true}.
	valid := `workflow "ok" { step "s" { tool "srv" "tool" { x "1" } } }`
	resp := h.ExecuteWithCallbacks(context.Background(), plugin.Request{ID: "c1", Action: "check", Args: map[string]string{"workflow": valid}}, nil)
	if resp.Error != "" {
		t.Fatalf("valid source should not error: %q", resp.Error)
	}
	if !strings.Contains(resp.StructuredContent, `"ok":true`) {
		t.Errorf("valid source structured content: %q", resp.StructuredContent)
	}

	// Invalid source → no RPC error, but ok:false + diagnostics reported.
	resp = h.ExecuteWithCallbacks(context.Background(), plugin.Request{ID: "c2", Action: "check", Args: map[string]string{"workflow": "workflow \"x\" {"}}, nil)
	if resp.Error != "" {
		t.Fatalf("invalid source is a normal result, not an RPC error: %q", resp.Error)
	}
	if !strings.Contains(resp.StructuredContent, `"ok":false`) {
		t.Errorf("invalid source should report ok:false, got %q", resp.StructuredContent)
	}
	if resp.Content == "" {
		t.Error("invalid source should return diagnostics in content")
	}

	// Missing arg → RPC error.
	resp = h.ExecuteWithCallbacks(context.Background(), plugin.Request{ID: "c3", Action: "check", Args: map[string]string{}}, nil)
	if resp.Error == "" {
		t.Error("missing workflow arg should error")
	}
}

func TestExecuteWithCallbacks_RunsWorkflowAndForwardsMCPCalls(t *testing.T) {
	src := `
workflow "test" {
  step "create" {
    tool "hr" "create-person" {
      name "Alice"
    }
  }
}`
	host := &stubHost{
		reply: func(_, _ string, _ map[string]string) (plugin.CallResult, error) {
			return plugin.CallResult{Content: `{"result":{"id":42}}`}, nil
		},
	}
	h := &handler{}
	resp := h.ExecuteWithCallbacks(context.Background(),
		plugin.Request{ID: "c1", Action: "execute_workflow", Args: map[string]string{"workflow": src}},
		host,
	)

	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if len(host.calls) != 1 {
		t.Fatalf("expected 1 host call, got %d", len(host.calls))
	}
	if host.calls[0].Plugin != "hr" || host.calls[0].Action != "create-person" {
		t.Errorf("dispatch dest: %+v", host.calls[0])
	}
	if host.calls[0].Args["name"] != "Alice" {
		t.Errorf("args: %+v", host.calls[0].Args)
	}
	if !strings.Contains(resp.Content, "Workflow completed") {
		t.Errorf("content: %q", resp.Content)
	}
}

func TestExecuteWithCallbacks_StepResultChaining(t *testing.T) {
	src := `
workflow "chain" {
  step "create" {
    tool "hr" "create-person" {
      name "Alice"
    }
  }
  step "assign" depends_on "create" {
    tool "inv" "assign-item" {
      person_id step("create").result.id
    }
  }
}`
	host := &stubHost{
		reply: func(_, action string, _ map[string]string) (plugin.CallResult, error) {
			if action == "create-person" {
				return plugin.CallResult{Content: `{"result":{"id":42}}`}, nil
			}
			return plugin.CallResult{Content: `{"ok":true}`}, nil
		},
	}
	h := &handler{}
	resp := h.ExecuteWithCallbacks(context.Background(),
		plugin.Request{ID: "c1", Action: "execute_workflow", Args: map[string]string{"workflow": src}},
		host,
	)

	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if len(host.calls) != 2 {
		t.Fatalf("expected 2 host calls, got %d", len(host.calls))
	}
	// The second step's person_id is "42" (encoded — tln resolved
	// step("create").result.id and the tlnCaller JSON-encoded the
	// non-string value before crossing the host's string-map boundary).
	if host.calls[1].Args["person_id"] != "42" {
		t.Errorf("person_id: %q (full args: %+v)", host.calls[1].Args["person_id"], host.calls[1].Args)
	}
}

func TestExecuteWithCallbacks_HostError(t *testing.T) {
	src := `
workflow "err" {
  step "s1" {
    tool "srv" "tool" {
      x 1
    }
  }
}`
	host := &stubHost{
		reply: func(_, _ string, _ map[string]string) (plugin.CallResult, error) {
			return plugin.CallResult{}, errors.New("permission denied")
		},
	}
	h := &handler{}
	resp := h.ExecuteWithCallbacks(context.Background(),
		plugin.Request{ID: "c1", Action: "execute_workflow", Args: map[string]string{"workflow": src}},
		host,
	)
	if resp.Error == "" {
		t.Fatal("expected error from MCP failure surfacing through tln runtime")
	}
	if !strings.Contains(resp.Error, "permission denied") {
		t.Errorf("error should include host detail: %q", resp.Error)
	}
}

func TestExecuteWithCallbacks_MissingWorkflowArg(t *testing.T) {
	h := &handler{}
	resp := h.ExecuteWithCallbacks(context.Background(),
		plugin.Request{ID: "c1", Action: "execute_workflow", Args: map[string]string{}},
		&stubHost{},
	)
	if resp.Error == "" {
		t.Error("expected error when workflow arg missing")
	}
}

func TestExecuteWithCallbacks_UnknownAction(t *testing.T) {
	h := &handler{}
	resp := h.ExecuteWithCallbacks(context.Background(),
		plugin.Request{ID: "c1", Action: "nope"},
		&stubHost{},
	)
	if resp.Error == "" {
		t.Error("expected error for unknown action")
	}
}

func TestExecute_UnaryPathRefuses(t *testing.T) {
	// The host should be calling ExecuteWithCallbacks (we declare
	// SupportsCallbacks=true); a unary Execute call signals a host
	// without bidi support, so we refuse loudly rather than silently.
	h := &handler{}
	resp := h.Execute(plugin.Request{ID: "c1", Action: "execute_workflow"})
	if resp.Error == "" {
		t.Error("unary Execute should return an error")
	}
}

// Compile-time guard: handler satisfies both Handler and StreamingHandler.
var (
	_ plugin.Handler          = (*handler)(nil)
	_ plugin.StreamingHandler = (*handler)(nil)
)
