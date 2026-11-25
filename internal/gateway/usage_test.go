package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadUsage(t *testing.T) {
	cases := map[string][2]int{
		`{"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`:                   {7, 3},
		`{"usage":{"input_tokens":5,"output_tokens":2}}`:                                          {5, 2},
		`{"type":"message_start","message":{"usage":{"input_tokens":9,"output_tokens":1}}}`:       {9, 1},
		`{"type":"response.completed","response":{"usage":{"input_tokens":4,"output_tokens":6}}}`: {4, 6},
		`{"type":"message_delta","usage":{"output_tokens":12}}`:                                   {0, 12},
		`{"choices":[]}`: {0, 0},
		`not json`:       {0, 0},
	}
	for raw, want := range cases {
		if in, out := readUsage([]byte(raw)); in != want[0] || out != want[1] {
			t.Errorf("%s: %d, %d", raw, in, out)
		}
	}
}

func TestUsageSniffer(t *testing.T) {
	// A stream whose usage line arrives in pieces, with the counts in two
	// events (Anthropic's way), and a long line that is skipped.
	info := &reqInfo{}
	var s usageSniffer
	for _, p := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_t",
		"okens\":11,\"output_tokens\":1}}}\r\n\n",
		// Too long to read, though it holds counts: skipped.
		"data: {\"usage\":{\"input_tokens\":99,\"output_tokens\":99},\"pad\":\"" + strings.Repeat("x", maxUsageLine/2),
		strings.Repeat("x", maxUsageLine/2) + "\"}\n",
		// The rest of a skipped line is skipped too, whatever it looks like.
		"data: " + strings.Repeat("y", maxUsageLine+1),
		"data: {\"usage\":{\"input_tokens\":77,\"output_tokens\":77}}\n",
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":0}}\n",
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}",
	} {
		s.feed(info, "text/event-stream; charset=utf-8", []byte(p))
	}
	if info.tokensIn != 11 || info.tokensOut != 1 {
		t.Errorf("before the end: %d, %d", info.tokensIn, info.tokensOut)
	}
	s.finish(info) // the last line had no line break
	if info.tokensIn != 11 || info.tokensOut != 42 {
		t.Errorf("stream: %d, %d", info.tokensIn, info.tokensOut)
	}
	if len(s.line) > 0 && s.skipLine {
		t.Error("the long line is still held")
	}

	// A JSON body is read at the end, in pieces.
	info, s = &reqInfo{}, usageSniffer{}
	s.feed(info, "application/json", []byte(`{"usage":{"prompt_tokens":3,`))
	s.feed(info, "application/json", []byte(`"completion_tokens":4}}`))
	if info.tokensIn != 0 {
		t.Error("a JSON body was read before the end")
	}
	s.finish(info)
	if info.tokensIn != 3 || info.tokensOut != 4 {
		t.Errorf("json: %d, %d", info.tokensIn, info.tokensOut)
	}

	// One too big for its usage is dropped, not held.
	info, s = &reqInfo{}, usageSniffer{}
	s.feed(info, "application/json", []byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4},"pad":"`+strings.Repeat("x", maxUsageBody)+`"}`))
	s.finish(info)
	if info.tokensIn != 0 || s.body != nil {
		t.Errorf("too big: %d, held %d bytes", info.tokensIn, len(s.body))
	}
}

// usageEngine is an OpenAI-compatible engine whose streams end with a usage
// chunk only when asked for one, like OpenAI's. With refuseOptions it
// answers 400 to stream_options, as a strict server might.
type usageEngine struct {
	refuseOptions bool
	mu            sync.Mutex
	sawOptions    []bool
}

func (e *usageEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream        bool            `json:"stream"`
		StreamOptions json.RawMessage `json:"stream_options"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	e.mu.Lock()
	e.sawOptions = append(e.sawOptions, req.StreamOptions != nil)
	e.mu.Unlock()
	if e.refuseOptions && req.StreamOptions != nil {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"unknown field stream_options"}}`)
		return
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`+"\n\n")
	io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	if req.StreamOptions != nil {
		io.WriteString(w, `data: {"id":"c1","choices":[],"usage":{"prompt_tokens":21,"completion_tokens":5,"total_tokens":26}}`+"\n\n")
	}
	io.WriteString(w, "data: [DONE]\n\n")
}

// lastEntry waits for a log line it hasn't returned yet: the gateway writes
// it just after the reply, so the test can read the reply first.
type logWatch struct {
	*syncBuffer
	seen int
}

