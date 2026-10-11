package config

import (
	"math"
	"net/http"
	"net/url"
	"strings"

	"xhttp/internal/hpack"
	"xhttp/internal/randutil"
)

// PaddingMethod selects how the padding value is generated.
type PaddingMethod string

// Supported padding methods.
const (
	PaddingMethodRepeatX  PaddingMethod = "repeat-x"
	PaddingMethodTokenish PaddingMethod = "tokenish"
)

const charsetBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// avgHuffmanBytesPerCharBase62: Huffman encoding gives ~20% size reduction for
// base62 sequences.
const avgHuffmanBytesPerCharBase62 = 0.8

const validationTolerance = 2

// XPaddingPlacement describes where a padding value is placed.
type XPaddingPlacement struct {
	Placement string
	Key       string
	Header    string
	RawURL    string
}

// XPaddingConfig describes a padding value to generate and place.
type XPaddingConfig struct {
	Length    int
	Placement XPaddingPlacement
	Method    PaddingMethod
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// GenerateTokenishPaddingBase62 returns a base62 string whose Huffman encoded
// length is close to targetHuffmanBytes.
func GenerateTokenishPaddingBase62(targetHuffmanBytes int) string {
	n := int(math.Ceil(float64(targetHuffmanBytes) / avgHuffmanBytesPerCharBase62))
	if n < 1 {
		n = 1
	}

	randBase62Str, ok := randutil.RandStringFromCharset(n, charsetBase62)
	if !ok {
		return ""
	}

	const maxIter = 150
	adjustChar := byte('X')

	for iter := 0; iter < maxIter; iter++ {
		currentLength := int(hpack.HuffmanEncodeLength(randBase62Str))
		diff := currentLength - targetHuffmanBytes

		if absInt(diff) <= validationTolerance {
			return randBase62Str
		}

		if diff < 0 {
			randBase62Str += string(adjustChar)
			if adjustChar == 'X' {
				adjustChar = 'Z'
			} else {
				adjustChar = 'X'
			}
		} else {
			if len(randBase62Str) <= 1 {
				return randBase62Str
			}
			randBase62Str = randBase62Str[:len(randBase62Str)-1]
		}
	}

	return randBase62Str
}

// GeneratePadding returns a padding string of the requested length using the
// given method.
func GeneratePadding(method PaddingMethod, length int) string {
	if length <= 0 {
		return ""
	}

	// HPACK/QPACK Huffman tables assign 'X' and 'Z' an 8 bit code, so the wire
	// length matches the requested length for repeat-x padding.
	switch method {
	case PaddingMethodRepeatX:
		return strings.Repeat("X", length)
	case PaddingMethodTokenish:
		paddingValue := GenerateTokenishPaddingBase62(length)
		if paddingValue == "" {
			return strings.Repeat("X", length)
		}
		return paddingValue
	default:
		return strings.Repeat("X", length)
	}
}

// ApplyPaddingToCookie appends a padding cookie to the request.
func ApplyPaddingToCookie(req *http.Request, name, value string) {
	if req == nil || name == "" || value == "" {
		return
	}
	req.AddCookie(&http.Cookie{Name: name, Value: value, Path: "/"})
}

// ApplyPaddingToResponseCookie sets a padding cookie on the response.
func ApplyPaddingToResponseCookie(writer http.ResponseWriter, name, value string) {
	if name == "" || value == "" {
		return
	}
	http.SetCookie(writer, &http.Cookie{Name: name, Value: value, Path: "/"})
}

// ApplyPaddingToQuery sets the padding value as a query parameter.
func ApplyPaddingToQuery(u *url.URL, key, value string) {
	if u == nil || key == "" || value == "" {
		return
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
}

// GetNormalizedXPaddingBytes returns the padding length range.
func (c *Config) GetNormalizedXPaddingBytes() Range {
	if c.XPaddingBytes == nil || c.XPaddingBytes.To == 0 {
		return Range{From: 100, To: 1000}
	}
	return *c.XPaddingBytes
}

// ApplyXPaddingToHeader writes the padding into an http.Header.
func (c *Config) ApplyXPaddingToHeader(h http.Header, xconfig XPaddingConfig) {
	if h == nil {
		return
	}

	paddingValue := GeneratePadding(xconfig.Method, xconfig.Length)

	switch p := xconfig.Placement; p.Placement {
	case PlacementHeader:
		h.Set(p.Header, paddingValue)
	case PlacementQueryInHeader:
		u, err := url.Parse(p.RawURL)
		if err != nil || u == nil {
			return
		}
		u.RawQuery = p.Key + "=" + paddingValue
		h.Set(p.Header, u.String())
	}
}

// ApplyXPaddingToRequest writes the padding into the request.
func (c *Config) ApplyXPaddingToRequest(req *http.Request, xconfig XPaddingConfig) {
	if req == nil {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}

	placement := xconfig.Placement.Placement

	if placement == PlacementHeader || placement == PlacementQueryInHeader {
		c.ApplyXPaddingToHeader(req.Header, xconfig)
		return
	}

	paddingValue := GeneratePadding(xconfig.Method, xconfig.Length)

	switch placement {
	case PlacementCookie:
		ApplyPaddingToCookie(req, xconfig.Placement.Key, paddingValue)
	case PlacementQuery:
		ApplyPaddingToQuery(req.URL, xconfig.Placement.Key, paddingValue)
	}
}

// ApplyXPaddingToResponse writes the padding into the response.
func (c *Config) ApplyXPaddingToResponse(writer http.ResponseWriter, xconfig XPaddingConfig) {
	placement := xconfig.Placement.Placement

	if placement == PlacementHeader || placement == PlacementQueryInHeader {
		c.ApplyXPaddingToHeader(writer.Header(), xconfig)
		return
	}

	paddingValue := GeneratePadding(xconfig.Method, xconfig.Length)

	switch placement {
	case PlacementCookie:
		ApplyPaddingToResponseCookie(writer, xconfig.Placement.Key, paddingValue)
	}
}

// ExtractXPaddingFromRequest reads the padding value from a request, returning
// both the value and a human-readable description of where it was found.
func (c *Config) ExtractXPaddingFromRequest(req *http.Request, obfsMode bool) (string, string) {
	if req == nil {
		return "", ""
	}

	if !obfsMode {
		referrer := req.Header.Get("Referer")

		if referrer != "" {
			if referrerURL, err := url.Parse(referrer); err == nil {
				paddingValue := referrerURL.Query().Get("x_padding")
				paddingPlacement := PlacementQueryInHeader + "=Referer, key=x_padding"
				return paddingValue, paddingPlacement
			}
		} else {
			paddingValue := req.URL.Query().Get("x_padding")
			return paddingValue, PlacementQuery + ", key=x_padding"
		}
	}

	key := c.XPaddingKey
	header := c.XPaddingHeader

	if cookie, err := req.Cookie(key); err == nil {
		if cookie != nil && cookie.Value != "" {
			return cookie.Value, PlacementCookie + ", key=" + key
		}
	}

	headerValue := req.Header.Get(header)

	if headerValue != "" {
		if c.XPaddingPlacement == PlacementHeader {
			return headerValue, PlacementHeader + "=" + header
		}

		if parsedURL, err := url.Parse(headerValue); err == nil {
			return parsedURL.Query().Get(key), PlacementQueryInHeader + "=" + header + ", key=" + key
		}
	}

	queryValue := req.URL.Query().Get(key)

	if queryValue != "" {
		return queryValue, PlacementQuery + ", key=" + key
	}

	return "", ""
}

// IsPaddingValid reports whether paddingValue matches the expected length range.
func (c *Config) IsPaddingValid(paddingValue string, from, to int32, method PaddingMethod) bool {
	if paddingValue == "" {
		return false
	}
	if to <= 0 {
		r := c.GetNormalizedXPaddingBytes()
		from, to = r.From, r.To
	}

	switch method {
	case PaddingMethodRepeatX:
		n := int32(len(paddingValue))
		return n >= from && n <= to
	case PaddingMethodTokenish:
		const tolerance = int32(validationTolerance)

		n := int32(hpack.HuffmanEncodeLength(paddingValue))
		f := from - tolerance
		t := to + tolerance
		if f < 0 {
			f = 0
		}
		return n >= f && n <= t
	default:
		n := int32(len(paddingValue))
		return n >= from && n <= to
	}
}
