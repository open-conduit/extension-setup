package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/storage"
)

// A change set is a session's list of JSON-pointer edits to the config
// file, each with a reason, waiting for the operator. It lives in the
// extension's KV namespace, keyed by the session's Work. The agent proposes
// and withdraws; the operator approves and rejects through actions, whose
// setup.decision Events are the record of each decision (an action may not
// write state); only the operator applies the candidate, the base text
// plus the approved changes, through POST /config/apply.

// ChangeSets is the KV namespace of change sets, by session Work ID.
const ChangeSets = "setup.changesets"

// Change states.
const (
	StateProposed  = "proposed"
	StateApproved  = "approved"
	StateRejected  = "rejected"
	StateWithdrawn = "withdrawn"
	StateApplied   = "applied"
)

// Change ops, as `config set` (add, replace) and `config unset` (remove).
const (
	OpAdd     = "add"
	OpReplace = "replace"
	OpRemove  = "remove"
)

// Redacted stands in for reference text in Events and in what the agent
// reads back.
const Redacted = "[redacted]"

// Proposal is a setup.proposal Event's data. Before and After are redacted
// under secret_refs.
type Proposal struct {
	Change      string                    `json:"change"`
	Op          string                    `json:"op"`
	Pointer     string                    `json:"pointer"`
	Before      json.RawMessage           `json:"before,omitempty"`
	After       json.RawMessage           `json:"after,omitempty"`
	Why         string                    `json:"why"`
	Diagnostics []inproc.ConfigDiagnostic `json:"diagnostics,omitempty"`
}

// Decision is a setup.decision Event's data: the operator's approval or
// rejection of one change, or of every open one (Approve all), or the
// agent's withdrawal of one.
type Decision struct {
	Changes  []string `json:"changes"`
	Decision string   `json:"decision"`
	Reason   string   `json:"reason,omitempty"`
}

// Checks is a setup.checks Event's data: the candidate's validation.
type Checks struct {
	Source        string                    `json:"source"`
	CandidateHash string                    `json:"candidate_hash,omitempty"`
	Valid         bool                      `json:"valid"`
	Diagnostics   []inproc.ConfigDiagnostic `json:"diagnostics,omitempty"`
	Approved      int                       `json:"approved"`
}

// Applied is a setup.applied Event's data.
type Applied struct {
	Generation string   `json:"generation"`
	Hash       string   `json:"hash"`
	Changes    []string `json:"changes"`
}

// changeSet is a session's stored change set. It names its base by the
// host's handle (inproc.ConfigHost) and keeps only the base's redacted
// text: the extension never holds reference text, except a reference a
// change proposes.
type changeSet struct {
	BaseGeneration string   `json:"base_generation"`
	BaseHash       string   `json:"base_hash"`
	BaseHandle     string   `json:"base_handle"`
	BaseRedacted   string   `json:"base_redacted"`
	Folders        []string `json:"folders,omitempty"`
	Changes        []change `json:"changes"`
	// Pending is a generation that applied the change set's candidate and
	// that setup.applied has not announced yet, with its hash: settle
	// marks the changes applied as it moves the change set on, and the
	// applied task announces it.
	Pending     string `json:"pending,omitempty"`
	PendingHash string `json:"pending_hash,omitempty"`
}

// change is one proposed edit. Value may hold reference text.
type change struct {
	ID          string                    `json:"id"`
	Request     string                    `json:"request"`
	Op          string                    `json:"op"`
	Pointer     string                    `json:"pointer"`
	Value       json.RawMessage           `json:"value,omitempty"`
	Before      json.RawMessage           `json:"before,omitempty"`
	Why         string                    `json:"why"`
	Diagnostics []inproc.ConfigDiagnostic `json:"diagnostics,omitempty"`
	Withdrawn   bool                      `json:"withdrawn,omitempty"`
	Applied     string                    `json:"applied,omitempty"`
	// Since is the acceptance watermark of the proposal, or of the rebase
	// that returned it to proposed: only decisions after it count.
	Since uint64 `json:"since"`
}

func loadChangeSet(tx storage.Tx, work domain.ID) (changeSet, bool, error) {
	var cs changeSet
	b, err := tx.Get(ChangeSets, string(work))
	if errors.Is(err, storage.ErrNotFound) {
		return cs, false, nil
	}
	if err != nil {
		return cs, false, err
	}
	return cs, true, json.Unmarshal(b, &cs)
}

