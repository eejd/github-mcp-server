# This fork

`eejd/github-mcp-server` is a tracking fork of `github/github-mcp-server`, carrying two patches for
a single-replica MCP deployment. It exists because the deployment needs rate-limit handling that
upstream does not have in `http` mode; both patches are written to stay offerable upstream.

Branches: `main` mirrors upstream. `hive/v1.11.0` is cut from the **`v1.11.0` tag** and carries our
commits, so re-verification against the deployment's evidence list is a clean differential.

## The fork runs no automation

This is a deliberate constraint, and it is enforced in two places because they fail differently.

**In this repository, versioned and visible in a diff:**

- All thirteen workflows in `.github/workflows/` are reduced to `on: workflow_dispatch:`.
- `.github/dependabot.yml` is deleted.

**In repository settings, which nothing here can record — these do not travel with a clone, a
fork, or a branch restore, and must be re-applied by hand if this repo is ever copied:**

| Setting | Required state | API |
|---|---|---|
| Actions | disabled | `PUT /repos/{o}/{r}/actions/permissions` `enabled=false` |
| Vulnerability alerts | disabled | `DELETE /repos/{o}/{r}/vulnerability-alerts` |

**The two Dependabot controls are coupled, in a direction that is easy to get wrong.** Deleting
`dependabot.yml` stops *version* updates. It does **not** stop *security* updates — GitHub opens
those regardless of the config file, gated only on vulnerability alerts being enabled. So
re-enabling alerts to get notifications in the Security tab would silently resume Dependabot pull
requests even though the config file is gone. Manage the two together.

### Deliberate exceptions

**Secret scanning and push protection stay enabled.** They open no pull requests and run no code;
they scan pushes and can block one carrying a credential. On a public fork of a tool whose whole
subject is API tokens, that protection is worth more than strict adherence to "nothing runs".

### Not verifiable from here

**Installed GitHub Apps.** An App with access to this repository can open pull requests and post
checks entirely independently of Actions being disabled. `GET /user/installations` and
`GET /repos/{o}/{r}/installation` both require GitHub App JWT authentication and cannot be read
with a personal access token, so this was checked by hand rather than by API:

- `https://github.com/settings/installations` — user-level, including apps granted "all
  repositories"
- `https://github.com/eejd/github-mcp-server/settings/installations` — this repository

Everything else was swept and is clean: no webhooks, no rulesets, no branch protection, no
code-scanning default setup, no Pages, no Discussions, no Issues, no auto-merge, no Actions
secrets, no linked Projects.

## Before re-enabling anything

Re-enabling a trigger is the easy half. The identities are the half that publishes to the wrong
place: `registry-releaser.yml` and `server.json` hardcode **upstream's** image path
(`ghcr.io/github/github-mcp-server`) and registry name (`io.github.github/github-mcp-server`), so
running that workflow from this fork would attempt to claim upstream's entry in the official MCP
registry. Audit for `ghcr.io/github/`, `io.github.github/` and `github/github-mcp-server` across
`.github/workflows/`, `server.json` and the goreleaser config first.

Tracked as `eejd/agent-services-hive#543`.

## Building the image

By hand, arm64 only. Three constraints, none of which a workflow now enforces:

- **`linux/arm64` only.** The deployment is Apple Silicon under Colima, and this portfolio moved its
  other MCP images off QEMU emulation after a prior incident.
- **Never a moving tag.** Registry pins must name an immutable version tag, so do not publish
  `latest`, `{{major}}`, or `{{major}}.{{minor}}`. Pin by digest at the point of use.
- **No OAuth build secrets.** `OAUTH_CLIENT_ID` / `OAUTH_CLIENT_SECRET` serve only the stdio
  interactive-login path this deployment does not use, and the Dockerfile degrades to empty strings
  without them. Minting substitutes would bake a hive-owned GitHub identity into the image, which is
  exactly what the deployment's design forbids. **Consequence: images built here support no stdio
  OAuth login and carry no cosign signature, unlike upstream's.**

Do build the `node:26-alpine` UI stage — skipping it ships placeholder HTML for the UI-returning
tools, which is a silent functional regression rather than a saving.
