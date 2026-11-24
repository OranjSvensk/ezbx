// Command fakeengine is a stand-in inference engine for tests that need a
// real network path but no real model. It answers OpenAI-compatible
// (/v1/models, /v1/chat/completions) or Anthropic-compatible (--api anthropic:
// /v1/models, /v1/messages, /v1/messages/count_tokens) requests and listens on
// loopback by default, like a real engine would.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// reply is the fixed answer to every chat request.
const reply = "Hello from the fake engine."

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "address to listen on")
	api := flag.String("api", "openai", "API kind to serve: openai or anthropic")
	keyEnv := flag.String("key-env", "", "NAME of an environment variable holding a key that chat requests must carry")
	keyOnModels := flag.Bool("key-on-models", false, "require the key for the model list too")
	flag.Parse()

	if *api != "openai" && *api != "anthropic" {
		fmt.Fprintf(os.Stderr, "unknown --api %q (use openai or anthropic)\n", *api)
		os.Exit(2)
	}

	// With a key, requests without the expected header get 401, as from an
	// engine started with an API key.
	key := ""
	if *keyEnv != "" {
		if key = os.Getenv(*keyEnv); key == "" {
			fmt.Fprintf(os.Stderr, "The environment variable %s is empty or not set.\n", *keyEnv)
			os.Exit(2)
		}
	}
	checkKey := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if key != "" {
				ok := r.Header.Get("Authorization") == "Bearer "+key
				if *api == "anthropic" {
					ok = r.Header.Get("X-Api-Key") == key
				}
				if !ok {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					io.WriteString(w, `{"error":{"message":"missing or wrong key","type":"invalid_request_error"}}`)
					return
				}
			}
			h(w, r)
		}
	}
	checkVersion := func(h http.HandlerFunc) http.HandlerFunc {
		if *api != "anthropic" {
			return h
		}
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Anthropic-Version") == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"missing anthropic-version header"}}`)
				return
			}
			h(w, r)
		}
	}
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return checkKey(checkVersion(h))
	}
	models := func(h http.HandlerFunc) http.HandlerFunc {
		if *keyOnModels {
			return guard(h)
		}
		return checkVersion(h)
	}

	mux := http.NewServeMux()
	if *api == "anthropic" {
		registerAnthropic(mux, models, guard)
	} else {
		registerOpenAI(mux, models, guard)
	}

	fmt.Fprintf(os.Stderr, "fake engine (%s) listening on %s\n", *api, *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func registerOpenAI(mux *http.ServeMux, models, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/models", models(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"fake-model","object":"model","owned_by":"ezbx"}]}`)
	}))
	mux.HandleFunc("POST /v1/chat/completions", guard(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"fake","object":"chat.completion","model":"fake-model",`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":"`+reply+`"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			return
		}
		// A streamed reply: one chunk per word, then the end marker.
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunk := func(delta, finish string) {
			fmt.Fprintf(w, `data: {"id":"fake","object":"chat.completion.chunk","model":"fake-model",`+
				`"choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`+"\n\n", delta, finish)
			if flusher != nil {
				flusher.Flush()
			}
		}
		chunk(`{"role":"assistant","content":""}`, "null")
		for i, word := range strings.Fields(reply) {
			if i > 0 {
				word = " " + word
			}
			b, _ := json.Marshal(map[string]string{"content": word})
			chunk(string(b), "null")
		}
		chunk(`{}`, `"stop"`)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
}

func registerAnthropic(mux *http.ServeMux, models, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/models", models(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"fake-model","type":"model","display_name":"Fake Model","created_at":"2026-01-01T00:00:00Z"}],"first_id":"fake-model","last_id":"fake-model","has_more":false}`)
	}))
	mux.HandleFunc("POST /v1/messages/count_tokens", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":7}`)
	}))
	mux.HandleFunc("POST /v1/messages", guard(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxTokens int  `json:"max_tokens"`
			Stream    bool `json:"stream"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&req)
		if req.MaxTokens <= 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must be greater than 0"}}`)
			return
		}
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"msg_fake","type":"message","role":"assistant","model":"fake-model",`+
				`"content":[{"type":"text","text":"`+reply+`"}],`+
				`"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		sse := func(event string, data any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		sse("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_fake", "type": "message", "role": "assistant", "model": "fake-model",
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
			},
		})
		sse("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		for i, word := range strings.Fields(reply) {
			if i > 0 {
				word = " " + word
			}
			sse("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": word},
			})
		}
		sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		sse("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 1},
		})
		sse("message_stop", map[string]any{"type": "message_stop"})
	}))
}