func saveChangeSet(tx storage.Tx, work domain.ID, cs changeSet) error {
	b, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	return tx.Put(ChangeSets, string(work), b)
}

// states folds the session's decisions into each change's state: the
// operator's latest decision on it after its Since, unless it was withdrawn
// or applied. Only the operator's actions (source core.action, on the
// session's key) decide.
func states(tx storage.Tx, w domain.Work, cs changeSet) (map[string]string, error) {
	out := map[string]string{}
	since := uint64(0)
	for i, c := range cs.Changes {
		out[c.ID] = StateProposed
		if i == 0 || c.Since < since {
			since = c.Since
		}
	}
	if len(cs.Changes) == 0 {
		return out, nil
	}
	key, ok := sessionKey(w)
	if !ok {
		return out, nil
	}
	events, err := tx.Events(since, 0)
	if err != nil {
		return nil, err
	}
	byID := map[string]change{}
	for _, c := range cs.Changes {
		byID[c.ID] = c
	}
	for _, e := range events {
		var d Decision
		if e.Event.Type != TypeDecision || e.Event.Source != extension.ActionSource || e.Event.CorrelationKey == nil || *e.Event.CorrelationKey != key ||
			e.Event.Data.Schema != SchemaDecision || json.Unmarshal(e.Event.Data.Data, &d) != nil {
			continue
		}
		if d.Decision != StateApproved && d.Decision != StateRejected {
			continue
		}
		for _, id := range d.Changes {
			if c, ok := byID[id]; ok && e.Sequence > c.Since {
				out[id] = d.Decision
			}
		}
	}
	for _, c := range cs.Changes {
		switch {
		case c.Applied != "":
			out[c.ID] = StateApplied
		case c.Withdrawn:
			out[c.ID] = StateWithdrawn
		}
	}
	return out, nil
}

// open lists the changes waiting for the operator, in proposal order.
func open(cs changeSet, st map[string]string) []change {
	var out []change
	for _, c := range cs.Changes {
		if st[c.ID] == StateProposed {
			out = append(out, c)
		}
	}
	return out
}

