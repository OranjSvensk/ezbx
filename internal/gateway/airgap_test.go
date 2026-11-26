package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// airgapEnv is a gateway with its proxy in front of an engine whose
// /v1/stream sends one chunk and then waits, and whose /v1/slow answers only
// when released, so a test can change the mode in the middle of a reply.
type airgapEnv struct {
	g         *Gateway
	p         *Proxy
	srv       *httptest.Server
	logs      *syncBuffer
	calls     atomic.Int32
	cancelled chan struct{} // a request the engine was serving was cancelled
	started   chan struct{} // the engine has a request
}

func newAirgapEnv(t *testing.T, mode string) *airgapEnv {
	t.Helper()
	env := &airgapEnv{logs: &syncBuffer{}, cancelled: make(chan struct{}, 4), started: make(chan struct{}, 4)}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.calls.Add(1)
		// Go's server notices a closed connection only after the body is read.
		io.Copy(io.Discard, r.Body)
		switch r.URL.Path {
		case "/v1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: one\n\n")
			w.(http.Flusher).Flush()
		case "/v1/slow":
		default:
			io.WriteString(w, `{"object":"list","data":[{"id":"big-model"}]}`)
			return
		}
		env.started <- struct{}{}
		select {
		case <-r.Context().Done():
			env.cancelled <- struct{}{}
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(engine.Close)
	g, err := New(Config{Upstream: engine.URL, EngineID: "openai", Log: env.logs})
	if err != nil {
		t.Fatal(err)
	}
	p, err := g.NewProxy(ProxyConfig{Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	env.g, env.p = g, p
	env.srv = httptest.NewServer(g)
	t.Cleanup(env.srv.Close)
	return env
}

func (env *airgapEnv) lines(t *testing.T) []entry {
	t.Helper()
	var out []entry
	for _, l := range strings.Split(strings.TrimSpace(env.logs.String()), "\n") {
		if l == "" {
			continue
		}
		var e entry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("log line is not JSON: %q", l)
		}
		out = append(out, e)
	}
	return out
}

func waitFor(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting: %s", what)
	}
}

// In airgap mode the model route is refused before the engine is asked,
// and /healthz still answers, so the gateway stays healthy.
func TestAirgapRefusesTheModelRoute(t *testing.T) {
	env := newAirgapEnv(t, ModeAirgap)
	resp, err := http.Get(env.srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), `"network_airgap"`) || !strings.Contains(string(body), "ezbx net off") {
		t.Errorf("airgap: %d %s", resp.StatusCode, body)
	}
	if env.calls.Load() != 0 {
		t.Error("the engine was asked in airgap mode")
	}
	if resp.Header.Get("X-Ezbx-Engine") != "" {
		t.Error("a refusal from the gateway carries engine headers")
	}
	e := env.lines(t)
	if len(e) != 1 || e[0].Route != "model" || !e[0].Blocked || e[0].Reason != reasonAirgap || e[0].Status != http.StatusForbidden || e[0].Path != "/v1/models" {
		t.Errorf("log: %+v", e)
	}
	if resp, err := http.Get(env.srv.URL + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("healthz in airgap: %v %v", resp, err)
	}
	// Opening it again lets requests through.
	if _, err := env.p.SetNetwork(ModeOff, nil); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(env.srv.URL + "/v1/models")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("after off: %v %v", resp, err)
	}
	resp.Body.Close()
	if env.calls.Load() != 1 {
		t.Errorf("engine calls after off: %d", env.calls.Load())
	}
	if mode, _ := env.p.Network(); mode != ModeOff {
		t.Errorf("mode %q", mode)
	}
}

