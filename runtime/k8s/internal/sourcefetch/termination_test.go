package sourcefetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"testing"
)

func TestArchiveRejectsDanglingMetadata(t *testing.T) {
	for _, kind := range []byte{'x', 'L'} {
		for _, terminated := range []bool{false, true} {
			t.Run(fmt.Sprintf("type-%c-terminated-%v", kind, terminated), func(t *testing.T) {
				raw := archiveFixture(t, nil)
				gz, _ := gzip.NewReader(bytes.NewReader(raw))
				expanded, _ := io.ReadAll(gz)
				gz.Close()
				var p bytes.Buffer
				tw := tar.NewWriter(&p)
				if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax", PAXRecords: map[string]string{"comment": "unused"}}); err != nil {
					t.Fatal(err)
				}
				tw.Close()
				meta := append([]byte(nil), p.Bytes()[:1024]...)
				meta[156] = kind
				for i := 148; i < 156; i++ {
					meta[i] = ' '
				}
				sum := 0
				for _, v := range meta[:512] {
					sum += int(v)
				}
				copy(meta[148:156], []byte(fmt.Sprintf("%06o\x00 ", sum)))
				malformed := append(expanded[:len(expanded)-1024], meta...)
				if terminated {
					malformed = append(malformed, make([]byte, 1024)...)
				}
				var out bytes.Buffer
				zw := gzip.NewWriter(&out)
				zw.Write(malformed)
				zw.Close()
				if _, err := DecodeArchive(out.Bytes(), testSHA); err == nil {
					t.Fatal("dangling PAX header without tar end accepted")
				}
			})
		}
	}
}
