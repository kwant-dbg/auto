package assess

import (
	"math"
	"testing"
)

func baseFactors() map[string]Range {
	return map[string]Range{
		NReachable: {200, 400, 800}, Price: {100, 200, 400}, PProblem: {0.6, 0.8, 0.9}, PPay: {0.05, 0.1, 0.2},
		PReach: {0.1, 0.2, 0.4}, PBuild: {0.7, 0.85, 0.95}, Retention: {0.5, 0.7, 0.85}, FounderFit: {0.8, 1, 1.2},
	}
}

func TestMRRFormula(t *testing.T) {
	p := Point{NReachable: 1000, PReach: 0.1, FounderFit: 1, PProblem: 0.5, PPay: 0.2, Price: 300, Retention: 0.6}
	// 1000 × 0.1 × 0.5 × 0.2 = 10 customers; × $300 × (1+0.6)/2 = $2,400
	if got := MRR(p); math.Abs(got-2400) > 1e-9 {
		t.Fatalf("MRR = %v, want 2400", got)
	}
	p[FounderFit], p[PReach] = 2, 0.8 // reach is capped at certainty
	if got := MRR(p); math.Abs(got-24000) > 1e-6 {
		t.Fatalf("capped MRR = %v, want 24000", got)
	}
}

func TestSimulateIsReproducibleAndOrdered(t *testing.T) {
	f := baseFactors()
	a := Simulate(f, 5000, 42)
	b := Simulate(f, 5000, 42)
	if a.P50 != b.P50 || a.PGe1k != b.PGe1k {
		t.Fatal("same seed must give the same result")
	}
	if !(a.P10 <= a.P50 && a.P50 <= a.P90) || a.PGe5k > a.PGe1k {
		t.Fatalf("result out of order: %+v", a)
	}
	// Base case: 400 × 0.2 × 0.8 × 0.1 = 6.4 customers × $200 × 0.85 ≈ $1,088 before p_build.
	if a.P50 < 300 || a.P50 > 2500 {
		t.Fatalf("P50 = %v, outside the plausible band", a.P50)
	}
	if a.Weakest == "" || len(a.Swing) != len(RevenueFactors) {
		t.Fatalf("weakest link missing: %+v", a)
	}
}

func TestWeakestLinkIsTheWidestUncertainty(t *testing.T) {
	f := baseFactors()
	f[PPay] = Range{0.001, 0.1, 0.6} // very uncertain willingness to pay
	if w := Simulate(f, 2000, 1).Weakest; w != PPay {
		t.Fatalf("weakest = %s, want p_pay", w)
	}
}

func TestClamp(t *testing.T) {
	r, clamped := Clamp(PPay, Range{0.3, 0.4, 0.5}, false)
	if !clamped || r != widePriors[PPay] {
		t.Fatalf("unevidenced probability = %+v, want the wide prior", r)
	}
	r, clamped = Clamp(PPay, Range{0.3, 0.4, 0.5}, true)
	if clamped || r != (Range{0.3, 0.4, 0.5}) {
		t.Fatalf("evidenced probability changed: %+v", r)
	}
	r, _ = Clamp(Price, Range{90, 100, 110}, false)
	if r.Low > 34 || r.High < 300 {
		t.Fatalf("unevidenced magnitude not widened: %+v", r)
	}
	r, _ = Clamp(PProblem, Range{1.5, 0.2, -1}, true) // out of order and out of bounds
	if r != (Range{0, 0.2, 1}) {
		t.Fatalf("bounds/order not enforced: %+v", r)
	}
	if !(Range{1, 2, 3}).Valid() || (Range{3, 2, 1}).Valid() {
		t.Fatal("Valid")
	}
}
