package handlers

import (
	"strings"
	"testing"
	"time"

	"github.com/jiotv-go/jiotv_go/v3/pkg/television"
)

func TestNormalizeCatchupTime(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantEpoch int64
		wantTime  string
	}{
		{
			name:      "Unix seconds from TiviMate",
			input:     "1784908800",
			wantEpoch: 1784908800000,
			wantTime:  "20260724T160000",
		},
		{
			name:      "Unix milliseconds from web player",
			input:     "1784908800000",
			wantEpoch: 1784908800000,
			wantTime:  "20260724T160000",
		},
		{
			name:      "Jio formatted timestamp",
			input:     "20260724T160000",
			wantEpoch: 1784908800000,
			wantTime:  "20260724T160000",
		},
		{
			name:      "Compact formatted timestamp",
			input:     "20260724160000",
			wantEpoch: 1784908800000,
			wantTime:  "20260724T160000",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			epoch, formatted, err := normalizeCatchupTime(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if epoch != test.wantEpoch || formatted != test.wantTime {
				t.Fatalf("normalizeCatchupTime(%q) = (%d, %q), want (%d, %q)", test.input, epoch, formatted, test.wantEpoch, test.wantTime)
			}
		})
	}
}

func TestFindCatchupSerial(t *testing.T) {
	start := int64(1784908800000)
	end := start + int64(30*time.Minute/time.Millisecond)
	epgData := []map[string]interface{}{
		{
			"startEpoch": start,
			"endEpoch":   end,
			"srno":       "260724143001",
		},
	}

	serial, ok := findCatchupSerial(epgData, start+30_000, end+30_000)
	if !ok || serial != "260724143001" {
		t.Fatalf("findCatchupSerial() = (%q, %v)", serial, ok)
	}
	if _, ok := findCatchupSerial(epgData, start+2*60_000, end+2*60_000); ok {
		t.Fatal("programme outside tolerance was matched")
	}
}

func TestCatchupDayOffset(t *testing.T) {
	location := time.FixedZone("IST", 5*60*60+30*60)
	now := time.Date(2026, 7, 24, 22, 0, 0, 0, location)
	start := time.Date(2026, 7, 20, 8, 0, 0, 0, location).UnixMilli()
	if offset := catchupDayOffset(start, now); offset != -4 {
		t.Fatalf("catchupDayOffset() = %d, want -4", offset)
	}
}

func TestBuildCatchupAttributes(t *testing.T) {
	channel := television.Channel{ID: "143", IsCatchupAvailable: true}
	attributes := buildCatchupAttributes("http://tv.example:5001", channel)
	for _, expected := range []string{
		`catchup="default"`,
		`catchup-days="7"`,
		`catchup-source="http://tv.example:5001/catchup/stream/143?start={utc}&end={utcend}"`,
	} {
		if !strings.Contains(attributes, expected) {
			t.Fatalf("catchup attributes missing %q: %s", expected, attributes)
		}
	}

	channel.IsCatchupAvailable = false
	if attributes := buildCatchupAttributes("http://tv.example:5001", channel); attributes != "" {
		t.Fatalf("non-catchup channel attributes = %q", attributes)
	}
}
