package detect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/OranjSvensk/ezbx/internal/adapter"
	"github.com/OranjSvensk/ezbx/providers"
)

// fakeEngine starts a server that answers the given paths with the given JSON.
// Any other path gets a 404, like a real server without that route.
func fakeEngine(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newDetector(t *testing.T) *Detector {
	t.Helper()
	adapters, err := adapter.LoadFS(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	// SkipHintPorts: tests probe only the fake servers, never real local ports.
	return &Detector{Adapters: adapters, SkipHintPorts: true, Timeout: 2 * time.Second}
}

const (
	plainModels     = `{"object":"list","data":[{"id":"some-model","object":"model"}]}`
	lmstudioNative  = `{"models":[{"type":"llm","key":"some-model","max_context_length":32768,"loaded_instances":[]}]}`
	anthropicModels = `{"data":[{"id":"big-model","type":"model","display_name":"Big Model","created_at":"2026-01-01T00:00:00Z"}],"first_id":"big-model","last_id":"big-model","has_more":false}`
)

func TestAPlainOpenAIServerIsTheOpenAIKind(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/v1/models": plainModels})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || res.Engines[0].ID != "openai" || res.Engines[0].Name != "OpenAI-compatible API" {
		t.Fatalf("want the openai kind, got %+v", res.Engines)
	}
	e := res.Engines[0]
	if len(e.Models) != 1 || e.Models[0].ID != "some-model" || len(e.AlsoMatched) != 0 {
		t.Errorf("openai server: %+v", e)
	}
	// The plain kind doesn't report context size, so it must stay unknown.
	if e.Models[0].ContextWindow != 0 {
		t.Errorf("context window should be unknown (0)")
	}
}

// Servers that add other routes beside the OpenAI ones (a health route, a
// native list of their own) are still just OpenAI-compatible to ezbx.
func TestOtherRoutesDontChangeTheKind(t *testing.T) {
	srv := fakeEngine(t, map[string]string{
		"/health":    `{"status":"ok","max_context":262144}`,
		"/api/ps":    `{"models":[]}`,
		"/v1/models": plainModels,
	})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || res.Engines[0].ID != "openai" {
		t.Fatalf("want the openai kind, got %+v", res.Engines)
	}
}

func TestTheLMStudioAPIWinsOverThePlainOne(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/v1/models": plainModels, "/api/v1/models": lmstudioNative})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || res.Engines[0].ID != "lmstudio" || res.Engines[0].Name != "LM Studio API" {
		t.Fatalf("want lmstudio, got %+v", res.Engines)
	}
	if strings.Join(res.Engines[0].AlsoMatched, ",") != "openai" {
		t.Errorf("the openai kind should be listed as also matching: %v", res.Engines[0].AlsoMatched)
	}
	if m := res.Engines[0].Models; len(m) != 1 || m[0].ContextWindow != 32768 {
		t.Errorf("the native list should give the context size: %+v", m)
	}
}

// Only the LM Studio API's native list is not enough: chat goes through the
// OpenAI routes, so both must answer.
func TestTheLMStudioAPINeedsTheOpenAIRoutesToo(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/api/v1/models": lmstudioNative})
	if res := newDetector(t).Detect(context.Background(), []string{srv.URL}); len(res.Engines) != 0 {
		t.Fatalf("a native list alone must not count: %+v", res.Engines)
	}
}

func TestServersWithNoModelsAreStillFound(t *testing.T) {
	empty := `{"object":"list","data":[]}`
	cases := map[string]map[string]string{
		"anthropic": {"/v1/models": `{"data":[],"has_more":false}`},
		"lmstudio":  {"/v1/models": empty, "/api/v1/models": `{"models":[]}`},
		"openai":    {"/v1/models": empty},
	}
	for want, routes := range cases {
		srv := fakeEngine(t, routes)
		res := newDetector(t).Detect(context.Background(), []string{srv.URL})
		if len(res.Engines) != 1 || res.Engines[0].ID != want {
			t.Errorf("%s with no models: got %+v", want, res.Engines)
			continue
		}
		if res.Engines[0].Models == nil || len(res.Engines[0].Models) != 0 {
			t.Errorf("%s: models should be an empty list, got %#v", want, res.Engines[0].Models)
		}
	}
}

// factKind is an API kind made for these tests, using every model fact the
// schema can read, so the reading code stays tested although the shipped
// kinds use only some of it.
const factKind = `{
  "schema": 1, "id": "facts", "name": "Facts",
  "detect": {"hintPorts": [], "probes": [{"path": "/v1/models", "expect": {"hasLists": ["data"]}}]},
  "openai": {"basePath": "/v1", "modelsPath": "/v1/models", "chatPath": "/v1/chat/completions"},
  "models": {"id": "data[].id", "contextWindow": "data[].meta.n_ctx", "input": "data[].architecture.input_modalities", "loaded": LOADED},
  "auth": {"mode": "none", "keyEnv": null, "note": ""},
  "capabilities": [], "verified": {"status": "docs-only", "on": null, "notes": ""}
}`

