package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// serveCountTokens answers Anthropic's /v1/messages/count_tokens locally so
// harnesses such as Claude Code work against OpenAI-compatible engines that
// don't have an Anthropic token-counting route.
func (g *Gateway) serveCountTokens(w http.ResponseWriter, r *http.Request, info *reqInfo) {
	body, _ := io.ReadAll(r.Body)
	tokens := len(body) / 4
	if tokens < 1 {
		tokens = 1
	}
	info.ttfb = time.Since(info.start)
	g.stampHeaders(w.Header(), info)
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}

func (g *Gateway) stampHeaders(h http.Header, info *reqInfo) {
	for name := range h {
		if strings.HasPrefix(name, "X-Ezbx-") {
			delete(h, name)
		}
	}
	h.Set("X-Ezbx-Engine", info.t.engine)
	h.Set("X-Ezbx-Request-Id", info.id)
	if info.model != "" {
		h.Set("X-Ezbx-Model", info.model)
	}
}

// callChat sends a translated /v1/chat/completions request to the upstream
// engine using the gateway's transport, key, host header and idle timer.
func (g *Gateway) callChat(w http.ResponseWriter, r *http.Request, info *reqInfo, chatReq map[string]any) (*http.Response, error) {
	payload, err := json.Marshal(chatReq)
	if err != nil {
		return nil, err
	}
	u := *info.t.url
	u.Path = "/v1/chat/completions"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.key != "" {
		req.Header.Set("Authorization", "Bearer "+g.key)
	}
	if info.t.host != "" {
		req.Host = info.t.host
	}
	resp, err := g.transport.RoundTrip(req)
	if err != nil {
		g.errorHandler(w, r, err)
		return nil, err
	}
	info.ttfb = time.Since(info.start)
	resp.Body = newIdleBody(resp.Body, g.idle, func() {
		info.idleHit.Store(true)
		info.cancel()
	})
	return resp, nil
}

// ---- Anthropic /v1/messages <-> OpenAI /v1/chat/completions ----

type anthropicRequest struct {
	Model       string          `json:"model"`
	System      json.RawMessage `json:"system"`
	Messages    []anthropicMsg  `json:"messages"`
	Tools       []anthropicTool `json:"tools"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stream      bool            `json:"stream"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

func (g *Gateway) serveMessages(w http.ResponseWriter, r *http.Request, info *reqInfo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The request body could not be read.")
		return
	}
	var areq anthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The /v1/messages body is not valid JSON.")
		return
	}
	chatReq := anthropicToChat(areq)
	resp, err := g.callChat(w, r, info, chatReq)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	g.stampHeaders(w.Header(), info)
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if !areq.Stream {
		g.writeAnthropicNonStream(w, resp.Body, areq.Model, info.id)
		return
	}
	g.writeAnthropicStream(w, resp.Body, areq.Model, info.id)
}

func anthropicToChat(areq anthropicRequest) map[string]any {
	var msgs []map[string]any
	if sys := extractText(areq.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range areq.Messages {
		var plain string
		if json.Unmarshal(m.Content, &plain) == nil {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": plain})
			continue
		}
		var blocks []anthropicBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		var texts []string
		var toolCalls []map[string]any
		var toolResults []map[string]any
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					texts = append(texts, b.Text)
				}
			case "tool_use":
				args := "{}"
				if len(b.Input) > 0 {
					args = string(b.Input)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   b.ID,
					"type": "function",
					"function": map[string]any{
						"name":      b.Name,
						"arguments": args,
					},
				})
			case "tool_result":
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": b.ToolUseID,
					"content":      extractText(b.Content),
				})
			}
		}
		if len(texts) > 0 || len(toolCalls) > 0 {
			msg := map[string]any{"role": m.Role, "content": strings.Join(texts, "\n")}
			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
			}
			msgs = append(msgs, msg)
		}
		for _, tr := range toolResults {
			msgs = append(msgs, tr)
		}
	}
	out := map[string]any{
		"model":    areq.Model,
		"messages": msgs,
		"stream":   areq.Stream,
	}
	if areq.MaxTokens > 0 {
		out["max_tokens"] = areq.MaxTokens
	}
	if areq.Temperature != nil {
		out["temperature"] = *areq.Temperature
	}
	if areq.TopP != nil {
		out["top_p"] = *areq.TopP
	}
	if len(areq.Tools) > 0 {
		var tools []map[string]any
		for _, t := range areq.Tools {
			params := any(map[string]any{"type": "object"})
			if len(t.InputSchema) > 0 {
				_ = json.Unmarshal(t.InputSchema, &params)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
		out["tools"] = tools
	}
	return out
}

func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

type chatCompletionResp struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func (g *Gateway) writeAnthropicNonStream(w http.ResponseWriter, body io.Reader, reqModel, reqID string) {
	var cresp chatCompletionResp
	if err := json.NewDecoder(body).Decode(&cresp); err != nil {
		writeError(w, http.StatusBadGateway, "bad_engine_reply", "The engine returned invalid JSON.")
		return
	}
	model := cresp.Model
	if model == "" {
		model = reqModel
	}
	var content []map[string]any
	stopReason := "end_turn"
	if len(cresp.Choices) > 0 {
		msg := cresp.Choices[0].Message
		if msg.Content != "" || len(msg.ToolCalls) == 0 {
			content = append(content, map[string]any{"type": "text", "text": msg.Content})
		}
		for i, tc := range msg.ToolCalls {
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("toolu_%s_%d", reqID, i)
			}
			var input any = map[string]any{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  tc.Function.Name,
				"input": input,
			})
			stopReason = "tool_use"
		}
	}
	if len(content) == 0 {
		content = []map[string]any{{"type": "text", "text": ""}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            "msg_" + reqID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  cresp.Usage.PromptTokens,
			"output_tokens": cresp.Usage.CompletionTokens,
		},
	})
}

type chatStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

type accToolCall struct {
	id   string
	name string
	args strings.Builder
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if flusher != nil {
		flusher.Flush()
	}
}

func (g *Gateway) writeAnthropicStream(w http.ResponseWriter, body io.Reader, reqModel, reqID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeSSE(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            "msg_" + reqID,
			"type":          "message",
			"role":          "assistant",
			"model":         reqModel,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 1, "output_tokens": 0},
		},
	})

	textStarted := false
	nextIndex := 0
	var tools []*accToolCall
	toolByIndex := map[int]*accToolCall{}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk chatStreamChunk
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			if !textStarted {
				textStarted = true
				writeSSE(w, flusher, "content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         nextIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				})
			}
			writeSSE(w, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": nextIndex,
				"delta": map[string]any{"type": "text_delta", "text": delta.Content},
			})
		}
		for _, tc := range delta.ToolCalls {
			acc, ok := toolByIndex[tc.Index]
			if !ok {
				acc = &accToolCall{id: tc.ID, name: tc.Function.Name}
				toolByIndex[tc.Index] = acc
				tools = append(tools, acc)
			}
			if tc.ID != "" && acc.id == "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" && acc.name == "" {
				acc.name = tc.Function.Name
			}
			acc.args.WriteString(tc.Function.Arguments)
		}
	}
	if !textStarted && len(tools) == 0 {
		textStarted = true
		writeSSE(w, flusher, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         nextIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	if textStarted {
		writeSSE(w, flusher, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": nextIndex,
		})
		nextIndex++
	}
	stopReason := "end_turn"
	for i, tc := range tools {
		stopReason = "tool_use"
		id := tc.id
		if id == "" {
			id = fmt.Sprintf("toolu_%s_%d", reqID, i)
		}
		args := tc.args.String()
		if args == "" {
			args = "{}"
		}
		writeSSE(w, flusher, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": nextIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  tc.name,
				"input": map[string]any{},
			},
		})
		writeSSE(w, flusher, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": nextIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
		writeSSE(w, flusher, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": nextIndex,
		})
		nextIndex++
	}
	writeSSE(w, flusher, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 1},
	})
	writeSSE(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
}

// ---- OpenAI /v1/responses <-> OpenAI /v1/chat/completions ----

type responsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions"`
	Input           json.RawMessage `json:"input"`
	Tools           []responsesTool `json:"tools"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stream          bool            `json:"stream"`
}

type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Function    *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func (g *Gateway) serveResponses(w http.ResponseWriter, r *http.Request, info *reqInfo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The request body could not be read.")
		return
	}
	var rreq responsesRequest
	if err := json.Unmarshal(body, &rreq); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The /v1/responses body is not valid JSON.")
		return
	}
	chatReq := responsesToChat(rreq)
	resp, err := g.callChat(w, r, info, chatReq)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	g.stampHeaders(w.Header(), info)
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if !rreq.Stream {
		g.writeResponsesNonStream(w, resp.Body, rreq.Model, info.id)
		return
	}
	g.writeResponsesStream(w, resp.Body, rreq.Model, info.id)
}

func responsesToChat(rreq responsesRequest) map[string]any {
	var msgs []map[string]any
	if rreq.Instructions != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": rreq.Instructions})
	}
	var plain string
	if json.Unmarshal(rreq.Input, &plain) == nil {
		msgs = append(msgs, map[string]any{"role": "user", "content": plain})
	} else {
		var items []responsesItem
		_ = json.Unmarshal(rreq.Input, &items)
		for _, it := range items {
			switch it.Type {
			case "function_call":
				args := it.Arguments
				if args == "" {
					args = "{}"
				}
				msgs = append(msgs, map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []map[string]any{{
						"id":   it.CallID,
						"type": "function",
						"function": map[string]any{
							"name":      it.Name,
							"arguments": args,
						},
					}},
				})
			case "function_call_output":
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": it.CallID,
					"content":      extractText(it.Output),
				})
			default:
				role := it.Role
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system"
				}
				msgs = append(msgs, map[string]any{
					"role":    role,
					"content": extractText(it.Content),
				})
			}
		}
	}
	if len(msgs) == 0 {
		msgs = []map[string]any{{"role": "user", "content": ""}}
	}
	out := map[string]any{
		"model":    rreq.Model,
		"messages": msgs,
		"stream":   rreq.Stream,
	}
	if rreq.MaxOutputTokens > 0 {
		out["max_tokens"] = rreq.MaxOutputTokens
	}
	if rreq.Temperature != nil {
		out["temperature"] = *rreq.Temperature
	}
	if rreq.TopP != nil {
		out["top_p"] = *rreq.TopP
	}
	if len(rreq.Tools) > 0 {
		var tools []map[string]any
		for _, t := range rreq.Tools {
			if t.Type != "" && t.Type != "function" {
				continue
			}
			name, desc, rawParams := t.Name, t.Description, t.Parameters
			if t.Function != nil {
				name, desc, rawParams = t.Function.Name, t.Function.Description, t.Function.Parameters
			}
			if name == "" {
				continue
			}
			params := any(map[string]any{"type": "object"})
			if len(rawParams) > 0 {
				_ = json.Unmarshal(rawParams, &params)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": desc,
					"parameters":  params,
				},
			})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	return out
}

func buildResponsesObject(reqID, status, model, text string, tools []*accToolCall, promptTokens, completionTokens int) map[string]any {
	var output []map[string]any
	if text != "" || len(tools) == 0 {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     "msg_" + reqID,
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		})
	}
	for i, tc := range tools {
		callID := tc.id
		if callID == "" {
			callID = fmt.Sprintf("call_%s_%d", reqID, i)
		}
		args := tc.args.String()
		if args == "" {
			args = "{}"
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        fmt.Sprintf("fc_%s_%d", reqID, i),
			"call_id":   callID,
			"name":      tc.name,
			"arguments": args,
			"status":    "completed",
		})
	}
	if status != "completed" {
		output = []map[string]any{}
	}
	return map[string]any{
		"id":         "resp_" + reqID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      model,
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  promptTokens,
			"output_tokens": completionTokens,
			"total_tokens":  promptTokens + completionTokens,
		},
	}
}

func (g *Gateway) writeResponsesNonStream(w http.ResponseWriter, body io.Reader, reqModel, reqID string) {
	var cresp chatCompletionResp
	if err := json.NewDecoder(body).Decode(&cresp); err != nil {
		writeError(w, http.StatusBadGateway, "bad_engine_reply", "The engine returned invalid JSON.")
		return
	}
	model := cresp.Model
	if model == "" {
		model = reqModel
	}
	text := ""
	var tools []*accToolCall
	if len(cresp.Choices) > 0 {
		msg := cresp.Choices[0].Message
		text = msg.Content
		for _, tc := range msg.ToolCalls {
			acc := &accToolCall{id: tc.ID, name: tc.Function.Name}
			acc.args.WriteString(tc.Function.Arguments)
			tools = append(tools, acc)
		}
	}
	writeJSON(w, http.StatusOK, buildResponsesObject(reqID, "completed", model, text, tools, cresp.Usage.PromptTokens, cresp.Usage.CompletionTokens))
}

func (g *Gateway) writeResponsesStream(w http.ResponseWriter, body io.Reader, reqModel, reqID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	seq := 0
	emit := func(event string, payload map[string]any) {
		payload["type"] = event
		payload["sequence_number"] = seq
		seq++
		writeSSE(w, flusher, event, payload)
	}

	emit("response.created", map[string]any{
		"response": buildResponsesObject(reqID, "in_progress", reqModel, "", nil, 0, 0),
	})
	emit("response.in_progress", map[string]any{
		"response": buildResponsesObject(reqID, "in_progress", reqModel, "", nil, 0, 0),
	})

	msgID := "msg_" + reqID
	textStarted := false
	var fullText strings.Builder
	var tools []*accToolCall
	toolByIndex := map[int]*accToolCall{}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk chatStreamChunk
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			if !textStarted {
				textStarted = true
				emit("response.output_item.added", map[string]any{
					"output_index": 0,
					"item": map[string]any{
						"type":    "message",
						"id":      msgID,
						"status":  "in_progress",
						"role":    "assistant",
						"content": []any{},
					},
				})
				emit("response.content_part.added", map[string]any{
					"item_id":       msgID,
					"output_index":  0,
					"content_index": 0,
					"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				})
			}
			fullText.WriteString(delta.Content)
			emit("response.output_text.delta", map[string]any{
				"item_id":       msgID,
				"output_index":  0,
				"content_index": 0,
				"delta":         delta.Content,
			})
		}
		for _, tc := range delta.ToolCalls {
			acc, ok := toolByIndex[tc.Index]
			if !ok {
				acc = &accToolCall{id: tc.ID, name: tc.Function.Name}
				toolByIndex[tc.Index] = acc
				tools = append(tools, acc)
			}
			if tc.ID != "" && acc.id == "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" && acc.name == "" {
				acc.name = tc.Function.Name
			}
			acc.args.WriteString(tc.Function.Arguments)
		}
	}
	outIndex := 0
	if textStarted || len(tools) == 0 {
		txt := fullText.String()
		if !textStarted {
			emit("response.output_item.added", map[string]any{
				"output_index": 0,
				"item": map[string]any{
					"type":    "message",
					"id":      msgID,
					"status":  "in_progress",
					"role":    "assistant",
					"content": []any{},
				},
			})
			emit("response.content_part.added", map[string]any{
				"item_id":       msgID,
				"output_index":  0,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
		}
		emit("response.output_text.done", map[string]any{
			"item_id":       msgID,
			"output_index":  0,
			"content_index": 0,
			"text":          txt,
		})
		emit("response.content_part.done", map[string]any{
			"item_id":       msgID,
			"output_index":  0,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": txt, "annotations": []any{}},
		})
		emit("response.output_item.done", map[string]any{
			"output_index": 0,
			"item": map[string]any{
				"type":   "message",
				"id":     msgID,
				"status": "completed",
				"role":   "assistant",
				"content": []map[string]any{{
					"type":        "output_text",
					"text":        txt,
					"annotations": []any{},
				}},
			},
		})
		outIndex++
	}
	for i, tc := range tools {
		callID := tc.id
		if callID == "" {
			callID = fmt.Sprintf("call_%s_%d", reqID, i)
		}
		itemID := fmt.Sprintf("fc_%s_%d", reqID, i)
		args := tc.args.String()
		if args == "" {
			args = "{}"
		}
		emit("response.output_item.added", map[string]any{
			"output_index": outIndex,
			"item": map[string]any{
				"type":      "function_call",
				"id":        itemID,
				"call_id":   callID,
				"name":      tc.name,
				"arguments": "",
				"status":    "in_progress",
			},
		})
		emit("response.function_call_arguments.delta", map[string]any{
			"item_id":      itemID,
			"output_index": outIndex,
			"delta":        args,
		})
		emit("response.function_call_arguments.done", map[string]any{
			"item_id":      itemID,
			"output_index": outIndex,
			"arguments":    args,
		})
		emit("response.output_item.done", map[string]any{
			"output_index": outIndex,
			"item": map[string]any{
				"type":      "function_call",
				"id":        itemID,
				"call_id":   callID,
				"name":      tc.name,
				"arguments": args,
				"status":    "completed",
			},
		})
		outIndex++
	}
	emit("response.completed", map[string]any{
		"response": buildResponsesObject(reqID, "completed", reqModel, fullText.String(), tools, 1, 1),
	})
}
