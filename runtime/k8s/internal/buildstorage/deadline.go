package buildstorage

import "time"

// CheckDeadline prevents a queued Pod starting after its signed authority has
// expired, even if its controller was unavailable while it waited for a node.
func CheckDeadline(phase string, expiresAt int64, now time.Time) error {
	budget := 10*time.Minute + 30*time.Second
	if phase == "publish" {
		budget = 5*time.Minute + 30*time.Second
	} else if phase != "build" {
		return ErrCustody
	}
	remaining := time.UnixMilli(expiresAt).Sub(now)
	if expiresAt <= 0 || remaining < budget || remaining > 30*time.Minute {
		return ErrCustody
	}
	return nil
}
