package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Token counts for the log. The gateway reads them from what it sends the
// box: a JSON body or server-sent events, in OpenAI's or Anthropic's shape,
// from the engine or from the gateway's own translation. It never logs the
// content, only the two numbers.

const (
	maxUsageLine = 1 << 20 // a longer event line is skipped, not buffered
	maxUsageBody = 4 << 20 // a larger JSON body isn't read for its usage
)

// usageCounts is a "usage" object in any of the shapes the APIs use.
type usageCounts struct {
	PromptTokens     int `json:"prompt_tokens"`     // OpenAI chat
	CompletionTokens int `json:"completion_tokens"` // OpenAI chat
	InputTokens      int `json:"input_tokens"`      // Anthropic, OpenAI Responses
	OutputTokens     int `json:"output_tokens"`     // Anthropic, OpenAI Responses
}

func (u *usageCounts) in() int  { return max(u.PromptTokens, u.InputTokens) }
func (u *usageCounts) out() int { return max(u.CompletionTokens, u.OutputTokens) }

// readUsage finds the usage in one JSON object: at the top (chat, Messages,
// Responses), under "message" (Anthropic's message_start) or under
// "response" (Responses' response.completed). Zero means not reported.
func readUsage(raw []byte) (in, out int) {
	var v struct {
		Usage   *usageCounts `json:"usage"`
		Message *struct {
			Usage *usageCounts `json:"usage"`
		} `json:"message"`
		Response *struct {
			Usage *usageCounts `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return 0, 0
	}
	for _, u := range []*usageCounts{v.Usage, msgUsage(v.Message), respUsage(v.Response)} {
		if u != nil {
			in, out = max(in, u.in()), max(out, u.out())
		}
	}
	return in, out
}

func msgUsage(m *struct {
	Usage *usageCounts `json:"usage"`
}) *usageCounts {
	if m == nil {
		return nil
	}
	return m.Usage
}

func respUsage(r *struct {
	Usage *usageCounts `json:"usage"`
}) *usageCounts {
	if r == nil {
		return nil
	}
	return r.Usage
}

// noteUsage keeps the latest counts a reply reported. A stream may report
// them more than once (Anthropic sends the input in message_start and the
// output at the end), so a zero never replaces a number.
func (info *reqInfo) noteUsage(in, out int) {
	if in > 0 {
		info.tokensIn = in
	}
	if out > 0 {
		info.tokensOut = out
	}
}

// usageSniffer watches the bytes of one reply for its usage.
type usageSniffer struct {
	decided, sse bool
	line         []byte
	skipLine     bool
	body         []byte
	tooBig       bool
}

func (s *usageSniffer) feed(info *reqInfo, contentType string, p []byte) {
	if !s.decided {
		s.decided = true
		s.sse = strings.HasPrefix(strings.ToLower(contentType), "text/event-stream")
	}
	if !s.sse {
		if !s.tooBig && len(s.body)+len(p) <= maxUsageBody {
			s.body = append(s.body, p...)
		} else {
			s.tooBig, s.body = true, nil
		}
		return
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if !s.skipLine {
			if len(s.line)+len(part) > maxUsageLine {
				s.skipLine, s.line = true, s.line[:0]
			} else {
				s.line = append(s.line, part...)
			}
		}
		if i < 0 {
			return
		}
		s.sseLine(info, s.line) // empty when the line was too long
		s.line, s.skipLine = s.line[:0], false
		p = p[i+1:]
	}
}

func (s *usageSniffer) sseLine(info *reqInfo, line []byte) {
	line = bytes.TrimSpace(line)
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok || !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	info.noteUsage(readUsage(bytes.TrimSpace(data)))
}

// finish reads a whole JSON body once the reply is done.
func (s *usageSniffer) finish(info *reqInfo) {
	if s.sse {
		if !s.skipLine && len(s.line) > 0 {
			s.sseLine(info, s.line)
		}
		return
	}
	if !s.tooBig && len(s.body) > 0 {
		info.noteUsage(readUsage(s.body))
	}
	s.body = nil
}
