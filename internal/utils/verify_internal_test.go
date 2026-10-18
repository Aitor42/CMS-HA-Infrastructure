package utils

import "testing"

func TestAllNodesReady(t *testing.T) {
	tests := []struct {
		name     string
		nodesOut string
		want     bool
	}{
		{
			name:     "all ready",
			nodesOut: "NAME   STATUS   ROLES\nm1     Ready    control-plane\nm2     Ready    control-plane\nm3     Ready    worker\n",
			want:     true,
		},
		{
			name:     "one notready",
			nodesOut: "NAME   STATUS     ROLES\nm1     Ready      control-plane\nm2     NotReady   control-plane\nm3     Ready      worker\n",
			want:     false,
		},
		{
			name:     "all notready must not pass",
			nodesOut: "NAME   STATUS     ROLES\nm1     NotReady   control-plane\nm2     NotReady   control-plane\n",
			want:     false,
		},
		{
			name:     "empty output",
			nodesOut: "",
			want:     false,
		},
		{
			name:     "header only",
			nodesOut: "NAME   STATUS   ROLES   AGE   VERSION\n",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allNodesReady(tt.nodesOut); got != tt.want {
				t.Errorf("allNodesReady(%q) = %v, want %v", tt.nodesOut, got, tt.want)
			}
		})
	}
}
