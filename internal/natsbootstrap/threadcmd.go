package natsbootstrap

import (
	"fmt"
	"time"

	"explorer/internal/threadcmd"

	"github.com/nats-io/nats.go"
)

func EnsureThreadCommandStream(js nats.JetStreamContext) error {
	cfg := &nats.StreamConfig{
		Name:      threadcmd.StreamName,
		Subjects:  []string{"thread.dispatch.>", "thread.worker.*.cmd.>"},
		Storage:   nats.MemoryStorage,
		Retention: nats.LimitsPolicy,
		Discard:   nats.DiscardOld,
		MaxAge:    time.Hour,
		MaxMsgs:   -1,
		MaxBytes:  -1,
	}

	info, err := js.StreamInfo(threadcmd.StreamName)
	if err == nil && info != nil {
		// Storage type is immutable on an existing stream. If the stream was
		// previously created with FileStorage, delete it so we can recreate it
		// with MemoryStorage on the next AddStream call.
		if info.Config.Storage != cfg.Storage {
			if err := js.DeleteStream(threadcmd.StreamName); err != nil {
				return fmt.Errorf("delete %s stream to change storage type: %w", threadcmd.StreamName, err)
			}
		} else {
			if _, err := js.UpdateStream(cfg); err != nil {
				return fmt.Errorf("update %s stream: %w", threadcmd.StreamName, err)
			}
			return nil
		}
	}

	if _, err := js.AddStream(cfg); err != nil {
		return fmt.Errorf("add %s stream: %w", threadcmd.StreamName, err)
	}

	return nil
}
