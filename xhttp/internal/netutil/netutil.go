// Package netutil holds small networking helpers used by the listener.
package netutil

import (
	"net"
	"net/http"
	"strings"
)

// IsValidHTTPHost reports whether the request Host matches the configured host.
// The comparison is case-insensitive and ignores an optional port.
func IsValidHTTPHost(request string, config string) bool {
	r := strings.ToLower(request)
	c := strings.ToLower(config)
	if strings.Contains(r, ":") {
		h, _, err := net.SplitHostPort(r)
		if err != nil {
			return false
		}
		return h == c
	}
	return r == c
}

// ParseXForwardedFor parses the X-Forwarded-For header and returns the
// contained IP addresses in order. Unparsable entries are skipped.
func ParseXForwardedFor(header http.Header) []net.IP {
	xff := header.Get("X-Forwarded-For")
	if xff == "" {
		return nil
	}
	list := strings.Split(xff, ",")
	ips := make([]net.IP, 0, len(list))
	for _, proxy := range list {
		proxy = strings.TrimSpace(proxy)
		if ip := net.ParseIP(proxy); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}
