package box

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// answering wraps the fake so the workspace check gets the given answer.
func answering(f *fakeDocker, reply string, err error) func(context.Context, []byte, ...string) ([]byte, error) {
	return func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "exec" {
			f.calls = append(f.calls, args)
			return []byte(reply), err
		}
		return f.run(ctx, stdin, args...)
	}
}

func TestUpWarnsWhenTheWorkspaceIsNotWritable(t *testing.T) {
	f := newFake()
	p := testPlan(t)
	var out strings.Builder
	r := runner(f)
	r.Out = &out
	r.Docker = answering(f, "0:0 755\n", nil)
	if err := r.Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !f.did("exec", p.Names.Box, "sh", "-c") {
		t.Error("the box wasn't asked")
	}
	for _, want := range []string{"warning:", "owned by 0:0 with mode 755", "ezbx admin shell --box " + p.Box + " -- chown 1000:1000 /workspace"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestUpIsQuietWhenTheWorkspaceIsWritable(t *testing.T) {
	for name, a := range map[string]struct {
		reply string
		err   error
	}{"writable": {"writable\n", nil}, "can't ask": {"", errors.New("boom")}, "no answer": {"", nil}} {
		f := newFake()
		var out strings.Builder
		r := runner(f)
		r.Out = &out
		r.Docker = answering(f, a.reply, a.err)
		if err := r.Up(context.Background(), testPlan(t)); err != nil {
			t.Fatal(name, err)
		}
		if strings.Contains(out.String(), "warning") {
			t.Errorf("%s: %s", name, out.String())
		}
	}
}

func TestNativeWorkspaceGuidancePreservesOwnership(t *testing.T) {
	f := newFake()
	p := testPlan(t)
	p.GatewayUser = "1001:1001"
	r := runner(f)
	r.Docker = answering(f, "1001:1001 700\n", nil)
	got := r.WorkspaceProblem(t.Context(), p)
	if !strings.Contains(got, "owned by 1001:1001 with mode 700") ||
		!strings.Contains(got, "writable by user 1000") || strings.Contains(got, "chown") {
		t.Fatal(got)
	}
}

func TestWorkspaceFix(t *testing.T) {
	if got := WorkspaceFix("main", "0:0 755"); !strings.Contains(got, "ezbx admin shell -- chown 1000:1000 /workspace") || strings.Contains(got, "--box") {
		t.Error(got)
	}
	if got := WorkspaceFix("work", "0:0 755"); !strings.Contains(got, "ezbx admin shell --box work -- chown") {
		t.Error(got)
	}
}
