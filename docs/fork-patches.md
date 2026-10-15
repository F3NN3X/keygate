# Fork patches — what `custom` carries on top of upstream

`license.deckshot.app` builds from this fork's **`custom`** branch, which is upstream
`tabloy/keygate`'s `main` plus the patch set below. **Currently tracking upstream v0.1.14** (merged
2026-10-15; previously v0.1.13, 2026-10-14). The fork's own `main` is kept as a clean mirror of
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
  config in `cmd/server/main.go`. `responseMeta` is therefore a method: new upstream call sites that use
  the bare `responseMeta()` function won't compile after a merge and need `s.` added (v0.1.11's
  activation-race path was one). v0.1.13 dropped the `entitlements` helper that sat beside it in
  `license.go`; the merge keeps only our `responseMeta`. Files: `internal/service/license.go`, `cmd/server/main.go`.

- **`/license/verify` returns owner email + activation usage** — `b5e3d33` (2026-09-07).
  Adds `email`, `activations_used`, `max_activations` to the `VerifyResult` so an SDK client can show a
  "2 / 3 devices · owner@…" summary from the refresh it already makes. Additive and backward-compatible;
  safe on a credential-free endpoint because verify only succeeds for a caller already holding a real key
  **and** an activated device. Files: `internal/service/license.go`, `internal/service/license_test.go`,
  `docs/openapi.yaml`. **Full detail + oracle analysis: [`verify-owner-and-activation-fields.md`](verify-owner-and-activation-fields.md).**

### Release feed — signing & Tauri

- **Tauri v2-compatible signature & public-key format** — ~~`7887733`, `014c074`~~ **UPSTREAMED in
  v0.1.13, patch dropped (2026-10-14).** Upstream fixed the same bug with its own design: a
  `tauri_signature` column holding the whole base64 minisign file (migration
  `20261005_artifact_tauri_signature`), built at publish in `internal/service/tauri_minisign.go`, and
  `BackfillTauriSignatures` on startup to fill it for artifacts signed before it existed. All signing
  and feed files were taken from upstream on the merge. The key id is derived the same way as ours
  (`sha256(pubkey)[:8]`), so signatures still verify against the pubkey DeckShot pinned from our build
  (key id `76eebeb39d2456fa`). What remains of our patch: migration
  `20260906_release_artifact_global_sig` and the `Ed25519GlobalSig` model field, kept only because the
  column exists in deployed databases (the migrator checksums applied files). Nothing reads or writes
  it now. Background: [`tauri-v2-signature-format.md`](tauri-v2-signature-format.md).
- **Signing-disabled error names everything signing needs** — part of `4ef2d15`. `ErrSigningDisabled`
  and the admin handler's messages name object storage as well as `RELEASE_KEY_ENCRYPTION_KEY`, since
  both are required. Upstream still says only the key is missing. Files:
  `internal/service/release_signing.go`, `internal/handler/release_signing_admin.go`.

### Release hosting — object storage (RustFS)

- **Let keygate reach the object store it signs from** — `0ff6702`, `4ef2d15`, `205168d`, `b1d295d`.
  Release signing and hosting need S3-compatible storage; the fork adds a RustFS compose
  (`deploy/rustfs/docker-compose.yml`) and the config to reach it over its **public** hostname
  (`s3.deckshot.app`), because presigned URLs bind the host into the SigV4 signature and a container
  reaching its host's public address hairpins — fixed with an `extra_hosts` host-gateway entry on the
  keygate service. `.env.example` adds `MAX_RELEASE_SIGN_SIZE_MB`, which upstream reads but doesn't list. It
  deliberately leaves out `STORAGE_PUBLIC_URL`: config reads it but nothing uses it, so setting it changes
  nothing (the compose still forwards it, harmlessly). Files: `docker-compose.yml`,
  `deploy/rustfs/docker-compose.yml`, `.env.example`, `internal/service/release.go`. Operational detail
  (bucket creation, the hairpin) lives in the DeckShot runbook's "Object storage — RustFS" section.

