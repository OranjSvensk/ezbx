package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OranjSvensk/ezbx/internal/box"
)

// fakeDocker answers the docker commands ezbx status runs.
type fakeDocker struct {
	down       bool              // the engine doesn't answer
	containers map[string]string // name -> "running|health|image"
	mode       string            // the gateway's network mode
	adminFails bool
}

func (d *fakeDocker) docker(_ context.Context, _ []byte, args ...string) ([]byte, error) {
	switch {
	case args[0] == "version":
		if d.down {
			return nil, errors.New("Cannot connect to the Docker daemon")
		}
		return []byte("29.1.0"), nil
	case args[0] == "container" && args[1] == "ls":
		var b strings.Builder
		for n := range d.containers {
			fmt.Fprintf(&b, "%s|true\n", n)
		}
		return []byte(b.String()), nil
	case args[0] == "container" && args[1] == "inspect":
		name := args[len(args)-1]
		if strings.Contains(args[3], "Health") {
			return []byte(d.containers[name]), nil
		}
		return []byte(strings.SplitN(d.containers[name], "|", 2)[0]), nil
	case args[0] == "exec":
		if d.adminFails {
			return nil, errors.New("connection refused")
		}
		out, _ := json.Marshal(map[string]any{"engine": "openai", "network": map[string]any{"mode": d.mode, "allow": []string{"github.com"}}})
		return out, nil
	}
	return nil, fmt.Errorf("unexpected docker %v", args)
}

func TestStatusOf(t *testing.T) {
	cfg := cfgWith(t, `["pi"]`)
	want, _ := expectedImages(target{name: "t", cfg: cfg})
	img := want["box"].Tag
	gw, bx := box.NamesFor("t").Gateway, box.NamesFor("t").Box
	cases := []struct {
		name    string
		tg      target
		d       fakeDocker
		state   string
		mode    string
		healthy bool
		current string // "", "yes" or "no"
		problem string
	}{
		{"not in the config, nothing made", target{name: "other", cfg: cfg}, fakeDocker{}, "missing", "", false, "", ""},
		{"in the config, nothing made", target{name: "t", cfg: cfg}, fakeDocker{}, "stopped", "off", false, "", ""},
		{"running", target{name: "t", cfg: cfg}, fakeDocker{mode: "on", containers: map[string]string{gw: "true|healthy|ezbx-gateway:x", bx: "true||" + img}}, "running", "on", true, "yes", ""},
		{"an old image", target{name: "t", cfg: cfg}, fakeDocker{mode: "off", containers: map[string]string{gw: "true|healthy|ezbx-gateway:x", bx: "true||ezbx-box:000000000000"}}, "running", "off", true, "no", ""},
		{"gateway starting", target{name: "t", cfg: cfg}, fakeDocker{mode: "off", containers: map[string]string{gw: "true|starting|g", bx: "true||" + img}}, "unhealthy", "off", false, "yes", "healthcheck says starting"},
		{"gateway stopped", target{name: "t", cfg: cfg}, fakeDocker{containers: map[string]string{gw: "false|unhealthy|g", bx: "true||" + img}}, "unhealthy", "off", false, "yes", "the gateway isn't running"},
		{"box stopped", target{name: "t", cfg: cfg}, fakeDocker{mode: "airgap", containers: map[string]string{gw: "true|healthy|g", bx: "false||" + img}}, "unhealthy", "airgap", true, "yes", "the box container isn't running"},
		{"gateway stopped, health left over", target{name: "t", cfg: cfg}, fakeDocker{containers: map[string]string{gw: "false|healthy|g", bx: "true||" + img}}, "unhealthy", "off", false, "yes", "the gateway isn't running"},
		{"gateway gone", target{name: "t", cfg: cfg}, fakeDocker{containers: map[string]string{bx: "true||" + img}}, "unhealthy", "off", false, "yes", "the gateway isn't running"},
		{"box gone", target{name: "t", cfg: cfg}, fakeDocker{mode: "off", containers: map[string]string{gw: "true|healthy|g"}}, "unhealthy", "off", true, "", "the box container isn't running"},
		{"admin API silent", target{name: "t", cfg: cfg}, fakeDocker{adminFails: true, containers: map[string]string{gw: "true|healthy|g", bx: "true||" + img}}, "unhealthy", "off", true, "yes", "admin API didn't answer"},
		{"left over, config gone", target{name: "other", cfg: cfg}, fakeDocker{mode: "on", containers: map[string]string{box.NamesFor("other").Gateway: "true|healthy|g", box.NamesFor("other").Box: "true||b"}}, "running", "on", true, "", ""},
	}
	for _, tc := range cases {
		s, err := statusOf(context.Background(), &box.Runner{Docker: tc.d.docker}, tc.tg)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		current := ""
		if s.ImageCurrent != nil {
			current = map[bool]string{true: "yes", false: "no"}[*s.ImageCurrent]
		}
		if s.State != tc.state || s.NetworkMode != tc.mode || s.GatewayHealthy != tc.healthy || current != tc.current ||
			!strings.Contains(strings.Join(s.Problems, "; "), tc.problem) || (tc.problem == "") != (len(s.Problems) == 0) {
			t.Errorf("%s: %+v (current %q)", tc.name, s, current)
		}
	}
	// The running gateway's allowlist wins over ezbx.toml's.
	d := fakeDocker{mode: "allow", containers: map[string]string{gw: "true|healthy|g", bx: "true||" + img}}
	if s, _ := statusOf(context.Background(), &box.Runner{Docker: d.docker}, target{name: "t", cfg: cfg}); strings.Join(s.Allow, ",") != "github.com" || s.ConfigMode != "off" {
		t.Errorf("allow: %+v", s)
	}
	// Docker not answering is its own error code.
	d = fakeDocker{down: true}
	_, err := statusOf(context.Background(), &box.Runner{Docker: d.docker}, target{name: "t", cfg: cfg})
	if codeOf(err) != box.CodeDockerUnavailable {
		t.Errorf("docker down: %q %v", codeOf(err), err)
	}
}

