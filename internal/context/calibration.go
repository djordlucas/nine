package ninectx

import "sync/atomic"

// Calibration learns how far the token estimate (countTokens, a fixed bytes
// per token ratio measured on one model family) falls short of what the
// provider counts, and scales the budget to match. On Mistral a full context
// was billed up to 25% above the estimate, so a request Nine sized to fit
// 32 768 tokens was nearer 41 000.
//
// It only ever scales up: an estimate above the provider's count trims a little
// more than needed, which is harmless, while one below it spends headroom that
// is not there. Every builder of one model shares one Calibration, since the
// ratio belongs to the tokenizer, not to a session.
type Calibration struct {
	// permille is the provider's count per 1000 estimated tokens: 1000 until a
	// call says otherwise.
	permille atomic.Int64
}

// Calibration bounds: a ratio is read only from calls large enough to be
// stable, moves a quarter of the way toward each new reading, and stays
// between 1.0 and 1.6.
const (
	calibMinEstimate = 1000
	calibMin         = 1000
	calibMax         = 1600
)

// NewCalibration returns a Calibration that starts at the estimate.
func NewCalibration() *Calibration {
	c := &Calibration{}
	c.permille.Store(calibMin)
	return c
}

// Observe records one call: the tokens the builder estimated and the input
// tokens the provider reported for the same request.
func (c *Calibration) Observe(estimated, actual int) {
	if c == nil || estimated < calibMinEstimate || actual <= 0 {
		return
	}
	reading := int64(actual) * 1000 / int64(estimated)
	for {
		old := c.permille.Load()
		next := min(max((old*3+reading)/4, calibMin), calibMax)
		if c.permille.CompareAndSwap(old, next) {
			return
		}
	}
}

// Scale is the provider's count per 1000 estimated tokens; 1000 for a nil
// Calibration.
func (c *Calibration) Scale() int {
	if c == nil {
		return calibMin
	}
	return int(c.permille.Load())
}
