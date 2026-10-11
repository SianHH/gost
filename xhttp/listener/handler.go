package listener

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"xhttp/config"
	"xhttp/internal/done"
	"xhttp/internal/logger"
	"xhttp/internal/netutil"
	"xhttp/internal/splitconn"
)

type requestHandler struct {
	config    *config.Config
	host      string
	path      string
	ln        *Listener
	sessionMu *sync.Mutex
	sessions  sync.Map
	localAddr net.Addr
}

type httpSession struct {
	uploadQueue *uploadQueue
	// Until the downlink GET arrives, isFullyConnected stays open and the
	// session is subject to a short reaping TTL. Once the GET arrives it becomes
	// done and the session lives as long as that request.
	isFullyConnected *done.Instance
}

func (h *requestHandler) upsertSession(sessionId string) *httpSession {
	// fast path
	if currentSessionAny, ok := h.sessions.Load(sessionId); ok {
		return currentSessionAny.(*httpSession)
	}

	// slow path
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()

	if currentSessionAny, ok := h.sessions.Load(sessionId); ok {
		return currentSessionAny.(*httpSession)
	}

	s := &httpSession{
		uploadQueue:      newUploadQueue(h.config.GetNormalizedScMaxBufferedPosts()),
		isFullyConnected: done.New(),
	}

	h.sessions.Store(sessionId, s)

	shouldReap := done.New()
	go func() {
		time.Sleep(30 * time.Second)
		shouldReap.Close()
	}()
	go func() {
		select {
		case <-shouldReap.Wait():
			h.sessions.Delete(sessionId)
			s.uploadQueue.Close()
		case <-s.isFullyConnected.Wait():
		}
	}()

	return s
}

