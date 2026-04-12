package logutil

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestNewHandlerRendersBareShortIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewHandler(&buf, slog.LevelInfo))

	logger.Info("stream completed",
		"thread_id", "thread_d746fa6e55f798e8c133d14c",
		"cmd_id", "cmd_123456789ab",
		"final_status", "ready",
		"last_response_id", "resp_8db52bca1",
		"events_received", 17,
	)

	got := strings.TrimSpace(buf.String())
	if strings.Contains(got, "msg=") {
		t.Fatalf("log line still contains msg key: %q", got)
	}
	if strings.Contains(got, "thread_id=") {
		t.Fatalf("log line still contains thread_id key: %q", got)
	}
	if strings.Contains(got, "cmd_id=") {
		t.Fatalf("log line still contains cmd_id key: %q", got)
	}
	if !strings.Contains(got, `"stream completed" thread_d746fa6e55`) {
		t.Fatalf("log line missing bare message or shortened thread id: %q", got)
	}
	if !strings.Contains(got, " cmd_123456789a ") {
		t.Fatalf("log line missing bare shortened cmd id: %q", got)
	}
	if !strings.Contains(got, "last_response_id=resp_8db52bca1") {
		t.Fatalf("response id should remain full: %q", got)
	}
}
