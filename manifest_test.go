package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// The manifest is valid as the host reads it, and its playbook directory
// holds the setup playbook the first-run setup starts on.
func TestManifest(t *testing.T) {
	b, err := json.Marshal(Manifest())
	if err != nil {
		t.Fatal(err)
	}
	m, err := protocol.ParseManifest(b)
	if err != nil {
		t.Fatal(err)
	}
	p := m.Provides
	if !p.EntryGated || !p.ConfigCandidate || p.Setup == nil || !p.Setup.FirstRun || len(p.Pages) != 1 || !p.Routes[0].Binding ||
		!m.Allows(protocol.PermissionConfigRead) || !m.Allows(protocol.PermissionProbes) {
		t.Fatalf("manifest: %+v", m)
	}
	text, err := os.ReadFile(filepath.Join(PlaybookDir, p.Setup.Type+".md"))
	if err != nil || !strings.Contains(string(text), "{{bin}}") {
		t.Fatalf("playbook: %v", err)
	}
	for _, op := range Operations() {
		if !strings.Contains(string(text), op) && op != OpTrace {
			t.Errorf("the playbook doesn't name %s", op)
		}
	}
}
