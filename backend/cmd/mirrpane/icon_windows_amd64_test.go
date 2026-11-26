package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// MyGo loads icon group 1 from the executable. Check its actual resources so
// a missing or stale .syso cannot silently ship a different icon from the ICO.
func TestEmbeddedAppIcon(t *testing.T) {
	ico, err := os.ReadFile(filepath.Join("..", "..", "..", "assets", "mirrpane.ico"))
	if err != nil {
		t.Fatal(err)
	}
	group := iconResource(t, windows.RT_GROUP_ICON, 1)
	if len(ico) < 6 || len(group) < 6 || !bytes.Equal(ico[:6], group[:6]) {
		t.Fatal("embedded icon group differs from the ICO header")
	}
	le := binary.LittleEndian
	count := int(le.Uint16(ico[4:6]))
	if count == 0 || len(ico) < 6+16*count || len(group) != 6+14*count {
		t.Fatal("invalid icon directory")
	}
	sizes := map[int]bool{}
	for i := 0; i < count; i++ {
		fileEntry := ico[6+16*i : 6+16*(i+1)]
		resourceEntry := group[6+14*i : 6+14*(i+1)]
		if !bytes.Equal(fileEntry[:12], resourceEntry[:12]) || fileEntry[0] != fileEntry[1] {
			t.Fatalf("icon entry %d has different dimensions or format", i)
		}
		size := int(fileEntry[0])
		if size == 0 {
			size = 256
		}
		sizes[size] = true
		start, length := int(le.Uint32(fileEntry[12:])), int(le.Uint32(fileEntry[8:12]))
		id := windows.ResourceID(le.Uint16(resourceEntry[12:]))
		if start+length > len(ico) || !bytes.Equal(iconResource(t, windows.RT_ICON, id), ico[start:start+length]) {
			t.Fatalf("embedded %d px icon differs from the ICO", size)
		}
	}
	for _, size := range []int{16, 20, 24, 32, 40, 48, 64, 256} {
		if !sizes[size] {
			t.Errorf("missing %d px icon", size)
		}
	}
}

func iconResource(t *testing.T, kind, id windows.ResourceID) []byte {
	t.Helper()
	info, err := windows.FindResource(0, id, kind)
	if err != nil {
		t.Fatalf("find resource type %d ID %d: %v", kind, id, err)
	}
	data, err := windows.LoadResourceData(0, info)
	if err != nil {
		t.Fatalf("load resource type %d ID %d: %v", kind, id, err)
	}
	return data
}