// The other modes leave the model route open.
func TestAirgapCanStartWithoutAnUpstream(t *testing.T) {
	g, err := New(Config{Airgap: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := g.NewProxy(ProxyConfig{Mode: ModeAirgap})
	if err != nil {
		t.Fatal(err)
	}
	check := func(path string, status int, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != status || !strings.Contains(w.Body.String(), body) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	check("/healthz", http.StatusOK, "")
	check("/v1/models", http.StatusForbidden, "network_airgap")
	w := httptest.NewRecorder()
	g.ready(w, httptest.NewRequest(http.MethodGet, "/admin/ready", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "no model server configured") {
		t.Fatalf("engine readiness: %d %s", w.Code, w.Body)
	}
	if _, err := p.SetNetwork(ModeOff, nil); err != nil {
		t.Fatal(err)
	}
	check("/v1/models", http.StatusServiceUnavailable, "engine_unconfigured")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[{"id":"fake-model"}]}`)
	}))
	defer upstream.Close()
	if err := g.SetUpstream(upstream.URL, "openai"); err != nil {
		t.Fatal(err)
	}
	check("/v1/models", http.StatusOK, "fake-model")
}

func TestModelRouteOpenOutsideAirgap(t *testing.T) {
	for _, m := range []string{ModeOff, ModeAllow, ModeOn} {
		env := newAirgapEnv(t, m)
		resp, err := http.Get(env.srv.URL + "/v1/models")
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %v %v", m, resp, err)
		}
		resp.Body.Close()
	}
}

// Switching to airgap ends a stream in the middle and cancels it at the
// engine; it is counted with the tunnels the change closed.
func TestAirgapEndsStreams(t *testing.T) {
	env := newAirgapEnv(t, ModeOn)
	resp, err := http.Get(env.srv.URL + "/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if line, _ := br.ReadString('\n'); line != "data: one\n" {
		t.Fatalf("first chunk %q", line)
	}
	waitFor(t, env.started, "the engine to get the stream")
	closed, err := env.p.SetNetwork(ModeAirgap, nil)
	if err != nil || closed != 1 {
		t.Fatalf("closed %d, %v", closed, err)
	}
	waitFor(t, env.cancelled, "the engine request to be cancelled")
	if rest, _ := io.ReadAll(br); strings.Contains(string(rest), "data:") {
		t.Errorf("more data after airgap: %q", rest)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		e := env.lines(t)
		if len(e) == 1 {
			if e[0].Closed != "network_tightened" || e[0].Blocked || e[0].End != "" {
				t.Errorf("log: %+v", e[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("log lines: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Nothing is left to close, and loosening closes nothing.
	if n, _ := env.p.SetNetwork(ModeAirgap, nil); n != 0 {
		t.Errorf("second airgap closed %d", n)
	}
	if n, _ := env.p.SetNetwork(ModeOff, nil); n != 0 {
		t.Errorf("off closed %d", n)
	}
}

// A request still waiting for the engine's first byte gets a clear refusal
// rather than an empty reply.
func TestAirgapEndsWaitingRequests(t *testing.T) {
	env := newAirgapEnv(t, ModeOff)
	done := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, env.srv.URL+"/v1/slow", strings.NewReader(`{"model":"big-model"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			done <- nil
			return
		}
		done <- resp
	}()
	waitFor(t, env.started, "the engine to get the request")
	if n, _ := env.p.SetNetwork(ModeAirgap, nil); n != 1 {
		t.Fatalf("closed %d", n)
	}
	var resp *http.Response
	select {
	case resp = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting request was not ended")
	}
	if resp == nil {
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "network_airgap") {
		t.Errorf("waiting request: %d %s", resp.StatusCode, body)
	}
	waitFor(t, env.cancelled, "the engine request to be cancelled")
}

// The gateway forgets finished requests, and a request ended by airgap is
// counted once.
func TestAirgapBookkeeping(t *testing.T) {
	g, err := New(Config{Upstream: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	var cancels atomic.Int32
	newInfo := func() *reqInfo { return &reqInfo{cancel: func() { cancels.Add(1) }} }
	done, open := newInfo(), newInfo()
	if !g.track(done) || !g.track(open) {
		t.Fatal("track refused outside airgap")
	}
	g.untrack(done)
	if n := g.setAirgap(true); n != 1 || cancels.Load() != 1 || !open.cut.Load() || done.cut.Load() {
		t.Fatalf("first airgap: %d ended, %d cancelled", n, cancels.Load())
	}
	if n := g.setAirgap(true); n != 0 || cancels.Load() != 1 {
		t.Errorf("second airgap: %d ended, %d cancelled", n, cancels.Load())
	}
	if g.track(newInfo()) {
		t.Error("track accepted a request in airgap")
	}
	if n := g.setAirgap(false); n != 0 || !g.track(newInfo()) {
		t.Error("the model route didn't open again")
	}
}
