# Configuration

Application settings use ENV; each workflow is one Markdown file with YAML front
matter and agent instructions in the body. See [.env.dist](../.env.dist) and the
[example workflow](../workflows/assistant.md).

`orpheus-mattermost validate` checks settings, routes, limits, and workflow syntax
offline. `serve` also resolves credentials and checks bot identity and the server's
post-size limit. Unknown YAML fields, duplicate keys, invalid types, duplicate
workflow IDs, and overlapping routes are errors.

## Application environment

| Variable | Default | Purpose |
| --- | --- | --- |
| `ORPHEUS_BASE_URL` | Required | Orpheus API origin/base path |
| `ORPHEUS_API_KEY` | Required for serving | Orpheus bearer credential |
| `AGENTBOX_API_KEY` | Unset | Enables SDK preparation of attachment clarifications |
| `AGENTBOX_API_URL` | SDK default | Optional endpoint override; has no effect without the key |
| `WORKFLOWS_DIR` | `workflows` | Directory of workflow Markdown files |
| `LISTEN_ADDR` | `:8080` | Health/readiness/metrics listener |
| `HTTP_TIMEOUT` | `30s` | API request timeout |
| `MAX_REQUEST_BYTES` | `1048576` | Orpheus request budget, 4096–1048576 bytes |
| `MAX_PARALLEL_THREADS` | `8` | Concurrent thread reconciliations per connection, 1–128 |

API URLs must be absolute HTTP(S) URLs without credentials, query, or fragment.
Credential-bearing requests do not follow redirects. Helpers receive the
Mattermost token via Orpheus `env_from`; allow its variable name in the worker's
`HARNESS_ENV_ALLOWLIST`. Helpers do not receive the Orpheus or AgentBox API key.

Files are loaded in filename order from `WORKFLOWS_DIR`, without recursion.
Hidden files, editor backups, and non-Markdown files are ignored. At least one
valid workflow is required. Deploy custom workflows outside the shipped example
directory to avoid accidentally loading both.

## Mattermost connection

Each workflow contains exactly two connection settings:

```yaml
mattermost:
  base_url: https://chat.example.com
  token_env: MATTERMOST_BOT_TOKEN
```

`token_env` names a secret in process ENV; it never contains the token itself.
Provide the same secret reference in the Orpheus worker for hook `env_from`.
The bot ID and username come from `/users/me`. Source identity is derived from
the normalized URL and bot ID; changing a token for the same bot preserves it.
There are no configured `source_id`, `expected_bot_id`, or mention aliases.

Workflows using the same connection share routing. Different bots or servers can
have independent generic workflows and DM owners. Use one `token_env` reference
for workflows sharing a bot. Changing URL or bot identity creates a different
source; finish the old source's work before switching. Rename the bot with care:
mentions are matched against its current username, including REST replay.

Optional SDK credentials affect only attachments in active-run clarifications.
With `AGENTBOX_API_KEY`, files are installed before the clarification is submitted.
Without it, text-only clarifications are immediate; a batch with files remains in
Mattermost until the next run can download them through `before_run`. The whole
batch waits so that text and its attachments stay together. `AGENTBOX_API_URL`
alone does not enable SDK access. Normal hooks always handle first-run inputs and
output uploads; neither requires SDK credentials in the connector.

A run accepts at most 32 clarification batches containing files. Further file
batches wait intact for the next run. This fixed limit needs no ENV or workflow
setting; see [file limits and failure behavior](FILES.md).

## Workflow front matter

Each accepted input is an ordered `messages` array in Orpheus. Each Mattermost post
is a separate user message with YAML front matter and Markdown body. The last
element's `metadata` contains the connector's input contract. It records the chosen trigger/context posts, their versions,
file request and publication settings for restart recovery. The connector reads
it from history and checks it against the session and message external keys.
Changing either part changes the input fingerprint. Existing test sessions with
the old text header are outside this version's recovery contract.

Required fields are `id`, `revision`, `reconcile_from`, `profile`, and
`sandbox_template`. Quote `revision` as a string. `reconcile_from` is a fixed UTC
time in RFC3339, not a moving lookback window. When transferring channel ownership,
set an explicit cutover for the new workflow to avoid replaying another owner's
old requests.

The Markdown body is the complete set of connector-provided agent instructions;
the connector does not append hidden instructions. See the suggested Mattermost
file-handling paragraph in the [README](../README.md). Changes to instructions, profile,
template, or semantic policy change the effective revision: the next request
starts a new session after the current run finishes. If an Orpheus profile or
AgentBox template changes under the same name, increment `revision` explicitly.
Channel routing, trigger selection, and batching/polling settings do not change
revision. The embedded file-script hash is part of the effective revision; script
changes rotate sessions after active work finishes. Link expansion and its channel-access policy remain semantic settings.

