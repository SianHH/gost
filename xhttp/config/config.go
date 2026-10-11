// Package config defines the xhttp protocol configuration and the wire-level
// helpers (path normalisation, session/sequence placement, padding).
//
// It is a dependency-free port of xray-core's transport/internet/splithttp
// configuration.
package config

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	"xhttp/internal/browser"
	"xhttp/internal/randutil"
)

// Placement values describe where a piece of metadata (session id, sequence
// number, padding, uplink payload) is carried in an HTTP request.
const (
	PlacementQueryInHeader = "queryInHeader"
	PlacementCookie        = "cookie"
	PlacementHeader        = "header"
	PlacementQuery         = "query"
	PlacementPath          = "path"
	PlacementBody          = "body"
	PlacementAuto          = "auto"
)

// Range is an inclusive [From, To] integer range used by randomised settings.
type Range struct {
	From int32
	To   int32
}

// XmuxConfig controls connection multiplexing on the client side.
type XmuxConfig struct {
	MaxConcurrency   *Range
	MaxConnections   *Range
	CMaxReuseTimes   *Range
	HMaxRequestTimes *Range
	HMaxReusableSecs *Range
	HKeepAlivePeriod int64
}

// Config is the xhttp protocol configuration. It is shared by the dialer and
// the listener. A nil pointer field means "use the default".
type Config struct {
	Host    string
	Path    string
	Mode    string // "", "auto", "packet-up", "stream-up", "stream-one"
	Headers map[string]string

	XPaddingBytes     *Range
	XPaddingObfsMode  bool
	XPaddingKey       string
	XPaddingHeader    string
	XPaddingPlacement string
	XPaddingMethod    string

	NoGRPCHeader bool
	NoSSEHeader  bool

	ScMaxEachPostBytes   *Range
	ScMinPostsIntervalMs *Range
	ScMaxBufferedPosts   int64
	ScStreamUpServerSecs *Range

	Xmux *XmuxConfig

	UplinkHTTPMethod    string
	SessionPlacement    string
	SessionKey          string
	SeqPlacement        string
	SeqKey              string
	UplinkDataPlacement string
	UplinkDataKey       string
	UplinkChunkSize     *Range

	ServerMaxHeaderBytes int32
}

// Rand returns a random value within the range.
func (r Range) Rand() int32 {
	return int32(randutil.RandBetween(int64(r.From), int64(r.To)))
}

// GetNormalizedPath returns Path as an absolute path that starts and ends with
// a slash, with any query string removed.
func (c *Config) GetNormalizedPath() string {
	pathAndQuery := strings.SplitN(c.Path, "?", 2)
	path := pathAndQuery[0]

	if path == "" || path[0] != '/' {
		path = "/" + path
	}

	if path[len(path)-1] != '/' {
		path = path + "/"
	}

	return path
}

// GetNormalizedQuery returns the query string embedded in Path, if any.
func (c *Config) GetNormalizedQuery() string {
	pathAndQuery := strings.SplitN(c.Path, "?", 2)
	query := ""

	if len(pathAndQuery) > 1 {
		query = pathAndQuery[1]
	}

	return query
}

// GetRequestHeader builds the HTTP header for an outgoing request, applying the
// configured headers and, unless a User-Agent was set, browser-like defaults.
func (c *Config) GetRequestHeader() http.Header {
	header := http.Header{}
	for k, v := range c.Headers {
		header.Add(k, v)
	}
	browser.TryDefaultHeadersWith(header, "fetch")
	return header
}

// GetRequestHeaderWithPayload builds a header carrying the payload split into
// base64url chunks in "<key>-<i>" headers.
func (c *Config) GetRequestHeaderWithPayload(payload []byte) http.Header {
	header := c.GetRequestHeader()

	key := c.UplinkDataKey
	encodedData := base64.RawURLEncoding.EncodeToString(payload)

	for i := 0; len(encodedData) > 0; i++ {
		chunkSize := min(int(c.GetNormalizedUplinkChunkSize().Rand()), len(encodedData))
		chunk := encodedData[:chunkSize]
		encodedData = encodedData[chunkSize:]
		headerKey := fmt.Sprintf("%s-%d", key, i)
		header.Set(headerKey, chunk)
	}

	return header
}

