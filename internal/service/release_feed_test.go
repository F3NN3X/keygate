package service

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// validBase64Sig generates a syntactically valid 64-byte Ed25519 signature in
// raw base64 form for tests that need to verify Sparkle accepts it.
func validBase64Sig() string {
	raw := make([]byte, 64)
	for i := range raw {
		raw[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func mkRelease(version string, opts ...func(*model.Release, *model.ReleaseArtifact)) *FeedRelease {
	t := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	r := &model.Release{
		ID:           "rel-" + version,
		ProductID:    "prod-1",
		Version:      version,
		Channel:      model.ReleaseChannelStable,
		Name:         "MyApp",
		ReleaseNotes: "Bug fixes",
		Status:       model.ReleaseStatusPublished,
		PublishedAt:  &t,
	}
	a := &model.ReleaseArtifact{
		ID:          "art-" + version,
		ReleaseID:   r.ID,
		Platform:    "darwin-arm64",
		FileKey:     "releases/myapp/" + version + "/darwin-arm64.dmg",
		FileSize:    1024 * 1024,
		SHA256:      "abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
		ContentType: "application/x-apple-diskimage",
	}
	for _, opt := range opts {
		opt(r, a)
	}
	return &FeedRelease{
		Release:     r,
		Artifact:    a,
		DownloadURL: "https://signed.example.com/" + version,
	}
}

func TestRenderSparkleHappyPath(t *testing.T) {
	in := FeedInput{
		ProductID:   "prod-1",
		ProductName: "MyApp",
		BaseURL:     "https://example.com",
		Releases: []*FeedRelease{
			mkRelease("1.2.3", func(_ *model.Release, a *model.ReleaseArtifact) { a.Ed25519Sig = validBase64Sig() }),
		},
	}
	body, err := RenderSparkle(in)
	if err != nil {
		t.Fatalf("RenderSparkle: %v", err)
	}
	s := string(body)
	if !strings.HasPrefix(s, xml.Header) {
		t.Errorf("missing XML prolog")
	}
	for _, want := range []string{
		"<rss version=\"2.0\"",
		"xmlns:sparkle=",
		"<title>MyApp Updates</title>",
		"<sparkle:version>1.2.3</sparkle:version>",
		`url="https://signed.example.com/1.2.3"`,
		`length="1048576"`,
		"sparkle:edSignature=",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("sparkle output missing %q\n--- output ---\n%s", want, s)
		}
	}

	// Must round-trip parse.
	var roundTrip sparkleAppcast
	if err := xml.Unmarshal(body, &roundTrip); err != nil {
		t.Errorf("sparkle output failed to re-parse: %v", err)
	}
	if len(roundTrip.Channel.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(roundTrip.Channel.Items))
	}
}

func TestRenderSparkleOmitsInvalidSignature(t *testing.T) {
	in := FeedInput{
		ProductName: "MyApp",
		Releases: []*FeedRelease{
			mkRelease("1.2.3", func(_ *model.Release, a *model.ReleaseArtifact) {
				a.Ed25519Sig = "untrusted comment: garbage\nBASE64_NOT_RIGHT_FORMAT"
			}),
		},
	}
	body, err := RenderSparkle(in)
	if err != nil {
		t.Fatalf("RenderSparkle: %v", err)
	}
	if strings.Contains(string(body), "sparkle:edSignature=") {
		t.Errorf("expected invalid sig to be omitted; got:\n%s", string(body))
	}
}

func TestSanitizeCDATAEscapesEndMarker(t *testing.T) {
	notes := "before ]]> middle ]]> after"
	in := FeedInput{
		ProductName: "X",
		Releases: []*FeedRelease{
			mkRelease("1.0.0", func(r *model.Release, _ *model.ReleaseArtifact) { r.ReleaseNotes = notes }),
		},
	}
	body, err := RenderSparkle(in)
	if err != nil {
		t.Fatalf("RenderSparkle: %v", err)
	}
	if strings.Contains(string(body), "<![CDATA[before ]]>") {
		t.Errorf("CDATA terminator was not escaped; output:\n%s", string(body))
	}
	// Round-trip should still parse despite the embedded ]]>
	var rt sparkleAppcast
	if err := xml.Unmarshal(body, &rt); err != nil {
		t.Fatalf("sparkle XML parse failed: %v", err)
	}
}

func TestBuildVelopack(t *testing.T) {
	in := FeedInput{
		ProductName: "MyApp",
		Releases: []*FeedRelease{
			mkRelease("1.2.3"),
			mkRelease("1.3.0", func(_ *model.Release, a *model.ReleaseArtifact) {
				a.FileKey = "releases/.../app-1.3.0.nupkg"
			}),
		},
	}
	feed := BuildVelopack(in)
	if len(feed) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(feed))
	}
	if feed[0].ID != "v1.2.3" {
		t.Errorf("expected v1.2.3 prefix, got %q", feed[0].ID)
	}
	if !strings.HasSuffix(feed[0].Filename, ".dmg") {
		t.Errorf("expected .dmg extension preserved, got %q", feed[0].Filename)
	}
	if !strings.HasSuffix(feed[1].Filename, ".nupkg") {
		t.Errorf("expected .nupkg extension preserved, got %q", feed[1].Filename)
	}
	if feed[0].Type != "Full" {
		t.Errorf("expected Type=Full, got %q", feed[0].Type)
	}

	// Marshalable to JSON.
	if _, err := json.Marshal(feed); err != nil {
		t.Errorf("BuildVelopack output not JSON-serialisable: %v", err)
	}
}

