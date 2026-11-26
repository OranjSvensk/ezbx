package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGatewayUserForNativeLinux(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	for _, tc := range []struct {
		name, goos, info, host string
		uid                    int
		want, problem          string
	}{
		{name: "windows", goos: "windows"},
		{name: "desktop", goos: "linux", info: "Docker Desktop|name=seccomp", uid: 1001},
		{name: "native", goos: "linux", info: "Ubuntu|name=seccomp", host: "unix:///var/run/docker.sock", uid: 1001, want: "1001:1001"},
		{name: "root", goos: "linux", info: "Ubuntu|name=seccomp", problem: "non-root"},
		{name: "rootless", goos: "linux", info: "Ubuntu|name=rootless", uid: 1001, problem: "rootless"},
		{name: "remapped", goos: "linux", info: "Ubuntu|name=userns", uid: 1001, problem: "user namespace remapping"},
		{name: "remote", goos: "linux", info: "Ubuntu|name=seccomp", host: "tcp://192.0.2.1:2376", uid: 1001, problem: "local Docker context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docker := func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
				if tc.goos != "linux" {
					t.Fatal("Windows startup queried Docker for its gateway identity")
				}
				switch args[0] {
				case "info":
					return []byte(tc.info), nil
				case "context":
					return []byte(tc.host), nil
				}
				return nil, errors.New("unexpected Docker command")
			}
			user, err := gatewayUserFor(t.Context(), docker, tc.goos, tc.uid, tc.uid)
			if tc.problem != "" {
				if err == nil || !strings.Contains(err.Error(), tc.problem) {
					t.Fatalf("got %q, %v, want %q", user, err, tc.problem)
				}
			} else if err != nil || user != tc.want {
				t.Fatalf("got %q, %v, want %q", user, err, tc.want)
			}
		})
	}
}

func TestNativeGatewayAddress(t *testing.T) {
	for _, address := range []string{"http://localhost:18080", "http://127.0.0.2:18080", "http://[::1]:18080",
		"http://HOST.DOCKER.INTERNAL:18080", "http://gateway.docker.internal:18080"} {
		if err := checkGatewayAddress("1001:1001", address); err == nil || !strings.Contains(err.Error(), "Docker bridge") {
			t.Errorf("native Linux accepted %s: %v", address, err)
		}
		if err := checkGatewayAddress("", address); err != nil {
			t.Errorf("Docker Desktop refused %s: %v", address, err)
		}
	}
	for _, address := range []string{"", "http://192.0.2.10:18080", "https://models.example.com"} {
		if err := checkGatewayAddress("1001:1001", address); err != nil {
			t.Errorf("native Linux refused %q: %v", address, err)
		}
	}
}