func factDetector(t *testing.T, loaded string) *Detector {
	t.Helper()
	body := strings.Replace(factKind, "LOADED", loaded, 1)
	adapters, err := adapter.LoadFS(fstest.MapFS{"facts/adapter.json": {Data: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	return &Detector{Adapters: adapters, SkipHintPorts: true, Timeout: 2 * time.Second}
}

func TestModelFactsAndTheLoadedFieldForm(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/v1/models": `{"object":"list","data":[
		{"id":"m1","status":{"value":"loaded"},"meta":{"n_ctx":262144},"architecture":{"input_modalities":["text","image"]}},
		{"id":"m2","status":{"value":"unloaded"}}]}`})
	res := factDetector(t, `"data[].status.value == loaded"`).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || len(res.Engines[0].Models) != 2 {
		t.Fatalf("got %+v", res.Engines)
	}
	m := res.Engines[0].Models[0]
	if m.ID != "m1" || m.ContextWindow != 262144 || strings.Join(m.Input, ",") != "text,image" {
		t.Errorf("model facts wrong: %+v", m)
	}
	if m.Loaded == nil || !*m.Loaded {
		t.Errorf("m1 should be marked loaded: %+v", m)
	}
	if l := res.Engines[0].Models[1].Loaded; l == nil || *l {
		t.Errorf("m2 should be marked not loaded: %v", l)
	}
}

func TestTheLoadedRouteForm(t *testing.T) {
	srv := fakeEngine(t, map[string]string{
		"/api/ps":    `{"models":[{"name":"m1:latest"}]}`,
		"/v1/models": `{"object":"list","data":[{"id":"m1:latest"},{"id":"m2:8b"}]}`,
	})
	res := factDetector(t, `"/api/ps models[].name"`).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 {
		t.Fatalf("got %+v", res.Engines)
	}
	got := map[string]bool{}
	for _, m := range res.Engines[0].Models {
		if m.Loaded == nil {
			t.Fatalf("%s: loaded should be known", m.ID)
		}
		got[m.ID] = *m.Loaded
	}
	if !got["m1:latest"] || got["m2:8b"] {
		t.Errorf("loaded = %v, want only m1:latest", got)
	}

	// If that route doesn't answer, loaded stays unknown.
	bare := fakeEngine(t, map[string]string{"/v1/models": `{"object":"list","data":[{"id":"m1"}]}`})
	res = factDetector(t, `"/api/ps models[].name"`).Detect(context.Background(), []string{bare.URL})
	if len(res.Engines) != 1 || res.Engines[0].Models[0].Loaded != nil {
		t.Errorf("loaded should stay unknown: %+v", res.Engines)
	}
}

func TestDetectCanRunFromSeveralPlacesAtOnce(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/v1/models": plainModels, "/api/v1/models": lmstudioNative})
	d := newDetector(t)
	d.Timeout = 0 // "use the default": Detect must not write the default back
	done := make(chan Result)
	for i := 0; i < 8; i++ {
		go func() { done <- d.Detect(context.Background(), []string{srv.URL}) }()
	}
	for i := 0; i < 8; i++ {
		if res := <-done; len(res.Engines) != 1 || res.Engines[0].ID != "lmstudio" {
			t.Errorf("call %d: %+v", i, res.Engines)
		}
	}
	if d.Timeout != 0 {
		t.Errorf("Detect changed the Detector's timeout to %v", d.Timeout)
	}
}

// Hint ports are probed on the hosts given (127.0.0.1 by default), and
// nowhere when SkipHintPorts is set.
func TestHintPorts(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/v1/models": plainModels})
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	body := strings.Replace(strings.Replace(factKind, "LOADED", "null", 1), `"hintPorts": []`, `"hintPorts": [`+port+`]`, 1)
	adapters, err := adapter.LoadFS(fstest.MapFS{"facts/adapter.json": {Data: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	d := &Detector{Adapters: adapters, Hosts: []string{host}, Timeout: 2 * time.Second}
	if res := d.Detect(context.Background(), nil); len(res.Engines) != 1 || res.Engines[0].URL != srv.URL {
		t.Fatalf("the hint port should be found: %+v", res.Engines)
	}
	d.SkipHintPorts = true
	if res := d.Detect(context.Background(), nil); len(res.Engines) != 0 {
		t.Fatalf("SkipHintPorts must probe nothing: %+v", res.Engines)
	}
}

func TestAModelListWithoutAListIsNotAServer(t *testing.T) {
	for _, body := range []string{`{"error":"unauthorized"}`, `{"data":"none"}`, `{"data":{"id":"x"}}`, `[]`} {
		srv := fakeEngine(t, map[string]string{"/v1/models": body})
		if res := newDetector(t).Detect(context.Background(), []string{srv.URL}); len(res.Engines) != 0 {
			t.Errorf("/v1/models answering %s must not count as a server: %+v", body, res.Engines)
		}
	}
}

func TestNothingRunning(t *testing.T) {
	srv := fakeEngine(t, map[string]string{}) // answers 404 to everything
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 0 {
		t.Fatalf("want no servers, got %+v", res.Engines)
	}
}

func TestAnswersThatAreNotJSONAreIgnored(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/v1/models": "<html>not a model server</html>"})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 0 {
		t.Fatalf("a web page must not count as a server: %+v", res.Engines)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	target := fakeEngine(t, map[string]string{"/v1/models": plainModels})
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	res := newDetector(t).Detect(context.Background(), []string{redirector.URL})
	if len(res.Engines) != 0 {
		t.Fatalf("a redirect must not be followed: %+v", res.Engines)
	}
}

func TestServerThatNeverAnswersTimesOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)

	d := newDetector(t)
	d.Timeout = 100 * time.Millisecond
	start := time.Now()
	res := d.Detect(context.Background(), []string{slow.URL})
	if len(res.Engines) != 0 {
		t.Fatalf("want no servers, got %+v", res.Engines)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("detection should give up quickly, took %v", time.Since(start))
	}
}

// The LM Studio API's own list says which models are embedding models (left
// out: they can't chat) and how much context each has. Its OpenAI list names
// every downloaded model, both kinds alike.
func TestLMStudioLeavesOutEmbeddingModels(t *testing.T) {
	srv := fakeEngine(t, map[string]string{
		"/v1/models":     `{"object":"list","data":[{"id":"chat-model"},{"id":"embed-model"}]}`,
		"/api/v1/models": `{"models":[{"type":"llm","key":"chat-model","max_context_length":32768,"loaded_instances":[]},{"type":"embedding","key":"embed-model","max_context_length":2048}]}`,
	})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || res.Engines[0].ID != "lmstudio" {
		t.Fatalf("want lmstudio, got %+v", res.Engines)
	}
	ms := res.Engines[0].Models
	if len(ms) != 1 || ms[0].ID != "chat-model" || ms[0].ContextWindow != 32768 {
		t.Errorf("want only chat-model with 32768 context, got %+v", ms)
	}
}

// A model named by a file path would put a host path into the box's harness
// config; names that only contain a slash are not paths.
func TestFileNamedModels(t *testing.T) {
	bs := string(rune(92)) // a backslash; drive paths are built from pieces
	for id, want := range map[string]bool{
		"D:" + bs + "models" + bs + "big-model.gguf":         true,
		"E:" + bs + "models" + bs + "big-model":              true,
		"C:" + "/models/big-model":                           true,
		"/models/big-model.gguf":                             true,
		"/models/big-model":                                  true,
		bs + bs + "server" + bs + "share" + bs + "big-model": true,
		"~/models/big-model":                                 true,
		"big-model.GGUF":                                     true,
		"weights.safetensors":                                true,
		"model.bin":                                          true,
		"big-model":                                          false,
		"org/big-model":                                      false,
		"hf.co/org/big-model:Q4_K_M":                         false,
		"big-model:latest":                                   false,
		"a:b":                                                false,
	} {
		if got := isFilePath(id); got != want {
			t.Errorf("isFilePath(%q) = %v, want %v", id, got, want)
		}
	}
	e := Engine{ID: "openai", Name: "OpenAI-compatible API", URL: "http://127.0.0.1:18080", Models: []Model{{ID: "/models/big-model.gguf"}, {ID: "big-model"}}}
	if got := FileNamed(e); len(got) != 1 || got[0] != "/models/big-model.gguf" {
		t.Errorf("FileNamed: %v", got)
	}
	if w := FileNamedWarning(e); !strings.Contains(w, "http://127.0.0.1:18080 names 1 model(s) by a file path") || !strings.Contains(w, "a short name (an alias)") {
		t.Errorf("warning: %q", w)
	}
	if w := FileNamedWarning(Engine{Models: []Model{{ID: "big-model"}}}); w != "" {
		t.Errorf("no file-named models, but warned: %q", w)
	}
}

// An Anthropic-compatible server requires Anthropic-Version and returns
// has_more on /v1/models, beating openai by specificity.
func TestAnthropicServerIsDetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Anthropic-Version") != anthropicVersion {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"missing anthropic-version"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicModels))
	}))
	t.Cleanup(srv.Close)

	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || res.Engines[0].ID != "anthropic" || res.Engines[0].Name != "Anthropic-compatible API" {
		t.Fatalf("want anthropic, got %+v", res.Engines)
	}
	if m := res.Engines[0].Models; len(m) != 1 || m[0].ID != "big-model" {
		t.Errorf("unexpected models: %+v", m)
	}
}

// A server that names its model by the file it loaded is flagged.
func TestFilePathModelIsFlagged(t *testing.T) {
	srv := fakeEngine(t, map[string]string{
		"/v1/models": `{"object":"list","data":[{"id":"/models/big-model.gguf"}]}`,
	})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || len(FileNamed(res.Engines[0])) != 1 {
		t.Fatalf("want one file-named model, got %+v", res.Engines)
	}
}
