package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"xhttp/internal/hpack"
)

func TestGetNormalizedPathAndQuery(t *testing.T) {
	cases := []struct {
		path      string
		wantPath  string
		wantQuery string
	}{
		{"", "/", ""},
		{"x", "/x/", ""},
		{"/x", "/x/", ""},
		{"/x/", "/x/", ""},
		{"/x?foo=bar", "/x/", "foo=bar"},
		{"/x/?a=1&b=2", "/x/", "a=1&b=2"},
	}

	for _, tc := range cases {
		c := &Config{Path: tc.path}
		if got := c.GetNormalizedPath(); got != tc.wantPath {
			t.Errorf("Path %q: got %q, want %q", tc.path, got, tc.wantPath)
		}
		if got := c.GetNormalizedQuery(); got != tc.wantQuery {
			t.Errorf("Path %q: query got %q, want %q", tc.path, got, tc.wantQuery)
		}
	}
}

func TestPaddingRepeatX(t *testing.T) {
	c := &Config{}
	r := c.GetNormalizedXPaddingBytes()

	value := GeneratePadding(PaddingMethodRepeatX, 200)
	if len(value) != 200 {
		t.Fatalf("repeat-x length = %d, want 200", len(value))
	}
	if !c.IsPaddingValid(value, r.From, r.To, PaddingMethodRepeatX) {
		t.Fatal("repeat-x padding should be valid within default range")
	}
	if c.IsPaddingValid("short", r.From, r.To, PaddingMethodRepeatX) {
		t.Fatal("short padding should be invalid")
	}
}

func TestPaddingTokenish(t *testing.T) {
	c := &Config{}
	value := GeneratePadding(PaddingMethodTokenish, 300)
	if value == "" {
		t.Fatal("tokenish padding is empty")
	}
	encoded := int(hpack.HuffmanEncodeLength(value))
	if diff := encoded - 300; diff > validationTolerance || diff < -validationTolerance {
		t.Fatalf("tokenish huffman length = %d, want ~300", encoded)
	}
	if !c.IsPaddingValid(value, 300, 300, PaddingMethodTokenish) {
		t.Fatal("tokenish padding should be valid")
	}
}

func TestMetaPlacementRoundTrip(t *testing.T) {
	placements := []string{PlacementPath, PlacementQuery, PlacementHeader, PlacementCookie}

	for _, p := range placements {
		t.Run(p, func(t *testing.T) {
			c := &Config{Path: "/x/", SessionPlacement: p, SeqPlacement: p}

			req, err := http.NewRequest("POST", "https://example.com/x/", nil)
			if err != nil {
				t.Fatal(err)
			}
			c.ApplyMetaToRequest(req, "session-1", "42")

			sessionId, seq := c.ExtractMetaFromRequest(req, "/x/")
			if sessionId != "session-1" {
				t.Fatalf("sessionId = %q, want session-1", sessionId)
			}
			if seq != "42" {
				t.Fatalf("seq = %q, want 42", seq)
			}
		})
	}
}

// collectUplinkPayload mirrors the listener's PlacementAuto handling: it reads
// the payload back from whichever of headers, cookies or body carried it.
func collectUplinkPayload(req *http.Request, key string) ([]byte, error) {
	var chunks []string
	for i := 0; true; i++ {
		chunk := req.Header.Get(fmt.Sprintf("%s-%d", key, i))
		if chunk == "" {
			break
		}
		chunks = append(chunks, chunk)
	}
	headerPayload, err := base64.RawURLEncoding.DecodeString(strings.Join(chunks, ""))
	if err != nil {
		return nil, err
	}

	chunks = chunks[:0]
	for i := 0; true; i++ {
		c, _ := req.Cookie(fmt.Sprintf("%s_%d", key, i))
		if c == nil {
			break
		}
		chunks = append(chunks, c.Value)
	}
	cookiePayload, err := base64.RawURLEncoding.DecodeString(strings.Join(chunks, ""))
	if err != nil {
		return nil, err
	}

	var bodyPayload []byte
	if req.Body != nil {
		bodyPayload, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}

	return slices.Concat(headerPayload, cookiePayload, bodyPayload), nil
}

