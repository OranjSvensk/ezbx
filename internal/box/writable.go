package box

import (
	"context"
	"fmt"
	"strings"
)

// writableCheck runs in the box as the agent. It prints "writable", or the
// workspace's owner and mode when the agent can't write there.
const writableCheck = `if [ -w /workspace ]; then echo writable; else stat -c '%u:%g %a' /workspace; fi`

// WorkspaceProblem asks the running box whether the agent can write to its
// workspace. It returns "" when it can, or when the box can't be asked (the
// doctor checks again), and otherwise says why not and how to fix it.
//
// This happens when Docker made the workspace folder itself: if the folder
// is missing when the box starts (deleted while the box existed, then a
// restart of Docker Desktop), Docker creates it owned by root.
func (r *Runner) WorkspaceProblem(ctx context.Context, p Plan) string {
	out, err := r.run(ctx, "exec", p.Names.Box, "sh", "-c", writableCheck)
	if err != nil {
		return ""
	}
	got := strings.TrimSpace(string(out))
	if got == "" || got == "writable" {
		return ""
	}
	if p.GatewayUser != "" {
		owner, mode, _ := strings.Cut(got, " ")
		return fmt.Sprintf("the agent (user %d) can't write to the workspace: it is owned by %s with mode %s. Choose a workspace writable by user %d or grant this user access to the workspace on the host", agentUID, owner, mode, agentUID)
	}
	return WorkspaceFix(p.Box, got)
}

// WorkspaceFix explains a workspace the agent can't write to. stat is the
// owner and mode, as "0:0 755".
func WorkspaceFix(box, stat string) string {
	owner, mode, _ := strings.Cut(stat, " ")
	flag := ""
	if box != "main" {
		flag = " --box " + box
	}
	return fmt.Sprintf("the agent (user %d) can't write to the workspace: /workspace is owned by %s with mode %s, probably because Docker created the folder itself. Fix it with: ezbx admin shell%s -- chown %d:%d /workspace",
		agentUID, owner, mode, flag, agentUID, agentUID)
}
