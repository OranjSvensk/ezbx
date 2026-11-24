package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/detect"
)

const testKey = "kv-docs-example-1"

// lockedEngine answers like an OpenAI-compatible server, but only when the
// key is sent. It counts every request.
func lockedEngine(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m","object":"model"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func keyConfig(t *testing.T, url, keyEnv string) *config.Config {
	t.Helper()
	text := "version = 1\n[box.t]\nworkspace = '/w'\nharnesses = ['pi']\n[engine]\napi = 'openai'\nurl = '" + url + "'\n"
	if keyEnv != "" {
		text += "key_env = '" + keyEnv + "'\n"
	}
	c, err := config.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChooseEngineSendsTheConfiguredKey(t *testing.T) {
	srv, _ := lockedEngine(t)
	t.Setenv("EZBX_TEST_KEY", testKey)
	eng, err := chooseEngine(context.Background(), keyConfig(t, srv.URL, "EZBX_TEST_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	if eng.ID != "openai" || len(eng.Models) != 1 || eng.URL != srv.URL {
		t.Errorf("%+v", eng)
	}
}

// A missing key is reported before the server is contacted at all.
func TestChooseEngineChecksTheKeyFirst(t *testing.T) {
	srv, n := lockedEngine(t)
	t.Setenv("EZBX_TEST_KEY", "")
	_, err := chooseEngine(context.Background(), keyConfig(t, srv.URL, "EZBX_TEST_KEY"))
	if err == nil || !strings.Contains(err.Error(), "EZBX_TEST_KEY") || !strings.Contains(err.Error(), "empty or not set") {
		t.Errorf("%v", err)
	}
	if n.Load() != 0 {
		t.Errorf("the server was asked %d time(s)", n.Load())
	}
}

func TestChooseEngineExplainsARefusal(t *testing.T) {
	srv, _ := lockedEngine(t)
	t.Setenv("EZBX_TEST_KEY", "kv-docs-wrong")
	_, err := chooseEngine(context.Background(), keyConfig(t, srv.URL, "EZBX_TEST_KEY"))
	if err == nil || !strings.Contains(err.Error(), "refused the key in EZBX_TEST_KEY") || strings.Contains(err.Error(), "kv-docs-wrong") {
		t.Errorf("wrong key: %v", err)
	}

	_, err = chooseEngine(context.Background(), keyConfig(t, srv.URL, ""))
	if err == nil || !strings.Contains(err.Error(), "asks for a key") || !strings.Contains(err.Error(), "key_env") {
		t.Errorf("no key: %v", err)
	}
}

// openServer answers like a plain OpenAI-compatible server, with no key.
func openServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m","object":"model"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func engineConfig(t *testing.T, engine string) *config.Config {
	t.Helper()
	c, err := config.Parse("version = 1\n[box.t]\nworkspace = '/w'\nharnesses = ['pi']\n" + engine)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChooseEngineByAPI(t *testing.T) {
	srv := openServer(t)
	ctx := context.Background()

	// auto takes the kind the server answers.
	if eng, err := chooseEngine(ctx, engineConfig(t, "[engine]\nurl = '"+srv.URL+"'\n")); err != nil || eng.ID != "openai" {
		t.Errorf("auto: %+v %v", eng, err)
	}
	// A kind the server doesn't speak is named in the error.
	_, err := chooseEngine(ctx, engineConfig(t, "[engine]\napi = 'lmstudio'\nurl = '"+srv.URL+"'\n"))
	if err == nil || !strings.Contains(err.Error(), "with the LM Studio API") {
		t.Errorf("lmstudio at a plain server: %v", err)
	}
	// An unknown kind lists the real ones.
	_, err = chooseEngine(ctx, engineConfig(t, "[engine]\napi = 'demo'\nurl = '"+srv.URL+"'\n"))
	if err == nil || !strings.Contains(err.Error(), `engine.api "demo": use auto or one of anthropic, lmstudio, openai`) {
		t.Errorf("unknown kind: %v", err)
	}
	// Without a url nothing is scanned: ezbx up says what to set.
	_, err = chooseEngine(ctx, engineConfig(t, ""))
	if err == nil || !strings.Contains(err.Error(), "no model server is set") || !strings.Contains(err.Error(), "ezbx engine detect") {
		t.Errorf("no url: %v", err)
	}
	// Nothing answering at the url says how to fix it.
	srv.Close()
	_, err = chooseEngine(ctx, engineConfig(t, "[engine]\nurl = '"+srv.URL+"'\n"))
	if err == nil || !strings.Contains(err.Error(), "Nothing answered at "+srv.URL+" with an OpenAI- or Anthropic-compatible API") {
		t.Errorf("closed server: %v", err)
	}
}

func TestChooseEngineAnthropicWithKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != testKey || r.Header.Get("Anthropic-Version") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"big-model","type":"model"}],"has_more":false}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("EZBX_TEST_KEY", testKey)
	eng, err := chooseEngine(context.Background(), engineConfig(t, "[engine]\napi = 'auto'\nurl = '"+srv.URL+"'\nkey_env = 'EZBX_TEST_KEY'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if eng.ID != "anthropic" || len(eng.Models) != 1 || eng.Models[0].ID != "big-model" {
		t.Errorf("got %+v", eng)
	}

	t.Setenv("EZBX_TEST_KEY", "kv-docs-wrong")
	_, err = chooseEngine(context.Background(), engineConfig(t, "[engine]\napi = 'anthropic'\nurl = '"+srv.URL+"'\nkey_env = 'EZBX_TEST_KEY'\n"))
	if err == nil || !strings.Contains(err.Error(), "refused the key in EZBX_TEST_KEY") || !strings.Contains(err.Error(), "Anthropic-compatible API") {
		t.Errorf("wrong key: %v", err)
	}
}

