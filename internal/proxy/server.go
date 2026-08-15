package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
	"github.com/hellowind777/hellogrok/internal/patch"
)

// Server is a channel-isolated protocol facade. Search-capable channels expose
// Responses to Grok Build while retaining the provider's real upstream format;
// other sessions use their configured consumer protocol. /responses also
// accepts Build's fixed non-streaming WebSearchClient request.
type Server struct {
	PathAddr string

	mu       sync.RWMutex
	channels map[string]config.Route
	log      *log.Logger

	pathLn        net.Listener
	pathServer    *http.Server
	wg            sync.WaitGroup
	lifecycleMu   sync.RWMutex
	requestCtx    context.Context
	requestCancel context.CancelFunc

	transport               *http.Transport
	client                  *http.Client
	deepSeekTransport       *http.Transport
	deepSeekClient          *http.Client
	connections             *connectionTracker
	shutdownTimeout         time.Duration
	bodyIdleTimeout         time.Duration
	deepSeekBodyIdleTimeout time.Duration

	probedMu  sync.Mutex
	probed    map[string]bool
	reasoning *reasoningProvenanceStore
}

const maxSSEEventBytes = 16 << 20

func New(logger *log.Logger) *Server {
	return newServer(logger, "")
}

// NewPersistent uses dataDir for privacy-preserving reasoning provenance so
// model hot switching remains reliable across process restarts.
func NewPersistent(logger *log.Logger, dataDir string) *Server {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return newServer(logger, "")
	}
	return newServer(logger, filepath.Join(dataDir, reasoningProvenanceFileName))
}

func newServer(logger *log.Logger, reasoningPath string) *Server {
	if logger == nil {
		logger = log.Default()
	}
	reasoning, reasoningErr := newReasoningProvenanceStore(reasoningPath)
	if reasoningErr != nil {
		logger.Printf("reasoning provenance load warning: %v", reasoningErr)
	}
	connections := newConnectionTracker()
	// Header deadlines are selected per route in forwardFacade. A transport-wide
	// value would make one channel's timeout silently govern every future model.
	transport := newUpstreamTransport(connections, 0)
	deepSeekTransport := newUpstreamTransport(connections, 0)
	requestCtx, requestCancel := context.WithCancel(context.Background())
	return &Server{
		PathAddr:                "127.0.0.1:18787",
		channels:                map[string]config.Route{},
		log:                     logger,
		transport:               transport,
		deepSeekTransport:       deepSeekTransport,
		connections:             connections,
		client:                  newUpstreamClient(transport),
		deepSeekClient:          newUpstreamClient(deepSeekTransport),
		shutdownTimeout:         5 * time.Second,
		bodyIdleTimeout:         defaultUpstreamBodyIdleTimeout,
		deepSeekBodyIdleTimeout: defaultDeepSeekBodyIdleTimeout,
		requestCtx:              requestCtx,
		requestCancel:           requestCancel,
		probed:                  map[string]bool{},
		reasoning:               reasoning,
	}
}

func (s *Server) SetRoutes(routes []config.Route) {
	s.mu.Lock()
	s.channels = make(map[string]config.Route, len(routes))
	for _, route := range routes {
		if id := strings.TrimSpace(route.ChannelID); id != "" {
			s.channels[id] = route
		}
	}
	s.mu.Unlock()
}

func (s *Server) lookupChannel(id string) (config.Route, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	route, ok := s.channels[strings.TrimSpace(id)]
	return route, ok
}

func (s *Server) StartPath() error {
	if err := s.ReservePath(); err != nil {
		return err
	}
	if err := s.ServePath(); err != nil {
		s.Stop()
		return err
	}
	return nil
}

// ReservePath claims the facade address without accepting requests. Startup
// uses this to establish single-instance ownership before recovering config.
func (s *Server) ReservePath() error {
	purgeLegacyRequestDiagnostics()
	if s.pathLn != nil {
		return fmt.Errorf("local facade address is already reserved")
	}
	listener, err := net.Listen("tcp", s.PathAddr)
	if err != nil {
		return err
	}
	s.pathLn = listener
	return nil
}

