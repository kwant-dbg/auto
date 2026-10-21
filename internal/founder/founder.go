// Package founder loads the founder profile (PRD §4, config/founder.yaml):
// the constraints and strengths that founder gates and ranking use.
package founder

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Profile is config/founder.yaml.
type Profile struct {
	Founder struct {
		WeeklyReviewMinutes   int            `yaml:"weekly_review_minutes"`
		WeeklyValidationHours int            `yaml:"weekly_validation_hours"`
		DomainsWithAccess     map[string]int `yaml:"domains_with_access"`
		ExcludedDomains       []string       `yaml:"excluded_domains"`
		EmploymentConstraints struct {
			ConflictCheckRequired bool     `yaml:"conflict_check_required"`
			ExcludedOverlap       []string `yaml:"excluded_overlap"`
		} `yaml:"employment_constraints"`
		SoloOperability struct {
			MaxSalesCycleDays         int  `yaml:"max_sales_cycle_days"`
			Allow24x7SLA              bool `yaml:"allow_24x7_sla"`
			AllowPreSaleSecurityAudit bool `yaml:"allow_pre_sale_security_audit"`
		} `yaml:"solo_operability"`
		ProductScope struct {
			AllowHorizontalTools bool `yaml:"allow_horizontal_tools"`
		} `yaml:"product_scope"`
	} `yaml:"founder"`
	CommercialTargets struct {
		MilestonesMRR      []int `yaml:"milestones_mrr"`
		PreferredPriceBand []int `yaml:"preferred_price_band_usd_month"`
		HorizonMonths      int   `yaml:"horizon_months"`
	} `yaml:"commercial_targets"`
}

// Load reads a profile.
func Load(path string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// Summary renders the profile for prompts. It holds no personal data beyond
// what the founder put in the profile.
func (p *Profile) Summary() string {
	var b strings.Builder
	f := p.Founder
	if len(f.DomainsWithAccess) > 0 {
		names := make([]string, 0, len(f.DomainsWithAccess))
		for d := range f.DomainsWithAccess {
			names = append(names, d)
		}
		sort.Strings(names)
		b.WriteString("Domains where the founder has access or credibility (0–3): ")
		for i, d := range names {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%d", strings.ReplaceAll(d, "_", " "), f.DomainsWithAccess[d])
		}
		b.WriteString(".\n")
	}
	if len(f.ExcludedDomains) > 0 {
		fmt.Fprintf(&b, "Excluded domains (regulated practice the founder won't enter): %s.\n", strings.ReplaceAll(strings.Join(f.ExcludedDomains, ", "), "_", " "))
	}
	so := f.SoloOperability
	fmt.Fprintf(&b, "Solo operator: sales cycle for the first 10 customers at most %d days; 24/7 SLA allowed: %v; pre-sale security audit allowed: %v.\n",
		so.MaxSalesCycleDays, so.Allow24x7SLA, so.AllowPreSaleSecurityAudit)
	if f.ProductScope.AllowHorizontalTools {
		b.WriteString("General-purpose (horizontal) tools are in scope, as are vertical products, services and plugins.\n")
	}
	if band := p.CommercialTargets.PreferredPriceBand; len(band) == 2 {
		fmt.Fprintf(&b, "Preferred price band: $%d–$%d per month per customer.\n", band[0], band[1])
	}
	if m := p.CommercialTargets.MilestonesMRR; len(m) > 0 {
		fmt.Fprintf(&b, "Commercial milestones (MRR, USD): %v within %d months.\n", m, p.CommercialTargets.HorizonMonths)
	}
	return b.String()
}
