package notion

import "testing"

func TestNamesAfter(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"x\n\nAfter: MKT-3", true},
		{"After: MKT-3\n\nmore", true},
		{"after: mkt-3", true},
		{"**After:** MKT-3", true},
		{"After: [MKT-3: Book the venue](https://www.notion.so/mkt-3)", true},
		{"After: [MKT-3](https://www.notion.so/mkt-3)", true},
		{"After: MKT-30", false},
		{"After: [MKT-4: Other](https://www.notion.so/mkt-3)", false},
		{"  After: MKT-3", false},
		{"Blocked after: MKT-3", false},
		{"no line", false},
	} {
		if got := namesAfter(tc.body, "MKT-3"); got != tc.want {
			t.Errorf("namesAfter(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}
