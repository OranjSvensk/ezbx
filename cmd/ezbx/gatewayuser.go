package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/OranjSvensk/ezbx/internal/box"
)

// gatewayUserFor keeps Docker Desktop's image identity. With local native
// Linux Docker, the gateway needs the identity that owns its private host
// bind mounts.
func gatewayUserFor(ctx context.Context, docker box.Docker, goos string, uid, gid int) (string, error) {
	if goos != "linux" {
		return "", nil
	}
	out, err := docker(ctx, nil, "info", "--format", "{{.OperatingSystem}}|{{range .SecurityOptions}}{{println .}}{{end}}")
	if err != nil {
		return "", box.CodedError{Code: box.CodeDockerUnavailable, Err: fmt.Errorf("checking the Docker engine: %w", err)}
	}
	platform, options, ok := strings.Cut(strings.TrimSpace(string(out)), "|")
	if !ok || platform == "" {
		return "", fmt.Errorf("Docker did not report its platform and security options")
	}
	if strings.Contains(platform, "Docker Desktop") {
		return "", nil
	}
	if strings.Contains(options, "name=rootless") || strings.Contains(options, "name=userns") {
		return "", fmt.Errorf("native Linux needs local Docker Engine without rootless mode or user namespace remapping")
	}
	if uid <= 0 || gid < 0 {
		return "", fmt.Errorf("run ezbx as your regular non-root Linux account so the gateway can use its private files")
	}
	host := os.Getenv("DOCKER_HOST")
	if os.Getenv("DOCKER_CONTEXT") != "" || host == "" {
		out, err = docker(ctx, nil, "context", "inspect", "--format", `{{(index .Endpoints "docker").Host}}`)
		if err != nil {
			return "", fmt.Errorf("checking the Docker context: %w", err)
		}
		host = strings.TrimSpace(string(out))
	}
	if !strings.HasPrefix(host, "unix:///") {
		return "", fmt.Errorf("native Linux needs a local Docker context so containers can mount ezbx's files")
	}
	return fmt.Sprintf("%d:%d", uid, gid), nil
}

// checkGatewayAddress catches Desktop-only addresses before a native Linux
// start or engine change asks the server for its model list.
func checkGatewayAddress(user, raw string) error {
	if user == "" || raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if host == "localhost" || host == "host.docker.internal" || host == "gateway.docker.internal" ||
		(ip != nil && ip.IsLoopback()) {
		return fmt.Errorf("native Linux needs a model server address reachable from the Docker bridge. Change [engine].url to that address, or use airgap for a box without a model")
	}
	return nil
}
