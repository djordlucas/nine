package agent

import (
	"context"
	"encoding/json"
	"time"
)

// shippedTime implements the time tool: the current date and time in UTC, the
// same shape the sandboxed tool returned (the guest had no timezone database,
// so UTC-only was already the behavior).
func shippedTime(_ context.Context, _ json.RawMessage) (string, error) {
	t := time.Now().UTC()
	days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	months := []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	h12 := t.Hour() % 12
	if h12 == 0 {
		h12 = 12
	}
	meridiem := "AM"
	if t.Hour() >= 12 {
		meridiem = "PM"
	}
	human := days[t.Weekday()] + ", " + months[t.Month()-1] + " " +
		itoa(t.Day()) + ", " + itoa(t.Year()) + " " +
		itoa(h12) + ":" + pad2(t.Minute()) + ":" + pad2(t.Second()) + " " + meridiem + " UTC"
	out, _ := json.Marshal(map[string]string{
		"iso8601":  t.Format(time.RFC3339),
		"human":    human,
		"timezone": "UTC",
	})
	return string(out), nil
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
