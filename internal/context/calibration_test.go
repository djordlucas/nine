package ninectx_test

import (
	"strings"
	"testing"

	ninectx "nine/internal/context"
	"nine/internal/llm"
)

// The calibration moves a quarter of the way toward each reading, never below
// the estimate and never above 1.6; small calls are ignored.
func TestCalibrationOnlyScalesUp(t *testing.T) {
	c := ninectx.NewCalibration()
	if c.Scale() != 1000 {
		t.Fatalf("start = %d, want 1000", c.Scale())
	}
	c.Observe(500, 900) // too small to read
	if c.Scale() != 1000 {
		t.Errorf("a small call moved it to %d", c.Scale())
	}
	c.Observe(10_000, 12_000) // reads 1.2
	if c.Scale() != 1050 {
		t.Errorf("after one 1.2 reading = %d, want 1050", c.Scale())
	}
	for range 50 {
		c.Observe(10_000, 30_000)
	}
	if c.Scale() != 1600 {
		t.Errorf("after many 3.0 readings = %d, want the 1600 cap", c.Scale())
	}
	for range 50 {
		c.Observe(10_000, 5_000)
	}
	if c.Scale() != 1000 {
		t.Errorf("after over-estimates = %d, want back at 1000, never below", c.Scale())
	}
	var none *ninectx.Calibration
	none.Observe(10_000, 20_000)
	if none.Scale() != 1000 {
		t.Error("a nil calibration does not read as 1.0")
	}
}

// Calibrated, the builder keeps less history in the same budget and reports
// its usage in the provider's count.
func TestCalibratedBuilderSpendsLessOfTheBudget(t *testing.T) {
	history := make([]llm.Message, 0, 40)
	for range 40 {
		history = append(history, llm.Message{Role: "user", Text: strings.Repeat("word ", 200)})
	}
	plain := ninectx.New(ninectx.Config{Budget: 8000})
	calib := ninectx.NewCalibration()
	for range 30 {
		calib.Observe(10_000, 15_000)
	}
	scaled := ninectx.New(ninectx.Config{Budget: 8000, Calibration: calib})

	reqPlain, usedPlain := plain.BuildWithUsage(ninectx.BuildInput{SystemCore: "core", History: history})
	reqScaled, usedScaled := scaled.BuildWithUsage(ninectx.BuildInput{SystemCore: "core", History: history})
	if len(reqScaled.Messages) >= len(reqPlain.Messages) {
		t.Errorf("calibrated kept %d messages, plain %d; want fewer", len(reqScaled.Messages), len(reqPlain.Messages))
	}
	if usedScaled > 8000 || usedPlain > 8000 {
		t.Errorf("used %d (calibrated) and %d (plain), want both within the 8000 budget", usedScaled, usedPlain)
	}
	if got := scaled.Calibrated(1000); got != calib.Scale() {
		t.Errorf("Calibrated(1000) = %d, want %d", got, calib.Scale())
	}
}
