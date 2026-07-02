// Generates the tapotray tray icon: a 64x64 PNG of a power symbol.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

const (
	size = 64
	ss   = 4 // supersampling factor for anti-aliasing
)

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// inShape reports whether the point (in image coordinates) is inside the
// power symbol: a ring with a gap at the top, crossed by a vertical bar.
func inShape(x, y float64) bool {
	dx := x - float64(size)/2
	dy := float64(size)/2 - y // y axis pointing up
	r := math.Hypot(dx, dy)
	ang := math.Atan2(dx, dy) // 0 = straight up
	// ring of radius 17..24 with a gap of +-0.5 rad around the top
	if r >= 17 && r <= 24 && math.Abs(ang) > 0.5 {
		return true
	}
	// vertical bar: capsule of radius 3.5 from (0,7) to (0,27)
	by := clamp(dy, 7, 27)
	return math.Hypot(dx, dy-by) <= 3.5
}

func main() {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	col := color.NRGBA{R: 0x23, G: 0xa8, B: 0xe0} // Tapo-ish light blue
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			hits := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					fx := float64(x) + (float64(sx)+0.5)/ss
					fy := float64(y) + (float64(sy)+0.5)/ss
					if inShape(fx, fy) {
						hits++
					}
				}
			}
			if hits > 0 {
				c := col
				c.A = uint8(hits * 255 / (ss * ss))
				img.SetNRGBA(x, y, c)
			}
		}
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}
