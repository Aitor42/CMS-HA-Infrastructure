package utils

import (
	"strings"
	"testing"
)

func TestReorderBootElements(t *testing.T) {
	const networkFirst = `  <os>
    <type arch='x86_64'>hvm</type>
    <boot dev='network'/>
    <boot dev='hd'/>
  </os>`
	const diskFirst = `  <os>
    <type arch='x86_64'>hvm</type>
    <boot dev='hd'/>
    <boot dev='network'/>
  </os>`

	tests := []struct {
		name        string
		xml         string
		wantChanged bool
	}{
		{name: "network first is reordered", xml: networkFirst, wantChanged: true},
		{name: "disk first is left alone", xml: diskFirst, wantChanged: false},
		{name: "only a disk entry", xml: "<os><boot dev='hd'/></os>", wantChanged: false},
		{name: "no boot entries", xml: "<os><type>hvm</type></os>", wantChanged: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := reorderBootElements(tt.xml)
			if changed != tt.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if !changed {
				if got != tt.xml {
					t.Fatalf("XML was rewritten though changed=false:\n%s", got)
				}
				return
			}

			diskAt := strings.Index(got, "<boot dev='hd'/>")
			networkAt := strings.Index(got, "<boot dev='network'/>")
			if diskAt < 0 || networkAt < 0 || diskAt > networkAt {
				t.Fatalf("expected the disk before the network, got:\n%s", got)
			}
			if strings.Count(got, "<boot dev='hd'/>") != 1 ||
				strings.Count(got, "<boot dev='network'/>") != 1 {
				t.Fatalf("boot entries must neither be duplicated nor lost:\n%s", got)
			}
		})
	}
}

func TestReorderBootElementsIsIdempotent(t *testing.T) {
	const networkFirst = "<os><boot dev='network'/><boot dev='hd'/></os>"

	once, changed := reorderBootElements(networkFirst)
	if !changed {
		t.Fatal("expected the first pass to reorder the boot entries")
	}
	twice, changedAgain := reorderBootElements(once)
	if changedAgain {
		t.Fatalf("second pass should be a no-op, got:\n%s", twice)
	}
	if twice != once {
		t.Fatalf("second pass rewrote the XML:\n%s\n%s", once, twice)
	}
}