### Payments — Stripe

- **Ignore webhook API-version mismatch** — ~~`a9bbe0b`~~ **UPSTREAMED in v0.1.5, patch dropped
  (2026-09-07).** Our fix relaxed the webhook API-version check via `ConstructEventWithOptions{
  IgnoreAPIVersionMismatch: true}`. Upstream's v0.1.5 payment overhaul adopted the same approach (and
  added a signing-secret-rotation fallback), so on taking v0.1.5 the `stripe.go` conflict was resolved by
  taking upstream's file — our patch is no longer carried. No fork deviation remains here.

### Email

- **Purchase emails honour the custom licence template** — ~~`90c98a8`~~ **UPSTREAMED in v0.1.13, patch
  dropped (2026-10-14).** Upstream's `fulfillCheckout` now renders through
  `EmailService.RenderLicenseCreated` itself, and also passes the product's download URL, so
  `stripe.go` was taken from upstream. Our integration test
  (`TestFulfillCheckout_UsesCustomLicenseTemplate` in `internal/payment/fulfill_test.go`) is kept: it
  still passes and pins the money-path behaviour.

- **Implicit-TLS SMTP (SMTPS, port 465)** — ~~`531d927`~~ **UPSTREAMED in v0.1.11, patch dropped
  (2026-09-29).** Upstream's mailer now picks implicit TLS for port 465 itself (`resolvedEmail.implicitTLS`)
  and trims the port first. Our `implicit` flag to `pickAuth` isn't needed: `smtp.NewClient` already
  marks a `*tls.Conn` as TLS, so the LOGIN/PLAIN guards accept an SMTPS socket. Both files taken from
  upstream on the merge; no fork deviation remains here.

### Admin analytics

- **Activation-trend end-of-day bound** — `8ade397`. The admin Analytics → Activations chart read "No
  activation data available" for an activation created *today*, because the `to` bound floored to
  midnight. Includes the full `to` day. Files: `internal/handler/admin.go`.

### Build & deploy (Dokploy, self-hosted)

- **Version stamped from a `VERSION` file** — `a5bf990`, `50502a9` (supersedes the git-describe stamp in
  `0d520ab`). Dokploy clones without tags, so `git describe` produced a bare SHA the update checker read
  as `0.0.0` (perpetually "out of date"). A committed `VERSION` file is the source of truth; bump it when
  taking a release. Since v0.1.11 the build step also keeps upstream's `TARGETOS`/`TARGETARCH`
  cross-compile args, so the Dockerfile conflict on a merge is the `RUN go build` block, and the fix is
  to combine both. Files: `VERSION`, `Dockerfile`.
- **Build from source, adapted for Dokploy** — `02a63cd`, `b86b415`, `14dc154`. Builds the image from
  source (`build: .`) instead of upstream's published image, with a `docker-compose.yml` shaped for
  Dokploy at `license.deckshot.app` (no published host port, `dokploy-network` for Traefik, secrets from
  env). **Note the `environment:` block is a whitelist** — a variable absent from it never reaches the
  container however Dokploy sets it. Files: `Dockerfile`, `docker-compose.yml`, `.dockerignore`.
- **Forward `RELEASE_KEY_ENCRYPTION_KEY` to the container** — `95fe24b`. The whitelist above is exactly
  how this variable once went missing; this adds it (and the `STORAGE_*` set) to the compose
  `environment:`. v0.1.11 added `REDIS_URL` (optional shared rate-limit backend; empty = in-memory), which
  is forwarded the same way. Files: `docker-compose.yml`, `.env.example`.

## Cross-references

The consuming product (DeckShot) documents the *operational* side of running against this instance —
endpoint contract, go-live steps, the RustFS hairpin, taking an upstream release — in
`docs/operations/runbooks/keygate-licensing.md` in the `F3NN3X/deckshot` repo. This file is the
*source-side* record: what diverges from upstream and why.
</content>
