package gateway

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestGatewayActivityFields(t *testing.T) {
	var records []string
	record := func(kind string, data any) {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, kind+" "+string(raw))
	}
	logger := newLogger(io.Discard, record)
	logger.write(entry{ID: "r1", Route: "model", Path: "/v1/hidden-path", Model: "hidden-model", Status: 429, TokensIn: 2})
	logger.write(entry{ID: "r2", Route: "proxy", Host: "[fe80::1%hidden-zone]:443", Status: 403, Blocked: true, Reason: reasonLocalAddress})
	logger.write(entry{ID: "r3", Route: "proxy", Host: "example.com", Status: 502, Reason: reasonDNS})
	newLogger(nil, record).write(entry{ID: "r4", Route: "model", Status: 200})
	want := strings.Join([]string{
		`gateway.request {"requestId":"r1","endpoint":"other","status":429,"durationMs":0,"tokensIn":2}`,
		`gateway.blocked_host {"requestId":"r2","host":"fe80::1","status":403,"reason":"local_address"}`,
	}, "\n")
	if got := strings.Join(records, "\n"); got != want {
		t.Fatalf("events:\n%s\nwant:\n%s", got, want)
	}
}
