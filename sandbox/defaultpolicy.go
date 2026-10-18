package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
)

// AllowAll is the explicit fail-open PolicyChecker. Every check passes
// with no config. Use it in tests and in the rare deployment that
// consciously wants ungated side effects — passing it is greppable and
// reviewable, unlike a nil-means-open default.
var AllowAll PolicyChecker = allowAllPolicy{}

type allowAllPolicy struct{}

func (allowAllPolicy) Check(context.Context, string, map[string]any) (PolicyCheckResult, error) {
	return PolicyCheckResult{Allowed: true}, nil
}

// sideEffectTools are the primitives DefaultPolicy denies unless
// explicitly granted: they move data out of the run (sendEmail), pull
// sensitive material into it (secret), or change something on disk that
// will still be changed after the run ends (writeFile, editFile).
// fetch/http are handled by the URL rules instead.
//
// Workspace READS are deliberately absent. A read confined to the
// workspace root cannot reach anything the user did not point the agent
// at, and gating it would mean a permission prompt per file — which in
// practice trains people to approve without reading. The mutations are
// where the consequence is.
var sideEffectTools = map[string]bool{
	"sendEmail": true,
	"secret":    true,
	"writeFile": true,
	"editFile":  true,
	"exec":      true,
}

// DefaultPolicy is the conservative PolicyChecker agentloop's Loop
// installs when Config.Policy is nil: side-effecting primitives are
// denied unless explicitly granted, and fetch/http may not reach private
// address ranges. The zero value is the safe default; a caller states
// its grants:
//
//	sandbox.DefaultPolicy{
//	    AllowTools:       []string{"sendEmail"},
//	    URLAllowPrefixes: []string{"https://api.example.com/"},
//	}
//
// A caller that truly wants fail-open passes AllowAll.
type DefaultPolicy struct {
	// AllowTools grants side-effecting tool names ("sendEmail",
	// "secret"). Empty means all of them are denied.
	AllowTools []string

	// URLAllowPrefixes is the outbound allowlist for fetch/http and for
	// browser navigation, matched by prefix (same semantics as
	// URLAllowlistPolicy). Empty means public URLs are allowed (the
	// fetch pack's per-domain user approval still applies); non-empty
	// means only matching URLs are. An explicit prefix also overrides
	// the private-network denial — listing an internal URL is a
	// conscious decision.
	URLAllowPrefixes []string

	// AllowCommands grants exec() for these command basenames, e.g.
	// {"go", "git"}. It is checked before AllowTools, so a caller can
	// permit a few known tools without granting exec() outright — which
	// is the difference between "this agent may run the build" and "this
	// agent may run anything".
	//
	// Matching is on the basename alone: listing "git" allows any git
	// invocation, with any arguments. That is a real grant, not a narrow
	// one, and it is worth being deliberate about which names go here.
	AllowCommands []string

	// AllowPrivateNetworks disables the deny-by-default for loopback,
	// RFC-1918/ULA, link-local, and *.localhost/*.local/*.internal
	// targets. Leave false outside local development.
	AllowPrivateNetworks bool
}

// Check implements PolicyChecker.
func (p DefaultPolicy) Check(_ context.Context, toolName string, args map[string]any) (PolicyCheckResult, error) {
	switch toolName {
	case "fetch", "http", "_http", "browser":
		return p.checkURL(args)
	case "exec":
		if command, _ := args["command"].(string); command != "" &&
			slices.Contains(p.AllowCommands, command) {
			return PolicyCheckResult{Allowed: true}, nil
		}
	}
	if sideEffectTools[toolName] && !slices.Contains(p.AllowTools, toolName) {
		return PolicyCheckResult{Allowed: false, Reason: p.refusal(toolName, args)}, nil
	}
	return PolicyCheckResult{Allowed: true}, nil
}

