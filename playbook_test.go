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
