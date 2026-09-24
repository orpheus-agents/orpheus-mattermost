# Sandbox files

File operations use an embedded Python 3.9+ standard-library script. `before_run`
ships a compressed copy through the Orpheus hook API and atomically installs it
at `.orpheus/mattermost/code/<sha256>.py`. `after_run` verifies the installed bytes against the hash before executing them.
SDK clarification import bootstraps its matching script with `python3 -I -c`,
leaving stdin available for file records. Different script versions coexist.
There is no connector-specific template installation or runtime package download.
The full hook body counts toward the Orpheus API request size limit.

Workspace files live under `.orpheus/mattermost/`:

| Path | Purpose |
| --- | --- |
| `input/files/<file_id>/<safe_name>` | Original file verified by size and SHA-256 |
| `input/batches/<anchor>/request.json` | Frozen preparation request |
| `input/batches/<anchor>/manifest.json` | Actual paths, hashes, and per-file errors |
| `runs/<run_id>/input-index.json` | Previous index and newly delivered batches |
| `current-run.json` | Current run, input index, and outbox |
| `output/<run_id>/outbox/` | Completed agent artifacts |
| `output/<run_id>/sealed/` | Atomically completed immutable snapshot |
| `output/<run_id>/upload-manifest.json` | Confirmed Mattermost uploads |

## Incoming files

`before_run` prepares files from the accepted batch. A new run in the same session
adds files from new messages, including passive context preceding a mention.
Existing files and conversation history survive an idle pause. A reference to an
older attachment verifies its cached bytes and restores a missing or changed copy;
it does not download the entire history again.

Images from both the main thread and linked threads are opened by the agent with
`view_image` using local paths. Same-thread links do not expand the thread again;
an explicitly referenced post omitted from earlier context is included once.
Cards in `props.attachments` are text context, not uploaded Mattermost files.
During continuation, the connector does not automatically feed its own answers or
notices back to the agent. Explicit references and initial context in a replacement
session can still include those posts and their files. Optional history is reduced
to the request/ENV budget with an omission notice before an input is rejected.

With optional `AGENTBOX_API_KEY`, clarification attachments are prepared by the
connector and installed through SDK stdin, one file at a time. A bounded JSON
request and per-file metadata precede raw file bytes; no batch-sized base64 JSON
is built. Only a complete, validated stream commits the manifest. Import inherits
the hook timeout and closes abandoned stdin to release the batch lock. The connector supplies the matching script with each SDK invocation; the
import protocol is internal.
Each file batch has a separate manifest and does not overwrite the running agent's index or `current-run.json`. Once delivered,
the batch enters the next run's input index. Text-only clarifications need neither
SDK access nor a separate manifest. Without the key, batches with files wait for
the next run's `before_run`; text and attachments are not split or silently dropped.
Preparing files does not establish that Orpheus accepted or delivered the input.
Each run accepts at most 32 clarification batches containing files, counted from
Orpheus admission history across connector restarts. Pending, uncertain, delivered,
and rejected admissions all consume this quota. Text-only clarifications do not.
At the limit, the next batch containing files stays in Mattermost for the next run;
its text and files stay together. The quota resets for each run. Already delivered
batches are never truncated from the next index. A batch waiting at the head of
the thread also holds up later messages to preserve order.

The ENV payload remains limited to 64 KiB, including the current request and index
references; oversized requests are rejected explicitly. Arbitrarily large
clarification deltas and additional paging are outside the supported contract.
A new run references the last run whose `before_run` completed successfully.
A failed preparation that never created an index does not break future runs.
Missing or corrupt pages of a confirmed index still fail preparation explicitly. Automatic reconstruction
from Orpheus envelopes is outside the supported contract; the helper never presents
an incomplete index as complete. Restore the workspace metadata or start a new
session to recover. Existing session files and the index chain are not trimmed.

`request.json` and its hash freeze the batch content, limits, and allowed channels.
`previous_index` and `delivered_batches` belong to a specific run and are excluded
from the batch hash. A prepared but undelivered clarification can therefore become
the next run's input without conflicting with immutable batch files.

## Outgoing files

The agent closes completed files and moves them atomically into the current run's
outbox. `after_run` first seals a snapshot, then uploads its files. It persists the
manifest after each upload. Retries use the sealed bytes, not a changed outbox.
Symlinks and workspace escapes are rejected. Per-file and per-run locks serialize
preparation and export; incomplete temporary copies cannot be published.

One output set belongs to `Run.final_message.id`, the last answer. Files attach
to its first text chunk. If there is no final message, the connector creates a
file-only post. Earlier answers and progress messages do not consume the set.

The connector verifies actual `post.file_ids` and `FileInfo.post_id`, not just
expected IDs in receipt properties. Partial attachment creates a separate repair
post without repeating the answer. Artifact identity derives from run ID,
relative path, and content hash.
A lost upload response can leave an unused file on the server.

If hook output is unavailable or an uploaded file has disappeared, the connector
publishes one durable `attachments_failed` notice and delivers the available text.
It does not read sandbox manifests, rerun export, reupload files, or manage sandbox
pauses through SDK. Ordinary publication retries still use verified receipts and
available file IDs. The next run gets a new outbox.

A link to the bot's previous output uses the input cache or downloads its
Mattermost upload, with the same access checks as other input files. The sealed
result remains available locally. Automatic receipt-to-snapshot reuse is outside
the supported contract; repeated download is acceptable.
