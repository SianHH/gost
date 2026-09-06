// Package xhttp implements the XHTTP (a.k.a. splithttp) transport protocol.
//
// This is a clean-room port of the XHTTP transport from xray-core's
// transport/internet/splithttp package. It deliberately does not import any
// xray-core package: only the HTTP protocol behaviour (session management,
// upload chunking, x-padding validation, ...) is reproduced so that a gost
// xhttp listener/dialer can interoperate with Xray (and vice versa).
//
// The package is shared by the gost dialer (x/dialer/xhttp) and listener
// (x/listener/xhttp) implementations.
package xhttp

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
)

// Placement values, kept identical to xray splithttp.
const (
	PlacementQueryInHeader = "queryInHeader"
	PlacementCookie        = "cookie"
	PlacementHeader        = "header"
	PlacementQuery         = "query"
	PlacementPath          = "path"
	PlacementBody          = "body"
	PlacementAuto          = "auto"
)

// RangeConfig describes a uniformly-random integer range [From, To].
type RangeConfig struct {
	From int
	To   int
}

// rand returns a uniformly random value inside [From, To]. When To <= From
// it returns From. Uses the math/rand/v2 top-level generator which is safe for
// concurrent use and automatically seeded.
func (c RangeConfig) rand() int {
	if c.To <= c.From {
		return c.From
	}
	return c.From + rand.IntN(c.To-c.From+1)
}

// Config mirrors the configurable options of the XHTTP protocol. All options
// are parsed from the component metadata by the dialer/listener packages; the
// zero value always maps onto the Xray defaults through the GetNormalized*
// accessors below.
type Config struct {
	Host    string
	Path    string
	Mode    string
	Headers map[string]string

	// UplinkHTTPMethod is the method used for upload requests
	// (default "POST").
	UplinkHTTPMethod string

	// NoGRPCHeader disables the "Content-Type: application/grpc" header on
	// stream upload requests.
	NoGRPCHeader bool

	// NoSSEHeader disables the "Content-Type: text/event-stream" header on
	// the server download responses.
	NoSSEHeader bool

	// XPaddingBytes is the length (in bytes) of the generated padding.
	XPaddingBytes RangeConfig

	// XPaddingObfsMode switches the padding from the default "Referer query"
	// trick to a customised placement/key/header/method.
	XPaddingObfsMode  bool
	XPaddingKey       string
	XPaddingHeader    string
	XPaddingPlacement string
	XPaddingMethod    string

	// sc* server-side / client-side chunked-upload tuning knobs.
	ScMaxEachPostBytes   RangeConfig
	ScMinPostsIntervalMs RangeConfig
	ScMaxBufferedPosts   int
	ScStreamUpServerSecs RangeConfig
	ServerMaxHeaderBytes int

	// Where session id and upload sequence number are carried.
	SessionPlacement string
	SessionKey       string
	SeqPlacement     string
	SeqKey           string

	// Where packet-up payloads are carried.
	UplinkDataPlacement string
	UplinkDataKey       string
	UplinkChunkSize     RangeConfig
}

// GetNormalizedPath returns the configured path, normalised to start and end
// with "/". Keeps the query string (if any) untouched for URL building.
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

// GetNormalizedQuery returns the query part of the configured path (the part
// after "?", if present). The historical xray "x_version" query is not added.
func (c *Config) GetNormalizedQuery() string {
	pathAndQuery := strings.SplitN(c.Path, "?", 2)
	query := ""
	if len(pathAndQuery) > 1 {
		query = pathAndQuery[1]
	}
	return query
}

// GetRequestHeader builds the base request header for upload/download
// requests: the configured custom headers plus default browser-like headers.
func (c *Config) GetRequestHeader() http.Header {
	header := http.Header{}
	for k, v := range c.Headers {
		header.Add(k, v)
	}
	applyDefaultFetchHeaders(header)
	return header
}

