package natsbootstrap

import (
	"fmt"
	"time"

	"explorer/internal/ocrcmd"

	"github.com/nats-io/nats.go"
)

func EnsureDocOCRStream(js nats.JetStreamContext) error {
	cfg := &nats.StreamConfig{
		Name: ocrcmd.StreamName,
		Subjects: []string{
			ocrcmd.SplitDoneSubject,
			ocrcmd.OCRDoneSubject,
			ocrcmd.ImageOCRRequestedSubject,
			ocrcmd.ImageOCRDoneSubject,
		},
		Storage:   nats.FileStorage,
		Retention: nats.LimitsPolicy,
		Discard:   nats.DiscardOld,
		MaxAge:    7 * 24 * time.Hour,
		MaxMsgs:   -1,
		MaxBytes:  -1,
	}

	info, err := js.StreamInfo(ocrcmd.StreamName)
	if err == nil && info != nil {
		if _, err := js.UpdateStream(cfg); err != nil {
			return fmt.Errorf("update %s stream: %w", ocrcmd.StreamName, err)
		}
		return nil
	}

	if _, err := js.AddStream(cfg); err != nil {
		return fmt.Errorf("add %s stream: %w", ocrcmd.StreamName, err)
	}

	return nil
}
