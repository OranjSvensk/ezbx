package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/detect"
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
	text := "version = 1\n[box.t]\nworkspace = '/w'\nharnesses = ['pi']\n[engine]\nuse = 'openai'\n[engine.openai]\nurl = '" + url + "'\n"
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
	eng, ov, err := chooseEngine(context.Background(), keyConfig(t, srv.URL, "EZBX_TEST_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	if eng.ID != "openai" || len(eng.Models) != 1 || ov.KeyEnv != "EZBX_TEST_KEY" {
		t.Errorf("%+v %+v", eng, ov)
	}
}

// A missing key is reported before the engine is contacted at all.
func TestChooseEngineChecksTheKeyFirst(t *testing.T) {
	srv, n := lockedEngine(t)
	t.Setenv("EZBX_TEST_KEY", "")
	_, _, err := chooseEngine(context.Background(), keyConfig(t, srv.URL, "EZBX_TEST_KEY"))
	if err == nil || !strings.Contains(err.Error(), "EZBX_TEST_KEY") || !strings.Contains(err.Error(), "empty or not set") {
		t.Errorf("%v", err)
	}
	if n.Load() != 0 {
		t.Errorf("the engine was asked %d time(s)", n.Load())
	}
}

func TestChooseEngineExplainsARefusal(t *testing.T) {
	srv, _ := lockedEngine(t)
	t.Setenv("EZBX_TEST_KEY", "kv-docs-wrong")
	_, _, err := chooseEngine(context.Background(), keyConfig(t, srv.URL, "EZBX_TEST_KEY"))
	if err == nil || !strings.Contains(err.Error(), "refused the key in EZBX_TEST_KEY") || strings.Contains(err.Error(), "kv-docs-wrong") {
		t.Errorf("wrong key: %v", err)
	}

	_, _, err = chooseEngine(context.Background(), keyConfig(t, srv.URL, ""))
	if err == nil || !strings.Contains(err.Error(), "asks for a key") || !strings.Contains(err.Error(), "key_env") {
		t.Errorf("no key: %v", err)
	}
}

func TestRefusedHint(t *testing.T) {
	if refusedHint(nil, nil, "auto") != "" {
		t.Error("nothing refused")
	}
	got := refusedHint([]detect.Refusal{{URL: "http://127.0.0.1:18080"}, {URL: "http://127.0.0.1:53000", KeySent: true}},
		map[string]string{"http://127.0.0.1:53000": "K"}, "auto")
	if !strings.Contains(got, "18080 asks for a key") || !strings.Contains(got, "53000 refused the key in K") || !strings.Contains(got, "[engine.ID]") {
		t.Error(got)
	}
	if got := refusedHint([]detect.Refusal{{URL: "http://127.0.0.1:18080"}}, nil, "openai"); !strings.Contains(got, "[engine.openai]") {
		t.Error(got)
	}
}
