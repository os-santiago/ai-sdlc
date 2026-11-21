package delivery

import (
	"testing"
	"time"
)

func TestRetriesCoverATargetThatIsStillStarting(t *testing.T) {
	var total time.Duration
	for _, d := range retryDelays {
		total += d
	}
	if len(retryDelays) != 7 || retryDelays[0] != time.Second || total < 15*time.Minute || total >= 20*time.Minute {
		t.Fatal("eight attempts must fit the 20-minute command lifetime, retrying quickly first", total)
	}
}
