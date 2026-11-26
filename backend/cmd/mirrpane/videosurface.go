package main

import (
	"image"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/shader"
)

// videoColorEffect mixes saturation using Rec.709 luma weights, adjusts
// contrast around mid grey, then adds brightness.
var videoColorEffect = shader.MustCompile(`package main

var Brightness float
var Contrast float
var Saturation float

func ColorEffect(pos vec2, color vec4) vec4 {
	c := color.rgb
	l := c.r*0.2126 + c.g*0.7152 + c.b*0.0722
	s := vec3(l) + (c-vec3(l))*Saturation
	s = (s-vec3(0.5))*Contrast + vec3(0.5) + vec3(Brightness)
	lo := max(s, vec3(0))
	hi := min(lo, vec3(1))
	return vec4(hi, color.a)
}
`)

func min32(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}

func max32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

// sharpSampling selects nearest-neighbour sampling for enlarged pixels.
func sharpSampling(filterQuality string) bool { return filterQuality == "none" }

// samplingChoices uses the existing preference keys. Saved low, medium and
// high values all select smooth interpolation.
var samplingChoices = []string{"銳利（像素）", "平滑（插值）"}

// samplingChoice maps a stored filterQuality, including legacy values, onto
// the mode the settings offer.
func samplingChoice(filterQuality string) string {
	if sharpSampling(filterQuality) {
		return samplingChoices[0]
	}
	return samplingChoices[1]
}

// filterQualityForChoice maps a settings choice back to the stored value. It
// writes "medium" for smooth so older builds read a valid value.
func filterQualityForChoice(choice string) string {
	if choice == samplingChoices[0] {
		return "none"
	}
	return "medium"
}

// opaqueBlank returns the picture the video surface shows when no session is
// live: one opaque black pixel, so nothing of the previous sender shows.
func opaqueBlank() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Pix[3] = 255
	return img
}

// presentVideoFrame shows img in the video surface, as the frame pump calls it
// on the UI thread. The pump adopts a picture and presents it in the same
// callback, so the bitmap points at the slot the frame source still holds;
// nothing can paint in between to read it half-adopted.
func (a *mirrpaneApp) presentVideoFrame(img *image.RGBA) {
	if img == nil || a.bitmap == nil {
		return
	}
	a.videoPicture = img
	if a.applySamplingMode() {
		// The bitmap was rebuilt from this very picture: showing it again
		// would only upload the same pixels twice.
		return
	}
	a.bitmap.Update(img)
}

// applySamplingMode makes the video surface's bitmap sample the way the
// preferences ask, and reports whether it replaced the bitmap. ui.Bitmap
// .Pixelated is one-way for the life of a bitmap, so switching back to smooth
// sampling rebuilds it from the picture still held by the frame source: the
// new bitmap shares those pixels, so the switch costs one texture upload and
// never a copy, and no frame goes blank in between.
//
// It runs on the UI thread, from presentVideoFrame and from the build.
func (a *mirrpaneApp) applySamplingMode() bool {
	sharp := sharpSampling(a.prefs.FilterQuality)
	if sharp == a.videoSharp || a.bitmap == nil {
		return false
	}
	if a.videoPicture == nil {
		// Before the first picture the surface shows a blank. Entering sharp
		// mode can mark that bitmap; leaving it needs a fresh one, since a
		// pixelated bitmap cannot go back to smoothing.
		a.videoSharp = sharp
		if sharp {
			a.bitmap.Pixelated()
			return false
		}
		a.bitmap = ui.NewBitmap(opaqueBlank())
		return true
	}
	next := ui.NewBitmap(a.videoPicture)
	if sharp {
		next.Pixelated()
	}
	a.bitmap = next
	a.videoSharp = sharp
	return true
}

// clearVideoPicture shows an opaque blank and forgets the picture, as when a
// session ends or the receiver restarts. Call it before the frame source is
// closed: it stops the surface from reading slot memory that is about to be
// unmapped, and stops a later sampling switch from reviving the last frame.
func (a *mirrpaneApp) clearVideoPicture() {
	a.videoPicture = nil
	a.bitmap = ui.NewBitmap(opaqueBlank())
	a.videoSharp = false
}

// buildVideoSurface lays the mirrored picture out for the current rotation,
// zoom mode and colour preferences. The fit (or cover) scale is computed here
// because a quarter turn swaps the fitted width and height; mygo's own Fit
// would fit the pre-rotation box. The picture is clipped to its display box.
//
// c.Size() is in DIPs, as the native layout is, so it is used as it is.
func (a *mirrpaneApp) buildVideoSurface(c *ui.Context) {
	a.applySamplingMode()
	availW, availH := c.Size()

	rawW, rawH := a.bitmap.Size()
	if rawW <= 0 || rawH <= 0 {
		rawW, rawH = 16, 9
	}
	turn := ((a.quarterTurns % 4) + 4) % 4
	effW, effH := float32(rawW), float32(rawH)
	if turn%2 == 1 {
		effW, effH = effH, effW
	}

	var fit float32
	if a.prefs.FillViewport {
		fit = max32(availW/effW, availH/effH)
	} else {
		fit = min32(availW/effW, availH/effH)
	}
	dispW, dispH := effW*fit, effH*fit

	// Place the picture with absolute insets: nested Fill/Center layouts did
	// not centre reliably in this mygo version, while Top/Left do.
	x := (availW - dispW) / 2
	y := (availH - dispH) / 2
	wrap := ui.Box(c).Absolute().Left(x).Top(y).Width(dispW).Height(dispH).Clip()
	wrap.Children(func() {
		imgW, imgH := dispW, dispH
		if turn%2 == 1 {
			imgW, imgH = dispH, dispW
		}
		img := ui.Image(c, a.bitmap).Absolute().
			Left((dispW - imgW) / 2).Top((dispH - imgH) / 2).
			Width(imgW).Height(imgH).Fit(ui.FillBox)
		if a.prefs.FlipHorizontal {
			img.Scale(-1, 1)
		}
		if turn != 0 {
			img.Rotate(float32(turn) * 90)
		}
		if !a.prefs.IsColorDefault() {
			img.Effect(videoColorEffect.Effect(shader.Options{Uniforms: map[string]any{
				"Brightness": a.prefs.Brightness,
				"Contrast":   a.prefs.Contrast,
				"Saturation": a.prefs.Saturation,
			}}))
		}
	})
}