// ServePath starts accepting requests on the previously reserved address.
func (s *Server) ServePath() error {
	if s.pathLn == nil {
		return fmt.Errorf("local facade address is not reserved")
	}
	if s.pathServer != nil {
		return fmt.Errorf("local facade is already serving")
	}
	s.lifecycleMu.Lock()
	if s.requestCtx == nil || s.requestCtx.Err() != nil {
		s.requestCtx, s.requestCancel = context.WithCancel(context.Background())
	}
	s.connections.Open()
	s.lifecycleMu.Unlock()
	s.pathServer = &http.Server{
		Handler:           http.HandlerFunc(s.servePath),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	server := s.pathServer
	listener := s.pathLn
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.log.Printf("local facade stopped unexpectedly: %v", err)
		}
	}()
	return nil
}

func (s *Server) Stop() {
	s.lifecycleMu.Lock()
	if s.requestCancel != nil {
		s.requestCancel()
	}
	s.connections.CloseAll()
	s.lifecycleMu.Unlock()
	server := s.pathServer
	listener := s.pathLn
	if server != nil {
		timeout := s.shutdownTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if err := server.Shutdown(ctx); err != nil {
			s.log.Printf("local facade graceful shutdown failed: %v; closing active connections", err)
			_ = server.Close()
		}
		cancel()
	}
	if listener != nil {
		_ = listener.Close()
	}
	s.transport.CloseIdleConnections()
	s.deepSeekTransport.CloseIdleConnections()
	s.wg.Wait()
	if err := s.reasoning.flush(); err != nil {
		s.log.Printf("reasoning provenance flush failed: %v", err)
	}
	if s.pathServer == server {
		s.pathServer = nil
	}
	if s.pathLn == listener {
		s.pathLn = nil
	}
}

func (s *Server) upstreamLifecycleContext() context.Context {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	if s.requestCtx == nil {
		return context.Background()
	}
	return s.requestCtx
}

func (s *Server) servePath(w http.ResponseWriter, request *http.Request) {
	if !isLoopbackRequestHost(request.Host) {
		writeJSONError(w, http.StatusMisdirectedRequest, "local facade requires a loopback Host header")
		return
	}
	if strings.TrimSpace(request.Header.Get("Origin")) != "" {
		writeJSONError(w, http.StatusForbidden, "browser-origin requests are not accepted")
		return
	}
	channelID, protocol, ok := channelFromPath(request.URL.EscapedPath())
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown proxy route")
		return
	}
	route, found := s.lookupChannel(channelID)
	if !found {
		writeJSONError(w, http.StatusNotFound, "unknown custom channel")
		return
	}
	s.forwardFacade(w, request, route, protocol)
}
func newUpstreamTransport(connections *connectionTracker, responseHeaderTimeout time.Duration) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return connections.Track(conn)
		},
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		},
	}
	return transport
}

func newUpstreamClient(transport *http.Transport) *http.Client {
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// connectionTracker makes stopping the local facade also terminate requests
// that are blocked in an upstream server. The closed gate prevents a dial that
// races with Stop from escaping the shutdown sweep.
type connectionTracker struct {
	mu     sync.Mutex
	closed bool
	conns  map[*trackedConn]struct{}
}

type trackedConn struct {
	net.Conn
	owner *connectionTracker
	once  sync.Once
}

func newConnectionTracker() *connectionTracker {
	return &connectionTracker{conns: make(map[*trackedConn]struct{})}
}

func (t *connectionTracker) Open() {
	t.mu.Lock()
	t.closed = false
	t.mu.Unlock()
}

func (t *connectionTracker) Track(conn net.Conn) (net.Conn, error) {
	tracked := &trackedConn{Conn: conn, owner: t}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	t.conns[tracked] = struct{}{}
	t.mu.Unlock()
	return tracked, nil
}

func (t *connectionTracker) CloseAll() {
	t.mu.Lock()
	t.closed = true
	connections := make([]*trackedConn, 0, len(t.conns))
	for conn := range t.conns {
		connections = append(connections, conn)
	}
	clear(t.conns)
	t.mu.Unlock()

	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (t *connectionTracker) remove(conn *trackedConn) {
	t.mu.Lock()
	delete(t.conns, conn)
	t.mu.Unlock()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.owner.remove(c) })
	return err
}

func writeJSONError(w http.ResponseWriter, code int, message string) {
	writeJSONErrorWithRetry(w, code, message, false)
}

func writeRetryableJSONError(w http.ResponseWriter, code int, message string) {
	writeJSONErrorWithRetry(w, code, message, true)
}

