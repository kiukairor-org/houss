package main

import (
	"bytes"
	"embed"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"sync"
)

//go:embed web
var webFS embed.FS

// Public static files (the app shell holds no data; the API is behind the session cookie).
var staticFiles = map[string]string{
	"/app.js":               "text/javascript; charset=utf-8",
	"/style.css":            "text/css; charset=utf-8",
	"/sw.js":                "text/javascript; charset=utf-8",
	"/manifest.webmanifest": "application/manifest+json",
}

func serveEmbedded(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := webFS.ReadFile("web/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache") // the service worker owns offline caching
		_, _ = w.Write(b)
	}
}

func registerStatic(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", serveEmbedded("index.html", "text/html; charset=utf-8"))
	for path, ctype := range staticFiles {
		mux.HandleFunc("GET "+path, serveEmbedded(path[1:], ctype))
	}
	mux.HandleFunc("GET /icons/{size}", serveIcon)
}

var (
	iconMu    sync.Mutex
	iconCache = map[string][]byte{}
)

type iconSpec struct {
	size     int
	maskable bool
}

var iconSpecs = map[string]iconSpec{
	"180.png":          {180, false},
	"192.png":          {192, false},
	"512.png":          {512, false},
	"maskable-512.png": {512, true},
}

func serveIcon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("size")
	spec, ok := iconSpecs[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	iconMu.Lock()
	b, ok := iconCache[name]
	if !ok {
		b = renderIcon(spec.size, spec.maskable)
		iconCache[name] = b
	}
	iconMu.Unlock()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

// distSeg is the distance from (px,py) to the segment a-b.
func distSeg(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := math.Max(0, math.Min(1, ((px-ax)*dx+(py-ay)*dy)/(dx*dx+dy*dy)))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}

// markAt reports whether the centred point (x,y) (unit square, origin in the middle) lies on the
// house, and whether it lies on the check mark cut out of the house body.
func markAt(x, y float64) (house, check bool) {
	roof := y >= -0.30 && y <= -0.02 && math.Abs(x) <= 0.32*(y+0.30)/0.28
	body := y > -0.02 && y <= 0.26 && math.Abs(x) <= 0.24
	if !roof && !body {
		return false, false
	}
	check = distSeg(x, y, -0.11, 0.12, -0.03, 0.20) < 0.035 || distSeg(x, y, -0.03, 0.20, 0.12, 0.04) < 0.035
	return true, check
}

// renderIcon draws a light house with a check mark on a purple gradient. The mark stays inside the
// central 60% so the full-bleed maskable variant survives any mask; the regular variant has
// rounded transparent corners and a slightly larger mark.
func renderIcon(n int, maskable bool) []byte {
	const ss = 3
	top := [3]float64{0x7a, 0x2f, 0x9c}
	bot := [3]float64{0x35, 0x06, 0x3e}
	light := [3]float64{0xec, 0xe3, 0xf4}
	scale := 1.0
	if !maskable {
		scale = 1.12
	}
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	f := float64(n * ss)
	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					u, v := (float64(px*ss+sx)+0.5)/f, (float64(py*ss+sy)+0.5)/f
					if !maskable {
						// rounded-square coverage, corner radius 0.22
						cx, cy := math.Max(math.Abs(u-0.5)-0.28, 0), math.Max(math.Abs(v-0.5)-0.28, 0)
						if math.Hypot(cx, cy) > 0.22 {
							continue
						}
					}
					t := (u + v) / 2 // diagonal gradient
					c := [3]float64{top[0] + (bot[0]-top[0])*t, top[1] + (bot[1]-top[1])*t, top[2] + (bot[2]-top[2])*t}
					if house, check := markAt((u-0.5)/scale, (v-0.5)/scale); house && !check {
						c = light
					}
					r, g, b, a = r+c[0], g+c[1], b+c[2], a+1
				}
			}
			if a > 0 {
				img.SetNRGBA(px, py, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(255 * a / (ss * ss))})
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
