package box

import (
	"context"
	"strings"
	"testing"
)

func TestInspect(t *testing.T) {
	answer := "true|healthy|ezbx-box:abc"
	ls := "ezbx-t-box|true\nother|\n"
	r := &Runner{Docker: func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		if args[1] == "ls" {
			return []byte(ls), nil
		}
		return []byte(answer + "\n"), nil
	}}
	c, err := r.Inspect(context.Background(), "ezbx-t-box")
	if err != nil || c != (Container{Exists: true, Running: true, Health: "healthy", Image: "ezbx-box:abc"}) {
		t.Errorf("running: %+v %v", c, err)
	}
	answer = "false||ezbx-box:abc"
	if c, _ = r.Inspect(context.Background(), "ezbx-t-box"); c.Running || c.Health != "" || !c.Exists {
		t.Errorf("stopped: %+v", c)
	}
	if c, err = r.Inspect(context.Background(), "ezbx-gone"); err != nil || c.Exists {
		t.Errorf("missing: %+v %v", c, err)
	}
	if _, err = r.Inspect(context.Background(), "other"); err == nil || !strings.Contains(err.Error(), "ezbx didn't create it") {
		t.Errorf("a container ezbx didn't make: %v", err)
	}
	answer = "true|healthy"
	if _, err = r.Inspect(context.Background(), "ezbx-t-box"); err == nil || !strings.Contains(err.Error(), "unexpected answer") {
		t.Errorf("a short answer: %v", err)
	}
}
