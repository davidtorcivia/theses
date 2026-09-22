package server

import (
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/boombuler/barcode/qr"
)

func (s *Server) legalQRPage(w http.ResponseWriter, r *http.Request) {
	release, err := s.api.Legal.Public(r.Context(), r.PathValue("token"))
	if err != nil {
		s.legalError(w, r, err)
		return
	}
	s.render(w, r, 200, "legal_qr.html", s.page(r, "Recording release QR", map[string]any{"Release": release}))
}
func (s *Server) legalQR(w http.ResponseWriter, r *http.Request) {
	release, err := s.api.Legal.Public(r.Context(), r.PathValue("token"))
	if err != nil {
		s.legalError(w, r, err)
		return
	}
	svg, err := releaseQR(s.cfg.BaseURL+"/legal/"+release.Token, release.Brand)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="recording-release-qr.svg"`)
	}
	w.Write([]byte(svg))
}
func releaseQR(url, brand string) (string, error) {
	code, err := qr.Encode(url, qr.H, qr.Auto)
	if err != nil {
		return "", err
	}
	chars := []rune(brand)
	lines := wrapBrand(chars, 24)
	qrY := max(190, 150+28*len(lines))
	height := qrY + 480
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="%d" viewBox="0 0 600 %d"><rect width="600" height="%d" fill="#f0f0e8"/><rect x="24" y="24" width="552" height="%d" rx="3" fill="#fffef8" stroke="#d5d8cc"/><rect x="48" y="48" width="44" height="44" fill="#d9fa69"/><path d="M59 59l22 22m-15 0h15V66" fill="none" stroke="#171b19" stroke-width="3"/>`, height*2, height, height, height-48)
	for i, line := range lines {
		// Fit wide letters without relying on the viewer's installed font metrics.
		fit := ""
		if len([]rune(line)) > 16 {
			fit = ` textLength="504" lengthAdjust="spacingAndGlyphs"`
		}
		fmt.Fprintf(&out, `<text x="48" y="%d" font-family="Arial,sans-serif" font-size="30" font-weight="700" fill="#171b19"%s>%s</text>`, 132+i*28, fit, html.EscapeString(line))
	}
	n := code.Bounds().Dx()
	scale := 440.0 / float64(n+8)
	fmt.Fprintf(&out, `<rect x="80" y="%d" width="440" height="440" fill="#fff"/><g transform="translate(80 %d) scale(%f)" fill="#000" shape-rendering="crispEdges">`, qrY, qrY, scale)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			red, _, _, _ := code.At(x, y).RGBA()
			if red == 0 {
				fmt.Fprintf(&out, `<rect x="%d" y="%d" width="1" height="1"/>`, x+4, y+4)
			}
		}
	}
	out.WriteString(`</g></svg>`)
	return out.String(), nil
}
func wrapBrand(chars []rune, width int) []string {
	out := []string{}
	for len(chars) > 0 {
		n := min(width, len(chars))
		if n < len(chars) {
			for i := n - 1; i > width/2; i-- {
				if chars[i] == ' ' {
					n = i
					break
				}
			}
		}
		out = append(out, string(chars[:n]))
		chars = []rune(strings.TrimSpace(string(chars[n:])))
	}
	return out
}
