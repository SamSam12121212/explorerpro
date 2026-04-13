package natsbootstrap

import (
	"fmt"
	"time"

	"explorer/internal/threadevents"

	"github.com/nats-io/nats.go"
)

func EnsureThreadEventsStream(js nats.JetStreamContext) error {
	cfg := &nats.StreamConfig{
		Name:         threadevents.StreamName,
		Subjects:     []string{threadevents.SubjectWildcard},
		Storage:      nats.MemoryStorage,
		Retention:    nats.WorkQueuePolicy,
		MaxConsumers: 1,
		MaxAge:       5 * time.Minute,
		MaxMsgs:      -1,
		MaxBytes:     -1,
	}

	info, err := js.StreamInfo(threadevents.StreamName)
	if err == nil && info != nil {
		if _, err := js.UpdateStream(cfg); err != nil {
			return fmt.Errorf("update %s stream: %w", threadevents.StreamName, err)
		}
		return nil
	}

	if _, err := js.AddStream(cfg); err != nil {
		return fmt.Errorf("add %s stream: %w", threadevents.StreamName, err)
	}

	return nil
}
