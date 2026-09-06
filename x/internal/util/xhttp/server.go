package xhttp

import (
	"encoding/base64"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-gost/core/logger"
)

// serverOptions holds the tunables of the XHTTP server-side engine.
type serverOptions struct {
	backlog int
	logger  logger.Logger
}

// ServerOption configures a serverOptions.
type ServerOption func(opts *serverOptions)

// BacklogServerOption sets the accept-queue depth.
func BacklogServerOption(backlog int) ServerOption {
	return func(opts *serverOptions) {
		opts.backlog = backlog
	}
}

// LoggerServerOption attaches a logger (may be nil).
func LoggerServerOption(log logger.Logger) ServerOption {
	return func(opts *serverOptions) {
		opts.logger = log
	}
}

// Server is the XHTTP server-side engine. It wraps an externally created
// (gost wrapper-chain processed, possibly TLS) net.Listener and hands each
// established XHTTP session out through Accept() as a net.Conn.
type Server struct {
	handler  *requestHandler
	httpSrv  *http.Server
	listener net.Listener
	cqueue   chan net.Conn
	closed   chan struct{}
	options  serverOptions
}

// NewServer creates an XHTTP server over ln. tlsEnabled selects the protocol
// mix advertised on the HTTP server: with TLS the stdlib auto-negotiates
// HTTP/2 vs HTTP/1.1 via ALPN; without TLS both plaintext HTTP/1.1 and h2c are
// enabled. HTTP/3 is intentionally not supported here yet (see Server.h3
// extension point in the xray reference design).
func NewServer(ln net.Listener, tlsEnabled bool, cfg *Config, opts ...ServerOption) *Server {
	var options serverOptions
	for _, opt := range opts {
		opt(&options)
	}
	if options.backlog <= 0 {
		options.backlog = defaultBacklog
	}

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	if tlsEnabled {
		protocols.SetHTTP2(true)
	} else {
		protocols.SetUnencryptedHTTP2(true)
	}

	s := &Server{
		listener: ln,
		cqueue:   make(chan net.Conn, options.backlog),
		closed:   make(chan struct{}),
		options:  options,
	}

	localAddr := ln.Addr()
	if localAddr == nil {
		localAddr = &net.TCPAddr{}
	}

	s.handler = &requestHandler{
		config:    cfg,
		host:      cfg.Host,
		path:      cfg.GetNormalizedPath(),
		sessionMu: &sync.Mutex{},
		sessions:  &sync.Map{},
		localAddr: localAddr,
		addConn:   s.enqueue,
		log:       options.logger,
	}

	s.httpSrv = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 4 * time.Second,
		MaxHeaderBytes:    cfg.GetNormalizedServerMaxHeaderBytes(),
		Protocols:         protocols,
	}
	return s
}

func (s *Server) enqueue(conn net.Conn) {
	select {
	case s.cqueue <- conn:
	default:
		conn.Close()
		if s.options.logger != nil {
			s.options.logger.Warnf("connection queue is full, session discarded")
		}
	}
}

// SetErrorLog routes http.Server diagnostics to the given writer.
func (s *Server) SetErrorLog(logger *stdlog.Logger) {
	if s.httpSrv != nil {
		s.httpSrv.ErrorLog = logger
	}
}

// Serve runs the HTTP server until the listener fails or Close is called.
func (s *Server) Serve() error {
	err := s.httpSrv.Serve(s.listener)
	select {
	case <-s.closed:
		return http.ErrServerClosed
	default:
	}
	return err
}

// Accept returns the next established XHTTP session.
func (s *Server) Accept() (conn net.Conn, err error) {
	select {
	case conn = <-s.cqueue:
	case <-s.closed:
		err = http.ErrServerClosed
	}
	return
}

// Close stops the server. The wrapped listener is owned by the caller and is
// not closed here.
func (s *Server) Close() error {
	select {
	case <-s.closed:
		return http.ErrServerClosed
	default:
		close(s.closed)
	}
	return s.httpSrv.Close()
}

// httpSession is the per-session state. For as long as the download GET has
// not been opened the session may be reaped after a TTL; once the GET arrives
// the session lives as long as the GET request.
type httpSession struct {
	uploadQueue      *uploadQueue
	isFullyConnected chan struct{} // closed once the download GET is open
}

func newHTTPSession(maxBuffered int) *httpSession {
	return &httpSession{
		uploadQueue:      NewUploadQueue(maxBuffered),
		isFullyConnected: make(chan struct{}),
	}
}

const sessionReapTimeout = 30 * time.Second

type requestHandler struct {
	config    *Config
	host      string
	path      string
	sessionMu *sync.Mutex
	sessions  *sync.Map
	localAddr net.Addr
	addConn   func(net.Conn)
	log       logger.Logger
}

func (h *requestHandler) logInfo(args ...any) {
	if h.log != nil {
		h.log.Info(args...)
	}
}

