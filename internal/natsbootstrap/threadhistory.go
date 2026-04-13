package natsbootstrap

import (
	"fmt"

	"explorer/internal/threadhistory"

	"github.com/nats-io/nats.go"
)

func EnsureThreadHistoryStream(js nats.JetStreamContext) error {
	cfg := &nats.StreamConfig{
		Name:        threadhistory.StreamName,
		Subjects:    []string{threadhistory.SubjectPrefix + ">"},
		Storage:     nats.FileStorage,
		Retention:   nats.LimitsPolicy,
		MaxMsgs:     -1,
		MaxBytes:    -1,
		AllowDirect: true,
	}

	info, err := js.StreamInfo(threadhistory.StreamName)
	if err == nil && info != nil {
		if _, err := js.UpdateStream(cfg); err != nil {
			return fmt.Errorf("update %s stream: %w", threadhistory.StreamName, err)
		}
		return nil
	}

	if _, err := js.AddStream(cfg); err != nil {
		return fmt.Errorf("add %s stream: %w", threadhistory.StreamName, err)
	}

	return nil
}
