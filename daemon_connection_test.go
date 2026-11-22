package setup_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensiontest"
)

// TestDaemonRepoListThroughConnection lists repositories in a session with
// no gh, through a connection its scope is granted and a token in the
// secret store. A connection the scope isn't granted is refused before
// anything reaches its host, and the token is in no Event, panel, turn or
// daemon log.
func TestDaemonRepoListThroughConnection(t *testing.T) {
	const canary = "canary-github-token-7f31ab"
	var authorized atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("Authorization"), canary) {
			authorized.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"octo"}`))
			return
		}
		_, _ = w.Write([]byte(`[{"full_name":"octo/widgets"}]`))
	}))
	defer api.Close()
	var strayCalls atomic.Int32
	stray := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		strayCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer stray.Close()

	fake := t.TempDir()
	repos := func(id, connection string) extensiontest.Call {
		return extensiontest.Call{Operation: "setup.probe", RequestID: id, Input: map[string]any{
			"probe": "github.repos", "args": map[string]any{"owner": "octo", "connection": connection}, "why": "which repositories can be set up"}}
	}
	claude, err := extensiontest.FakeClaude(fake, extensiontest.Turn{State: "finished", Calls: []extensiontest.Call{
		repos("granted", "github"),
		repos("ungranted", "stray"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	binding := setupBinding(fake, claude.Path)
	binding["operations"] = []string{"read_context", "wait", "declare_outcome", "setup.say", "setup.ask", "setup.probe"}
	var cfg map[string]any
	if err = json.Unmarshal(setupDaemonConfig(binding), &cfg); err != nil {
		t.Fatal(err)
	}
	connection := func(id, apiURL string) map[string]any {
		return map[string]any{"id": id, "type": "github", "config": map[string]any{"api_url": apiURL},
			"secret_refs": map[string]any{"token": "stored:github-token"}}
	}
	cfg["connections"] = []any{connection("github", api.URL), connection("stray", stray.URL)}
	cfg["scopes"].([]any)[0].(map[string]any)["authorization"].(map[string]any)["connections"] = []string{"github"}
	doc, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	d := extensiontest.Daemon(t, extensiontest.Package("."), extensiontest.Options{Config: doc})
	c := d.Client()
	ctx := context.Background()
	if _, err = c.SetSecret(ctx, "github-token", canary, "file"); err != nil {
		t.Fatalf("store: %v", err)
	}
	res, err := c.InvokeAction(ctx, client.ActionTarget{ScopeID: "setup"}, "setup", "start", "opus", "start")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	session := client.ActionTarget{WorkID: res.Event.RoutedWorkID}
	d.Until("the session finished", 30*time.Second, func() bool {
		w, _ := c.Work(ctx, session.WorkID)
		return w.State == "completed"
	})

	turn, ok := claude.Turn(1)
	if !ok {
		t.Fatal("no turn")
	}
	result := map[string]extensiontest.Result{}
	for _, r := range turn.Results {
		result[r.RequestID] = r
	}
	if got := result["granted"].Text; !strings.Contains(got, "octo/widgets") {
		t.Errorf("the granted connection's list: %s", got)
	}
	if authorized.Load() == 0 {
		t.Error("GitHub never received the stored token")
	}
	if got := result["ungranted"].Text; !strings.Contains(got, "authorized") || strings.Contains(got, "octo/widgets") {
		t.Errorf("the ungranted connection: %s", got)
	}
	if n := strayCalls.Load(); n != 0 {
		t.Errorf("the ungranted connection's host got %d requests", n)
	}

	page, err := c.Events(ctx, url.Values{"work_id": {session.WorkID}})
	if err != nil {
		t.Fatal(err)
	}
	panels, err := c.Panels(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"events": mustJSONString(page.Items), "panels": mustJSONString(panels), "turns": mustJSONString(claude.Turns()), "log": d.Log()} {
		if strings.Contains(got, canary) {
			t.Errorf("the token is in the %s", name)
		}
	}
}