func (h *requestHandler) upsertSession(sessionID string) *httpSession {
	// fast path
	if current, ok := h.sessions.Load(sessionID); ok {
		return current.(*httpSession)
	}

	// slow path
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()

	if current, ok := h.sessions.Load(sessionID); ok {
		return current.(*httpSession)
	}

	s := newHTTPSession(h.config.GetNormalizedScMaxBufferedPosts())
	h.sessions.Store(sessionID, s)

	// Reap sessions whose download GET never arrives within the TTL.
	go func() {
		timer := time.NewTimer(sessionReapTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			h.sessions.Delete(sessionID)
			s.uploadQueue.Close()
		case <-s.isFullyConnected:
		}
	}()

	return s
}

func (h *requestHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if len(h.host) > 0 && !isValidHTTPHost(request.Host, h.host) {
		h.logInfo("failed to validate host, request:", request.Host, ", config:", h.host)
		writer.WriteHeader(http.StatusNotFound)
		return
	}

	if !strings.HasPrefix(request.URL.Path, h.path) {
		h.logInfo("failed to validate path, request:", request.URL.Path, ", config:", h.path)
		writer.WriteHeader(http.StatusNotFound)
		return
	}

	h.config.WriteResponseHeader(writer, request.Method, request.Header)

	length := h.config.GetNormalizedXPaddingBytes().rand()
	paddingCfg := XPaddingConfig{Length: length}
	if h.config.XPaddingObfsMode {
		paddingCfg.Placement = XPaddingPlacement{
			Placement: h.config.XPaddingPlacement,
			Key:       h.config.XPaddingKey,
			Header:    h.config.XPaddingHeader,
		}
		paddingCfg.Method = PaddingMethod(h.config.XPaddingMethod)
	} else {
		paddingCfg.Placement = XPaddingPlacement{
			Placement: PlacementHeader,
			Header:    "X-Padding",
		}
	}
	h.config.ApplyXPaddingToResponse(writer, paddingCfg)

	if request.Method == http.MethodOptions {
		writer.WriteHeader(http.StatusOK)
		return
	}

	valid, paddingValue, paddingPlacement := h.config.ValidateRequestPadding(request)
	if !valid {
		h.logInfo("invalid padding (", paddingPlacement, ") length:", len(paddingValue))
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	sessionID, seqStr := h.config.ExtractMetaFromRequest(request, h.path)

	if sessionID == "" && h.config.Mode != "" && h.config.Mode != "auto" &&
		h.config.Mode != "stream-one" && h.config.Mode != "stream-up" {
		h.logInfo("stream-one mode is not allowed")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	// X-Forwarded-For is intentionally not honoured here: the remote address
	// reported to the application is simply request.RemoteAddr. (gost listener
	// wrappers perform their own admission/metrics based on this address.)
	remoteAddr, err := net.ResolveTCPAddr("tcp", request.RemoteAddr)
	if err != nil {
		remoteAddr = &net.TCPAddr{IP: net.IPv4zero, Port: 0}
	}

	var currentSession *httpSession
	if sessionID != "" {
		currentSession = h.upsertSession(sessionID)
	}
	scMaxEachPostBytes := h.config.GetNormalizedScMaxEachPostBytes().To

	isUplinkRequest := request.Method != http.MethodGet || seqStr != ""

	if isUplinkRequest && sessionID != "" { // stream-up / packet-up
		if seqStr == "" { // stream-up upload stream
			if h.config.Mode != "" && h.config.Mode != "auto" && h.config.Mode != "stream-up" {
				h.logInfo("stream-up mode is not allowed")
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			httpSC := &httpServerConn{
				done:   make(chan struct{}),
				reader: request.Body,
				writer: writer,
			}
			err := currentSession.uploadQueue.Push(Packet{Reader: httpSC})
			if err != nil {
				h.logInfo("failed to upload (PushReader):", err)
				writer.WriteHeader(http.StatusConflict)
			} else {
				writer.Header().Set("X-Accel-Buffering", "no")
				writer.Header().Set("Cache-Control", "no-store")
				writer.WriteHeader(http.StatusOK)
				scStreamUpServerSecs := h.config.GetNormalizedScStreamUpServerSecs()
				referrer := request.Header.Get("Referer")
				if referrer != "" && scStreamUpServerSecs.To > 0 {
					// Heartbeats keep middleboxes from considering the upload
					// stream idle/complete.
					go func() {
						for {
							heartbeat := make([]byte, h.config.GetNormalizedXPaddingBytes().rand())
							for i := range heartbeat {
								heartbeat[i] = 'X'
							}
							if _, err := httpSC.Write(heartbeat); err != nil {
								return
							}
							time.Sleep(time.Duration(scStreamUpServerSecs.rand()) * time.Second)
						}
					}()
				}
				select {
				case <-request.Context().Done():
				case <-httpSC.done:
				}
			}
			httpSC.Close()
			return
		}

		// packet-up: a discrete upload chunk carrying payload + seq.
		if h.config.Mode != "" && h.config.Mode != "auto" && h.config.Mode != "packet-up" {
			h.logInfo("packet-up mode is not allowed")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		dataPlacement := h.config.GetNormalizedUplinkDataPlacement()
		uplinkDataKey := h.config.UplinkDataKey

		var headerPayload []byte
		if dataPlacement == PlacementAuto || dataPlacement == PlacementHeader {
			var headerPayloadChunks []string
			for i := 0; ; i++ {
				chunk := request.Header.Get(fmt.Sprintf("%s-%d", uplinkDataKey, i))
				if chunk == "" {
					break
				}
				headerPayloadChunks = append(headerPayloadChunks, chunk)
			}
			headerPayloadEncoded := strings.Join(headerPayloadChunks, "")
			headerPayload, err = base64.RawURLEncoding.DecodeString(headerPayloadEncoded)
			if err != nil {
				h.logInfo("invalid base64 in header's payload:", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		var cookiePayload []byte
		if dataPlacement == PlacementAuto || dataPlacement == PlacementCookie {
			var cookiePayloadChunks []string
			for i := 0; ; i++ {
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
				h.logInfo("invalid base64 in cookies' payload:", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		var bodyPayload []byte
		if dataPlacement == PlacementAuto || dataPlacement == PlacementBody {
			var readErr error
			if request.ContentLength > int64(scMaxEachPostBytes) {
				h.logInfo("Too large upload. scMaxEachPostBytes is set to ", scMaxEachPostBytes,
					" but request size exceed it. Adjust scMaxEachPostBytes on the server to be at least as large as client.")
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
				h.logInfo("failed to read body payload:", readErr)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		var payload []byte
		switch dataPlacement {
		case PlacementHeader:
			payload = headerPayload
		case PlacementCookie:
			payload = cookiePayload
		case PlacementBody:
			payload = bodyPayload
		case PlacementAuto:
			payload = append(append(headerPayload, cookiePayload...), bodyPayload...)
		}

		if len(payload) > scMaxEachPostBytes {
			h.logInfo("Too large upload. scMaxEachPostBytes is set to ", scMaxEachPostBytes,
				" but request size exceed it. Adjust scMaxEachPostBytes on the server to be at least as large as client.")
			writer.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			h.logInfo("failed to upload (ParseUint):", err)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = currentSession.uploadQueue.Push(Packet{Payload: payload, Seq: seq})
		if err != nil {
			h.logInfo("failed to upload (PushPayload):", err)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}

		if len(bodyPayload) == 0 {
			// Methods without a body are usually cached by default.
			writer.Header().Set("Cache-Control", "no-store")
		}

		writer.WriteHeader(http.StatusOK)
	} else if request.Method == http.MethodGet || sessionID == "" { // stream-down / stream-one
		if sessionID != "" {
			// After the download GET is done, the connection is finished.
			// Disable automatic session reaping; handled by the deferred delete.
			select {
			case <-currentSession.isFullyConnected:
			default:
				close(currentSession.isFullyConnected)
			}
			defer h.sessions.Delete(sessionID)
		}

		// magic header instructing nginx + apache to not buffer the response.
		writer.Header().Set("X-Accel-Buffering", "no")
		// A web-compliant header telling all middleboxes to disable caching.
		writer.Header().Set("Cache-Control", "no-store")

		if !h.config.NoSSEHeader {
			// magic header to make HTTP middleboxes treat this as SSE so they
			// disable their buffering.
			writer.Header().Set("Content-Type", "text/event-stream")
		}

		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()

		httpSC := &httpServerConn{
			done:   make(chan struct{}),
			reader: request.Body,
			writer: writer,
		}
		conn := &splitConn{
			writer:     httpSC,
			reader:     httpSC,
			remoteAddr: remoteAddr,
			localAddr:  h.localAddr,
		}
		if sessionID != "" { // stream-down: read the reassembled upload stream
			conn.reader = currentSession.uploadQueue
		}

		h.addConn(conn)

		// "A ResponseWriter may not be used after [Handler.ServeHTTP] has returned."
		select {
		case <-request.Context().Done():
		case <-httpSC.done:
		}

		conn.Close()
	} else {
		h.logInfo("unsupported method: ", request.Method)
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// isValidHTTPHost ports xray internet.IsValidHTTPHost.
func isValidHTTPHost(request, config string) bool {
	r := strings.ToLower(request)
	c := strings.ToLower(config)
	if strings.Contains(r, ":") {
		if h, _, err := net.SplitHostPort(r); err == nil {
			return h == c
		}
	}
	return r == c
}

const defaultBacklog = 128
