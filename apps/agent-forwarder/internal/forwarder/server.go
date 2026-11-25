package forwarder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/tsxuehu/agent-proxy/apps/agent-forwarder/internal/lease"
	"github.com/tsxuehu/agent-proxy/shared/app"
	"github.com/tsxuehu/agent-proxy/shared/codex"
	"github.com/tsxuehu/agent-proxy/shared/keys"
	relay "github.com/tsxuehu/agent-proxy/shared/relay/v1"
	"github.com/tsxuehu/agent-proxy/shared/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	relay.UnimplementedRelayServer
	Store        *storage.Store
	Cache        *redis.Client
	Vault        keys.Vault
	Token        string
	HTTP         *http.Client
	BaseURL      string
	UsageURL     string
	Tokens       codex.TokenClient
	UserLimit    int
	AccountLimit int
}

type connection struct {
	ws         *websocket.Conn
	account    string
	credential [32]byte
	responses  map[string]struct{}
	header     http.Header
}

func (c *connection) close() {
	if c.ws != nil {
		_ = c.ws.Close()
	}
	*c = connection{}
}

type relayStream = grpc.BidiStreamingServer[relay.Request, relay.Event]

func (s *Server) Forward(stream relayStream) error {
	digest := keys.Digest(s.Token)
	token := metadata.ValueFromIncomingContext(stream.Context(), "authorization")
	if len(token) != 1 || !keys.Matches(digest[:], app.Bearer(token[0])) {
		return status.Error(codes.Unauthenticated, "relay authentication required")
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil || open.Backend != codex.Backend || open.EndpointId == "" ||
		open.UserId == "" || open.RouteVersion == 0 || len(open.SessionId) > 1024 ||
		(open.Operation != "responses" && open.Operation != "compact") ||
		(open.Websocket && open.Operation != "responses") {
		return status.Error(codes.InvalidArgument, "invalid relay session")
	}
	var conn connection
	defer conn.close()
	for {
		request, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if request.GetTurn() == nil {
			return status.Error(codes.InvalidArgument, "expected turn")
		}
		if err := s.turn(stream, open, request.GetTurn(), &conn); err != nil {
			return err
		}
		if err := stream.Send(&relay.Event{Payload: &relay.Event_Done{Done: &relay.Done{}}}); err != nil {
			return err
		}
		if !open.Websocket {
			return nil
		}
	}
}

type attemptResult struct {
	failure int
	header  http.Header
	body    []byte
	wire    []byte
	err     error
	started bool
	safe    bool
}

func (s *Server) turn(stream relayStream, open *relay.Open, raw []byte, conn *connection) error {
	ctx, cancel := context.WithTimeout(stream.Context(), 30*time.Minute)
	defer cancel()
	turn, err := codex.ReadTurn(raw)
	if err != nil {
		return problem(stream, 400, "invalid Responses request", "")
	}
	if open.Websocket && turn.Type != "response.create" {
		return problem(stream, 400, "only response.create is supported on this connection", "")
	}
	endpoint, err := s.Store.Endpoint(ctx, open.EndpointId)
	if err != nil {
		return problem(stream, 503, "endpoint storage unavailable", "")
	}
	if !endpoint.Enabled || endpoint.Version != open.RouteVersion ||
		endpoint.UserID != open.UserId || endpoint.KeyID != open.KeyId || endpoint.Backend != open.Backend {
		return problem(stream, 403, "endpoint authorization or version has changed", "")
	}
	session := open.SessionId
	if session == "" {
		session = turn.Session
	}
	scope := func(value string) [32]byte {
		return storage.Scope(open.UserId, open.EndpointId, strconv.FormatUint(open.RouteVersion, 10), open.Backend, value)
	}
	raw, err = codex.Normalize(raw, open.Operation, open.Websocket, scope)
	if err != nil {
		return problem(stream, 400, "invalid Responses payload", "")
	}
	turnOpen := proto.CloneOf(open)
	for name, value := range open.Headers {
		if name == "session_id" || name == "conversation_id" {
			digest := scope(value)
			value = hex.EncodeToString(digest[:])
		}
		turnOpen.Headers[name] = value
	}
	pinned := ""
	if turn.Previous != "" {
		pinned, err = s.Store.Binding(ctx, storage.Response, scope(turn.Previous))
		if err != nil {
			return problem(stream, 503, "session storage unavailable", "")
		}
		_, knownConnection := conn.responses[turn.Previous]
		if pinned == "" || (open.Websocket && (!knownConnection || pinned != conn.account || conn.ws == nil)) {
			return problem(stream, 409, "previous response unavailable; reconnect and resend full context", "")
		}
	}
	preferred := pinned
	if preferred == "" && session != "" {
		preferred, err = s.Store.Binding(ctx, storage.Session, scope(session))
		if err != nil {
			return problem(stream, 503, "session storage unavailable", "")
		}
	}
	if preferred == "" && open.Websocket {
		preferred = conn.account
	}
	accounts, err := s.Store.Accounts(ctx, true, turn.Model)
	if err != nil {
		return problem(stream, 503, "account storage unavailable", "")
	}
	rank(accounts, preferred, scope(session))
	tried := 0
	var nextReset time.Time
	hadOtherFailure := false
	for _, account := range accounts {
		if tried >= 3 {
			break
		}
		if pinned != "" && account.ID != pinned {
			continue
		}
		limits := []lease.Limit{{Key: "user:" + open.UserId, Max: s.UserLimit}, {Key: "account:" + account.ID, Max: s.AccountLimit}}
		if session != "" {
			bind := scope(session)
			limits = append(limits, lease.Limit{Key: "session:" + hex.EncodeToString(bind[:]), Max: 1})
		}
		active, release, err := lease.Acquire(ctx, s.Cache, limits...)
		if err != nil {
			return problem(stream, 503, "concurrency store unavailable", "")
		}
		if release == nil {
			hadOtherFailure = true
			continue
		}
		tried++
		request := turnOpen
		if account.ID != preferred && turnOpen.Headers["x-codex-turn-state"] != "" {
			request = proto.CloneOf(turnOpen)
			delete(request.Headers, "x-codex-turn-state")
		}
		result := s.runAccount(active, stream, request, turn, raw, account, conn, scope, session)
		release()
		if result.err != nil {
			if result.started {
				return result.err
			}
			hadOtherFailure = true
			if !result.safe {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return problem(stream, 502, "upstream disconnected before a complete response", "")
			}
			continue
		}
		if result.failure == 0 || result.started {
			return nil
		}
		if result.failure == 429 {
			reset := codex.RetryAt(codex.ParseFailure(result.body), result.header, time.Now())
			if nextReset.IsZero() || reset.Before(nextReset) {
				nextReset = reset
			}
		}
		if result.safe && pinned == "" && (result.failure == 401 || result.failure == 429) {
			if result.failure != 429 {
				hadOtherFailure = true
			}
			continue
		}
		return rejected(stream, result)
	}
	if !hadOtherFailure && pinned == "" {
		if nextReset.IsZero() {
			nextReset, err = s.Store.NextCooldown(ctx, turn.Model)
			if err != nil {
				return problem(stream, 503, "account storage unavailable", "")
			}
		}
		if !nextReset.IsZero() {
			wait := max(1, int(time.Until(nextReset).Seconds()+0.999))
			return problem(stream, 429, "upstream quota temporarily exhausted", strconv.Itoa(wait))
		}
	}
	if pinned != "" || turn.Previous != "" {
		return problem(stream, 409, "bound account unavailable; reconnect and resend full context", "")
	}
	return problem(stream, 503, "no eligible Codex account or concurrency slot", "")
}

func rank(accounts []storage.Account, pinned string, hash [32]byte) {
	now := time.Now().Add(-5 * time.Minute)
	band := func(a storage.Account) int {
		if !a.Quota.Valid || !a.QuotaObserved.Time.After(now) {
			return 2
		}
		switch {
		case a.Quota.Float64 < 0.5:
			return 0
		case a.Quota.Float64 < 0.9:
			return 1
		default:
			return 3
		}
	}
	score := func(id string) string {
		key := sha256.Sum256(append(hash[:], id...))
		return string(key[:])
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		a, b := accounts[i], accounts[j]
		if a.ID == pinned || b.ID == pinned {
			return a.ID == pinned
		}
		if x, y := band(a), band(b); x != y {
			return x < y
		}
		return score(a.ID) > score(b.ID)
	})
}

