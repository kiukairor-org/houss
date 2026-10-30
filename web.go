package main

import (
	"bytes"
	"embed"
	"image"
	"image/color"
	"image/png"
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
	iconCache = map[int][]byte{}
)

var iconSizes = map[string]int{"180.png": 180, "192.png": 192, "512.png": 512}

func serveIcon(w http.ResponseWriter, r *http.Request) {
	size, ok := iconSizes[r.PathValue("size")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	iconMu.Lock()
	b, ok := iconCache[size]
	if !ok {
		b = renderIcon(size)
		iconCache[size] = b
	}
	iconMu.Unlock()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

// renderIcon draws a white house on a green square, full-bleed so it is safe as a maskable icon
// (the house stays inside the central 60%).
func renderIcon(n int) []byte {
	bg, fg := color.RGBA{0x2e, 0x7d, 0x32, 0xff}, color.RGBA{0xff, 0xff, 0xff, 0xff}
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	f := float64(n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			u, v := float64(x)/f, float64(y)/f
			c := bg
			// roof: triangle apex (0.5,0.22), base y=0.5 from x 0.2..0.8
			if v >= 0.22 && v <= 0.5 {
				half := 0.3 * (v - 0.22) / 0.28
				if u >= 0.5-half && u <= 0.5+half {
					c = fg
				}
			}
			// body, with a door cut-out
			if v > 0.5 && v <= 0.76 && u >= 0.3 && u <= 0.7 {
				c = fg
				if v > 0.6 && u >= 0.45 && u <= 0.55 {
					c = bg
				}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
