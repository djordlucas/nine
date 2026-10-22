package cron

import (
	"testing"
	"time"
)

func TestParseCadence(t *testing.T) {
	cases := []struct {
		in       string
		every    time.Duration
		schedule string
		bad      bool
	}{
		{in: ""},
		{in: "24h", every: 24 * time.Hour},
		{in: " 90m ", every: 90 * time.Minute},
		{in: "0 8 * * *", schedule: "0 8 * * *"},
		{in: "30s", bad: true},
		{in: "daily", bad: true},
		{in: "0 25 * * *", bad: true},
	}
	for _, c := range cases {
		every, schedule, err := ParseCadence(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseCadence(%q) accepted it", c.in)
			}
			continue
		}
		if err != nil || every != c.every || schedule != c.schedule {
			t.Errorf("ParseCadence(%q) = %s, %q, %v; want %s, %q", c.in, every, schedule, err, c.every, c.schedule)
		}
	}
}

func TestFormatEvery(t *testing.T) {
	for d, want := range map[time.Duration]string{24 * time.Hour: "24h", 90 * time.Minute: "90m", 45 * time.Second: "45s"} {
		if got := FormatEvery(d); got != want {
			t.Errorf("FormatEvery(%s) = %q, want %q", d, got, want)
		}
	}
}
