package main

import "testing"

// androidMapPoint maps the pointer from window DIPs onto the device's pixels,
// undoing letterboxing, rotation and the mirror flip. Mirror a wrong mapping
// with a tap landing somewhere else on the phone.
func TestAndroidMapPoint(t *testing.T) {
	const w, h = float32(1280), float32(800)
	const dw, dh = float32(1080), float32(2400) // portrait phone, 9:20
	approx := func(got, want int) bool {
		d := got - want
		return d >= -1 && d <= 1
	}

	// Letterboxed portrait video is 360 DIPs wide, centred.
	if x, y, ok := androidMapPoint(640, 400, w, h, dw, dh, 0, false, false); !ok ||
		!approx(x, 540) || !approx(y, 1200) {
		t.Fatalf("centre = %d,%d ok=%v, want 540,1200", x, y, ok)
	}
	if x, y, _ := androidMapPoint(460, 400, w, h, dw, dh, 0, false, false); !approx(x, 0) || !approx(y, 1200) {
		t.Fatalf("left video edge = %d,%d, want 0,1200", x, y)
	}
	// A point in the black bars clamps to the nearest edge.
	if x, _, _ := androidMapPoint(100, 400, w, h, dw, dh, 0, false, false); x != 0 {
		t.Fatalf("bar point x=%d, want 0", x)
	}

	// A quarter turn draws the picture 1280×576, centred vertically.
	if x, y, _ := androidMapPoint(320, 400, w, h, dw, dh, 1, false, false); !approx(x, 540) || !approx(y, 1800) {
		t.Fatalf("quarter turn = %d,%d, want 540,1800", x, y)
	}

	// Mirroring flips the horizontal device coordinate.
	if x, y, _ := androidMapPoint(550, 400, w, h, dw, dh, 0, true, false); !approx(x, 810) || !approx(y, 1200) {
		t.Fatalf("flipped = %d,%d, want 810,1200", x, y)
	}

	// Fill crops instead of letterboxing: the centre stays the centre.
	if x, y, _ := androidMapPoint(640, 400, w, h, dw, dh, 0, false, true); !approx(x, 540) || !approx(y, 1200) {
		t.Fatalf("fill centre = %d,%d, want 540,1200", x, y)
	}

	if _, _, ok := androidMapPoint(10, 10, w, h, 0, 0, 0, false, false); ok {
		t.Fatal("mapping succeeded without a picture")
	}
}