func TestFillPacketRequestPlacements(t *testing.T) {
	payload := []byte("hello xhttp payload")

	placements := []string{PlacementBody, PlacementHeader, PlacementCookie, PlacementAuto}

	for _, p := range placements {
		t.Run(p, func(t *testing.T) {
			c := &Config{
				Path:                "/x/",
				UplinkDataPlacement: p,
				UplinkDataKey:       "x_data",
				XPaddingBytes:       &Range{From: 10, To: 20},
			}

			req, err := http.NewRequest("POST", "https://example.com/x/", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.FillPacketRequest(req, "sess", "7", payload); err != nil {
				t.Fatal(err)
			}

			// "auto" is an alias for "body" when sending: the client always
			// picks one concrete placement, while the server accepts any.
			if p == PlacementBody || p == PlacementAuto {
				if req.ContentLength != int64(len(payload)) {
					t.Fatalf("ContentLength = %d, want %d", req.ContentLength, len(payload))
				}
			}
			if p == PlacementHeader {
				if req.Header.Get("x_data-0") == "" {
					t.Fatal("expected header payload chunk x_data-0")
				}
			}
			if p == PlacementCookie {
				if _, err := req.Cookie("x_data_0"); err != nil {
					t.Fatal("expected cookie payload chunk x_data_0")
				}
			}

			// Whatever the placement, the server must be able to recover the
			// exact payload from the request.
			got, err := collectUplinkPayload(req, "x_data")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("recovered payload = %q, want %q", got, payload)
			}

			// Every non-obfs request carries the padding in the Referer.
			referer := req.Header.Get("Referer")
			if referer == "" {
				t.Fatal("expected Referer padding header")
			}
			u, err := url.Parse(referer)
			if err != nil {
				t.Fatal(err)
			}
			if u.Query().Get("x_padding") == "" {
				t.Fatal("expected x_padding in Referer query")
			}
		})
	}
}

func TestNormalizedXmuxDefaults(t *testing.T) {
	c := &Config{}
	x := c.GetNormalizedXmux()

	if x.GetNormalizedMaxConcurrency() != (Range{From: 1, To: 1}) {
		t.Fatalf("maxConcurrency = %+v", x.GetNormalizedMaxConcurrency())
	}
	if x.GetNormalizedHMaxRequestTimes() != (Range{From: 600, To: 900}) {
		t.Fatalf("hMaxRequestTimes = %+v", x.GetNormalizedHMaxRequestTimes())
	}
	if x.GetNormalizedHMaxReusableSecs() != (Range{From: 1800, To: 3000}) {
		t.Fatalf("hMaxReusableSecs = %+v", x.GetNormalizedHMaxReusableSecs())
	}

	// Any explicitly set field suppresses the defaults entirely, matching the
	// xray-core config layer which only fills in a completely empty XmuxConfig.
	c = &Config{Xmux: &XmuxConfig{HKeepAlivePeriod: 30}}
	x = c.GetNormalizedXmux()
	if x.MaxConcurrency != nil || x.HMaxRequestTimes != nil || x.HMaxReusableSecs != nil {
		t.Fatalf("defaults applied to a non-empty xmux config: %+v", x)
	}
}

func TestUplinkChunkSizeDefaults(t *testing.T) {
	c := &Config{}
	if got := c.GetNormalizedUplinkChunkSize(); got.From != 1000000 || got.To != 1000000 {
		t.Fatalf("body chunk size = %+v", got)
	}

	c = &Config{UplinkDataPlacement: PlacementHeader}
	if got := c.GetNormalizedUplinkChunkSize(); got.From != 3000 || got.To != 4000 {
		t.Fatalf("header chunk size = %+v", got)
	}

	c = &Config{UplinkDataPlacement: PlacementCookie}
	if got := c.GetNormalizedUplinkChunkSize(); got.From != 2048 || got.To != 3072 {
		t.Fatalf("cookie chunk size = %+v", got)
	}
}

func TestWriteResponseHeaderCORS(t *testing.T) {
	c := &Config{}
	rec := httptest.NewRecorder()
	header := http.Header{}
	header.Set("Origin", "https://app.example")

	c.WriteResponseHeader(rec, "GET", header)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("origin = %q", got)
	}
}

func TestDefaultRequestHeaderHasUserAgent(t *testing.T) {
	c := &Config{}
	h := c.GetRequestHeader()
	if !strings.Contains(h.Get("User-Agent"), "Mozilla") {
		t.Fatalf("unexpected User-Agent: %q", h.Get("User-Agent"))
	}

	c = &Config{Headers: map[string]string{"User-Agent": "golang"}}
	h = c.GetRequestHeader()
	if h.Get("User-Agent") != "" {
		t.Fatalf("golang User-Agent should be stripped, got %q", h.Get("User-Agent"))
	}
}
