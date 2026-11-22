package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/OranjSvensk/ezbx/internal/detect"
)

// Model holds the facts a harness config can use for one model.
type Model struct {
	ID            string
	Name          string
	ContextWindow int
	MaxTokens     int
	Reasoning     bool
	Input         []string
}

// Defaults fill in what an engine doesn't report.
type Defaults struct {
	ContextWindow int  // used when the engine reports none; 0 means 32768
	MaxTokens     int  // 0 means 8192
	Reasoning     bool // engines don't report it consistently
}

// Override changes the facts for one model, for example from ezbx.local.toml.
// Zero values leave a fact as it is.
type Override struct {
	Name          string
	ContextWindow int
	MaxTokens     int
	Reasoning     *bool
}

// Models turns what detection found into harness model facts, using the
// defaults for anything the engine didn't report, then the overrides.
func Models(found []detect.Model, d Defaults, overrides map[string]Override) []Model {
	if d.ContextWindow <= 0 {
		d.ContextWindow = 32768
	}
	if d.MaxTokens <= 0 {
		d.MaxTokens = 8192
	}
	out := make([]Model, 0, len(found))
	for _, f := range found {
		m := Model{ID: f.ID, Name: f.ID, ContextWindow: f.ContextWindow, MaxTokens: d.MaxTokens, Reasoning: d.Reasoning, Input: []string{"text"}}
		if m.ContextWindow <= 0 {
			m.ContextWindow = d.ContextWindow
		}
		if len(f.Input) > 0 {
			m.Input = append([]string(nil), f.Input...)
		}
		if o, ok := overrides[f.ID]; ok {
			if o.Name != "" {
				m.Name = o.Name
			}
			if o.ContextWindow > 0 {
				m.ContextWindow = o.ContextWindow
			}
			if o.MaxTokens > 0 {
				m.MaxTokens = o.MaxTokens
			}
			if o.Reasoning != nil {
				m.Reasoning = *o.Reasoning
			}
		}
		// A reply can't be longer than the whole context.
		if m.MaxTokens > m.ContextWindow {
			m.MaxTokens = m.ContextWindow
		}
		out = append(out, m)
	}
	return out
}

// Values is everything a config template can refer to.
type Values struct {
	GatewayBaseURL string // as seen from inside the box, e.g. http://gateway:8000/v1
	GatewayKey     string // a placeholder, never a real credential
	Models         []Model
	Default        string // id of the default model; empty means the first
}

// File is one rendered config file. Path is relative to the agent's home.
type File struct {
	Path string
	Mode uint32
	Data []byte
}

var placeholder = regexp.MustCompile(`\$\{([A-Za-z][A-Za-z0-9.]*)\}`)

// Render fills in every config file of a manifest. It refuses unknown
// placeholders and leaves nothing unfilled, so a broken template is an error
// rather than a broken config inside the box.
func Render(m Manifest, v Values) ([]File, error) {
	if len(v.Models) == 0 {
		return nil, fmt.Errorf("the engine reports no models, so there is nothing to configure %s with yet", m.Name)
	}
	def := v.Models[0].ID
	if v.Default != "" {
		found := false
		for _, mod := range v.Models {
			found = found || mod.ID == v.Default
		}
		if !found {
			return nil, fmt.Errorf("default model %q is not one the engine reports", v.Default)
		}
		def = v.Default
	}
	top := map[string]any{
		"gateway.baseUrl":   v.GatewayBaseURL,
		"gateway.url":       strings.TrimSuffix(strings.TrimSuffix(v.GatewayBaseURL, "/"), "/v1"),
		"gateway.key":       v.GatewayKey,
		"models.default.id": def,
	}
	r := &renderer{top: top, models: v.Models}

	var files []File
	for _, c := range m.Config {
		rel, err := homePath(c.Path)
		if err != nil {
			return nil, err
		}
		mode, err := parseMode(c.Mode)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(c.Content))
		dec.UseNumber()
		var tmpl any
		if err := dec.Decode(&tmpl); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Path, err)
		}
		out, err := r.value(tmpl, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Path, err)
		}
		data, err := encodeConfig(c.Format, out)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Path, err)
		}
		files = append(files, File{Path: rel, Mode: mode, Data: data})
	}
	return files, nil
}

// encodeConfig writes a rendered config value in the chosen format (JSON by
// default, or TOML or YAML).
func encodeConfig(format string, v any) ([]byte, error) {
	switch format {
	case "", "json":
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(data, '\n'), nil
	case "toml":
		m, ok := tomlReady(v).(map[string]any)
		if !ok {
			return nil, fmt.Errorf("a TOML config must be an object at the top")
		}
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(m); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "yaml":
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("a YAML config must be an object at the top")
		}
		var buf bytes.Buffer
		writeYAMLMap(&buf, m, 0)
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("unsupported config format %q", format)
	}
}

// tomlReady turns json.Number values into int64 or float64 so the TOML
// encoder writes them as numbers rather than text.
func tomlReady(v any) any {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = tomlReady(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = tomlReady(val)
		}
		return out
	default:
		return v
	}
}

var plainYAMLKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func yamlKey(k string) string {
	if plainYAMLKey.MatchString(k) {
		return k
	}
	return strconv.Quote(k)
}

func yamlScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(t)
	default:
		return fmt.Sprint(t)
	}
}

func writeYAMLMap(b *bytes.Buffer, m map[string]any, indent int) {
	if len(m) == 0 {
		b.WriteString("{}\n")
		return
	}
	pad := strings.Repeat(" ", indent)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		key := yamlKey(k)
		switch val := m[k].(type) {
		case map[string]any:
			if len(val) == 0 {
				fmt.Fprintf(b, "%s%s: {}\n", pad, key)
			} else {
				b.WriteString(pad + key + ":\n")
				writeYAMLMap(b, val, indent+2)
			}
		case []any:
			if len(val) == 0 {
				fmt.Fprintf(b, "%s%s: []\n", pad, key)
			} else {
				b.WriteString(pad + key + ":\n")
				writeYAMLList(b, val, indent+2)
			}
		default:
			fmt.Fprintf(b, "%s%s: %s\n", pad, key, yamlScalar(val))
		}
	}
}

