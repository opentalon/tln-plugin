package runtime

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/opentalon/opentalon/pkg/plugin"
	"github.com/opentalon/tln-language/pkg/tln"
)

//go:embed tln_language.txt
var tlnDoc string

// config is the JSON shape this plugin accepts from the host's
// `plugins.<name>.config:` block. All fields are optional.
type config struct {
	// DatalevinURL points at a Datalevin HTTP server. When set, the
	// plugin uses tln.Run (full language: workflow + detect + queries
	// + ML primitives). When empty, only workflow-only programs run
	// via tln.RunWorkflow — detect-bearing programs return
	// ErrRequiresFactStore from the SDK.
	DatalevinURL string `json:"datalevin_url"`

	// RulesDir is the filesystem path where preauthored Tln rules
	// live (one .tln file per rule). The admin HTTP API reads and
	// writes here; rules loaded at startup become advertised actions
	// (the rules loader is a follow-up). Required when admin_token is
	// set; otherwise the API can't store anything it accepts.
	RulesDir string `json:"rules_dir"`

	// AdminToken guards the admin HTTP API (rule CRUD).
	// Every request must carry `Authorization: Bearer <token>` with a
	// matching value. Empty disables the admin API entirely — without
	// a token there's no auth model, so we refuse to serve.
	AdminToken string `json:"admin_token"`
}

// handler implements pkg/plugin.StreamingHandler. The plugin advertises
// SupportsCallbacks=true so the host dispatches over the bidirectional
// ExecuteBidi stream and passes a live HostCaller — that's how every
// MCP step inside a Tln program flows back through the host's
// expert system (executeCall → policy → observability → credentials).
type handler struct {
	cfg config
	// pluginOpts are the bundle plugin registrations (tln.WithPlugin) an
	// external bundle passed to Serve; runTln appends them to every program's
	// options. connectorNames is the matching set of names the guard allows in
	// `connector "…" via <name>`. Both empty for the plain build.
	pluginOpts     []tln.Option
	connectorNames map[string]bool
}

// Configure parses the host-supplied config block. Empty configJSON is
// valid — the plugin runs in workflow-only mode. Side effect: starts
// the admin HTTP server in a goroutine when both OPENTALON_HTTP_PORT
// (set by the host's plugin loader) and admin_token (from this config)
// are present. The server outlives Configure; it shuts down when the
// process exits alongside the gRPC server.
func (h *handler) Configure(configJSON string) error {
	if configJSON != "" {
		if err := json.Unmarshal([]byte(configJSON), &h.cfg); err != nil {
			return fmt.Errorf("tln-plugin: parse config: %w", err)
		}
	}
	if h.cfg.DatalevinURL != "" {
		slog.Info("tln-plugin: datalevin backend configured", "url", h.cfg.DatalevinURL)
	} else {
		slog.Info("tln-plugin: workflow-only mode (no datalevin_url configured)")
	}

	port := os.Getenv("OPENTALON_HTTP_PORT")
	switch {
	case port == "" && h.cfg.AdminToken != "":
		// Operator gave us a token but no HTTP grant from the host —
		// the API is unreachable. Loud warning so the misconfiguration
		// is obvious, but don't error (gRPC still works).
		slog.Warn("tln-plugin: admin_token set but OPENTALON_HTTP_PORT not granted; admin API disabled")
	case port != "" && h.cfg.AdminToken == "":
		// Inverse: host granted HTTP but no token. We refuse to serve
		// an auth-less API on principle — there's no audience for
		// uncredentialed mutation of rules.
		slog.Warn("tln-plugin: OPENTALON_HTTP_PORT granted but no admin_token in config; admin API refused (set admin_token to enable)")
	case port != "" && h.cfg.AdminToken != "":
		if err := h.startAdminServer(port); err != nil {
			return fmt.Errorf("tln-plugin: admin server: %w", err)
		}
	}
	return nil
}

