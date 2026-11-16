package executor

import (
	"net/http"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestResponseFormatOrSourceUsesExplicitResponseFormat(t *testing.T) {
	opts := Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatClaude,
	}

	if got := ResponseFormatOrSource(opts); got != sdktranslator.FormatClaude {
		t.Fatalf("ResponseFormatOrSource() = %q, want %q", got, sdktranslator.FormatClaude)
	}
}

func TestResponseFormatOrSourceFallsBackToSourceFormat(t *testing.T) {
	opts := Options{SourceFormat: sdktranslator.FormatGemini}

	if got := ResponseFormatOrSource(opts); got != sdktranslator.FormatGemini {
		t.Fatalf("ResponseFormatOrSource() = %q, want %q", got, sdktranslator.FormatGemini)
	}
}

func TestRequestTerminatedErrorUsesControlledPluginMessage(t *testing.T) {
	err := &RequestTerminatedError{
		Header: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:   []byte("Claude thinking replay is unavailable for this Responses turn"),
	}
	if got := err.Error(); got != "Claude thinking replay is unavailable for this Responses turn" {
		t.Fatalf("Error() = %q, want controlled plugin message", got)
	}
}

func TestRequestTerminatedErrorUsesJSONPluginMessage(t *testing.T) {
	err := &RequestTerminatedError{Body: []byte(`{"error":{"message":"replay unavailable"}}`)}
	if got := err.Error(); got != "replay unavailable" {
		t.Fatalf("Error() = %q, want controlled plugin message", got)
	}
}

func TestRequestTerminatedErrorFallsBackForUnstructuredBody(t *testing.T) {
	err := &RequestTerminatedError{Body: []byte("not-json")}
	if got := err.Error(); got != "request terminated by plugin" {
		t.Fatalf("Error() = %q, want generic fallback", got)
	}
}

func TestRequestTerminatedErrorRejectsMultilinePlainText(t *testing.T) {
	err := &RequestTerminatedError{
		Header: http.Header{"Content-Type": []string{"text/plain"}},
		Body:   []byte("safe first line\nuntrusted second line"),
	}
	if got := err.Error(); got != "request terminated by plugin" {
		t.Fatalf("Error() = %q, want generic fallback", got)
	}
}
