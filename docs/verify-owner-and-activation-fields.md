# `/license/verify`: owner email and activation usage

This documents a fork addition (F3NN3X/keygate `custom`, 2026-09-07) to the public **`POST
/license/verify`** response. It is written for anyone maintaining keygate (this fork or upstream
`tabloy/keygate`): the change is additive and backward-compatible, but it puts three new fields on a
credential-free SDK endpoint, so the trust reasoning matters and is recorded here in full.

## TL;DR

- `VerifyResult` now carries `email`, `activations_used`, and `max_activations`.
- They let an SDK client render a licence summary — "2 / 3 devices · owner@example.com" — from the
  refresh it already makes, with no second call, no new route, and no new auth surface.
- Safe on this endpoint **specifically** because `verify` only returns success for a caller that already
  holds a real `license_key` **and** an activated device slot. It is the owner reading their own summary,
  not a new key-existence oracle.
- The per-device **list** deliberately stays behind the session-auth portal
  (`/portal/licenses/:license_key/activations`). Only the aggregate counts and the owner's own email
  ride the credential-free SDK.

## What changed

`internal/service/license.go`:

- `VerifyResult` gained three fields (JSON `email` (omitempty), `activations_used`, `max_activations`).
- `Verify` populates them after the existing success path: `email` from `lic.Email`, `max_activations`
  from `s.maxActivations(lic)` (the plan cap, default 3), and `activations_used` from
  `s.store.CountActivations(ctx, lic.ID)`.
- The count is **best-effort**: if the count query errors it logs a warning and degrades to `0` rather
  than failing an otherwise-valid verify. The `FindActivation` lookup earlier in `Verify` already proved
  the DB is reachable, so a count failure here is vanishingly unlikely and self-heals on the next refresh.

`internal/service/license_test.go`:

- `TestVerifyResultJSONShape` pins the wire contract — the exact JSON key names (`activations_used`,
  `max_activations`, `email`) and that `email` is `omitempty` while the two counts always emit (a real
  `0` must reach the client, not vanish). A silent struct-tag rename would otherwise leave a client
  rendering a blank owner or "0 / 0" with no error; this catches it without needing a database.

`docs/openapi.yaml`: the three fields are added to `VerifyResponse.data` with the same rationale inline.

## Why it is safe here — the oracle analysis

keygate is deliberately strict about not being a **license-key existence oracle**. `Verify`,
`Deactivate`, and the other public SDK paths collapse every "license-knowable" failure (unknown key,
wrong product, suspended / revoked / expired, not-activated-for-this-device) into the same
`404 LICENSE_NOT_FOUND`, so the endpoint surface cannot be probed to learn which key strings are real
(`licenseNotFound()` and its call sites).

`Verify` reaches the success path — the only path that now carries `email` — **only** after:

1. the `license_key` resolves to a real licence (`FindLicenseByKey`),
2. the licence is usable (`assertUsable`), and
3. **the caller's `identifier` is already an activation on that licence** (`FindActivation`) — a
   not-yet-activated device gets `404`, identical to a nonexistent key.

So a caller who receives an email back has already demonstrated possession of a valid key *and* an
active device slot on it. That is the licence owner (or a device they activated) reading their own
summary. Returning their own email and their own slot counts to them crosses no boundary the endpoint
did not already sit behind — it is strictly less capability than `activate` / `deactivate`, which this
same `license_key` already drives.

Two guard rails are kept:

- **Rate limiting is unchanged.** The `/license` group runs behind `LicenseBruteForceGuard` and a
  per-route-family IP cap, and `Verify` records key/IP failures into the brute-force tracker on every
  non-success path. Key-guessing to fish for an email is throttled exactly as before.
- **No new failure-path disclosure.** The email is attached on the success path only. Every failure
  still collapses to `404 LICENSE_NOT_FOUND`, so the field cannot be used to test key/email pairs.

The DeckShot client's own trust model already treats the `license_key` as bearer-equivalent (it alone
drives activate/verify/deactivate), and ADR-015 there accepts an even weaker bearer (a signed email
link) for the adjacent slot-management action — so this addition is consistent with the consuming
product's accepted stance, for one person reading their own machines.

## Why the device list is NOT here

Listing each activation (identifier, label, IP, last-seen) is a heavier disclosure than an aggregate
count, and keygate already secures it: `GET /portal/licenses/:license_key/activations` requires a
logged-in portal session (email OTP or an accepted seat), by the branch's own design that the
`license_key` is a **device** credential, not a **management** credential (`cmd/server/main.go`, the
`/license` group comment). This change does not move that line — it exposes only the two counts needed
for an "N / max" badge. A consumer that wants the full list deep-links a human to the portal.

## Backward compatibility

Additive JSON on the `{success, data}` envelope. Older clients ignore the new fields; a client written
against this addition treats them as optional (absent → the summary is simply not shown). No migration,
no new route, no config.

## Consumer

Built for the DeckShot desktop client, which renders "ACTIVE 2 / 3 · owner@…" on its licence card and a
"Manage devices" link to the portal. See that repo's
`docs/operations/runbooks/keygate-licensing.md` and `docs/audits/2026-09-07-licence-process-and-reporting-audit.md`.
</content>
