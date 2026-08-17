package version

import "testing"

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{
		"v1.2.3":     true,
		"v0.0.0":     true,
		"v10.20.30":  true,
		"dev":        false,
		"":           false,
		"1.2.3":      false,
		"v1.2":       false,
		"v1.2.3.4":   false,
		"v1.2.3-rc1": false,
	} {
		if got := Valid(s); got != want {
			t.Errorf("Valid(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v1.2.3", "v1.2.4", -1},
		{"v1.3.0", "v1.2.9", 1},
		{"v2.0.0", "v1.99.99", 1},
		// Numeric, not lexicographic: v0.9.0 < v0.10.0.
		{"v0.9.0", "v0.10.0", -1},
	} {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := Compare(tc.b, tc.a); got != -tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}
