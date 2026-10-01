# Bounded Step tool budget

Only the exact Step 3.7 Flash free Chat path can use this runtime. The fixed
official template counts tool definitions locally; it does not summarize or
send customer content to another model. Caller tools and output limits remain
unchanged. Counts receive 10% plus 4096 tokens of conservative padding. Other
models, protocols, media, opaque content and requests that already fit retain
the existing path.

CircleCI packages Python 3.13 dependencies and four public assets pinned to
revision `5f6244077ac62e04eec3f320501ff8c2b293373a`, validates their hashes, and
tests the real packaged worker in a network-disabled container. Production
uses that CI artifact, never a local virtual environment. A manifest records
every packaged file. No weights or remote model code are loaded.

`CONTEXT_STEP_TOOLS_BUNDLE` selects the unpacked, verified CI runtime. Its
absence leaves the old behavior intact. `CONTEXT_RECALL_METADATA_ONLY=true`
disables the sidecar's request-body logger while retaining numeric recall
metadata. Existing services keep their default logging behavior.

The existing hot policy accepts independent `step_tools_mode` (`shadow`,
`apply`, or absent/off) and `step_tools_percent` (0–100). Invalid new flags
disable only this feature. Shadow counts after the existing selection and
must produce exactly the old request body. Apply changes only the estimated
tool reservation and protects already-retained dependencies referenced by
cached Read notices. Existing recall and repeated-row controls stay separate.

Two owned Python processes handle schemas up to 512 KiB. There is no queue.
Busy, deadline, invalid schema, startup failure or unavailable workers fall
back to the old estimate. Cache entries contain only schema hashes and numeric
counts, with at most 256 entries and one-hour expiry. Native text caches are
cleared after each job. Counting is limited to 200 ms including validation and
serialization; counting and recall share a 250 ms additional-work deadline.
Existing parsing and synchronous scans are not an end-to-end hard deadline.

Request cancellation kills and waits for the exact borrowed worker. A single
supervisor can restore dead slots; requests do not wait for it. The application
closes the pool after HTTP shutdown. Numeric metadata distinguishes eligible,
sampled, shadow-counted, budget-applied, busy, deadline and worker errors.
Canonical input hashes identify an exact input, not a platform request or final
user result. Budget application is not answer correctness.

Rollback removes the new policy fields (or sets the new percentage to zero).
The original recall settings remain active. Independent sidecar rollout must
preserve old connections and verify process, artifact, port, policy and the
first matching NAT rule; a control file alone is insufficient.
