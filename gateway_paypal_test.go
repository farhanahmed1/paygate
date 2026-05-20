package paygate

import "testing"

func TestFormatPayPalAmount(t *testing.T) {
	cases := map[int64]string{
		1:       "0.01",
		5:       "0.05",
		100:     "1.00",
		2500:    "25.00",
		2550:    "25.50",
		2505:    "25.05",
		1234567: "12345.67",
	}
	for cents, want := range cases {
		if got := formatPayPalAmount(cents); got != want {
			t.Errorf("formatPayPalAmount(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestNewPayPalGatewayRequiresCredentials(t *testing.T) {
	if _, err := newPayPalGateway(map[string]string{"client_id": "abc"}, true); err == nil {
		t.Fatal("expected error when client_secret is missing")
	}
	if _, err := newPayPalGateway(map[string]string{"client_secret": "xyz"}, true); err == nil {
		t.Fatal("expected error when client_id is missing")
	}
}
