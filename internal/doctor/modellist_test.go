package doctor

import (
	"net/http"
	"testing"
)

func TestJudgeModelList(t *testing.T) {
	list := []byte(`{"object":"list","data":[{"id":"big-model"},{"id":"small"}]}`)
	airgap := []byte(`{"error":{"code":"network_airgap","type":"network_airgap","message":"m"}}`)
	cases := []struct {
		name   string
		mode   string
		status int
		engine string
		body   []byte
		want   Status
		model  string
	}{
		{"a list", "off", http.StatusOK, "openai", list, Pass, "big-model"},
		{"no models", "off", http.StatusOK, "openai", []byte(`{"data":[]}`), Warn, ""},
		{"refused", "on", http.StatusBadGateway, "", []byte(`{}`), Fail, ""},
		{"another engine", "allow", http.StatusOK, "anthropic", list, Fail, ""},
		// Outside airgap, the airgap refusal is a failure.
		{"airgap refusal in off", "off", http.StatusForbidden, "", airgap, Fail, ""},
		// In airgap, only the gateway's own refusal passes.
		{"airgap", "airgap", http.StatusForbidden, "", airgap, Pass, ""},
		{"airgap but a list", "airgap", http.StatusOK, "openai", list, Fail, ""},
		{"airgap, wrong status", "airgap", http.StatusOK, "", airgap, Fail, ""},
		{"airgap, other 403", "airgap", http.StatusForbidden, "", []byte(`{"error":{"code":"forbidden"}}`), Fail, ""},
		{"airgap, from the engine", "airgap", http.StatusForbidden, "openai", airgap, Fail, ""},
	}
	for _, tc := range cases {
		c, model := judgeModelList(InsideConfig{Mode: tc.mode, Engine: "openai"}, tc.status, tc.engine, tc.body)
		if c.Status != tc.want || model != tc.model {
			t.Errorf("%s: %s %q (%s), want %s %q", tc.name, c.Status, model, c.Detail, tc.want, tc.model)
		}
		if (tc.mode == "airgap") != (c.Name == "airgap closes the model route") {
			t.Errorf("%s: check named %q", tc.name, c.Name)
		}
	}
}
