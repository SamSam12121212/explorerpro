package threadevents

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	StreamName              = "THREAD_EVENTS"
	SubjectPrefix           = "thread.events."
	SubjectWildcard         = SubjectPrefix + ">"
	ConsumerName            = "thread-events-wsserver"
	EventTypeClientResponse = "client.response.create"
	EventTypeThreadSnapshot = "thread.snapshot"
	EventTypeThreadItem     = "thread.item.appended"
)

type EventEnvelope struct {
	ThreadID         int64           `json:"thread_id"`
	EventType        string          `json:"event_type"`
	SocketGeneration uint64          `json:"socket_generation"`
	Timestamp        string          `json:"ts"`
	Payload          json.RawMessage `json:"payload"`
}

func Subject(threadID int64) string {
	return SubjectPrefix + strconv.FormatInt(threadID, 10)
}

func MsgID(threadID int64, socketGeneration uint64, key string) string {
	return fmt.Sprintf("%d-%d-%s", threadID, socketGeneration, key)
}

func Encode(env EventEnvelope) ([]byte, error) {
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal thread event envelope: %w", err)
	}
	return data, nil
}

func Decode(data []byte) (EventEnvelope, error) {
	var env EventEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return EventEnvelope{}, fmt.Errorf("decode thread event envelope: %w", err)
	}
	if env.ThreadID <= 0 {
		return EventEnvelope{}, fmt.Errorf("thread event envelope missing thread_id")
	}
	if strings.TrimSpace(env.EventType) == "" {
		return EventEnvelope{}, fmt.Errorf("thread event envelope missing event_type")
	}
	return env, nil
}
