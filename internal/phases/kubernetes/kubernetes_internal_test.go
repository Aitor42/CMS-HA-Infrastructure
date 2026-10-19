package kubernetes

import "testing"

func TestCountNonEmptyLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{
			name: "no members at all",
			in:   "",
			want: 0,
		},
		{
			name: "only whitespace and newlines",
			in:   "\n\n   \n\t\n",
			want: 0,
		},
		{
			name: "one member with trailing newline",
			in:   "node1 Ready control-plane,etcd,master 5m v1.29.15+k3s1\n",
			want: 1,
		},
		{
			name: "three members",
			in:   "a\nb\nc\n",
			want: 3,
		},
		{
			name: "no trailing newline",
			in:   "a\nb",
			want: 2,
		},
		{
			name: "blank lines between members are not members",
			in:   "a\n\nb\n",
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countNonEmptyLines(tt.in); got != tt.want {
				t.Errorf("countNonEmptyLines(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
