package peer

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustGenKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

func mustNonce(t *testing.T) string {
	t.Helper()
	var b [AuthNonceLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b[:])
}

func freshInput(t *testing.T) (SigningInput, ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv := mustGenKey(t)
	in := SigningInput{
		DeviceID: "deadbeef0123456789abcdef01234567",
		Audience: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TS:       time.Now().UnixMilli(),
		Nonce:    mustNonce(t),
		Method:   "GET",
		Path:     "/api/v1/peers/events",
		Body:     nil,
	}
	return in, pub, priv
}

func TestSignVerifyRoundTrip(t *testing.T) {
	in, pub, priv := freshInput(t)
	sig := Sign(priv, in)
	if err := Verify(pub, sig, in); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyFailsOnTamperedPath(t *testing.T) {
	in, pub, priv := freshInput(t)
	sig := Sign(priv, in)
	in.Path = "/api/v1/peers/admin" // attacker swap
	if err := Verify(pub, sig, in); !errors.Is(err, ErrAuthBadSignature) {
		t.Errorf("tampered path: want ErrAuthBadSignature, got %v", err)
	}
}

func TestVerifyFailsOnTamperedRawQuery(t *testing.T) {
	in, pub, priv := freshInput(t)
	in.RawQuery = "since=10"
	sig := Sign(priv, in)
	in.RawQuery = "since=99999" // attacker bumps the cursor
	if err := Verify(pub, sig, in); !errors.Is(err, ErrAuthBadSignature) {
		t.Errorf("tampered query: want ErrAuthBadSignature, got %v", err)
	}
}

func TestVerifyFailsOnTamperedBody(t *testing.T) {
	in, pub, priv := freshInput(t)
	in.Body = []byte("original")
	sig := Sign(priv, in)
	in.Body = []byte("tampered")
	if err := Verify(pub, sig, in); !errors.Is(err, ErrAuthBadSignature) {
		t.Errorf("tampered body: want ErrAuthBadSignature, got %v", err)
	}
}

func TestVerifyFailsOnWrongPublicKey(t *testing.T) {
	in, _, priv := freshInput(t)
	sig := Sign(priv, in)
	otherPub, _ := mustGenKey(t)
	if err := Verify(otherPub, sig, in); !errors.Is(err, ErrAuthBadSignature) {
		t.Errorf("wrong pubkey: want ErrAuthBadSignature, got %v", err)
	}
}

func TestVerifyFailsOnCrossDomainSignature(t *testing.T) {
	// A signature minted by a DIFFERENT domain prefix must not
	// validate under this verifier — that's the whole point of
	// AuthDomainPrefix's "kojo-peer-auth-v1\n" line.
	in, pub, priv := freshInput(t)
	// Build a fake "v0" payload that omits the domain prefix —
	// what an older signing impl would have produced. We sign it
	// directly with raw ed25519 and submit it through Verify which
	// uses the v1 payload shape.
	rawPayload := []byte(in.DeviceID + "\n" + "0\n" + in.Nonce + "\n" + in.Method + "\n" + in.Path + "\n")
	fakeSig := ed25519.Sign(priv, rawPayload)
	sigB64 := base64.StdEncoding.EncodeToString(fakeSig)
	if err := Verify(pub, sigB64, in); !errors.Is(err, ErrAuthBadSignature) {
		t.Errorf("cross-domain sig: want ErrAuthBadSignature, got %v", err)
	}
}

func TestVerifyRejectsMalformedBase64Sig(t *testing.T) {
	in, pub, _ := freshInput(t)
	if err := Verify(pub, "***not-base64***", in); !errors.Is(err, ErrAuthBadSignature) {
		t.Errorf("malformed b64: want ErrAuthBadSignature, got %v", err)
	}
}

func TestCheckTimestampWindow(t *testing.T) {
	now := int64(1_700_000_000_000)
	cases := []struct {
		name string
		ts   int64
		want error
	}{
		{"exact now", now, nil},
		{"+1 minute", now + 60_000, nil},
		{"-1 minute", now - 60_000, nil},
		{"+5min +1ms (out)", now + AuthMaxClockSkew.Milliseconds() + 1, ErrAuthStaleTimestamp},
		{"-5min -1ms (out)", now - AuthMaxClockSkew.Milliseconds() - 1, ErrAuthStaleTimestamp},
		{"way in the future", now + 24*60*60*1000, ErrAuthStaleTimestamp},
		// Overflow-safety: attacker-controlled extreme values must
		// stay outside the window even when the naive |delta|
		// arithmetic would overflow.
		{"int64 min", -9223372036854775808, ErrAuthStaleTimestamp},
		{"int64 max", 9223372036854775807, ErrAuthStaleTimestamp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckTimestamp(c.ts, now)
			if c.want == nil {
				if err != nil {
					t.Errorf("want nil, got %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Errorf("want %v, got %v", c.want, err)
			}
		})
	}
}

func TestCheckNonceShape(t *testing.T) {
	if err := CheckNonce(mustNonce(t)); err != nil {
		t.Errorf("valid nonce rejected: %v", err)
	}
	if err := CheckNonce(""); !errors.Is(err, ErrAuthMalformedHeader) {
		t.Errorf("empty nonce: want ErrAuthMalformedHeader, got %v", err)
	}
	if err := CheckNonce("not-base64-!!"); !errors.Is(err, ErrAuthMalformedHeader) {
		t.Errorf("bad b64: want ErrAuthMalformedHeader, got %v", err)
	}
	// Too short.
	short := base64.StdEncoding.EncodeToString([]byte("only-7b"))
	if err := CheckNonce(short); !errors.Is(err, ErrAuthMalformedHeader) {
		t.Errorf("short nonce: want ErrAuthMalformedHeader, got %v", err)
	}
}

func TestNonceCacheDetectsReplay(t *testing.T) {
	c := NewNonceCache(AuthMaxClockSkew)
	deviceID := "peer-a"
	nonce := mustNonce(t)
	if c.Seen(deviceID, nonce) {
		t.Fatal("fresh nonce reported as seen")
	}
	if !c.Seen(deviceID, nonce) {
		t.Errorf("replay not detected")
	}
}

func TestNonceCacheCommitRetainsAcrossTimestampWindow(t *testing.T) {
	// A request whose timestamp is at +maxAge (peer clock leads
	// ours) must have its nonce remembered until that timestamp
	// falls out of CheckTimestamp's accept window — i.e. ts + maxAge
	// from our clock's POV. The 2×maxAge retention in Commit is
	// what guarantees this.
	c := NewNonceCache(100 * time.Millisecond)
	var now time.Time
	c.now = func() time.Time { return now }
	now = time.UnixMilli(1000)
	// Sender's clock leads ours by maxAge. ts is at the upper
	// edge of the accept window.
	ts := now.Add(100 * time.Millisecond).UnixMilli()
	if dup := c.Commit("peer-a", "nonce-1", ts); dup {
		t.Fatal("fresh Commit reported dup")
	}
	// Advance ours by maxAge. The original ts is now at the
	// boundary — a replay must still be detected.
	now = now.Add(100 * time.Millisecond)
	if dup := c.Commit("peer-a", "nonce-1", ts); !dup {
		t.Errorf("replay within timestamp accept window not detected")
	}
}

func TestNonceCacheDistinguishesPeers(t *testing.T) {
	c := NewNonceCache(AuthMaxClockSkew)
	nonce := mustNonce(t)
	if c.Seen("peer-a", nonce) {
		t.Fatal("fresh peer-a reported seen")
	}
	// Same nonce from a DIFFERENT peer is treated as fresh — the
	// cache key includes device_id.
	if c.Seen("peer-b", nonce) {
		t.Errorf("peer-b reported seen on peer-a's nonce")
	}
}

func TestNonceCacheExpiresAfterTTL(t *testing.T) {
	c := NewNonceCache(100 * time.Millisecond)
	// Pin the cache's clock so the test isn't flaky on slow CI.
	var now time.Time
	c.now = func() time.Time { return now }
	now = time.Now()
	if c.Seen("peer-a", "nonce-1") {
		t.Fatal("fresh nonce seen")
	}
	// Advance past TTL + sweep cadence.
	now = now.Add(200 * time.Millisecond)
	if c.Seen("peer-a", "nonce-1") {
		t.Errorf("post-TTL nonce should be re-acceptable (entry purged)")
	}
}

func TestCanonicalPayloadIsStable(t *testing.T) {
	// Pin the canonical byte shape so a future change to
	// CanonicalPayload that re-orders fields or drops a delimiter
	// surfaces here. Without this, the sender and receiver could
	// silently diverge as the encoding evolves.
	in := SigningInput{
		DeviceID: "abc",
		Audience: "xyz",
		TS:       42,
		Nonce:    "n",
		Method:   "get",
		Path:     "/p",
		RawQuery: "q=1",
		Body:     []byte("hi"),
	}
	got := string(in.CanonicalPayload())
	wantHead := "kojo-peer-auth-v1\nabc\nxyz\n42\nn\nGET\n/p\nq=1\n"
	if !strings.HasPrefix(got, wantHead) {
		t.Errorf("payload head mismatch:\nwant prefix: %q\ngot: %q", wantHead, got)
	}
	// 64 hex chars of sha256("hi") + nothing else.
	bodyHashPart := got[len(wantHead):]
	if len(bodyHashPart) != 64 {
		t.Errorf("body hash suffix length = %d, want 64", len(bodyHashPart))
	}
}
