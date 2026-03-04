package nvd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	baseURL = "https://services.nvd.nist.gov/rest/json/cves/2.0"

	// Response body size limit for io.LimitReader.
	maxResponseBody = 5 << 20 // 5 MB
)

// Client talks to the NVD CVE API v2.0.
type Client struct {
	apiKey     string
	httpClient *http.Client
}

// NewClient creates an NVD API client. apiKey may be empty (unauthenticated
// requests are rate-limited to ~5 req/30s; with a key it's ~50 req/30s).
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// --------------------------------------------------------------------------
// Public methods
// --------------------------------------------------------------------------

// LookupCVE fetches a single CVE by its ID (e.g. "CVE-2025-13836").
func (c *Client) LookupCVE(ctx context.Context, cveID string) (*CVEItem, error) {
	params := url.Values{"cveId": {cveID}}
	var resp cveResponse
	if err := c.get(ctx, params, &resp); err != nil {
		return nil, err
	}
	if len(resp.Vulnerabilities) == 0 {
		return nil, fmt.Errorf("CVE %s not found in NVD", cveID)
	}
	return &resp.Vulnerabilities[0].CVE, nil
}

// SearchCVE runs a keyword search against NVD descriptions. Returns up to
// resultsPerPage results (max 20 to keep output manageable).
func (c *Client) SearchCVE(ctx context.Context, keyword string, resultsPerPage int) ([]CVEItem, int, error) {
	if resultsPerPage <= 0 || resultsPerPage > 20 {
		resultsPerPage = 5
	}
	params := url.Values{
		"keywordSearch":  {keyword},
		"resultsPerPage": {fmt.Sprintf("%d", resultsPerPage)},
	}
	var resp cveResponse
	if err := c.get(ctx, params, &resp); err != nil {
		return nil, 0, err
	}
	items := make([]CVEItem, len(resp.Vulnerabilities))
	for i, v := range resp.Vulnerabilities {
		items[i] = v.CVE
	}
	return items, resp.TotalResults, nil
}

// --------------------------------------------------------------------------
// Formatting helpers
// --------------------------------------------------------------------------

// FormatCVE returns a concise Slack-friendly summary of a CVE.
func FormatCVE(cve *CVEItem) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%s*\n", cve.ID)

	// Description (first English one).
	for _, d := range cve.Descriptions {
		if d.Lang == "en" {
			desc := d.Value
			if len(desc) > 500 {
				desc = desc[:500] + "…"
			}
			sb.WriteString(desc + "\n\n")
			break
		}
	}

	// CVSS scores.
	if m := cve.Metrics; m != nil {
		if len(m.CvssV40) > 0 {
			d := m.CvssV40[0].CvssData
			fmt.Fprintf(&sb, "• *CVSS v4.0:* %.1f (%s) — `%s`\n", d.BaseScore, d.BaseSeverity, d.VectorString)
		}
		if len(m.CvssV31) > 0 {
			d := m.CvssV31[0].CvssData
			fmt.Fprintf(&sb, "• *CVSS v3.1:* %.1f (%s) — `%s`\n", d.BaseScore, d.BaseSeverity, d.VectorString)
		}
		if len(m.CvssV30) > 0 {
			d := m.CvssV30[0].CvssData
			fmt.Fprintf(&sb, "• *CVSS v3.0:* %.1f (%s) — `%s`\n", d.BaseScore, d.BaseSeverity, d.VectorString)
		}
		if len(m.CvssV2) > 0 {
			d := m.CvssV2[0].CvssData
			fmt.Fprintf(&sb, "• *CVSS v2.0:* %.1f — `%s`\n", d.BaseScore, d.VectorString)
		}
	}

	// Weaknesses (CWE IDs).
	if len(cve.Weaknesses) > 0 {
		var cwes []string
		for _, w := range cve.Weaknesses {
			for _, d := range w.Description {
				if d.Lang == "en" && d.Value != "NVD-CWE-noinfo" && d.Value != "NVD-CWE-Other" {
					cwes = append(cwes, d.Value)
				}
			}
		}
		if len(cwes) > 0 {
			fmt.Fprintf(&sb, "• *Weaknesses:* %s\n", strings.Join(cwes, ", "))
		}
	}

	// Affected configurations (CPEs) — show first few.
	if len(cve.Configurations) > 0 {
		var cpes []string
		for _, cfg := range cve.Configurations {
			for _, node := range cfg.Nodes {
				for _, match := range node.CpeMatch {
					label := match.Criteria
					if match.VersionStartIncluding != "" || match.VersionEndExcluding != "" {
						label += fmt.Sprintf(" [%s, %s)", nvdOr(match.VersionStartIncluding, "*"), nvdOr(match.VersionEndExcluding, "*"))
					}
					if match.VersionEndIncluding != "" {
						label += fmt.Sprintf(" [%s, %s]", nvdOr(match.VersionStartIncluding, "*"), match.VersionEndIncluding)
					}
					cpes = append(cpes, label)
					if len(cpes) >= 10 {
						break
					}
				}
				if len(cpes) >= 10 {
					break
				}
			}
			if len(cpes) >= 10 {
				break
			}
		}
		if len(cpes) > 0 {
			sb.WriteString("• *Affected products:*\n")
			for _, cpe := range cpes {
				fmt.Fprintf(&sb, "  – `%s`\n", cpe)
			}
		}
	}

	// References — first 5.
	if len(cve.References) > 0 {
		sb.WriteString("• *References:*\n")
		limit := 5
		if len(cve.References) < limit {
			limit = len(cve.References)
		}
		for _, ref := range cve.References[:limit] {
			tag := ""
			if len(ref.Tags) > 0 {
				tag = " (" + strings.Join(ref.Tags, ", ") + ")"
			}
			fmt.Fprintf(&sb, "  – <%s|link>%s\n", ref.URL, tag)
		}
	}

	fmt.Fprintf(&sb, "• *Published:* %s | *Last modified:* %s\n", cve.Published, cve.LastModified)
	fmt.Fprintf(&sb, "• *NVD:* <https://nvd.nist.gov/vuln/detail/%s|View on NVD>", cve.ID)

	return sb.String()
}

