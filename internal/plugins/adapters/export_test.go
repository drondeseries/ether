package adapters

import "time"

// SetFirstOpenWait overrides how long a first open waits for a fill and
// returns a function that restores the previous value.
func SetFirstOpenWait(d time.Duration) (restore func()) {
	prev := firstOpenWait
	firstOpenWait = d
	return func() { firstOpenWait = prev }
}
