package llm

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// TokenSource supplies the bearer token for one request. Implementations
// cache and refresh; the client asks on every request.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

const (
	defaultSTSURL = "https://sts.googleapis.com/v1/token"
	// assertionTTL is how long the self-signed JWT lives; it is spent at
	// once on the exchange, so it only has to outlast one round trip.
	assertionTTL = 5 * time.Minute
	// refreshEarly renews the access token this long before it expires, so
	// a request never leaves with a token that dies in flight.
	refreshEarly = time.Minute
)

// GoogleWIFConfig configures GoogleWIF. Audience, Issuer and Subject must
// match the Workload Identity Federation provider the key was uploaded to.
type GoogleWIFConfig struct {
	// Audience is the provider's resource name,
	// "//iam.googleapis.com/projects/N/locations/global/workloadIdentityPools/P/providers/X".
	Audience string
	Issuer   string
	Subject  string
	// Key signs the assertion; its public half is the provider's JWKS.
	Key *rsa.PrivateKey

	// STSURL is optional; empty uses Google's token exchange endpoint.
	STSURL string
	// HTTPClient is optional; nil falls back to a 30-second-timeout client.
	HTTPClient *http.Client
	// Now is optional; tests set it to move the clock.
	Now func() time.Time
}

// GoogleWIF is a TokenSource for Vertex AI through Workload Identity
// Federation: it signs a short RS256 JWT with the app's own key and trades
// it at Google's STS for an access token, so the app holds no Google key.
type GoogleWIF struct {
	cfg GoogleWIFConfig
	kid string

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewGoogleWIF validates cfg and returns a TokenSource over it.
func NewGoogleWIF(cfg GoogleWIFConfig) (*GoogleWIF, error) {
	switch {
	case cfg.Audience == "", cfg.Issuer == "", cfg.Subject == "":
		return nil, errors.New("llm: google wif: audience, issuer and subject are required")
	case cfg.Key == nil:
		return nil, errors.New("llm: google wif: signing key is required")
	}
	if cfg.STSURL == "" {
		cfg.STSURL = defaultSTSURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &GoogleWIF{cfg: cfg, kid: rsaThumbprint(&cfg.Key.PublicKey)}, nil
}

// GoogleWIFFromEnv reads GCP_WIF_AUDIENCE, GCP_WIF_ISSUER, GCP_WIF_SUBJECT
// and GCP_SIGNING_KEY (a PEM private key, PKCS#8 or PKCS#1). It returns
// nil and no error when GCP_WIF_AUDIENCE is unset, so an app falls back to
// its API key.
func GoogleWIFFromEnv() (*GoogleWIF, error) {
	aud := os.Getenv("GCP_WIF_AUDIENCE")
	if aud == "" {
		return nil, nil
	}
	key, err := ParseRSAPrivateKey([]byte(os.Getenv("GCP_SIGNING_KEY")))
	if err != nil {
		return nil, fmt.Errorf("llm: GCP_SIGNING_KEY: %w", err)
	}
	return NewGoogleWIF(GoogleWIFConfig{
		Audience: aud,
		Issuer:   os.Getenv("GCP_WIF_ISSUER"),
		Subject:  os.Getenv("GCP_WIF_SUBJECT"),
		Key:      key,
	})
}

// ParseRSAPrivateKey decodes a PEM RSA private key in PKCS#8 or PKCS#1 form.
// Literal "\n" sequences count as newlines, because a PEM passed through a
// .env file or a deploy tool's variable often arrives on one line.
func ParseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	pemBytes = []byte(strings.ReplaceAll(string(pemBytes), `\n`, "\n"))
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("want an RSA key, got %T", k)
	}
	return rk, nil
}

// Token returns a cached access token, exchanging a fresh assertion when
// the cached one is missing or about to expire.
func (g *GoogleWIF) Token(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.cfg.Now()
	if g.token != "" && now.Add(refreshEarly).Before(g.expiry) {
		return g.token, nil
	}
	assertion, err := g.assertion(now)
	if err != nil {
		return "", err
	}
	tok, ttl, err := g.exchange(ctx, assertion)
	if err != nil {
		return "", err
	}
	g.token, g.expiry = tok, now.Add(ttl)
	return tok, nil
}

// assertion signs the JWT the provider verifies against its uploaded JWKS.
// The kid is the RFC 7638 thumbprint, the same one authit's jwt.KeyID
// derives, so a key set published by authit matches without a lookup.
func (g *GoogleWIF) assertion(now time.Time) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": g.kid})
	claims, _ := json.Marshal(map[string]any{
		"iss": g.cfg.Issuer,
		"sub": g.cfg.Subject,
		// The JWT names the provider over https; the STS audience is the
		// bare "//iam.googleapis.com/..." resource name.
		"aud": "https:" + g.cfg.Audience,
		"iat": now.Unix(),
		"exp": now.Add(assertionTTL).Unix(),
	})
	signing := b64url(header) + "." + b64url(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.cfg.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("llm: google wif: sign: %w", err)
	}
	return signing + "." + b64url(sig), nil
}

func (g *GoogleWIF) exchange(ctx context.Context, assertion string) (string, time.Duration, error) {
	body, _ := json.Marshal(map[string]string{
		"grantType":          "urn:ietf:params:oauth:grant-type:token-exchange",
		"audience":           g.cfg.Audience,
		"scope":              "https://www.googleapis.com/auth/cloud-platform",
		"requestedTokenType": "urn:ietf:params:oauth:token-type:access_token",
		"subjectTokenType":   "urn:ietf:params:oauth:token-type:jwt",
		"subjectToken":       assertion,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.STSURL, strings.NewReader(string(body)))
	if err != nil {
		return "", 0, fmt.Errorf("llm: google wif: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("llm: google wif: exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		// STS names the refusal ("rejected by the attribute condition"),
		// which is the only clue to a misconfigured provider.
		return "", 0, fmt.Errorf("llm: google wif: exchange: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" {
		return "", 0, fmt.Errorf("llm: google wif: exchange: no access token in response")
	}
	return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
}

// rsaThumbprint is the RFC 7638 JWK thumbprint: SHA-256 over the required
// members in lexicographic order, base64url without padding.
func rsaThumbprint(k *rsa.PublicKey) string {
	canonical := fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`,
		b64url(big.NewInt(int64(k.E)).Bytes()), b64url(k.N.Bytes()))
	sum := sha256.Sum256([]byte(canonical))
	return b64url(sum[:])
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
