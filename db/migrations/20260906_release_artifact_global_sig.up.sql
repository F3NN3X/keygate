-- Tauri's updater (minisign-verify) requires a FULL minisign signature: besides the signature over the
-- file, a "global" signature over (raw_sig || trusted_comment). Only the private key can make it, at
-- sign time, so it is stored here for the Tauri feed to assemble the 4-line envelope. Empty for
-- artifacts signed before this and for unsigned ones.
ALTER TABLE release_artifacts ADD COLUMN IF NOT EXISTS ed25519_global_sig TEXT NOT NULL DEFAULT '';
