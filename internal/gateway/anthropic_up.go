package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const defaultAnthropicMaxTokens = 4096

// callAnthropic sends a translated /v1/messages request to an Anthropic-compatible
// upstream server.
func (g *Gateway) callAnthropic(w http.ResponseWriter, r *http.Request, info *reqInfo, areq map[string]any) (*http.Response, error) {
	return g.postUpstreamJSON(w, r, info, "/v1/messages", areq)
}

// forwardAnthropicMessages sends areq to /v1/messages and passes non-200
// replies straight through. When ok is true, the caller must close resp.Body.
func (g *Gateway) forwardAnthropicMessages(w http.ResponseWriter, r *http.Request, info *reqInfo, areq map[string]any) (*http.Response, bool) {
	resp, err := g.callAnthropic(w, r, info, areq)
	if err != nil {
		return nil, false
	}
	g.stampHeaders(w.Header(), info)
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		resp.Body.Close()
		return nil, false
	}
	return resp, true
}

// serveAnthropicCountTokens forwards /v1/messages/count_tokens to an
// Anthropic-compatible upstream server, falling back to the local estimate
// when the server's answer isn't a count.
func (g *Gateway) serveAnthropicCountTokens(w http.ResponseWriter, r *http.Request, info *reqInfo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The request body could not be read.")
		return
	}
	u := *info.t.url
	u.Path = "/v1/messages/count_tokens"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The request could not be built.")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for _, hdr := range []string{"Anthropic-Version", "Anthropic-Beta"} {
		if v := r.Header.Get(hdr); v != "" {
			req.Header.Set(hdr, v)
		}
	}
	g.setUpstreamAuth(req.Header, info.t)
	if info.t.host != "" {
		req.Host = info.t.host
	}
	resp, err := g.transport.RoundTrip(req)
	if err != nil {
		g.errorHandler(w, r, err)
		return
	}
	defer resp.Body.Close()
	// The server's answer is used only when it is a count. Servers without the
	// endpoint answer 404 or 501, and some answer 200 with an error body (seen
	// live on LM Studio), so everything else gets the local estimate.
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxCountReply))
	// A reply cut off at the cap is no longer valid JSON, so isTokenCount
	// refuses it too.
	if err != nil || resp.StatusCode != http.StatusOK || !isTokenCount(reply) {
		r.Body = io.NopCloser(bytes.NewReader(body))
		g.serveCountTokens(w, r, info)
		return
	}

	info.ttfb = time.Since(info.start)
	g.stampHeaders(w.Header(), info)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(reply)
}

// maxCountReply caps how much of a count_tokens reply is read; a real one is
// a few dozen bytes.
const maxCountReply = 64 << 10

// isTokenCount reports whether b is a count_tokens reply: a JSON object with
// a whole, non-negative input_tokens.
func isTokenCount(b []byte) bool {
	var v struct {
		InputTokens *int64 `json:"input_tokens"`
	}
	if json.Unmarshal(b, &v) != nil || v.InputTokens == nil {
		return false
	}
	return *v.InputTokens >= 0
}

// ---- OpenAI /v1/chat/completions <-> Anthropic /v1/messages ----

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMsg       `json:"messages"`
	Tools               []chatTool      `json:"tools"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stream              bool            `json:"stream"`
	Stop                json.RawMessage `json:"stop"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type chatMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type anthropicOutMsg struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

func (g *Gateway) serveChatViaAnthropic(w http.ResponseWriter, r *http.Request, info *reqInfo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The request body could not be read.")
		return
	}
	var creq chatRequest
	if err := json.Unmarshal(body, &creq); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The /v1/chat/completions body is not valid JSON.")
		return
	}
	resp, ok := g.forwardAnthropicMessages(w, r, info, chatToAnthropic(creq))
	if !ok {
		return
	}
	defer resp.Body.Close()

	if !creq.Stream {
		translated, err := anthropicRespToChatBytes(resp.Body, creq.Model, info.id)
		if err != nil {
			writeError(w, http.StatusBadGateway, "bad_engine_reply", "The engine returned invalid JSON.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(translated)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	info.noteUsage(translateAnthropicStreamToChat(w, flusher, resp.Body, creq.Model, info.id, creq.StreamOptions != nil && creq.StreamOptions.IncludeUsage))
}

// serveResponsesViaAnthropic reuses the gateway's Responses <-> chat structures
// so /v1/responses works against an Anthropic upstream without a second full
// translator.
func (g *Gateway) serveResponsesViaAnthropic(w http.ResponseWriter, r *http.Request, info *reqInfo) {
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
	chatBytes, err := json.Marshal(responsesToChat(rreq))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The /v1/responses body could not be converted.")
		return
	}
	var creq chatRequest
	if err := json.Unmarshal(chatBytes, &creq); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The /v1/responses body could not be converted.")
		return
	}
	resp, ok := g.forwardAnthropicMessages(w, r, info, chatToAnthropic(creq))
	if !ok {
		return
	}
	defer resp.Body.Close()

	if !rreq.Stream {
		translated, err := anthropicRespToChatBytes(resp.Body, rreq.Model, info.id)
		if err != nil {
			writeError(w, http.StatusBadGateway, "bad_engine_reply", "The engine returned invalid JSON.")
			return
		}
		g.writeResponsesNonStream(w, bytes.NewReader(translated), rreq.Model, info.id)
		return
	}
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer pw.Close()
		translateAnthropicStreamToChat(pw, nil, resp.Body, rreq.Model, info.id, true)
	}()
	g.writeResponsesStream(w, pr, rreq.Model, info.id)
	_ = pr.Close()
	_ = resp.Body.Close()
	wg.Wait()
}

