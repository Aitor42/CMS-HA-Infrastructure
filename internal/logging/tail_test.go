package logging

import "testing"

func TestTail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short output is unchanged", "one\ntwo\n", 10, "one\ntwo"},
		{"keeps only the last lines", "a\nb\nc\nd", 2, "c\nd"},
		{"strips trailing line breaks", "a\nb\n\r\n", 10, "a\nb"},
		{"non-positive max is empty", "a\nb", 0, ""},
		{"single line", "only", 10, "only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Tail(tc.in, tc.max); got != tc.want {
				t.Errorf("Tail(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}
