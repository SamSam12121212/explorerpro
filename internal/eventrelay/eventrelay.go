package eventrelay

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const (
	EventTypeClientResponse = "client.response.create"
	EventTypeThreadSnapshot = "thread.snapshot"
	EventTypeThreadItem     = "thread.item.appended"

	maxFrameSize = 16 << 20 // 16 MB
	headerSize   = 4 + 8 + 1 // frame length + thread_id + event_type_length
)

type Frame struct {
	ThreadID  int64
	EventType string
	Payload   json.RawMessage
}

func WriteFrame(w io.Writer, f Frame) error {
	etBytes := []byte(f.EventType)
	if len(etBytes) > 255 {
		return fmt.Errorf("event type too long: %d bytes", len(etBytes))
	}

	payloadLen := len(f.Payload)
	frameLen := 8 + 1 + len(etBytes) + payloadLen

	var header [headerSize]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(frameLen))
	binary.BigEndian.PutUint64(header[4:12], uint64(f.ThreadID))
	header[12] = byte(len(etBytes))

	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(etBytes); err != nil {
		return err
	}
	if _, err := w.Write(f.Payload); err != nil {
		return err
	}
	return nil
}

func ReadFrame(r io.Reader) (Frame, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}

	frameLen := binary.BigEndian.Uint32(header[0:4])
	if frameLen > maxFrameSize {
		return Frame{}, fmt.Errorf("frame too large: %d bytes", frameLen)
	}

	threadID := int64(binary.BigEndian.Uint64(header[4:12]))
	etLen := int(header[12])

	remaining := int(frameLen) - 8 - 1
	if remaining < etLen {
		return Frame{}, fmt.Errorf("invalid frame: event type length %d exceeds remaining %d", etLen, remaining)
	}

	buf := make([]byte, remaining)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Frame{}, err
	}

	return Frame{
		ThreadID:  threadID,
		EventType: string(buf[:etLen]),
		Payload:   json.RawMessage(buf[etLen:]),
	}, nil
}