// GetRequestHeaderWithPayload encodes payload as base64 (RawURLEncoding) and
// spreads it over "<key>-0", "<key>-1", ... request headers. It is used when
// UplinkDataPlacement is PlacementHeader.
func (c *Config) GetRequestHeaderWithPayload(payload []byte) http.Header {
	header := c.GetRequestHeader()

	key := c.UplinkDataKey
	encodedData := base64.RawURLEncoding.EncodeToString(payload)

	for i := 0; len(encodedData) > 0; i++ {
		chunkSize := min(int(c.GetNormalizedUplinkChunkSize().rand()), len(encodedData))
		chunk := encodedData[:chunkSize]
		encodedData = encodedData[chunkSize:]
		headerKey := fmt.Sprintf("%s-%d", key, i)
		header.Set(headerKey, chunk)
	}

	return header
}

// GetRequestCookiesWithPayload encodes payload as base64 (RawURLEncoding) and
// spreads it over "<key>_0", "<key>_1", ... cookies. Used when
// UplinkDataPlacement is PlacementCookie.
func (c *Config) GetRequestCookiesWithPayload(payload []byte) []*http.Cookie {
	var cookies []*http.Cookie

	key := c.UplinkDataKey
	encodedData := base64.RawURLEncoding.EncodeToString(payload)

	for i := 0; len(encodedData) > 0; i++ {
		chunkSize := min(int(c.GetNormalizedUplinkChunkSize().rand()), len(encodedData))
		chunk := encodedData[:chunkSize]
		encodedData = encodedData[chunkSize:]
		cookieName := fmt.Sprintf("%s_%d", key, i)
		cookies = append(cookies, &http.Cookie{Name: cookieName, Value: chunk})
	}

	return cookies
}

