package buildstorage

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWorkspaceGuardRejectsUninstalledPathsAndIdentity(t *testing.T) {
	for _, input := range [][4]string{{"/tmp", strings.Repeat("a", 64), "11111111-1111-1111-1111-111111111111", "build"}, {"/slot", "invalid", "11111111-1111-1111-1111-111111111111", "build"}, {"/slot", strings.Repeat("a", 64), "invalid", "build"}, {"/slot", strings.Repeat("a", 64), "11111111-1111-1111-1111-111111111111", "repair"}} {
		if _, e := Inspect(input[0], input[1], input[2], input[3]); e == nil {
			t.Fatal("uninstalled guard input accepted")
		}
	}
}

// Only a disposable container fixture sets this switch. Production binaries
// have no allocation or repair operation; the test prepares a previously empty
// dedicated /slot tmpfs for subsequent calls as UID 1000.
func TestPrepareWorkspaceGuardFixture(t *testing.T) {
	if os.Getenv("AI_SDLC_PREPARE_GUARD_FIXTURE") != "1" {
		t.Skip("explicit disposable container fixture required")
	}
	if os.Geteuid() != 0 {
		t.Fatal("fixture preparation requires container root")
	}
	f, e := os.Open("/slot")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	fs, e := filesystem(int(f.Fd()))
	if e != nil {
		t.Fatal(e)
	}
	entries, e := f.ReadDir(1)
	if e != nil && e != io.EOF {
		t.Fatal(e)
	}
	if len(entries) != 0 {
		t.Fatal("fixture slot not empty")
	}
	boot, e := nodeBootID()
	if e != nil {
		t.Fatal(e)
	}
	id := os.Getenv("AI_SDLC_GUARD_FIXTURE_ID")
	if !digest.MatchString(id) {
		t.Fatal("fixture identity required")
	}
	raw, _ := json.Marshal(Marker{Version: "ai-sdlc.local-build-custody/v1", StorageIdentitySHA256: id, NodeBootID: boot, FSID: fs.Fsid.Val, CapacityBytes: Capacity, InodeLimit: Inodes})
	marker, e := os.OpenFile("/slot/"+MarkerName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = marker.Write(raw); e != nil {
		t.Fatal(e)
	}
	if e = marker.Close(); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir("/slot/workspace", 0o700); e != nil {
		t.Fatal(e)
	}
	if e = os.Chown("/slot/workspace", 1000, 1000); e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod("/slot", 0o555); e != nil {
		t.Fatal(e)
	}
	var st unix.Stat_t
	if unix.Stat("/slot/custody.json", &st) != nil || st.Uid != 0 || st.Mode&0o7777 != 0o444 {
		t.Fatal("fixture marker permissions")
	}
}
