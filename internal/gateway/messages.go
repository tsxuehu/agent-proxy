package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	relay "github.com/tsxuehu/agent-proxy/api/relay/v1"
	"github.com/tsxuehu/agent-proxy/internal/codex"
	"github.com/tsxuehu/agent-proxy/internal/keys"
	"github.com/tsxuehu/agent-proxy/internal/storage"
)

type messageRequest struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
		Type        string          `json:"type"`
	} `json:"tools"`
	ToolChoice struct {
		Type            string `json:"type"`
		Name            string `json:"name"`
		DisableParallel bool   `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
	Stream    bool `json:"stream"`
	MaxTokens int  `json:"max_tokens"`
}

type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	UseID   string          `json:"tool_use_id"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

func blocks(input json.RawMessage) ([]contentBlock, error) {
	var simple string
	if json.Unmarshal(input, &simple) == nil {
		return []contentBlock{{Type: "text", Text: simple}}, nil
	}
	var list []contentBlock
	err := json.Unmarshal(input, &list)
	return list, err
}

func toResponses(req messageRequest, e storage.Endpoint) ([]byte, error) {
	model := e.Models[req.Model]
	if model == "" || len(req.Messages) == 0 {
		return nil, errors.New("model and messages must be available on this endpoint")
	}
	input := []map[string]any{}
	instructions := []string{}
	if len(req.System) > 0 {
		system, err := blocks(req.System)
		if err != nil {
			return nil, errors.New("invalid system content")
		}
		for _, block := range system {
			if block.Type != "text" {
				return nil, errors.New("unsupported system block")
			}
			instructions = append(instructions, block.Text)
		}
	}
	for _, message := range req.Messages {
		parts, err := blocks(message.Content)
		if err != nil || (message.Role != "user" && message.Role != "assistant") {
			return nil, errors.New("invalid message role or content")
		}
		var textParts []map[string]string
		flush := func() {
			if len(textParts) != 0 {
				input = append(input, map[string]any{"type": "message", "role": message.Role, "content": textParts})
				textParts = nil
			}
		}
		for _, block := range parts {
			switch block.Type {
			case "text":
				kind := "input_text"
				if message.Role == "assistant" {
					kind = "output_text"
				}
				textParts = append(textParts, map[string]string{"type": kind, "text": block.Text})
			case "tool_use":
				flush()
				if message.Role != "assistant" || block.ID == "" || block.Name == "" || len(block.Input) == 0 {
					return nil, errors.New("invalid tool_use block")
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": block.ID, "name": block.Name, "arguments": string(block.Input)})
			case "tool_result":
				flush()
				if message.Role != "user" || block.UseID == "" {
					return nil, errors.New("invalid tool_result block")
				}
				if len(block.Content) == 0 {
					block.Content = json.RawMessage(`""`)
				}
				result, err := blocks(block.Content)
				if err != nil {
					return nil, errors.New("unsupported tool_result content")
				}
				var output []string
				if block.IsError {
					output = append(output, "Tool error:")
				}
				for _, piece := range result {
					if piece.Type != "text" {
						return nil, errors.New("unsupported tool_result block")
					}
					output = append(output, piece.Text)
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": block.UseID, "output": strings.Join(output, "\n")})
			default:
				return nil, errors.New("unsupported Messages content block")
			}
		}
		flush()
	}
	payload := map[string]any{"model": model, "store": false, "stream": true, "input": input, "instructions": strings.Join(instructions, "\n\n")}
	if len(req.Tools) > 0 {
		tools := []map[string]any{}
		for _, tool := range req.Tools {
			if tool.Name == "" || len(tool.InputSchema) == 0 || (tool.Type != "" && tool.Type != "custom") {
				return nil, errors.New("only JSON-schema function tools are supported")
			}
			tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema, "strict": false})
		}
		payload["tools"] = tools
		payload["parallel_tool_calls"] = !req.ToolChoice.DisableParallel
	}
	switch req.ToolChoice.Type {
	case "":
	case "auto", "none":
		payload["tool_choice"] = req.ToolChoice.Type
	case "any":
		payload["tool_choice"] = "required"
	case "tool":
		if req.ToolChoice.Name == "" {
			return nil, errors.New("tool choice requires a name")
		}
		payload["tool_choice"] = map[string]string{"type": "function", "name": req.ToolChoice.Name}
	default:
		return nil, errors.New("unsupported tool choice")
	}
	return json.Marshal(payload)
}

func (s Server) readMessages(w http.ResponseWriter, r *http.Request) (messageRequest, storage.Endpoint, []byte, bool) {
	var request messageRequest
	e, failure := s.endpoint(r, "messages")
	if failure != 0 {
		messageError(w, failure, "endpoint unavailable")
		return request, e, nil, false
	}
	raw, err := readBody(w, r)
	if err != nil || json.Unmarshal(raw, &request) != nil {
		messageError(w, 400, "invalid Messages request")
		return request, e, nil, false
	}
	mapped, err := toResponses(request, e)
	if err != nil {
		messageError(w, 400, err.Error())
		return request, e, nil, false
	}
	return request, e, mapped, true
}

func (s Server) countTokens(w http.ResponseWriter, r *http.Request) {
	_, _, mapped, ok := s.readMessages(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]int{"input_tokens": int(math.Ceil(float64(utf8.RuneCount(mapped)) / 3))})
}

