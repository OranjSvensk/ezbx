package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// finishEngine is a fake engine that ends every reply with the finish_reason
// named in the request's model ("fr-length" ends with "length"), with a tool
// call when that reason is tool_calls.
func finishEngine(t *testing.T) *httptest.Server {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		finish := strings.TrimPrefix(req.Model, "fr-")
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
			last := `{"choices":[{"delta":{},"finish_reason":"` + finish + `"}]}`
			if finish == "tool_calls" {
				last = `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
			}
			_, _ = io.WriteString(w, "data: "+last+"\n\n")
			// An empty finish_reason after the real one doesn't replace it.
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":""}]}`+"\n\n")
			// Some engines send a last chunk with usage and no choices.
			_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		msg := map[string]any{"role": "assistant", "content": "partial"}
		if finish == "tool_calls" {
			msg["tool_calls"] = []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"model":   req.Model,
			"choices": []map[string]any{{"message": msg, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 2},
		})
	}))
	t.Cleanup(up.Close)
	return up
}

// sseEvents reads a stream into its events, in order: name and data.
func sseEvents(t *testing.T, body io.Reader) (names []string, data []map[string]any) {
	t.Helper()
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, name)
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			var v map[string]any
			if err := json.Unmarshal([]byte(d), &v); err != nil {
				t.Fatalf("bad event data %q: %v", d, err)
			}
			data = append(data, v)
		}
	}
	return names, data
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: status %d: %s", url, resp.StatusCode, b)
	}
	return resp
}

// A reply the engine cut off at the token limit must reach the harness as
// cut off, in the harness's own terms, streamed or not. Otherwise a harness
// takes half an answer, or half a tool call, as the whole one.
func TestFinishReasonReachesTheHarness(t *testing.T) {
	_, gw := start(t, finishEngine(t).URL, "real-key", nil)
	cases := []struct {
		finish     string
		stop       string // Anthropic stop_reason
		status     string // Responses status
		incomplete string // Responses incomplete_details.reason
		item       string // the status of the Responses message item
	}{
		{"stop", "end_turn", "completed", "", "completed"},
		{"length", "max_tokens", "incomplete", "max_output_tokens", "incomplete"},
		{"tool_calls", "tool_use", "completed", "", "completed"},
		{"content_filter", "end_turn", "incomplete", "content_filter", "incomplete"},
	}
	for _, tc := range cases {
		model := "fr-" + tc.finish

		// Anthropic Messages, whole reply.
		var msg struct {
			StopReason string `json:"stop_reason"`
		}
		_ = json.NewDecoder(post(t, gw.URL+"/v1/messages", `{"model":"`+model+`","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`).Body).Decode(&msg)
		if msg.StopReason != tc.stop {
			t.Errorf("%s: /v1/messages stop_reason %q, want %q", tc.finish, msg.StopReason, tc.stop)
		}

		// Anthropic Messages, streamed: message_delta carries the stop reason.
		names, data := sseEvents(t, post(t, gw.URL+"/v1/messages", `{"model":"`+model+`","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`).Body)
		got := ""
		for i, n := range names {
			if n == "message_delta" {
				got, _ = data[i]["delta"].(map[string]any)["stop_reason"].(string)
			}
		}
		if got != tc.stop || names[len(names)-1] != "message_stop" {
			t.Errorf("%s: streamed /v1/messages stop_reason %q (want %q), events %v", tc.finish, got, tc.stop, names)
		}

		// OpenAI Responses, whole reply.
		var res map[string]any
		_ = json.NewDecoder(post(t, gw.URL+"/v1/responses", `{"model":"`+model+`","max_output_tokens":8,"input":"hi"}`).Body).Decode(&res)
		checkResponse(t, tc.finish+": /v1/responses", res, tc.status, tc.incomplete, tc.item)

		// OpenAI Responses, streamed: the last event is response.incomplete
		// for a cut-off reply, else response.completed.
		names, data = sseEvents(t, post(t, gw.URL+"/v1/responses", `{"model":"`+model+`","max_output_tokens":8,"stream":true,"input":"hi"}`).Body)
		want := "response.completed"
		if tc.status == "incomplete" {
			want = "response.incomplete"
		}
		if last := names[len(names)-1]; last != want {
			t.Errorf("%s: streamed /v1/responses ends with %s, want %s", tc.finish, last, want)
		}
		checkResponse(t, tc.finish+": streamed /v1/responses", data[len(data)-1]["response"].(map[string]any), tc.status, tc.incomplete, tc.item)
		for i, n := range names {
			item, _ := data[i]["item"].(map[string]any)
			if n == "response.output_item.done" && item["type"] == "message" && item["status"] != tc.item {
				t.Errorf("%s: streamed message item status %v", tc.finish, item["status"])
			}
		}
	}
}

func checkResponse(t *testing.T, what string, res map[string]any, status, incomplete, itemWant string) {
	t.Helper()
	if res["status"] != status {
		t.Errorf("%s: status %v, want %s", what, res["status"], status)
	}
	det, _ := res["incomplete_details"].(map[string]any)
	if reason, _ := det["reason"].(string); reason != incomplete {
		t.Errorf("%s: incomplete_details %v, want reason %q", what, res["incomplete_details"], incomplete)
	}
	out, _ := res["output"].([]any)
	if len(out) == 0 {
		t.Errorf("%s: no output; a cut-off reply still carries what came", what)
		return
	}
	for _, o := range out {
		item := o.(map[string]any)
		if item["type"] == "message" && item["status"] != itemWant {
			t.Errorf("%s: message item status %v", what, item["status"])
		}
	}
}
