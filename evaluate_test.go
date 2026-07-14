package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/opentalon/opentalon/pkg/plugin"
)

// watcherSrc is the canonical stock watcher: fire once on the downward
// crossing below 10, running a workflow that creates a ticket via MCP.
const watcherSrc = `
on change attr "current_stock" {
  when prev_value >= 10 and new_value < 10
  workflow "Refill stock"
}
workflow "Refill stock" {
  step "ticket" {
    mcp "inventory" "create-ticket" {
      item     step("trigger").result.entity
      quantity 50
    }
  }
}`

func evaluate(t *testing.T, h *handler, host plugin.HostCaller, facts, snapshot string) evalResponse {
	t.Helper()
	resp := h.ExecuteWithCallbacks(context.Background(), plugin.Request{
		ID: "e", Action: "evaluate",
		Args: map[string]string{"source": watcherSrc, "facts": facts, "snapshot": snapshot},
	}, host)
	if resp.Error != "" {
		t.Fatalf("evaluate errored: %q", resp.Error)
	}
	var out evalResponse
	if err := json.Unmarshal([]byte(resp.StructuredContent), &out); err != nil {
		t.Fatalf("decode result %q: %v", resp.StructuredContent, err)
	}
	return out
}

func countCalls(h *stubHost, action string) int {
	n := 0
	for _, c := range h.calls {
		if c.Action == action {
			n++
		}
	}
	return n
}

func TestEvaluate_CrossingFiresAndCallsMCP(t *testing.T) {
	h := &handler{}
	host := &stubHost{}
	// prior snapshot has stock at 15; assert 8 → downward crossing.
	out := evaluate(t, h, host,
		`[{"record_id":"1","attribute":"current_stock","value":8}]`,
		`{"1":{"current_stock":15}}`)

	if len(out.Firings) != 1 {
		t.Fatalf("expected 1 firing on crossing, got %d: %+v", len(out.Firings), out.Firings)
	}
	if out.Firings[0].Ref != "Refill stock" || out.Firings[0].RefKind != "workflow" {
		t.Errorf("firing ref: %+v", out.Firings[0])
	}
	if n := countCalls(host, "create-ticket"); n != 1 {
		t.Errorf("expected exactly one create-ticket MCP call, got %d", n)
	}
	// Snapshot reflects the new value.
	if v := out.Snapshot[1]["current_stock"]; v != float64(8) {
		t.Errorf("snapshot current_stock: got %v, want 8", v)
	}
}

func TestEvaluate_IdempotentReassertNoFire(t *testing.T) {
	h := &handler{}
	host := &stubHost{}
	out := evaluate(t, h, host,
		`[{"record_id":"1","attribute":"current_stock","value":8}]`,
		`{"1":{"current_stock":8}}`)
	if len(out.Firings) != 0 {
		t.Errorf("re-asserting the same value must not fire, got %+v", out.Firings)
	}
	if n := countCalls(host, "create-ticket"); n != 0 {
		t.Errorf("no MCP call expected on unchanged value, got %d", n)
	}
}

func TestEvaluate_FirstObservationDoesNotFire(t *testing.T) {
	h := &handler{}
	host := &stubHost{}
	// No prior snapshot: asserting 8 is an initial assert (not a change),
	// so `on change` does not fire — we only know a crossing happened if
	// we saw a higher value before.
	out := evaluate(t, h, host,
		`[{"record_id":"1","attribute":"current_stock","value":8}]`, "")
	if len(out.Firings) != 0 {
		t.Errorf("first observation should not fire an on-change block, got %+v", out.Firings)
	}
	if out.Snapshot[1]["current_stock"] != float64(8) {
		t.Errorf("snapshot should record the first observation: %+v", out.Snapshot)
	}
}

func TestEvaluate_Errors(t *testing.T) {
	h := &handler{}
	host := &stubHost{}
	// Missing source.
	if resp := h.ExecuteWithCallbacks(context.Background(), plugin.Request{ID: "e", Action: "evaluate",
		Args: map[string]string{"facts": "[]"}}, host); resp.Error == "" {
		t.Error("missing source should error")
	}
	// Malformed facts.
	if resp := h.ExecuteWithCallbacks(context.Background(), plugin.Request{ID: "e", Action: "evaluate",
		Args: map[string]string{"source": watcherSrc, "facts": "{not json"}}, host); resp.Error == "" {
		t.Error("malformed facts should error")
	}
}
