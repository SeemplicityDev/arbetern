package grafana

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAnnotationLimit = 50
	maxAnnotationLimit     = 200
)

// AnnotationOptions narrows an annotation listing. Zero values are not sent.
type AnnotationOptions struct {
	From         time.Time
	To           time.Time
	DashboardUID string
	PanelID      int
	Tags         []string
	MatchAny     bool
	Type         string
	Limit        int
}

// GetAnnotations lists annotations, newest first.
func (c *Client) GetAnnotations(ctx context.Context, opts AnnotationOptions) ([]Annotation, error) {
	q := url.Values{}
	if !opts.From.IsZero() {
		q.Set("from", strconv.FormatInt(opts.From.UnixMilli(), 10))
	}
	if !opts.To.IsZero() {
		q.Set("to", strconv.FormatInt(opts.To.UnixMilli(), 10))
	}
	if uid := strings.TrimSpace(opts.DashboardUID); uid != "" {
		q.Set("dashboardUID", uid)
	}
	if opts.PanelID > 0 {
		q.Set("panelId", strconv.Itoa(opts.PanelID))
	}
	for _, t := range opts.Tags {
		if t = strings.TrimSpace(t); t != "" {
			q.Add("tags", t)
		}
	}
	if opts.MatchAny {
		q.Set("matchAny", "true")
	}
	if t := strings.ToLower(strings.TrimSpace(opts.Type)); t == "annotation" || t == "alert" {
		q.Set("type", t)
	}
	q.Set("limit", strconv.Itoa(clamp(opts.Limit, 1, maxAnnotationLimit, defaultAnnotationLimit)))
	var list []Annotation
	if err := c.get(ctx, "/api/annotations", q, &list); err != nil {
		return nil, err
	}
	return list, nil
}
