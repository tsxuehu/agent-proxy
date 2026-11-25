package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsxuehu/agent-proxy/shared/app"
	"github.com/tsxuehu/agent-proxy/shared/codex"
	"github.com/tsxuehu/agent-proxy/shared/keys"
	relay "github.com/tsxuehu/agent-proxy/shared/relay/v1"
	"github.com/tsxuehu/agent-proxy/shared/storage"
	"google.golang.org/grpc/metadata"
)

type Server struct {
	Store *storage.Store
	Relay relay.RelayClient
	Token string
}

func (s Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /e/{id}/v1/models", s.models)
	mux.HandleFunc("POST /e/{id}/v1/responses", s.serveResponses)
	mux.HandleFunc("POST /e/{id}/v1/responses/compact", s.serveResponses)
	mux.HandleFunc("GET /e/{id}/v1/responses", s.websocket)
	mux.HandleFunc("POST /e/{id}/v1/messages", s.messages)
	mux.HandleFunc("POST /e/{id}/v1/messages/count_tokens", s.countTokens)
}

func (s Server) endpoint(r *http.Request, protocol string) (storage.Endpoint, int) {
	token := app.Bearer(r.Header.Get("Authorization"))
	if token == "" && protocol == "messages" {
		token = r.Header.Get("X-Api-Key")
	}
	if token == "" || len(token) > 512 || len(r.PathValue("id")) > 64 {
		return storage.Endpoint{}, 401
	}
	e, err := s.Store.Endpoint(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Endpoint{}, 401
	}
	if err != nil {
		return storage.Endpoint{}, 503
	}
	if !e.Enabled || e.Protocol != protocol || !keys.Matches(e.KeyHash, token) {
		return storage.Endpoint{}, 401
	}
	return e, 0
}

func (s Server) models(w http.ResponseWriter, r *http.Request) {
	e, failure := s.endpoint(r, "responses")
	if failure != 0 {
		problem(w, failure, "endpoint unavailable")
		return
	}
	names := make([]string, 0, len(e.Models))
	for name := range e.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	data := make([]map[string]string, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]string{"id": name, "object": "model", "owned_by": "agent-proxy"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s Server) serveResponses(w http.ResponseWriter, r *http.Request) {
	e, failure := s.endpoint(r, "responses")
	if failure != 0 {
		problem(w, failure, "endpoint unavailable")
		return
	}
	raw, err := readBody(w, r)
	if err != nil {
		inputError(w, err)
		return
	}
	raw, err = mapModel(raw, e)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	operation := "responses"
	if strings.HasSuffix(r.URL.Path, "/compact") {
		operation = "compact"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	stream, err := s.open(ctx, r, e, operation, false)
	if err != nil || sendTurn(stream, raw) != nil || stream.CloseSend() != nil {
		problem(w, 503, "upstream relay unavailable")
		return
	}
	copyResponse(w, stream)
}

func mapModel(raw []byte, endpoint storage.Endpoint) ([]byte, error) {
	return mapTurnModel(raw, endpoint, nil)
}

func mapTurnModel(raw []byte, endpoint storage.Endpoint, previous *string) ([]byte, error) {
	var payload map[string]json.RawMessage
	var model string
	if json.Unmarshal(raw, &payload) != nil || payload == nil {
		return nil, errors.New("missing or invalid model")
	}
	inherited := payload["model"] == nil && previous != nil
	if inherited {
		model = *previous
	} else if json.Unmarshal(payload["model"], &model) != nil {
		return nil, errors.New("missing or invalid model")
	}
	mapped := endpoint.Models[model]
	if mapped == "" {
		return nil, errors.New("model is unavailable on this endpoint")
	}
	if previous != nil {
		*previous = model
	}
	if mapped == model && !inherited {
		return raw, nil
	}
	payload["model"], _ = json.Marshal(mapped)
	return json.Marshal(payload)
}

func (s Server) open(ctx context.Context, r *http.Request, e storage.Endpoint, operation string, websocketMode bool) (relay.Relay_ForwardClient, error) {
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+s.Token)
	stream, err := s.Relay.Forward(ctx)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for _, key := range codex.AllowedHeaders {
		if value := r.Header.Get(key); value != "" && len(value) <= 8192 {
			headers[key] = value
		}
	}
	session := r.Header.Get("session_id")
	if session == "" {
		session = r.Header.Get("conversation_id")
	}
	if len(session) > 1024 {
		session = ""
	}
	request := &relay.Open{EndpointId: e.ID, UserId: e.UserID, RouteVersion: e.Version,
		KeyId: e.KeyID, Backend: e.Backend, SessionId: session, Operation: operation, Websocket: websocketMode, Headers: headers}
	if err := stream.Send(&relay.Request{Payload: &relay.Request_Open{Open: request}}); err != nil {
		return nil, err
	}
	return stream, nil
}

func sendTurn(stream relay.Relay_ForwardClient, turn []byte) error {
	return stream.Send(&relay.Request{Payload: &relay.Request_Turn{Turn: turn}})
}

func copyResponse(w http.ResponseWriter, stream relay.Relay_ForwardClient) {
	started := false
	sse := false
	for {
		event, err := stream.Recv()
		if err != nil {
			if !started {
				problem(w, 503, "upstream relay unavailable")
			} else if sse {
				_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"upstream_interrupted\",\"message\":\"the upstream stream was interrupted\"}\n\n")
				_ = http.NewResponseController(w).Flush()
			}
			return
		}
		switch item := event.Payload.(type) {
		case *relay.Event_Head:
			for name, value := range item.Head.Headers {
				w.Header().Set(name, value)
			}
			sse = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
			if sse {
				w.Header().Set("X-Accel-Buffering", "no")
				w.Header().Set("Cache-Control", "no-cache")
			}
			w.WriteHeader(int(item.Head.Status))
			started = true
		case *relay.Event_Data:
			if _, err := w.Write(item.Data); err != nil {
				return
			}
			if sse && http.NewResponseController(w).Flush() != nil {
				return
			}
		case *relay.Event_Done:
			return
		}
	}
}

