# Acceptance testing

Run `make fix gofix check` and `make smoke` for local acceptance. CI uses mock
services only. The live suite uses real Mattermost, AgentBox and model calls and
must be run manually with the environment described in the README.

## External compatibility

The validated combination is Mattermost 10.10.1 (REST API v4), Orpheus 0.1.2,
and the public AgentBox `codex` template rebuilt with envd 0.6.16 and Python 3.11.
The test profile uses Codex with an image-capable model and `view_image`.
Other templates and profiles need their own live checks.

`TestLiveOrpheusImagesAndOutput` checks control images with random filenames:

1. Recognize a red triangle in the first run and a blue circle delivered as an
   active clarification. Attach the generated report to the final message.
2. Wait for memory pause, resume the same session, and open the original image,
   the delivered clarification image and a new image. Keep each run's outbox
   separate and verify publication with fresh engine and publisher objects.
3. Expand a permalink to another thread, download its image through `before_run`,
   recognize it and attach another report.

Only the test's own posts in `dev-test-group` and disposable sandboxes are changed.
The credentials belong to the bot: the link-expansion scenario represents the
trigger's author as a human in memory. Linked posts, file metadata, downloads,
uploads and agent execution use the real APIs. Trigger policy, channel routing
and runtime event scheduling are tested separately with local fixtures.

For worker recovery, use a dedicated local Orpheus worker. During the second run,
wait until its `sleep 30` tool call is running, record the run ID, and kill that
worker with SIGKILL. Start it again before the run timeout. Do not kill a shared
production worker. The test must complete on the same session and run, keep
exactly two runs before the linked-thread step, and publish each result once.
The live test does not stop workers itself; a normal passing run does not attest
that crash injection was performed.

`TestLiveSandboxFileHooks` checks the embedded Python protocol independently:
input download, SDK clarification import, output upload and attachment, then
memory pause/resume with the previous index and delivered manifests. It logs the
actual sandbox envd version. No connector binary is installed in the template.

## Recovery workload

Run this offline test through the tools container:

```sh
docker compose --profile tools run --rm --no-deps tools \
  go test -v -count=5 -run '^TestReplayRecovery1000Threads$' ./internal/service
```

The workload has 1,000 threads and 10,000 historical source posts. Of these,
250 threads have completed Orpheus sessions with accepted inputs and unpublished
answers; the other 750 contain passive conversation. No WebSocket or SSE hint is
sent. A fresh runtime must discover the sessions, publish all 250 answers and
drain its queue. A second fresh runtime must recover without new posts or runs.

The test logs wall time through queue drain, reconciliation count and publication
count. It measures local processing with in-memory API substitutes, including
discovery, context selection and receipt replay. It excludes HTTP latency,
Mattermost rate limits, external database cost and sandbox startup; it is not a
production SLA. Pagination is tested independently with HTTP fixtures containing
405 sessions, runs and history messages, plus equal-timestamp Mattermost posts.

## Fault coverage

| Behavior | Regression tests |
| --- | --- |
| Lost admission, edited source/config, accepted delivery states, rejected clarification transfer | `service/reconcile_test.go`, `service/engine_test.go` |
| Connector restart, partial answer, lost POST after server cache expiry, attachment repair | `delivery/delivery_test.go`, `service/replay_test.go` |
| Missing/failed/truncated export, wrong bot, output still uploading | `service/fault_test.go` |
| Commentary policy, multiple answers, file-only final answer, failed run | `service/fault_test.go` |
| 32/33 clarification batches, optional SDK, before-run failure, finalizing and revoked scope | `service/reconcile_test.go`, `agentbox/access_test.go` |
| HTTP backoff, Retry-After, permanent failures, capacity, reload and shutdown | `service/fault_test.go`, `service/runtime_test.go` |
| Missing/corrupt input index, interrupted stream, output process crash, partial upload | `sandbox/test_files.py` |
| Same-thread references, far linked target, channel policy, bot/Markdown triggers and budgets | `conversation/context_test.go`, `config/config_test.go` |
| Pagination, SSE cursor recovery, redirects and stream limits | `orpheus/client_test.go`, `mattermost/client_test.go` |

Exactly-once publication across two independent APIs is not guaranteed. If a POST
commits but its response and receipt remain unavailable after Mattermost's
deduplication cache expires, retry can duplicate a text post. A regression test
models this remaining window explicitly. Visible receipts suppress duplicates
without relying on either process's memory or the server cache.
