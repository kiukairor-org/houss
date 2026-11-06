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
		if name == "index.html" {
			b = withBrowserAgent(b, browserAgent)
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

// Icon files are versioned by name (and ?v= in the references) because they are cached for a day.
var iconSpecs = map[string]iconSpec{
	"favicon-32.png":        {32, false},
	"apple-touch-180.png":   {180, false},
	"icon-192.png":          {192, false},
	"icon-512.png":          {512, false},
	"icon-maskable-512.png": {512, true},
}

func serveIcon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("size")
	if name == "icon.svg" {
		b, err := webFS.ReadFile("web/icons/icon.svg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
		return
	}
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

// Palette colours (web/style.css) the icon is drawn with; web/icons/icon.svg uses the same ones.
var (
	iconDeep  = color.NRGBA{0x35, 0x06, 0x3e, 0xff} // --purple-900
	iconMid   = color.NRGBA{0x9a, 0x5a, 0xa9, 0xff} // --purple-400
	iconEdge  = color.NRGBA{0xc9, 0xa3, 0xd4, 0xff} // --purple-300
	iconLight = color.NRGBA{0xef, 0xe3, 0xf3, 0xff} // --purple-100
)

// inRR reports whether (x,y) is inside the rounded rectangle [x0,x1]x[y0,y1] with corner radius r.
func inRR(x, y, x0, y0, x1, y1, r float64) bool {
	cx, cy := (x0+x1)/2, (y0+y1)/2
	qx, qy := math.Abs(x-cx)-((x1-x0)/2-r), math.Abs(y-cy)-((y1-y0)/2-r)
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) <= r
}

// iconAt returns the colour of the "raised card with a tick" mark at (x,y) in the unit square, or
// ok=false outside it. The mark stays inside the central 60% so the full-bleed maskable variant
// survives any mask.
func iconAt(x, y float64) (c color.NRGBA, ok bool) {
	switch {
	case distSeg(x, y, 0.359, 0.480, 0.457, 0.580) <= 0.035 || distSeg(x, y, 0.457, 0.580, 0.648, 0.391) <= 0.035:
		return iconMid, true
	case inRR(x, y, 0.25, 0.22, 0.75, 0.74, 0.08):
		return iconLight, true
	case inRR(x, y, 0.25, 0.265, 0.75, 0.785, 0.08):
		return iconEdge, true
	case inRR(x, y, 0.31, 0.17, 0.69, 0.40, 0.06):
		return iconMid, true
	}
	return c, false
}

// renderIcon draws a raised card with a tick (echoing the app's raised list items) on deep purple.
// The regular variant has rounded transparent corners; the maskable one is full-bleed.
func renderIcon(n int, maskable bool) []byte {
	const ss = 3
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	f := float64(n * ss)
	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					u, v := (float64(px*ss+sx)+0.5)/f, (float64(py*ss+sy)+0.5)/f
					if !maskable && !inRR(u, v, 0, 0, 1, 1, 0.22) {
						continue
					}
					c := iconDeep
					if m, ok := iconAt(u, v); ok {
						c = m
					}
					r, g, b, a = r+float64(c.R), g+float64(c.G), b+float64(c.B), a+1
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