// GetRequestCookiesWithPayload builds cookies carrying the payload split into
// base64url chunks named "<key>_<i>".
func (c *Config) GetRequestCookiesWithPayload(payload []byte) []*http.Cookie {
	cookies := []*http.Cookie{}

	key := c.UplinkDataKey
	encodedData := base64.RawURLEncoding.EncodeToString(payload)

	for i := 0; len(encodedData) > 0; i++ {
		chunkSize := min(int(c.GetNormalizedUplinkChunkSize().Rand()), len(encodedData))
		chunk := encodedData[:chunkSize]
		encodedData = encodedData[chunkSize:]
		cookieName := fmt.Sprintf("%s_%d", key, i)
		cookies = append(cookies, &http.Cookie{Name: cookieName, Value: chunk})
	}

	return cookies
}

// WriteResponseHeader writes the CORS-related response headers.
func (c *Config) WriteResponseHeader(writer http.ResponseWriter, requestMethod string, requestHeader http.Header) {
	if origin := requestHeader.Get("Origin"); origin == "" {
		writer.Header().Set("Access-Control-Allow-Origin", "*")
	} else {
		writer.Header().Set("Access-Control-Allow-Origin", origin)
	}

	if c.GetNormalizedSessionPlacement() == PlacementCookie ||
		c.GetNormalizedSeqPlacement() == PlacementCookie ||
		c.XPaddingPlacement == PlacementCookie ||
		c.GetNormalizedUplinkDataPlacement() == PlacementCookie {
		writer.Header().Set("Access-Control-Allow-Credentials", "true")
	}

	if requestMethod == "OPTIONS" {
		requestedMethod := requestHeader.Get("Access-Control-Request-Method")
		if requestedMethod != "" {
			writer.Header().Set("Access-Control-Allow-Methods", requestedMethod)
		} else {
			writer.Header().Set("Access-Control-Allow-Methods", "*")
		}

		requestedHeaders := requestHeader.Get("Access-Control-Request-Headers")
		if requestedHeaders == "" {
			writer.Header().Set("Access-Control-Allow-Headers", "*")
		} else {
			writer.Header().Set("Access-Control-Allow-Headers", requestedHeaders)
		}
	}
}

// GetNormalizedUplinkHTTPMethod returns the HTTP method used for uploads.
func (c *Config) GetNormalizedUplinkHTTPMethod() string {
	if c.UplinkHTTPMethod == "" {
		return "POST"
	}
	return c.UplinkHTTPMethod
}

// GetNormalizedScMaxEachPostBytes returns the maximum size of a single POST body.
func (c *Config) GetNormalizedScMaxEachPostBytes() Range {
	if c.ScMaxEachPostBytes == nil || c.ScMaxEachPostBytes.To == 0 {
		return Range{From: 1000000, To: 1000000}
	}
	return *c.ScMaxEachPostBytes
}

// GetNormalizedScMinPostsIntervalMs returns the minimum interval between POSTs.
func (c *Config) GetNormalizedScMinPostsIntervalMs() Range {
	if c.ScMinPostsIntervalMs == nil || c.ScMinPostsIntervalMs.To == 0 {
		return Range{From: 30, To: 30}
	}
	return *c.ScMinPostsIntervalMs
}

// GetNormalizedScMaxBufferedPosts returns the server-side upload queue capacity.
func (c *Config) GetNormalizedScMaxBufferedPosts() int {
	if c.ScMaxBufferedPosts == 0 {
		return 30
	}
	return int(c.ScMaxBufferedPosts)
}