func (h *requestHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if len(h.host) > 0 && !netutil.IsValidHTTPHost(request.Host, h.host) {
		logger.Printf("xhttp: failed to validate host, request: %s, config: %s", request.Host, h.host)
		writer.WriteHeader(http.StatusNotFound)
		return
	}

	if !strings.HasPrefix(request.URL.Path, h.path) {
		logger.Printf("xhttp: failed to validate path, request: %s, config: %s", request.URL.Path, h.path)
		writer.WriteHeader(http.StatusNotFound)
		return
	}

	h.config.WriteResponseHeader(writer, request.Method, request.Header)
	length := int(h.config.GetNormalizedXPaddingBytes().Rand())
	xconfig := config.XPaddingConfig{Length: length}

	if h.config.XPaddingObfsMode {
		xconfig.Placement = config.XPaddingPlacement{
			Placement: h.config.XPaddingPlacement,
			Key:       h.config.XPaddingKey,
			Header:    h.config.XPaddingHeader,
		}
		xconfig.Method = config.PaddingMethod(h.config.XPaddingMethod)
	} else {
		xconfig.Placement = config.XPaddingPlacement{
			Placement: config.PlacementHeader,
			Header:    "X-Padding",
		}
	}

	h.config.ApplyXPaddingToResponse(writer, xconfig)

	if request.Method == "OPTIONS" {
		writer.WriteHeader(http.StatusOK)
		return
	}

	validRange := h.config.GetNormalizedXPaddingBytes()
	paddingValue, paddingPlacement := h.config.ExtractXPaddingFromRequest(request, h.config.XPaddingObfsMode)

	if !h.config.IsPaddingValid(paddingValue, validRange.From, validRange.To, config.PaddingMethod(h.config.XPaddingMethod)) {
		logger.Printf("xhttp: invalid padding (%s) length: %d", paddingPlacement, int32(len(paddingValue)))
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	sessionId, seqStr := h.config.ExtractMetaFromRequest(request, h.path)

	if sessionId == "" && h.config.Mode != "" && h.config.Mode != "auto" && h.config.Mode != "stream-one" && h.config.Mode != "stream-up" {
		logger.Printf("xhttp: stream-one mode is not allowed")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	var forwardedAddrs []net.IP
	forwardedAddrs = netutil.ParseXForwardedFor(request.Header)

	var err error
	var remoteAddr net.Addr
	remoteAddr, err = net.ResolveTCPAddr("tcp", request.RemoteAddr)
	if err != nil {
		remoteAddr = &net.TCPAddr{IP: []byte{0, 0, 0, 0}, Port: 0}
	}
	if len(forwardedAddrs) > 0 {
		remoteAddr = &net.TCPAddr{IP: forwardedAddrs[0], Port: 0}
	}

	var currentSession *httpSession
	if sessionId != "" {
		currentSession = h.upsertSession(sessionId)
	}
	scMaxEachPostBytes := int(h.config.GetNormalizedScMaxEachPostBytes().To)

	isUplinkRequest := false
	switch request.Method {
	case "GET":
		isUplinkRequest = seqStr != ""
	default:
		isUplinkRequest = true
	}

	uplinkDataKey := h.config.UplinkDataKey

	if isUplinkRequest && sessionId != "" { // stream-up, packet-up
		if seqStr == "" {
			if h.config.Mode != "" && h.config.Mode != "auto" && h.config.Mode != "stream-up" {
				logger.Printf("xhttp: stream-up mode is not allowed")
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			httpSC := &httpServerConn{
				Instance:       done.New(),
				Reader:         request.Body,
				ResponseWriter: writer,
			}
			err = currentSession.uploadQueue.Push(Packet{
				Reader: httpSC,
			})
			if err != nil {
				logger.Printf("xhttp: failed to upload (PushReader): %v", err)
				writer.WriteHeader(http.StatusConflict)
			} else {
				writer.Header().Set("X-Accel-Buffering", "no")
				writer.Header().Set("Cache-Control", "no-store")
				writer.WriteHeader(http.StatusOK)
				scStreamUpServerSecs := h.config.GetNormalizedScStreamUpServerSecs()
				referrer := request.Header.Get("Referer")
				if referrer != "" && scStreamUpServerSecs.To > 0 {
					go func() {
						for {
							_, err := httpSC.Write(bytes.Repeat([]byte{'X'}, int(h.config.GetNormalizedXPaddingBytes().Rand())))
							if err != nil {
								break
							}
							time.Sleep(time.Duration(scStreamUpServerSecs.Rand()) * time.Second)
						}
					}()
				}
				select {
				case <-request.Context().Done():
				case <-httpSC.Wait():
				}
			}
			httpSC.Close()
			return
		}

		if h.config.Mode != "" && h.config.Mode != "auto" && h.config.Mode != "packet-up" {
			logger.Printf("xhttp: packet-up mode is not allowed")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		dataPlacement := h.config.GetNormalizedUplinkDataPlacement()
		var headerPayload []byte
		if dataPlacement == config.PlacementAuto || dataPlacement == config.PlacementHeader {
			var headerPayloadChunks []string
			for i := 0; true; i++ {
				chunk := request.Header.Get(fmt.Sprintf("%s-%d", uplinkDataKey, i))
				if chunk == "" {
					break
				}
				headerPayloadChunks = append(headerPayloadChunks, chunk)
			}
			headerPayloadEncoded := strings.Join(headerPayloadChunks, "")
			headerPayload, err = base64.RawURLEncoding.DecodeString(headerPayloadEncoded)
			if err != nil {
				logger.Printf("xhttp: invalid base64 in header's payload: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		var cookiePayload []byte
		if dataPlacement == config.PlacementAuto || dataPlacement == config.PlacementCookie {
			var cookiePayloadChunks []string
			for i := 0; true; i++ {
				cookieName := fmt.Sprintf("%s_%d", uplinkDataKey, i)
				if c, _ := request.Cookie(cookieName); c != nil {
					cookiePayloadChunks = append(cookiePayloadChunks, c.Value)
				} else {
					break
				}
			}
			cookiePayloadEncoded := strings.Join(cookiePayloadChunks, "")
			cookiePayload, err = base64.RawURLEncoding.DecodeString(cookiePayloadEncoded)
			if err != nil {
				logger.Printf("xhttp: invalid base64 in cookies' payload: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		var bodyPayload []byte
		if dataPlacement == config.PlacementAuto || dataPlacement == config.PlacementBody {
			var readErr error
			if request.ContentLength > int64(scMaxEachPostBytes) {
				logger.Printf("xhttp: too large upload. ScMaxEachPostBytes is set to %d but request size exceeds it", scMaxEachPostBytes)
				writer.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			if request.ContentLength > 0 {
				bodyPayload = make([]byte, request.ContentLength)
				_, readErr = io.ReadFull(request.Body, bodyPayload)
			} else {
				bodyPayload, readErr = io.ReadAll(io.LimitReader(request.Body, int64(scMaxEachPostBytes)+1))
			}
			if readErr != nil {
				logger.Printf("xhttp: failed to read body payload: %v", readErr)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		var payload []byte
		switch dataPlacement {
		case config.PlacementHeader:
			payload = headerPayload
		case config.PlacementCookie:
			payload = cookiePayload
		case config.PlacementBody:
			payload = bodyPayload
		case config.PlacementAuto:
			payload = slices.Concat(headerPayload, cookiePayload, bodyPayload)
		}

		if len(payload) > scMaxEachPostBytes {
			logger.Printf("xhttp: too large upload. ScMaxEachPostBytes is set to %d but request size exceeds it", scMaxEachPostBytes)
			writer.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			logger.Printf("xhttp: failed to upload (ParseUint): %v", err)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = currentSession.uploadQueue.Push(Packet{
			Payload: payload,
			Seq:     seq,
		})

		if err != nil {
			logger.Printf("xhttp: failed to upload (PushPayload): %v", err)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}

		if len(bodyPayload) == 0 {
			// Methods without a body are usually cached by default.
			writer.Header().Set("Cache-Control", "no-store")
		}

		writer.WriteHeader(http.StatusOK)
	} else if request.Method == "GET" || sessionId == "" { // stream-down, stream-one
		if sessionId != "" {
			// After GET is done the connection is finished: disable automatic
			// session reaping and handle it in defer.
			currentSession.isFullyConnected.Close()
			defer h.sessions.Delete(sessionId)
		}

		// Instructs nginx + apache not to buffer the response body.
		writer.Header().Set("X-Accel-Buffering", "no")
		// A web-compliant header telling all middleboxes to disable caching.
		writer.Header().Set("Cache-Control", "no-store")

		if !h.config.NoSSEHeader {
			// Make the HTTP middlebox consider this a stream to disable buffering.
			writer.Header().Set("Content-Type", "text/event-stream")
		}

		writer.WriteHeader(http.StatusOK)
		if f, ok := writer.(http.Flusher); ok {
			f.Flush()
		}

		httpSC := &httpServerConn{
			Instance:       done.New(),
			Reader:         request.Body,
			ResponseWriter: writer,
		}
		conn := splitconn.New()
		conn.SetWriter(httpSC)
		conn.SetReader(httpSC)
		conn.SetRemoteAddr(remoteAddr)
		conn.SetLocalAddr(h.localAddr)
		if sessionId != "" { // if not stream-one
			conn.SetReader(currentSession.uploadQueue)
		}

		h.ln.addConn(conn)

		// "A ResponseWriter may not be used after Handler.ServeHTTP has returned."
		select {
		case <-request.Context().Done():
		case <-httpSC.Wait():
		}

		conn.Close()
	} else {
		logger.Printf("xhttp: unsupported method: %s", request.Method)
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type httpServerConn struct {
	sync.Mutex
	*done.Instance
	io.Reader // no need to Close request.Body
	http.ResponseWriter
}

func (c *httpServerConn) Write(b []byte) (int, error) {
	c.Lock()
	defer c.Unlock()
	if c.Done() {
		return 0, io.ErrClosedPipe
	}
	n, err := c.ResponseWriter.Write(b)
	if err == nil {
		if f, ok := c.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
	return n, err
}

func (c *httpServerConn) Close() error {
	c.Lock()
	defer c.Unlock()
	return c.Instance.Close()
}
