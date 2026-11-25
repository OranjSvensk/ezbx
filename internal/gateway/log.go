package gateway

import (
	"encoding/json"
	"io"
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
	mu  sync.Mutex
	enc *json.Encoder
}

func newLogger(w io.Writer) *logger {
	if w == nil {
		return &logger{}
	}
	return &logger{enc: json.NewEncoder(w)}
}

func (l *logger) write(e entry) {
	if l.enc == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(e)
}