// decoyOnHintPort starts a server and makes it the only API kind's hint
// port, so a scan of this computer would find it. It counts requests.
func decoyOnHintPort(t *testing.T) *atomic.Int32 {
	t.Helper()
	var decoyHits atomic.Int32
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoyHits.Add(1)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(decoy.Close)
	_, port, _ := strings.Cut(strings.TrimPrefix(decoy.URL, "http://"), ":")
	kind := `{"schema": 1, "id": "openai", "name": "OpenAI-compatible API",
	  "detect": {"hintPorts": [` + port + `], "probes": [{"path": "/v1/models", "expect": {"hasLists": ["data"]}}]},
	  "openai": {"basePath": "/v1", "modelsPath": "/v1/models", "chatPath": "/v1/chat/completions"},
	  "models": {"id": "data[].id", "contextWindow": null, "input": null, "loaded": null},
	  "auth": {"mode": "none", "keyEnv": null, "note": ""}, "capabilities": [], "verified": {"status": "docs-only", "on": null, "notes": ""}}`
	old := kindsFS
	kindsFS = fstest.MapFS{"openai/adapter.json": {Data: []byte(kind)}}
	t.Cleanup(func() { kindsFS = old })
	return &decoyHits
}

// ezbx up contacts only engine.url: a server on one of the kinds' hint ports
// is never asked, even with api = auto.
func TestChooseEngineNeverScans(t *testing.T) {
	hits := decoyOnHintPort(t)
	srv := openServer(t)
	eng, err := chooseEngine(context.Background(), engineConfig(t, "[engine]\nurl = '"+srv.URL+"'\n"))
	if err != nil || eng.URL != srv.URL {
		t.Fatalf("%+v %v", eng, err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("a server on a hint port was asked %d time(s)", n)
	}
}

// The doctor checks the configured server and the gateway's upstream, and
// nothing else; without a config only the upstream.
func TestDoctorDetectNeverScans(t *testing.T) {
	hits := decoyOnHintPort(t)
	configured, upstream := openServer(t), openServer(t)
	find, err := doctorDetect(engineConfig(t, "[engine]\nurl = '"+configured.URL+"'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res := find(context.Background(), []string{upstream.URL}); len(res.Engines) != 2 {
		t.Errorf("want the configured server and the upstream: %+v", res.Engines)
	}
	find, _ = doctorDetect(nil)
	if res := find(context.Background(), []string{upstream.URL}); len(res.Engines) != 1 || res.Engines[0].URL != upstream.URL {
		t.Errorf("no config: %+v", res.Engines)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("a server on a hint port was asked %d time(s)", n)
	}
}

func TestRefusedHint(t *testing.T) {
	if refusedHint(nil, "") != "" {
		t.Error("nothing refused")
	}
	if got := refusedHint([]detect.Refusal{{URL: "http://127.0.0.1:18080"}}, ""); !strings.Contains(got, "18080 asks for a key") || !strings.Contains(got, "key_env = 'NAME' under [engine]") {
		t.Error(got)
	}
	if got := refusedHint([]detect.Refusal{{URL: "http://127.0.0.1:53000", KeySent: true}}, "K"); !strings.Contains(got, "53000 refused the key in K") {
		t.Error(got)
	}
}
