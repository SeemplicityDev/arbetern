package grafana

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

func (c *Client) fetchDatasources(ctx context.Context) ([]Datasource, error) {
	var list []Datasource
	err := c.get(ctx, "/api/datasources", nil, &list)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusForbidden {
		// Viewer accounts lack datasources:read; the frontend settings still
		// list every datasource they may query.
		list, err = c.frontendDatasources(ctx)
	}
	if err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].IsDefault != list[j].IsDefault {
			return list[i].IsDefault
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	return list, nil
}

func (c *Client) frontendDatasources(ctx context.Context) ([]Datasource, error) {
	var fs frontendSettings
	if err := c.get(ctx, "/api/frontend/settings", nil, &fs); err != nil {
		return nil, err
	}
	var list []Datasource
	for key, d := range fs.Datasources {
		if isBuiltinDatasource(d.UID, d.Type) {
			continue
		}
		name := d.Name
		if name == "" {
			name = key
		}
		list = append(list, Datasource{
			UID:       d.UID,
			Name:      name,
			Type:      d.Type,
			TypeName:  d.Meta.Name,
			URL:       d.URL,
			IsDefault: d.IsDefault || name == fs.DefaultDatasource,
			JSONData:  d.JSONData,
		})
	}
	return list, nil
}

// isBuiltinDatasource reports the pseudo datasources: -- Grafana --, -- Mixed --,
// -- Dashboard -- and server-side expressions.
func isBuiltinDatasource(uid, typ string) bool {
	switch {
	case typ == "datasource", typ == "grafana", typ == "__expr__", uid == "grafana", uid == "__expr__":
		return true
	case strings.HasPrefix(uid, "-- "):
		return true
	}
	return false
}

// ListDatasources returns the datasources the service account can see, default
// first, narrowed by case-insensitive substrings of type and name.
func (c *Client) ListDatasources(ctx context.Context, typeFilter, nameFilter string) ([]Datasource, error) {
	all, err := c.datasources.Get(ctx)
	if err != nil {
		return nil, err
	}
	typeFilter = strings.ToLower(strings.TrimSpace(typeFilter))
	nameFilter = strings.ToLower(strings.TrimSpace(nameFilter))
	var out []Datasource
	for _, d := range all {
		if typeFilter != "" && !strings.Contains(strings.ToLower(d.Type), typeFilter) && !strings.Contains(strings.ToLower(d.TypeName), typeFilter) {
			continue
		}
		if nameFilter != "" && !strings.Contains(strings.ToLower(d.Name), nameFilter) && !strings.Contains(strings.ToLower(d.UID), nameFilter) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// findDatasource matches a reference against the visible datasources by UID,
// then by name. An empty reference or "default" means the instance default.
func (c *Client) findDatasource(ctx context.Context, ref string) (*Datasource, error) {
	all, err := c.datasources.Get(ctx)
	if err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "default" {
		for i := range all {
			if all[i].IsDefault {
				return &all[i], nil
			}
		}
		return nil, errors.New("the panel uses the default datasource and none is set")
	}
	for i := range all {
		if all[i].UID == ref {
			return &all[i], nil
		}
	}
	for i := range all {
		if strings.EqualFold(all[i].Name, ref) {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("datasource %q is not visible to the service account: it does not exist, or its permissions exclude the account", ref)
}

// pickDatasource chooses what Grafana would for a datasource variable with no
// saved selection: the default if it matches, else the first match.
func (c *Client) pickDatasource(ctx context.Context, pluginType, regex string) (*Datasource, error) {
	all, err := c.datasources.Get(ctx)
	if err != nil {
		return nil, err
	}
	var re *regexp.Regexp
	if p := strings.Trim(strings.TrimSpace(regex), "/"); p != "" {
		re, _ = regexp.Compile(p)
	}
	var matches []*Datasource
	for i := range all {
		d := &all[i]
		if pluginType != "" && d.Type != pluginType {
			continue
		}
		if re != nil && !re.MatchString(d.Name) {
			continue
		}
		if d.IsDefault {
			return d, nil
		}
		matches = append(matches, d)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no visible datasource of type %q", pluginType)
	}
	return matches[0], nil
}
