package logutil

import (
	"bytes"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func TestNewHandlerRendersCommandIDAndExplicitThreadID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewHandler(&buf, slog.LevelInfo))

	logger.Info("stream completed",
		"thread_id", "thread_d746fa6e55f798e8c133d14c",
		"cmd_id", int64(123456789),
		"final_status", "ready",
		"last_response_id", "resp_8db52bca1",
		"events_received", 17,
	)

	got := strings.TrimSpace(buf.String())
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} `).MatchString(got) {
		t.Fatalf("log line missing bare timestamp prefix: %q", got)
	}
	if strings.Contains(got, "msg=") {
		t.Fatalf("log line still contains msg key: %q", got)
	}
	if strings.Contains(got, "cmd_id=") {
		t.Fatalf("log line should rewrite cmd_id key: %q", got)
	}
	if !strings.Contains(got, `"stream completed" thread_id=thread_d746fa6e55f798e8c133d14c`) {
		t.Fatalf("log line missing explicit full thread id: %q", got)
	}
	if !strings.Contains(got, "command_id=123456789") {
		t.Fatalf("log line missing explicit full command id: %q", got)
	}
	if !strings.Contains(got, "last_response_id=resp_8db52bca1") {
		t.Fatalf("response id should remain full: %q", got)
	}
}
