package logutil

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
)

const logTimestampFormat = "2006-01-02 15:04:05"

// NewHandler returns a text-based slog handler that renders a bare timestamp
// prefix, omits level noise, renders msg/cmd_id without keys, and keeps
// thread ids explicit for troubleshooting.
func NewHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return slog.NewTextHandler(&bareIDLogWriter{dst: w}, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})
}

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.String(slog.TimeKey, a.Value.Time().UTC().Format(logTimestampFormat))
	}

	if a.Key == slog.LevelKey {
		return slog.Attr{}
	}

	return a
}

type bareIDLogWriter struct {
	dst io.Writer

	mu      sync.Mutex
	pending []byte
}

func (w *bareIDLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending = append(w.pending, p...)
	for {
		idx := bytes.IndexByte(w.pending, '\n')
		if idx == -1 {
			return len(p), nil
		}

		line := rewriteBareIDTokens(string(w.pending[:idx]))
		if _, err := io.WriteString(w.dst, line+"\n"); err != nil {
			return 0, err
		}

		w.pending = w.pending[idx+1:]
	}
}

func rewriteBareIDTokens(line string) string {
	line = stripLeadingQuotedTime(line)
	line = strings.ReplaceAll(line, " time=", " ")
	line = strings.ReplaceAll(line, " msg=", " ")
	line = strings.ReplaceAll(line, " cmd_id=", " ")
	line = strings.TrimPrefix(line, "time=")
	line = strings.TrimPrefix(line, "msg=")
	return strings.TrimPrefix(line, "cmd_id=")
}

func stripLeadingQuotedTime(line string) string {
	if !strings.HasPrefix(line, `time="`) {
		return line
	}

	rest := strings.TrimPrefix(line, `time="`)
	closingIdx := strings.IndexByte(rest, '"')
	if closingIdx == -1 {
		return line
	}

	return rest[:closingIdx] + rest[closingIdx+1:]
}
