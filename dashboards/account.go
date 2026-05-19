package dashboards

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/chorus"
	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/salesforce"
)

// BuildAccountDashboard resolves an account by name, fans out to every
// configured integration in parallel, scores the result, and returns a ready-
// to-persist Dashboard with Kind="account".
//
// Missing integrations do not block the pipeline: each fan-out goroutine
// records an empty result set and a soft error instead, so a dashboard still
// renders with whatever data is available.
func BuildAccountDashboard(ctx context.Context, clients Clients, agent, createdBy, query string) (*Dashboard, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("account name is required")
	}
	if clients.SF == nil || !clients.SF.Ready() {
		return nil, fmt.Errorf("salesforce integration is not configured — cannot resolve account")
	}

	acct, err := resolveAccount(clients.SF, query)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	snap := AccountSnapshot{Account: acct, Now: start.UTC()}
	needles := accountNeedles(acct.Name, query)
	sourceResults := make(map[string]SourceResult, 4)
	sources := []DataSource{
		{Type: "salesforce_query", Name: "Opportunities"},
		{Type: "jira_search", Name: "Open Tickets"},
		{Type: "chorus_list_conversations", Name: "Recent Calls"},
		{Type: "datadog_list_monitors", Name: "Active Monitors"},
	}

	var mu sync.Mutex
	var wg sync.WaitGroup

	// ── Salesforce: opportunities ─────────────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		t0 := time.Now()
		opps, err := fetchOpportunities(clients.SF, acct.ID, needles)
		res := SourceResult{
			FetchedAt:  time.Now().UTC().Format(time.RFC3339),
			DurationMS: time.Since(t0).Milliseconds(),
		}
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Content = opps
			mu.Lock()
			snap.Opps = opps
			mu.Unlock()
		}
		mu.Lock()
		sourceResults["Opportunities"] = res
		mu.Unlock()
	}()

	// ── Jira: open tickets mentioning the account ────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		t0 := time.Now()
		res := SourceResult{FetchedAt: time.Now().UTC().Format(time.RFC3339)}
		if clients.Jira == nil || !clients.Jira.Ready() {
			res.Error = "jira client not ready"
			mu.Lock()
			sourceResults["Open Tickets"] = res
			mu.Unlock()
			return
		}
		jql := buildAccountJQL(needles)
		issues, err := clients.Jira.SearchIssuesJQL(jql, 50)
		res.DurationMS = time.Since(t0).Milliseconds()
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Content = issues
			mu.Lock()
			snap.JiraIssues = issues
			mu.Unlock()
		}
		mu.Lock()
		sourceResults["Open Tickets"] = res
		mu.Unlock()
	}()

	// ── Chorus: calls over the last 45 days matching account by name ─────
	wg.Add(1)
	go func() {
		defer wg.Done()
		t0 := time.Now()
		res := SourceResult{FetchedAt: time.Now().UTC().Format(time.RFC3339)}
		if clients.Chorus == nil || !clients.Chorus.Ready() {
			res.Error = "chorus client not ready"
			mu.Lock()
			sourceResults["Recent Calls"] = res
			mu.Unlock()
			return
		}
		engs, err := fetchEngagements(clients.Chorus, needles, acct.Website)
		res.DurationMS = time.Since(t0).Milliseconds()
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Content = engs
			mu.Lock()
			snap.Engagements = engs
			mu.Unlock()
		}
		mu.Lock()
		sourceResults["Recent Calls"] = res
		mu.Unlock()
	}()

	// ── Datadog: alerting monitors (best-effort, tag match optional) ─────
	wg.Add(1)
	go func() {
		defer wg.Done()
		t0 := time.Now()
		res := SourceResult{FetchedAt: time.Now().UTC().Format(time.RFC3339)}
		if clients.Datadog == nil {
			res.Error = "datadog not configured"
			mu.Lock()
			sourceResults["Active Monitors"] = res
			mu.Unlock()
			return
		}
		monitors, err := fetchMonitors(ctx, clients.Datadog, needles)
		res.DurationMS = time.Since(t0).Milliseconds()
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Content = monitors
			mu.Lock()
			snap.Monitors = monitors
			mu.Unlock()
		}
		mu.Lock()
		sourceResults["Active Monitors"] = res
		mu.Unlock()
	}()

	wg.Wait()

	summary := ComputeHealth(snap)
	d := &Dashboard{
		Agent:        agent,
		Kind:         "account",
		Name:         fmt.Sprintf("%s — Account Health", acct.Name),
		ShortName:    "acct-" + CacheSlug(acct.Name),
		Description:  fmt.Sprintf("Health snapshot for %s, fanning out across Salesforce, Jira, Chorus, and Datadog.", acct.Name),
		SyncInterval: DefaultSyncInterval.String(),
		Sources:      sources,
		Data:         sourceResults,
		CreatedBy:    createdBy,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		LastSync:     time.Now().UTC().Format(time.RFC3339),
		Account:      summary,
	}
	log.Printf("[dashboards] account built agent=%s account=%q score=%d band=%s in %s",
		agent, acct.Name, summary.Score, summary.Band, time.Since(start))
	return d, nil
}

