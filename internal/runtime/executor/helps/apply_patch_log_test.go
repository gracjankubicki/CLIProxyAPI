package helps

import (
	"bytes"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// LogUpstreamToolInputRejected must emit one warn line with provider, model and
// phase, and must not contain patch text or upstream JSON.
func TestLogUpstreamToolInputRejected(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.WarnLevel)
	defer func() {
		log.SetOutput(prevOut)
		log.SetLevel(prevLevel)
	}()

	LogUpstreamToolInputRejected("openai-compatible-openrouter", "or-deepseek-v4.1-flash-1017", "stream-ended-early")

	line := buf.String()
	for _, want := range []string{
		"upstream tool arguments rejected",
		"provider=openai-compatible-openrouter",
		"model=or-deepseek-v4.1-flash-1017",
		"phase=stream-ended-early",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q missing %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("expected exactly one log line, got %q", line)
	}
}