func TestBuildVelopackEmpty(t *testing.T) {
	feed := BuildVelopack(FeedInput{})
	if len(feed) != 0 {
		t.Errorf("expected empty slice, got %v", feed)
	}
	body, err := json.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// MUST be `[]` not `null` — Velopack clients can't handle null.
	if string(body) != "[]" {
		t.Errorf("expected []; got %s", string(body))
	}
}

func TestBuildTauri(t *testing.T) {
	in := FeedInput{
		ProductName: "MyApp",
		Releases: []*FeedRelease{
			mkRelease("1.2.3"),
			mkRelease("1.3.0"), // Tauri picks first only
		},
	}
	m := BuildTauri(in)
	if m.Version != "1.2.3" {
		t.Errorf("Tauri picked wrong version: %q", m.Version)
	}
	if m.URL == "" {
		t.Errorf("Tauri URL empty")
	}
	if m.PubDate == "" {
		t.Errorf("Tauri pub_date empty")
	}
}

func TestBuildTauriEmpty(t *testing.T) {
	m := BuildTauri(FeedInput{})
	if m.Version != "" {
		t.Errorf("expected empty manifest, got %+v", m)
	}
}

// TestTauriEnvelopeVerifiesLikeTauri reproduces exactly what tauri-plugin-updater's verifier does with
// the feed's `signature` and the config `pubkey` — base64-DECODE each into minisign file text, parse a
// full 4-line signature, and verify BOTH the file signature and the global signature — and asserts our
// envelopes pass. This is the check the previous version of this test got wrong: it asserted a raw
// two-line envelope "matched the verifier", which the real verifier rejects at the base64-decode step
// (that shipped the field bug where every self-update failed with "signature could not be decoded").
func TestTauriEnvelopeVerifiesLikeTauri(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	message := []byte("the installer bytes")
	rawSig := ed25519.Sign(priv, message)
	// The global signature keygate must also produce: ed25519(raw_sig || trusted_comment_body).
	globalSig := ed25519.Sign(priv, append(append([]byte{}, rawSig...), []byte(tauriTrustedComment)...))

	pubB64 := base64.StdEncoding.EncodeToString(pub)
	envelope := TauriSignatureEnvelope(
		base64.StdEncoding.EncodeToString(rawSig),
		base64.StdEncoding.EncodeToString(globalSig),
		pubB64,
	)
	tauriPub := TauriPublicKey(pubB64)

	// ── what tauri's base64_to_string does: base64-decode the field into the minisign file text ──
	pubText, err := base64.StdEncoding.DecodeString(tauriPub)
	if err != nil {
		t.Fatalf("pubkey field must base64-decode (tauri base64_to_string): %v", err)
	}
	sigText, err := base64.StdEncoding.DecodeString(envelope)
	if err != nil {
		t.Fatalf("signature field must base64-decode (tauri base64_to_string): %v", err)
	}

	// ── PublicKey::decode: 2 lines; line 2 = base64("Ed"+key_id+32-byte key) ──
	pubLines := strings.Split(string(pubText), "\n")
	if len(pubLines) < 2 || !strings.HasPrefix(pubLines[0], "untrusted comment:") {
		t.Fatalf("pubkey file must be 'untrusted comment:'+key line, got %q", pubText)
	}
	pubBlob, err := base64.StdEncoding.DecodeString(pubLines[1])
	if err != nil || len(pubBlob) != 42 || string(pubBlob[0:2]) != "Ed" {
		t.Fatalf("pubkey line 2 must decode to 42-byte 'Ed'-prefixed blob, got %d bytes err=%v", len(pubBlob), err)
	}
	pubKeyID, pubKey := pubBlob[2:10], pubBlob[10:42]

	// ── Signature::decode: 4 lines (comment, 74-byte sig blob, trusted comment, 64-byte global sig) ──
	sigLines := strings.Split(string(sigText), "\n")
	if len(sigLines) != 4 {
		t.Fatalf("signature file must have 4 lines, got %d:\n%s", len(sigLines), sigText)
	}
	sigBlob, err := base64.StdEncoding.DecodeString(sigLines[1])
	if err != nil || len(sigBlob) != 74 || string(sigBlob[0:2]) != "Ed" {
		t.Fatalf("sig line 2 must decode to 74-byte 'Ed'-prefixed blob, got %d bytes err=%v", len(sigBlob), err)
	}
	if !strings.HasPrefix(sigLines[2], "trusted comment: ") {
		t.Fatalf("sig line 3 must start with 'trusted comment: ', got %q", sigLines[2])
	}
	globalBlob, err := base64.StdEncoding.DecodeString(sigLines[3])
	if err != nil || len(globalBlob) != 64 {
		t.Fatalf("sig line 4 must decode to a 64-byte global signature, got %d bytes err=%v", len(globalBlob), err)
	}

	// key_id must match between pubkey and signature, or tauri returns UnexpectedKeyId.
	if string(pubKeyID) != string(sigBlob[2:10]) {
		t.Errorf("sig key_id must equal pubkey key_id; sig=%x pub=%x", sigBlob[2:10], pubKeyID)
	}
	// The two verifications tauri's verify_ed25519 performs, against the parsed public key.
	if !ed25519.Verify(pubKey, message, sigBlob[10:74]) {
		t.Error("file signature must verify over the message with the parsed public key")
	}
	trustedBody := sigLines[2][len("trusted comment: "):]
	global := append(append([]byte{}, sigBlob[10:74]...), []byte(trustedBody)...)
	if !ed25519.Verify(pubKey, global, globalBlob) {
		t.Error("global signature must verify over (file_sig || trusted_comment)")
	}
}

