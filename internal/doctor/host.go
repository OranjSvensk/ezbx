package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/internal/detect"
	"github.com/OranjSvensk/ezbx/internal/images"
)

// Docker runs one docker command and returns what it printed. It is a field
// so tests can replace it.
type Docker func(ctx context.Context, args ...string) ([]byte, error)

// HostConfig holds what the host checks need.
type HostConfig struct {
	Box        string // box name; containers are ezbx-<box>-gateway and so on
	Docker     Docker
	SkipEngine bool                                                    // the box is configured to start in airgap
	LinuxHost  bool                                                    // give host-side permissions guidance on Linux
	EngineURL  string                                                  // configured address, if any
	Detect     func(ctx context.Context, extra []string) detect.Result // finds engines on this computer, also checking the extra addresses
	LocalAddrs func() []string                                         // this computer's own non-loopback addresses
	// Images are the images the box needs now, by kind ("gateway", "box"),
	// worked out from the files carried in ezbx and the box's ezbx.toml.
	// Nil skips the image checks, with ImagesNote (or a default) as the reason.
	Images     map[string]Image
	ImagesNote string
	// ReadOnly are the read-only folders ezbx up recorded for this box;
	// ReadOnlyNote says why they couldn't be read.
	ReadOnly     []ReadOnlyFolder
	ReadOnlyNote string
}

// Image is one image the box should run: its name and the hash of its inputs.
type Image struct{ Tag, Inputs string }

// Docker resource names for a box.
func names(box string) (inside, outside, gateway, boxName string) {
	p := "ezbx-" + box + "-"
	return p + "inside", p + "outside", p + "gateway", p + "box"
}

// The option that leaves an internal network's bridge without an address.
// Without it, the box can reach services in Docker Desktop's VM.
const isolatedOption = "com.docker.network.bridge.gateway_mode_ipv4"

