// Package assess estimates an opportunity's commercial potential
// (docs/03 §2.2): the model fills ranged factors with citations, code clamps
// unsupported factors to wide priors, runs a Monte Carlo over them and finds
// the weakest link. No single-point forecasts.
package assess

import (
	"math"
	"math/rand/v2"
	"sort"
)

// Range is a (low, base, high) estimate.
type Range struct {
	Low  float64 `json:"low"`
	Base float64 `json:"base"`
	High float64 `json:"high"`
}

// Valid reports whether low ≤ base ≤ high and all are finite.
func (r Range) Valid() bool {
	return !math.IsNaN(r.Low+r.Base+r.High) && !math.IsInf(r.Low+r.Base+r.High, 0) && r.Low <= r.Base && r.Base <= r.High
}

// Factor codes (docs/03 §2.2). The first group drives the revenue model.
const (
	NReachable   = "N_reachable"   // organisations reachable through the named channel in 12 months
	Price        = "price"         // USD per customer per month
	PProblem     = "p_problem"     // P(the problem is as described for this segment)
	PPay         = "p_pay"         // P(a qualified prospect pays at price)
	PReach       = "p_reach"       // P(a listed prospect becomes a qualified conversation in 12 months)
	PBuild       = "p_build"       // P(the critical workflow can be delivered at acceptable quality and cost)
	Retention    = "retention_12m" // fraction of customers retained at 12 months
	UnitCost     = "unit_cost"     // USD per customer per month
	FounderFit   = "founder_fit"   // multiplier 0.5–2.0 on reach
	TimeToDollar = "time_to_first_dollar"
	Defensible   = "defensibility_24m"
)

// Units of every factor.
var Units = map[string]string{
	NReachable: "orgs", Price: "usd_month", PProblem: "prob", PPay: "prob", PReach: "prob", PBuild: "prob",
	Retention: "fraction", UnitCost: "usd_customer_month", FounderFit: "multiplier", TimeToDollar: "weeks", Defensible: "ordinal_1_5",
}

// RevenueFactors are the factors the revenue model uses.
var RevenueFactors = []string{NReachable, Price, PProblem, PPay, PReach, PBuild, Retention, FounderFit}

// widePriors are what a probability-like factor becomes without evidence
// (OPP-5): the model can't narrow a range it can't cite.
var widePriors = map[string]Range{
	PProblem:   {0.2, 0.5, 0.8},
	PPay:       {0.01, 0.05, 0.2},
	PReach:     {0.02, 0.1, 0.3},
	PBuild:     {0.3, 0.6, 0.9},
	Retention:  {0.3, 0.6, 0.85},
	FounderFit: {0.6, 1.0, 1.5},
	Defensible: {1, 2, 4},
}

// bounds keep every factor in a sane domain.
var bounds = map[string][2]float64{
	PProblem: {0, 1}, PPay: {0, 1}, PReach: {0, 1}, PBuild: {0, 1}, Retention: {0, 1},
	FounderFit: {0.5, 2}, Defensible: {1, 5}, NReachable: {0, 1e7}, Price: {0, 1e6}, UnitCost: {0, 1e6}, TimeToDollar: {0, 520},
}

// Clamp returns the range bounded to the factor's domain and, when the factor
// has no supporting evidence, widened to the factor's prior: fixed wide
// priors for probabilities, ×3 either side of the base for magnitudes.
func Clamp(code string, r Range, evidenced bool) (Range, bool) {
	b, ok := bounds[code]
	if ok {
		r.Low, r.Base, r.High = math.Max(b[0], math.Min(b[1], r.Low)), math.Max(b[0], math.Min(b[1], r.Base)), math.Max(b[0], math.Min(b[1], r.High))
	}
	vals := []float64{r.Low, r.Base, r.High}
	sort.Float64s(vals)
	r = Range{vals[0], vals[1], vals[2]}
	if evidenced {
		return r, false
	}
	if p, ok := widePriors[code]; ok {
		return p, true
	}
	widened := Range{Low: math.Min(r.Low, r.Base/3), Base: r.Base, High: math.Max(r.High, r.Base*3)}
	if ok {
		widened.High = math.Min(widened.High, b[1])
	}
	return widened, true
}

// Point is one value per factor.
type Point map[string]float64

// MRR is the 12-month MRR model: reachable organisations × P(conversation) ×
// founder fit (capped at certainty) × P(problem real) × P(pay), times price,
// times average retention over the year ((1 + r12)/2, customers arrive evenly).
func MRR(p Point) float64 {
	reach := math.Min(1, p[PReach]*p[FounderFit])
	customers := p[NReachable] * reach * p[PProblem] * p[PPay]
	return customers * p[Price] * (1 + p[Retention]) / 2
}

// Result is the simulation output.
type Result struct {
	P10, P50, P90        float64
	PGe500, PGe1k, PGe5k float64
	Weakest              string
	Swing                map[string]float64 // expected-MRR swing from low to high, per factor
}

// triangular samples a triangular distribution.
func triangular(rng *rand.Rand, r Range) float64 {
	if r.High <= r.Low {
		return r.Base
	}
	u := rng.Float64()
	c := (r.Base - r.Low) / (r.High - r.Low)
	if u < c {
		return r.Low + math.Sqrt(u*(r.High-r.Low)*(r.Base-r.Low))
	}
	return r.High - math.Sqrt((1-u)*(r.High-r.Low)*(r.High-r.Base))
}

// Simulate runs n draws, seeded for reproducibility. p_build is a gate: with
// probability 1−p_build the product can't be delivered and MRR is zero.
func Simulate(factors map[string]Range, n int, seed uint64) Result {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	draws := make([]float64, n)
	ge500, ge1, ge5 := 0, 0, 0
	for i := range draws {
		p := Point{}
		for _, code := range RevenueFactors {
			p[code] = triangular(rng, factors[code])
		}
		v := 0.0
		if rng.Float64() < p[PBuild] {
			v = MRR(p)
		}
		draws[i] = v
		if v >= 500 {
			ge500++
		}
		if v >= 1000 {
			ge1++
		}
		if v >= 5000 {
			ge5++
		}
	}
	sort.Float64s(draws)
	q := func(f float64) float64 { return draws[int(f*float64(n-1))] }
	res := Result{P10: q(0.1), P50: q(0.5), P90: q(0.9), PGe500: float64(ge500) / float64(n), PGe1k: float64(ge1) / float64(n), PGe5k: float64(ge5) / float64(n),
		Swing: map[string]float64{}}

	// Weakest link: one-at-a-time sensitivity of expected MRR (p_build as a
	// multiplier), the factor whose range moves the outcome most.
	expected := func(p Point) float64 { return MRR(p) * p[PBuild] }
	base := Point{}
	for _, code := range RevenueFactors {
		base[code] = factors[code].Base
	}
	best := -1.0
	for _, code := range RevenueFactors {
		lo, hi := Point{}, Point{}
		for k, v := range base {
			lo[k], hi[k] = v, v
		}
		lo[code], hi[code] = factors[code].Low, factors[code].High
		swing := math.Abs(expected(hi) - expected(lo))
		res.Swing[code] = swing
		if swing > best {
			best, res.Weakest = swing, code
		}
	}
	return res
}
