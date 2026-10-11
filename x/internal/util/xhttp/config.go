// Package xhttp provides helpers shared by the GOST xhttp listener and dialer.
// It translates GOST metadata into the xhttp library's protocol configuration
// so that both sides agree on the metadata key names.
package xhttp

import (
	"strconv"
	"strings"
	"time"

	md "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
	xhconfig "xhttp/config"
)

// DefaultPath is the xhttp request base path used when none is configured.
const DefaultPath = "/"

// ParseConfig builds the xhttp protocol configuration from metadata. Keys are
// tried both with and without the "xhttp." prefix, so options can be set from a
// service/node metadata block or from a top-level listener/node option.
func ParseConfig(md md.Metadata) *xhconfig.Config {
	cfg := &xhconfig.Config{
		Host: mdutil.GetString(md, "xhttp.host", "host"),
		Path: mdutil.GetString(md, "xhttp.path", "path"),
		Mode: mdutil.GetString(md, "xhttp.mode", "mode"),
	}
	if cfg.Path == "" {
		cfg.Path = DefaultPath
	}
	if m := mdutil.GetStringMapString(md, "xhttp.header", "header"); len(m) > 0 {
		cfg.Headers = m
	}

	cfg.XPaddingBytes = ParseRange(md, "xhttp.xpaddingBytes", "xpaddingBytes")
	cfg.XPaddingObfsMode = mdutil.GetBool(md, "xhttp.xpaddingObfsMode", "xpaddingObfsMode")
	cfg.XPaddingKey = mdutil.GetString(md, "xhttp.xpaddingKey", "xpaddingKey")
	cfg.XPaddingHeader = mdutil.GetString(md, "xhttp.xpaddingHeader", "xpaddingHeader")
	cfg.XPaddingPlacement = mdutil.GetString(md, "xhttp.xpaddingPlacement", "xpaddingPlacement")
	cfg.XPaddingMethod = mdutil.GetString(md, "xhttp.xpaddingMethod", "xpaddingMethod")

	cfg.NoGRPCHeader = mdutil.GetBool(md, "xhttp.noGRPCHeader", "noGRPCHeader")
	cfg.NoSSEHeader = mdutil.GetBool(md, "xhttp.noSSEHeader", "noSSEHeader")

	cfg.ScMaxEachPostBytes = ParseRange(md, "xhttp.scMaxEachPostBytes", "scMaxEachPostBytes")
	cfg.ScMinPostsIntervalMs = ParseRange(md, "xhttp.scMinPostsIntervalMs", "scMinPostsIntervalMs")
	cfg.ScMaxBufferedPosts = int64(mdutil.GetInt(md, "xhttp.scMaxBufferedPosts", "scMaxBufferedPosts"))
	cfg.ScStreamUpServerSecs = ParseRange(md, "xhttp.scStreamUpServerSecs", "scStreamUpServerSecs")

	cfg.UplinkHTTPMethod = mdutil.GetString(md, "xhttp.uplinkHTTPMethod", "uplinkHTTPMethod")
	cfg.SessionPlacement = mdutil.GetString(md, "xhttp.sessionPlacement", "sessionPlacement")
	cfg.SessionKey = mdutil.GetString(md, "xhttp.sessionKey", "sessionKey")
	cfg.SeqPlacement = mdutil.GetString(md, "xhttp.seqPlacement", "seqPlacement")
	cfg.SeqKey = mdutil.GetString(md, "xhttp.seqKey", "seqKey")
	cfg.UplinkDataPlacement = mdutil.GetString(md, "xhttp.uplinkDataPlacement", "uplinkDataPlacement")
	cfg.UplinkDataKey = mdutil.GetString(md, "xhttp.uplinkDataKey", "uplinkDataKey")
	cfg.UplinkChunkSize = ParseRange(md, "xhttp.uplinkChunkSize", "uplinkChunkSize")

	cfg.ServerMaxHeaderBytes = int32(mdutil.GetInt(md, "xhttp.serverMaxHeaderBytes", "serverMaxHeaderBytes"))

	cfg.Xmux = ParseXmux(md)

	return cfg
}

// ParseXmux builds the client-side multiplexing configuration. A nil result
// lets the xhttp library apply its own defaults, which is required for
// connection reuse: an explicitly empty XmuxConfig would disable it.
func ParseXmux(md md.Metadata) *xhconfig.XmuxConfig {
	xmux := &xhconfig.XmuxConfig{
		MaxConcurrency:   ParseRange(md, "xhttp.xmux.maxConcurrency", "xmux.maxConcurrency"),
		MaxConnections:   ParseRange(md, "xhttp.xmux.maxConnections", "xmux.maxConnections"),
		CMaxReuseTimes:   ParseRange(md, "xhttp.xmux.cMaxReuseTimes", "xmux.cMaxReuseTimes"),
		HMaxRequestTimes: ParseRange(md, "xhttp.xmux.hMaxRequestTimes", "xmux.hMaxRequestTimes"),
		HMaxReusableSecs: ParseRange(md, "xhttp.xmux.hMaxReusableSecs", "xmux.hMaxReusableSecs"),
		HKeepAlivePeriod: int64(mdutil.GetDuration(md, "xhttp.xmux.hKeepAlivePeriod", "xmux.hKeepAlivePeriod") / time.Second),
	}
	if xmux.MaxConcurrency == nil && xmux.MaxConnections == nil && xmux.CMaxReuseTimes == nil &&
		xmux.HMaxRequestTimes == nil && xmux.HMaxReusableSecs == nil && xmux.HKeepAlivePeriod == 0 {
		return nil
	}
	return xmux
}

// ParseRange parses an inclusive integer range. Accepted forms are:
//
//	1024        -> {1024, 1024}  (also when the metadata value is an int)
//	"100-200"   -> {100, 200}
//
// It returns nil when the value is empty or invalid, in which case the xhttp
// library default applies.
func ParseRange(md md.Metadata, keys ...string) *xhconfig.Range {
	s := strings.TrimSpace(mdutil.GetString(md, keys...))
	if s == "" {
		return nil
	}

	if i := strings.IndexByte(s, '-'); i > 0 {
		from, err1 := strconv.Atoi(strings.TrimSpace(s[:i]))
		to, err2 := strconv.Atoi(strings.TrimSpace(s[i+1:]))
		if err1 != nil || err2 != nil {
			return nil
		}
		return &xhconfig.Range{From: int32(from), To: int32(to)}
	}

	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &xhconfig.Range{From: int32(n), To: int32(n)}
}
