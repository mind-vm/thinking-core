package sandbox

import (
	"context"
	"net/url"
	"strings"
)

// egress.go lets the host add request headers a script never sees.
//
// The case it exists for: an agent embedded in a product calls that product's
// API as the person who is talking to it. The person's token has to reach the
// wire, and it must not reach the script, the model, or the trace — a token a
// model can read is a token a prompt injection can exfiltrate. So the host
// puts a function on the run's context, and every request fetch() or http
// makes passes through it just before it is sent. Headers it returns replace
// any the script set under the same name, so a script cannot pre-empt the
// host's Authorization with its own.

type egressKey struct{}

// EgressHeaders returns the headers the host adds to a request for u, or nil.
type EgressHeaders func(u *url.URL) map[string]string

// WithEgressHeaders returns ctx carrying fn. The fetch pack reads it from the
// context it was built with.
func WithEgressHeaders(ctx context.Context, fn EgressHeaders) context.Context {
	return context.WithValue(ctx, egressKey{}, fn)
}

func egressHeadersFor(ctx context.Context, rawURL string) map[string]string {
	fn, _ := ctx.Value(egressKey{}).(EgressHeaders)
	if fn == nil {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	return fn(u)
}

// SameOrigin reports whether u is at origin: the same scheme, host and port,
// with the scheme's default port filled in. It is exact on purpose. A prefix
// match would let https://api.example.com.attacker.net carry a header meant
// for https://api.example.com.
func SameOrigin(u *url.URL, origin string) bool {
	o, err := url.Parse(origin)
	if err != nil || o.Host == "" || u == nil {
		return false
	}
	return strings.EqualFold(u.Scheme, o.Scheme) &&
		strings.EqualFold(u.Hostname(), o.Hostname()) &&
		portOf(u) == portOf(o)
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
