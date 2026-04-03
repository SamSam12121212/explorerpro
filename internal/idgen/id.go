package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func New(prefix string) (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate random id bytes: %w", err)
	}

	return prefix + "_" + hex.EncodeToString(buf[:]), nil
}
