package helps

import (
	"reflect"
	"strings"
	"testing"
)

func TestClaudeWebSearchOutcomesStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_AAAA11111111","name":"web_search","input":{}}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_AAAA11111111","content":[{"type":"web_search_result","url":"https://a","encrypted_content":"x"},{"type":"web_search_result","url":"https://b","encrypted_content":"y"}]}}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"server_tool_use","id":"srvtoolu_BBBB22222222","name":"web_search","input":{}}}`,
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_BBBB22222222","content":{"type":"web_search_tool_result_error","error_code":"too_many_requests"}}}`,
		`data: {"type":"content_block_start","index":4,"content_block":{"type":"server_tool_use","id":"srvtoolu_CCCC33333333","name":"web_search","input":{}}}`,
		`data: {"type":"content_block_start","index":5,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_CCCC33333333","content":[]}}`,
		`data: {"type":"content_block_start","index":6,"content_block":{"type":"server_tool_use","id":"srvtoolu_DDDD44444444","name":"web_search","input":{}}}`,
		`data: {"type":"content_block_start","index":7,"content_block":{"type":"server_tool_use","id":"srvtoolu_EEEE55555555","name":"web_fetch","input":{}}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"}}`,
	}, "\n")
	o := NewClaudeWebSearchOutcomes()
	// Feed line by line, as the stream executor does.
	for _, line := range strings.Split(sse, "\n") {
		o.Observe([]byte(line))
	}
	want := []string{"11111111=results=2", "22222222=error:too_many_requests", "33333333=empty", "44444444=missing"}
	if got := o.Lines(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	if o.stopReason != "pause_turn" {
		t.Fatalf("stop_reason = %q", o.stopReason)
	}
}

func TestClaudeWebSearchOutcomesNonStream(t *testing.T) {
	body := []byte(`{"stop_reason":"end_turn","content":[{"type":"server_tool_use","id":"srvtoolu_AAAA11111111","name":"web_search"},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_AAAA11111111","content":[{"type":"web_search_result","url":"https://a"}]},{"type":"text","text":"ok"}]}`)
	o := NewClaudeWebSearchOutcomes()
	o.Observe(body)
	if got, want := o.Lines(), []string{"11111111=results=1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	if o.stopReason != "end_turn" {
		t.Fatalf("stop_reason = %q", o.stopReason)
	}
}

func TestClaudeWebSearchOutcomesNoSearchNoLines(t *testing.T) {
	o := NewClaudeWebSearchOutcomes()
	o.Observe([]byte(`{"content":[{"type":"text","text":"hi"}]}`))
	if got := o.Lines(); len(got) != 0 {
		t.Fatalf("lines = %v, want none", got)
	}
	var nilTracker *ClaudeWebSearchOutcomes
	nilTracker.Observe([]byte(`data: {}`))
	nilTracker.Log(true)
}
