package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func startAnthropic(t *testing.T, upstream, key string, log io.Writer) (*Gateway, *httptest.Server) {
	t.Helper()
	g, err := New(Config{Upstream: upstream, EngineID: "anthropic", Key: key, Log: log, MaxBody: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

func TestAnthropicPassthroughAndHeaders(t *testing.T) {
	type seenReq struct {
		path, auth, xKey, version, beta string
	}
	var seen []seenReq
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, seenReq{
			path:    r.URL.Path,
			auth:    r.Header.Get("Authorization"),
			xKey:    r.Header.Get("X-Api-Key"),
			version: r.Header.Get("Anthropic-Version"),
			beta:    r.Header.Get("Anthropic-Beta"),
		})
		w.Header().Set("X-Ezbx-Engine", "spoofed")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	t.Cleanup(up.Close)
	_, gw := startAnthropic(t, up.URL, "real-anth-key", nil)

	// 1. Default Anthropic-Version added when absent; box keys stripped and replaced.
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{"model":"big-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer box-placeholder")
	req.Header.Set("X-Api-Key", "box-placeholder")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Ezbx-Engine") != "anthropic" || resp.Header.Get("X-Ezbx-Model") != "big-model" {
		t.Fatalf("unexpected headers: %v", resp.Header)
	}
	if len(seen) != 1 || seen[0].path != "/v1/messages" || seen[0].auth != "" || seen[0].xKey != "real-anth-key" || seen[0].version != anthropicVersion {
		t.Fatalf("upstream saw: %+v", seen)
	}

	// 2. Client-supplied Anthropic-Version and Anthropic-Beta are preserved.
	req, _ = http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{"model":"big-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Anthropic-Version", "2024-06-01")
	req.Header.Set("Anthropic-Beta", "tools-2024-04-04")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(seen) != 2 || seen[1].version != "2024-06-01" || seen[1].beta != "tools-2024-04-04" {
		t.Fatalf("client headers not preserved: %+v", seen)
	}
}

func TestAnthropicPassthroughStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(up.Close)
	_, gw := startAnthropic(t, up.URL, "", nil)

	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 2 * time.Second}}
	resp, err := client.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"big-model","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		close(release)
		t.Fatalf("gateway buffered response headers: %v", err)
	}
	defer resp.Body.Close()

	first := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "event: message_start\n" {
			t.Fatalf("first line = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first chunk buffered")
	}
	close(release)
}

