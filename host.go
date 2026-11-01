package setup

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// configHost is inproc.ConfigHost over the host's config service
// (permission config.read: config.base, config.edit, config.validate and
// config.trace). A handle the host no longer holds comes back not_found,
// which is inproc.ErrConfigHandle here as it is in-process.
type configHost struct {
	host func() *sdk.Host
}

var _ inproc.ConfigHost = configHost{}

func configErr(err error) error {
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.Code == protocol.CodeNotFound {
		return inproc.ErrConfigHandle
	}
	return err
}

func (c configHost) Base(ctx context.Context) (inproc.ConfigBase, error) {
	h := c.host()
	if h == nil {
		return inproc.ConfigBase{}, errNoHost
	}
	b, err := h.ConfigBase(ctx)
	if err != nil {
		return inproc.ConfigBase{}, configErr(err)
	}
	return inproc.ConfigBase{Generation: b.Generation, Hash: b.Hash, Handle: b.Handle, Text: []byte(b.Text), Folders: b.Folders}, nil
}

func (c configHost) Edit(ctx context.Context, handle, op, pointer string, value json.RawMessage) (inproc.ConfigEdited, error) {
	h := c.host()
	if h == nil {
		return inproc.ConfigEdited{}, errNoHost
	}
	e, err := h.ConfigEdit(ctx, handle, op, pointer, value)
	if err != nil {
		// The host's edit reasons are fixed and secret-free, as in-process.
		var pe *protocol.Error
		if errors.As(err, &pe) && pe.Code == protocol.CodeInvalid {
			return inproc.ConfigEdited{}, errors.New(pe.Message)
		}
		return inproc.ConfigEdited{}, configErr(err)
	}
	return inproc.ConfigEdited{Handle: e.Handle, Text: []byte(e.Text), Before: e.Before}, nil
}

func (c configHost) Validate(ctx context.Context, handle string) (string, []inproc.ConfigDiagnostic, error) {
	h := c.host()
	if h == nil {
		return "", nil, errNoHost
	}
	v, err := h.ConfigValidate(ctx, handle)
	if err != nil {
		return "", nil, configErr(err)
	}
	var diags []inproc.ConfigDiagnostic
	for _, d := range v.Diagnostics {
		diags = append(diags, inproc.ConfigDiagnostic{Path: d.Path, Code: d.Code, Message: d.Message})
	}
	return v.Hash, diags, nil
}

func (c configHost) Trace(ctx context.Context, handle, scope, sample string) (inproc.CandidateTrace, error) {
	h := c.host()
	if h == nil {
		return inproc.CandidateTrace{}, errNoHost
	}
	t, err := h.ConfigTrace(ctx, handle, scope, sample)
	if err != nil {
		return inproc.CandidateTrace{}, configErr(err)
	}
	out := inproc.CandidateTrace{Sample: t.Sample,
		Effect: inproc.CandidateEffect{Kind: t.Effect.Kind, WorkID: t.Effect.WorkID, Binding: t.Effect.Binding, Detail: t.Effect.Detail}}
	for _, s := range t.Samples {
		out.Samples = append(out.Samples, inproc.CandidateSample{ID: s.ID, Title: s.Title})
	}
	for _, r := range t.Routes {
		out.Routes = append(out.Routes, inproc.CandidateRoute{Route: r.Route, Handler: r.Handler, Binding: r.Binding, Verdict: r.Verdict, Reason: r.Reason})
	}
	if t.Error != "" {
		return out, errors.New(t.Error)
	}
	return out, nil
}

// probeListTimeout bounds asking the host for its probes.
const probeListTimeout = 5 * time.Second

// hostProbes are the probes the host, its other extensions and its
// executors offer (permission probes: probe.list and probe.run), by full
// name. Each runs on the host.
func hostProbes(host func() *sdk.Host) func(context.Context) map[string]extension.SetupProbe {
	return func(ctx context.Context) map[string]extension.SetupProbe {
		h := host()
		if h == nil {
			return nil
		}
		cx, cancel := context.WithTimeout(ctx, probeListTimeout)
		list, err := h.Probes(cx)
		cancel()
		if err != nil {
			return nil
		}
		out := map[string]extension.SetupProbe{}
		for _, info := range list {
			name := info.Name
			p := extension.SetupProbe{Name: name, Summary: info.Summary,
				Run: func(ctx context.Context, args map[string]string) (extension.ProbeResult, error) {
					r, err := h.Probe(ctx, name, args)
					if err != nil {
						return extension.ProbeResult{}, err
					}
					if r.Error != "" {
						return extension.ProbeResult{Command: r.Command}, errors.New(r.Error)
					}
					var output any
					if len(r.Output) > 0 {
						output = r.Output
					}
					return extension.ProbeResult{Command: r.Command, Output: output}, nil
				}}
			for _, a := range info.Args {
				p.Args = append(p.Args, extension.ProbeArg{Name: a.Name, Summary: a.Summary, Required: a.Required})
			}
			out[name] = p
		}
		return out
	}
}
