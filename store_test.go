package setup

import (
	"context"
	"errors"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
	"github.com/open-conduit/open-conduit/storage"
)

// A transaction with an Event commits through event.accept, its writes and
// the versions it read going with the Event; one without commits KV alone.
func TestHostStoreCommits(t *testing.T) {
	trigger := domain.Event{ID: "trigger", Type: TypeStart, Source: Type, DeliveryID: "start/1"}
	h := sdktest.New(t, New(BuildOptions{}).Build(),
		sdktest.WithKV(ChangeSets, "w1", []byte("old")),
		sdktest.WithEvents("scope", trigger))
	s := hostStore{host: func() *sdk.Host { return h.Server().Host() }}
	ctx := context.Background()
	seeded := len(h.Events())
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
	events := h.Events()
	if len(events) != seeded+1 || len(h.Commits()) != 0 {
		t.Fatalf("events %d commits %d", len(events)-seeded, len(h.Commits()))
	}
	e := events[len(events)-1].Event
	if e.ID == "ignored" || e.CausationID != "trigger" || e.ExecutionID != "" || e.WorkID != "w1" || e.Type != TypeProposal {
		t.Fatalf("accepted: %+v", e)
	}
	// The write went with the Event, guarded by the version it read.
	if got := string(h.KV(ChangeSets)["w1"]); got != "new" {
		t.Fatalf("the Event's record: %q", got)
	}
	if err = s.Transact(ctx, storage.Guard{}, func(tx storage.Tx) error {
		if _, err := tx.Get(Namespace, "question/w1"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("absent key: %v", err)
		}
		return tx.Put(Namespace, "question/w1", []byte("q"))
	}); err != nil {
		t.Fatal(err)
	}
	if c := h.Commits(); len(c) != 1 || len(c[0].Checks) != 1 || !c[0].Checks[0].Absent || len(c[0].Puts) != 1 {
		t.Fatalf("commit: %+v", c)
	}
	// A read that moved under the transaction refuses the Event and its
	// writes together.
	err = s.Transact(ctx, storage.Guard{}, func(tx storage.Tx) error {
		if _, err := tx.Get(ChangeSets, "w1"); err != nil {
			return err
		}
		if err := s.Transact(ctx, storage.Guard{}, func(inner storage.Tx) error {
			return inner.Put(ChangeSets, "w1", []byte("moved"))
		}); err != nil {
			return err
		}
		if err := tx.Put(ChangeSets, "w1", []byte("stale")); err != nil {
			return err
		}
		return record(tx, "scope", "trigger", domain.Event{Type: TypeProposal, Source: Type, DeliveryID: "proposal/2", WorkID: "w1"})
	})
	if err == nil || len(h.Events()) != seeded+1 || string(h.KV(ChangeSets)["w1"]) != "moved" {
		t.Fatalf("a stale transaction: %v (%d events, %q)", err, len(h.Events())-seeded, h.KV(ChangeSets)["w1"])
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
	if !errors.Is(err, errOneEvent) || len(h.Events()) != seeded+1 {
		t.Fatalf("second event: %v (%d events)", err, len(h.Events())-seeded)
	}
}
