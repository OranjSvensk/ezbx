package detect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/OranjSvensk/ezbx/internal/adapter"
	"github.com/OranjSvensk/ezbx/providers"
)

const testKey = "kv-docs-example-1"

// recorder is a server that remembers the Authorization header of every
// request. With want set, it answers 401 unless that key is sent.
type recorder struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newRecorder(t *testing.T, want string, status int) *recorder {
	t.Helper()
	r := &recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.mu.Unlock()
		if want != "" && req.Header.Get("Authorization") != "Bearer "+want {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"no key"}`))
			return
		}
		if req.URL.Path != "/v1/models" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(plainModels))
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

// The key goes to its own address on every probe, and nowhere else: not to
// another address given, not to one the port scan finds.
func TestKeyGoesOnlyToItsOwnAddress(t *testing.T) {
	keyed := newRecorder(t, testKey, http.StatusUnauthorized)
	other := newRecorder(t, "", 0)
	listed := newRecorder(t, "", 0) // found on a hint port

	host, port, _ := strings.Cut(strings.TrimPrefix(listed.URL, "http://"), ":")
	body := strings.Replace(strings.Replace(factKind, "LOADED", "null", 1), `"hintPorts": []`, `"hintPorts": [`+port+`]`, 1)
	adapters, err := adapter.LoadFS(fstest.MapFS{"facts/adapter.json": {Data: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	d := &Detector{Adapters: adapters, Hosts: []string{host}, Timeout: 2 * time.Second}
	d.Keys = map[string]string{keyed.URL + "/": testKey} // a trailing slash still matches
	res := d.Detect(context.Background(), []string{keyed.URL, other.URL})

	if len(res.Engines) != 3 || len(res.Refused) != 0 {
		t.Fatalf("want all three found: %+v refused=%+v", res.Engines, res.Refused)
	}
	got := keyed.seen()
	if len(got) == 0 {
		t.Fatal("the keyed address was never asked")
	}
	for _, a := range got {
		if a != "Bearer "+testKey {
			t.Errorf("the keyed address got %q", a)
		}
	}
	for name, r := range map[string]*recorder{"other": other, "listed": listed} {
		for _, a := range r.seen() {
			if a != "" {
				t.Errorf("%s address got a key: %q", name, a)
			}
		}
	}
}

// An address that only differs in its path is another address.
func TestKeyNeedsTheExactAddress(t *testing.T) {
	keyed := newRecorder(t, "", 0)
	d := newDetector(t)
	d.Keys = map[string]string{keyed.URL + "/other": testKey}
	d.Detect(context.Background(), []string{keyed.URL})
	for _, a := range keyed.seen() {
		if a != "" {
			t.Errorf("got a key meant for another path: %q", a)
		}
	}
}

func TestAServerThatAsksForAKeyIsReported(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		locked := newRecorder(t, testKey, status)

		res := newDetector(t).Detect(context.Background(), []string{locked.URL})
		if len(res.Engines) != 0 || len(res.Refused) != 1 || res.Refused[0].URL != locked.URL || res.Refused[0].KeySent {
			t.Errorf("%d without a key: %+v %+v", status, res.Engines, res.Refused)
		}

		d := newDetector(t)
		d.Keys = map[string]string{locked.URL: "kv-docs-wrong"}
		res = d.Detect(context.Background(), []string{locked.URL})
		if len(res.Engines) != 0 || len(res.Refused) != 1 || !res.Refused[0].KeySent {
			t.Errorf("%d with a wrong key: %+v %+v", status, res.Engines, res.Refused)
		}
	}
}

// A server that is found isn't also reported as refusing, even when some of
// the other kinds' routes answer 401.
func TestAFoundServerIsNotReportedAsRefusing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(plainModels))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || len(res.Refused) != 0 {
		t.Errorf("%+v refused=%+v", res.Engines, res.Refused)
	}
}

// Other errors are not a request for a key.
func TestOtherErrorsAreNotRefusals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	if res := newDetector(t).Detect(context.Background(), []string{srv.URL}); len(res.Refused) != 0 {
		t.Errorf("%+v", res.Refused)
	}
}

// An Anthropic-compatible server expects X-Api-Key + Anthropic-Version (and
// rejects Authorization: Bearer with 401). The key still goes only to its
// own address, and the Bearer 401 never hides the X-Api-Key match.
func TestAnthropicKeyUsesXApiKeyOnlyOnOwnAddress(t *testing.T) {
	var mu sync.Mutex
	type reqHeaders struct{ version, xKey, auth string }
	var gotKeyed []reqHeaders
	var gotOther []string

	keyed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotKeyed = append(gotKeyed, reqHeaders{
			version: r.Header.Get("Anthropic-Version"),
			xKey:    r.Header.Get("X-Api-Key"),
			auth:    r.Header.Get("Authorization"),
		})
		mu.Unlock()
		if r.Header.Get("X-Api-Key") != testKey || r.Header.Get("Anthropic-Version") != anthropicVersion {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicModels))
	}))
	t.Cleanup(keyed.Close)

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotOther = append(gotOther, r.Header.Get("X-Api-Key")+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicModels))
	}))
	t.Cleanup(other.Close)

	// Without a key, keyed reports a refusal with KeySent=false.
	res := newDetector(t).Detect(context.Background(), []string{keyed.URL})
	if len(res.Engines) != 0 || len(res.Refused) != 1 || res.Refused[0].URL != keyed.URL || res.Refused[0].KeySent {
		t.Fatalf("without key: engines=%+v refused=%+v", res.Engines, res.Refused)
	}

	// With the wrong key, keyed reports a refusal with KeySent=true.
	d := newDetector(t)
	d.Keys = map[string]string{keyed.URL: "kv-docs-wrong"}
	res = d.Detect(context.Background(), []string{keyed.URL})
	if len(res.Engines) != 0 || len(res.Refused) != 1 || !res.Refused[0].KeySent {
		t.Fatalf("wrong key: engines=%+v refused=%+v", res.Engines, res.Refused)
	}

	// With the right key, keyed is found as anthropic, refused is empty, and
	// the key never reaches other or leaks into an Authorization header.
	mu.Lock()
	gotKeyed = nil
	mu.Unlock()
	d = newDetector(t)
	d.Keys = map[string]string{keyed.URL: testKey}
	res = d.Detect(context.Background(), []string{keyed.URL, other.URL})
	if len(res.Engines) != 2 || len(res.Refused) != 0 {
		t.Fatalf("with key: engines=%+v refused=%+v", res.Engines, res.Refused)
	}
	foundAnthropic := false
	for _, e := range res.Engines {
		if e.URL == keyed.URL && e.ID == "anthropic" {
			foundAnthropic = true
		}
	}
	if !foundAnthropic {
		t.Fatalf("keyed server not found as anthropic: %+v", res.Engines)
	}
	for _, h := range gotKeyed {
		if h.xKey == testKey && h.auth != "" {
			t.Errorf("Anthropic request also carried Authorization: %+v", h)
		}
		if h.auth == "Bearer "+testKey && h.xKey != "" {
			t.Errorf("OpenAI request also carried X-Api-Key: %+v", h)
		}
	}
	for _, s := range gotOther {
		if s != "" {
			t.Errorf("other server received a key: %q", s)
		}
	}

	// Even when an OpenAI Bearer adapter probes /v1/models first and gets 401,
	// the Anthropic X-Api-Key probe still runs and finds the server.
	all, err := adapter.LoadFS(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	var byID = map[string]adapter.Adapter{}
	for _, a := range all {
		byID[a.ID] = a
	}
	dOrdered := &Detector{
		Adapters:      []adapter.Adapter{byID["openai"], byID["anthropic"]},
		SkipHintPorts: true,
		Timeout:       2 * time.Second,
		Keys:          map[string]string{keyed.URL: testKey},
	}
	if res := dOrdered.Detect(context.Background(), []string{keyed.URL}); len(res.Engines) != 1 || res.Engines[0].ID != "anthropic" {
		t.Fatalf("openai 401 poisoned anthropic probe: engines=%+v refused=%+v", res.Engines, res.Refused)
	}
}