func TestWriteResult(t *testing.T) {
	var out bytes.Buffer
	if code := writeResult(&out, "agent-ada", map[string]int{"n": 1}, nil); code != 0 || strings.TrimSpace(out.String()) != `{"ok":true,"box":"agent-ada","contractVersion":1,"data":{"n":1}}` {
		t.Errorf("ok: %d %s", code, out.String())
	}
	cases := []struct {
		err  error
		code string
		exit int
	}{
		{usageError("bad flag"), "invalid_args", 2},
		{box.ErrNotUp{Box: "b", What: "x"}, "box_not_running", 1},
		{errNoBox("b", []string{"main"}), "box_not_found", 1},
		{fmt.Errorf("waiting: %w", context.DeadlineExceeded), "timeout", 1},
		{box.CodedError{Code: box.CodeImageMismatch, Err: errors.New("old image")}, "image_mismatch", 1},
		{fmt.Errorf("wrapped: %w", box.CodedError{Code: box.CodeAlreadyRunning, Err: errors.New("up")}), "box_already_running", 1},
		{errors.New("something else"), "internal_error", 1},
	}
	for _, tc := range cases {
		out.Reset()
		exit := writeResult(&out, "b", "ignored", tc.err)
		var env struct {
			OK              bool            `json:"ok"`
			Box             string          `json:"box"`
			ContractVersion int             `json:"contractVersion"`
			Data            json.RawMessage `json:"data"`
			Error           struct{ Code, Message string }
		}
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("%v: %s", err, out.String())
		}
		if exit != tc.exit || env.OK || env.Box != "b" || env.ContractVersion != 1 || env.Data != nil || env.Error.Code != tc.code || env.Error.Message != tc.err.Error() {
			t.Errorf("%v: exit %d, %s", tc.err, exit, out.String())
		}
		if strings.Count(out.String(), "\n") != 1 {
			t.Errorf("not one line: %q", out.String())
		}
	}
}

func TestVersionJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var v struct {
		OK              bool     `json:"ok"`
		Version         string   `json:"version"`
		ContractVersion int      `json:"contractVersion"`
		Features        []string `json:"features"`
		Platform        string   `json:"platform"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !v.OK || v.ContractVersion != 1 || v.Version == "" || strings.HasPrefix(v.Version, "v") || !strings.Contains(v.Platform, "/") ||
		strings.Join(v.Features, ",") != "events-jsonl,gateway-events-jsonl,lifecycle-v1,net-four-modes,readonly-masks,status-v1" {
		t.Errorf("%+v", v)
	}
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		out.Reset()
		if code := run(args, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "ezbx ") || strings.Contains(out.String(), "{") {
			t.Errorf("%v: %d %q", args, code, out.String())
		}
	}
	if code := run([]string{"version", "extra"}, &out, &errOut); code != 2 {
		t.Errorf("extra word: exit %d", code)
	}
}

// A usage mistake with --json is still one JSON object on standard output.
func TestStatusUsageJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"status", "--json", "extra"}, &out, &errOut); code != 2 || !strings.Contains(out.String(), `"code":"invalid_args"`) {
		t.Errorf("%d %s / %s", code, out.String(), errOut.String())
	}
}