// resolveAccount does a case-insensitive fuzzy LIKE search in Salesforce and
// returns the highest-ranked match. Ranking prefers exact (case-insensitive)
// matches, then prefix matches, then shortest name (shorter = less generic).
func resolveAccount(sf *salesforce.Client, query string) (salesforce.Account, error) {
	// Guard against SOQL injection by stripping single quotes.
	safe := strings.ReplaceAll(query, "'", "")
	soql := fmt.Sprintf(
		"SELECT Id, Name, Type, Industry, Website, Owner.Name FROM Account WHERE Name LIKE '%%%s%%' ORDER BY LastModifiedDate DESC LIMIT 10",
		safe,
	)
	res, err := sf.Query(soql)
	if err != nil {
		return salesforce.Account{}, fmt.Errorf("salesforce account lookup failed: %w", err)
	}
	accts := salesforce.ParseAccounts(res.Records)
	if len(accts) == 0 {
		return salesforce.Account{}, fmt.Errorf("no Salesforce account matched %q", query)
	}
	// Pick the best match.
	q := strings.ToLower(strings.TrimSpace(query))
	best := accts[0]
	bestRank := rankAccountMatch(q, best.Name)
	for _, a := range accts[1:] {
		if r := rankAccountMatch(q, a.Name); r > bestRank {
			best = a
			bestRank = r
		}
	}
	return best, nil
}

// rankAccountMatch returns higher scores for better matches.
func rankAccountMatch(query, name string) int {
	n := strings.ToLower(name)
	switch {
	case n == query:
		return 100
	case strings.HasPrefix(n, query):
		return 50 + lengthBonus(n)
	case strings.Contains(n, query):
		return 10 + lengthBonus(n)
	}
	return 0
}

// Shorter names get a slight bonus — "Sprout Social" beats "Sprout Social DNR".
func lengthBonus(n string) int {
	if len(n) >= 40 {
		return 0
	}
	return 40 - len(n)
}