// startAdminServer launches the management HTTP server in a goroutine.
// It hosts the rule CRUD API only — tln-plugin is a language gateway,
// not a data store, so it does not expose a fact-seeding API.
func (h *handler) startAdminServer(port string) error {
	// rules_dir is optional: the /check validator is a pure function of the
	// posted source and needs no store. When rules_dir is unset the /rules
	// CRUD endpoints return 503 (a.rules == nil) but /check still serves.
	admin := &adminServer{token: h.cfg.AdminToken, connectors: h.connectorNames}
	if h.cfg.RulesDir != "" {
		admin.rules = &ruleStore{RootDir: h.cfg.RulesDir, connectors: h.connectorNames}
	}
	srv := &http.Server{
		Addr:              "127.0.0.1:" + port,
		Handler:           admin.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		slog.Info("tln-plugin: admin server listening", "addr", srv.Addr, "rules_dir", h.cfg.RulesDir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("tln-plugin: admin server", "error", err)
		}
	}()
	return nil
}

func (h *handler) Capabilities() plugin.CapabilitiesMsg {
	return plugin.CapabilitiesMsg{
		Name:        "tln-plugin",
		Description: "Executes Tln workflow blocks — deterministic multi-step MCP chains generated by the LLM for batch operations.",
		Actions: []plugin.ActionMsg{
			{
				Name:        "execute_workflow",
				Description: "Execute a Tln workflow block as a single atomic operation. See system_prompt_addition for the DSL syntax and examples.",
				Parameters: []plugin.ParameterMsg{
					{
						Name:        "workflow",
						Description: "Tln workflow source: a single `workflow \"...\" { step \"...\" { mcp \"<server>\" \"<tool>\" { ... } } }` block.",
						Type:        "string",
						Required:    true,
					},
				},
			},
			{
				Name:        "check",
				Description: "Validate Tln source without executing it (lex → parse → resolve → validate → plan). Returns {\"ok\":true} for valid source, or the compile diagnostics for invalid source. No MCP calls, no side effects — safe for validating machine-generated source before storing or running it.",
				ReadOnly:    true,
				Parameters: []plugin.ParameterMsg{
					{
						Name:        "workflow",
						Description: "Tln source to validate.",
						Type:        "string",
						Required:    true,
					},
				},
			},
			{
				Name:        "evaluate",
				Description: "Reactively evaluate Tln source against a set of facts. Hydrates a session from the prior snapshot, asserts the new facts, fires any matching on-blocks (running their workflow bodies — including MCP steps), and returns which blocks fired plus the updated snapshot. Stateless: all state is carried in the request. Used to run event-driven watchers a tick at a time.",
				Parameters: []plugin.ParameterMsg{
					{Name: "source", Description: "Tln source, typically containing on-blocks and the workflows they fire.", Type: "string", Required: true},
					{Name: "facts", Description: `JSON array of facts to assert, e.g. [{"record_id":"1","attribute":"current_stock","value":8}].`, Type: "string", Required: true},
					{Name: "snapshot", Description: `Optional prior store snapshot to hydrate before asserting, e.g. {"1":{"current_stock":15}}. Re-asserting an unchanged value fires nothing.`, Type: "string", Required: false},
				},
			},
		},
		SystemPromptAddition: tlnDoc,
		SupportsCallbacks:    true,
	}
}

// Execute is the unary path — never called by the host for this plugin
// because we declare SupportsCallbacks=true above. Implemented so the
// type satisfies plugin.Handler, but returns a clear error so a
// misconfigured host (e.g. older opentalon without bidi support)
// surfaces the mismatch loudly instead of silently mis-running.
func (h *handler) Execute(req plugin.Request) plugin.Response {
	return plugin.Response{
		CallID: req.ID,
		Error:  "tln-plugin requires the host to dispatch over ExecuteBidi (host opentalon >= v0.0.18). The unary path is not supported.",
	}
}

