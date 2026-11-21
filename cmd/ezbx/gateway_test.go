package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminMustListenOnLoopback(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8099", "127.0.0.2:1", "[::1]:8099"} {
		if err := loopbackOnly(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:8099", ":8099", "[::]:8099", "localhost:8099", "172.18.0.1:8099", "gateway:8099", "127.0.0.1"} {
		if err := loopbackOnly(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestGatewayKeyFlags(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	os.WriteFile(bad, []byte("a\nb\n"), 0o600)
	good := filepath.Join(dir, "good")
	os.WriteFile(good, []byte("kv-docs-example-1\n"), 0o600)
	t.Setenv("EZBX_TEST_KEY", "kv-docs-example-1")
	t.Setenv("EZBX_TEST_BAD_KEY", "a\nb")
	for name, args := range map[string][]string{
		"both":        {"--key-env", "EZBX_TEST_KEY", "--key-file", good},
		"bad file":    {"--key-file", bad},
		"no file":     {"--key-file", filepath.Join(dir, "missing")},
		"empty env":   {"--key-env", "EZBX_TEST_UNSET_KEY"},
		"bad env key": {"--key-env", "EZBX_TEST_BAD_KEY"},
	} {
		var out, errs strings.Builder
		if code := runGateway(append(args, "--upstream", "http://127.0.0.1:18080"), &out, &errs); code != 2 {
			t.Errorf("%s: exit %d, want 2 (%s)", name, code, errs.String())
		}
		if strings.Contains(errs.String(), "kv-docs-example-1") {
			t.Errorf("%s: the key is in the error: %s", name, errs.String())
		}
	}
}
