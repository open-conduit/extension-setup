package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// Extension is the `setup` extension, served as a package over the
// protocol (Build). The entry type, the route and the session Work kind are
// its own; its operations join a generation's toolbelt only when an enabled
// scope has an entry (manifest entry_gated), and its actions are offered on
// a scope with an entry.
type Extension struct {
	host   atomic.Pointer[sdk.Host]
	store  storage.Store
	config inproc.ConfigHost
	tools  *Tools
	now    func() time.Time

	mu      sync.Mutex
	entries map[domain.ID]string // entry name by enabled scope
	// applied is set by each configured generation, for the applied task
	// to match against the sessions' candidates.
	applied bool
}

// BuildOptions configure New.
type BuildOptions struct {
	Now func() time.Time
	// Home is the directory setup.fs.list starts from ("" is the user's
	// home directory).
	Home string
}

// New returns the extension, served once a host binds it.
func New(o BuildOptions) *Extension {
	x := &Extension{now: o.Now}
	if x.now == nil {
		x.now = time.Now
	}
	host := func() *sdk.Host { return x.host.Load() }
	x.store, x.config = hostStore{host: host}, configHost{host: host}
	x.tools = &Tools{o: Options{Store: x.store, Accept: record, Enabled: x.enabled, Now: x.now, Config: x.config,
		OwnProbes: Probes{Home: o.Home}, Probes: hostProbes(host)}}
	return x
}

// bind keeps the host the extension is served by.
func (x *Extension) bind(h *sdk.Host) {
	if h != nil {
		x.host.Store(h)
	}
}

// AppliedTask is the task that marks a session's changes applied.
const AppliedTask = "setup-applied"

// Configure keeps each enabled scope's first `setup` entry.
func (x *Extension) Configure(_ context.Context, h *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
	x.bind(h)
	entries := map[domain.ID]string{}
	for _, e := range p.Entries {
		scope := domain.ID(e.ScopeID)
		if _, seen := entries[scope]; e.Type != Type || seen || ValidateExtension(e.Config, nil) != nil {
			continue
		}
		entries[scope] = e.Name
	}
	x.mu.Lock()
	x.entries, x.applied = entries, true
	x.mu.Unlock()
	return protocol.ConfigureResult{}, nil
}

// checkApplied is the applied task: after a generation is configured, it
// marks the sessions whose candidate it is as applied.
func (x *Extension) checkApplied(ctx context.Context, h *sdk.Host) error {
	x.bind(h)
	x.mu.Lock()
	due := x.applied
	x.applied = false
	x.mu.Unlock()
	if !due {
		return nil
	}
	if err := MarkApplied(ctx, x.store, x.config, record, x.now); err != nil {
		x.mu.Lock()
		x.applied = true
		x.mu.Unlock()
		return err
	}
	return nil
}

func (x *Extension) entry(scope domain.ID) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	name, ok := x.entries[scope]
	return name, ok
}

func (x *Extension) enabled(scope domain.ID) bool {
	_, ok := x.entry(scope)
	return ok
}

// actions is the action provider on a scope with a `setup` entry.
func (x *Extension) actions(scope domain.ID) (Actions, bool) {
	if !x.enabled(scope) {
		return Actions{}, false
	}
	return Actions{Store: x.store, Now: x.now, Config: x.config}, true
}

// validate checks the extension's and route's config, and the binding a
// `setup` route binds.
func validate(_ context.Context, p protocol.ValidateParams) error {
	switch {
	case p.Kind == protocol.ValidateExtension && p.Type == Type:
		return ValidateExtension(p.Config, p.SecretNames)
	case p.Kind == protocol.ValidateRoute && p.Type == Type:
		return ValidateRoute(p.Config, p.SecretNames)
	case p.Kind == protocol.ValidateBinding && p.Type == Type && p.Binding != nil:
		return ValidateBinding(p.Binding.Contract())
	}
	return nil
}

