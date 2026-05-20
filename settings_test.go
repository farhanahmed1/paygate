package paygate

import (
	"crypto/rand"
	"testing"
)

// testService builds a Service with a random 32-byte key and a nil DB. The
// crypto helpers exercised here do not touch the database.
func testService(t *testing.T) *Service {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return NewService(nil, key, "payment_gateway")
}

func TestGatewayConfigEncryptRoundTrip(t *testing.T) {
	s := testService(t)
	in := map[string]string{"secret_key": "sk_test_abc", "publishable_key": "pk_test_xyz"}

	enc, err := s.encryptConfig(GatewayStripe, in)
	if err != nil {
		t.Fatalf("encryptConfig: %v", err)
	}
	if enc == "" {
		t.Fatal("expected non-empty ciphertext for a populated config")
	}

	out, err := s.decryptConfig(GatewayStripe, enc)
	if err != nil {
		t.Fatalf("decryptConfig: %v", err)
	}
	if len(out) != len(in) || out["secret_key"] != in["secret_key"] || out["publishable_key"] != in["publishable_key"] {
		t.Fatalf("round-trip mismatch: got %v want %v", out, in)
	}
}

func TestGatewayConfigEmptyIsBlank(t *testing.T) {
	s := testService(t)

	enc, err := s.encryptConfig(GatewayPayPal, nil)
	if err != nil {
		t.Fatalf("encryptConfig(empty): %v", err)
	}
	if enc != "" {
		t.Fatalf("empty config should encrypt to blank string, got %q", enc)
	}

	out, err := s.decryptConfig(GatewayPayPal, "")
	if err != nil {
		t.Fatalf("decryptConfig(blank): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("blank ciphertext should decrypt to empty map, got %v", out)
	}
}

func TestGatewayConfigAADBinding(t *testing.T) {
	// A ciphertext bound to stripe must not decrypt under the paypal AAD —
	// the gateway name is part of the AES-GCM additional authenticated data.
	s := testService(t)

	enc, err := s.encryptConfig(GatewayStripe, map[string]string{"secret_key": "sk_test_abc"})
	if err != nil {
		t.Fatalf("encryptConfig: %v", err)
	}
	if _, err := s.decryptConfig(GatewayPayPal, enc); err == nil {
		t.Fatal("expected AAD-mismatch error decrypting a stripe ciphertext as paypal")
	}
}
