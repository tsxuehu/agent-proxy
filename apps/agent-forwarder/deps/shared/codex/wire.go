package codex

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const MaxBody = 16 << 20
const MaxEvent = 8 << 20
const Backend = "codex-pool"

var AllowedHeaders = []string{
	"session_id", "conversation_id", "x-codex-turn-state", "x-codex-turn-metadata",
	"x-codex-client-version", "x-client-version", "version", "user-agent",
}

var ResponseHeaders = []string{
	"Content-Type", "Retry-After", "X-Request-Id", "X-Codex-Turn-State",
	"X-Codex-Primary-Used-Percent", "X-Codex-Secondary-Used-Percent",
	"X-Codex-Primary-Reset-At", "X-Codex-Secondary-Reset-At",
}

func Headers(selected map[string]string, id, credential string) http.Header {
	h := make(http.Header)
	for _, header := range AllowedHeaders {
		value := selected[header]
		if len(value) <= 8192 && value != "" && !strings.ContainsAny(value, "\r\n") {
			h.Set(header, value)
		}
	}
	h.Set("Authorization", "Bearer "+credential)
	h.Set("ChatGPT-Account-Id", id)
	return h
}

type Turn struct {
	Model    string `json:"model"`
	Type     string `json:"type"`
	Previous string `json:"previous_response_id"`
	Session  string `json:"prompt_cache_key"`
	Stream   bool   `json:"stream"`
	Generate *bool  `json:"generate"`
}

func ReadTurn(raw []byte) (Turn, error) {
	var turn Turn
	if err := json.Unmarshal(raw, &turn); err != nil || turn.Model == "" || len(turn.Model) > 128 ||
		len(turn.Previous) > 1024 || len(turn.Session) > 1024 {
		return Turn{}, errors.New("invalid Responses request or missing model")
	}
	return turn, nil
}

func Normalize(raw []byte, operation string, websocket bool, scope func(string) [32]byte) ([]byte, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, errors.New("invalid request object")
	}
	if cacheKey := value["prompt_cache_key"]; cacheKey != nil {
		var key string
		if json.Unmarshal(cacheKey, &key) == nil && key != "" {
			hash := scope(key)
			value["prompt_cache_key"], _ = json.Marshal(hex.EncodeToString(hash[:]))
		}
	}
	value["store"] = json.RawMessage("false")
	if operation == "compact" {
		delete(value, "stream")
	} else if !websocket {
		value["stream"] = json.RawMessage("true")
		delete(value, "generate")
	}
	var instructions string
	if json.Unmarshal(value["instructions"], &instructions) != nil || strings.TrimSpace(instructions) == "" {
		value["instructions"] = json.RawMessage(`"You are a helpful coding assistant."`)
	}
	return json.Marshal(value)
}

type Frame struct {
	Type     string `json:"type"`
	Status   int    `json:"status"`
	Response struct {
		ID    string  `json:"id"`
		Error Failure `json:"error"`
	} `json:"response"`
	Error Failure `json:"error"`
}

type Failure struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Scope   string `json:"scope"`
	Model   string `json:"model"`
	ResetAt int64  `json:"resets_at"`
	ResetIn int64  `json:"resets_in_seconds"`
}

func ParseFrame(raw []byte) Frame {
	var frame Frame
	_ = json.Unmarshal(raw, &frame)
	return frame
}

func (f Frame) Terminal() bool {
	switch f.Type {
	case "response.completed", "response.done", "response.incomplete", "response.failed", "response.cancelled", "error":
		return true
	}
	return false
}

func (f Frame) Rejection() (int, Failure) {
	if f.Type != "error" && f.Type != "response.failed" {
		return 0, Failure{}
	}
	failure := f.Error
	if failure == (Failure{}) {
		failure = f.Response.Error
	}
	if f.Status != 0 {
		return f.Status, failure
	}
	for _, kind := range []string{failure.Type, failure.Code} {
		switch kind {
		case "usage_limit_reached", "rate_limit_exceeded", "too_many_requests":
			return 429, failure
		case "invalid_token", "token_expired", "unauthorized":
			return 401, failure
		}
	}
	return 0, failure
}

func ParseFailure(raw []byte) Failure {
	var envelope struct {
		Error Failure `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)
	return envelope.Error
}

func RetryAt(f Failure, header http.Header, now time.Time) time.Time {
	if f.ResetAt > now.Unix() {
		return capTime(time.Unix(f.ResetAt, 0), now)
	}
	if f.ResetIn > 0 {
		return now.Add(time.Duration(min(f.ResetIn, 30*86400)) * time.Second)
	}
	raw := header.Get("Retry-After")
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		return now.Add(time.Duration(min(n, 30*86400)) * time.Second)
	}
	if t, err := http.ParseTime(raw); err == nil && t.After(now) {
		return capTime(t, now)
	}
	for _, name := range []string{"Primary", "Secondary"} {
		if percent, err := strconv.ParseFloat(header.Get("X-Codex-"+name+"-Used-Percent"), 64); err == nil && percent >= 100 {
			if reset, err := strconv.ParseInt(header.Get("X-Codex-"+name+"-Reset-At"), 10, 64); err == nil && reset > now.Unix() {
				return capTime(time.Unix(reset, 0), now)
			}
		}
	}
	return now.Add(30 * time.Second)
}

func capTime(t, now time.Time) time.Time {
	if maxTime := now.Add(30 * 24 * time.Hour); t.After(maxTime) {
		return maxTime
	}
	return t
}

func SSEData(event []byte) []byte {
	var fields [][]byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if prefix, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			fields = append(fields, bytes.TrimPrefix(prefix, []byte(" ")))
		}
	}
	return bytes.Join(fields, []byte("\n"))
}
