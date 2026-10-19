package utils

import "testing"

func TestCountReadyNodes(t *testing.T) {
	tests := []struct {
		name     string
		nodesOut string
		want     int
	}{
		{
			name:     "all ready",
			nodesOut: "NAME   STATUS   ROLES\nm1     Ready    control-plane\nm2     Ready    control-plane\nm3     Ready    worker\n",
			want:     3,
		},
		{
			name:     "notready is never counted",
			nodesOut: "NAME   STATUS     ROLES\nm1     Ready      control-plane\nm2     NotReady   control-plane\nm3     Ready      worker\n",
			want:     2,
		},
		{
			name:     "all notready counts zero",
			nodesOut: "NAME   STATUS     ROLES\nm1     NotReady   control-plane\nm2     NotReady   control-plane\n",
			want:     0,
		},
		{
			name:     "empty output",
			nodesOut: "",
			want:     0,
		},
		{
			name:     "header only",
			nodesOut: "NAME   STATUS   ROLES   AGE   VERSION\n",
			want:     0,
		},
		{
			name:     "cordoned node still counts as ready",
			nodesOut: "NAME   STATUS                     ROLES\nw1     Ready,SchedulingDisabled   worker\n",
			want:     1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countReadyNodes(tt.nodesOut); got != tt.want {
				t.Errorf("countReadyNodes(%q) = %d, want %d", tt.nodesOut, got, tt.want)
			}
		})
	}
}
