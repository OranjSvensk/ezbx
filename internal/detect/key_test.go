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