// revision names the open changes as the operator sees them, so Approve
// all approves exactly those.
func revision(openChanges []change) string {
	h := sha256.New()
	for _, c := range openChanges {
		h.Write([]byte(c.ID + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// newChangeSet is an empty change set on base.
func newChangeSet(base inproc.ConfigBase) changeSet {
	return changeSet{BaseGeneration: base.Generation, BaseHash: base.Hash, BaseHandle: base.Handle, BaseRedacted: string(base.Text), Folders: base.Folders}
}

// built is a candidate the host holds: its handle and its redacted text.
type built struct {
	Handle string
	Text   []byte
}

// candidate is the base plus the approved changes, in proposal order, built
// by the host. An approved change that no longer applies is a diagnostic.
func candidate(ctx context.Context, host inproc.ConfigHost, cs changeSet, st map[string]string) (built, []string, []inproc.ConfigDiagnostic) {
	out := built{Handle: cs.BaseHandle, Text: []byte(cs.BaseRedacted)}
	var ids []string
	var diags []inproc.ConfigDiagnostic
	for _, c := range cs.Changes {
		if st[c.ID] != StateApproved {
			continue
		}
		e, err := host.Edit(ctx, out.Handle, c.Op, c.Pointer, c.Value)
		if err != nil {
			diags = append(diags, inproc.ConfigDiagnostic{Path: c.Pointer, Code: "edit_failed", Message: c.ID + ": " + err.Error()})
			continue
		}
		out = built{Handle: e.Handle, Text: e.Text}
		ids = append(ids, c.ID)
	}
	return out, ids, diags
}

// refsPattern is a secret reference, as `config set` accepts one.
var refsPattern = regexp.MustCompile(`^(keyring:[a-z][a-z0-9_-]{0,62}|file:[a-z][a-z0-9_-]{0,62}|env:[A-Za-z_][A-Za-z0-9_]*|cmd:[^\r\n]+)$`)

// underSecretRefs reports whether pointer addresses a value in a
// secret_refs object.
func underSecretRefs(pointer string) bool {
	parts := strings.Split(pointer, "/")
	return len(parts) >= 3 && parts[len(parts)-2] == "secret_refs"
}

// isSecretRefs reports whether pointer addresses a secret_refs object.
func isSecretRefs(pointer string) bool {
	return strings.HasSuffix(pointer, "/secret_refs")
}

// checkReferences refuses a value for a secret_refs slot that is not a
// reference: the agent proposes references, never values.
func checkReferences(pointer string, value json.RawMessage) error {
	var v any
	if err := json.Unmarshal(value, &v); err != nil {
		return refuse("invalid_input", "value must be JSON")
	}
	bad := false
	var walk func(v any, refs bool)
	walk = func(v any, refs bool) {
		switch x := v.(type) {
		case map[string]any:
			for k, m := range x {
				if refs {
					s, ok := m.(string)
					bad = bad || !ok || !refsPattern.MatchString(s)
					continue
				}
				walk(m, k == "secret_refs")
			}
		case []any:
			for _, m := range x {
				walk(m, false)
			}
		}
	}
	switch {
	case underSecretRefs(pointer):
		s, ok := v.(string)
		bad = !ok || !refsPattern.MatchString(s)
	default:
		walk(v, isSecretRefs(pointer))
	}
	if bad {
		return refuse("not_a_reference", "a secret_refs value is a reference (keyring:, file:, env:, cmd:): ask the operator to store the secret with open-conduit secret set and propose its reference")
	}
	return nil
}

// redact replaces reference text in a value at pointer with Redacted,
// keeping the names: what Events carry and the agent reads back.
func redact(pointer string, value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	if underSecretRefs(pointer) {
		b, _ := json.Marshal(Redacted)
		return b
	}
	var v any
	if json.Unmarshal(value, &v) != nil {
		return nil
	}
	var walk func(v any, refs bool) any
	walk = func(v any, refs bool) any {
		switch x := v.(type) {
		case map[string]any:
			for k, m := range x {
				if refs {
					x[k] = Redacted
					continue
				}
				x[k] = walk(m, k == "secret_refs")
			}
		case []any:
			for i, m := range x {
				x[i] = walk(m, false)
			}
		}
		return v
	}
	b, _ := json.Marshal(walk(v, isSecretRefs(pointer)))
	return b
}

// inFolder reports whether pointer reaches into a scope from an included
// folder, whose text is in another file.
func inFolder(cs changeSet, pointer string) bool {
	parts := strings.Split(pointer, "/")
	return len(parts) >= 3 && parts[1] == "scopes" && slices.Contains(cs.Folders, strings.ReplaceAll(strings.ReplaceAll(parts[2], "~1", "/"), "~0", "~"))
}

// refresh moves a change set onto the configuration now when it has moved
// (a generation applied, or the file edited): each change's before is
// recomputed, and a decided change whose before changed returns to
// proposed. latest is the acceptance watermark the returned changes count
// decisions after.
func refresh(ctx context.Context, cs changeSet, base inproc.ConfigBase, host inproc.ConfigHost, latest uint64) changeSet {
	if cs.BaseGeneration == base.Generation && cs.BaseHandle == base.Handle {
		return cs
	}
	cs.BaseGeneration, cs.BaseHash, cs.BaseHandle, cs.BaseRedacted, cs.Folders = base.Generation, base.Hash, base.Handle, string(base.Text), base.Folders
	for i, c := range cs.Changes {
		if c.Applied != "" || c.Withdrawn {
			continue
		}
		op := OpAdd
		if c.Op == OpRemove {
			op = OpRemove
		}
		e, err := host.Edit(ctx, base.Handle, op, c.Pointer, c.Value)
		before := e.Before
		if err != nil || !bytes.Equal(compact(before), compact(c.Before)) {
			cs.Changes[i].Before, cs.Changes[i].Since = before, latest
		}
	}
	return cs
}

// settle is refresh for a change set the operator may have applied. When
// the generation moved, and the new one is the change set's candidate (its
// base plus its approved changes), it marks those changes applied and
// leaves setup.applied pending before moving the change set onto the
// generation. Whatever reads the change set first after an apply (the
// Setup page reloading its review, the agent, the applied task) moves it
// on, so each must settle it first: once moved, the base the candidate
// was built on is gone.
func settle(ctx context.Context, tx storage.Tx, w domain.Work, cs changeSet, base inproc.ConfigBase, host inproc.ConfigHost, latest uint64) (changeSet, error) {
	if cs.BaseGeneration != base.Generation {
		st, err := states(tx, w, cs)
		if err != nil {
			return cs, err
		}
		if cand, ids, diags := candidate(ctx, host, cs, st); len(ids) > 0 && len(diags) == 0 {
			if hash, more, err := host.Validate(ctx, cand.Handle); err == nil && len(more) == 0 && hash == base.Hash {
				for i, c := range cs.Changes {
					if slices.Contains(ids, c.ID) {
						cs.Changes[i].Applied = base.Generation
					}
				}
				cs.Pending, cs.PendingHash = base.Generation, base.Hash
			}
		}
	}
	return refresh(ctx, cs, base, host, latest), nil
}

func compact(b json.RawMessage) []byte {
	if len(b) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return b
	}
	return buf.Bytes()
}

// nextChangeID is the next change's ID: c1, c2, …
func nextChangeID(cs changeSet) string {
	return "c" + strconv.Itoa(len(cs.Changes)+1)
}

// changeSetFor loads the session's change set, starting one on the
// configuration now if it has none, and refreshes it. It reads the
// configuration outside the transaction, since the host's config service
// has its own lock.
func (t *Tools) changeSetFor(ctx context.Context, tx storage.Tx, w domain.Work, base inproc.ConfigBase) (changeSet, error) {
	cs, ok, err := loadChangeSet(tx, w.ID)
	if err != nil {
		return cs, err
	}
	latest, err := tx.LatestSequence()
	if err != nil {
		return cs, err
	}
	if !ok {
		return newChangeSet(base), nil
	}
	return settle(ctx, tx, w, cs, base, t.o.Config, latest)
}

// lastTrigger is the Event that started the session's latest attempt: the
// one setup.applied derives from, since an extension's Events are always
// derived from an accepted Event. The session proposed what was applied,
// so it has had an attempt.
func lastTrigger(tx storage.Tx, work domain.ID) (domain.ID, error) {
	xs, err := tx.ListExecutions(work)
	if err != nil {
		return "", err
	}
	var last domain.Execution
	for _, x := range xs {
		if x.TriggerID != "" && (last.ID == "" || x.CreatedAt.After(last.CreatedAt) || x.CreatedAt.Equal(last.CreatedAt) && x.Attempt > last.Attempt) {
			last = x
		}
	}
	if last.ID == "" {
		return "", storage.ErrNotFound
	}
	return last.TriggerID, nil
}

// MarkApplied is the applied hook. It settles each open session's change
// set on the active generation and accepts setup.applied on the session's
// key for the changes a generation applied, which wakes the agent. A change
// set something else settled first (the Setup page reading its review
// straight after the apply) has its setup.applied pending, and gets it
// here.
func MarkApplied(ctx context.Context, store storage.Store, host inproc.ConfigHost, accept AcceptFunc, now func() time.Time) error {
	base, err := host.Base(ctx)
	if err != nil {
		return err
	}
	var due []domain.Work
	if err = store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		for _, w := range works {
			if w.Kind != WorkKind || w.State.Terminal() {
				continue
			}
			cs, ok, err := loadChangeSet(tx, w.ID)
			if err != nil {
				return err
			}
			if ok && (cs.Pending != "" || cs.BaseGeneration != base.Generation) {
				due = append(due, w)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, w := range due {
		key, ok := sessionKey(w)
		if !ok {
			continue
		}
		if err = store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
			cs, ok, err := loadChangeSet(tx, w.ID)
			if err != nil || !ok {
				return err
			}
			latest, err := tx.LatestSequence()
			if err != nil {
				return err
			}
			if cs, err = settle(ctx, tx, w, cs, base, host, latest); err != nil || cs.Pending == "" {
				// Nothing applied this session's candidate: it moves on
				// when it is next read.
				return err
			}
			var ids []string
			for _, c := range cs.Changes {
				if c.Applied == cs.Pending {
					ids = append(ids, c.ID)
				}
			}
			applied := Applied{Generation: cs.Pending, Hash: cs.PendingHash, Changes: ids}
			cs.Pending, cs.PendingHash = "", ""
			if err = saveChangeSet(tx, w.ID, cs); err != nil {
				return err
			}
			parent, err := lastTrigger(tx, w.ID)
			if errors.Is(err, storage.ErrNotFound) {
				return errNoAttempt
			}
			if err != nil {
				return err
			}
			at := now()
			id := string(w.ID) + "/" + applied.Generation
			return accept(tx, w.ScopeID, parent, domain.Event{Type: TypeApplied, Source: Type, DeliveryID: "applied/" + id,
				OccurredAt: at, ReceivedAt: at, CorrelationKey: &key, Data: payload(SchemaApplied, applied)})
		}); err != nil && !errors.Is(err, errNoAttempt) {
			return err
		}
	}
	return nil
}

// errNoAttempt skips a session with no attempt to derive setup.applied
// from, writing nothing.
var errNoAttempt = errors.New("setup: the session has no attempt")
