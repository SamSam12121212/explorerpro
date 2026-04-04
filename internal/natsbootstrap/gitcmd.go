package natsbootstrap

import (
	"fmt"
	"time"

	"explorer/internal/gitcmd"

	"github.com/nats-io/nats.go"
)

func EnsureGitCommandStream(js nats.JetStreamContext) error {
	cfg := &nats.StreamConfig{
		Name:      gitcmd.StreamName,
		Subjects:  []string{"git.>"},
		Storage:   nats.FileStorage,
		Retention: nats.LimitsPolicy,
		Discard:   nats.DiscardOld,
		MaxAge:    7 * 24 * time.Hour,
		MaxMsgs:   -1,
		MaxBytes:  -1,
	}

	info, err := js.StreamInfo(gitcmd.StreamName)
	if err == nil && info != nil {
		if _, err := js.UpdateStream(cfg); err != nil {
			return fmt.Errorf("update %s stream: %w", gitcmd.StreamName, err)
		}
		return nil
	}

	if _, err := js.AddStream(cfg); err != nil {
		return fmt.Errorf("add %s stream: %w", gitcmd.StreamName, err)
	}

	return nil
}
