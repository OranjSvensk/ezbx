// Package detect finds model servers and the API each one speaks.
//
// Ports are only hints. A server counts as found when it answers an API
// kind's probes with the fields that kind expects. See
// docs/engine-adapters.md.
package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OranjSvensk/ezbx/internal/adapter"
	"github.com/OranjSvensk/ezbx/internal/jsonpath"
)

// maxBody stops a misbehaving server from making us read forever.
const maxBody = 1 << 20 // 1 MiB

// anthropicVersion is sent to Anthropic-compatible servers on every request.
const anthropicVersion = "2023-06-01"

// fetchKey separates cached replies by whether Anthropic auth headers were
// sent, so a refusal on an OpenAI Bearer probe never hides an Anthropic reply.
type fetchKey struct {
	path      string
	anthropic bool
}

// Detector holds everything detection needs. Detect doesn't change it, so
// one Detector can be used by several callers at once.
type Detector struct {
	Adapters      []adapter.Adapter
	Timeout       time.Duration // per request; 0 means 500ms
	Hosts         []string      // hosts to probe for hint ports; nil means 127.0.0.1
	SkipHintPorts bool          // check only the given addresses (ezbx up, doctor)

	// Keys maps an address to the key of the server configured there. A key
	// is sent only to that exact address (scheme, host, port and path, after
	// trailing slashes are dropped), never to another candidate, so the port
	// scan never sees it. Redirects are never followed.
	Keys map[string]string
}

// Model is one model an engine reports.
type Model struct {
	ID            string   `json:"id"`
	ContextWindow int      `json:"contextWindow,omitempty"`
	Input         []string `json:"input,omitempty"`
	Loaded        *bool    `json:"loaded,omitempty"`
}

// Engine is one model server that was found. ID and Name are its API kind.
type Engine struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	Models      []Model  `json:"models"`
	Specificity int      `json:"specificity"`
	AlsoMatched []string `json:"alsoMatched,omitempty"`
	Verified    string   `json:"verified"`
}

// Refusal is an address that answered 401 or 403 and matched no API kind:
// most likely a server that wants a key.
type Refusal struct {
	URL     string `json:"url"`
	KeySent bool   `json:"keySent"` // true: it refused the key it was given
}

// Result is what Detect returns.
type Result struct {
	Engines []Engine  `json:"engines"`
	Refused []Refusal `json:"refused,omitempty"`
}

// scan holds what one Detect call needs, so the Detector itself never changes.
type scan struct {
	d       *Detector
	client  *http.Client
	timeout time.Duration
	keys    map[string]string // trimmed address -> key

	mu      sync.Mutex
	refused map[string]bool // addresses that answered 401 or 403
}

// trim is how addresses are compared: without trailing slashes.
func trim(raw string) string { return strings.TrimRight(raw, "/") }

// Detect looks for model servers. extra holds addresses the user gave.
func (d *Detector) Detect(ctx context.Context, extra []string) Result {
	s := &scan{d: d, timeout: d.Timeout, keys: map[string]string{}, refused: map[string]bool{}}
	for u, k := range d.Keys {
		if k != "" {
			s.keys[trim(u)] = k
		}
	}
	if s.timeout <= 0 {
		s.timeout = 500 * time.Millisecond
	}
	s.client = &http.Client{
		Timeout:   s.timeout,
		Transport: &http.Transport{Proxy: nil}, // probes go straight to this computer
		// A redirect could point somewhere else entirely, so never follow one.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	var res Result
	candidates := map[string]bool{}
	add := func(raw string) {
		candidates[trim(raw)] = true
	}

	// 1. Probe the usual ports on this computer only.
	if !d.SkipHintPorts {
		hosts := d.Hosts
		if len(hosts) == 0 {
			hosts = []string{"127.0.0.1"}
		}
		for _, a := range d.Adapters {
			for _, port := range a.Detect.HintPorts {
				for _, h := range hosts {
					add("http://" + net.JoinHostPort(h, fmt.Sprint(port)))
				}
			}
		}
	}

	// 2. Addresses the user gave are trusted as given.
	for _, u := range extra {
		add(u)
	}

	// 3. Check every candidate at the same time.
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for base := range candidates {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			if eng, ok := s.check(ctx, base); ok {
				mu.Lock()
				res.Engines = append(res.Engines, eng)
				mu.Unlock()
			}
		}(base)
	}
	wg.Wait()

	// 4. Addresses that refused us and matched nothing: say why.
	found := map[string]bool{}
	for _, e := range res.Engines {
		found[e.URL] = true
	}
	for base := range s.refused {
		if !found[base] {
			res.Refused = append(res.Refused, Refusal{URL: base, KeySent: s.keys[base] != ""})
		}
	}
	sort.Slice(res.Refused, func(i, j int) bool { return res.Refused[i].URL < res.Refused[j].URL })

	sort.Slice(res.Engines, func(i, j int) bool {
		if res.Engines[i].ID != res.Engines[j].ID {
			return res.Engines[i].ID < res.Engines[j].ID
		}
		return res.Engines[i].URL < res.Engines[j].URL
	})
	return res
}

