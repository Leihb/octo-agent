package fixture

import "testing"

func TestEffectiveTimeoutDefault(t *testing.T) {
	if got := EffectiveTimeout(&Config{}); got != 30 {
		t.Errorf("nil Timeout: got %d, want 30", got)
	}
}

func TestEffectiveTimeoutSet(t *testing.T) {
	v := 90
	if got := EffectiveTimeout(&Config{Timeout: &v}); got != 90 {
		t.Errorf("set Timeout: got %d, want 90", got)
	}
}
