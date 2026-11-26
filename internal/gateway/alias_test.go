package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestModelListAdvertisesDefault(t *testing.T) {
	for _, tc := range []struct {
		name, kind, target, reply string
		status                    int
		ids                       []string
	}{
		{"openai", "openai", "large", `{"object":"list","data":[{"id":"small"},{"id":"large","context_window":65536}]}`, 200, []string{"small", "large", "default"}},
		{"anthropic collision", "anthropic", "large", `{"first_id":"default","last_id":"large","has_more":true,"data":[{"id":"default"},{"id":"large","display_name":"Large","context_window":65536}]}`, 200, []string{"large", "default"}},
		{"disabled", "openai", "", `{"data":[{"id":"default"},{"id":"large"}]}`, 200, nil},
		{"target absent", "openai", "large", `{"data":[{"id":"small"}]}`, 200, nil},
		{"reserved id without target", "openai", "large", `{"data":[{"id":"default"}]}`, 200, []string{}},
		{"upstream error", "openai", "large", `{"data":[{"id":"large"}]}`, 401, nil},
		{"malformed list", "openai", "large", `{"data":[{"id":"large"},null]}`, 200, nil},
		{"oversized list", "openai", "large", `{"data":[{"id":"large"}]}` + strings.Repeat(" ", maxAliasList), 200, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"upstream"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.(http.Flusher).Flush() // exercise the path without Content-Length
				io.WriteString(w, tc.reply)
			}))
			defer up.Close()
			g, err := New(Config{Upstream: up.URL, EngineID: tc.kind, DefaultModel: tc.target})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d", rec.Code, tc.status)
			}
			if tc.ids == nil {
				if rec.Body.String() != tc.reply || rec.Header().Get("ETag") != `"upstream"` {
					t.Fatal("the upstream reply changed")
				}
				return
			}
			var list struct {
				Object  string
				FirstID string `json:"first_id"`
				LastID  string `json:"last_id"`
				HasMore bool   `json:"has_more"`
				Data    []struct {
					ID          string
					Context     int    `json:"context_window"`
					DisplayName string `json:"display_name"`
				}
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || list.Data == nil {
				t.Fatalf("invalid list: %s (%v)", rec.Body.String(), err)
			}
			var ids []string
			for _, model := range list.Data {
				ids = append(ids, model.ID)
				if model.ID == "default" && model.Context != 65536 {
					t.Errorf("alias lost the target's context: %+v", model)
				}
				if model.ID == "default" && tc.kind == "anthropic" && model.DisplayName != "default" {
					t.Errorf("alias display name %q", model.DisplayName)
				}
			}
			if !slices.Equal(ids, tc.ids) {
				t.Errorf("ids %v, want %v", ids, tc.ids)
			}
			if tc.kind == "anthropic" && (list.FirstID != "default" || list.LastID != "large" || !list.HasMore) {
				t.Errorf("pagination changed: %+v", list)
			}
			if rec.Header().Get("ETag") != "" || rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("cache headers: %v", rec.Header())
			}
		})
	}
}