func (s *Server) runAccount(ctx context.Context, stream relayStream, open *relay.Open, turn codex.Turn, raw []byte,
	account storage.Account, conn *connection, scope func(string) [32]byte, session string) attemptResult {
	account, err := s.Store.EligibleAccount(ctx, account.ID, turn.Model)
	if err != nil {
		return attemptResult{err: err, safe: true}
	}
	credential, err := s.credential(ctx, account, false)
	if err != nil {
		return attemptResult{err: err, safe: true}
	}
	var nResponses int
	observed := func(frame codex.Frame) error {
		if frame.Response.ID == "" || nResponses > 1 {
			return nil
		}
		if frame.Type != "response.created" && frame.Type != "response.completed" && frame.Type != "response.done" && !frame.Terminal() {
			return nil
		}
		nResponses++
		expiry := time.Now().Add(12 * time.Hour)
		if err := s.Store.Bind(ctx, storage.Response, scope(frame.Response.ID), account.ID, expiry); err != nil {
			return err
		}
		if open.Websocket {
			if len(conn.responses) >= 1024 {
				clear(conn.responses)
			}
			conn.responses[frame.Response.ID] = struct{}{}
		}
		if session != "" {
			return s.Store.Bind(ctx, storage.Session, scope(session), account.ID, expiry)
		}
		return nil
	}
	run := func(credential codex.Credentials) attemptResult {
		if open.Websocket {
			return s.websocketAttempt(ctx, stream, open, turn, raw, account, credential, conn, observed)
		}
		return s.httpAttempt(ctx, stream, open, turn, raw, account, credential, observed)
	}
	result := run(credential)
	if result.failure == 401 && result.safe && turn.Previous == "" {
		fresh, err := s.credential(ctx, account, true)
		if err == nil {
			result = run(fresh)
		}
	}
	if result.failure == 429 {
		failure := codex.ParseFailure(result.body)
		model := ""
		if failure.Scope == "model" || failure.Model == turn.Model {
			model = turn.Model
		}
		if err := s.Store.Cool(ctx, account.ID, model, codex.RetryAt(failure, result.header, time.Now())); err != nil {
			return attemptResult{err: err, started: result.started}
		}
	}
	return result
}

func sendHead(stream relayStream, status int, headers http.Header) error {
	out := make(map[string]string)
	for _, name := range codex.ResponseHeaders {
		if value := headers.Get(name); value != "" && len(value) <= 8192 {
			out[name] = value
		}
	}
	return stream.Send(&relay.Event{Payload: &relay.Event_Head{Head: &relay.Head{Status: int32(status), Headers: out}}})
}

func sendData(stream relayStream, data []byte) error {
	return stream.Send(&relay.Event{Payload: &relay.Event_Data{Data: data}})
}

func problem(stream relayStream, code int, message, retryAfter string) error {
	header := http.Header{"Content-Type": []string{"application/json"}}
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "proxy_error", "message": message}})
	if err := sendHead(stream, code, header); err != nil {
		return err
	}
	return sendData(stream, raw)
}

func rejected(stream relayStream, result attemptResult) error {
	if len(result.wire) > 0 {
		if err := sendHead(stream, 200, nil); err != nil {
			return err
		}
		return sendData(stream, result.wire)
	}
	return problem(stream, result.failure, http.StatusText(result.failure), result.header.Get("Retry-After"))
}
