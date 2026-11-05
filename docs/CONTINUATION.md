# Continuing after sandbox deletion

Orpheus expires paused reusable sandboxes after a configured retention period
(default 14 days). When the previous sandbox is deleting, deleted, or unavailable,
the next new request starts a new session in the same Mattermost thread. The
connector rebuilds context and attachments from the available thread history.
Files and processes created only inside the old sandbox cannot be restored.

If deletion races with submission, the connector rereads acceptance history
before rebuilding unaccepted input. A run that was accepted and then failed with
`sandbox_lost` is reported as failed and is not automatically rerun. Send a new
request to continue.

An undelivered clarification from a successfully completed run can be transferred
to the next run. When it moves to a new session, for example after sandbox expiry, the connector
preserves the saved clarification and rebuilds surrounding context and attachments. It does not transfer input from failed or cancelled runs.

Deploy Mattermost connector v0.6.3 or newer before starting a core worker with
paused sandbox retention (Orpheus v0.7.0 or newer).
