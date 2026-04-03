package natsbootstrap

import (
	"fmt"
	"time"

	"explorer/internal/agentcmd"

	"github.com/nats-io/nats.go"
)

func EnsureAgentCommandStream(js nats.JetStreamContext) error {
	cfg := &nats.StreamConfig{
		Name:      agentcmd.StreamName,
		Subjects:  []string{"agent.dispatch.>", "agent.worker.*.cmd.>"},
		Storage:   nats.FileStorage,
		Retention: nats.LimitsPolicy,
		Discard:   nats.DiscardOld,
		MaxAge:    7 * 24 * time.Hour,
		MaxMsgs:   -1,
		MaxBytes:  -1,
	}

	info, err := js.StreamInfo(agentcmd.StreamName)
	if err == nil && info != nil {
		if _, err := js.UpdateStream(cfg); err != nil {
			return fmt.Errorf("update %s stream: %w", agentcmd.StreamName, err)
		}
		return nil
	}

	if _, err := js.AddStream(cfg); err != nil {
		return fmt.Errorf("add %s stream: %w", agentcmd.StreamName, err)
	}

	return nil
}
