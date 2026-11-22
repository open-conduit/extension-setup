# extension-setup

Open Conduit's web setup assistant, as an installed extension package.

From the web view's Setup page, the operator starts a session: a Claude Code agent running the setup playbook looks at the machine, says what it finds, asks what the operator wants and proposes config changes. The operator approves or rejects each one, validates them, and applies them. The agent never changes the config itself.

A session can ask for a credential in the page: `setup.ask_secret` draws a password field through the public `extension.Action.Secret` contract, the web view stores the value as the signed-in operator, and the agent learns only that the name is stored.

A first-run session needs a model credential before it can do anything. With no Claude login on the machine, the operator adds `anthropic-api-key` on the Secrets page and turns on the `model` grant when starting the session. The key reaches only a session that took the grant, so a session without it keeps using a Claude login. A session that takes the grant before the key is stored fails its attempt.

Sessions run only on `claude-code`, because it is the one executor whose shell can be turned off. Codex's sandbox limits what a command may do, but its agent can still run commands and read files.

The package speaks only Open Conduit's extension protocol, through `protocol/sdk`. It is built from this repository and doesn't run inside Open Conduit.

## How it ships

- **Bundled.** Each Open Conduit release archive carries this package under `extensions/setup/`, beside the `open-conduit` binary. Open Conduit finds it there, so a fresh install with no config starts on a setup scope: the package's setup is the first-run setup (`setup.first_run`).
- **Installed.** An operator can install another build with `open-conduit extension install`. A copy in `<config dir>/extensions/setup/` replaces the bundled one, even when the installed copy is refused. Remove it with `open-conduit extension remove setup` to go back to the bundled copy.

Open Conduit pins the version it bundles with a `tool` directive in its `go.mod`, and builds the package from that version.

## Compatibility

- **Protocol:** version 1.
- **Open Conduit:** the commit this module's `go.mod` requires, or later. A build that lacks a surface the manifest declares (`entry_gated`, route `binding`, `setup.first_run`, pages, `config_candidate` and `secret_actions`) refuses the package at install and at start, so a host that can't take a secret privately never draws the request as a text box.
- **KV:** the change sets live in `setup.changesets` and the sessions' questions in `setup.sessions`, as they did when setup was compiled into Open Conduit, so sessions in flight carry over.

## Layout

- The root package is the extension: the session route and Work kind, the agent's `setup.*` operations, the operator's actions, the page's panels, the change set and the applied hook.
- `cmd/setup-extension` serves it, and prints the package files (`package-manifest`, `package-certification`).
- `playbooks/setup.md` is the setup playbook. Open Conduit adds it to the agent's plugin and fills in `{{bin}}`.

## Developing

```sh
make check                          # gofmt, vet, tests with -race, build, package, extension check
make certify                        # certify with Open Conduit main
make certify OPEN_CONDUIT_REF=v1.10.0
```

`make package` writes the package directory to `dist/setup`, and `make extension-check` lints it with `open-conduit extension check`: a playbook that names an operation neither the package nor Open Conduit registers fails. The package's own probes are allowed by name (`OWN_PROBES`), and a test keeps that list in step with the probes it serves.

The tests run on core's test tooling, so none needs a real service or a real Claude Code:

- `store_test.go` runs the KV store over `protocol/sdk/sdktest`, an in-memory host.
- `daemon_session_test.go` and `daemon_changeset_test.go` run the package in the real daemon with `extensiontest`, with a scripted stand-in for the Claude Code CLI, and check the Setup page through the management client's pages, panels and actions. `extensiontest` builds `open-conduit` and `os` at the version `go.mod` pins, from its `tool` directives. CI runs `make check`, and `make certify` against Open Conduit's `main` and its latest release. A release older than the commit `go.mod` requires is skipped.
