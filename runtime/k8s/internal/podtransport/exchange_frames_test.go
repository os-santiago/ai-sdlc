package podtransport

import (
	"bytes"
	"testing"
)

// A helper that flushes one byte per message must not make the controller
// keep far more than the bytes it received: a million 1-byte messages keep
// about 1 MiB of chunks, not hundreds of MiB of per-message buffers.
func TestFramesKeepAboutTheBytesReceived(t *testing.T) {
	const messages = 1 << 20
	f := newFrames(1 << 30)
	for i := 0; i < messages; i++ {
		f.add([]byte{byte(i)})
	}
	retained := 0
	for _, p := range f.parts {
		retained += cap(p)
	}
	if f.Len() != messages || retained > messages+(1<<20) {
		t.Fatalf("kept %d bytes of chunks for %d bytes", retained, f.Len())
	}
	joined := f.Bytes()
	if len(joined) != messages || joined[255] != 255 || joined[256] != 0 {
		t.Fatal("joined bytes differ")
	}
	small := newFrames(8192)
	small.add(bytes.Repeat([]byte("x"), 3))
	if cap(small.parts[0]) != 8192 || small.Len() != 3 {
		t.Fatal("small channel chunk not sized by its limit")
	}
	if newFrames(8192).Bytes() != nil {
		t.Fatal("empty channel must join to nil")
	}
}
