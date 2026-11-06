// Package secgate turns a scanner's findings into a pass or fail for a
// pipeline. It reads Trivy's JSON report, applies a severity threshold and
// time-limited waivers, and checks a running service's response headers against
// the OWASP Secure Headers Project recommendations.
package secgate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

var severityRank = map[string]int{"UNKNOWN": 0, "LOW": 1, "MEDIUM": 2, "HIGH": 3, "CRITICAL": 4}

type Finding struct {
	ID        string `json:"id"`
	Package   string `json:"package"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed,omitempty"`
	Severity  string `json:"severity"`
	Title     string `json:"title,omitempty"`
	Target    string `json:"target,omitempty"`
}

// ParseTrivy reads the JSON written by `trivy ... --format json`.
func ParseTrivy(r io.Reader) ([]Finding, error) {
	var rep struct {
		SchemaVersion int `json:"SchemaVersion"`
		Results       []struct {
			Target          string `json:"Target"`
			Vulnerabilities []struct {
				VulnerabilityID  string `json:"VulnerabilityID"`
				PkgName          string `json:"PkgName"`
				InstalledVersion string `json:"InstalledVersion"`
				FixedVersion     string `json:"FixedVersion"`
				Severity         string `json:"Severity"`
				Title            string `json:"Title"`
			} `json:"Vulnerabilities"`
		} `json:"Results"`
	}
	if err := json.NewDecoder(r).Decode(&rep); err != nil {
		return nil, fmt.Errorf("not a Trivy JSON report: %w", err)
	}
	if rep.SchemaVersion == 0 && rep.Results == nil {
		return nil, fmt.Errorf("not a Trivy JSON report: no SchemaVersion or Results")
	}
	var out []Finding
	for _, res := range rep.Results {
		for _, v := range res.Vulnerabilities {
			sev := strings.ToUpper(v.Severity)
			if _, ok := severityRank[sev]; !ok {
				sev = "UNKNOWN"
			}
			out = append(out, Finding{ID: v.VulnerabilityID, Package: v.PkgName, Installed: v.InstalledVersion, Fixed: v.FixedVersion, Severity: sev, Title: v.Title, Target: res.Target})
		}
	}
	return out, nil
}

// Waiver accepts a finding for a limited time, with a reason. An expired waiver
// no longer applies, so an accepted risk has to be looked at again.
type Waiver struct {
	ID      string    `json:"id"`
	Package string    `json:"package,omitempty"` // empty matches any package
	Reason  string    `json:"reason"`
	Expires time.Time `json:"expires"`
}

type Policy struct {
	FailOn     string   `json:"failOn"`    // lowest severity that blocks: LOW, MEDIUM, HIGH or CRITICAL
	FixedOnly  bool     `json:"fixedOnly"` // only block findings that have a fixed version available
	Waivers    []Waiver `json:"waivers"`
	MaxWaivers int      `json:"maxWaivers"` // 0 = no limit
}

type Decision struct {
	Pass     bool      `json:"pass"`
	Blocking []Finding `json:"blocking"`
	Waived   []Finding `json:"waived"`
	Expired  []Finding `json:"expiredWaiver"` // blocking because the waiver ran out
	Below    int       `json:"belowThreshold"`
	NoFix    int       `json:"noFixSkipped"`
	Problems []string  `json:"problems,omitempty"`
}

func (w Waiver) matches(f Finding) bool {
	return w.ID == f.ID && (w.Package == "" || w.Package == f.Package)
}

