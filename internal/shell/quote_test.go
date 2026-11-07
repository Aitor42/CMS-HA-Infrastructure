package shell

import "testing"

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"simple":      "'simple'",
		"with space":  "'with space'",
		"it's":        `'it'\''s'`,
		"":            "''",
		"a;rm -rf /":  "'a;rm -rf /'",
		"$(touch /x)": "'$(touch /x)'",
		"line\nbreak": "'line\nbreak'",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}