func chatToAnthropic(creq chatRequest) map[string]any {
	var systemParts []string
	var messages []anthropicOutMsg
	appendMsg := func(role string, blocks ...map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if len(messages) > 0 && messages[len(messages)-1].Role == role {
			messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, blocks...)
			return
		}
		messages = append(messages, anthropicOutMsg{Role: role, Content: blocks})
	}

	for _, m := range creq.Messages {
		switch m.Role {
		case "system", "developer":
			if text := extractText(m.Content); text != "" {
				systemParts = append(systemParts, text)
			}
		case "tool":
			appendMsg("user", map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     extractText(m.Content),
			})
		case "assistant":
			var blocks []map[string]any
			if text := extractText(m.Content); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			for i, tc := range m.ToolCalls {
				var input map[string]any
				if tc.Function.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
				}
				if input == nil {
					input = map[string]any{}
				}
				id := tc.ID
				if id == "" {
					id = fmt.Sprintf("toolu_req_%d", i)
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    id,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			appendMsg("assistant", blocks...)
		default:
			if text := extractText(m.Content); text != "" {
				appendMsg("user", map[string]any{"type": "text", "text": text})
			}
		}
	}
	if len(messages) == 0 {
		appendMsg("user", map[string]any{"type": "text", "text": ""})
	}

	maxTokens := creq.MaxTokens
	if maxTokens <= 0 {
		maxTokens = creq.MaxCompletionTokens
	}
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}

	out := map[string]any{
		"model":      creq.Model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     creq.Stream,
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if creq.Temperature != nil {
		out["temperature"] = *creq.Temperature
	}
	if creq.TopP != nil {
		out["top_p"] = *creq.TopP
	}
	if seqs := parseStopSequences(creq.Stop); len(seqs) > 0 {
		out["stop_sequences"] = seqs
	}
	if len(creq.Tools) > 0 {
		var tools []map[string]any
		for _, t := range creq.Tools {
			if t.Type != "" && t.Type != "function" {
				continue
			}
			if t.Function.Name == "" {
				continue
			}
			schema := map[string]any{"type": "object"}
			if len(t.Function.Parameters) > 0 {
				var parsed map[string]any
				if json.Unmarshal(t.Function.Parameters, &parsed) == nil && parsed != nil {
					schema = parsed
				}
			}
			tool := map[string]any{
				"name":         t.Function.Name,
				"input_schema": schema,
			}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			tools = append(tools, tool)
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	return out
}

func parseStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s != "" {
			return []string{s}
		}
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		var out []string
		for _, item := range list {
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	}
	return nil
}

// anthropicToChatFinish maps Anthropic's stop_reason back to OpenAI's
// finish_reason so callers (and Responses translation) detect token limits,
// content refusals, and tool calls accurately.
func anthropicToChatFinish(stopReason string, hasTools bool) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	if hasTools {
		return "tool_calls"
	}
	return "stop"
}

