package steamcrypto

import (
	"bytes"
	"testing"
)

func TestSymmetricRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	iv := bytes.Repeat([]byte{9}, 16)
	for _, n := range []int{0, 1, 15, 16, 17, 1000} {
		plain := bytes.Repeat([]byte{0xAB}, n)
		enc, err := SymmetricEncrypt(plain, key, iv)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := SymmetricDecrypt(enc, key)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !bytes.Equal(dec, plain) {
			t.Fatalf("n=%d: round trip mismatch", n)
		}
	}
}

func TestDecryptRejectsBadPadding(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	enc, _ := SymmetricEncrypt([]byte("hello"), key, bytes.Repeat([]byte{1}, 16))
	enc[len(enc)-1] ^= 0xFF
	if _, err := SymmetricDecrypt(enc, key); err == nil {
		t.Fatal("expected padding error")
	}
}

func TestAdler(t *testing.T) {
	// Standard Adler-32 of "abc" is 0x024d0127; Steam's zero-seeded variant
	// drops the initial 1 from the low half and shifts the high half.
	if got := Adler([]byte("abc")); got != 0x024a0126 {
		t.Fatalf("got %08x", got)
	}
	if got := Adler(nil); got != 0 {
		t.Fatalf("empty: %08x", got)
	}
}
