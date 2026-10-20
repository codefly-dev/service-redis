package cache

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	cache "github.com/codefly-dev/interface-cache/go/cache"
)

// Every field the Layer contract names round-trips through the entry format:
// at nanosecond precision, with the zero times kept zero, a full-width
// sequence, and an empty value kept distinct from none.
func TestEntryRoundTrip(t *testing.T) {
	confirmed := time.Date(2026, 10, 20, 15, 4, 5, 123456789, time.UTC)
	for name, want := range map[string]cache.Entry{
		"full":     {Value: []byte{0, 1, 2, 0xff, 0}, Version: `"etag-1"`, Sequence: 1<<63 + 7, Confirmed: confirmed, Expires: confirmed.Add(2 * time.Hour)},
		"empty":    {Value: []byte{}, Version: "v2"},
		"negative": {Missing: true, Sequence: 3, Confirmed: confirmed},
		"bare":     {Value: []byte("plain")},
	} {
		got, err := decode(encode(want))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got.Value, want.Value) || (got.Value == nil) != (want.Value == nil) ||
			got.Version != want.Version || got.Sequence != want.Sequence || got.Missing != want.Missing ||
			!got.Confirmed.Equal(want.Confirmed) || got.Confirmed.IsZero() != want.Confirmed.IsZero() ||
			!got.Expires.Equal(want.Expires) || got.Expires.IsZero() != want.Expires.IsZero() {
			t.Fatalf("%s: decode(encode(%+v)) = %+v", name, want, got)
		}
	}
}

// fencedScript reads the sequence at sequenceOffset of the stored bytes.
func TestSequenceSitsAtItsOffset(t *testing.T) {
	raw := encode(cache.Entry{Sequence: 0x0102030405060708, Value: []byte("v")})
	if got := raw[sequenceOffset : sequenceOffset+8]; !bytes.Equal(got, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("the sequence at offset %d is %x", sequenceOffset, got)
	}
}

// encodeV1 is the entry format of cache/v0.2.0 and before: format 1, flags,
// FreshUntil as Unix nanoseconds, the version length as a uvarint, the version,
// then the value.
func encodeV1(value, version string, freshUntil time.Time) []byte {
	buf := []byte{formatV1, 0}
	buf = binary.BigEndian.AppendUint64(buf, uint64(freshUntil.UnixNano()))
	buf = binary.AppendUvarint(buf, uint64(len(version)))
	buf = append(buf, version...)
	return append(buf, value...)
}

// A value Get cannot decode is a miss, never an error: the stack counts an
// error against the tier's breaker (see decode), so a keyspace of entries left
// by cache/v0.2.0 would read as a failing server after the upgrade instead of
// being refilled.
func TestUndecodableValueIsAMiss(t *testing.T) {
	v2 := encode(cache.Entry{Value: []byte("v"), Version: "1"})
	for name, raw := range map[string][]byte{
		"formatV1":          encodeV1("value", "etag", time.Now().Add(time.Hour)),
		"another program's": []byte("written externally"),
		"empty":             {},
		"header only":       v2[:headerLen],
		"truncated version": v2[:headerLen+1],
	} {
		if _, err := decode(raw); !errors.Is(err, cache.ErrMiss) {
			t.Errorf("decode(%s) = %v, want cache.ErrMiss", name, err)
		}
	}
}
