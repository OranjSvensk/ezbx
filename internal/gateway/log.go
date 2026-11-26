package gateway

import (
	"encoding/json"
	"io"
	"net/netip"
	"sync"
)

// entry is one log line. It records what was asked and how it went, never
// the prompt, the reply or any key.
type entry struct {
	Time   string `json:"time"`
	ID     string `json:"id"`
	Route  string `json:"route"`
	Method string `json:"method"`
	Path   string `json:"path,omitempty"`   // model route
	Host   string `json:"host,omitempty"`   // proxy: host and port asked for
	Engine string `json:"engine,omitempty"` // model route
	Model  string `json:"model,omitempty"`
	Status int    `json:"status"`
	Ms     int64  `json:"ms"`
	TTFBMs int64  `json:"ttfb_ms,omitempty"`
	End    string `json:"end,omitempty"` // client_gone or engine_idle when a reply was cut short

	// Model route: the token counts the reply reported, when it did.
	TokensIn  int `json:"tokens_in,omitempty"`
	TokensOut int `json:"tokens_out,omitempty"`

	// Blocked means the network rules refused the request, and Reason says
	// which rule (on the model route, only network_airgap). A failed lookup or
	// connection through the proxy has a Reason too, but is not Blocked.
	Blocked bool   `json:"blocked,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Closed  string `json:"closed,omitempty"` // "network_tightened" when a mode change ended a tunnel or model request
	Doctor  bool   `json:"doctor,omitempty"` // proxy: a probe of ezbx doctor's, not the agent's
}

type logger struct {
	mu    sync.Mutex
	enc   *json.Encoder
	event func(string, any)
}

func newLogger(w io.Writer, event func(string, any)) *logger {
	if w == nil {
		return &logger{}
	}
	return &logger{enc: json.NewEncoder(w), event: event}
}

func (l *logger) write(e entry) {
	if l.enc == nil {
		return
	}
	l.mu.Lock()
	_ = l.enc.Encode(e)
	l.mu.Unlock()
	if l.event != nil {
		l.recordActivity(e)
	}
}

// Publish only the fields in the event contract. The diagnostic log can
// contain request-controlled paths and model IDs.
func (l *logger) recordActivity(e entry) {
	switch {
	case e.Route == "model":
		endpoint := e.Path
		switch endpoint {
		case "/v1/models", "/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens", "/v1/embeddings":
		default:
			endpoint = "other"
		}
		l.event("gateway.request", struct {
			RequestID string `json:"requestId"`
			Endpoint  string `json:"endpoint"`
			Status    int    `json:"status"`
			Duration  int64  `json:"durationMs"`
			TokensIn  int    `json:"tokensIn,omitempty"`
			TokensOut int    `json:"tokensOut,omitempty"`
			End       string `json:"end,omitempty"`
			Closed    string `json:"closed,omitempty"`
			Reason    string `json:"reason,omitempty"`
		}{
			RequestID: e.ID, Endpoint: endpoint, Status: e.Status, Duration: e.Ms,
			TokensIn: e.TokensIn, TokensOut: e.TokensOut, End: e.End, Closed: e.Closed, Reason: e.Reason,
		})
	case e.Route == "proxy" && e.Blocked:
		host, err := normalizeHost(hostOnly(e.Host))
		if err != nil {
			return
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			host = ip.WithZone("").String()
		}
		l.event("gateway.blocked_host", struct {
			RequestID string `json:"requestId"`
			Host      string `json:"host"`
			Status    int    `json:"status"`
			Reason    string `json:"reason"`
			Doctor    bool   `json:"doctor,omitempty"`
		}{RequestID: e.ID, Host: host, Status: e.Status, Reason: e.Reason, Doctor: e.Doctor})
	}
}
