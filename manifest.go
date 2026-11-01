package setup

import (
	"context"
	"encoding/json"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// Version is this package's own version.
const Version = "0.1.0"

// Command is the package's executable, relative to its directory.
const Command = "setup-extension"

// PlaybookDir is the package directory of its playbooks: setup.md is the
// setup playbook, which a session's binding (`playbook: setup`) starts on.
const PlaybookDir = "playbooks"

// recorded is the loss every setup family states: the session's own record,
// written once as it happens, with no source to re-derive it from.
const recorded = "Recorded once by the setup session as it happens; nothing re-derives it."

// families are the Event types setup records or its actions produce.
var families = []string{TypeStart, TypeMessage, TypeQuestion, TypeAnswer, TypeProposal, TypeDecision, TypeChecks, TypeApplied, TypeProbe}

// schemas are the payload schema bases setup writes, each at version 1.
var schemas = []string{SchemaStart, SchemaMessage, SchemaQuestion, SchemaAnswer, SchemaProposal, SchemaDecision, SchemaChecks, SchemaApplied, SchemaProbe, SchemaContext, SchemaTool}

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	info := Setup{}.SetupInfo()
	p := protocol.Provides{
		Extensions: []protocol.ExtensionDecl{{Type: Type}},
		Routes:     []protocol.RouteDecl{{Handler: Type, Config: true, Binding: true, Samples: true}},
		WorkKinds:  []protocol.WorkKindDecl{{Kind: WorkKind}},
		EntryGated: true,
		Tasks:      []protocol.TaskDecl{{Name: AppliedTask, IntervalSeconds: 1}},
		Setup:      &protocol.SetupDecl{Type: info.Type, Summary: info.Summary, Usage: info.Usage, FirstRun: info.FirstRun},
		Actions:    true,
		Panels:     true,
		// Core draws the page and reviews and applies the change set.
		ConfigCandidate: true,
		Render:          true,
		Playbooks:       PlaybookDir,
	}
	for _, pg := range (&Extension{}).Pages() {
		p.Pages = append(p.Pages, protocol.PageDecl{ID: pg.ID, Title: pg.Title, Summary: pg.Summary, Entry: pg.Entry, WorkKind: pg.WorkKind, Start: pg.Start, Compose: pg.Compose, Route: pg.Route})
	}
	for _, f := range families {
		p.Families = append(p.Families, protocol.Family{Type: f, Loss: recorded})
	}
	for _, s := range schemas {
		p.Schemas = append(p.Schemas, protocol.SchemaDecl{Name: s[:len(s)-len(".v1")], Versions: []int{1}})
	}
	for _, op := range (&Tools{}).Registration().Operations {
		kind := protocol.OperationHandler
		switch {
		case op.Wait != nil:
			kind = protocol.OperationWait
		case op.Emit != nil:
			kind = protocol.OperationEmit
		}
		p.Operations = append(p.Operations, protocol.OperationDecl{Name: op.Name, Kind: kind, Commit: kind == protocol.OperationEmit, Guidance: op.Guidance})
	}
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Type, Version: Version,
		Summary:     "The web setup assistant: a Claude Code session that looks at this machine and proposes config for the operator to approve and apply.",
		Command:     []string{Command},
		Permissions: []string{protocol.PermissionConfigRead, protocol.PermissionProbes},
		Provides:    p}
}

// CertifyScope is the scope the certification fixture configures.
const CertifyScope = "setup-certify"

// Certification is the package's certification.json: an inline fixture
// with one `setup` entry, the route starting a session and skipping what
// isn't the operator's, and operations refusing bad input before they
// touch anything.
func Certification() protocol.CertificationFile {
	scope := domain.Scope{ID: CertifyScope, Name: CertifyScope}
	key := SessionKey(CertifyScope, "certify")
	start := extension.RouteInput{Scope: scope, Route: domain.Route{Name: Type, Handler: Type, Binding: "agent"},
		Event: domain.AcceptedEvent{Sequence: 1, Event: domain.Event{ID: "certify-start", Type: TypeStart, Source: extension.ActionSource, CorrelationKey: &key,
			Data: payload(SchemaStart, Start{Model: "opus"})}}}
	stray := start
	stray.Event.Event.ID, stray.Event.Event.Type, stray.Event.Event.Source = "certify-stray", TypeMessage, Type
	var cases []protocol.RouteCase
	for _, in := range []extension.RouteInput{start, stray} {
		want, _ := Route{}.Match(context.Background(), in)
		cases = append(cases, protocol.RouteCase{Handler: Type, Input: protocol.FromRouteInput(in), Want: protocol.FromRouteDecision(want)})
	}
	bad := func(op string, input string) protocol.OperationCase {
		return protocol.OperationCase{Operation: op, Input: protocol.Payload{Data: json.RawMessage(input)}, Error: "invalid_input"}
	}
	return protocol.CertificationFile{Fixture: &protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: CertifyScope, Type: Type, Name: Type, Config: json.RawMessage(`{}`)}},
		Routes:  cases,
		Operations: []protocol.OperationCase{
			bad(OpPropose, `{"op":"add","pointer":"no-slash","why":"certify"}`),
			bad(OpRead, `{"unknown":true}`),
			bad(OpTrace, `{"scope":"setup"}`),
			bad(OpWithdraw, `{"unknown":true}`),
		},
	}}
}
