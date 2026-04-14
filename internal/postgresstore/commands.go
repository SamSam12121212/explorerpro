package postgresstore

import (
	"context"
	"fmt"
	"strings"
)

func (s *Store) ReserveCommandID(ctx context.Context, bus, kind string) (int64, error) {
	bus = strings.TrimSpace(bus)
	kind = strings.TrimSpace(kind)
	if bus == "" {
		return 0, fmt.Errorf("reserve command id: bus is required")
	}
	if kind == "" {
		return 0, fmt.Errorf("reserve command id: kind is required")
	}

	var commandID int64
	if err := s.pool.QueryRow(ctx, `
INSERT INTO commands (
    bus,
    kind
) VALUES (
    $1,
    $2
)
RETURNING id
`, bus, kind).Scan(&commandID); err != nil {
		return 0, fmt.Errorf("reserve command id for %s/%s: %w", bus, kind, err)
	}

	return commandID, nil
}

func (s *Store) LoadOrCreateCommandID(ctx context.Context, bus, kind, dedupeKey string) (int64, error) {
	bus = strings.TrimSpace(bus)
	kind = strings.TrimSpace(kind)
	dedupeKey = strings.TrimSpace(dedupeKey)
	if bus == "" {
		return 0, fmt.Errorf("load or create command id: bus is required")
	}
	if kind == "" {
		return 0, fmt.Errorf("load or create command id: kind is required")
	}
	if dedupeKey == "" {
		return 0, fmt.Errorf("load or create command id: dedupe key is required")
	}

	var commandID int64
	if err := s.pool.QueryRow(ctx, `
INSERT INTO commands (
    bus,
    kind,
    dedupe_key
) VALUES (
    $1,
    $2,
    $3
)
ON CONFLICT (bus, kind, dedupe_key) WHERE dedupe_key IS NOT NULL
DO UPDATE SET
    updated_at = now()
RETURNING id
`, bus, kind, dedupeKey).Scan(&commandID); err != nil {
		return 0, fmt.Errorf("load or create command id for %s/%s/%s: %w", bus, kind, dedupeKey, err)
	}

	return commandID, nil
}
