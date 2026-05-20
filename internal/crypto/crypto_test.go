package crypto

import (
	"bytes"
	"testing"
)

func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key, aad := key32(), []byte("payment_gateway|stripe")
	msg := []byte(`{"secret_key":"sk_test_x","webhook_secret":"whsec_y"}`)
	ct, err := Encrypt(key, aad, msg)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := Decrypt(key, aad, ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, msg)
	}
}

func TestDecryptAADMismatch(t *testing.T) {
	key := key32()
	ct, err := Encrypt(key, []byte("payment_gateway|stripe"), []byte("secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := Decrypt(key, []byte("payment_gateway|paypal"), ct); err == nil {
		t.Fatal("expected error decrypting under a different aad")
	}
}

func TestEncryptKeyLength(t *testing.T) {
	if _, err := Encrypt(make([]byte, 16), nil, []byte("x")); err != ErrKeyLength {
		t.Fatalf("want ErrKeyLength, got %v", err)
	}
	if _, err := Decrypt(make([]byte, 16), nil, "AAAA"); err != ErrKeyLength {
		t.Fatalf("want ErrKeyLength, got %v", err)
	}
}