func TestTauriEnvelopeEmptyOnUnsigned(t *testing.T) {
	if got := TauriSignatureEnvelope("", "g", "anything"); got != "" {
		t.Errorf("unsigned artifact must produce empty envelope, got %q", got)
	}
	if got := TauriSignatureEnvelope("s", "g", ""); got != "" {
		t.Errorf("missing pubkey must produce empty envelope, got %q", got)
	}
	if got := TauriSignatureEnvelope("s", "", "anything"); got != "" {
		t.Errorf("missing global signature must produce empty envelope, got %q", got)
	}
}

func TestTauriEnvelopeRejectsMalformedInputs(t *testing.T) {
	good64 := base64.StdEncoding.EncodeToString(make([]byte, 64))
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	// 32-byte sig (too short) → empty
	short := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if got := TauriSignatureEnvelope(short, good64, pub); got != "" {
		t.Errorf("short sig must produce empty envelope, got %q", got)
	}
	// short global sig → empty
	if got := TauriSignatureEnvelope(good64, short, pub); got != "" {
		t.Errorf("short global sig must produce empty envelope, got %q", got)
	}
	// non-base64 → empty
	if got := TauriSignatureEnvelope("!!!not base64!!!", good64, pub); got != "" {
		t.Errorf("malformed base64 must produce empty envelope, got %q", got)
	}
}

func TestIsValidFeedFormat(t *testing.T) {
	for _, ok := range []FeedFormat{FeedFormatSparkle, FeedFormatVelopack, FeedFormatTauri, FeedFormatJSON} {
		if !IsValidFeedFormat(ok) {
			t.Errorf("expected %q to be valid", ok)
		}
	}
	for _, bad := range []FeedFormat{"", "atom", "rss", "SPARKLE"} {
		if IsValidFeedFormat(bad) {
			t.Errorf("expected %q to be invalid", bad)
		}
	}
}