// fetchOpportunities pulls open + recently-closed opps for the account, plus
// any opportunity whose Account.Name matches one of the supplied needles so
// related accounts (e.g. a parent "Acme Pharmaceuticals" account holding
// the commercial relationship for the "Acme Ireland" subsidiary) are
// surfaced too. Results are deduped by opportunity Id.
func fetchOpportunities(sf *salesforce.Client, accountID string, needles []string) ([]salesforce.Opportunity, error) {
	clauses := make([]string, 0, len(needles)+1)
	if accountID != "" {
		clauses = append(clauses, fmt.Sprintf("AccountId = '%s'", strings.ReplaceAll(accountID, "'", "")))
	}
	for _, n := range needles {
		safe := strings.ReplaceAll(n, "'", "")
		if safe == "" {
			continue
		}
		clauses = append(clauses, fmt.Sprintf("Account.Name LIKE '%%%s%%'", safe))
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	soql := fmt.Sprintf(
		"SELECT Id, Name, StageName, Amount, CloseDate, Type, IsClosed, IsWon, Account.Name, Owner.Name FROM Opportunity WHERE %s ORDER BY CloseDate DESC LIMIT 50",
		strings.Join(clauses, " OR "),
	)
	res, err := sf.Query(soql)
	if err != nil {
		return nil, err
	}
	parsed := salesforce.ParseOpportunities(res.Records)
	seen := make(map[string]bool, len(parsed))
	out := parsed[:0]
	for _, o := range parsed {
		if o.ID == "" || seen[o.ID] {
			continue
		}
		seen[o.ID] = true
		out = append(out, o)
	}
	return out, nil
}

// buildAccountJQL returns JQL that matches open tickets whose summary or
// description references any of the supplied needles. Closed/done states are
// excluded. Multiple needles are OR-joined so "Acme Ireland" also picks
// up tickets that only mention "Acme".
func buildAccountJQL(needles []string) string {
	if len(needles) == 0 {
		return `statusCategory != Done ORDER BY priority DESC, updated DESC`
	}
	clauses := make([]string, 0, len(needles))
	for _, n := range needles {
		escaped := strings.ReplaceAll(n, `"`, `\"`)
		clauses = append(clauses, fmt.Sprintf(`summary ~ "%s" OR description ~ "%s"`, escaped, escaped))
	}
	return fmt.Sprintf(
		`(%s) AND statusCategory != Done ORDER BY priority DESC, updated DESC`,
		strings.Join(clauses, " OR "),
	)
}

// accountNeedles returns a deduped, lowercased list of search terms suited to
// free-text matching across Jira, Chorus, and Datadog. It always includes the
// full resolved account name plus the original user query, and additionally
// derives a "stem" by stripping common corporate and regional suffixes so a
// dashboard for "Acme Ireland" still surfaces data referencing only
// "Acme".
func accountNeedles(accountName, originalQuery string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 3)
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(accountName)
	add(originalQuery)
	add(stripAccountSuffixes(accountName))
	return out
}

// stripAccountSuffixes removes trailing corporate/regional tokens commonly
// appended to account names in Salesforce (e.g. "Acme Ireland",
// "Acme Inc.", "Foo Ltd"). Matching is case-insensitive and applied
// repeatedly so chained suffixes ("Acme Ireland Ltd") fully collapse.
func stripAccountSuffixes(name string) string {
	n := strings.TrimSpace(name)
	suffixes := []string{
		" Ireland", " UK", " USA", " US", " EMEA", " APAC", " Americas",
		" EU", " Europe", " Global", " International",
		" Inc", " Inc.", " Corp", " Corp.", " Corporation",
		" LLC", " L.L.C.", " Ltd", " Ltd.", " Limited",
		" GmbH", " AG", " SA", " S.A.", " PLC", " Co.", " Company",
	}
	for changed := true; changed; {
		changed = false
		lower := strings.ToLower(n)
		for _, s := range suffixes {
			ls := strings.ToLower(s)
			if strings.HasSuffix(lower, ls) {
				n = strings.TrimSpace(strings.TrimRight(n[:len(n)-len(s)], ","))
				changed = true
				break
			}
		}
	}
	return n
}

