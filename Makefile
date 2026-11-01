# make check is what CI runs on every push. make certify builds Open Conduit
# at OPEN_CONDUIT_REF (a branch or tag) and certifies the package with it.

GO ?= go
OPEN_CONDUIT_REPO ?= https://github.com/open-conduit/open-conduit.git
OPEN_CONDUIT_REF ?= main
BUILD_DIR ?= .build
PACKAGE_DIR ?= dist/setup
LDFLAGS ?= -s -w
# The package's own setup probes. They run inside its process, so extension
# check can't see them offline and is told their names.
OWN_PROBES ?= setup.fs.list,setup.tools

.PHONY: check fmt-check vet test build package extension-check certify clean

check: fmt-check vet test build package extension-check

fmt-check:
	@test -z "$$(gofmt -l $$(git ls-files '*.go') $$(git ls-files --others --exclude-standard '*.go'))" || { gofmt -l $$(git ls-files '*.go') $$(git ls-files --others --exclude-standard '*.go'); exit 1; }

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

build:
	$(GO) build ./...

# The package directory, laid out as Open Conduit installs and bundles it:
# the executable, its manifest and certification files, and the playbooks.
package:
	rm -rf $(PACKAGE_DIR)
	mkdir -p $(PACKAGE_DIR)
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(PACKAGE_DIR)/setup-extension ./cmd/setup-extension
	$(PACKAGE_DIR)/setup-extension package-manifest > $(PACKAGE_DIR)/open-conduit-extension.json
	$(PACKAGE_DIR)/setup-extension package-certification > $(PACKAGE_DIR)/certification.json
	cp -R playbooks $(PACKAGE_DIR)/playbooks

# Lint the built package with the Open Conduit go.mod pins: the manifest, its
# compatibility, certification.json, and that every operation a playbook
# names is one the package declares or core registers.
extension-check: package
	$(GO) tool open-conduit extension check $(PACKAGE_DIR) --allow $(OWN_PROBES)

# Certification against an Open Conduit build. A ref that doesn't contain
# the Open Conduit commit go.mod requires can't host the package (it lacks
# surfaces the manifest declares), so it is skipped, not failed.
certify: package
	@set -eu; src=$(BUILD_DIR)/open-conduit; rm -rf $$src; \
	git clone --quiet --filter=blob:none $(OPEN_CONDUIT_REPO) $$src; \
	git -C $$src checkout --quiet $(OPEN_CONDUIT_REF); \
	required=$$($(GO) list -m -f '{{.Version}}' github.com/open-conduit/open-conduit); \
	commit=$${required##*-}; \
	if ! git -C $$src merge-base --is-ancestor $$commit HEAD 2>/dev/null; then \
		echo "skip: open-conduit $(OPEN_CONDUIT_REF) predates $$required, which this package requires"; exit 0; \
	fi; \
	(cd $$src && $(GO) build -o bin/open-conduit ./cmd/open-conduit); \
	$$src/bin/open-conduit extension certify $(PACKAGE_DIR) --report $(PACKAGE_DIR).certification.json

clean:
	rm -rf $(BUILD_DIR) dist
