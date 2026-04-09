package postgresstore

import "testing"

func TestShouldPersistThreadEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		eventType string
		want      bool
	}{
		{
			name:      "client checkpoint persists",
			eventType: "client.response.create",
			want:      true,
		},
		{
			name:      "terminal event persists",
			eventType: "response.completed",
			want:      true,
		},
		{
			name:      "output delta stays ephemeral",
			eventType: "response.output_text.delta",
			want:      false,
		},
		{
			name:      "reasoning delta stays ephemeral",
			eventType: "response.reasoning_text.delta",
			want:      false,
		},
		{
			name:      "blank event does not persist",
			eventType: "  ",
			want:      false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := shouldPersistThreadEvent(tt.eventType); got != tt.want {
				t.Fatalf("shouldPersistThreadEvent(%q) = %t, want %t", tt.eventType, got, tt.want)
			}
		})
	}
}