type match struct {
	adapter     adapter.Adapter
	specificity int
}

// check decides which API kind, if any, answers at base.
func (s *scan) check(ctx context.Context, base string) (Engine, bool) {
	cache := map[fetchKey]any{} // answers already fetched from this address

	var matches []match
	for _, a := range s.d.Adapters {
		if spec, ok := s.matches(ctx, base, a, cache); ok {
			matches = append(matches, match{a, spec})
		}
	}
	if len(matches) == 0 {
		return Engine{}, false
	}

	// More specific adapters win. Ties go to the alphabetically first id.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].specificity != matches[j].specificity {
			return matches[i].specificity > matches[j].specificity
		}
		return matches[i].adapter.ID < matches[j].adapter.ID
	})

	best := matches[0]
	eng := Engine{
		ID:          best.adapter.ID,
		Name:        best.adapter.Name,
		URL:         base,
		Models:      []Model{},
		Specificity: best.specificity,
		Verified:    best.adapter.Verified.Status,
	}
	for _, m := range matches[1:] {
		eng.AlsoMatched = append(eng.AlsoMatched, m.adapter.ID)
	}
	if doc, ok := s.fetch(ctx, base, best.adapter.ListPath(), best.adapter.IsAnthropicAuth(), cache); ok {
		eng.Models = readModels(doc, best.adapter.Models)
		s.markLoaded(ctx, base, best.adapter.Models, best.adapter.IsAnthropicAuth(), eng.Models, cache)
	}
	return eng, true
}

// matches runs one adapter's probes against base. Every required probe must
// answer with all of its expected fields and lists. The score counts them.
func (s *scan) matches(ctx context.Context, base string, a adapter.Adapter, cache map[fetchKey]any) (int, bool) {
	score := 0
	for _, p := range a.Detect.Probes {
		doc, ok := s.fetch(ctx, base, p.Path, a.IsAnthropicAuth(), cache)
		if ok {
			for _, field := range p.Expect.HasFields {
				if !jsonpath.Has(doc, field) {
					ok = false
					break
				}
			}
		}
		if ok {
			for _, l := range p.Expect.HasLists {
				if !jsonpath.IsList(doc, l) {
					ok = false
					break
				}
			}
		}
		if !ok {
			if p.Optional {
				continue
			}
			return 0, false
		}
		score += len(p.Expect.HasFields) + len(p.Expect.HasLists)
	}
	return score, true
}

// fetch does a plain GET and decodes the JSON answer. It sends no
// information about the user, and a key only to the address it belongs to.
func (s *scan) fetch(ctx context.Context, base, path string, anthropic bool, cache map[fetchKey]any) (any, bool) {
	ck := fetchKey{path: path, anthropic: anthropic}
	if doc, seen := cache[ck]; seen {
		return doc, doc != nil
	}
	cache[ck] = nil

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ezbx-detect")
	if anthropic {
		req.Header.Set("Anthropic-Version", anthropicVersion)
		if k := s.keys[base]; k != "" {
			req.Header.Set("X-Api-Key", k)
		}
	} else if k := s.keys[base]; k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		s.mu.Lock()
		s.refused[base] = true
		s.mu.Unlock()
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}

	var doc any
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&doc); err != nil {
		return nil, false
	}
	cache[ck] = doc
	return doc, true
}

