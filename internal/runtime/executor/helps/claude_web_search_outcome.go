package helps

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	log "github.com/sirupsen/logrus"
)

// ClaudeWebSearchOutcomes records, for one upstream Claude response, what each
// server-side web search returned. It logs only counts, error codes and short
// id suffixes, never queries or result content, so an empty search seen by the
// client can be told apart from an empty or missing upstream result.
type ClaudeWebSearchOutcomes struct {
	order      []string
	outcome    map[string]string
	stopReason string
}

// NewClaudeWebSearchOutcomes returns an empty tracker.
func NewClaudeWebSearchOutcomes() *ClaudeWebSearchOutcomes {
	return &ClaudeWebSearchOutcomes{outcome: make(map[string]string)}
}

// Observe accepts a complete Messages response or SSE text (one or more lines).
func (o *ClaudeWebSearchOutcomes) Observe(data []byte) {
	if o == nil || len(data) == 0 {
		return
	}
	if content := ojson.GetBytes(data, "content"); content.IsArray() {
		content.ForEach(func(_, block ojson.Result) bool {
			o.observeBlock(block)
			return true
		})
		if stop := ojson.GetBytes(data, "stop_reason").String(); stop != "" {
			o.stopReason = stop
		}
		return
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		event := ojson.ParseBytes(bytes.TrimSpace(line[len("data:"):]))
		switch event.Get("type").String() {
		case "content_block_start":
			o.observeBlock(event.Get("content_block"))
		case "message_delta":
			if stop := event.Get("delta.stop_reason").String(); stop != "" {
				o.stopReason = stop
			}
		}
	}
}

func (o *ClaudeWebSearchOutcomes) observeBlock(block ojson.Result) {
	switch block.Get("type").String() {
	case "server_tool_use":
		if block.Get("name").String() != "web_search" {
			return
		}
		id := block.Get("id").String()
		if _, seen := o.outcome[id]; !seen {
			o.order = append(o.order, id)
			o.outcome[id] = "missing"
		}
	case "web_search_tool_result":
		id := block.Get("tool_use_id").String()
		if _, seen := o.outcome[id]; !seen {
			o.order = append(o.order, id)
		}
		o.outcome[id] = claudeWebSearchOutcome(block.Get("content"))
	}
}

func claudeWebSearchOutcome(content ojson.Result) string {
	switch {
	case !content.Exists() || content.Type == ojson.Null:
		return "empty"
	case content.IsObject():
		if content.Get("type").String() == "web_search_tool_result_error" {
			return "error:" + content.Get("error_code").String()
		}
		return "object"
	case content.IsArray():
		n := 0
		for _, entry := range content.Array() {
			if entry.Get("type").String() == "web_search_tool_result_error" {
				return "error:" + entry.Get("error_code").String()
			}
			n++
		}
		if n == 0 {
			return "empty"
		}
		return "results=" + strconv.Itoa(n)
	}
	return "unknown"
}

// Lines returns one "id-suffix=outcome" entry per search, in arrival order.
func (o *ClaudeWebSearchOutcomes) Lines() []string {
	if o == nil {
		return nil
	}
	out := make([]string, 0, len(o.order))
	for _, id := range o.order {
		suffix := id
		if len(suffix) > 8 {
			suffix = suffix[len(suffix)-8:]
		}
		out = append(out, suffix+"="+o.outcome[id])
	}
	return out
}

// Log writes one info line when the response contained at least one search.
// streamCompleted reports whether the upstream message reached message_stop.
func (o *ClaudeWebSearchOutcomes) Log(streamCompleted bool) {
	lines := o.Lines()
	if len(lines) == 0 {
		return
	}
	log.Infof("claude web search outcome | searches=%s stop_reason=%s completed=%t", strings.Join(lines, ","), o.stopReason, streamCompleted)
}