type anthropicResp struct {
	ID         string `json:"id"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func anthropicRespToChatBytes(body io.Reader, reqModel, reqID string) ([]byte, error) {
	var aresp anthropicResp
	if err := json.NewDecoder(body).Decode(&aresp); err != nil {
		return nil, err
	}
	id := aresp.ID
	if id == "" {
		id = "chatcmpl_" + reqID
	}
	model := aresp.Model
	if model == "" {
		model = reqModel
	}
	var texts, thinkings []string
	var toolCalls []map[string]any
	for i, b := range aresp.Content {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "thinking":
			thinkings = append(thinkings, b.Thinking)
		case "tool_use":
			tcID := b.ID
			if tcID == "" {
				tcID = fmt.Sprintf("call_%s_%d", reqID, i)
			}
			args := "{}"
			if trimmed := strings.TrimSpace(string(b.Input)); trimmed != "" && trimmed != "null" {
				args = trimmed
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   tcID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
		}
	}
	msg := map[string]any{
		"role":    "assistant",
		"content": strings.Join(texts, ""),
	}
	if len(thinkings) > 0 {
		msg["reasoning_content"] = strings.Join(thinkings, "")
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	finish := anthropicToChatFinish(aresp.StopReason, len(toolCalls) > 0)
	return json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     aresp.Usage.InputTokens,
			"completion_tokens": aresp.Usage.OutputTokens,
			"total_tokens":      aresp.Usage.InputTokens + aresp.Usage.OutputTokens,
		},
	})
}

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		ID    string      `json:"id"`
		Model string      `json:"model"`
		Usage usageCounts `json:"usage"`
	} `json:"message"`
	Usage        usageCounts `json:"usage"` // message_delta's
	ContentBlock struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
}

type streamToolBlock struct {
	toolIdx    int
	sawArgs    bool
	inlineArgs string
}

// With includeUsage (the client's stream_options.include_usage), a last chunk
// carries the token counts, as OpenAI's API does. They are returned either
// way, for the gateway's log.
func translateAnthropicStreamToChat(w io.Writer, flusher http.Flusher, body io.Reader, reqModel, reqID string, includeUsage bool) (tokensIn, tokensOut int) {
	msgID := "chatcmpl_" + reqID
	model := reqModel
	created := time.Now().Unix()
	started := false
	stopReason := ""
	nextToolIdx := 0
	toolBlocks := map[int]*streamToolBlock{}
	writeFailed := false

	emit := func(delta any, finish *string) {
		if writeFailed {
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"id":      msgID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		})
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			writeFailed = true
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	ensureStarted := func() {
		if !started {
			started = true
			emit(map[string]any{"role": "assistant", "content": ""}, nil)
		}
	}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	done := false
	for !done && !writeFailed && sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev anthropicStreamEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			tokensIn = max(tokensIn, ev.Message.Usage.in())
			tokensOut = max(tokensOut, ev.Message.Usage.out())
			if ev.Message.ID != "" {
				msgID = ev.Message.ID
			}
			if ev.Message.Model != "" {
				model = ev.Message.Model
			}
			ensureStarted()
		case "content_block_start":
			ensureStarted()
			switch ev.ContentBlock.Type {
			case "text":
				if ev.ContentBlock.Text != "" {
					emit(map[string]any{"content": ev.ContentBlock.Text}, nil)
				}
			case "thinking":
				if ev.ContentBlock.Thinking != "" {
					emit(map[string]any{"reasoning_content": ev.ContentBlock.Thinking}, nil)
				}
			case "tool_use":
				id := ev.ContentBlock.ID
				if id == "" {
					id = fmt.Sprintf("call_%s_%d", reqID, nextToolIdx)
				}
				inline := strings.TrimSpace(string(ev.ContentBlock.Input))
				if inline == "null" || inline == "{}" {
					inline = ""
				}
				tb := &streamToolBlock{toolIdx: nextToolIdx, inlineArgs: inline}
				toolBlocks[ev.Index] = tb
				nextToolIdx++
				emit(map[string]any{
					"tool_calls": []map[string]any{{
						"index": tb.toolIdx,
						"id":    id,
						"type":  "function",
						"function": map[string]any{
							"name":      ev.ContentBlock.Name,
							"arguments": "",
						},
					}},
				}, nil)
			}
		case "content_block_delta":
			ensureStarted()
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					emit(map[string]any{"content": ev.Delta.Text}, nil)
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					emit(map[string]any{"reasoning_content": ev.Delta.Thinking}, nil)
				}
			case "input_json_delta":
				if tb, ok := toolBlocks[ev.Index]; ok && ev.Delta.PartialJSON != "" {
					tb.sawArgs = true
					emit(map[string]any{
						"tool_calls": []map[string]any{{
							"index": tb.toolIdx,
							"function": map[string]any{
								"arguments": ev.Delta.PartialJSON,
							},
						}},
					}, nil)
				}
			}
		case "content_block_stop":
			if tb, ok := toolBlocks[ev.Index]; ok && !tb.sawArgs {
				args := tb.inlineArgs
				if args == "" {
					args = "{}"
				}
				tb.sawArgs = true
				emit(map[string]any{
					"tool_calls": []map[string]any{{
						"index": tb.toolIdx,
						"function": map[string]any{
							"arguments": args,
						},
					}},
				}, nil)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			tokensIn = max(tokensIn, ev.Usage.in())
			tokensOut = max(tokensOut, ev.Usage.out())
		case "message_stop":
			done = true
		}
	}
	if !writeFailed {
		ensureStarted()
		finish := anthropicToChatFinish(stopReason, nextToolIdx > 0)
		emit(map[string]any{}, &finish)
		if includeUsage && !writeFailed {
			payload, _ := json.Marshal(map[string]any{
				"id": msgID, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{},
				"usage":   map[string]any{"prompt_tokens": tokensIn, "completion_tokens": tokensOut, "total_tokens": tokensIn + tokensOut},
			})
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				writeFailed = true
			}
		}
		if !writeFailed {
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	return tokensIn, tokensOut
}
