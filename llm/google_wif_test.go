package llm

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testAudience = "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/x"

// fakeSTS verifies each assertion the way Google's provider does — the
// signature against the public key, the kid, and the claims — and answers
// with a numbered token.
func fakeSTS(t *testing.T, pub *rsa.PublicKey, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req["audience"] != testAudience || req["subjectTokenType"] != "urn:ietf:params:oauth:token-type:jwt" {
			t.Errorf("exchange request = %v", req)
		}
		parts := strings.Split(req["subjectToken"], ".")
		if len(parts) != 3 {
			t.Fatalf("assertion has %d parts", len(parts))
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
			t.Errorf("signature: %v", err)
		}
		var header, claims map[string]any
		h, _ := base64.RawURLEncoding.DecodeString(parts[0])
		c, _ := base64.RawURLEncoding.DecodeString(parts[1])
		_ = json.Unmarshal(h, &header)
		_ = json.Unmarshal(c, &claims)
		if header["alg"] != "RS256" || header["kid"] != rsaThumbprint(pub) {
			t.Errorf("header = %v", header)
		}
		if claims["iss"] != "https://app.test" || claims["sub"] != "app:test" || claims["aud"] != "https:"+testAudience {
			t.Errorf("claims = %v", claims)
		}
		n := calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + string(rune('0'+n)), "expires_in": 3600})
	}))
}

func newTestWIF(t *testing.T, key *rsa.PrivateKey, sts string, now *time.Time) *GoogleWIF {
	t.Helper()
	g, err := NewGoogleWIF(GoogleWIFConfig{
		Audience: testAudience, Issuer: "https://app.test", Subject: "app:test",
		Key: key, STSURL: sts, Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGoogleWIFCachesUntilNearExpiry(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var calls atomic.Int32
	sts := fakeSTS(t, &key.PublicKey, &calls)
	defer sts.Close()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	g := newTestWIF(t, key, sts.URL, &now)

	for range 2 {
		if tok, err := g.Token(context.Background()); err != nil || tok != "tok-1" {
			t.Fatalf("Token = %q, %v; want tok-1", tok, err)
		}
	}
	// Inside the last minute of the hour the token is renewed.
	now = now.Add(3600*time.Second - 30*time.Second)
	if tok, _ := g.Token(context.Background()); tok != "tok-2" {
		t.Fatalf("Token near expiry = %q, want tok-2", tok)
	}
	if calls.Load() != 2 {
		t.Fatalf("exchanges = %d, want 2", calls.Load())
	}
}

func TestGoogleWIFRefusalCarriesReason(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"unauthorized_client","error_description":"rejected by the attribute condition"}`))
	}))
	defer sts.Close()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	_, err := newTestWIF(t, key, sts.URL, &now).Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "attribute condition") {
		t.Fatalf("err = %v, want the STS reason", err)
	}
}

// The kid must equal authit's jwt.KeyID for the same key, or the provider
// cannot find the key in the JWKS authit published. RFC 7638 §3.1 fixes it.
func TestRSAThumbprintMatchesRFC7638(t *testing.T) {
	n, _ := base64.RawURLEncoding.DecodeString("0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FDW2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw")
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: 65537}
	if got := rsaThumbprint(pub); got != "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs" {
		t.Fatalf("thumbprint = %s", got)
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

func TestOpenAIUsesTokenSourceInPlaceOfAPIKey(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c, err := NewOpenAI(Config{BaseURL: srv.URL, TokenSource: staticToken("fresh"), MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(context.Background(), CompletionRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if got := auth.Load(); got != "Bearer fresh" {
		t.Fatalf("Authorization = %v, want Bearer fresh", got)
	}
	if _, err := NewOpenAI(Config{BaseURL: srv.URL}); err == nil {
		t.Fatal("NewOpenAI with neither APIKey nor TokenSource must fail")
	}
}
