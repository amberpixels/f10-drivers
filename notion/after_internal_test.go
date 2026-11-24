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

func TestAfterEdit(t *testing.T) {
	for _, tc := range []struct {
		name, body, id, from, to string
		ok                       bool
	}{
		{"empty body", "", "MKT-3", "", "After: MKT-3", true},
		{"append", "a\n\nb\n", "MKT-3", "b", "b\n\nAfter: MKT-3", true},
		{"append past a repeat", "x\n\ny\n\nx", "MKT-3", "y\n\nx", "y\n\nx\n\nAfter: MKT-3", true},
		{"rewrite", "a\n\nAfter: MKT-1", "MKT-3", "a\n\nAfter: MKT-1", "a\n\nAfter: MKT-3", true},
		{"rewrite a link", "a\n\nAfter: [MKT-1: T](https://x)", "MKT-3", "a\n\nAfter: [MKT-1: T](https://x)", "a\n\nAfter: MKT-3", true},
		{"drop", "a\n\nAfter: MKT-1\n", "", "a\n\nAfter: MKT-1", "a", true},
		{"drop the only line", "After: MKT-1", "", "After: MKT-1", "", true},
		{"same id", "a\n\nafter: mkt-3", "MKT-3", "", "", false},
		{"nothing to drop", "a\n\nAfter: MKT-1 is not last\n\nb", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, to, ok := afterEdit(tc.body, tc.id)
			if from != tc.from || to != tc.to || ok != tc.ok {
				t.Fatalf("afterEdit(%q, %q) = %q, %q, %v; want %q, %q, %v",
					tc.body, tc.id, from, to, ok, tc.from, tc.to, tc.ok)
			}
		})
	}
}
