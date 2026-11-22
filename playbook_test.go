package setup

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The setup playbook is a skill with frontmatter, and the probes make
// check allows by name (OWN_PROBES, for `open-conduit extension check`) are
// exactly the package's own: a probe added or renamed here must be named
// there, and a stale name there would hide a playbook typo.
func TestPlaybookAndOwnProbes(t *testing.T) {
	playbook, err := os.ReadFile("playbooks/setup.md")
	if err != nil || !strings.HasPrefix(string(playbook), "---\nname: setup\n") {
		t.Fatalf("the setup playbook has no frontmatter: %v", err)
	}
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^OWN_PROBES \?= (.+)$`).FindSubmatch(makefile)
	if m == nil {
		t.Fatal("the Makefile sets no OWN_PROBES")
	}
	allowed := strings.Split(string(m[1]), ",")
	var own []string
	for _, p := range (Probes{}).SetupProbes() {
		own = append(own, Type+"."+p.Name)
	}
	slices.Sort(allowed)
	slices.Sort(own)
	if !slices.Equal(allowed, own) {
		t.Fatalf("OWN_PROBES %v, the package's probes %v", allowed, own)
	}
}

// The model grant reaches only the setup session, so the playbook must give
// the work scope the stored key when the daemon has no Claude login, and
// leave a daemon with a login on it.
func TestPlaybookWorkScopeModelCredential(t *testing.T) {
	playbook, err := os.ReadFile("playbooks/setup.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(playbook), "### A model credential for the work scope\n")
	if !ok {
		t.Fatal("the playbook has no section on the work scope's model credential")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	_, noLogin, ok := strings.Cut(section, "- **With no Claude login** (`\"signed_in\": false`)")
	if !ok {
		t.Fatal("the section has no case for a daemon with no Claude login")
	}
	withLogin, _, _ := strings.Cut(section, "- **With no Claude login**")
	_, withLogin, ok = strings.Cut(withLogin, "- **With a Claude login** (`\"login\": \"subscription\"`)")
	if !ok {
		t.Fatal("the section has no case for a daemon with a Claude login")
	}
	if !strings.Contains(withLogin, "Add no credential") || strings.Contains(withLogin, "stored:") || strings.Contains(withLogin, "secret_names") {
		t.Fatalf("a daemon with a Claude login must get no credential change: %q", withLogin)
	}
	for _, want := range []string{
		"`secret_refs: {\"" + ModelSecret + "\": \"stored:" + ModelSecret + "\"}`",
		"`secret_names: [\"" + ModelSecret + "\"]`",
		"`setup.ask_secret`, name `" + ModelSecret + "`",
		"`why`",
	} {
		if !strings.Contains(noLogin, want) {
			t.Errorf("the no-login case doesn't say %s", want)
		}
	}
	if err := checkReferences("/scopes/web/secret_refs/"+ModelSecret, []byte(`"stored:`+ModelSecret+`"`)); err != nil {
		t.Errorf("the reference the playbook proposes is refused: %v", err)
	}
}
