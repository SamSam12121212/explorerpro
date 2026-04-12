package logutil

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
)

const (
	threadPrefix      = "thread_"
	cmdPrefix         = "cmd_"
	visibleSuffixSize = 10
)

// NewHandler returns a text-based slog handler that omits timestamps
// (Docker/container runtimes add their own), renders msg/thread_id/cmd_id
// without keys, and shortens long IDs for log readability.
func NewHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return slog.NewTextHandler(&bareIDLogWriter{dst: w}, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})
}

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey || a.Key == slog.LevelKey {
		return slog.Attr{}
	}

	if a.Key == "thread_id" {
		a.Value = slog.StringValue(ShortThreadID(a.Value.String()))
	}

	if a.Key == "cmd_id" {
		a.Value = shortenPrefixedIDFromStart(a.Value, cmdPrefix, visibleSuffixSize)
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
	line = strings.ReplaceAll(line, " msg=", " ")
	line = strings.ReplaceAll(line, " thread_id=", " ")
	line = strings.ReplaceAll(line, " cmd_id=", " ")
	line = strings.TrimPrefix(line, "msg=")
	line = strings.TrimPrefix(line, "thread_id=")
	return strings.TrimPrefix(line, "cmd_id=")
}

func shortenPrefixedIDFromStart(v slog.Value, prefix string, visibleChars int) slog.Value {
	s := v.String()
	if !strings.HasPrefix(s, prefix) {
		return v
	}

	suffix := strings.TrimPrefix(s, prefix)
	if len(suffix) <= visibleChars {
		return v
	}

	return slog.StringValue(prefix + suffix[:visibleChars])
}

// ShortThreadID returns thread_<first 10 chars of the suffix> for log output.
func ShortThreadID(id string) string {
	if !strings.HasPrefix(id, threadPrefix) {
		return id
	}

	suffix := strings.TrimPrefix(id, threadPrefix)
	if len(suffix) <= visibleSuffixSize {
		return id
	}

	return threadPrefix + suffix[:visibleSuffixSize]
}
