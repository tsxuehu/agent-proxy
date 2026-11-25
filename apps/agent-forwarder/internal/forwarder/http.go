package forwarder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tsxuehu/agent-proxy/shared/codex"
	relay "github.com/tsxuehu/agent-proxy/shared/relay/v1"
	"github.com/tsxuehu/agent-proxy/shared/storage"
)

func (s *Server) httpAttempt(ctx context.Context, stream relayStream, request *relay.Open, turn codex.Turn, raw []byte, a storage.Account,
	credential codex.Credentials, observe func(codex.Frame) error) attemptResult {
	path := "/responses"
	if request.Operation == "compact" {
		path += "/compact"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return attemptResult{err: err}
	}
	req.Header = codex.Headers(request.Headers, a.ID, credential.Access)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	response, err := s.HTTP.Do(req)
	if err != nil {
		return attemptResult{err: err}
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return fromResponse(response)
	}
	s.observeHeaders(ctx, a.ID, response.Header)
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		raw, err := readLimited(response.Body, codex.MaxEvent)
		if err != nil {
			return attemptResult{err: err}
		}
		var result struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return attemptResult{err: err}
		}
		frame := codex.ParseFrame(raw)
		frame.Type = "response.completed"
		frame.Response.ID = result.ID
		if err := observe(frame); err != nil {
			return attemptResult{err: err}
		}
		if err := sendHead(stream, response.StatusCode, response.Header); err != nil {
			return attemptResult{err: err, started: true}
		}
		return attemptResult{err: sendData(stream, raw), started: true}
	}
	return readStream(stream, response, turn.Stream, observe)
}

func fromResponse(response *http.Response) attemptResult {
	raw, err := readLimited(response.Body, 128<<10)
	safe := response.StatusCode == 401 || response.StatusCode == 429
	if err != nil {
		raw = nil
	}
	return attemptResult{failure: response.StatusCode, header: response.Header, body: raw, safe: safe}
}

func readLimited(input io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err == nil && int64(len(data)) > limit {
		return nil, errors.New("upstream response exceeds limit")
	}
	return data, err
}

func readStream(stream relayStream, response *http.Response, streamOutput bool, observe func(codex.Frame) error) attemptResult {
	reader := bufio.NewReaderSize(response.Body, 32<<10)
	started := false
	committedUpstream := false
	for {
		event, err := readEvent(reader)
		if err != nil {
			return attemptResult{err: err, started: started}
		}
		data := codex.SSEData(event)
		if len(data) == 0 && !started {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			return attemptResult{err: errors.New("response stream ended without a terminal event"), started: started}
		}
		frame := codex.ParseFrame(data)
		code, failure := frame.Rejection()
		if !committedUpstream {
			if code == 429 || code == 401 {
				body, _ := json.Marshal(map[string]codex.Failure{"error": failure})
				return attemptResult{failure: code, header: response.Header, body: body, safe: true}
			}
		}
		committedUpstream = committedUpstream || frame.Type != ""
		if err := observe(frame); err != nil {
			return attemptResult{err: err, started: started}
		}
		if !streamOutput {
			if !frame.Terminal() {
				continue
			}
			if frame.Type != "response.completed" && frame.Type != "response.done" && frame.Type != "response.incomplete" {
				if code == 0 {
					code = 502
				}
				body, _ := json.Marshal(map[string]codex.Failure{"error": failure})
				return attemptResult{failure: code, header: response.Header, body: body}
			}
			var completed struct {
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(data, &completed) != nil || len(completed.Response) == 0 {
				return attemptResult{err: errors.New("upstream terminal response was empty")}
			}
			headers := response.Header.Clone()
			headers.Set("Content-Type", "application/json")
			if err := sendHead(stream, response.StatusCode, headers); err != nil {
				return attemptResult{err: err, started: true}
			}
			return attemptResult{err: sendData(stream, completed.Response), started: true}
		}
		if !started {
			started = true
			if err := sendHead(stream, response.StatusCode, response.Header); err != nil {
				return attemptResult{err: err, started: true}
			}
		}
		if err := sendData(stream, event); err != nil {
			return attemptResult{err: err, started: true}
		}
		if frame.Terminal() {
			body, _ := json.Marshal(map[string]codex.Failure{"error": failure})
			return attemptResult{started: true, failure: code, body: body, header: response.Header}
		}
	}
}

func readEvent(reader *bufio.Reader) ([]byte, error) {
	var event bytes.Buffer
	lineBytes := 0
	for {
		part, err := reader.ReadSlice('\n')
		if event.Len()+len(part) > codex.MaxEvent {
			return nil, errors.New("SSE event exceeds limit")
		}
		_, _ = event.Write(part)
		lineBytes += len(part)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if lineBytes == len(part) && (bytes.Equal(part, []byte("\n")) || bytes.Equal(part, []byte("\r\n"))) {
			return event.Bytes(), nil
		}
		lineBytes = 0
	}
}

func (s *Server) observeHeaders(ctx context.Context, id string, header http.Header) {
	var seen bool
	var peak float64
	now := time.Now()
	for _, name := range []string{"Primary", "Secondary"} {
		value, err := strconv.ParseFloat(header.Get("X-Codex-"+name+"-Used-Percent"), 64)
		if err == nil && value >= 0 && value <= 100 {
			reset, _ := strconv.ParseInt(header.Get("X-Codex-"+name+"-Reset-At"), 10, 64)
			if reset != 0 && reset <= now.Unix() {
				continue
			}
			seen = true
			peak = max(peak, value/100)
			if value >= 100 && reset > now.Unix() {
				_ = s.Store.Cool(ctx, id, "", capReset(time.Unix(reset, 0), now))
			}
		}
	}
	if seen {
		_ = s.Store.ObserveQuota(ctx, id, peak, now)
	}
}
