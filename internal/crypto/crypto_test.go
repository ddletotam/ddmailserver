package crypto

import (
	"testing"
	"time"
)

func TestKeyCacheDecryptsLikeDecryptPassword(t *testing.T) {
	ct, err := EncryptPassword("hunter2", "master")
	if err != nil {
		t.Fatal(err)
	}
	c := NewKeyCache()
	for i := 0; i < 2; i++ {
		got, err := c.Decrypt(ct, "master")
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if got != "hunter2" {
			t.Fatalf("pass %d: got %q", i, got)
		}
	}
	if n := len(c.keys); n != 1 {
		t.Fatalf("one ciphertext must derive one key, cache has %d", n)
	}
	plain, err := DecryptPassword(ct, "master")
	if err != nil || plain != "hunter2" {
		t.Fatalf("DecryptPassword: %q, %v", plain, err)
	}
}

// The cache is keyed by the secret too: a wrong secret must not reuse the
// key derived from the right one.
func TestKeyCacheWrongSecretFails(t *testing.T) {
	ct, err := EncryptPassword("hunter2", "master")
	if err != nil {
		t.Fatal(err)
	}
	c := NewKeyCache()
	if _, err := c.Decrypt(ct, "master"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Decrypt(ct, "other"); err == nil {
		t.Fatal("decrypting with a wrong secret must fail")
	}
}

// The point of the cache: a repeat decryption skips PBKDF2 (100k
// iterations), which made every account load cost ~50 ms per secret.
func TestKeyCacheRepeatIsCheap(t *testing.T) {
	ct, err := EncryptPassword("hunter2", "master")
	if err != nil {
		t.Fatal(err)
	}
	c := NewKeyCache()
	start := time.Now()
	if _, err := c.Decrypt(ct, "master"); err != nil {
		t.Fatal(err)
	}
	first := time.Since(start)
	start = time.Now()
	for i := 0; i < 20; i++ {
		if _, err := c.Decrypt(ct, "master"); err != nil {
			t.Fatal(err)
		}
	}
	repeat := time.Since(start)
	if repeat > first {
		t.Fatalf("20 cached decryptions took %v, one uncached %v", repeat, first)
	}
}

func TestKeyCacheStartsOverAtLimit(t *testing.T) {
	c := NewKeyCache()
	for i := 0; i < keyCacheLimit; i++ {
		c.keys[[32]byte{byte(i), byte(i >> 8)}] = []byte{1}
	}
	ct, err := EncryptPassword("x", "master")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Decrypt(ct, "master"); err != nil || got != "x" {
		t.Fatalf("got %q, %v", got, err)
	}
	if n := len(c.keys); n != 1 {
		t.Fatalf("full cache must start over, has %d", n)
	}
}