func TestModelListKeepsTheAliasFromItsUpstream(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			io.WriteString(w, `{"data":[{"id":"first"}]}`)
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	g, err := New(Config{Upstream: up.URL, DefaultModel: "first"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(rec, req.WithContext(ctx))
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first engine did not receive the request")
	}
	if err := g.setUpstream("http://127.0.0.1:2", "openai", "second"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"default"`) {
		t.Fatalf("response from the first engine: %d %s", rec.Code, rec.Body.String())
	}
}

func TestModelListAliasBudget(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[{"id":"large"}]}`)
	}))
	defer up.Close()
	g, err := New(Config{Upstream: up.URL, DefaultModel: "large", MaxBuffered: 4 * maxAliasList})
	if err != nil {
		t.Fatal(err)
	}
	if !g.buffered.take(1) {
		t.Fatal("could not reserve a byte")
	}
	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		return rec
	}
	if rec := request(); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("busy reply: %d %s", rec.Code, rec.Body.String())
	}
	g.buffered.give(1)
	for range 2 {
		if rec := request(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"default"`) {
			t.Fatalf("available reply: %d %s", rec.Code, rec.Body.String())
		}
	}
}

// echoEngine records the request bodies it gets.
type echoEngine struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (e *echoEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var b map[string]any
	json.NewDecoder(r.Body).Decode(&b)
	e.mu.Lock()
	e.bodies = append(e.bodies, b)
	e.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
}

func (e *echoEngine) last() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bodies[len(e.bodies)-1]
}

func TestDefaultModelAlias(t *testing.T) {
	eng := &echoEngine{}
	up := httptest.NewServer(eng)
	t.Cleanup(up.Close)
	logs := &logWatch{syncBuffer: &syncBuffer{}}
	g, err := New(Config{Upstream: up.URL, EngineID: "openai", Log: logs.syncBuffer, DefaultModel: "big-model"})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)

	resp := post(t, gw.URL+"/v1/chat/completions", `{"model":"default","temperature":0.5,"messages":[{"role":"user","content":"hi"}]}`)
	resp.Body.Close()
	b := eng.last()
	if b["model"] != "big-model" || b["temperature"] != 0.5 || len(b["messages"].([]any)) != 1 {
		t.Errorf("the engine got %v", b)
	}
	if resp.Header.Get("X-Ezbx-Model") != "big-model" {
		t.Errorf("X-Ezbx-Model %q", resp.Header.Get("X-Ezbx-Model"))
	}
	if e := lastEntry(t, logs); e.Model != "big-model" {
		t.Errorf("log names %q", e.Model)
	}
	// Translated routes get the real name too.
	post(t, gw.URL+"/v1/messages", `{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`).Body.Close()
	if eng.last()["model"] != "big-model" {
		t.Errorf("messages: %v", eng.last())
	}
	lastEntry(t, logs)
	// Any other model is left alone.
	post(t, gw.URL+"/v1/chat/completions", `{"model":"small","messages":[]}`).Body.Close()
	if eng.last()["model"] != "small" {
		t.Errorf("other model: %v", eng.last())
	}
	lastEntry(t, logs)

	// Without a default model, "default" reaches the engine as it is.
	if err := g.SetDefaultModel(""); err != nil {
		t.Fatal(err)
	}
	post(t, gw.URL+"/v1/chat/completions", `{"model":"default","messages":[]}`).Body.Close()
	if eng.last()["model"] != "default" {
		t.Errorf("no alias: %v", eng.last())
	}

	for _, bad := range []string{"default", "a\nb", strings.Repeat("m", 201)} {
		if err := g.SetDefaultModel(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
		if _, err := New(Config{Upstream: up.URL, DefaultModel: bad}); err == nil {
			t.Errorf("New took %q", bad)
		}
	}
}

// A longer real name needs more of the shared body budget, and a full
// budget refuses the request instead of going over.
func TestDefaultModelBudget(t *testing.T) {
	eng := &echoEngine{}
	up := httptest.NewServer(eng)
	t.Cleanup(up.Close)
	body := `{"model":"default"}`
	long := strings.Repeat("m", 200)
	// Room for two short bodies: a request gives its share back only when
	// its handler ends, which can be just after the client has the reply,
	// so the next request may start while the last still holds it. The
	// 200-character name still doesn't fit.
	g, err := New(Config{Upstream: up.URL, DefaultModel: long, MaxBuffered: 2*int64(len(body)) + 10})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", resp.StatusCode)
	}
	// A shorter name gives budget back: the next request still fits.
	if err := g.SetDefaultModel("m"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		resp := post(t, gw.URL+"/v1/chat/completions", body)
		resp.Body.Close()
	}
	if eng.last()["model"] != "m" {
		t.Errorf("%v", eng.last())
	}
}

func TestAdminSetsTheDefaultModel(t *testing.T) {
	g, _, srv, _ := newAdminEnv(t)
	code, out := call(t, srv, http.MethodPut, "/admin/engine", testToken, `{"upstream":"http://127.0.0.1:2","engine":"openai","default_model":"big-model"}`)
	if code != http.StatusOK || out["default_model"] != "big-model" || g.DefaultModel() != "big-model" {
		t.Errorf("set: %d %v", code, out)
	}
	// Switching engines without one turns the alias off.
	if code, out = call(t, srv, http.MethodPut, "/admin/engine", testToken, `{"upstream":"http://127.0.0.1:3","engine":"openai"}`); code != http.StatusOK || g.DefaultModel() != "" || out["default_model"] != nil {
		t.Errorf("cleared: %d %v", code, out)
	}
	if code, _ = call(t, srv, http.MethodPut, "/admin/engine", testToken, `{"upstream":"http://127.0.0.1:4","default_model":"default"}`); code != http.StatusBadRequest {
		t.Errorf("the alias itself: %d", code)
	}
	if up, _ := g.Upstream(); up != "http://127.0.0.1:3" {
		t.Errorf("a refused change moved the upstream to %s", up)
	}
}