// WriteResponseHeader writes CORS headers (used for browser based dialers)
// and handles preflight OPTIONS requests.
func (c *Config) WriteResponseHeader(writer http.ResponseWriter, requestMethod string, requestHeader http.Header) {
	if origin := requestHeader.Get("Origin"); origin == "" {
		writer.Header().Set("Access-Control-Allow-Origin", "*")
	} else {
		// Chrome refuses the wildcard origin when credentials are involved.
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

// GetNormalizedUplinkHTTPMethod returns the HTTP method used for upload
// requests (default "POST").
func (c *Config) GetNormalizedUplinkHTTPMethod() string {
	if c.UplinkHTTPMethod == "" {
		return "POST"
	}
	return c.UplinkHTTPMethod
}

func (c *Config) GetNormalizedScMaxEachPostBytes() RangeConfig {
	if c.ScMaxEachPostBytes.To == 0 {
		return RangeConfig{From: 1000000, To: 1000000}
	}
	return c.ScMaxEachPostBytes
}

func (c *Config) GetNormalizedScMinPostsIntervalMs() RangeConfig {
	if c.ScMinPostsIntervalMs.To == 0 {
		return RangeConfig{From: 30, To: 30}
	}
	return c.ScMinPostsIntervalMs
}

func (c *Config) GetNormalizedScMaxBufferedPosts() int {
	if c.ScMaxBufferedPosts <= 0 {
		return 30
	}
	return c.ScMaxBufferedPosts
}

func (c *Config) GetNormalizedScStreamUpServerSecs() RangeConfig {
	if c.ScStreamUpServerSecs.To == 0 {
		return RangeConfig{From: 20, To: 80}
	}
	return c.ScStreamUpServerSecs
}

func (c *Config) GetNormalizedUplinkChunkSize() RangeConfig {
	if c.UplinkChunkSize.To == 0 {
		switch c.GetNormalizedUplinkDataPlacement() {
		case PlacementCookie:
			return RangeConfig{From: 2 * 1024, To: 3 * 1024}
		case PlacementHeader:
			return RangeConfig{From: 3 * 1000, To: 4 * 1000}
		default:
			return c.GetNormalizedScMaxEachPostBytes()
		}
	}
	if c.UplinkChunkSize.From < 64 {
		return RangeConfig{From: 64, To: max(64, c.UplinkChunkSize.To)}
	}
	return c.UplinkChunkSize
}

func (c *Config) GetNormalizedServerMaxHeaderBytes() int {
	if c.ServerMaxHeaderBytes <= 0 {
		return 8192
	}
	return c.ServerMaxHeaderBytes
}

func (c *Config) GetNormalizedSessionPlacement() string {
	if c.SessionPlacement == "" {
		return PlacementPath
	}
	return c.SessionPlacement
}

func (c *Config) GetNormalizedSeqPlacement() string {
	if c.SeqPlacement == "" {
		return PlacementPath
	}
	return c.SeqPlacement
}

func (c *Config) GetNormalizedUplinkDataPlacement() string {
	if c.UplinkDataPlacement == "" {
		return PlacementBody
	}
	return c.UplinkDataPlacement
}

// GetNormalizedSessionKey returns the default header/query/cookie key for the
// session id, depending on the configured placement.
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

// ApplyMetaToRequest attaches sessionId and seqStr to the request according to
// their configured placements.
func (c *Config) ApplyMetaToRequest(req *http.Request, sessionID string, seqStr string) {
	sessionPlacement := c.GetNormalizedSessionPlacement()
	seqPlacement := c.GetNormalizedSeqPlacement()
	sessionKey := c.GetNormalizedSessionKey()
	seqKey := c.GetNormalizedSeqKey()

	if sessionID != "" {
		switch sessionPlacement {
		case PlacementPath:
			req.URL.Path = appendToPath(req.URL.Path, sessionID)
		case PlacementQuery:
			q := req.URL.Query()
			q.Set(sessionKey, sessionID)
			req.URL.RawQuery = q.Encode()
		case PlacementHeader:
			req.Header.Set(sessionKey, sessionID)
		case PlacementCookie:
			req.AddCookie(&http.Cookie{Name: sessionKey, Value: sessionID})
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

// FillStreamRequest configures a stream upload/download request (stream-one,
// stream-up and stream-down).
func (c *Config) FillStreamRequest(request *http.Request, sessionID string, seqStr string) {
	request.Header = c.GetRequestHeader()
	length := int(c.GetNormalizedXPaddingBytes().rand())
	config := XPaddingConfig{Length: length}

	if c.XPaddingObfsMode {
		config.Placement = XPaddingPlacement{
			Placement: c.XPaddingPlacement,
			Key:       c.XPaddingKey,
			Header:    c.XPaddingHeader,
			RawURL:    request.URL.String(),
		}
		config.Method = PaddingMethod(c.XPaddingMethod)
	} else {
		config.Placement = XPaddingPlacement{
			Placement: PlacementQueryInHeader,
			Key:       "x_padding",
			Header:    "Referer",
			RawURL:    request.URL.String(),
		}
	}

	c.ApplyXPaddingToRequest(request, config)
	c.ApplyMetaToRequest(request, sessionID, "")

	if request.Body != nil && !c.NoGRPCHeader { // stream-up/one
		request.Header.Set("Content-Type", "application/grpc")
	}
}

// FillPacketRequest configures a packet-up upload request carrying payload.
func (c *Config) FillPacketRequest(request *http.Request, sessionID string, seqStr string, payload []byte) {
	dataPlacement := c.GetNormalizedUplinkDataPlacement()

	if dataPlacement == PlacementBody || dataPlacement == PlacementAuto {
		request.Header = c.GetRequestHeader()
		request.Body = io.NopCloser(bytes.NewReader(payload))
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

	length := int(c.GetNormalizedXPaddingBytes().rand())
	config := XPaddingConfig{Length: length}

	if c.XPaddingObfsMode {
		config.Placement = XPaddingPlacement{
			Placement: c.XPaddingPlacement,
			Key:       c.XPaddingKey,
			Header:    c.XPaddingHeader,
			RawURL:    request.URL.String(),
		}
		config.Method = PaddingMethod(c.XPaddingMethod)
	} else {
		config.Placement = XPaddingPlacement{
			Placement: PlacementQueryInHeader,
			Key:       "x_padding",
			Header:    "Referer",
			RawURL:    request.URL.String(),
		}
	}

	c.ApplyXPaddingToRequest(request, config)
	c.ApplyMetaToRequest(request, sessionID, seqStr)
}

// ExtractMetaFromRequest pulls the sessionId and seqStr out of the request
// according to their configured placements. path is the normalized base path.
func (c *Config) ExtractMetaFromRequest(req *http.Request, path string) (sessionID string, seqStr string) {
	sessionPlacement := c.GetNormalizedSessionPlacement()
	seqPlacement := c.GetNormalizedSeqPlacement()
	sessionKey := c.GetNormalizedSessionKey()
	seqKey := c.GetNormalizedSeqKey()

	var subpath []string
	pathPart := 0
	if sessionPlacement == PlacementPath || seqPlacement == PlacementPath {
		subpath = strings.Split(req.URL.Path[len(path):], "/")
	}

	switch sessionPlacement {
	case PlacementPath:
		if len(subpath) > pathPart {
			sessionID = subpath[pathPart]
			pathPart++
		}
	case PlacementQuery:
		sessionID = req.URL.Query().Get(sessionKey)
	case PlacementHeader:
		sessionID = req.Header.Get(sessionKey)
	case PlacementCookie:
		if cookie, e := req.Cookie(sessionKey); e == nil {
			sessionID = cookie.Value
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

	return sessionID, seqStr
}

// HasPaddingError reports whether the request carries an invalid x-padding.
// It returns the extracted padding value and its placement for logging.
func (c *Config) ValidateRequestPadding(req *http.Request) (bool, string, string) {
	validRange := c.GetNormalizedXPaddingBytes()
	paddingValue, paddingPlacement := c.ExtractXPaddingFromRequest(req, c.XPaddingObfsMode)
	return c.IsPaddingValid(paddingValue, validRange.From, validRange.To, PaddingMethod(c.XPaddingMethod)), paddingValue, paddingPlacement
}

func appendToPath(path, value string) string {
	if strings.HasSuffix(path, "/") {
		return path + value
	}
	return path + "/" + value
}

// defaultUserAgent mirrors the xray transport default (Chrome) user agent.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// applyDefaultFetchHeaders reproduces xray's TryDefaultHeadersWith(header,
// "fetch") for a Chrome-like user agent. Only fills in values that are not
// already present, mirroring xray's behaviour closely enough for interop.
func applyDefaultFetchHeaders(header http.Header) {
	if len(header.Values("User-Agent")) > 0 {
		// When the user explicitly configured a User-Agent we leave the
		// header block untouched (except for the special "chrome"/"firefox"/
		// "edge"/"golang" masquerade keywords handled by xray, which make no
		// sense outside of xray's uTLS machinery).
		return
	}

	if header.Get("User-Agent") == "" {
		header.Set("User-Agent", defaultUserAgent)
	}
	if header.Get("Accept-Language") == "" {
		header.Set("Accept-Language", "en-US,en;q=0.9")
	}
	if header.Get("Sec-Fetch-Mode") == "" {
		header.Set("Sec-Fetch-Mode", "cors")
	}
	if header.Get("Sec-Fetch-Dest") == "" {
		header.Set("Sec-Fetch-Dest", "empty")
	}
	if header.Get("Sec-Fetch-Site") == "" {
		header.Set("Sec-Fetch-Site", "same-origin")
	}
	if header.Get("Priority") == "" {
		header.Set("Priority", "u=1, i")
	}
	if header.Get("Cache-Control") == "" {
		header.Set("Cache-Control", "no-cache")
	}
	if header.Get("Pragma") == "" {
		header.Set("Pragma", "no-cache")
	}
	if header.Get("Accept") == "" {
		header.Set("Accept", "*/*")
	}
}