// ExecuteWithCallbacks is the bidi path. It receives a live HostCaller
// the tln runtime uses to dispatch each `mcp "<server>" "<tool>"`
// step back through the host's orchestrator.
func (h *handler) ExecuteWithCallbacks(ctx context.Context, req plugin.Request, host plugin.HostCaller) plugin.Response {
	switch req.Action {
	case "execute_workflow":
		return h.execWorkflow(ctx, req, host)
	case "check":
		// check is a pure compile — no MCP, no HostCaller needed.
		return h.execCheck(req)
	case "evaluate":
		return h.execEvaluate(ctx, req, host)
	default:
		return plugin.Response{CallID: req.ID, Error: "unknown action: " + req.Action}
	}
}

func (h *handler) execWorkflow(ctx context.Context, req plugin.Request, host plugin.HostCaller) plugin.Response {
	src := req.Args["workflow"]
	if src == "" {
		return plugin.Response{CallID: req.ID, Error: "workflow argument is required; pass a Tln workflow block as a string"}
	}

	slog.Info("tln-plugin: execute_workflow",
		"call_id", req.ID,
		"workflow_len", len(src),
		"datalevin", h.cfg.DatalevinURL != "")

	result, err := h.runTln(ctx, src, req.ID, host)
	if err != nil {
		return plugin.Response{
			CallID: req.ID,
			Error:  fmt.Sprintf("tln-plugin: %v", err),
		}
	}

	content, structured := formatResult(result)
	return plugin.Response{
		CallID:            req.ID,
		Content:           content,
		StructuredContent: structured,
	}
}

// execCheck validates Tln source without executing it. Invalid source
// is a normal result (reported as diagnostics), not an RPC error — the
// caller (e.g. an LLM authoring a program) relays the diagnostics and
// retries. Pure compile: no HostCaller, no MCP, no side effects.
func (h *handler) execCheck(req plugin.Request) plugin.Response {
	src := req.Args["workflow"]
	if src == "" {
		return plugin.Response{CallID: req.ID, Error: "workflow argument is required; pass Tln source as a string"}
	}
	if err := guardUnsafeSource(src, h.connectorNames); err != nil {
		return plugin.Response{CallID: req.ID, Error: err.Error()}
	}

	// HasReactiveRules both validates (same pipeline as Check) and reports
	// whether the program has on/detect blocks, so callers (opentalon-agents)
	// can route a domain event: reactive → evaluate against facts, workflow-only
	// → run imperatively.
	reactive, err := tln.HasReactiveRules(src, tln.WithFilename("check:"+req.ID))
	if err == nil {
		structured, _ := json.Marshal(map[string]any{"ok": true, "reactive": reactive})
		return plugin.Response{
			CallID:            req.ID,
			Content:           "ok: source is valid Tln.",
			StructuredContent: string(structured),
		}
	}

	payload := map[string]any{"ok": false, "error": err.Error()}
	var ce *tln.CompileError
	if errors.As(err, &ce) {
		payload["stage"] = ce.Stage
	}
	structured, _ := json.Marshal(payload)
	return plugin.Response{
		CallID:            req.ID,
		Content:           err.Error(),
		StructuredContent: string(structured),
	}
}

// evalFact is one fact to assert, as received in the `facts` JSON arg.
type evalFact struct {
	RecordID  string `json:"record_id"`
	Attribute string `json:"attribute"`
	Value     any    `json:"value"`
}

// evalFiring is one fired on-block, as returned in the `evaluate` result.
type evalFiring struct {
	OnBlock string `json:"on_block"`
	Ref     string `json:"ref,omitempty"`
	RefKind string `json:"ref_kind,omitempty"`
	Error   string `json:"error,omitempty"`
}

type evalResponse struct {
	OK       bool                   `json:"ok"`
	Firings  []evalFiring           `json:"firings"`
	Snapshot map[int]map[string]any `json:"snapshot"`
}

