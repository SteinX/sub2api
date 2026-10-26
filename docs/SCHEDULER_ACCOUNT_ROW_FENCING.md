# Account row cache publication

A delayed scheduler rebuild can read account A, then publish it after an admin
update has committed and cached account B. The bucket epoch protects bucket
activation, but full account and metadata keys are shared across buckets. The
older publish can undo B even when its own bucket activation is later rejected.
For a configured Fusion name this can hide the pair and produce a route error
before the price admission check. A deterministic regression demonstrates this
overwrite; it does not prove the historical CI failure had this exact cause.

Waiting for the outbox, clearing L1 or configuring the fixture earlier cannot
prevent an already-running old writer. Publication now atomically compares the
account row version and updates full payload, slim metadata, version watermark
and L1 invalidation. A rejected snapshot cannot activate its stale member list.
Equal versions allow only the same protected row fields; the separate hot
last-used lane and independently hydrated relations are excluded from that row
fingerprint. Native credentials and pricing semantics are unchanged.

Migration 248 uses one BEFORE UPDATE trigger on `accounts` to set `updated_at`
to the greater of the database clock and the previous value plus one microsecond.
This makes the existing row timestamp strictly monotonic, including multiple
updates in one transaction and an older transaction that mutates last. Ordinary
`NOW()` or application timestamps do not provide that property. Cache comparison
uses fixed-width UTC strings, without Lua floating-point timestamp conversion.
There is no new column, Ent regeneration, association trigger or cross-row lock.

Full and metadata JSON retain their old top-level Account shape and add an
internal format/version marker. New readers reject unmarked old-process writes
and fall back to the database. A marked payload retains its version when the
independent watermark key is lost. The same authoritative row can repair an old
process's marker-stripping overwrite. Keys, bucket membership and outbox
watermarks are shared with the existing protocol; there is no second consumer
namespace. Mixed versions may cause cache misses. The old binary's own readers
still have the original defect; all serving instances must run the fix to obtain
its protection.

Confirmed deletion permanently fences the deleted account ID, which the product
does not reuse. Serialization failure only evicts a live projection when its
candidate is newer, and cannot evict a later valid row. The hot last-used key is
never rewritten by the guarded row publisher.

This change versions the `accounts` row. Pure association changes with the same
row version and the existing ID-only bucket member reuse are outside this fix's
claim. Adding relation triggers would introduce lock-order changes and needs a
separate design. Existing bucket lifecycle fencing remains in place.

The migration is forward-only. An old binary remains schema-compatible and the
timestamp trigger can remain if the application is rolled back, but rolling back
the code restores the old cache-read behavior and its known defect. This draft
does not authorize a production migration, cache flush, rollout or rollback.