func writeYAMLList(b *bytes.Buffer, list []any, indent int) {
	pad := strings.Repeat(" ", indent)
	for _, item := range list {
		switch val := item.(type) {
		case map[string]any:
			if len(val) == 0 {
				fmt.Fprintf(b, "%s- {}\n", pad)
				continue
			}
			var sub bytes.Buffer
			writeYAMLMap(&sub, val, indent+2)
			trimmed := strings.TrimPrefix(sub.String(), pad+"  ")
			fmt.Fprintf(b, "%s- %s", pad, trimmed)
		default:
			fmt.Fprintf(b, "%s- %s\n", pad, yamlScalar(val))
		}
	}
}

type renderer struct {
	top    map[string]any
	models []Model
}

// value renders one part of a template. item holds the current model's facts
// inside a $each block, and is nil outside one.
func (r *renderer) value(v any, item map[string]any) (any, error) {
	switch t := v.(type) {
	case string:
		return r.text(t, item)
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			got, err := r.value(e, item)
			if err != nil {
				return nil, err
			}
			out = append(out, got)
		}
		return out, nil
	case map[string]any:
		if _, isEach := t["$each"]; isEach {
			return r.each(t, item)
		}
		out := make(map[string]any, len(t))
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.HasPrefix(k, "$") {
				return nil, fmt.Errorf("unknown template key %q", k)
			}
			if strings.Contains(k, "${") {
				return nil, fmt.Errorf("placeholders are not allowed in key names (%q)", k)
			}
			got, err := r.value(t[k], item)
			if err != nil {
				return nil, err
			}
			out[k] = got
		}
		return out, nil
	default: // numbers, booleans, null
		return v, nil
	}
}

// each repeats its "as" block once per model. With "key", the result is an
// object keyed by that text (filled in per model) instead of a list.
func (r *renderer) each(t map[string]any, item map[string]any) (any, error) {
	if item != nil {
		return nil, fmt.Errorf("$each blocks can't be nested")
	}
	keyTmpl, keyed := t["key"]
	want := 2
	if keyed {
		want = 3
	}
	if len(t) != want || t["as"] == nil {
		return nil, fmt.Errorf(`a $each block needs exactly "$each" and "as", and optionally "key"`)
	}
	if t["$each"] != "models" {
		return nil, fmt.Errorf(`$each can only repeat over "models", not %v`, t["$each"])
	}
	keyText, ok := keyTmpl.(string)
	if keyed && (!ok || keyText == "") {
		return nil, fmt.Errorf(`the "key" of a $each block must be text, such as "${id}"`)
	}
	list := make([]any, 0, len(r.models))
	obj := make(map[string]any, len(r.models))
	for _, m := range r.models {
		input := make([]any, len(m.Input))
		for i, s := range m.Input {
			input[i] = s
		}
		facts := map[string]any{
			"id":            m.ID,
			"name":          m.Name,
			"contextWindow": m.ContextWindow,
			"maxTokens":     m.MaxTokens,
			"reasoning":     m.Reasoning,
			"input":         input,
		}
		got, err := r.value(t["as"], facts)
		if err != nil {
			return nil, err
		}
		if !keyed {
			list = append(list, got)
			continue
		}
		k, err := r.text(keyText, facts)
		if err != nil {
			return nil, err
		}
		ks, isText := k.(string)
		if !isText || ks == "" {
			return nil, fmt.Errorf(`the "key" of a $each block must give text, but %q gave %v for model %s`, keyText, k, m.ID)
		}
		if _, dup := obj[ks]; dup {
			return nil, fmt.Errorf(`the "key" %q gives %q for two models`, keyText, ks)
		}
		obj[ks] = got
	}
	if keyed {
		return obj, nil
	}
	return list, nil
}

// text fills placeholders in a string. A string that is exactly one
// placeholder takes that value's own type, so a number stays a number.
func (r *renderer) text(s string, item map[string]any) (any, error) {
	if m := placeholder.FindStringSubmatch(s); m != nil && m[0] == s {
		return r.lookup(m[1], item)
	}
	var err error
	out := placeholder.ReplaceAllStringFunc(s, func(ph string) string {
		if err != nil {
			return ""
		}
		val, e := r.lookup(placeholder.FindStringSubmatch(ph)[1], item)
		if e != nil {
			err = e
			return ""
		}
		switch x := val.(type) {
		case string:
			return x
		case int, bool, json.Number:
			return fmt.Sprint(x)
		}
		err = fmt.Errorf("%s is a list and can't be put inside a longer string", ph)
		return ""
	})
	if err != nil {
		return nil, err
	}
	if strings.Contains(out, "${") {
		return nil, fmt.Errorf("unfinished placeholder in %q", s)
	}
	return out, nil
}

func (r *renderer) lookup(name string, item map[string]any) (any, error) {
	if item != nil {
		if v, ok := item[name]; ok {
			return v, nil
		}
	}
	if v, ok := r.top[name]; ok {
		return v, nil
	}
	if item == nil {
		if _, isModelFact := map[string]bool{"id": true, "name": true, "contextWindow": true, "maxTokens": true, "reasoning": true, "input": true}[name]; isModelFact {
			return nil, fmt.Errorf("${%s} is a model fact and only works inside a $each block", name)
		}
	}
	return nil, fmt.Errorf("unknown placeholder ${%s}", name)
}