// execEvaluate reactively evaluates Tln source against a set of facts.
// It hydrates a fresh session from the prior snapshot (so re-asserting an
// unchanged value fires nothing), asserts the new facts, runs any matching
// on-blocks (their workflow bodies dispatch MCP steps back through the
// host), and returns which blocks fired plus the updated snapshot. Fully
// stateless: no session is persisted between calls — the caller carries
// the snapshot. tln-plugin stays agent-agnostic; this is a generic
// reactive-evaluation primitive.
func (h *handler) execEvaluate(ctx context.Context, req plugin.Request, host plugin.HostCaller) plugin.Response {
	src := req.Args["source"]
	if src == "" {
		return plugin.Response{CallID: req.ID, Error: "source argument is required; pass Tln source as a string"}
	}
	if err := guardUnsafeSource(src, h.connectorNames); err != nil {
		return plugin.Response{CallID: req.ID, Error: err.Error()}
	}

	var factsIn []evalFact
	if raw := req.Args["facts"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &factsIn); err != nil {
			return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("facts must be a JSON array of {record_id,attribute,value}: %v", err)}
		}
	}
	facts := make([]tln.Fact, 0, len(factsIn))
	for _, f := range factsIn {
		facts = append(facts, tln.Fact{RecordID: f.RecordID, Attribute: f.Attribute, Value: f.Value})
	}

	// Hydrate a fresh store from the prior snapshot BEFORE creating the
	// session, so replaying already-known facts fires nothing.
	store := tln.NewMemoryStore()
	if raw := req.Args["snapshot"]; raw != "" && raw != "{}" {
		var snap map[string]map[string]any
		if err := json.Unmarshal([]byte(raw), &snap); err != nil {
			return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("snapshot must be a JSON object of {record_id:{attr:value}}: %v", err)}
		}
		var hydrate []tln.Fact
		for id, attrs := range snap {
			for attr, val := range attrs {
				hydrate = append(hydrate, tln.Fact{RecordID: id, Attribute: attr, Value: val})
			}
		}
		if len(hydrate) > 0 {
			if err := store.Assert(ctx, hydrate); err != nil {
				return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("tln-plugin: hydrate snapshot: %v", err)}
			}
		}
	}

	s, err := tln.NewSession(src,
		tln.WithToolResolver(&tlnCaller{host: host}),
		tln.WithFactStore(store),
		tln.WithFilename("eval:"+req.ID))
	if err != nil {
		// Invalid source (should have been caught by `check` at authoring time).
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("tln-plugin: %v", err)}
	}
	defer s.Close()

	slog.Info("tln-plugin: evaluate", "call_id", req.ID, "facts", len(facts), "source_len", len(src))

	firings, err := s.Assert(ctx, facts)
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("tln-plugin: assert: %v", err)}
	}

	out := evalResponse{OK: true, Firings: make([]evalFiring, 0, len(firings)), Snapshot: s.Snapshot()}
	for _, f := range firings {
		ef := evalFiring{OnBlock: f.OnBlock, Ref: f.Ref, RefKind: f.RefKind}
		if f.Err != nil {
			ef.Error = f.Err.Error()
		}
		out.Firings = append(out.Firings, ef)
	}

	structured, err := json.Marshal(out)
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("tln-plugin: encode result: %v", err)}
	}
	return plugin.Response{
		CallID:            req.ID,
		Content:           fmt.Sprintf("Evaluated: %d firing(s).", len(out.Firings)),
		StructuredContent: string(structured),
	}
}

