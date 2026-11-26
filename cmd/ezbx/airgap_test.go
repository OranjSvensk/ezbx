package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/paths"
)

func TestAirgapStartupPlanSkipsTheEngine(t *testing.T) {
	var requests atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		io.WriteString(w, `{"data":[{"id":"fake-model"}]}`)
	}))
	defer engine.Close()
	root := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	spec := fmt.Sprintf("version = 1\n[box.vt]\nworkspace = '%s'\nharnesses = ['pi']\nmemory = '2g'\n", filepath.ToSlash(filepath.Join(root, "work")))
	server := "[engine]\napi = 'openai'\nurl = '" + engine.URL + "'\n"
	for _, extra := range []string{"", server, server + "key_env = 'EZBX_TEST_MISSING_KEY'\ndefault_model = 'missing'\n"} {
		cfg, err := config.Parse(spec + "network = 'airgap'\n" + extra)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := makePlan(t.Context(), cfg, "vt", dirs, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		flags := strings.Join(plan.GatewayArgs(), " ")
		if plan.Upstream != "" || plan.KeyEnv != "" || len(plan.HomeFiles) != 0 ||
			!strings.Contains(flags, "--network airgap") || strings.Contains(flags, "--upstream") {
			t.Fatalf("unexpected airgap plan: %+v", plan)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("airgap contacted the model server %d times", requests.Load())
	}
	for _, mode := range []string{"off", "allow", "on"} {
		cfg, err := config.Parse(spec + "network = '" + mode + "'\n")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := makePlan(t.Context(), cfg, "vt", dirs, io.Discard); err == nil {
			t.Errorf("%s startup accepted a missing engine", mode)
		}
		cfg, err = config.Parse(spec + "network = '" + mode + "'\n" + server)
		if err != nil {
			t.Fatal(err)
		}
		before := requests.Load()
		plan, err := makePlan(t.Context(), cfg, "vt", dirs, io.Discard)
		if err != nil || plan.Upstream == "" || len(plan.HomeFiles) == 0 || requests.Load() == before {
			t.Errorf("%s startup failed to configure the model route: %v", mode, err)
		}
	}
}
