package mosaic

import "math"

// Lab è un colore nello spazio CIE L*a*b* (illuminante D65). Lo usiamo al posto
// di RGB perché la distanza euclidea in Lab approssima la differenza percepita
// dall'occhio: due tessere "vicine" in Lab sembrano davvero simili.
type Lab struct{ L, A, B float64 }

// srgbToLinear è una lookup table sRGB (8 bit) -> luce lineare. La media dei
// colori va fatta in spazio lineare, altrimenti le tessere risultano più scure
// del dovuto.
var srgbToLinear [256]float64

func init() {
	for i := range srgbToLinear {
		c := float64(i) / 255
		if c <= 0.04045 {
			srgbToLinear[i] = c / 12.92
		} else {
			srgbToLinear[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
}

func labF(t float64) float64 {
	const eps = 216.0 / 24389.0
	if t > eps {
		return math.Cbrt(t)
	}
	return (24389.0/27.0*t + 16) / 116
}

// LinearRGBToLab converte un colore già in luce lineare (canali 0..1) in Lab.
func LinearRGBToLab(r, g, b float64) Lab {
	x := (0.4124564*r + 0.3575761*g + 0.1804375*b) / 0.95047
	y := 0.2126729*r + 0.7151522*g + 0.0721750*b
	z := (0.0193339*r + 0.1191920*g + 0.9503041*b) / 1.08883

	fx, fy, fz := labF(x), labF(y), labF(z)
	return Lab{L: 116*fy - 16, A: 500 * (fx - fy), B: 200 * (fy - fz)}
}
