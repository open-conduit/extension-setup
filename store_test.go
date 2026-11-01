package setup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// fakeHost answers kv.get, kv.commit and event.accept from memory and keeps
// what each commit carried.
type fakeHost struct {
	mu      sync.Mutex
	kv      map[string]string
	commits []protocol.KVCommitParams
	accepts []protocol.EventAcceptParams
}

func (f *fakeHost) handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "kv.get":
		var p protocol.KVGetParams
		_ = json.Unmarshal(raw, &p)
		v, ok := f.kv[p.Namespace+"/"+p.Key]
		if !ok {
			return protocol.KVGetResult{}, nil
		}
		return protocol.KVGetResult{Found: true, Entry: protocol.KVEntry{Key: p.Key, Value: []byte(v), Version: "v-" + v}}, nil
	case "kv.commit":
		var p protocol.KVCommitParams
		_ = json.Unmarshal(raw, &p)
		f.commits = append(f.commits, p)
		return struct{}{}, nil
	case "event.accept":
		var p protocol.EventAcceptParams
		_ = json.Unmarshal(raw, &p)
		f.accepts = append(f.accepts, p)
		return protocol.EventAcceptResult{Added: true}, nil
	}
	return nil, protocol.Errorf(protocol.CodeMethodNotFound, method)
}

func newFakeHost(t *testing.T) (*fakeHost, *sdk.Host) {
	t.Helper()
	f := &fakeHost{kv: map[string]string{"setup.changesets/w1": "old"}}
	hr, ew := io.Pipe()
	er, hw := io.Pipe()
	host := jsonrpc.NewConn(hr, hw, f.handle, jsonrpc.Options{})
	ext := jsonrpc.NewConn(er, ew, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil }, jsonrpc.Options{})
	t.Cleanup(func() { _ = ew.Close(); _ = hw.Close(); <-host.Done(); <-ext.Done() })
	return f, sdk.NewHost(ext)
}

// A transaction with an Event commits through event.accept, its writes and
// the versions it read going with the Event; one without commits KV alone.
func TestHostStoreCommits(t *testing.T) {
	f, h := newFakeHost(t)
	s := hostStore{host: func() *sdk.Host { return h }}
	ctx := context.Background()
	err := s.Transact(ctx, storage.Guard{}, func(tx storage.Tx) error {
		if v, err := tx.Get(ChangeSets, "w1"); err != nil || string(v) != "old" {
			t.Fatalf("get: %q %v", v, err)
		}
		if err := tx.Put(ChangeSets, "w1", []byte("new")); err != nil {
			return err
		}
		return record(tx, "scope", "trigger", domain.Event{ID: "ignored", Type: TypeProposal, Source: Type, DeliveryID: "proposal/1", WorkID: "w1", ExecutionID: "x1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.accepts) != 1 || len(f.commits) != 0 {
		t.Fatalf("accepts %d commits %d", len(f.accepts), len(f.commits))
	}
	a := f.accepts[0]
	if a.ScopeID != "scope" || a.Parent != "trigger" || a.Event.ID != "" || a.Event.ExecutionID != "" || a.Event.WorkID != "w1" ||
		len(a.Checks) != 1 || a.Checks[0].Version != "v-old" || len(a.Records) != 1 || string(a.Records[0].Value) != "new" {
		t.Fatalf("accept: %+v", a)
	}
	if err = s.Transact(ctx, storage.Guard{}, func(tx storage.Tx) error {
		if _, err := tx.Get(Namespace, "question/w1"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("absent key: %v", err)
		}
		return tx.Put(Namespace, "question/w1", []byte("q"))
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.commits) != 1 || !f.commits[0].Checks[0].Absent || len(f.commits[0].Puts) != 1 {
		t.Fatalf("commit: %+v", f.commits)
	}
	// A second Event in one transaction, or one with no parent, is refused.
	err = s.Transact(ctx, storage.Guard{}, func(tx storage.Tx) error {
		e := domain.Event{Type: TypeProbe, Source: Type, DeliveryID: "probe/1"}
		if err := record(tx, "scope", "", e); err == nil {
			t.Fatal("an Event with no parent was queued")
		}
		if err := record(tx, "scope", "trigger", e); err != nil {
			return err
		}
		return record(tx, "scope", "trigger", e)
	})
	if !errors.Is(err, errOneEvent) || len(f.accepts) != 1 {
		t.Fatalf("second event: %v (%d accepts)", err, len(f.accepts))
	}
}
