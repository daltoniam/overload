package catalog

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Policy restricts which models may review code. The zero value allows
// every model, so installs without a policy keep working.
type Policy struct {
	// AllowedOrigins, when set, are the only publisher countries allowed
	// (ISO 3166 codes). Models whose origin is not recorded are refused.
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	// BlockedOrigins are refused even if allowed.
	BlockedOrigins []string `json:"blocked_origins,omitempty"`
	// CheckLineage applies the origin rules to base models and to models
	// that generated training data, not only the publisher.
	CheckLineage bool `json:"check_lineage,omitempty"`
	// RequireCommercialUse refuses models whose licence does not allow
	// the organization's commercial use, or whose terms are not recorded.
	RequireCommercialUse bool `json:"require_commercial_use,omitempty"`
	// AnnualRevenueUSD is the organization's annual revenue, for licences
	// that are free below a revenue cap; 0 means not given.
	AnnualRevenueUSD int64 `json:"annual_revenue_usd,omitempty"`
	// LicenceReviewed lists catalog entries whose terms an administrator
	// has confirmed the organization may use, for example under a separate
	// licence. They pass the licence rule but not the origin rules.
	LicenceReviewed []string `json:"licence_reviewed,omitempty"`
	// LineageReviewed lists catalog entries whose base model an
	// administrator has confirmed, when the catalog does not record it and
	// lineage is checked against allowed origins.
	LineageReviewed []string `json:"lineage_reviewed,omitempty"`
	// RefuseUncataloged refuses model connections that match no catalog
	// entry, since nothing is known about where they come from.
	RefuseUncataloged bool `json:"refuse_uncataloged,omitempty"`
}

// Restrictive reports whether the policy refuses anything.
func (policy Policy) Restrictive() bool {
	return len(policy.AllowedOrigins) > 0 || len(policy.BlockedOrigins) > 0 || policy.RequireCommercialUse || policy.RefuseUncataloged
}

// Normalize sorts and deduplicates the policy's lists and upper-cases
// origins, so equal policies compare and store equally.
func (policy Policy) Normalize() Policy {
	clean := func(values []string, upper bool) []string {
		var out []string
		for _, value := range values {
			value = strings.TrimSpace(value)
			if upper {
				value = strings.ToUpper(value)
			}
			if value != "" && !slices.Contains(out, value) {
				out = append(out, value)
			}
		}
		slices.Sort(out)
		return out
	}
	policy.AllowedOrigins = clean(policy.AllowedOrigins, true)
	policy.BlockedOrigins = clean(policy.BlockedOrigins, true)
	policy.LicenceReviewed = clean(policy.LicenceReviewed, false)
	policy.LineageReviewed = clean(policy.LineageReviewed, false)
	return policy
}

// Validate checks a normalized policy.
func (policy Policy) Validate() error {
	for _, origin := range append(slices.Clone(policy.AllowedOrigins), policy.BlockedOrigins...) {
		if !originPattern.MatchString(origin) {
			return fmt.Errorf("%q is not a two-letter country code", origin)
		}
	}
	for _, origin := range policy.AllowedOrigins {
		if slices.Contains(policy.BlockedOrigins, origin) {
			return fmt.Errorf("%s is both allowed and blocked", origin)
		}
	}
	if policy.AnnualRevenueUSD < 0 {
		return errors.New("annual revenue cannot be negative")
	}
	for _, id := range append(slices.Clone(policy.LicenceReviewed), policy.LineageReviewed...) {
		if _, ok := Lookup(id); !ok {
			return fmt.Errorf("%q is not in the catalog", id)
		}
	}
	return nil
}

// Decision is the policy's verdict on one model.
type Decision struct {
	Allowed bool
	Reasons []string
}

// Error returns the refusal as an error, or nil when allowed.
func (decision Decision) Error() error {
	if decision.Allowed {
		return nil
	}
	return errors.New(strings.Join(decision.Reasons, "; "))
}

// Check decides whether a model connection may be used. catalogID is the
// entry the connection names, if any, and modelName the model ID it sends;
// see Identify. Connections that match no entry are refused when the
// policy allowlists origins, requires a commercial licence or refuses
// uncataloged models, since nothing is known about them; a policy that only
// blocks origins lets them through.
func (policy Policy) Check(catalogID, modelName string) Decision {
	if !policy.Restrictive() {
		return Decision{Allowed: true}
	}
	model, ok, err := Identify(catalogID, modelName)
	if err != nil {
		return Decision{Reasons: []string{err.Error()}}
	}
	if !ok {
		if len(policy.AllowedOrigins) > 0 || policy.RequireCommercialUse || policy.RefuseUncataloged {
			return Decision{Reasons: []string{"it names no catalog entry, so the model policy cannot check it; pick the model's catalog entry"}}
		}
		return Decision{Allowed: true}
	}
	return policy.Evaluate(model.ID)
}

// Evaluate decides whether the catalog entry id may be used.
func (policy Policy) Evaluate(id string) Decision {
	model, ok := Lookup(id)
	if !ok {
		if !policy.Restrictive() {
			return Decision{Allowed: true}
		}
		return Decision{Reasons: []string{fmt.Sprintf("catalog entry %q does not exist", id)}}
	}
	var reasons []string
	origins := []string{model.Origin}
	if policy.CheckLineage {
		origins = model.Lineage()
	}
	for index, origin := range origins {
		subject := "its publisher"
		if index > 0 {
			subject = "its lineage"
		}
		switch {
		case origin == "" && len(policy.AllowedOrigins) > 0:
			reasons = append(reasons, "the origin of "+subject+" is not recorded")
		case origin != "" && slices.Contains(policy.BlockedOrigins, origin):
			reasons = append(reasons, fmt.Sprintf("%s is from %s, which the model policy blocks", subject, origin))
		case origin != "" && len(policy.AllowedOrigins) > 0 && !slices.Contains(policy.AllowedOrigins, origin):
			reasons = append(reasons, fmt.Sprintf("%s is from %s, which the model policy does not allow", subject, origin))
		}
	}
	if policy.CheckLineage && len(policy.AllowedOrigins) > 0 && model.Kind == KindOpen && !model.TrainedFromScratch && model.BaseModel == "" && !slices.Contains(policy.LineageReviewed, id) {
		reasons = append(reasons, "its base model is not recorded in the catalog; review it and mark its lineage reviewed")
	}
	if policy.RequireCommercialUse && !slices.Contains(policy.LicenceReviewed, id) {
		if reason := policy.licenceRefusal(model.Licence); reason != "" {
			reasons = append(reasons, reason)
		}
	}
	return Decision{Allowed: len(reasons) == 0, Reasons: reasons}
}

func (policy Policy) licenceRefusal(licence Licence) string {
	switch licence.Commercial {
	case CommercialYes, CommercialProvider:
		return ""
	case CommercialNo:
		return "its licence does not allow commercial use"
	case CommercialRevenueCap:
		if policy.AnnualRevenueUSD > 0 && policy.AnnualRevenueUSD < licence.RevenueCapUSD {
			return ""
		}
		return fmt.Sprintf("its licence allows free commercial use only below %s in annual revenue", FormatUSD(licence.RevenueCapUSD))
	default:
		return "its licence terms are not recorded in the catalog; review them and mark it reviewed"
	}
}

// FormatUSD formats whole dollars with thousands separators, such as
// $10,000,000.
func FormatUSD(value int64) string {
	digits := fmt.Sprint(value)
	var out strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	return "$" + out.String()
}
