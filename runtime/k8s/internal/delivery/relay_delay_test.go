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
	if len(retryDelays) != 7 || retryDelays[0] != time.Second || total < 25*time.Minute || total > 30*time.Minute {
		t.Fatal("eight attempts must span about 26 minutes, retrying quickly first", total)
	}
}