| Field | Default | Behavior |
| --- | --- | --- |
| `env_from` | `[]` | Worker ENV names passed as `configuration.sandbox.env_from` to the agent session |
| `include_ids`, `exclude_ids` | `[]` | Specialized workflows reserve their channels ahead of a generic workflow |
| `direct_messages` | `false` | One DM owner; messages need no mention |
| `private_channels`, `group_messages` | `false` | Explicit opt-in for private/group channels |
| `start_on_mention` | `true` | Require a mention to start a new channel thread |
| `trigger_bot_ids` | `[]` | Allow listed external bots/webhooks; the connector's bot is always excluded |
| `send_commentary_messages` | `true` | Publish completed progress messages before the run finishes |
| `draining` | `false` | Deliver accepted results without admitting new requests |
| `message_batch_window` | `2s` | Window from the first unaccepted trigger |
| `poll_interval` | `30s` | Channel REST replay and known-thread reconciliation |
| `full_reconcile_interval` | `5m` | Rediscover all sessions in each workflow namespace |
| `max_concurrent_runs` | `10` | Active-run limit per workflow |
| `run_timeout_seconds` | `3600` | Orpheus run timeout |
| `hook_timeout_seconds` | `120` | File preparation deadline |
| `max_session_tokens` | `100000000` | Session token budget |
| `max_post_chars` | `12000` | Unicode code points, also capped by the server limit |
| `initial_context_token_budget` | `100000` | Approximate context budget, with separate API/ENV byte checks |

`env_from` contains unique environment variable names, never values. The connector
does not resolve them. Provide values to the Orpheus worker and allow the names in
`HARNESS_ENV_ALLOWLIST` on both API and worker. Orpheus validates reserved names
and the allowlist; the worker resolves values when preparing the session.
Changing this list rotates the session; reordering it does not.
The Mattermost token reference remains run-scoped for file hooks unless explicitly
included in this list.

```yaml
env_from:
  - GITLAB_TOKEN
  - GITLAB_HOST
```

Subsequent channel requests require a mention even when `start_on_mention=false`.
Idle time does not close or rotate a session. Orpheus owns sandbox pauses. Budget
exhaustion, lost sandbox/context, and revision changes can replace the session on
the next request.

## Attachments and links

The `files` mapping has these defaults:

| Field | Default |
| --- | --- |
| `max_per_post` | `5` |
| `max_file_bytes` | `10485760` |
| `max_image_bytes` | `20971520` |
| `max_batch_bytes` | `104857600` |
| `max_output_files` | `5` |
| `max_output_bytes` | `31457280` |

`max_batch_bytes` cannot exceed 100 MiB. SDK import sends file metadata and raw
bytes sequentially; memory use is bounded by one file rather than the whole batch.
`max_output_bytes` applies to each outgoing file. Input errors and exceeded limits
are recorded explicitly in the manifest. Incomplete output snapshots are never
published.

The `link_expansion` mapping defaults to `enabled: true`, `max_links: 5`, and
`max_posts: 20`. Only links to the same Mattermost installation are expanded.
Same-channel links are allowed; cross-channel transfer requires a directional
pair in `allowed_channel_pairs`:

```yaml
link_expansion:
  allowed_channel_pairs:
    - source: aaaaaaaaaaaaaaaaaaaaaaaaaa  # Linked channel.
      destination: bbbbbbbbbbbbbbbbbbbbbbbbbb  # Requesting channel.
```

Bot access to a channel alone does not authorize copying its content. Selection
includes the exact linked target, even far into a thread. Links within the current
thread do not expand it again. Repeated files reuse a path keyed by file ID;
missing or changed copies are restored. Linked images use the same local image
viewing path as images in the main thread.

## Reload and operation

`SIGHUP` reloads the workflow directory atomically. A bad file leaves the active
configuration intact. Process ENV settings and credentials should be changed
through a restart. Adding a connection or changing its URL/token reference also
requires a restart; workflow policy changes on existing connections reload in place. Mount the directory when using containers so that atomic file
replacement is visible inside the container.

Before removing a workflow, set `draining: true` and wait for delivery to finish.
Removing it stops admission and cancels active work. During `finalizing`, the
connector waits for the worker and does not interrupt the sandbox through SDK.
Removed-workflow tracking is in memory only; after restart, the deleted
workflow's namespace will no longer be enumerated.

Run one connector replica per source with sequential replacement. One process can
serve several sources from workflow definitions. Its
`mattermost/<workflow>` namespaces must have no other run initiators or sandbox
controllers. On restart, the connector reconciles all session generations and
all accepted runs; there is no local database or persistent volume.

`/health` (`/healthz`) checks the process; `/ready` (`/readyz`) reports the latest
reconciliation. `/metrics` exposes reconciliation/error/reconnect/retry counters,
duration, active runs, last success, replay lag, pending inputs/outputs, and age
of the oldest uncertain admission. Queue gauges describe the last reconciliation,
not an instantaneous Mattermost snapshot. JSON logs contain identifiers and error
classes, without message bodies or credentials.

Permanent POST failures and receipt conflicts suspend retries for that thread;
inspect the thread and settings before reloading to reset retries. Deleted bot
answers may be recreated. Exactly-once delivery across Mattermost and Orpheus is
not guaranteed: `pending_post_id` supplements durable receipts but has a short TTL.

WebSocket events enqueue only the affected thread; SSE events enqueue the owning
session's thread. Duplicate hints coalesce. The worker pool processes threads
independently, with one history snapshot per attempt and no barrier between
unrelated threads. Discovery runs separately. At most 4096 threads are queued;
REST discovery waits for queue space, and event overflow requests a rescan.
Unchanged thread fingerprints suppress redundant processing during channel polls;
threads without possible triggers are skipped. Active and pending work retains
periodic reconciliation. SSE starts at the watermark of the fetched history.

Full REST replay costs grow with history since `reconcile_from`. Concurrent writes
can shift pages; the next full scan catches omissions, while admission keys
suppress duplicates. Channel replay uses page/per-page, not `since` or `before`,
which can truncate results or lose posts sharing a timestamp at a page boundary.