// GetNormalizedScStreamUpServerSecs returns the server keep-alive interval for
// stream-up uploads.
func (c *Config) GetNormalizedScStreamUpServerSecs() Range {
	if c.ScStreamUpServerSecs == nil || c.ScStreamUpServerSecs.To == 0 {
		return Range{From: 20, To: 80}
	}
	return *c.ScStreamUpServerSecs
}

// GetNormalizedUplinkChunkSize returns the chunk size used when the uplink
// payload is carried in headers or cookies.
func (c *Config) GetNormalizedUplinkChunkSize() Range {
	if c.UplinkChunkSize == nil || c.UplinkChunkSize.To == 0 {
		switch c.UplinkDataPlacement {
		case PlacementCookie:
			return Range{From: 2 * 1024, To: 3 * 1024}
		case PlacementHeader:
			return Range{From: 3 * 1000, To: 4 * 1000}
		default:
			return c.GetNormalizedScMaxEachPostBytes()
		}
	} else if c.UplinkChunkSize.From < 64 {
		return Range{From: 64, To: max(64, c.UplinkChunkSize.To)}
	}

	return *c.UplinkChunkSize
}

// GetNormalizedServerMaxHeaderBytes returns the maximum accepted request header size.
func (c *Config) GetNormalizedServerMaxHeaderBytes() int {
	if c.ServerMaxHeaderBytes <= 0 {
		return 8192
	}
	return int(c.ServerMaxHeaderBytes)
}

// GetNormalizedSessionPlacement returns the placement used for the session id.
func (c *Config) GetNormalizedSessionPlacement() string {
	if c.SessionPlacement == "" {
		return PlacementPath
	}
	return c.SessionPlacement
}

// GetNormalizedSeqPlacement returns the placement used for the sequence number.
func (c *Config) GetNormalizedSeqPlacement() string {
	if c.SeqPlacement == "" {
		return PlacementPath
	}
	return c.SeqPlacement
}

// GetNormalizedUplinkDataPlacement returns the placement used for the uplink payload.
func (c *Config) GetNormalizedUplinkDataPlacement() string {
	if c.UplinkDataPlacement == "" {
		return PlacementBody
	}
	return c.UplinkDataPlacement
}

// GetNormalizedSessionKey returns the key/cookie/query name for the session id.
func (c *Config) GetNormalizedSessionKey() string {
	if c.SessionKey != "" {
		return c.SessionKey
	}
	switch c.GetNormalizedSessionPlacement() {
	case PlacementHeader:
		return "X-Session"
	case PlacementCookie, PlacementQuery:
		return "x_session"
	default:
		return ""
	}
}

// GetNormalizedSeqKey returns the key/cookie/query name for the sequence number.
func (c *Config) GetNormalizedSeqKey() string {
	if c.SeqKey != "" {
		return c.SeqKey
	}
	switch c.GetNormalizedSeqPlacement() {
	case PlacementHeader:
		return "X-Seq"
	case PlacementCookie, PlacementQuery:
		return "x_seq"
	default:
		return ""
	}
}

// ApplyMetaToRequest places the session id and sequence number into the request
// according to the configured placements.
func (c *Config) ApplyMetaToRequest(req *http.Request, sessionId string, seqStr string) {
	sessionPlacement := c.GetNormalizedSessionPlacement()
	seqPlacement := c.GetNormalizedSeqPlacement()
	sessionKey := c.GetNormalizedSessionKey()
	seqKey := c.GetNormalizedSeqKey()

	if sessionId != "" {
		switch sessionPlacement {
		case PlacementPath:
			req.URL.Path = appendToPath(req.URL.Path, sessionId)
		case PlacementQuery:
			q := req.URL.Query()
			q.Set(sessionKey, sessionId)
			req.URL.RawQuery = q.Encode()
		case PlacementHeader:
			req.Header.Set(sessionKey, sessionId)
		case PlacementCookie:
			req.AddCookie(&http.Cookie{Name: sessionKey, Value: sessionId})
		}
	}

	if seqStr != "" {
		switch seqPlacement {
		case PlacementPath:
			req.URL.Path = appendToPath(req.URL.Path, seqStr)
		case PlacementQuery:
			q := req.URL.Query()
			q.Set(seqKey, seqStr)
			req.URL.RawQuery = q.Encode()
		case PlacementHeader:
			req.Header.Set(seqKey, seqStr)
		case PlacementCookie:
			req.AddCookie(&http.Cookie{Name: seqKey, Value: seqStr})
		}
	}
}

