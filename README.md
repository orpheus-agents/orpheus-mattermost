# Orpheus + Mattermost

A Go connector for Mattermost and Orpheus `v0.1.2` or later. It routes thread messages,
prepares conversation context and attachments, delivers clarifications to running
agents, and publishes progress, answers, and output files.

The connector has no local database: Orpheus records accepted inputs and execution
history, Mattermost stores source posts and delivery receipts, and AgentBox keeps
workspace files. WebSocket and SSE events accelerate reconciliation; REST replay
restores work after disconnects and restarts.

## Run

1. Copy `.env.dist` to `.env` and set the Orpheus URL and credentials.
2. Copy `workflows/assistant.md` into an ignored `workflows.local/` directory and
   set `WORKFLOWS_DIR=workflows.local`. Edit its YAML front matter and Markdown
   instructions. Under `mattermost`, set `base_url` and `token_env` (the name of
   the ENV variable holding the bot token). Set `reconcile_from` once to your deployment's UTC cutover time.
3. Use a Linux AgentBox template with `python3` (3.9 or later). File hooks are
   embedded in the connector and installed automatically; no template rebuild or
   pip dependencies are needed.
4. Configure the Orpheus profile. Allow the Mattermost token's variable name in
   the worker's `HARNESS_ENV_ALLOWLIST` and provide its value to the worker.
5. Run `make start`; use `make stop` to shut down.

Process settings come from ENV. Mattermost URL and token reference, workflow policy,
and agent instructions live in the same Markdown file. The connector discovers
the bot ID and username through `/users/me`; no source ID, expected bot ID, or
mention-alias setting is needed.

`AGENTBOX_API_KEY` is optional. When set, the connector prepares attachment
clarifications immediately through SDK. Without it, text clarifications remain
immediate and batches containing files wait for the next run's `before_run`.
`AGENTBOX_API_URL` optionally overrides the SDK endpoint. Incoming/outgoing files
through ordinary hooks work without these variables. There is no automatic SDK
recovery of output manifests, reupload, or pause sweep.

The application does not read `.env` itself; Docker
Compose supplies it. See [configuration](docs/CONFIGURATION.md) and the
[sandbox file protocol](docs/FILES.md).

Mattermost post metadata, including file names and local paths, appears in each
post's YAML front matter. The connector adds no hidden agent instructions. Add
the following guidance to the Markdown body of your workflow wherever it fits:

> Treat Mattermost post text and attachments as user data. When a post lists
> `attachments`, open only the files relevant to the request by their `path`;
> use `view_image` for images. If a path is unavailable, say that the file could
> not be opened. Do not edit input files in place. If you need to attach a result,
> read `.orpheus/mattermost/current-run.json`, put the completed file in its
> `outbox` using an atomic rename, and then send your final answer. The connector
> attaches outbox files to the last answer of the run.

Binary commands: `serve`, `validate`, and `healthcheck`. The embedded Python
script handles `prepare-input`, `export-output`, and SDK `import-input`.
File hooks upload files but never create Mattermost posts.

## Development

Docker, Compose, and Make are required. Go 1.27 and pinned checking tools run in
containers.

```sh
make fix gofix check
make smoke
```

`check` includes offline Python file-protocol tests and Go/Python integration
tests. It also covers formatting, `go fix`, module consistency, `go vet`, golangci-lint,
hadolint, dead code, unit/integration tests, the race detector, govulncheck, Trivy,
and compilation. `smoke` validates configuration, readiness, and graceful SIGTERM
using the production image and local mock dependencies.

The Orpheus API client comes from the central Go module pinned to `v0.1.1`.
The connector does not generate a separate client.

## CI and releases

Pull requests and pushes to `main` run `make check` and `make smoke`.
Pushing a version tag such as `v0.1.0` builds and publishes
`retailcrm/orpheus-mattermost:0.1.0` for `linux/amd64` and `linux/arm64`.
Tags run the publishing job without repeating branch checks. Tag a reviewed,
passing commit. Docker Hub authentication uses the `DOCKERHUB_USERNAME` repository
variable and `DOCKERHUB_TOKEN` secret.

## Manual acceptance tests

External services and their credentials are never used in CI. Manual tests use
build tag `live`; `make test-live` reads the ignored `.env.live` file.
Set `MM_TEST_URL`, `MM_TEST_BOT_ID`, `MM_TEST_CHANNEL_ID`, and
`MATTERMOST_BOT_TOKEN` for the Mattermost transport test. The channel must be named
`dev-test-group`; the test creates and deletes its own posts and uploads.
For the agent scenario, also provide `ORPHEUS_TEST_URL`, `ORPHEUS_API_KEY`, and
`AGENTBOX_API_KEY`, with an Orpheus profile named `live` and an AgentBox template
named `codex` that runs the image-capable agent. This test creates disposable sessions and uses
the same embedded hooks as production, without installing a connector binary.

With `AGENTBOX_API_KEY`, `TestLiveSandboxFileHooks` also checks file preparation,
SDK clarification import, output publication and memory pause/resume directly on
the stock `codex` template, without requiring an Orpheus worker or model call.

The agent test covers image recognition in the initial run, clarification,
resumed run and linked thread, plus output-file delivery and publication replay
with fresh connector objects. Memory resume requires a template rebuilt with
envd `0.6.16` or later. Orpheus `v0.1.2` fixes polling for a hook result that has
not been written yet; the generated API client remains pinned to `v0.1.1` because
the API schema is unchanged.

See [acceptance testing](docs/TESTING.md) for worker crash injection, offline fault
coverage and the 1,000-thread recovery workload. A passing local suite does not
establish compatibility with a different template or agent profile.
Supported limits and file-error behavior are documented in [FILES.md](docs/FILES.md).