type networkInfo struct {
	Name       string            `json:"Name"`
	Internal   bool              `json:"Internal"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Options    map[string]string `json:"Options"`
	Labels     map[string]string `json:"Labels"`
	IPAM       struct {
		Config []struct {
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

type containerInfo struct {
	Name  string `json:"Name"`
	Image string `json:"Image"` // the ID of the image it was started from
	State struct {
		Running bool `json:"Running"`
		Health  *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		User   string            `json:"User"`
		Cmd    []string          `json:"Cmd"`
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
		Privileged     bool     `json:"Privileged"`
		CapDrop        []string `json:"CapDrop"`
		CapAdd         []string `json:"CapAdd"`
		SecurityOpt    []string `json:"SecurityOpt"`
		PortBindings   map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
	} `json:"HostConfig"`
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			Aliases   []string `json:"Aliases"`
			DNSNames  []string `json:"DNSNames"`
			IPAddress string   `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (cfg HostConfig) inspect(ctx context.Context, kind, name string, v any) error {
	out, err := cfg.Docker(ctx, kind, "inspect", name)
	if err != nil {
		return err
	}
	var list []json.RawMessage
	if err := json.Unmarshal(out, &list); err != nil || len(list) != 1 {
		return fmt.Errorf("unexpected answer from docker %s inspect", kind)
	}
	return json.Unmarshal(list[0], v)
}

// Run does every check: on the host, then from inside the box.
func Run(ctx context.Context, cfg HostConfig) Report {
	var r Report
	insideNet, outsideNet, gwName, boxName := names(cfg.Box)

	// Docker itself.
	const gd = "Docker"
	out, err := cfg.Docker(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		r.add(gd, "Docker engine answers", Fail, shortErr(err), "Start Docker and check your active context, then run ezbx doctor again.")
		return r
	}
	version := strings.TrimSpace(string(out))
	major, _ := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	if major >= 28 {
		r.add(gd, "Docker engine version", Pass, version, "")
	} else {
		r.add(gd, "Docker engine version", Fail, version, "ezbx needs Docker Engine 28 or later for the isolated network option. Update Docker.")
	}

	var gw, box containerInfo
	gwErr := cfg.inspect(ctx, "container", gwName, &gw)
	boxErr := cfg.inspect(ctx, "container", boxName, &box)

	// The engine on this computer.
	const ge = "Engine"
	var engines []detect.Engine
	var refused []detect.Refusal // addresses that asked for a key
	if cfg.SkipEngine {
		r.add(ge, "an engine is running", Skip, "the box is configured for airgap", "")
	} else if cfg.Detect != nil {
		// Also look where the gateway was started to forward, so an engine on
		// an unusual port is found too.
		var extra []string
		if gwErr == nil {
			if a := hostSide(cmdFlags(gw.Config.Cmd)["--upstream"]); a != "" {
				extra = append(extra, a)
			}
		}
		res := cfg.Detect(ctx, extra)
		engines, refused = res.Engines, res.Refused
		if len(engines) == 0 && len(refused) > 0 {
			// The doctor sends no key, so it can't list this engine's
			// models. The gateway has the key: its "engine ready" check
			// below asks the engine through it.
			var asks []string
			for _, f := range refused {
				asks = append(asks, f.URL)
			}
			r.add(ge, "an engine is running", Pass, "an engine at "+strings.Join(asks, ", ")+" asks for a key (the gateway's engine ready check uses it)", "")
		} else if len(engines) == 0 {
			detail := "no model server address is configured"
			fix := "Set [engine].url in ezbx.toml, or choose network = \"airgap\" for a box that needs no model."
			if cfg.EngineURL != "" {
				detail = "no compatible model server answered at " + cfg.EngineURL
				fix = "Check [engine].url and [engine].api in ezbx.toml, and confirm the server is running at that address."
				if cfg.LinuxHost {
					fix += " On native Linux, use an address reachable from Docker's bridge."
				}
			}
			r.add(ge, "an engine is running", Warn, detail, fix)
		} else {
			var found []string
			for _, e := range engines {
				found = append(found, fmt.Sprintf("%s at %s (%d model(s))", e.Name, e.URL, len(e.Models)))
			}
			r.add(ge, "an engine is running", Pass, strings.Join(found, "; "), "")
		}
	}

	// Networks.
	start := "Start with ezbx up --box " + cfg.Box + "."
	if gwErr != nil {
		checkLocalImage(ctx, &r, cfg, "gateway")
	}
	if boxErr != nil {
		checkLocalImage(ctx, &r, cfg, "box")
	}
	if gwErr == nil || boxErr == nil {
		start = "Run ezbx down --box " + cfg.Box + ", then ezbx up --box " + cfg.Box + " to recreate the box."
	}

	const gn = "Networks"
	var inside, outside networkInfo
	insideOK := cfg.inspect(ctx, "network", insideNet, &inside) == nil
	if !insideOK {
		r.add(gn, insideNet, Fail, "missing", start)
	} else {
		var problems []string
		if !inside.Internal {
			problems = append(problems, "not internal, so it has a route out")
		}
		if inside.Options[isolatedOption] != "isolated" {
			problems = append(problems, "its bridge has an address, so the box can reach services on Docker's own machine")
		}
		if inside.EnableIPv6 || inside.Options["com.docker.network.enable_ipv6"] == "true" {
			problems = append(problems, "IPv6 is on")
		}
		if len(problems) == 0 {
			r.add(gn, insideNet, Pass, "internal, isolated bridge, IPv6 off", "")
		} else {
			r.add(gn, insideNet, Fail, strings.Join(problems, "; "),
				"Recreate it: docker network create --internal -o "+isolatedOption+"=isolated -o com.docker.network.enable_ipv6=false --label ezbx.managed=true "+insideNet)
		}
	}
	if err := cfg.inspect(ctx, "network", outsideNet, &outside); err != nil {
		r.add(gn, outsideNet, Fail, "missing", start)
	} else if outside.Internal {
		r.add(gn, outsideNet, Fail, "internal, so the gateway can't reach the engine or the internet", "Recreate it without --internal.")
	} else {
		r.add(gn, outsideNet, Pass, "", "")
	}

	// The gateway container.
	const gg = "Gateway"
	if gwErr != nil {
		r.add(gg, gwName, Fail, "missing", start)
		r.add("From inside the box", "inside checks", Skip, "the gateway is missing", "")
		return r
	}
	args := cmdFlags(gw.Config.Cmd)
	switch {
	case !gw.State.Running:
		r.add(gg, "running", Fail, "stopped", start)
	case gw.State.Health == nil:
		r.add(gg, "running", Warn, "running, with no healthcheck", "Run ezbx build, then ezbx down and ezbx up: the gateway image has a healthcheck built in.")
	case gw.State.Health.Status != "healthy":
		r.add(gg, "running", Warn, "health is "+gw.State.Health.Status, "Wait a few seconds; if it stays unhealthy, see docker logs "+gwName+".")
	default:
		r.add(gg, "running", Pass, "healthy", "")
	}
	r.add(gg, "locked down", lockStatus(gw, true), lockDetail(gw, true), "Recreate it with ezbx down and ezbx up.")
	if st, detail := keyStatus(gw, args); st != "" {
		r.add(gg, "engine key", st, detail, "Run ezbx down, then ezbx up: this version gives the gateway its key in a read-only file, which docker inspect doesn't show.")
	}
	st, detail, fix := imageStatus(ctx, cfg, "gateway", gw)
	r.add(gg, "image is current", st, detail, fix)

	nets := sortedKeys(gw.NetworkSettings.Networks)
	if len(nets) == 2 && nets[0] == insideNet && nets[1] == outsideNet {
		in := gw.NetworkSettings.Networks[insideNet]
		if contains(in.Aliases, "gateway") || contains(in.DNSNames, "gateway") {
			r.add(gg, "networks", Pass, "inside (as \"gateway\") and outside", "")
		} else {
			r.add(gg, "networks", Fail, "not reachable as \"gateway\" on the inside network", "docker network disconnect "+insideNet+" "+gwName+", then docker network connect --alias gateway "+insideNet+" "+gwName+".")
		}
	} else {
		r.add(gg, "networks", Fail, "attached to: "+joinOr(nets, "none"), "The gateway must be on exactly "+insideNet+" and "+outsideNet+".")
	}

	_, adminPort, _ := net.SplitHostPort(args["--admin"])
	var exposed []string
	for port, binds := range gw.HostConfig.PortBindings {
		for _, b := range binds {
			ip := net.ParseIP(b.HostIP)
			if adminPort != "" && strings.HasPrefix(port, adminPort+"/") {
				exposed = append(exposed, port+" (the admin API, which is never published)")
			} else if ip == nil || !ip.IsLoopback() {
				where := b.HostIP
				if ip == nil || ip.IsUnspecified() {
					where = "every address, so other computers can reach it"
				}
				exposed = append(exposed, port+" on "+where)
			}
		}
	}
	if len(exposed) == 0 {
		r.add(gg, "published ports", Pass, "none beyond this computer's loopback", "")
	} else {
		sort.Strings(exposed)
		r.add(gg, "published ports", Fail, strings.Join(exposed, "; "), "Publish only as 127.0.0.1:PORT:PORT, and never the admin port.")
	}

	// Through the admin API, the same way the CLI controls the gateway.
	status := struct {
		Engine   string `json:"engine"`
		Upstream string `json:"upstream"`
		Network  *struct {
			Mode string `json:"mode"`
		} `json:"network"`
	}{}
	mode := ""
	if !gw.State.Running {
		r.add(gg, "admin API", Skip, "the gateway is stopped", "")
	} else if adminPort == "" {
		r.add(gg, "admin API", Warn, "not running", "Start the gateway with --admin 127.0.0.1:8099 and a token file.")
	} else if out, err := cfg.Docker(ctx, "exec", gwName, "/ezbx", "gateway", "admin", "--addr", args["--admin"], "status"); err != nil || json.Unmarshal(out, &status) != nil {
		r.add(gg, "admin API", Fail, "no answer through docker exec", "See docker logs "+gwName+". The token file must be mounted at /run/ezbx/admin-token.")
	} else {
		if status.Network != nil {
			mode = status.Network.Mode
		}
		r.add(gg, "admin API", Pass, fmt.Sprintf("engine %s, network %s", status.Engine, joinOr([]string{mode}, "(no proxy)")), "")

		if cfg.SkipEngine {
			r.add(gg, "engine ready", Skip, "the box is configured for airgap", "")
		} else {
			ready := struct {
				Ready  bool   `json:"ready"`
				Reason string `json:"reason"`
			}{}
			out, _ := cfg.Docker(ctx, "exec", gwName, "/ezbx", "gateway", "admin", "--addr", args["--admin"], "ready")
			if json.Unmarshal(out, &ready) == nil && ready.Ready {
				r.add(gg, "engine ready", Pass, "", "")
			} else {
				detail := ready.Reason
				if detail == "" {
					detail = "no readiness response"
				}
				fix := "Check [engine].url and the server from the host. If it requires a key, verify [engine].key_env names a variable set in the terminal that starts ezbx."
				if cfg.LinuxHost {
					fix += " The server must also listen on an address reachable from Docker's bridge."
				}
				r.add(gg, "engine ready", Fail, detail, fix)
			}
			s, detail, fix := upstreamMatches(status.Upstream, engines, refused)
			r.add(gg, "forwards to the running engine", s, detail, fix)
		}
	}

	// The box container, if there is one yet.
	const gb = "Box"
	if boxErr != nil {
		r.add(gb, boxName, Fail, "missing", start)
	} else {
		r.add(gb, "locked down", lockStatus(box, false), lockDetail(box, false), "Recreate it with --cap-drop ALL --security-opt no-new-privileges and a non-root user.")
		st, detail, fix := imageStatus(ctx, cfg, "box", box)
		r.add(gb, "image is current", st, detail, fix)
		bnets := sortedKeys(box.NetworkSettings.Networks)
		if len(bnets) == 1 && bnets[0] == insideNet {
			r.add(gb, "networks", Pass, "only the inside network", "")
		} else {
			r.add(gb, "networks", Fail, "attached to: "+joinOr(bnets, "none"), "The box must be attached to "+insideNet+" and nothing else.")
		}
		var sock []string
		for _, m := range box.Mounts {
			if strings.Contains(m.Source, "docker.sock") || strings.Contains(m.Destination, "docker.sock") {
				sock = append(sock, m.Destination)
			}
		}
		if len(sock) == 0 {
			r.add(gb, "no Docker socket", Pass, "", "")
		} else {
			r.add(gb, "no Docker socket", Fail, "mounted at "+strings.Join(sock, ", "), "Never mount the Docker socket into the box: it gives full control of this computer.")
		}
		if box.State.Running {
			r.add(gb, boxName, Pass, "running", "")
			st, detail, fix := writableStatus(ctx, cfg, boxName)
			r.add(gb, "agent can write the workspace", st, detail, fix)
		} else {
			r.add(gb, boxName, Fail, "stopped", start)
		}
		readOnlyChecks(ctx, &r, cfg, boxName, box)
	}

	// From inside, seeing what the box sees.
	if !insideOK {
		r.add("From inside the box", "inside checks", Skip, "the inside network is missing", "")
		return r
	}
	if !gw.State.Running || boxErr != nil || !box.State.Running {
		r.add("From inside the box", "inside checks", Skip, "the gateway and box must both be running", "")
		return r
	}
	ic := InsideConfig{Gateway: "gateway:" + portOf(args["--listen"], "8000"), Mode: mode, Engine: status.Engine, SkipModel: cfg.SkipEngine && mode != "airgap"}
	ic.OpenPorts = []int{atoi(portOf(args["--listen"], "8000"))}
	if p := args["--proxy"]; p != "" {
		ic.Proxy = "gateway:" + portOf(p, "3128")
		ic.OpenPorts = append(ic.OpenPorts, atoi(portOf(p, "3128")))
	}
	upPort := "80"
	if u, err := url.Parse(status.Upstream); err == nil && u.Port() != "" {
		upPort = u.Port()
	}
	// Addresses the box must not reach: the gateway's own outside address,
	// the outside network's bridge, Docker Desktop's host address and this
	// computer's own addresses.
	if o, ok := gw.NetworkSettings.Networks[outsideNet]; ok && o.IPAddress != "" {
		ic.Deny = append(ic.Deny, net.JoinHostPort(o.IPAddress, strconv.Itoa(ic.OpenPorts[0])))
		ic.Local = append(ic.Local, net.JoinHostPort(o.IPAddress, "443"))
	}
	for _, c := range outside.IPAM.Config {
		if c.Gateway != "" {
			ic.Deny = append(ic.Deny, net.JoinHostPort(c.Gateway, "80"))
		}
	}
	for _, c := range inside.IPAM.Config {
		if c.Gateway != "" {
			ic.Bridges = append(ic.Bridges, c.Gateway)
		}
	}
	ic.Deny = append(ic.Deny, net.JoinHostPort("192.168.65.254", upPort))
	ic.Local = append(ic.Local, "host.docker.internal:443", "localhost:443", "169.254.169.254:80", net.JoinHostPort("192.168.65.254", "443"))
	if adminPort != "" {
		ic.Local = append(ic.Local, net.JoinHostPort("127.0.0.1", adminPort))
	}
	if cfg.LocalAddrs != nil {
		for _, a := range cfg.LocalAddrs() {
			ic.Deny = append(ic.Deny, net.JoinHostPort(a, "445"))
			ic.Local = append(ic.Local, net.JoinHostPort(a, "443"))
		}
	}
	ic.Timeout = time.Second
	spec, _ := json.Marshal(ic)
	out, err = cfg.Docker(ctx, "run", "--rm", "--pull=never", "--label", "ezbx.managed=true", "--network", insideNet,
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532",
		"--entrypoint", "/ezbx", gw.Config.Image, "doctor", "inside", "--config", string(spec))
	var inner Report
	if jerr := json.Unmarshal(out, &inner); jerr != nil || len(inner.Checks) == 0 {
		detail := "the check container gave no report"
		if err != nil {
			detail = shortErr(err)
		}
		r.add("From inside the box", "inside checks", Fail, detail, "Run ezbx build, then ezbx down and ezbx up, so the gateway has the current ezbx.")
		return r
	}
	r.Checks = append(r.Checks, inner.Checks...)
	return r
}

// keyStatus checks how the gateway gets the engine's key. A key in its
// environment shows in plain text in docker inspect, so that is a warning;
// a key file passes. With neither (the engine needs no key) there is no
// check to show, and Status is "".
func keyStatus(gw containerInfo, args map[string]string) (Status, string) {
	var inEnv []string
	if name := args["--key-env"]; name != "" {
		inEnv = append(inEnv, name)
	}
	for _, kv := range gw.Config.Env {
		name, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(name)
		if !contains(inEnv, name) && (strings.Contains(up, "KEY") || strings.Contains(up, "TOKEN") || strings.Contains(up, "SECRET") || strings.Contains(up, "PASSWORD")) {
			inEnv = append(inEnv, name)
		}
	}
	switch {
	case len(inEnv) > 0:
		sort.Strings(inEnv)
		return Warn, "in the environment (" + strings.Join(inEnv, ", ") + "), so docker inspect shows it in plain text"
	case args["--key-file"] != "":
		return Pass, "from a read-only file, not the environment"
	}
	return "", ""
}

// lockStatus checks a container's privileges. The gateway also needs a
// read-only root filesystem.
func lockStatus(c containerInfo, readOnly bool) Status {
	if len(lockProblems(c, readOnly)) == 0 {
		return Pass
	}
	return Fail
}

func lockDetail(c containerInfo, readOnly bool) string {
	if bad := lockProblems(c, readOnly); len(bad) > 0 {
		return strings.Join(bad, "; ")
	}
	if readOnly {
		return "no capabilities, no-new-privileges, not root, read-only"
	}
	return "no capabilities, no-new-privileges, not root"
}

func lockProblems(c containerInfo, readOnly bool) []string {
	var bad []string
	if c.HostConfig.Privileged {
		bad = append(bad, "privileged")
	}
	if !containsFold(c.HostConfig.CapDrop, "ALL") {
		bad = append(bad, "capabilities not dropped")
	}
	if len(c.HostConfig.CapAdd) > 0 {
		bad = append(bad, "capabilities added: "+strings.Join(c.HostConfig.CapAdd, ","))
	}
	nnp := false
	for _, o := range c.HostConfig.SecurityOpt {
		nnp = nnp || o == "no-new-privileges" || o == "no-new-privileges:true" || o == "no-new-privileges=true"
	}
	if !nnp {
		bad = append(bad, "no-new-privileges is off")
	}
	if u := strings.SplitN(c.Config.User, ":", 2)[0]; u == "" || u == "0" || u == "root" {
		bad = append(bad, "runs as root")
	}
	if readOnly && !c.HostConfig.ReadonlyRootfs {
		bad = append(bad, "root filesystem is writable")
	}
	return bad
}

// upstreamMatches checks that the gateway forwards to an engine that is
// running, by port, since the gateway reaches it through the host alias.
func upstreamMatches(upstream string, engines []detect.Engine, refused []detect.Refusal) (Status, string, string) {
	u, err := url.Parse(upstream)
	if err != nil || u.Port() == "" {
		return Warn, "can't tell from " + upstream, ""
	}
	// An address that asked for a key is an engine the doctor can't list,
	// not a missing one.
	for _, f := range refused {
		engines = append(engines, detect.Engine{Name: "an engine that asks for a key", URL: f.URL})
	}
	for _, e := range engines {
		if eu, err := url.Parse(e.URL); err == nil && eu.Port() == u.Port() {
			return Pass, e.Name + " on port " + u.Port(), ""
		}
	}
	if len(engines) == 0 {
		return Skip, "no engine found to compare with", ""
	}
	return Warn, "the gateway forwards to port " + u.Port() + ", but no engine was found there",
		"Point the gateway at the running engine: docker exec ezbx-<box>-gateway /ezbx gateway admin engine http://host.docker.internal:PORT ID"
}

// hostSide turns the gateway's upstream into the same engine's address as
// seen from this computer: Docker Desktop's host alias is this computer's
// loopback. Other addresses are kept. It returns "" if there is nothing to
// check.
func hostSide(upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if strings.EqualFold(u.Hostname(), "host.docker.internal") {
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
		}
		u.Host = net.JoinHostPort("127.0.0.1", port)
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return u.String()
}

// cmdFlags reads --name value pairs from a container's command.
func cmdFlags(cmd []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(cmd); i++ {
		if !strings.HasPrefix(cmd[i], "--") {
			continue
		}
		if k, v, ok := strings.Cut(cmd[i], "="); ok {
			out[k] = v
		} else if i+1 < len(cmd) && !strings.HasPrefix(cmd[i+1], "--") {
			out[cmd[i]] = cmd[i+1]
			i++
		} else {
			out[cmd[i]] = "true"
		}
	}
	return out
}

func portOf(addr, def string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		return p
	}
	return def
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// writableScript runs in the box as the agent: it prints "writable", or the
// workspace's owner and mode.
const writableScript = `if [ -w /workspace ]; then echo writable; else stat -c '%u:%g %a' /workspace; fi`

// writableStatus asks the running box, as the agent, whether it can write
// to /workspace. When Docker creates a missing workspace folder itself (for
// example after it was deleted while the box existed), it is owned by root.
func writableStatus(ctx context.Context, cfg HostConfig, boxName string) (Status, string, string) {
	out, err := cfg.Docker(ctx, "exec", boxName, "sh", "-c", writableScript)
	got := strings.TrimSpace(string(out))
	switch {
	case err != nil:
		return Warn, "couldn't ask the box: " + shortErr(err), ""
	case got == "writable":
		return Pass, "", ""
	}
	flag := ""
	if cfg.Box != "main" {
		flag = " --box " + cfg.Box
	}
	owner, mode, _ := strings.Cut(got, " ")
	detail := "/workspace is owned by " + owner + " with mode " + mode + ", so the agent (user 1000) can't change anything"
	if cfg.LinuxHost {
		return Fail, detail, "Choose a workspace writable by user 1000 or grant this user access to the workspace on the host."
	}
	if owner == "0:0" {
		return Fail, detail, "If Docker created this folder, fix it with: ezbx admin shell" + flag + " -- chown 1000:1000 /workspace"
	}
	return Fail, detail, "Check write access and Docker Desktop file sharing for the host folder set by workspace in ezbx.toml."
}

// checkLocalImage also works before the first start, when there is no
// container to compare with the image required by ezbx.toml.
func checkLocalImage(ctx context.Context, r *Report, cfg HostConfig, kind string) {
	want, ok := cfg.Images[kind]
	if !ok {
		return
	}
	name := kind + " image is available"
	fix := "Run ezbx build --box " + cfg.Box + " to build the pinned image. The first build may need internet access."
	out, err := cfg.Docker(ctx, "image", "inspect", "--format", `{{index .Config.Labels "ezbx.inputs"}}`, want.Tag)
	switch {
	case err != nil:
		r.add("Images", name, Fail, want.Tag+" could not be inspected: "+shortErr(err), fix)
	case strings.TrimSpace(string(out)) != want.Inputs:
		r.add("Images", name, Fail, want.Tag+" does not match the current build inputs", fix)
	default:
		r.add("Images", name, Pass, want.Tag, "")
	}
}

// imageStatus checks that a container runs the image the box needs now: the
// one named after the current inputs, built by ezbx build from them, and the
// same build that carries that name today (ezbx reset --rebuild makes a new
// one under the same name). An old image still works, so this only warns.
func imageStatus(ctx context.Context, cfg HostConfig, kind string, c containerInfo) (Status, string, string) {
	want, ok := cfg.Images[kind]
	if !ok {
		note := cfg.ImagesNote
		if note == "" {
			note = "no ezbx.toml for this box to compare with"
		}
		return Skip, note, ""
	}
	flag := " --box " + cfg.Box
	fix := "Run ezbx build" + flag + ", then ezbx down" + flag + " and ezbx up" + flag + ". The workspace, the agent's home and snapshots are kept."
	if c.Config.Image != want.Tag {
		return Warn, fmt.Sprintf("runs %s, but the current files and ezbx.toml make %s", joinOr([]string{c.Config.Image}, "an unnamed image"), want.Tag), fix
	}
	if got := c.Config.Labels[images.InputsLabel]; got != want.Inputs {
		return Warn, fmt.Sprintf("%s wasn't built by ezbx build from the current files", want.Tag), fix
	}
	out, err := cfg.Docker(ctx, "image", "inspect", "--format", "{{.Id}}", want.Tag)
	if err != nil {
		return Warn, want.Tag + " is no longer on this computer", fix
	}
	if id := strings.TrimSpace(string(out)); id != c.Image {
		return Warn, want.Tag + " was built again after this container started", fix
	}
	return Pass, want.Tag, ""
}
