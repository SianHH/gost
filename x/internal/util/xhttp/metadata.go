package xhttp

import (
	"strconv"
	"strings"

	mdata "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
)

// ConfigFromMetadata parses the XHTTP engine Config out of the component
// metadata. Every key accepts both an "xhttp.<name>" prefixed form and a bare
// "<name>" fallback form, matching the gost metadata conventions of the ws and
// pht transports.
func ConfigFromMetadata(md mdata.Metadata) Config {
	c := Config{}

	c.Host = mdutil.GetString(md, "xhttp.host", "host")
	c.Path = mdutil.GetString(md, "xhttp.path", "path")
	c.Mode = mdutil.GetString(md, "xhttp.mode", "mode")
	c.UplinkHTTPMethod = mdutil.GetString(md, "xhttp.uplinkHTTPMethod", "uplinkHTTPMethod")

	c.NoGRPCHeader = mdutil.GetBool(md, "xhttp.noGRPCHeader", "noGRPCHeader")
	c.NoSSEHeader = mdutil.GetBool(md, "xhttp.noSSEHeader", "noSSEHeader")

	c.XPaddingBytes = getRange(md, "xhttp.xPaddingBytes", "xPaddingBytes")
	c.XPaddingObfsMode = mdutil.GetBool(md, "xhttp.xPaddingObfsMode", "xPaddingObfsMode")
	c.XPaddingKey = mdutil.GetString(md, "xhttp.xPaddingKey", "xPaddingKey")
	c.XPaddingHeader = mdutil.GetString(md, "xhttp.xPaddingHeader", "xPaddingHeader")
	c.XPaddingPlacement = mdutil.GetString(md, "xhttp.xPaddingPlacement", "xPaddingPlacement")
	c.XPaddingMethod = mdutil.GetString(md, "xhttp.xPaddingMethod", "xPaddingMethod")

	c.ScMaxEachPostBytes = getRange(md, "xhttp.scMaxEachPostBytes", "scMaxEachPostBytes")
	c.ScMinPostsIntervalMs = getRange(md, "xhttp.scMinPostsIntervalMs", "scMinPostsIntervalMs")
	c.ScMaxBufferedPosts = mdutil.GetInt(md, "xhttp.scMaxBufferedPosts", "scMaxBufferedPosts")
	c.ScStreamUpServerSecs = getRange(md, "xhttp.scStreamUpServerSecs", "scStreamUpServerSecs")
	c.ServerMaxHeaderBytes = mdutil.GetInt(md, "xhttp.serverMaxHeaderBytes", "serverMaxHeaderBytes")

	c.SessionPlacement = mdutil.GetString(md, "xhttp.sessionPlacement", "sessionPlacement")
	c.SessionKey = mdutil.GetString(md, "xhttp.sessionKey", "sessionKey")
	c.SeqPlacement = mdutil.GetString(md, "xhttp.seqPlacement", "seqPlacement")
	c.SeqKey = mdutil.GetString(md, "xhttp.seqKey", "seqKey")

	c.UplinkDataPlacement = mdutil.GetString(md, "xhttp.uplinkDataPlacement", "uplinkDataPlacement")
	c.UplinkDataKey = mdutil.GetString(md, "xhttp.uplinkDataKey", "uplinkDataKey")
	c.UplinkChunkSize = getRange(md, "xhttp.uplinkChunkSize", "uplinkChunkSize")

	if m := mdutil.GetStringMapString(md, "xhttp.headers", "headers"); len(m) > 0 {
		c.Headers = m
	}

	return c
}

// getRange reads a RangeConfig from the metadata. Accepted forms are a single
// number ("100", treated as From == To) or "from-to" / "from..to" range
// strings. Unknown/empty values yield the zero RangeConfig, which makes the
// GetNormalized* accessors fall back to the xray defaults.
func getRange(md mdata.Metadata, keys ...string) (r RangeConfig) {
	s := mdutil.GetString(md, keys...)
	if s == "" {
		return
	}
	return ParseRange(s)
}

// ParseRange parses "N", "N-M" or "N..M" into a RangeConfig. It returns the
// zero value when the input is invalid.
func ParseRange(s string) (r RangeConfig) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	sep := "-"
	if strings.Contains(s, "..") {
		sep = ".."
	} else if !strings.Contains(s, sep) {
		sep = ""
	}
	if sep == "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return RangeConfig{From: n, To: n}
		}
		return
	}
	parts := strings.SplitN(s, sep, 2)
	from, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	to, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return
	}
	if from < 0 || to < 0 {
		return
	}
	if from > to {
		from, to = to, from
	}
	return RangeConfig{From: from, To: to}
}
