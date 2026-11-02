package calib

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestFitRecoversKnownModel(t *testing.T) {
	// Simulate a sensor: raw = growth(RH) * (ref - a)/b with noise.
	r := rand.New(rand.NewPCG(1, 2))
	truth := Model{A: 2, B: 1.5, Kappa: 0.3}
	var ps []Pair
	for i := 0; i < 500; i++ {
		ref := 2 + r.Float64()*40
		rh := 40 + r.Float64()*55
		raw := (ref - truth.A) / truth.B * Growth(rh, truth.Kappa)
		raw *= 1 + 0.03*r.NormFloat64()
		ps = append(ps, Pair{Raw: raw, RH: rh, Ref: ref})
	}
	ps[3].Ref = 400 // one wild outlier must not move the fit
	k := FitKappa(map[int][]Pair{1: ps})
	m := Fit(1, ps, k)
	if math.Abs(k-0.3) > 0.06 || math.Abs(m.B-1.5) > 0.1 || math.Abs(m.A-2) > 0.6 {
		t.Fatalf("got κ=%.2f a=%.2f b=%.2f", k, m.A, m.B)
	}
}

func TestGrowthIsCapped(t *testing.T) {
	if g := Growth(100, 0.3); math.IsInf(g, 0) || g > 7 {
		t.Fatalf("growth at 100%% RH = %v", g)
	}
	if Growth(0, 0.3) != 1 {
		t.Fatal("no growth in dry air")
	}
}

func TestScore(t *testing.T) {
	m := Score([]float64{1, 2, 3}, []float64{1, 2, 4})
	if math.Abs(m.MAE-1.0/3) > 1e-9 || m.Bias >= 0 {
		t.Fatalf("%+v", m)
	}
}
