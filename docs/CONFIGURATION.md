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
Mattermost token via Orpheus `env_from`. Allow its variable name on API and worker
through a service or `HARNESS_ENV_ALLOWLIST`. Helpers do not receive the Orpheus
or AgentBox API key.

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

## Message context

Each accepted input is an ordered `messages` array in Orpheus. Each Mattermost post
is a separate user message with YAML front matter and Markdown body. The last
element's `metadata` contains the connector's input contract. It records the chosen trigger/context posts, their versions,
file request and publication settings for restart recovery. The connector reads
it from history and checks it against the session and message external keys.
Changing either part changes the input fingerprint. Accepted messages retain their
saved text during recovery, even if a user's profile or channel name has changed.

Each post starts with front matter like this:

```yaml
---
kind: thread
post_id: post-id
root_id: root-id
channel: {"id":"channel-id","name":"dev-test-group"}
author: {"id":"user-id","username":"alice","nickname":"Alice","email":"alice@example.com"}
created_at: 2026-10-01T07:00:00Z
---
```

`author.email` comes from the Mattermost user API, never from the post body.
It is omitted for bot and webhook posts: their profiles do not identify a human
requester. `channel.name` is Mattermost's `name` field (`dev-test-group` in
`~dev-test-group`), not its display title. Direct and group messages use generated
names (user IDs joined with `__` or a hash), not readable slugs.
Linked posts carry their own author's email and channel. Empty or unavailable
optional fields (including email, nickname, username, and channel name) are omitted,
not rendered as `null`. The channel ID remains present. Channel lookup 403/404
omits the name; other errors abort submission so reconciliation can retry.
These fields describe each post and do not enforce access to external tools.
The text field `channel_id` is replaced by `channel`; internal metadata, attachment
manifests, external keys, and `MM_CHANNEL_ID` keep their existing contracts.