func writeJSONErrorWithRetry(w http.ResponseWriter, code int, message string, shouldRetry bool) {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "proxy_error",
			"code":    code,
		},
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Should-Retry", fmt.Sprintf("%t", shouldRetry))
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (s *Server) probeOnce(channel string, sample []byte, isSSELine bool) {
	s.probedMu.Lock()
	if s.probed[channel] {
		s.probedMu.Unlock()
		return
	}
	s.probed[channel] = true
	s.probedMu.Unlock()

	var missing []string
	if isSSELine {
		missing = patch.FindMissingSSELine(string(sample))
	} else {
		missing = patch.FindMissingJSON(sample)
	}
	if len(missing) == 0 {
		s.log.Printf("schema probe channel=%s: no critical fields missing", channel)
		return
	}
	if len(missing) > 12 {
		missing = append(missing[:12], fmt.Sprintf("...(+%d more)", len(missing)-12))
	}
	s.log.Printf("schema probe channel=%s: filled %s", channel, strings.Join(missing, "; "))
}

func (s *Server) streamResponsesSSE(w http.ResponseWriter, response *http.Response, route config.Route, request facadeRequest, options patch.Options, started time.Time) {
	channel := route.ChannelID
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "stream unsupported")
		return
	}
	modelObserver := newUpstreamModelObserver(request.Protocol)
	defer modelObserver.log(s.log, route)
	copySafeResponseHeaders(w.Header(), response.Header)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(response.StatusCode)
	flusher.Flush()

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	events := 0
	terminal := ""
	probed := false
	evidence := newSearchEvidence()
	clientWriteFailed := false
	heartbeats := 0
	type deferredFrame struct {
		lines []string
		event map[string]any
	}
	var deferredSearchDone []deferredFrame
	var streamedText strings.Builder
	var streamedURLs []string

	writePayloadFrame := func(lines []string, payload string) error {
		insertedData := false
		for _, line := range lines {
			if strings.HasPrefix(line, "data:") {
				if !insertedData {
					if _, err := io.WriteString(w, "data: "+payload+"\n"); err != nil {
						clientWriteFailed = true
						return err
					}
					insertedData = true
				}
				continue
			}
			if _, err := io.WriteString(w, line+"\n"); err != nil {
				clientWriteFailed = true
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			clientWriteFailed = true
			return err
		}
		flusher.Flush()
		return nil
	}
	writeJSONFrame := func(lines []string, event map[string]any) error {
		event["sequence_number"] = events
		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("encode upstream Responses SSE data: %w", err)
		}
		if err := validateResponsesEventPayload(payload); err != nil {
			return fmt.Errorf("invalid upstream Responses SSE data: %w", err)
		}
		events++
		return writePayloadFrame(lines, string(payload))
	}
	writeHeartbeat := func() error {
		if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
			clientWriteFailed = true
			return err
		}
		flusher.Flush()
		return nil
	}
	findOutputItem := func(response map[string]any, id string) map[string]any {
		for _, raw := range anySlice(response["output"]) {
			item, _ := raw.(map[string]any)
			if item != nil && stringValue(item["id"]) == id {
				return item
			}
		}
		return nil
	}
	writeFrame := func(lines []string) error {
		if len(lines) == 0 {
			return nil
		}
		payload, hasData := sseFramePayload(lines)
		trimmedPayload := strings.TrimSpace(payload)
		if isPrivateSSEHeartbeat(lines, payload) {
			heartbeats++
			return writeHeartbeat()
		}
		if !hasData {
			return writePayloadFrame(lines, "")
		}
		if trimmedPayload == "" || trimmedPayload == "[DONE]" {
			return writePayloadFrame(lines, payload)
		}

		restored, err := restoreClientWebSearchAliasJSON([]byte(payload), request.ClientSearchAlias, wireResponses)
		if err != nil {
			return fmt.Errorf("invalid upstream Responses SSE data: %w", err)
		}
		rawEvent, err := decodeJSONMap(restored)
		if err != nil {
			return fmt.Errorf("invalid upstream Responses SSE data: %w", err)
		}
		modelObserver.observe(rawEvent, false)
		evidence.observeJSON(restored)
		patchedData := patch.PatchSSEDataLineWithSequence("data: "+string(restored), options, events)
		patchedPayload := strings.TrimSpace(strings.TrimPrefix(patchedData, "data:"))
		event, err := decodeJSONMap([]byte(patchedPayload))
		if err != nil {
			return fmt.Errorf("invalid upstream Responses SSE data: %w", err)
		}
		s.captureReasoningProvenance(route, event)
		streamedURLs = mergeUniqueStrings(streamedURLs, urlsFromJSON(event)...)
		switch stringValue(event["type"]) {
		case "response.output_text.delta":
			streamedText.WriteString(stringValue(event["delta"]))
		case "response.output_text.done":
			streamedText.WriteString(stringValue(event["text"]))
		}

		if stringValue(event["type"]) == "response.output_item.done" && request.HostedWebSearch {
			item, _ := event["item"].(map[string]any)
			action, _ := item["action"].(map[string]any)
			if item != nil && stringValue(item["type"]) == "web_search_call" && len(anySlice(action["sources"])) == 0 {
				deferredSearchDone = append(deferredSearchDone, deferredFrame{lines: append([]string(nil), lines...), event: event})
				return nil
			}
		}

		eventType := stringValue(event["type"])
		if eventType == "response.completed" || eventType == "response.incomplete" || eventType == "response.failed" {
			responseBody, _ := event["response"].(map[string]any)
			if responseBody != nil {
				backfillResponseSearchSources(responseBody, request.HostedWebSearch, request.SearchQuery)
				streamedURLs = mergeUniqueStrings(streamedURLs, urlsFromText(streamedText.String())...)
				mergeResponseSearchURLs(responseBody, streamedURLs)
			}
			for index, deferred := range deferredSearchDone {
				item, _ := deferred.event["item"].(map[string]any)
				if replacement := findOutputItem(responseBody, stringValue(item["id"])); replacement != nil {
					deferred.event["item"] = cloneMap(replacement)
				} else if index == len(deferredSearchDone)-1 {
					mergeWebSearchSources(item, urlsToSources(streamedURLs))
				}
				if err := writeJSONFrame(deferred.lines, deferred.event); err != nil {
					return err
				}
			}
			deferredSearchDone = nil
			terminal = eventType
			if !probed {
				encoded, _ := json.Marshal(event)
				s.probeOnce(channel, append([]byte("data: "), encoded...), true)
				probed = true
			}
		}
		return writeJSONFrame(lines, event)
	}

	frame := make([]string, 0, 4)
	frameBytes := 0
	var streamErr error
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := writeFrame(frame); err != nil {
				streamErr = err
				break
			}
			frame = frame[:0]
			frameBytes = 0
			if terminal != "" {
				break
			}
			continue
		}
		if frameBytes+len(line)+1 > maxSSEEventBytes {
			streamErr = fmt.Errorf("SSE event exceeds 16 MiB")
			break
		}
		frame = append(frame, line)
		frameBytes += len(line) + 1
	}
	if streamErr == nil {
		streamErr = scanner.Err()
	}
	if streamErr == nil && len(frame) > 0 {
		streamErr = writeFrame(frame)
	}
	if streamErr != nil {
		s.log.Printf("UP channel=%s SSE read error: %v", channel, streamErr)
		if !clientWriteFailed {
			writeResponsesStreamError(w, flusher, events, upstreamStreamFailureMessage("Responses", streamErr))
		}
	}
	s.log.Printf("UP channel=%s SSE done events=%d heartbeats=%d terminal=%s %s", channel, events, heartbeats, terminal, time.Since(started).Round(time.Millisecond))
	s.logSearchEvidence(channel, request, evidence)
	if terminal == "" {
		s.log.Printf("UP channel=%s SSE ended without a Responses terminal event", channel)
		if streamErr == nil {
			writeResponsesStreamError(w, flusher, events, "upstream Responses stream ended without a terminal event")
		}
	}
}

func writeResponsesStreamError(w http.ResponseWriter, flusher http.Flusher, sequence int, message string) {
	payload, err := json.Marshal(map[string]any{
		"type":            "error",
		"code":            "proxy_stream_error",
		"message":         message,
		"param":           nil,
		"sequence_number": sequence,
	})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	flusher.Flush()
}

func sseFramePayload(lines []string) (string, bool) {
	var parts []string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

func extractModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var request struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &request) == nil {
		return request.Model
	}
	return ""
}