type wsInput struct {
	raw []byte
	err error
}
type wsOutput struct {
	event *relay.Event
	err   error
}

func (s Server) websocket(w http.ResponseWriter, r *http.Request) {
	e, failure := s.endpoint(r, "responses")
	if failure != 0 {
		problem(w, failure, "endpoint unavailable")
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		w.Header().Set("Upgrade", "websocket")
		problem(w, http.StatusUpgradeRequired, "use WebSocket upgrade or POST responses")
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stream, err := s.open(ctx, r, e, "responses", true)
	if err != nil {
		wsProblem(conn, 503, "upstream relay unavailable")
		return
	}
	conn.SetReadLimit(codex.MaxBody)
	frames := make(chan wsInput, 1)
	go func() {
		defer cancel()
		for {
			kind, raw, err := conn.ReadMessage()
			if kind != websocket.TextMessage && err == nil {
				err = errors.New("only text frames are supported")
			}
			select {
			case frames <- wsInput{raw, err}:
			case <-ctx.Done():
				return
			default:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	responses := make(chan wsOutput)
	go func() {
		for {
			event, err := stream.Recv()
			select {
			case responses <- wsOutput{event, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	var sent bool
	var statusCode int
	var retryAfter string
	var previousModel string
	for {
		inputStream := frames
		if sent {
			inputStream = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}
		case input := <-inputStream:
			if input.err != nil {
				return
			}
			mapped, err := mapTurnModel(input.raw, e, &previousModel)
			if err != nil {
				if wsProblem(conn, 400, err.Error()) != nil {
					return
				}
				continue
			}
			if sendTurn(stream, mapped) != nil {
				wsProblem(conn, 503, "upstream relay unavailable")
				return
			}
			sent = true
			statusCode = 0
			retryAfter = ""
		case output := <-responses:
			if output.err != nil {
				wsProblem(conn, 502, "upstream stream interrupted")
				return
			}
			switch part := output.event.Payload.(type) {
			case *relay.Event_Head:
				statusCode = int(part.Head.Status)
				retryAfter = part.Head.Headers["Retry-After"]
			case *relay.Event_Data:
				if statusCode >= 400 {
					message := http.StatusText(statusCode)
					var envelope struct{ Error struct{ Message string } }
					if json.Unmarshal(part.Data, &envelope) == nil && envelope.Error.Message != "" {
						message = envelope.Error.Message
					}
					if wsError(conn, statusCode, message, retryAfter) != nil {
						return
					}
				} else if wsWrite(conn, part.Data) != nil {
					return
				}
			case *relay.Event_Done:
				sent = false
				if statusCode == 403 {
					return
				}
			}
		}
	}
}

func wsWrite(conn *websocket.Conn, data []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

func wsProblem(conn *websocket.Conn, status int, message string) error {
	return wsError(conn, status, message, "")
}

func wsError(conn *websocket.Conn, status int, message, retry string) error {
	event := map[string]any{"type": "error", "status": status, "error": map[string]string{"type": "proxy_error", "message": message}}
	if retry != "" {
		event["headers"] = map[string]string{"retry-after": retry}
	}
	raw, _ := json.Marshal(event)
	return wsWrite(conn, raw)
}

func problem(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"type": "proxy_error", "message": message}})
}

func inputError(w http.ResponseWriter, err error) {
	if errors.Is(err, errEncoding) {
		problem(w, 415, errEncoding.Error())
		return
	}
	var size *http.MaxBytesError
	if errors.As(err, &size) {
		problem(w, 413, "request body too large")
		return
	}
	problem(w, 400, "invalid request body")
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