// FillStreamRequest prepares a request that carries a (possibly empty) stream
// body: the downlink GET or a stream-one/stream-up request.
func (c *Config) FillStreamRequest(request *http.Request, sessionId string, seqStr string) {
	request.Header = c.GetRequestHeader()
	length := int(c.GetNormalizedXPaddingBytes().Rand())
	xconfig := XPaddingConfig{Length: length}

	if c.XPaddingObfsMode {
		xconfig.Placement = XPaddingPlacement{
			Placement: c.XPaddingPlacement,
			Key:       c.XPaddingKey,
			Header:    c.XPaddingHeader,
			RawURL:    request.URL.String(),
		}
		xconfig.Method = PaddingMethod(c.XPaddingMethod)
	} else {
		xconfig.Placement = XPaddingPlacement{
			Placement: PlacementQueryInHeader,
			Key:       "x_padding",
			Header:    "Referer",
			RawURL:    request.URL.String(),
		}
	}

	c.ApplyXPaddingToRequest(request, xconfig)
	c.ApplyMetaToRequest(request, sessionId, "")

	if request.Body != nil && !c.NoGRPCHeader { // stream-up/one
		request.Header.Set("Content-Type", "application/grpc")
	}
}

// FillPacketRequest prepares a packet-up POST carrying payload.
func (c *Config) FillPacketRequest(request *http.Request, sessionId string, seqStr string, payload []byte) error {
	dataPlacement := c.GetNormalizedUplinkDataPlacement()

	if dataPlacement == PlacementBody || dataPlacement == PlacementAuto {
		request.Header = c.GetRequestHeader()
		request.Body = io.NopCloser(&bytesReader{data: payload})
		request.ContentLength = int64(len(payload))
	} else {
		switch dataPlacement {
		case PlacementHeader:
			request.Header = c.GetRequestHeaderWithPayload(payload)
		case PlacementCookie:
			request.Header = c.GetRequestHeader()
			for _, cookie := range c.GetRequestCookiesWithPayload(payload) {
				request.AddCookie(cookie)
			}
		}
	}

	length := int(c.GetNormalizedXPaddingBytes().Rand())
	xconfig := XPaddingConfig{Length: length}

	if c.XPaddingObfsMode {
		xconfig.Placement = XPaddingPlacement{
			Placement: c.XPaddingPlacement,
			Key:       c.XPaddingKey,
			Header:    c.XPaddingHeader,
			RawURL:    request.URL.String(),
		}
		xconfig.Method = PaddingMethod(c.XPaddingMethod)
	} else {
		xconfig.Placement = XPaddingPlacement{
			Placement: PlacementQueryInHeader,
			Key:       "x_padding",
			Header:    "Referer",
			RawURL:    request.URL.String(),
		}
	}

	c.ApplyXPaddingToRequest(request, xconfig)
	c.ApplyMetaToRequest(request, sessionId, seqStr)

	return nil
}