// runTln picks the right SDK entry point based on whether a Datalevin
// backend is configured. With one, tln.Run handles the full language
// (workflows + detect + queries + ML primitives). Without one we fall
// back to tln.RunWorkflow which is faster but rejects detect-bearing
// programs with tln.ErrRequiresFactStore — the LLM gets a clear error
// pointing at the missing config rather than a panic.
func (h *handler) runTln(ctx context.Context, src, callID string, host plugin.HostCaller) (*tln.Result, error) {
	if err := guardUnsafeSource(src, h.connectorNames); err != nil {
		return nil, err
	}
	opts := []tln.Option{
		tln.WithToolResolver(&tlnCaller{host: host}),
		tln.WithFilename("workflow:" + callID),
	}
	// Bundle plugins wired in by Serve (e.g. the asp solver). Empty for the
	// plain build. Declared in an external bundle's mod.tln, never here.
	opts = append(opts, h.pluginOpts...)
	if h.cfg.DatalevinURL != "" {
		// tln-language v0.13 dropped the built-in datalevin-URL fact store
		// (WithDatalevinURL); a Datalevin-backed store is now supplied as an
		// external store plugin via WithFactStore. Fail loudly rather than
		// silently running detect rules against no store.
		return nil, fmt.Errorf("datalevin_url is set but not supported by this build "+
			"(tln-language v0.13 requires a datalevin store plugin via WithFactStore): %s", h.cfg.DatalevinURL)
	}
	return tln.RunWorkflow(ctx, src, opts...)
}

// tlnCaller bridges tln-language's ToolResolver interface (args
// map[string]any → any) to the host SDK's HostCaller (args
// map[string]string → CallResult). Each side carries the data the
// other doesn't natively understand, so we JSON-encode the args on
// the way out and parse the reply on the way back.
type tlnCaller struct {
	host plugin.HostCaller
}

func (c *tlnCaller) Call(ctx context.Context, server, tool string, args map[string]any) (any, error) {
	// Args cross the gRPC boundary as map[string]string per the
	// existing tool-call contract. The host re-parses them when
	// delivering to the target plugin; for tln arguments that are
	// non-strings (numbers, bools, nested maps) we JSON-encode the
	// whole arg value so the receiver can decode back if needed.
	encoded := make(map[string]string, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok {
			encoded[k] = s
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encode arg %q: %w", k, err)
		}
		encoded[k] = string(b)
	}

	res, err := c.host.RunAction(ctx, server, tool, encoded)
	if err != nil {
		return nil, err
	}

	// Prefer structured content when the upstream tool produced it
	// (that's the schema-validated JSON record per MCP 2025-06). Fall
	// back to the human-readable content string and try to JSON-decode
	// it so workflow expressions like step("find").result.items work.
	payload := res.StructuredContent
	if payload == "" {
		payload = res.Content
	}
	if payload == "" {
		return map[string]any{}, nil
	}

	var parsed any
	if err := json.Unmarshal([]byte(payload), &parsed); err == nil {
		return parsed, nil
	}
	return map[string]any{"text": payload}, nil
}

// formatResult renders a workflow run as a human-readable summary
// (Content) and a JSON payload (StructuredContent) carrying the
// per-step trace. The summary is what the LLM sees; the structured
// blob is for clients that want the raw step-by-step view.
func formatResult(r *tln.Result) (string, string) {
	type stepEntry struct {
		Type   string `json:"type"`
		Name   string `json:"name"`
		Output any    `json:"output,omitempty"`
	}
	type blockEntry struct {
		Steps []stepEntry `json:"steps"`
	}

	blocks := map[string]blockEntry{}
	totalSteps := 0
	for name, b := range r.Blocks {
		be := blockEntry{}
		for _, s := range b.Steps {
			be.Steps = append(be.Steps, stepEntry{Type: s.Type, Name: s.Name, Output: s.Output})
			totalSteps++
		}
		blocks[name] = be
	}
	summary := fmt.Sprintf("Workflow completed: %d block(s), %d step(s).", len(blocks), totalSteps)
	if totalSteps == 0 {
		summary = "Workflow executed (no steps)."
	}

	out, err := json.Marshal(map[string]any{"blocks": blocks})
	if err != nil {
		return summary, ""
	}
	return summary, string(out)
}