// markLoaded handles the "/route list[].field" form of models.loaded: a model
// is loaded when another route lists its id. If that route doesn't answer,
// loaded stays unknown rather than being reported as false.
func (s *scan) markLoaded(ctx context.Context, base string, spec adapter.Models, anthropic bool, models []Model, cache map[fetchKey]any) {
	route, path, ok := spec.LoadedIn()
	if !ok {
		return
	}
	doc, ok := s.fetch(ctx, base, route, anthropic, cache)
	if !ok {
		return
	}
	listed := map[string]bool{}
	for _, v := range jsonpath.Collect(doc, path) {
		if str, isStr := v.(string); isStr {
			listed[str] = true
		}
	}
	for i := range models {
		loaded := listed[models[i].ID]
		models[i].Loaded = &loaded
	}
}

// readModels turns an engine's model list into Model values, using only the
// facts the adapter says the engine reports.
func readModels(doc any, spec adapter.Models) []Model {
	out := []Model{}
	arrayPath, idPath, ok := jsonpath.SplitArray(spec.ID)
	if !ok {
		return out
	}
	list, _ := jsonpath.Get(doc, arrayPath)
	items, _ := list.([]any)

	for _, item := range items {
		id, _ := jsonpath.Get(item, idPath)
		name, isString := id.(string)
		if !isString || name == "" {
			continue
		}
		if skipped(item, arrayPath, spec.Skip) {
			continue
		}
		m := Model{ID: name}

		if v, ok := factAt(item, arrayPath, spec.ContextWindow); ok {
			if n, isNum := v.(float64); isNum {
				m.ContextWindow = int(n)
			}
		}
		if v, ok := factAt(item, arrayPath, spec.Input); ok {
			if list, isList := v.([]any); isList {
				for _, s := range list {
					if str, isStr := s.(string); isStr {
						m.Input = append(m.Input, str)
					}
				}
			}
		}
		if spec.Loaded != nil {
			if path, want, found := strings.Cut(*spec.Loaded, " == "); found {
				if rel, ok := relative(path, arrayPath); ok {
					if v, ok := jsonpath.Get(item, rel); ok {
						loaded := fmt.Sprint(v) == want
						m.Loaded = &loaded
					}
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// skipped reports whether the adapter's models.skip rule ("list[].field ==
// value") leaves this model out, such as an embedding model.
func skipped(item any, arrayPath string, rule *string) bool {
	if rule == nil {
		return false
	}
	path, want, _ := strings.Cut(*rule, " == ")
	rel, ok := relative(path, arrayPath)
	if !ok {
		return false
	}
	v, ok := jsonpath.Get(item, rel)
	return ok && fmt.Sprint(v) == want
}

// FileNamed lists the models a server names by a file path on the host, as
// some servers do unless the model is given a short name (an alias). ezbx
// writes model ids into the harness config inside the box, so such a name
// would show the box a path on the host.
func FileNamed(e Engine) []string {
	var out []string
	for _, m := range e.Models {
		if isFilePath(m.ID) {
			out = append(out, m.ID)
		}
	}
	return out
}

// isFilePath reports whether a model id looks like a path to a file rather
// than a name: a model file's extension, a drive letter, or a leading slash.
// Names such as "org/model" or "hf.co/org/model:Q4" are not paths.
func isFilePath(id string) bool {
	low := strings.ToLower(id)
	for _, ext := range []string{".gguf", ".bin", ".safetensors"} {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	if len(id) >= 3 && id[1] == ':' && (id[2] == '\\' || id[2] == '/') {
		return true
	}
	return strings.HasPrefix(id, "/") || strings.HasPrefix(id, "\\") || strings.HasPrefix(id, "~/")
}

// FileNamedWarning explains FileNamed models and the fix, or is "" when there
// are none.
func FileNamedWarning(e Engine) string {
	names := FileNamed(e)
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("The server at %s names %d model(s) by a file path on this computer, and that path would be written into the harness config inside the box. To keep it out, give the model a short name (an alias) in the server's settings", e.URL, len(names))
}

// factAt reads one fact for one list item, when the adapter names a path.
func factAt(item any, arrayPath string, path *string) (any, bool) {
	if path == nil {
		return nil, false
	}
	rel, ok := relative(*path, arrayPath)
	if !ok {
		return nil, false
	}
	return jsonpath.Get(item, rel)
}

// relative turns "data[].meta.n_ctx" into "meta.n_ctx" for a list at "data".
func relative(path, arrayPath string) (string, bool) {
	prefix := arrayPath + "[]."
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}