func TestAnthropicCountTokensAndReady(t *testing.T) {
	statusCount := http.StatusOK
	var gotBeta, gotVer string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "real-key" || r.Header.Get("Anthropic-Version") == "" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{{"id": "big-model"}}, "has_more": false})
		case "/v1/messages/count_tokens":
			gotBeta = r.Header.Get("Anthropic-Beta")
			gotVer = r.Header.Get("Anthropic-Version")
			if statusCount != http.StatusOK {
				writeError(w, statusCount, "not_found", "missing")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"input_tokens": 42})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	g, gw := startAnthropic(t, up.URL, "real-key", nil)

	// /admin/ready succeeds with X-Api-Key + Anthropic-Version.
	a, err := NewAdmin(g, nil, testToken)
	if err != nil {
		t.Fatal(err)
	}
	adm := httptest.NewServer(a)
	t.Cleanup(adm.Close)
	if code, body := call(t, adm, http.MethodGet, "/admin/ready", testToken, ""); code != http.StatusOK || body["ready"] != true {
		t.Fatalf("ready failed: code=%d body=%v", code, body)
	}

	// Upstream 200 on count_tokens passes through, preserving Anthropic-Version and Anthropic-Beta.
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages/count_tokens", strings.NewReader(`{"model":"big-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Anthropic-Version", "2024-06-01")
	req.Header.Set("Anthropic-Beta", "token-counting-2024-11-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		InputTokens int `json:"input_tokens"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if resp.StatusCode != 200 || tok.InputTokens != 42 || resp.Header.Get("X-Ezbx-Engine") != "anthropic" || gotVer != "2024-06-01" || gotBeta != "token-counting-2024-11-01" {
		t.Fatalf("passthrough count_tokens: status=%d tokens=%d ver=%q beta=%q", resp.StatusCode, tok.InputTokens, gotVer, gotBeta)
	}

	// Upstream 404 falls back to local estimate.
	statusCount = http.StatusNotFound
	resp, err = http.Post(gw.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader(`{"model":"big-model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tok.InputTokens = 0
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if resp.StatusCode != 200 || tok.InputTokens < 1 || resp.Header.Get("X-Ezbx-Engine") != "anthropic" {
		t.Fatalf("fallback count_tokens: status=%d tokens=%d", resp.StatusCode, tok.InputTokens)
	}
}

// TestAnthropicCountTokensFallback: only a 200 with a whole, non-negative
// input_tokens is passed on; every other answer gets the local estimate.
func TestAnthropicCountTokensFallback(t *testing.T) {
	var status int
	var reply string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(up.Close)
	_, gw := startAnthropic(t, up.URL, "real-key", nil)

	big := `{"input_tokens":4242,"pad":"` + strings.Repeat("x", 64<<10) + `"}`
	cases := []struct {
		name   string
		status int
		reply  string
		want   int // -1: the local estimate
	}{
		{"count", http.StatusOK, `{"input_tokens":4242}`, 4242},
		{"zero", http.StatusOK, `{"input_tokens":0}`, 0},
		{"200 with an error body", http.StatusOK, `{"error":"Unexpected endpoint or method. (POST /v1/messages/count_tokens)"}`, -1},
		{"404", http.StatusNotFound, `{"input_tokens":4242}`, -1},
		{"501", http.StatusNotImplemented, `{"input_tokens":4242}`, -1},
		{"400", http.StatusBadRequest, `{"input_tokens":4242}`, -1},
		{"negative", http.StatusOK, `{"input_tokens":-1}`, -1},
		{"fraction", http.StatusOK, `{"input_tokens":4.5}`, -1},
		{"null", http.StatusOK, `{"input_tokens":null}`, -1},
		{"not JSON", http.StatusOK, `input_tokens: 4242`, -1},
		{"too long", http.StatusOK, big, -1},
	}
	for _, c := range cases {
		status, reply = c.status, c.reply
		resp, err := http.Post(gw.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader(`{"model":"big-model","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var tok struct {
			InputTokens *int `json:"input_tokens"`
		}
		_ = json.Unmarshal(raw, &tok)
		if resp.StatusCode != http.StatusOK || tok.InputTokens == nil || resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("X-Ezbx-Engine") != "anthropic" {
			t.Fatalf("%s: status=%d type=%q body=%.200s", c.name, resp.StatusCode, resp.Header.Get("Content-Type"), raw)
		}
		got := *tok.InputTokens
		if c.want >= 0 && got != c.want {
			t.Errorf("%s: input_tokens = %d, want the server's %d", c.name, got, c.want)
		}
		// The local estimate for this 67-byte body is 67/4 = 16.
		if c.want < 0 && got != 16 {
			t.Errorf("%s: input_tokens = %d, want the local estimate 16", c.name, got)
		}
	}
}

