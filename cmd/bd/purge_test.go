package main

import (
	"testing"
	"time"
)

// TestParseOlderThan pins --older-than on both `bd purge` and `bd prune`.
// Day-denominated values keep their meaning; hour (and finer) values are taken
// exactly rather than floored to whole days — "36h" used to become 24h and
// sweep rows younger than the caller asked for.
func TestParseOlderThan(t *testing.T) {
	const day = 24 * time.Hour
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		// Unchanged day semantics.
		{"7", 7 * day, false},
		{"30", 30 * day, false},
		{"7d", 7 * day, false},
		{"30d", 30 * day, false},
		{"2w", 14 * day, false},
		{"1w", 7 * day, false},
		{"7D", 7 * day, false},
		{"2W", 14 * day, false},
		// Hour precision, no longer floored to days.
		{"48h", 48 * time.Hour, false},
		{"168h", 7 * day, false},
		{"36h", 36 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"36H", 36 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		// Refusals.
		{"", 0, true},
		{"0", 0, true},
		{"-1", 0, true},
		{"0d", 0, true},
		{"0h", 0, true},
		{"-36h", 0, true},
		{"abc", 0, true},
		{"7x", 0, true},
		{"d", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseOlderThan(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseOlderThan(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseOlderThan(%q) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}