func nvdOr(val, fallback string) string {
	if val == "" {
		return fallback
	}
	return val
}

// --------------------------------------------------------------------------
// HTTP transport
// --------------------------------------------------------------------------

func (c *Client) get(ctx context.Context, params url.Values, target interface{}) error {
	u, _ := url.Parse(baseURL)
	u.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create NVD request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("apiKey", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("NVD API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("failed to read NVD response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("NVD API returned %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("failed to parse NVD response: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --------------------------------------------------------------------------
// NVD CVE API v2.0 response types
// --------------------------------------------------------------------------

type cveResponse struct {
	ResultsPerPage  int             `json:"resultsPerPage"`
	StartIndex      int             `json:"startIndex"`
	TotalResults    int             `json:"totalResults"`
	Vulnerabilities []vulnerability `json:"vulnerabilities"`
}

type vulnerability struct {
	CVE CVEItem `json:"cve"`
}

// CVEItem represents a single CVE record from NVD.
type CVEItem struct {
	ID             string          `json:"id"`
	SourceID       string          `json:"sourceIdentifier"`
	Published      string          `json:"published"`
	LastModified   string          `json:"lastModified"`
	VulnStatus     string          `json:"vulnStatus"`
	Descriptions   []langString    `json:"descriptions"`
	Metrics        *metrics        `json:"metrics,omitempty"`
	Weaknesses     []weakness      `json:"weaknesses"`
	Configurations []configuration `json:"configurations"`
	References     []reference     `json:"references"`
}

type langString struct {
	Lang  string `json:"lang"`
	Value string `json:"value"`
}

type metrics struct {
	CvssV40 []cvssEntry `json:"cvssMetricV40,omitempty"`
	CvssV31 []cvssEntry `json:"cvssMetricV31,omitempty"`
	CvssV30 []cvssEntry `json:"cvssMetricV30,omitempty"`
	CvssV2  []cvssEntry `json:"cvssMetricV2,omitempty"`
}

type cvssEntry struct {
	Source   string   `json:"source"`
	Type     string   `json:"type"`
	CvssData cvssData `json:"cvssData"`
}

type cvssData struct {
	Version      string  `json:"version"`
	VectorString string  `json:"vectorString"`
	BaseScore    float64 `json:"baseScore"`
	BaseSeverity string  `json:"baseSeverity,omitempty"`
}

type weakness struct {
	Source      string       `json:"source"`
	Type        string       `json:"type"`
	Description []langString `json:"description"`
}

type configuration struct {
	Nodes []node `json:"nodes"`
}

type node struct {
	Operator string     `json:"operator"`
	Negate   bool       `json:"negate"`
	CpeMatch []cpeMatch `json:"cpeMatch"`
}

type cpeMatch struct {
	Vulnerable            bool   `json:"vulnerable"`
	Criteria              string `json:"criteria"`
	VersionStartIncluding string `json:"versionStartIncluding,omitempty"`
	VersionStartExcluding string `json:"versionStartExcluding,omitempty"`
	VersionEndIncluding   string `json:"versionEndIncluding,omitempty"`
	VersionEndExcluding   string `json:"versionEndExcluding,omitempty"`
	MatchCriteriaID       string `json:"matchCriteriaId"`
}

type reference struct {
	URL    string   `json:"url"`
	Source string   `json:"source"`
	Tags   []string `json:"tags,omitempty"`
}