func lastEntry(t *testing.T, logs *logWatch) entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		text := strings.TrimSpace(logs.String())
		lines := strings.Split(text, "\n")
		if text != "" && len(lines) > logs.seen {
			logs.seen = len(lines)
			var e entry
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &e); err != nil {
				t.Fatalf("log: %q", text)
			}
			return e
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new log line after %d: %q", logs.seen, text)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTokensInTheLog(t *testing.T) {
	eng := &usageEngine{}
	up := httptest.NewServer(eng)
	t.Cleanup(up.Close)
	logs := &logWatch{syncBuffer: &syncBuffer{}}
	g, err := New(Config{Upstream: up.URL, EngineID: "openai", Log: logs.syncBuffer})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)

	send := func(path, body string) string {
		resp := post(t, gw.URL+path, body)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	cases := []struct {
		path, body string
		in, out    int
		reply      string // what the box must see
	}{
		// Passed through: the engine's own usage.
		{"/v1/chat/completions", `{"model":"big-model","messages":[{"role":"user","content":"hi"}]}`, 7, 3, `"prompt_tokens":7`},
		{"/v1/chat/completions", `{"model":"big-model","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, 21, 5, `"prompt_tokens":21`},
		// Translated: the gateway asks for usage and passes the real numbers on.
		{"/v1/messages", `{"model":"big-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, 21, 5, `"usage":{"input_tokens":21,"output_tokens":5}`},
		{"/v1/messages", `{"model":"big-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, 7, 3, `"input_tokens":7`},
		{"/v1/responses", `{"model":"big-model","stream":true,"input":"hi"}`, 21, 5, `"input_tokens":21,"output_tokens":5`},
		{"/v1/responses", `{"model":"big-model","input":"hi"}`, 7, 3, `"input_tokens":7`},
	}
	for _, tc := range cases {
		reply := send(tc.path, tc.body)
		e := lastEntry(t, logs)
		if e.TokensIn != tc.in || e.TokensOut != tc.out || e.Path != tc.path {
			t.Errorf("%s %s: log %+v", tc.path, tc.body, e)
		}
		if !strings.Contains(reply, tc.reply) {
			t.Errorf("%s: the box saw %s", tc.path, reply)
		}
	}
	// The Messages stream no longer says one token at the start.
	if reply := send("/v1/messages", `{"model":"big-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`); strings.Contains(reply, `"input_tokens":1,`) {
		t.Errorf("placeholder count: %s", reply)
	}
	// Take its log line too, or the next lastEntry may return it: the line is
	// written just after the reply (seen only under -race in CI).
	if e := lastEntry(t, logs); e.Path != "/v1/messages" || e.TokensIn != 21 {
		t.Errorf("placeholder request's log: %+v", e)
	}
	// A plain stream through the gateway is left as the box asked for it.
	eng.sawOptions = nil
	send("/v1/chat/completions", `{"model":"big-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if fmt.Sprint(eng.sawOptions) != "[false]" {
		t.Errorf("passthrough changed the request: %v", eng.sawOptions)
	}
	if e := lastEntry(t, logs); e.TokensIn != 0 || e.TokensOut != 0 {
		t.Errorf("no usage reported, none logged: %+v", e)
	}

	// A server that refuses stream_options is asked again without it.
	eng.refuseOptions, eng.sawOptions = true, nil
	reply := send("/v1/messages", `{"model":"big-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if fmt.Sprint(eng.sawOptions) != "[true false]" || !strings.Contains(reply, "message_stop") || !strings.Contains(reply, `"output_tokens":0`) {
		t.Errorf("retry: saw %v, reply %s", eng.sawOptions, reply)
	}
	if e := lastEntry(t, logs); e.Status != http.StatusOK || e.TokensIn != 0 {
		t.Errorf("retry log: %+v", e)
	}
	// A non-streamed request never carries the option.
	eng.sawOptions = nil
	send("/v1/messages", `{"model":"big-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if fmt.Sprint(eng.sawOptions) != "[false]" {
		t.Errorf("non-streamed: %v", eng.sawOptions)
	}
}

// Against an Anthropic server, a translated chat stream carries the usage
// only when the client asked, and the log has it either way.
func TestTokensViaAnthropic(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"type":"message_start","message":{"id":"msg_1","model":"big-model","usage":{"input_tokens":13,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}`,
			`{"type":"message_stop"}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}))
	t.Cleanup(up.Close)
	logs := &logWatch{syncBuffer: &syncBuffer{}}
	_, gw := startAnthropic(t, up.URL, "", logs.syncBuffer)
	for _, tc := range []struct {
		body      string
		wantChunk bool
	}{
		{`{"model":"big-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`, false},
		{`{"model":"big-model","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, true},
	} {
		resp := post(t, gw.URL+"/v1/chat/completions", tc.body)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if got := strings.Contains(string(b), `"usage":{"completion_tokens":8,"prompt_tokens":13,"total_tokens":21}`); got != tc.wantChunk {
			t.Errorf("usage chunk %v, want %v: %s", got, tc.wantChunk, b)
		}
		if !strings.HasSuffix(strings.TrimSpace(string(b)), "data: [DONE]") {
			t.Errorf("the stream must still end with [DONE]: %s", b)
		}
		if e := lastEntry(t, logs); e.TokensIn != 13 || e.TokensOut != 8 {
			t.Errorf("log: %+v", e)
		}
	}
	// Responses over Anthropic gets the real numbers in response.completed.
	resp := post(t, gw.URL+"/v1/responses", `{"model":"big-model","stream":true,"input":"hi"}`)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"input_tokens":13,"output_tokens":8`) {
		t.Errorf("responses: %s", b)
	}
	if e := lastEntry(t, logs); e.TokensIn != 13 || e.TokensOut != 8 {
		t.Errorf("responses log: %+v", e)
	}
}
