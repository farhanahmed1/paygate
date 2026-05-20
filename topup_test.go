package paygate

import "testing"

func TestCentsToAmount(t *testing.T) {
	cases := map[int64]string{
		1:       "0.0100",
		5:       "0.0500",
		99:      "0.9900",
		100:     "1.0000",
		2500:    "25.0000",
		2505:    "25.0500",
		2550:    "25.5000",
		1234567: "12345.6700",
	}
	for cents, want := range cases {
		if got := CentsToAmount(cents); got != want {
			t.Errorf("CentsToAmount(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestPublicConfigKey(t *testing.T) {
	if got := publicConfigKey(GatewayStripe); got != "publishable_key" {
		t.Errorf("publicConfigKey(stripe) = %q, want publishable_key", got)
	}
	if got := publicConfigKey(GatewayPayPal); got != "client_id" {
		t.Errorf("publicConfigKey(paypal) = %q, want client_id", got)
	}
	if got := publicConfigKey("unknown"); got != "" {
		t.Errorf("publicConfigKey(unknown) = %q, want empty", got)
	}
}
