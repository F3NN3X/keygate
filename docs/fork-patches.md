# Fork patches — what `custom` carries on top of upstream

`license.deckshot.app` builds from this fork's **`custom`** branch, which is upstream
`tabloy/keygate`'s `main` plus the patch set below. The fork's own `main` is kept as a clean mirror of
upstream, so `git log --oneline main..custom` is the authoritative, current list of deviations — this
document is its annotated form: what each patch is, why it exists, and where the detail lives.

Keep this file honest when you touch `custom`: a new deviation gets an entry here, and taking an
upstream release (fast-forward `main`, merge into `custom`) is the moment to prune anything upstream has
absorbed.

## How to regenerate the raw list

```
git fetch upstream --tags        # or the fork's own main, kept as the upstream mirror
git log --oneline --no-merges main..custom
git diff --stat main..custom
```

## The patches

Grouped by area, newest first within each group. SHAs are the `custom` commits as of 2026-09-07.

### API — licence SDK

- **Response `meta.instance` = this deployment's public origin** — 2026-09-07. `responseMeta` (activate /
  verify) now adds `instance` = `cfg.BaseURL` (e.g. `https://license.deckshot.app`) alongside the existing
  `server` / `url`. The AGPL §7(b) attribution (`server` = "Keygate", `url` = keygate.app, from
  `internal/branding`) is **kept unchanged** — `instance` is additive so a caller can see which deployment
  answered without the attribution being repurposed. `LicenseService` gained a `baseURL` field, wired from
  config in `cmd/server/main.go`. Files: `internal/service/license.go`, `cmd/server/main.go`.

- **`/license/verify` returns owner email + activation usage** — `b5e3d33` (2026-09-07).
  Adds `email`, `activations_used`, `max_activations` to the `VerifyResult` so an SDK client can show a
  "2 / 3 devices · owner@…" summary from the refresh it already makes. Additive and backward-compatible;
  safe on a credential-free endpoint because verify only succeeds for a caller already holding a real key
  **and** an activated device. Files: `internal/service/license.go`, `internal/service/license_test.go`,
  `docs/openapi.yaml`. **Full detail + oracle analysis: [`verify-owner-and-activation-fields.md`](verify-owner-and-activation-fields.md).**

### Release feed — signing & Tauri

- **Tauri v2-compatible signature & public-key format** — `7887733`, `014c074` (+ migration
  `20260906_release_artifact_global_sig`). keygate emitted a raw, un-base64-wrapped 2-line minisign
  signature and a bare pubkey; Tauri v2's updater requires base64-wrapped full minisign files and a
  4-line signature with a **global signature**. Without this every self-update failed in the field at
  the pubkey-decode step. Adds an `ed25519_global_sig` column and computes the global signature at
  publish. Files: `internal/service/release_signing.go`, `internal/service/release_feed.go`,
  `internal/store/release.go`, `internal/handler/release_signing_admin.go`, `internal/model/model.go`.
  **Full detail: [`tauri-v2-signature-format.md`](tauri-v2-signature-format.md).**

### Release hosting — object storage (RustFS)

- **Let keygate reach the object store it signs from** — `0ff6702`, `4ef2d15`, `205168d`, `b1d295d`.
  Release signing and hosting need S3-compatible storage; the fork adds a RustFS compose
  (`deploy/rustfs/docker-compose.yml`) and the config to reach it over its **public** hostname
  (`s3.deckshot.app`), because presigned URLs bind the host into the SigV4 signature and a container
  reaching its host's public address hairpins — fixed with an `extra_hosts` host-gateway entry on the
  keygate service. `.env.example` documents the `STORAGE_*` variables. Files: `docker-compose.yml`,
  `deploy/rustfs/docker-compose.yml`, `.env.example`, `internal/service/release.go`. Operational detail
  (bucket creation, the hairpin) lives in the DeckShot runbook's "Object storage — RustFS" section.

### Payments — Stripe

- **Ignore webhook API-version mismatch** — `a9bbe0b`. keygate auto-registers its Stripe webhook using
  the account's *default* API version, but its pinned `stripe-go` expects an older one, so events failed
  signature verification and every `POST /webhook/stripe` returned 400 — issuance then rode only on the
  success-page fallback, missing any buyer who closed the tab. Switches to `ConstructEventWithOptions`
  with `IgnoreAPIVersionMismatch`: the HMAC signature and the livemode gate still apply; only the version
  check is relaxed. Files: `internal/payment/stripe.go`.

### Email

- **Implicit-TLS SMTP (SMTPS, port 465)** — `531d927`. Cloudflare Email Service offers no STARTTLS on
  587, and upstream's mailer only did plaintext-dial-then-STARTTLS. Adds an implicit-TLS (465) dial path
  so key-delivery email can leave the box. Files: `internal/service/email.go`,
  `internal/service/email_auth_test.go`.

### Admin analytics

- **Activation-trend end-of-day bound** — `8ade397`. The admin Analytics → Activations chart read "No
  activation data available" for an activation created *today*, because the `to` bound floored to
  midnight. Includes the full `to` day. Files: `internal/handler/admin.go`.

### Build & deploy (Dokploy, self-hosted)

- **Version stamped from a `VERSION` file** — `a5bf990`, `50502a9` (supersedes the git-describe stamp in
  `0d520ab`). Dokploy clones without tags, so `git describe` produced a bare SHA the update checker read
  as `0.0.0` (perpetually "out of date"). A committed `VERSION` file is the source of truth; bump it when
  taking a release. Files: `VERSION`, `Dockerfile`.
- **Build from source, adapted for Dokploy** — `02a63cd`, `b86b415`, `14dc154`. Builds the image from
  source (`build: .`) instead of upstream's published image, with a `docker-compose.yml` shaped for
  Dokploy at `license.deckshot.app` (no published host port, `dokploy-network` for Traefik, secrets from
  env). **Note the `environment:` block is a whitelist** — a variable absent from it never reaches the
  container however Dokploy sets it. Files: `Dockerfile`, `docker-compose.yml`, `.dockerignore`.
- **Forward `RELEASE_KEY_ENCRYPTION_KEY` to the container** — `95fe24b`. The whitelist above is exactly
  how this variable once went missing; this adds it (and the `STORAGE_*` set) to the compose
  `environment:`. Files: `docker-compose.yml`, `.env.example`.

## Cross-references

The consuming product (DeckShot) documents the *operational* side of running against this instance —
endpoint contract, go-live steps, the RustFS hairpin, taking an upstream release — in
`docs/operations/runbooks/keygate-licensing.md` in the `F3NN3X/deckshot` repo. This file is the
*source-side* record: what diverges from upstream and why.
</content>
