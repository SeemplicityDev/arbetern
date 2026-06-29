package billing

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Price is the cost of a model in US dollars per one million tokens, split
// between input (prompt) and output (completion). LLM vendors bill these two
// rates separately, so the cost of a turn is:
//
//	cost = in*prompt/1e6 + out*completion/1e6
type Price struct {
	In  float64 `json:"in"`  // USD per 1M prompt tokens
	Out float64 `json:"out"` // USD per 1M completion tokens
}

// defaultPriceSourceURL is the single source of truth synced on boot and daily:
// LiteLLM's community price file (USD per token, input/output). Override with
// PRICE_SOURCE_URL, or set it empty to rely solely on LLM_PRICE_OVERRIDES.
const defaultPriceSourceURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

var (
	pricesMu    sync.RWMutex
	prices      = mergePrices(nil)
	priceSrcURL string
	priceSrcAt  time.Time
	priceSrcErr string
)

// PriceSource is the live pricing-feed status surfaced in the billing UI.
type PriceSource struct {
	URL      string    `json:"url,omitempty"`
	Models   int       `json:"models"`
	LastSync time.Time `json:"last_sync,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// SourceInfo returns the current pricing-feed status (URL, model count, last
// successful sync, last error) for display.
func SourceInfo() PriceSource {
	pricesMu.RLock()
	defer pricesMu.RUnlock()
	return PriceSource{URL: priceSrcURL, Models: len(prices), LastSync: priceSrcAt, Error: priceSrcErr}
}

// mergePrices builds the table from the fetched source of truth, with
// LLM_PRICE_OVERRIDES layered on top so negotiated rates always win. Malformed
// override JSON is ignored so a bad value never blocks startup. Changing the
// table only affects future turns: Record() snapshots CostUSD at record time
// and persisted aggregates are never recomputed.
func mergePrices(fetched map[string]Price) map[string]Price {
	merged := make(map[string]Price, len(fetched))
	for k, v := range fetched {
		merged[strings.ToLower(k)] = v
	}
	if raw := strings.TrimSpace(os.Getenv("LLM_PRICE_OVERRIDES")); raw != "" {
		var ov map[string]Price
		if err := json.Unmarshal([]byte(raw), &ov); err == nil {
			for k, v := range ov {
				merged[strings.ToLower(k)] = v
			}
		}
	}
	return merged
}

// StartPriceSync refreshes the price table from the source of truth now and
// every 24h until stop closes. Fetch failures keep the previous table and are
// surfaced via SourceInfo. Past spend is unaffected.
func StartPriceSync(stop <-chan struct{}) {
	url := defaultPriceSourceURL
	if v, ok := os.LookupEnv("PRICE_SOURCE_URL"); ok {
		url = strings.TrimSpace(v)
	}
	if url == "" {
		return // rely solely on LLM_PRICE_OVERRIDES
	}
	pricesMu.Lock()
	priceSrcURL = url
	pricesMu.Unlock()
	refresh := func() {
		f, err := fetchPrices(url)
		pricesMu.Lock()
		defer pricesMu.Unlock()
		if err != nil {
			priceSrcErr = err.Error()
			log.Printf("billing: price sync failed, keeping current rates: %v", err)
			return
		}
		prices = mergePrices(f)
		priceSrcAt = time.Now().UTC()
		priceSrcErr = ""
		log.Printf("billing: synced %d model prices from %s", len(f), url)
	}
	go func() {
		refresh()
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				refresh()
			}
		}
	}()
}

// fetchPrices pulls the source-of-truth file (per-token USD) and converts to
// per-1M-token rates. Only models exposing both input and output costs are kept.
func fetchPrices(url string) (map[string]Price, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errStatus(resp.StatusCode)
	}
	var raw map[string]struct {
		In  *float64 `json:"input_cost_per_token"`
		Out *float64 `json:"output_cost_per_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	out := make(map[string]Price, len(raw))
	for name, c := range raw {
		if c.In == nil || c.Out == nil {
			continue
		}
		out[strings.ToLower(name)] = Price{In: *c.In * 1e6, Out: *c.Out * 1e6}
	}
	return out, nil
}

type errStatus int

func (e errStatus) Error() string {
	return "price source returned HTTP " + strings.TrimSpace(http.StatusText(int(e)))
}

// PriceFor returns the per-1M-token rate for a model and whether a rate is
// known. The longest matching substring wins so specific variants override
// their family default.
func PriceFor(model string) (Price, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return Price{}, false
	}
	pricesMu.RLock()
	defer pricesMu.RUnlock()

	keys := make([]string, 0, len(prices))
	for k := range prices {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if strings.Contains(m, k) {
			return prices[k], true
		}
	}
	return Price{}, false
}

// Cost returns the dollar cost of a turn and whether the model was priced.
func Cost(model string, promptTokens, completionTokens int) (float64, bool) {
	p, ok := PriceFor(model)
	if !ok {
		return 0, false
	}
	return p.In*float64(promptTokens)/1e6 + p.Out*float64(completionTokens)/1e6, true
}