// fetchEngagements queries Chorus for the last 45 days with no participant
// filter, then filters client-side across every signal we can lay hands on:
// subject, Chorus-reported account name, participant company name, and
// participant email (matched against either the account's website domain or
// any of the provided needles). The v3 filter API only supports a single
// participant email pattern, which is too narrow when the account has no
// website on file or when relevant calls include reps from a parent account —
// so we cast a wider net here and dedupe by engagement_id.
func fetchEngagements(ch *chorus.Client, needles []string, website string) ([]chorus.Engagement, error) {
	from := time.Now().AddDate(0, 0, -45).Format("2006-01-02")
	engs, err := ch.ListEngagements(chorus.EngagementFilter{MinDate: from, WithTrackers: true})
	if err != nil {
		return nil, err
	}
	domain := domainFromWebsite(website)
	seen := make(map[string]bool, len(engs))
	out := engs[:0]
	for _, e := range engs {
		if matchesEngagement(e, needles, domain) {
			id := e.EngagementID
			if id == "" || !seen[id] {
				seen[id] = true
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// matchesEngagement returns true when any of the supplied needles (or the
// account's email domain) appears in a meaningful field on the engagement.
func matchesEngagement(e chorus.Engagement, needles []string, domain string) bool {
	subject := strings.ToLower(engagementTitle(e))
	acctName := strings.ToLower(e.AccountName)
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(subject, n) || strings.Contains(acctName, n) {
			return true
		}
		for _, p := range e.Participants {
			if p.CompanyName != nil && strings.Contains(strings.ToLower(*p.CompanyName), n) {
				return true
			}
			if strings.Contains(strings.ToLower(p.Email), n) {
				return true
			}
		}
	}
	if domain != "" {
		needle := "@" + strings.ToLower(domain)
		for _, p := range e.Participants {
			if strings.Contains(strings.ToLower(p.Email), needle) {
				return true
			}
		}
	}
	return false
}

func engagementTitle(e chorus.Engagement) string {
	// Chorus's v3 "subject" is the closest thing to a title.
	return e.Subject
}

func domainFromWebsite(w string) string {
	w = strings.TrimSpace(strings.ToLower(w))
	if w == "" {
		return ""
	}
	w = strings.TrimPrefix(w, "https://")
	w = strings.TrimPrefix(w, "http://")
	w = strings.TrimPrefix(w, "www.")
	if i := strings.IndexAny(w, "/?#"); i >= 0 {
		w = w[:i]
	}
	return w
}

// fetchMonitors pulls monitors from every configured Datadog site and keeps
// only those that are currently alerting or have a tag matching the account.
func fetchMonitors(ctx context.Context, mc *datadog.MultiClient, needles []string) ([]datadog.Monitor, error) {
	clientList := []*datadog.Client{}
	if mc.US != nil {
		clientList = append(clientList, mc.US)
	}
	if mc.EU != nil {
		clientList = append(clientList, mc.EU)
	}
	if len(clientList) == 0 {
		return nil, fmt.Errorf("no datadog clients configured")
	}
	var out []datadog.Monitor
	for _, c := range clientList {
		monitors, err := c.ListMonitors(ctx, "", 100)
		if err != nil {
			return nil, err
		}
		for _, m := range monitors {
			state := strings.ToLower(m.OverallState)
			relevant := state == "alert" || state == "warn" || state == "warning"
			tag := anyTagMatchesAccount(m.Tags, needles)
			nameMatch := anyNameMatchesAccount(m.Name, needles)
			if !relevant {
				// Keep non-alerting monitors only if they have an explicit
				// customer/account tag match, so unrelated monitors don't dilute the
				// list.
				if !tag {
					continue
				}
			}
			if len(needles) > 0 && !tag && !nameMatch {
				continue
			}
			out = append(out, m)
		}
	}
	return out, nil
}

func anyTagMatchesAccount(tags []string, needles []string) bool {
	for _, n := range needles {
		if tagMatchesAccount(tags, n) {
			return true
		}
	}
	return false
}

func anyNameMatchesAccount(name string, needles []string) bool {
	ln := strings.ToLower(name)
	for _, n := range needles {
		if n != "" && strings.Contains(ln, n) {
			return true
		}
	}
	return false
}

func tagMatchesAccount(tags []string, needle string) bool {
	if needle == "" {
		return false
	}
	for _, t := range tags {
		tl := strings.ToLower(t)
		if strings.Contains(tl, "customer:"+needle) ||
			strings.Contains(tl, "account:"+needle) ||
			strings.Contains(tl, "tenant:"+needle) {
			return true
		}
	}
	return false
}
