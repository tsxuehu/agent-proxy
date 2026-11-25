package forwarder

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	relay "github.com/tsxuehu/agent-proxy/api/relay/v1"
	"github.com/tsxuehu/agent-proxy/internal/codex"
	"github.com/tsxuehu/agent-proxy/internal/keys"
	"github.com/tsxuehu/agent-proxy/internal/storage"
)

func (s *Server) websocketAttempt(ctx context.Context, stream relayStream, request *relay.Open, turn codex.Turn, raw []byte,
	a storage.Account, credential codex.Credentials, conn *connection, observe func(codex.Frame) error) attemptResult {
	fingerprint := keys.Digest(credential.Access)
	if conn.ws == nil || conn.account != a.ID || conn.credential != fingerprint {
		if turn.Previous != "" {
			conn.close()
			return attemptResult{failure: 409}
		}
		conn.close()
		upstream, err := url.Parse(s.BaseURL + "/responses")
		if err != nil {
			return attemptResult{err: err, safe: true}
		}
		switch upstream.Scheme {
		case "https":
			upstream.Scheme = "wss"
		case "http":
			upstream.Scheme = "ws"
		default:
			return attemptResult{err: errors.New("invalid Codex upstream"), safe: true}
		}
		headers := codex.Headers(request.Headers, a.ID, credential.Access)
		headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
		dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 12 * time.Second}
		ws, response, err := dialer.DialContext(ctx, upstream.String(), headers)
		if err != nil {
			if response != nil && response.Body != nil {
				defer response.Body.Close()
				return fromResponse(response)
			}
			return attemptResult{err: err, safe: true}
		}
		ws.SetReadLimit(codex.MaxEvent)
		conn.ws = ws
		conn.account = a.ID
		conn.credential = fingerprint
		conn.responses = make(map[string]struct{})
		conn.header = response.Header.Clone()
		s.observeHeaders(ctx, a.ID, response.Header)
	}
	ws := conn.ws
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	if err := ws.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.close()
		return attemptResult{err: err, safe: true}
	}
	if err := ws.WriteMessage(websocket.TextMessage, raw); err != nil {
		conn.close()
		return attemptResult{err: err}
	}
	started := false
	for {
		_ = ws.SetReadDeadline(time.Now().Add(3 * time.Minute))
		messageType, data, err := ws.ReadMessage()
		if err != nil || messageType != websocket.TextMessage || !json.Valid(data) {
			conn.close()
			if err == nil {
				err = errors.New("invalid upstream WebSocket event")
			}
			return attemptResult{err: err, started: started}
		}
		frame := codex.ParseFrame(data)
		code, failure := frame.Rejection()
		if !started {
			if code == 429 || code == 401 {
				h := conn.header.Clone()
				conn.close()
				body, _ := json.Marshal(map[string]codex.Failure{"error": failure})
				return attemptResult{failure: code, header: h, body: body, wire: data, safe: true}
			}
			if frame.Type == "codex.rate_limits" {
				continue
			}
			started = true
			if err := sendHead(stream, 200, conn.header); err != nil {
				return attemptResult{err: err, started: true}
			}
		}
		if err := observe(frame); err != nil {
			return attemptResult{err: err, started: true}
		}
		if err := sendData(stream, data); err != nil {
			return attemptResult{err: err, started: true}
		}
		if frame.Terminal() {
			h := conn.header
			if code == 429 || code == 401 {
				conn.close()
			}
			body, _ := json.Marshal(map[string]codex.Failure{"error": failure})
			return attemptResult{started: true, failure: code, body: body, header: h}
		}
	}
}
