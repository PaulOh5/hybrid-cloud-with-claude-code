// Package router parses SSH direct-tcpip target hostnames into hybrid-cloud
// subdomain prefixes. The proxy accepts only targets under the configured
// zone (e.g. "hybrid-cloud.com") and rejects everything else so the proxy
// cannot be abused as an open SSH relay.
package router

import (
	"errors"
	"strings"
)

// ErrWrongZone is returned when the target host is not a subdomain of the
// configured zone.
var ErrWrongZone = errors.New("router: target host not in zone")

// ErrBadSubdomain is returned when the target has no subdomain label or the
// label is not the expected hybrid-cloud prefix shape.
var ErrBadSubdomain = errors.New("router: malformed subdomain")

// Route is the routing decision produced from an SSH direct-tcpip target.
type Route struct {
	// Prefix is the leftmost label of the target hostname — the hybrid-cloud
	// instance identifier. Matches the first 8 characters of an instance
	// UUID.
	Prefix string
	// Zone is the remainder of the target hostname (for logging / telemetry).
	Zone string
}

// ExtractRoute pulls the subdomain prefix from a target hostname like
// "abc12345.hybrid-cloud.com" when zone is "hybrid-cloud.com". Returns
// ErrWrongZone or ErrBadSubdomain for inputs the proxy should refuse.
func ExtractRoute(target, zone string) (Route, error) {
	target = strings.ToLower(strings.TrimSuffix(target, "."))
	zone = strings.ToLower(strings.TrimPrefix(zone, "."))
	if zone == "" {
		return Route{}, errors.New("router: zone is empty")
	}

	suffix := "." + zone
	if !strings.HasSuffix(target, suffix) {
		return Route{}, ErrWrongZone
	}
	prefix := strings.TrimSuffix(target, suffix)
	if prefix == "" || strings.Contains(prefix, ".") {
		// Multi-label prefixes (e.g. "foo.bar.hybrid-cloud.com") are not
		// supported — the instance id occupies exactly one label.
		return Route{}, ErrBadSubdomain
	}
	return Route{Prefix: prefix, Zone: zone}, nil
}