func (s Server) messages(w http.ResponseWriter, r *http.Request) {
	request, e, mapped, ok := s.readMessages(w, r)
	if !ok {
		return
	}
	if request.MaxTokens <= 0 {
		messageError(w, 400, "max_tokens must be positive")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	stream, err := s.open(ctx, r, e, "responses", false)
	if err != nil || sendTurn(stream, mapped) != nil || stream.CloseSend() != nil {
		messageError(w, 503, "upstream relay unavailable")
		return
	}
	translator := messageStream{out: w, id: keys.New("msg_"), model: request.Model, stream: request.Stream, indices: make(map[string]int)}
	for {
		event, err := stream.Recv()
		if err != nil {
			translator.fail(502, "upstream stream interrupted")
			return
		}
		switch part := event.Payload.(type) {
		case *relay.Event_Head:
			if part.Head.Status >= 400 {
				if retry := part.Head.Headers["Retry-After"]; retry != "" {
					w.Header().Set("Retry-After", retry)
				}
				translator.fail(int(part.Head.Status), http.StatusText(int(part.Head.Status)))
				return
			}
		case *relay.Event_Data:
			raw := codex.SSEData(part.Data)
			if len(raw) == 0 && json.Valid(part.Data) {
				raw = part.Data
			}
			if err := translator.process(raw); err != nil {
				translator.fail(502, "upstream Messages translation failed")
				return
			}
		case *relay.Event_Done:
			if !translator.done {
				translator.fail(502, "upstream stream ended without a terminal event")
			}
			return
		}
	}
}

type responseItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type responseObject struct {
	ID     string         `json:"id"`
	Output []responseItem `json:"output"`
	Usage  struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
}

type messageStream struct {
	out                         http.ResponseWriter
	id, model                   string
	stream, started, done, tool bool
	indices                     map[string]int
	populated                   map[string]bool
	nextIndex                   int
}

func (m *messageStream) process(raw []byte) error {
	if len(raw) == 0 || m.done {
		return nil
	}
	var ev struct {
		Type         string         `json:"type"`
		Item         responseItem   `json:"item"`
		ItemID       string         `json:"item_id"`
		Delta        string         `json:"delta"`
		ContentIndex int            `json:"content_index"`
		Response     responseObject `json:"response"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return err
	}
	switch ev.Type {
	case "response.failed", "response.cancelled", "error":
		m.fail(502, "upstream request failed")
		m.done = true
	case "response.output_text.delta":
		if m.stream {
			key := ev.ItemID + ":text:" + string(rune(ev.ContentIndex))
			if err := m.delta(key, map[string]any{"type": "text", "text": ""},
				map[string]any{"type": "text_delta", "text": ev.Delta}); err != nil {
				return err
			}
		}
	case "response.output_item.added":
		if ev.Item.Type == "function_call" {
			m.tool = true
			if m.stream {
				return m.openBlock(ev.Item.ID, map[string]any{"type": "tool_use", "id": ev.Item.CallID, "name": ev.Item.Name, "input": map[string]any{}})
			}
		}
	case "response.function_call_arguments.delta":
		if m.stream {
			if index, ok := m.indices[ev.ItemID]; ok {
				m.populated[ev.ItemID] = true
				return m.emit("content_block_delta", map[string]any{"index": index,
					"delta": map[string]string{"type": "input_json_delta", "partial_json": ev.Delta}})
			}
		}
	case "response.completed", "response.done", "response.incomplete":
		return m.finish(ev.Response, ev.Type == "response.incomplete")
	case "":
		var response responseObject
		if json.Unmarshal(raw, &response) == nil && response.ID != "" {
			return m.finish(response, false)
		}
	}
	return nil
}

func (m *messageStream) openBlock(key string, content map[string]any) error {
	if _, ok := m.indices[key]; ok {
		return nil
	}
	if !m.started {
		m.started = true
		m.populated = make(map[string]bool)
		m.out.Header().Set("Content-Type", "text/event-stream")
		m.out.Header().Set("Cache-Control", "no-cache")
		m.out.Header().Set("X-Accel-Buffering", "no")
		if err := m.emit("message_start", map[string]any{"message": m.message([]any{}, nil, 0, 0)}); err != nil {
			return err
		}
	}
	index := m.nextIndex
	m.nextIndex++
	m.indices[key] = index
	return m.emit("content_block_start", map[string]any{"index": index, "content_block": content})
}

func (m *messageStream) delta(key string, content, delta map[string]any) error {
	if err := m.openBlock(key, content); err != nil {
		return err
	}
	m.populated[key] = true
	return m.emit("content_block_delta", map[string]any{"index": m.indices[key], "delta": delta})
}

func (m *messageStream) finish(response responseObject, incomplete bool) error {
	list := []any{}
	for _, item := range response.Output {
		switch item.Type {
		case "message":
			for index, content := range item.Content {
				if content.Type != "output_text" {
					continue
				}
				list = append(list, map[string]any{"type": "text", "text": content.Text})
				key := item.ID + ":text:" + string(rune(index))
				if m.stream && !m.populated[key] {
					if err := m.delta(key, map[string]any{"type": "text", "text": ""}, map[string]any{"type": "text_delta", "text": content.Text}); err != nil {
						return err
					}
				}
			}
		case "function_call":
			m.tool = true
			input := json.RawMessage(item.Arguments)
			if !json.Valid(input) {
				input = json.RawMessage(`{}`)
			}
			list = append(list, map[string]any{"type": "tool_use", "id": item.CallID, "name": item.Name, "input": input})
			if m.stream && !m.populated[item.ID] {
				if err := m.delta(item.ID, map[string]any{"type": "tool_use", "id": item.CallID, "name": item.Name, "input": map[string]any{}},
					map[string]any{"type": "input_json_delta", "partial_json": string(input)}); err != nil {
					return err
				}
			}
		}
	}
	stop := "end_turn"
	if m.tool {
		stop = "tool_use"
	}
	if incomplete {
		stop = "max_tokens"
	}
	m.done = true
	if !m.stream {
		writeJSON(m.out, 200, m.message(list, stop, response.Usage.Input, response.Usage.Output))
		return nil
	}
	if !m.started {
		if err := m.openBlock("empty", map[string]any{"type": "text", "text": ""}); err != nil {
			return err
		}
	}
	for i := 0; i < m.nextIndex; i++ {
		if err := m.emit("content_block_stop", map[string]any{"index": i}); err != nil {
			return err
		}
	}
	if err := m.emit("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]int{"input_tokens": response.Usage.Input, "output_tokens": response.Usage.Output}}); err != nil {
		return err
	}
	return m.emit("message_stop", map[string]any{})
}

func (m *messageStream) message(content []any, stop any, input, output int) map[string]any {
	return map[string]any{"id": m.id, "type": "message", "role": "assistant", "model": m.model, "content": content,
		"stop_reason": stop, "stop_sequence": nil, "usage": map[string]int{"input_tokens": input, "output_tokens": output}}
}

func (m *messageStream) emit(kind string, event map[string]any) error {
	event["type"] = kind
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(m.out, "event: "+kind+"\ndata: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	return http.NewResponseController(m.out).Flush()
}

func (m *messageStream) fail(code int, message string) {
	if m.done {
		return
	}
	m.done = true
	if m.started {
		_ = m.emit("error", map[string]any{"error": map[string]string{"type": "api_error", "message": message}})
		return
	}
	messageError(m.out, code, message)
}

func messageError(w http.ResponseWriter, code int, message string) {
	kind := "api_error"
	switch code {
	case 400, 409:
		kind = "invalid_request_error"
	case 401:
		kind = "authentication_error"
	case 403:
		kind = "permission_error"
	case 429:
		kind = "rate_limit_error"
	case 503:
		kind = "overloaded_error"
	}
	writeJSON(w, code, map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
}
