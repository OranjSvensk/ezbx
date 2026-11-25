package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

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