func TestChatCompletionsViaAnthropicNonStream(t *testing.T) {
	var seenReq map[string]any
	var seenAuth, seenXKey, seenVer string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		seenAuth = r.Header.Get("Authorization")
		seenXKey = r.Header.Get("X-Api-Key")
		seenVer = r.Header.Get("Anthropic-Version")
		_ = json.NewDecoder(r.Body).Decode(&seenReq)

		model, _ := seenReq["model"].(string)
		stopReason := "tool_use"
		if strings.HasPrefix(model, "sr-") {
			stopReason = strings.TrimPrefix(model, "sr-")
		}
		content := []map[string]any{
			{"type": "thinking", "thinking": "step 1"},
			{"type": "text", "text": "checking files"},
		}
		if stopReason == "tool_use" {
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    "toolu_1",
				"name":  "read_file",
				"input": map[string]any{"path": "a.txt"},
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":          "msg_abc",
			"type":        "message",
			"role":        "assistant",
			"model":       model,
			"content":     content,
			"stop_reason": stopReason,
			"usage":       map[string]any{"input_tokens": 12, "output_tokens": 7},
		})
	}))
	t.Cleanup(up.Close)
	_, gw := startAnthropic(t, up.URL, "real-key", nil)

	// Full request: system + developer extraction, parallel tool messages merged,
	// empty tool args normalized, stop string -> stop_sequences, max_tokens default.
	chatReq := `{
		"model": "big-model",
		"stop": "DONE",
		"tools": [{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object"}}}],
		"messages": [
			{"role":"system","content":"be brief"},
			{"role":"developer","content":"use tools"},
			{"role":"user","content":[{"type":"text","text":"read both"}]},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"read_file","arguments":""}},
				{"id":"c2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.txt\"}"}}
			]},
			{"role":"tool","tool_call_id":"c1","content":"one"},
			{"role":"tool","tool_call_id":"c2","content":"two"}
		]
	}`
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(chatReq))
	req.Header.Set("Authorization", "Bearer box-placeholder")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var cresp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
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
	_ = json.NewDecoder(resp.Body).Decode(&cresp)
	resp.Body.Close()

	if seenAuth != "" || seenXKey != "real-key" || seenVer != anthropicVersion {
		t.Fatalf("auth headers: auth=%q xKey=%q ver=%q", seenAuth, seenXKey, seenVer)
	}
	if seenReq["system"] != "be brief\n\nuse tools" {
		t.Errorf("system = %v", seenReq["system"])
	}
	if int(seenReq["max_tokens"].(float64)) != defaultAnthropicMaxTokens {
		t.Errorf("default max_tokens = %v", seenReq["max_tokens"])
	}
	stops, _ := seenReq["stop_sequences"].([]any)
	if len(stops) != 1 || stops[0] != "DONE" {
		t.Errorf("stop_sequences = %v", seenReq["stop_sequences"])
	}
	msgs, _ := seenReq["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 merged messages (user, assistant, user with 2 tool_results), got %d: %v", len(msgs), msgs)
	}
	lastUser := msgs[2].(map[string]any)
	if lastUser["role"] != "user" || len(lastUser["content"].([]any)) != 2 {
		t.Fatalf("tool messages should merge into one user message: %v", lastUser)
	}
	asstMsg := msgs[1].(map[string]any)
	firstTool := asstMsg["content"].([]any)[0].(map[string]any)
	if inp, ok := firstTool["input"].(map[string]any); !ok || inp == nil {
		t.Fatalf("empty tool args must become non-nil object, got %#v", firstTool["input"])
	}
	if cresp.Object != "chat.completion" || len(cresp.Choices) != 1 ||
		cresp.Choices[0].Message.Content != "checking files" ||
		cresp.Choices[0].Message.ReasoningContent != "step 1" ||
		cresp.Choices[0].FinishReason != "tool_calls" ||
		len(cresp.Choices[0].Message.ToolCalls) != 1 ||
		cresp.Choices[0].Message.ToolCalls[0].Function.Name != "read_file" ||
		cresp.Usage.TotalTokens != 19 {
		t.Fatalf("chat completion reply wrong: %+v", cresp)
	}

	// Verify stop_reason mappings and max_completion_tokens fallback.
	for sr, wantFinish := range map[string]string{
		"end_turn":   "stop",
		"max_tokens": "length",
		"refusal":    "content_filter",
	} {
		resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"sr-`+sr+`","max_completion_tokens":77,"stop":["A","B"],"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		var out chatCompletionResp
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if int(seenReq["max_tokens"].(float64)) != 77 {
			t.Errorf("%s: max_completion_tokens not copied: %v", sr, seenReq["max_tokens"])
		}
		if len(seenReq["stop_sequences"].([]any)) != 2 {
			t.Errorf("%s: stop array lost: %v", sr, seenReq["stop_sequences"])
		}
		if out.Choices[0].FinishReason != wantFinish {
			t.Errorf("%s: finish_reason = %q, want %q", sr, out.Choices[0].FinishReason, wantFinish)
		}
	}
}

func TestChatCompletionsViaAnthropicStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")

		switch req.Model {
		case "zero-arg-tool":
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"zero-arg-tool\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Let me check.\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_0\",\"name\":\"ping\",\"input\":{}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		case "max-tokens":
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m2\",\"model\":\"max-tokens\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"cut\"}}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		default:
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m3\",\"model\":\"big-model\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_9\",\"name\":\"read_file\",\"input\":{}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"x\\\"}\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		}
	}))
	t.Cleanup(up.Close)
	_, gw := startAnthropic(t, up.URL, "", nil)

	// 1. Text block 0 + tool block 1 -> tool_calls index must be 0, and empty tool args filled with "{}".
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"zero-arg-tool","stream":true,"messages":[{"role":"user","content":"go"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	s := string(raw)
	if !strings.Contains(s, "data: [DONE]") {
		t.Fatalf("missing [DONE]:\n%s", s)
	}
	var gotText, gotToolName, gotToolID, gotArgs, gotFinish string
	var gotToolIndices []int
	for _, line := range strings.Split(s, "\n") {
		d, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok || d == "[DONE]" {
			continue
		}
		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(d), &chunk); err != nil || len(chunk.Choices) == 0 {
			continue
		}
		gotText += chunk.Choices[0].Delta.Content
		for _, tc := range chunk.Choices[0].Delta.ToolCalls {
			gotToolIndices = append(gotToolIndices, tc.Index)
			if tc.ID != "" {
				gotToolID = tc.ID
			}
			if tc.Function.Name != "" {
				gotToolName = tc.Function.Name
			}
			gotArgs += tc.Function.Arguments
		}
		if chunk.Choices[0].FinishReason != nil {
			gotFinish = *chunk.Choices[0].FinishReason
		}
	}
	if gotText != "Let me check." || len(gotToolIndices) != 2 || gotToolIndices[0] != 0 || gotToolIndices[1] != 0 || gotToolID != "toolu_0" || gotToolName != "ping" || gotArgs != "{}" || gotFinish != "tool_calls" {
		t.Errorf("zero-arg-tool stream parsed text=%q indices=%v id=%q name=%q args=%q finish=%q\n%s",
			gotText, gotToolIndices, gotToolID, gotToolName, gotArgs, gotFinish, s)
	}

	// 2. thinking_delta -> reasoning_content, max_tokens -> length.
	resp, err = http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"max-tokens","stream":true,"messages":[{"role":"user","content":"go"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	s = string(raw)
	for _, want := range []string{`"reasoning_content":"hmm"`, `"content":"cut"`, `"finish_reason":"length"`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("max-tokens stream missing %q\n%s", want, s)
		}
	}

	// 3. Streamed tool arguments.
	resp, err = http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"big-model","stream":true,"messages":[{"role":"user","content":"go"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"name":"read_file"`) || !strings.Contains(string(raw), `\"x\"`) {
		t.Errorf("tool args stream unexpected:\n%s", string(raw))
	}
}

func TestResponsesViaAnthropic(t *testing.T) {
	var seenReq map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seenReq)
		model, _ := seenReq["model"].(string)
		stream, _ := seenReq["stream"].(bool)
		stopReason := "end_turn"
		if strings.HasPrefix(model, "sr-") {
			stopReason = strings.TrimPrefix(model, "sr-")
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\""+model+"\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
			if stopReason == "tool_use" {
				_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_7\",\"name\":\"shell\",\"input\":{}}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
			}
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\""+stopReason+"\"}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		content := []map[string]any{{"type": "text", "text": "partial"}}
		if stopReason == "tool_use" {
			content = append(content, map[string]any{"type": "tool_use", "id": "call_7", "name": "shell", "input": map[string]any{"cmd": "ls"}})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":          "m1",
			"model":       model,
			"content":     content,
			"stop_reason": stopReason,
			"usage":       map[string]any{"input_tokens": 5, "output_tokens": 3},
		})
	}))
	t.Cleanup(up.Close)
	_, gw := startAnthropic(t, up.URL, "real-key", nil)

	// Non-stream with instructions, developer, function_call, function_call_output, tools.
	reqBody := `{"model":"sr-tool_use","instructions":"sys","max_output_tokens":64,"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]},{"type":"function_call","call_id":"call_0","name":"shell","arguments":"{}"},{"type":"function_call_output","call_id":"call_0","output":"out"}]}`
	var rresp map[string]any
	_ = json.NewDecoder(post(t, gw.URL+"/v1/responses", reqBody).Body).Decode(&rresp)
	if seenReq["system"] != "sys\n\ndev" {
		t.Errorf("system = %v", seenReq["system"])
	}
	if int(seenReq["max_tokens"].(float64)) != 64 {
		t.Errorf("max_tokens = %v", seenReq["max_tokens"])
	}
	checkResponse(t, "responses tool_use", rresp, "completed", "", "completed")

	// Finish reasons (non-stream and stream): max_tokens -> incomplete/max_output_tokens,
	// refusal -> incomplete/content_filter, end_turn -> completed.
	for sr, want := range map[string]struct {
		status, reason, item, endEvent string
	}{
		"end_turn":   {"completed", "", "completed", "response.completed"},
		"max_tokens": {"incomplete", "max_output_tokens", "incomplete", "response.incomplete"},
		"refusal":    {"incomplete", "content_filter", "incomplete", "response.incomplete"},
		"tool_use":   {"completed", "", "completed", "response.completed"},
	} {
		var res map[string]any
		_ = json.NewDecoder(post(t, gw.URL+"/v1/responses", `{"model":"sr-`+sr+`","max_output_tokens":8,"input":"hi"}`).Body).Decode(&res)
		checkResponse(t, "non-stream "+sr, res, want.status, want.reason, want.item)

		names, data := sseEvents(t, post(t, gw.URL+"/v1/responses", `{"model":"sr-`+sr+`","max_output_tokens":8,"stream":true,"input":"hi"}`).Body)
		if len(names) == 0 || names[len(names)-1] != want.endEvent {
			t.Fatalf("stream %s: end event = %v, want %s", sr, names, want.endEvent)
		}
		checkResponse(t, "stream "+sr, data[len(data)-1]["response"].(map[string]any), want.status, want.reason, want.item)
	}
}