// refusal is the Reason for a denied tool. It is read by the model, not
// by whoever configured the policy: the script that made the call gets
// it as the error it throws, and nothing else reaches the model about
// why. So it says what was refused and what the model can do instead,
// and never what an operator could change — the model cannot grant
// itself a tool, and being told how only earns a retry. labs/codeloop
// measured a developer-facing refusal costing a turn and teaching
// nothing, and a model that then claimed to have done what was refused.
func (p DefaultPolicy) refusal(toolName string, args map[string]any) string {
	switch toolName {
	case "writeFile", "editFile":
		return toolName + " is not permitted in this run, so no file was changed. " +
			"Read and search as much as you need, then report the exact change you would make — the file, and the text before and after — instead of making it."
	case "exec":
		command, _ := args["command"].(string)
		if len(p.AllowCommands) > 0 {
			return fmt.Sprintf("this run may not run %q. The commands it may run are: %s. "+
				"Use one of those or the other functions for the rest, and say plainly what you could not do.",
				command, strings.Join(p.AllowCommands, ", "))
		}
		return "exec is not permitted in this run, so no command can be run. " +
			"Do the work with the other functions, and say plainly what you could not verify."
	case "sendEmail":
		return "sendEmail is not permitted in this run, so nothing was sent. Put what you would have sent in your answer instead."
	case "secret":
		return "secret is not permitted in this run. Finish without the value, and say which one you needed."
	}
	return toolName + " is not permitted in this run. Do the task with the functions that are, and say plainly what you could not do."
}

func (p DefaultPolicy) checkURL(args map[string]any) (PolicyCheckResult, error) {
	rawURL, _ := args["url"].(string)
	if rawURL == "" {
		// Config probe (fetchWhitelist(), pack warm-up): surface the
		// allowlist for the pack to consult per-request.
		cfg, _ := json.Marshal(map[string]any{"whitelist": p.URLAllowPrefixes})
		return PolicyCheckResult{Allowed: true, ConfigJSON: cfg}, nil
	}
	for _, prefix := range p.URLAllowPrefixes {
		if strings.HasPrefix(rawURL, prefix) {
			return PolicyCheckResult{Allowed: true}, nil
		}
	}
	if !p.AllowPrivateNetworks && isPrivateHostURL(rawURL) {
		return PolicyCheckResult{
			Allowed: false,
			Reason:  fmt.Sprintf("this run may not reach %q: private and internal addresses are closed to it. Only public URLs can be fetched.", rawURL),
		}, nil
	}
	if len(p.URLAllowPrefixes) > 0 {
		return PolicyCheckResult{
			Allowed: false,
			Reason: fmt.Sprintf("this run may not reach %q. The URLs it may reach start with: %s.",
				rawURL, strings.Join(p.URLAllowPrefixes, ", ")),
		}, nil
	}
	return PolicyCheckResult{Allowed: true}, nil
}

// isPrivateHostURL reports whether rawURL's host is a non-public
// target: an IP literal in one of the nonPublic ranges, or a
// conventional internal hostname. The check is syntactic (IP literals
// and hostname suffixes) — it does not resolve DNS, so a public name
// pointing at a private address (DNS rebinding) is out of scope here
// and belongs to the HTTP client layer. An unparseable URL counts as
// private: conservative default.
func isPrivateHostURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return true
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	// An IPv4-mapped IPv6 address is judged as the IPv4 address it
	// carries, so ::ffff:127.0.0.1 is loopback like 127.0.0.1.
	ip = ip.WithZone("").Unmap()
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// nonPublic is every range a run is never let reach: the IANA
// special-purpose registries' non-global entries, and the prefixes that
// embed an IPv4 address a gateway would reach on the caller's behalf.
// net.IP's IsPrivate knows only RFC 1918 and ULA, which leaves shared
// address space — what a tailnet hands out — open.
//
// The same list guards apps/weekly's calendar fetch
// (internal/kitchen/calendar_sync.go); agentloop is its own module and
// cannot import it, so a change to one belongs in the other.
var nonPublic = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // this network
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // shared address space: CGNAT, Tailscale
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local, the cloud metadata service
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, and the limited broadcast address

		"::/96",          // unspecified, loopback, the deprecated IPv4-compatible form
		"64:ff9b::/96",   // NAT64, well-known prefix
		"64:ff9b:1::/48", // NAT64, local-use
		"100::/64",       // discard-only
		"2001::/23",      // IETF protocol assignments, Teredo among them
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4
		"3fff::/20",      // documentation
		"fc00::/7",       // unique local
		"fe80::/10",      // link-local
		"fec0::/10",      // site-local, deprecated
		"ff00::/8",       // multicast
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()
