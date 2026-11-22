package modelmailbox

import (
	"syscall"
	"testing"
)

func TestRemovedDuringReadIsOnlyAnUnlinkedUnchangedFile(t *testing.T) {
	before := syscall.Stat_t{Nlink: 1, Size: 10, Mtim: syscall.Timespec{Sec: 5}}
	removed := before
	removed.Nlink = 0
	if !removedDuringRead(before, removed) {
		t.Fatal("removed envelope not recognized")
	}
	for name, after := range map[string]syscall.Stat_t{
		"still linked": before,
		"hard linked":  {Nlink: 2, Size: 10, Mtim: syscall.Timespec{Sec: 5}},
		"resized":      {Nlink: 0, Size: 11, Mtim: syscall.Timespec{Sec: 5}},
		"rewritten":    {Nlink: 0, Size: 10, Mtim: syscall.Timespec{Sec: 6}},
	} {
		if removedDuringRead(before, after) {
			t.Fatal(name + " treated as removed")
		}
	}
}
