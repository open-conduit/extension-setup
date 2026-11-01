package setup

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// The store is the host's, over the protocol. In-process, setup recorded
// its Events with inproc.Host.AcceptInTx inside its own transactions, so a
// change set and the Event that announces it commit together. A package
// has event.accept, which commits KV checks and records with one Event
// derived from an accepted parent. hostStore keeps that shape: a
// transaction buffers its KV writes and at most one Event, and commits
// them with event.accept when it holds an Event, else with kv.commit, in
// both cases guarded by the versions of every key it read.

// errOneEvent refuses a second Event in one transaction.
var errOneEvent = errors.New("setup: one Event per transaction")

// errNoHost is a call before the host has configured the extension.
var errNoHost = errors.New("setup: not configured yet")

// hostStore is storage.Store over the host the extension is served by.
type hostStore struct {
	host func() *sdk.Host
}

var _ storage.Store = hostStore{}

type kvKey struct{ ns, key string }

type kvRead struct {
	found   bool
	version string
}

// queued is the Event a transaction accepts when it commits.
type queued struct {
	scope, parent domain.ID
	event         domain.Event
}

type hostTx struct {
	storage.Tx // the SDK's scoped state reads
	ctx        context.Context
	h          *sdk.Host
	reads      map[kvKey]kvRead
	writes     map[kvKey][]byte // nil: delete
	order      []kvKey
	event      *queued
}

func (s hostStore) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	h := s.host()
	if h == nil {
		return errNoHost
	}
	var tx *hostTx
	if err := sdk.NewStore(h).Transact(ctx, g, func(inner storage.Tx) error {
		tx = &hostTx{Tx: inner, ctx: ctx, h: h, reads: map[kvKey]kvRead{}, writes: map[kvKey][]byte{}}
		return fn(tx)
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return tx.commit()
}

func (t *hostTx) commit() error {
	if t.event == nil && len(t.writes) == 0 {
		return nil
	}
	var checks []protocol.KVCheck
	for k, r := range t.reads {
		checks = append(checks, protocol.KVCheck{Namespace: k.ns, Key: k.key, Version: r.version, Absent: !r.found})
	}
	sort.Slice(checks, func(i, j int) bool {
		return checks[i].Namespace+"\x00"+checks[i].Key < checks[j].Namespace+"\x00"+checks[j].Key
	})
	var puts []protocol.KVPut
	var deletes []protocol.KVDelete
	for _, k := range t.order {
		if v := t.writes[k]; v == nil {
			deletes = append(deletes, protocol.KVDelete{Namespace: k.ns, Key: k.key})
		} else {
			puts = append(puts, protocol.KVPut{Namespace: k.ns, Key: k.key, Value: v})
		}
	}
	if t.event == nil {
		return t.h.Commit(t.ctx, protocol.KVCommitParams{Checks: checks, Puts: puts, Deletes: deletes})
	}
	_, _, err := t.h.Accept(t.ctx, protocol.EventAcceptParams{ScopeID: string(t.event.scope), Parent: string(t.event.parent),
		Event: protocol.FromEvent(t.event.event), Checks: checks, Records: puts, Deletes: deletes})
	return err
}

func (t *hostTx) read(k kvKey) ([]byte, bool, error) {
	if v, ok := t.writes[k]; ok {
		return v, v != nil, nil
	}
	v, version, err := t.h.Get(t.ctx, k.ns, k.key)
	if errors.Is(err, storage.ErrNotFound) {
		if _, seen := t.reads[k]; !seen {
			t.reads[k] = kvRead{}
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if _, seen := t.reads[k]; !seen {
		t.reads[k] = kvRead{found: true, version: version}
	}
	return v, true, nil
}

func (t *hostTx) Get(namespace, key string) ([]byte, error) {
	v, found, err := t.read(kvKey{namespace, key})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, storage.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (t *hostTx) write(k kvKey, v []byte) {
	if _, ok := t.writes[k]; !ok {
		t.order = append(t.order, k)
	}
	t.writes[k] = v
}

func (t *hostTx) Put(namespace, key string, value []byte) error {
	if namespace == "" || key == "" || value == nil {
		return storage.ErrInvalid
	}
	t.write(kvKey{namespace, key}, append([]byte(nil), value...))
	return nil
}

func (t *hostTx) Delete(namespace, key string) error {
	if namespace == "" || key == "" {
		return storage.ErrInvalid
	}
	t.write(kvKey{namespace, key}, nil)
	return nil
}

func (t *hostTx) List(namespace, prefix string) ([]storage.Entry, error) {
	entries, err := t.h.List(t.ctx, namespace, prefix)
	if err != nil {
		return nil, err
	}
	merged := map[string][]byte{}
	for _, e := range entries {
		k := kvKey{namespace, e.Key}
		if _, seen := t.reads[k]; !seen {
			t.reads[k] = kvRead{found: true, version: e.Version}
		}
		merged[e.Key] = e.Value
	}
	for k, v := range t.writes {
		if k.ns != namespace || !strings.HasPrefix(k.key, prefix) {
			continue
		}
		if v == nil {
			delete(merged, k.key)
		} else {
			merged[k.key] = v
		}
	}
	out := make([]storage.Entry, 0, len(merged))
	for k, v := range merged {
		out = append(out, storage.Entry{Key: k, Value: append([]byte(nil), v...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Events reads every page the host answers. The host filters each page to
// the extension's scopes after limiting it, so a short page is not the end:
// only an empty one is.
func (t *hostTx) Events(after uint64, limit int) ([]domain.AcceptedEvent, error) {
	var all []domain.AcceptedEvent
	for {
		page, err := t.h.Events(t.ctx, after, 1000)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		for _, e := range page {
			all = append(all, e)
			if limit > 0 && len(all) == limit {
				return all, nil
			}
		}
		after = page[len(page)-1].Sequence
	}
}

// LatestSequence is the last sequence of the extension's scopes: the
// watermark setup compares its sessions' Events against.
func (t *hostTx) LatestSequence() (uint64, error) {
	all, err := t.Events(0, 0)
	if err != nil || len(all) == 0 {
		return 0, err
	}
	return all[len(all)-1].Sequence, nil
}

// AcceptFunc records an Event of the extension's own families in scope,
// derived from parent, an accepted Event of the scope. It commits with the
// caller's transaction.
type AcceptFunc func(tx storage.Tx, scope, parent domain.ID, e domain.Event) error

// record is the AcceptFunc of a hostStore transaction. The host assigns the
// Event's ID, causation and execution, so they are left out.
func record(tx storage.Tx, scope, parent domain.ID, e domain.Event) error {
	t, ok := tx.(*hostTx)
	if !ok {
		return storage.ErrInvalid
	}
	if parent == "" {
		return errors.New("setup: an Event needs the accepted Event it derives from")
	}
	if t.event != nil {
		return errOneEvent
	}
	e.ID, e.CausationID, e.ExecutionID = "", "", ""
	t.event = &queued{scope: scope, parent: parent, event: e}
	return nil
}