// ExtractMetaFromRequest reads the session id and sequence number from an
// incoming request. path is the configured, normalised base path.
func (c *Config) ExtractMetaFromRequest(req *http.Request, path string) (sessionId string, seqStr string) {
	sessionPlacement := c.GetNormalizedSessionPlacement()
	seqPlacement := c.GetNormalizedSeqPlacement()
	sessionKey := c.GetNormalizedSessionKey()
	seqKey := c.GetNormalizedSeqKey()

	var subpath []string
	pathPart := 0
	if sessionPlacement == PlacementPath || seqPlacement == PlacementPath {
		rest := ""
		if len(req.URL.Path) > len(path) {
			rest = req.URL.Path[len(path):]
		}
		subpath = strings.Split(rest, "/")
	}

	switch sessionPlacement {
	case PlacementPath:
		if len(subpath) > pathPart {
			sessionId = subpath[pathPart]
			pathPart++
		}
	case PlacementQuery:
		sessionId = req.URL.Query().Get(sessionKey)
	case PlacementHeader:
		sessionId = req.Header.Get(sessionKey)
	case PlacementCookie:
		if cookie, e := req.Cookie(sessionKey); e == nil {
			sessionId = cookie.Value
		}
	}

	switch seqPlacement {
	case PlacementPath:
		if len(subpath) > pathPart {
			seqStr = subpath[pathPart]
			pathPart++
		}
	case PlacementQuery:
		seqStr = req.URL.Query().Get(seqKey)
	case PlacementHeader:
		seqStr = req.Header.Get(seqKey)
	case PlacementCookie:
		if cookie, e := req.Cookie(seqKey); e == nil {
			seqStr = cookie.Value
		}
	}

	return sessionId, seqStr
}

// GetNormalizedXmux returns the xmux configuration, applying the defaults that
// xray-core's config layer uses when the user specifies no xmux settings at all
// (maxConcurrency 1, hMaxRequestTimes 600-900, hMaxReusableSecs 1800-3000).
//
// Without these defaults a dialer would keep reusing a single HTTP transport
// (and therefore a single TCP connection) for the lifetime of the process.
func (c *Config) GetNormalizedXmux() XmuxConfig {
	var xmux XmuxConfig
	if c.Xmux != nil {
		xmux = *c.Xmux
	}

	if xmux.MaxConcurrency == nil && xmux.MaxConnections == nil && xmux.CMaxReuseTimes == nil &&
		xmux.HMaxRequestTimes == nil && xmux.HMaxReusableSecs == nil && xmux.HKeepAlivePeriod == 0 {
		xmux.MaxConcurrency = &Range{From: 1, To: 1}
		xmux.HMaxRequestTimes = &Range{From: 600, To: 900}
		xmux.HMaxReusableSecs = &Range{From: 1800, To: 3000}
	}

	return xmux
}

// GetNormalizedMaxConcurrency returns the xmux max concurrency range.
func (m *XmuxConfig) GetNormalizedMaxConcurrency() Range {
	if m.MaxConcurrency == nil {
		return Range{}
	}
	return *m.MaxConcurrency
} // GetNormalizedMaxConnections returns the xmux max connections range.
func (m *XmuxConfig) GetNormalizedMaxConnections() Range {
	if m.MaxConnections == nil {
		return Range{}
	}
	return *m.MaxConnections
}

// GetNormalizedCMaxReuseTimes returns the xmux connection reuse range.
func (m *XmuxConfig) GetNormalizedCMaxReuseTimes() Range {
	if m.CMaxReuseTimes == nil {
		return Range{}
	}
	return *m.CMaxReuseTimes
}

// GetNormalizedHMaxRequestTimes returns the xmux per-connection request range.
func (m *XmuxConfig) GetNormalizedHMaxRequestTimes() Range {
	if m.HMaxRequestTimes == nil {
		return Range{}
	}
	return *m.HMaxRequestTimes
}

// GetNormalizedHMaxReusableSecs returns the xmux per-connection reuse lifetime range.
func (m *XmuxConfig) GetNormalizedHMaxReusableSecs() Range {
	if m.HMaxReusableSecs == nil {
		return Range{}
	}
	return *m.HMaxReusableSecs
}

func appendToPath(path, value string) string {
	if strings.HasSuffix(path, "/") {
		return path + value
	}
	return path + "/" + value
}

// bytesReader is a minimal io.ReadCloser over a byte slice.
type bytesReader struct {
	data []byte
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *bytesReader) Close() error { return nil }