func TestUpstreamErrorsPassThroughAndSwitchBetweenKinds(t *testing.T) {
	var seenOpenAIAuth, seenAnthKey, seenAnthPath string
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenOpenAIAuth = r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": "from-openai"}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(openaiUp.Close)

	anthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAnthKey = r.Header.Get("X-Api-Key")
		seenAnthPath = r.URL.Path
		if r.Header.Get("Content-Type") != "" && strings.Contains(seenAnthPath, "/v1/messages") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
		}
	}))
	t.Cleanup(anthUp.Close)

	g, gw := start(t, openaiUp.URL, "shared-key", nil)
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seenOpenAIAuth != "Bearer shared-key" {
		t.Fatalf("openai auth = %q", seenOpenAIAuth)
	}

	if err := g.SetUpstream(anthUp.URL, "anthropic"); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if seenAnthPath != "/v1/messages" || seenAnthKey != "shared-key" || resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "rate_limit_error") {
		t.Fatalf("after switch to anthropic: path=%q key=%q status=%d body=%s", seenAnthPath, seenAnthKey, resp.StatusCode, body)
	}

	// /v1/responses also passes upstream non-200 errors straight through.
	resp, err = http.Post(gw.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "rate_limit_error") {
		t.Fatalf("/v1/responses error passthrough: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestChatCompletionsViaAnthropicStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"big-model\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"one\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(up.Close)
	_, gw := startAnthropic(t, up.URL, "", nil)

	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 2 * time.Second}}
	resp, err := client.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"big-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		close(release)
		t.Fatalf("gateway buffered response headers: %v", err)
	}
	defer resp.Body.Close()

	first := make(chan string, 1)
	go func() {
		r := bufio.NewReader(resp.Body)
		var saw strings.Builder
		for i := 0; i < 4; i++ {
			line, err := r.ReadString('\n')
			saw.WriteString(line)
			if strings.Contains(line, `"content":"one"`) || err != nil {
				break
			}
		}
		first <- saw.String()
	}()
	select {
	case chunk := <-first:
		if !strings.Contains(chunk, `"content":"one"`) {
			t.Fatalf("expected unbuffered text chunk, got %q", chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("translated chat stream buffered before upstream finished")
	}
	close(release)
}