// Evaluate applies the policy at the given time.
func Evaluate(findings []Finding, p Policy, now time.Time) Decision {
	d := Decision{}
	threshold, ok := severityRank[strings.ToUpper(p.FailOn)]
	if !ok {
		d.Problems = append(d.Problems, fmt.Sprintf("unknown failOn %q, expected LOW, MEDIUM, HIGH or CRITICAL", p.FailOn))
		return d
	}
	if p.MaxWaivers > 0 && len(p.Waivers) > p.MaxWaivers {
		d.Problems = append(d.Problems, fmt.Sprintf("%d waivers, the policy allows %d", len(p.Waivers), p.MaxWaivers))
	}
	for _, w := range p.Waivers {
		if strings.TrimSpace(w.Reason) == "" {
			d.Problems = append(d.Problems, fmt.Sprintf("waiver for %s has no reason", w.ID))
		}
		if w.Expires.IsZero() {
			d.Problems = append(d.Problems, fmt.Sprintf("waiver for %s has no expiry", w.ID))
		}
	}
	for _, f := range findings {
		if severityRank[f.Severity] < threshold {
			d.Below++
			continue
		}
		if p.FixedOnly && f.Fixed == "" {
			d.NoFix++
			continue
		}
		var matched, live bool
		for _, w := range p.Waivers {
			if w.matches(f) {
				matched = true
				if !w.Expires.IsZero() && now.Before(w.Expires) {
					live = true
				}
			}
		}
		switch {
		case live:
			d.Waived = append(d.Waived, f)
		case matched:
			d.Expired = append(d.Expired, f)
			d.Blocking = append(d.Blocking, f)
		default:
			d.Blocking = append(d.Blocking, f)
		}
	}
	sortFindings(d.Blocking)
	d.Pass = len(d.Blocking) == 0 && len(d.Problems) == 0
	return d
}

func sortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if severityRank[f[i].Severity] != severityRank[f[j].Severity] {
			return severityRank[f[i].Severity] > severityRank[f[j].Severity]
		}
		return f[i].ID < f[j].ID
	})
}

// ---- response headers (OWASP Secure Headers Project) ----

type HeaderFinding struct {
	Header string `json:"header"`
	Level  string `json:"level"` // fail or warn
	Why    string `json:"why"`
}

// CheckHeaders reviews one response's headers.
func CheckHeaders(h http.Header) []HeaderFinding {
	var out []HeaderFinding
	add := func(header, level, why string) { out = append(out, HeaderFinding{header, level, why}) }
	if v := h.Get("Strict-Transport-Security"); v == "" {
		add("Strict-Transport-Security", "fail", "missing: browsers may use plain HTTP and be downgraded")
	} else if !strings.Contains(v, "max-age=") || maxAge(v) < 15552000 {
		add("Strict-Transport-Security", "warn", "max-age under 180 days")
	}
	if !strings.EqualFold(h.Get("X-Content-Type-Options"), "nosniff") {
		add("X-Content-Type-Options", "fail", "should be nosniff, so a browser does not guess a content type")
	}
	csp := h.Get("Content-Security-Policy")
	if csp == "" {
		add("Content-Security-Policy", "fail", "missing: no limit on where scripts and frames can load from")
	} else if strings.Contains(csp, "'unsafe-inline'") || strings.Contains(csp, "'unsafe-eval'") {
		add("Content-Security-Policy", "warn", "allows unsafe-inline or unsafe-eval, which defeats most of the policy")
	}
	xfo := strings.ToUpper(h.Get("X-Frame-Options"))
	if xfo != "DENY" && xfo != "SAMEORIGIN" && !strings.Contains(csp, "frame-ancestors") {
		add("X-Frame-Options", "fail", "no clickjacking protection: set DENY, or frame-ancestors in the CSP")
	}
	if h.Get("Referrer-Policy") == "" {
		add("Referrer-Policy", "warn", "missing: full URLs may leak to other sites")
	}
	if h.Get("Permissions-Policy") == "" {
		add("Permissions-Policy", "warn", "missing: camera, microphone and location are not explicitly turned off")
	}
	for _, leak := range []string{"X-Powered-By", "X-AspNet-Version"} {
		if h.Get(leak) != "" {
			add(leak, "fail", "reveals the framework and version to anyone scanning")
		}
	}
	if s := h.Get("Server"); strings.ContainsAny(s, "0123456789") && strings.Contains(s, "/") {
		add("Server", "warn", "includes a version number: "+s)
	}
	return out
}

func maxAge(v string) int {
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "max-age=") {
			var n int
			fmt.Sscanf(strings.TrimPrefix(part, "max-age="), "%d", &n)
			return n
		}
	}
	return 0
}

// HasFailures reports whether any finding is a fail.
func HasFailures(f []HeaderFinding) bool {
	for _, x := range f {
		if x.Level == "fail" {
			return true
		}
	}
	return false
}