// Build is the extension as the SDK serves it.
func (x *Extension) Build() *sdk.Extension {
	ext := &sdk.Extension{Manifest: Manifest(), Validate: validate, Configure: x.Configure,
		Routes: map[string]extension.RouteHandler{Type: Route{}}}
	for _, op := range x.tools.Registration().Operations {
		ext.Operations = append(ext.Operations, x.bound(op))
	}
	ext.Tasks = map[string]func(context.Context, *sdk.Host) error{AppliedTask: x.checkApplied}
	ext.Render = RenderEvent
	ext.Actions = func(ctx context.Context, h *sdk.Host, t extension.ActionTarget) ([]extension.Action, error) {
		x.bind(h)
		a, ok := x.actions(t.Scope.ID)
		if !ok {
			return nil, nil
		}
		return a.Actions(ctx, t)
	}
	ext.Act = func(ctx context.Context, h *sdk.Host, c extension.ActionCall) (extension.ActionPlan, error) {
		x.bind(h)
		a, ok := x.actions(c.Target.Scope.ID)
		if !ok {
			return extension.ActionPlan{}, &extension.OperationError{Code: "not_enabled", Message: "this scope has no setup entry"}
		}
		return a.Invoke(ctx, c)
	}
	ext.Panels = func(ctx context.Context, h *sdk.Host, t extension.ActionTarget) ([]extension.Panel, error) {
		x.bind(h)
		a, ok := x.actions(t.Scope.ID)
		if !ok {
			return nil, nil
		}
		return a.Panels(ctx, t)
	}
	ext.ConfigCandidate = func(ctx context.Context, h *sdk.Host, w domain.Work) (extension.Candidate, error) {
		x.bind(h)
		return x.ConfigCandidate(ctx, w)
	}
	ext.Setups = map[string]extension.Setup{Type: Setup{}}
	return ext
}

// bound binds the host serving each call before the operation runs.
func (x *Extension) bound(op extension.Operation) extension.Operation {
	if f := op.Handler; f != nil {
		op.Handler = func(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
			x.bind(sdk.HostFrom(ctx))
			return f(ctx, a, in)
		}
	}
	if f := op.Wait; f != nil {
		op.Wait = func(ctx context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
			x.bind(sdk.HostFrom(ctx))
			return f(ctx, a, in)
		}
	}
	if f := op.Emit; f != nil {
		op.Emit = func(ctx context.Context, a extension.Authority, in domain.Payload) (extension.EmitPlan, error) {
			x.bind(sdk.HostFrom(ctx))
			return f(ctx, a, in)
		}
	}
	return op
}

// RenderEvent renders the operator's input for the session's agent.
func RenderEvent(e domain.Event) (string, bool) {
	switch e.Type {
	case TypeStart:
		var s Start
		if e.Data.Schema != SchemaStart || json.Unmarshal(e.Data.Data, &s) != nil {
			return "", false
		}
		out := "### The operator started a setup session"
		if s.Model != "" {
			out += " (on " + s.Model + ")"
		}
		out += "\nSpeak to them with `" + OpSay + "` and ask with `" + OpAsk + "`: they see nothing else you write. Look around with `" + OpProbe + "` and `" + OpRead + "`, and change the config only by proposing with `" + OpPropose + "`: they approve and apply."
		if len(s.Grants) > 0 {
			out += "\nThey turned on: " + strings.Join(s.Grants, ", ") + ". Even with a shell, change the config only by proposing."
		} else {
			out += "\nThey turned on nothing beyond talking: no shell, no MCP servers but Open Conduit's."
		}
		return out, true
	case TypeMessage:
		var m Message
		if e.Data.Schema != SchemaMessage || json.Unmarshal(e.Data.Data, &m) != nil || m.Role != RoleOperator {
			return "", false
		}
		return "### The operator says\n" + m.Text, true
	case TypeAnswer:
		var a Answer
		if e.Data.Schema != SchemaAnswer || json.Unmarshal(e.Data.Data, &a) != nil {
			return "", false
		}
		return "### The operator answered your question\n" + a.Answer, true
	case TypeDecision:
		return decisionRender(e)
	case TypeChecks:
		var c Checks
		if e.Data.Schema != SchemaChecks || json.Unmarshal(e.Data.Data, &c) != nil {
			return "", false
		}
		if c.Valid {
			return fmt.Sprintf("### The operator validated the approved changes\nThe %d approved changes are valid.", c.Approved), true
		}
		out := fmt.Sprintf("### The operator validated the approved changes\n%d problems:", len(c.Diagnostics))
		for _, d := range c.Diagnostics {
			out += "\n- " + d.Path + ": " + d.Code + ": " + d.Message
		}
		return out, true
	case TypeApplied:
		var a Applied
		if e.Data.Schema != SchemaApplied || json.Unmarshal(e.Data.Data, &a) != nil {
			return "", false
		}
		return "### The operator applied your changes\nGeneration " + a.Generation + " is active with " + strings.Join(a.Changes, ", ") + ". Check it, then tell them what is left.", true
	}
	return "", false
}