For email-based tools, the connector's token must be able to read other users'
email addresses. For an ordinary bot this requires Mattermost's
[`PrivacySettings.ShowEmailAddress`](https://docs.mattermost.com/administration-guide/configure/site-configuration-settings#show-email-address)
to be enabled (the default). Privileged accounts may still see email when it is
disabled. Check `GET /api/v4/users/{human-user-id}` with the connector token before
deployment; do not infer availability from the bot's own profile. If email is
absent, the agent must not guess the schedule owner.

New inputs use renderer version 4. After accepting them, do not roll back to a
connector that only recognizes versions 1–3: it can treat those triggers as new
and submit duplicate runs. Roll forward with a fix that recognizes version 4.

For new runs, failure notices include the error message returned by Orpheus as a
Markdown quote after `Run failed (<code>).` Multiline messages keep every line
inside the quote. Empty messages add no quote. Cancelled runs and token-limit
notices retain their own wording. Runs accepted with an earlier renderer retain
their original notice text so replay does not conflict with existing receipts.

## Workflow front matter

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
changes rotate sessions after active work finishes. Link expansion remains a
semantic setting.

| Field | Default | Behavior |
| --- | --- | --- |
| `services` | `[]` | Unique catalog codes passed as `configuration.sandbox.services` to the agent session |
| `env_from` | `[]` | Worker ENV names passed as `configuration.sandbox.env_from` to the agent session |
| `include_ids`, `exclude_ids` | `[]` | Specialized workflows reserve their channels ahead of a generic workflow |
| `direct_messages` | `false` | One DM owner; messages need no mention |
| `private_channels`, `group_messages` | `false` | Explicit opt-in for private/group channels |
| `start_on_mention` | `true` | Require a mention to start a new channel thread |
| `trigger_bot_ids` | `[]` | Allow listed external bots/webhooks; the connector's bot is always excluded |
| `send_commentary_messages` | `true` | Publish completed progress messages before the run finishes |
| `draining` | `false` | Deliver accepted results without admitting new requests |
| `message_batch_window` | `0` | Window from the first unaccepted trigger; `0` starts it immediately, so later posts are outside that batch |
| `poll_interval` | `30s` | Channel REST replay and known-thread reconciliation |
| `full_reconcile_interval` | `5m` | Rediscover all sessions in each workflow namespace |
| `max_concurrent_runs` | `10` | Active-run limit per workflow |
| `run_timeout_seconds` | `3600` | Orpheus run timeout |
| `hook_timeout_seconds` | `120` | File preparation deadline |
| `max_session_tokens` | `100000000` | Session token budget |
| `max_post_chars` | `12000` | Unicode code points, also capped by the server limit |
| `initial_context_token_budget` | `100000` | Approximate context budget, with separate API/ENV byte checks |

`services` contains unique codes from the Orpheus service catalog. Unknown codes
reject session creation. `env_from` can add individual ENV names. Configure the
same catalog and `HARNESS_ENV_ALLOWLIST` on Orpheus API and worker. Service ENV
names automatically join that allowlist. Values belong on the Orpheus worker.
Changing either selection rotates the session. Reordering it does not. If a
service definition changes under the same code, increment `revision` explicitly.
The Mattermost token reference remains run-scoped for file hooks unless selected
for the agent through a service or `env_from`.

```yaml
services: [gitlab]
```

After correcting an unknown profile, template or service, increase `revision`
and reload workflows. Rejected input is retried under the new effective revision
without restarting the connector. Rejection notices are deduplicated by revision,
reason and input.

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

`max_per_post` and `max_output_files` accept values from 1 to 100.
The Mattermost server must support the configured number of attachments per post.
`max_batch_bytes` cannot exceed 100 MiB. SDK import sends file metadata and raw
bytes sequentially; memory use is bounded by one file rather than the whole batch.
`max_output_bytes` applies to each outgoing file. Input errors and exceeded limits
are recorded explicitly in the manifest. Incomplete output snapshots are never
published.

The `link_expansion` mapping defaults to `enabled: true`, `max_links: 5`, and
`max_posts: 20`. Only links to the same Mattermost installation are expanded.
Selection includes the exact linked target, even far into a thread. Links within
the current thread do not expand it again. Repeated files reuse a path keyed by
file ID; missing or changed copies are restored. Linked images use the same local
image viewing path as images in the main thread.

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
reconciliation. `/metrics` exposes Go runtime and process metrics alongside reconciliation/error/reconnect/retry counters,
duration, active runs, suspended threads (`orpheus_mattermost_suspended_threads`),
last success, replay lag, pending inputs/outputs, and age of the oldest uncertain
admission. Queue gauges describe the last reconciliation,
not an instantaneous Mattermost snapshot. JSON logs contain identifiers and error
classes, without message bodies or credentials.

Permanent POST failures and receipt conflicts suspend retries for that thread;
run state remains observed, releasing capacity when no active run remains.
Inspect the thread and settings before reloading to reset retries. Deleted bot
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

## Protected service access

A workflow can bind a private channel to provider-backed services:

```yaml
services: [protected-data]
private_channels: true
direct_messages: false
group_messages: false
include_ids: [cccccccccccccccccccccccccc]
start_on_mention: true
link_expansion:
  enabled: false
access_context:
  namespace: mattermost:example
  bot_id: bbbbbbbbbbbbbbbbbbbbbbbbbb
  scope_context:
    workspace: example
  identity_projection:
    actor_field: requester
    audience_field: readers
    subjects:
      hhhhhhhhhhhhhhhhhhhhhhhhhh: 42
      nnnnnnnnnnnnnnnnnnnnnnnnnn: 77
```

These are server-controlled integration identifiers, not model instructions.
The configured bot must match `/users/me`. The provider validates the binding
and its own data permissions. Bot choice and product instructions remain in the
workflow. An unconfigured workflow retains its existing behavior.

`scope_context` is a static JSON object expressed in YAML. `identity_projection`
adds the configured actor field and reader array using explicit source-user
mappings. The field names and service identities belong to the integration;
the connector has no provider-specific schema. For the example above, a request
from `hhhh...` with both mapped users present sends this opaque scope:

```json
{"workspace":"example","requester":42,"readers":[42,77]}
```

Mapped identities must be distinct nonempty strings (up to 128 UTF-8 bytes) or
canonical decimal integers (up to 128 characters). Integer identity values do
not pass through floating-point conversion. The provider validates its own ID
range and data policy. Booleans, null, arrays, objects and fractions are not
identities. A projection field cannot overlap the static context. Static context
is limited to 64 fields and 16 KiB; mappings are limited to 4,096 source users.
A missing mapping fails admission. Multiple source users cannot share one
service identity. The authenticated execution bot is the only excluded service
reader and must not appear in the mapping. Other bots are rejected, even if
mapped. No role or display-name fallback is used.

The full confirmed Mattermost audience, including the execution bot, remains
in a separate `allowed_input_subjects` list of namespaced source identities.
The connector stores `access_proof` in its message metadata: the frozen source
audience and a digest of the projection configuration. Channel, root and source
identity remain in the existing envelope. Provider scope contains none of these
transport fields unless an integration explicitly configures such static data.

After a run slot is reserved, the connector reads the private channel and all
membership pages, including admins and bots. It checks counts before and after
pagination and rejects missing, duplicate, invalid or incomplete results. It
confirms each mapped reader through the user API, including account state and
bot status, before issuing credentials.
Mattermost has no transactional membership snapshot: equal-count replacements
concurrent with pagination cannot be detected by counts alone. Live acceptance
must verify the server's membership semantics. Public channels and DMs are not
supported by protected workflows. The observation must complete in less than 60 seconds.
See the [Mattermost membership API](https://github.com/mattermost/mattermost/blob/master/api/v4/source/channels.yaml).

The invocation UUID depends only on source, workflow, channel, root and anchor.
Before acceptance, the existing pending input freezes the audience and context;
accepted metadata survives process restarts. Uncertain responses are reconciled
against accepted history before another submission. A lost, unaccepted pending
snapshot is not reconstructed transparently: the same invocation with different
context is rejected by the provider and needs a new mention.

On recovery, the connector validates the envelope binding, transport proof,
projection digest, projected service scope and explicit input list before
building context or preparing files. Missing legacy authority/proof, an invalid
projection or a mismatched namespace/list blocks updates to an active run.
The next run starts a clean session and history boundary. The connector never
derives missing input authority from service scope. An unreadable metadata
envelope still fails closed and requires repair.

Projection configuration participates in the existing workflow revision. A
mapping or static-context change does not reinterpret an active run: the current
response finishes with its frozen snapshot, no new clarifications are delivered
under the changed revision, and the next run starts clean. Frozen pending
admission retries retain the original projection and invocation.

Active clarifications use the initial explicit input list. Authors outside it remain
unaccepted in Mattermost; neither their text nor files enter `BuildMessages` or
`Sandbox.Prepare`. Every delivered item has its confirmed author in the existing
`SendMessage.access_context.input_subjects`. Renewal and publication do not fetch
a new audience.

Only `409 service_scope_changed` rotates a reusable protected session. The new
session keeps the invocation, but rebuilds only the frozen triggering posts.
It does not transfer the old payload or file index. First admission and unknown
memory provenance use the same `trigger_only` builder mode. Accepted metadata
stores `context_start: {created_at, post_id}`; later builders filter before
reading files, even if the boundary post has been deleted. Linked-thread
expansion is disabled. Rejected clarifications from a finished run are not
transferred automatically for protected workflows; a new mention is required.
Delivery, outbox, external thread keys and the stock queue remain unchanged.
