package disk

import (
	"io"
	"strings"
	"testing"
)

func TestCappedBufferReaderFromCannotBypassLimit(t *testing.T) {
	for _, size := range []int{16, 17} {
		b := &cappedBuffer{limit: 16}
		// Hide strings.Reader's WriterTo so io.Copy selects dst.ReadFrom,
		// just as the subprocess stdout copier can.
		r := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", size))}
		_, err := io.Copy(b, r)
		if size == 16 && (err != nil || b.overflow || b.Len() != 16) {
			t.Fatal("exact-limit output rejected", err, b.Len())
		}
		if size == 17 && (err == nil || !b.overflow || b.Len() > b.limit) {
			t.Fatal("ReaderFrom bypassed output limit", err, b.Len())
		}
	}
}
