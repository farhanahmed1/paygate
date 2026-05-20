package paygate

import "testing"

func TestValidateTopupAmount(t *testing.T) {
	valid := []string{
		"25.0000",
		"0.0001",
		"1",
		"100",
		"99999999.9999",
		"0.5",
		"12.34",
	}
	for _, s := range valid {
		if err := validateTopupAmount(s); err != nil {
			t.Errorf("validateTopupAmount(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",         // empty
		"0",        // zero
		"0.0000",   // zero with scale
		"-5",       // negative
		"-0.01",    // negative
		"1.23456",  // 5 fractional digits (> scale 4)
		"abc",      // non-numeric
		"25.00.00", // malformed
		"1,000",    // grouping separator
		" 25.00",   // leading space
		"25 ",      // trailing space
		"$25",      // currency symbol
		"1e3",      // scientific notation
	}
	for _, s := range invalid {
		if err := validateTopupAmount(s); err == nil {
			t.Errorf("validateTopupAmount(%q) = nil, want error", s)
		}
	}
}
