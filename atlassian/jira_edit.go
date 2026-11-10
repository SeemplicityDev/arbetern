package atlassian

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// EditOp is how a FieldEdit changes its field; EditAuto adds to a list field and sets any other.
type EditOp string

const (
	EditAuto   EditOp = ""
	EditSet    EditOp = "set"
	EditAdd    EditOp = "add"
	EditRemove EditOp = "remove"
)

// FieldEdit changes one issue field, named by id or name: Value is the new value, or one value or a list of them to add or remove.
type FieldEdit struct {
	Field string
	Op    EditOp
	Value any
}

// IssueUpdate is what UpdateIssue did; Changed is false when the issue already held every requested value.
type IssueUpdate struct {
	Key     string
	Browse  string
	Changed bool
	Fields  []EditedField
}

// EditedField is one edited field as text before and after the edit, with notes on requested values that needed no change.
type EditedField struct {
	ID, Name      string
	Before, After string
	Skipped       []string
}

// EditRejected lists why UpdateIssue refused an edit without writing to Jira.
type EditRejected struct{ Problems []string }

func (e *EditRejected) Error() string { return strings.Join(e.Problems, "; ") }

const (
	maxEditFields    = 20
	maxListedOptions = 30
	maxLabelLength   = 255
)

// UpdateIssue applies the edits in one write and reads the edited fields back; list fields are added to and removed from, never replaced.
func (c *Client) UpdateIssue(issueKey string, edits []FieldEdit) (*IssueUpdate, error) {
	issueKey = strings.TrimSpace(issueKey)
	if issueKey == "" {
		return nil, &EditRejected{[]string{"an issue key is required"}}
	}
	cat := c.fieldCatalog()
	var ids, problems []string
	byID := map[string][]FieldEdit{}
	for _, e := range edits {
		ref := strings.TrimSpace(e.Field)
		if ref == "" || len(ref) > maxFieldRef || strings.ContainsFunc(ref, unicode.IsControl) {
			problems = append(problems, fmt.Sprintf("%q is not a field id or name", e.Field))
			continue
		}
		id, problem := cat.resolve(ref)
		if problem != "" {
			problems = append(problems, ref+": "+problem)
			continue
		}
		if _, seen := byID[id]; !seen {
			ids = append(ids, id)
		}
		byID[id] = append(byID[id], e)
	}
	switch {
	case len(problems) > 0:
		return nil, &EditRejected{problems}
	case len(ids) == 0:
		return nil, &EditRejected{[]string{"no field to change"}}
	case len(ids) > maxEditFields:
		return nil, &EditRejected{[]string{fmt.Sprintf("at most %d fields per update", maxEditFields)}}
	}

	before, err := c.issueState(issueKey, ids, true)
	if err != nil {
		return nil, err
	}
	res := &IssueUpdate{Key: before.key, Browse: fmt.Sprintf("%s/browse/%s", c.siteURL, before.key)}
	set := map[string]any{}
	update := map[string][]map[string]any{}
	fields := make([]editField, len(ids))
	for i, id := range ids {
		m, editable := before.meta[id]
		f := newEditField(id, m, editable, cat)
		cur, returned := before.values[id]
		p := f.plan(byID[id], cur)
		problems = append(problems, p.problems...)
		switch {
		case p.hasSet:
			set[id] = p.set
		case len(p.ops) > 0:
			update[id] = p.ops
		}
		fields[i] = f
		res.Fields = append(res.Fields, EditedField{ID: id, Name: f.name, Before: f.text(cur, returned), Skipped: p.notes})
	}
	if len(problems) > 0 {
		return nil, &EditRejected{problems}
	}
	if len(set) == 0 && len(update) == 0 {
		for i := range res.Fields {
			res.Fields[i].After = res.Fields[i].Before
		}
		return res, nil
	}

	body := map[string]any{}
	if len(set) > 0 {
		body["fields"] = set
	}
	if len(update) > 0 {
		body["update"] = update
	}
	if err := c.jiraJSON(http.MethodPut, "/rest/api/3/issue/"+url.PathEscape(res.Key), body, nil); err != nil {
		if apiErr, ok := errors.AsType[*jiraAPIError](err); ok && apiErr.status < http.StatusInternalServerError {
			return nil, fmt.Errorf("jira refused the edit (HTTP %d), so nothing changed: %s", apiErr.status, jiraErrorText(apiErr.body))
		}
		return nil, err
	}
	res.Changed = true
	after, err := c.issueState(res.Key, ids, false)
	for i, f := range fields {
		if err != nil {
			res.Fields[i].After = "(could not read it back: " + err.Error() + ")"
			continue
		}
		v, returned := after.values[f.id]
		res.Fields[i].After = f.text(v, returned)
	}
	return res, nil
}

type issueState struct {
	key    string
	values map[string]any
	meta   map[string]editMeta
}

func (c *Client) issueState(issueKey string, ids []string, withMeta bool) (*issueState, error) {
	q := url.Values{"fields": {strings.Join(ids, ",")}}
	if withMeta {
		q.Set("expand", "editmeta")
	}
	var issue struct {
		Key      string                     `json:"key"`
		Fields   map[string]json.RawMessage `json:"fields"`
		Editmeta struct {
			Fields map[string]editMeta `json:"fields"`
		} `json:"editmeta"`
	}
	if err := c.jiraJSON(http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(issueKey)+"?"+q.Encode(), nil, &issue); err != nil {
		return nil, fmt.Errorf("read %s: %w", issueKey, err)
	}
	st := &issueState{key: cmp.Or(issue.Key, issueKey), values: make(map[string]any, len(issue.Fields)), meta: issue.Editmeta.Fields}
	for id, raw := range issue.Fields {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		_ = dec.Decode(&v)
		st.values[id] = v
	}
	return st, nil
}

type editMeta struct {
	Name          string      `json:"name"`
	Schema        fieldSchema `json:"schema"`
	Operations    []string    `json:"operations"`
	AllowedValues []any       `json:"allowedValues"`
}

type editField struct {
	id, name string
	schema   fieldSchema
	ops      []string
	allowed  []any
	known    bool
}

func newEditField(id string, m editMeta, editable bool, cat *fieldCatalog) editField {
	if editable {
		return editField{id: id, name: cmp.Or(m.Name, id), schema: m.Schema, ops: m.Operations, allowed: m.AllowedValues, known: true}
	}
	// Edit metadata covers only the issue's edit screen, but Jira accepts edits to other fields too.
	f := editField{id: id, name: id}
	if jf, ok := cat.field(id); ok {
		f.name = cmp.Or(jf.Name, id)
		if jf.Schema != nil {
			f.schema, f.known = *jf.Schema, true
		}
	}
	return f
}

func (f editField) label() string {
	if strings.EqualFold(f.name, f.id) {
		return f.name
	}
	return f.name + " (" + f.id + ")"
}

func (f editField) multi() bool {
	// Sprint reads as a list but takes a single sprint id.
	return (f.schema.Type == "array" || f.labels()) && !strings.HasSuffix(f.schema.Custom, ":gh-sprint")
}

func (f editField) itemType() string {
	if f.schema.Type == "array" {
		return f.schema.Items
	}
	return f.schema.Type
}

func (f editField) can(op EditOp) bool {
	if f.ops != nil {
		return slices.Contains(f.ops, string(op))
	}
	return !f.known || op == EditSet || f.multi()
}

func (f editField) allowedOps() string {
	switch {
	case f.ops != nil && len(f.ops) == 0:
		return "no edits"
	case f.ops != nil:
		return strings.Join(f.ops, ", ")
	}
	return "set"
}

func (f editField) mergeable() bool { return f.multi() && f.can(EditAdd) }

func (f editField) system() string { return cmp.Or(f.schema.System, f.id) }

func (f editField) richText() bool {
	return f.system() == "description" || f.system() == "environment" || strings.HasSuffix(f.schema.Custom, ":textarea")
}

func (f editField) labels() bool {
	return f.system() == "labels" || strings.HasSuffix(f.schema.Custom, ":labels")
}

func (f editField) team() bool {
	return f.schema.Type == "team" || strings.Contains(strings.ToLower(f.schema.Custom), "teams")
}

func (f editField) autoOp(v any) EditOp {
	if v != nil && (f.mergeable() || !f.known && isList(v)) {
		return EditAdd
	}
	return EditSet
}

type fieldPlan struct {
	set      any
	hasSet   bool
	ops      []map[string]any
	notes    []string
	problems []string
}

func (f editField) plan(edits []FieldEdit, cur any) fieldPlan {
	var p fieldPlan
	have := slices.Clone(listOf(cur))
	var present, absent []string
	sets := 0
	for _, e := range edits {
		op := cmp.Or(e.Op, f.autoOp(e.Value))
		switch op {
		case EditSet:
			sets++
			switch v := e.Value; {
			case sets > 1:
				p.problems = append(p.problems, f.label()+" was given more than one new value")
			case f.mergeable() || isList(v) && !f.known:
				p.problems = append(p.problems, f.label()+" is a list: add or remove values instead, since replacing the whole list is not supported")
			case isList(v):
				p.problems = append(p.problems, f.label()+" takes a single value, not a list")
			case !f.can(EditSet):
				p.problems = append(p.problems, fmt.Sprintf("%s cannot be set; Jira allows %s", f.label(), f.allowedOps()))
			case v == nil && isEmpty(cur):
				p.notes = append(p.notes, "already empty")
			case v == nil:
				p.set, p.hasSet = nil, true
			default:
				write, match, err := f.normalize(v)
				switch {
				case err != nil:
					p.problems = append(p.problems, err.Error())
				case match != nil && len(matches([]any{cur}, match, equal)) > 0:
					p.notes = append(p.notes, "already set to this value")
				default:
					p.set, p.hasSet = write, true
				}
			}
		case EditAdd, EditRemove:
			if !f.can(op) {
				p.problems = append(p.problems, fmt.Sprintf("%s does not take %s; Jira allows %s", f.label(), op, f.allowedOps()))
				continue
			}
			for _, v := range listOf(e.Value) {
				if op == EditRemove {
					found := matches(have, v, equal)
					if len(found) == 0 {
						found = matches(have, v, strings.EqualFold)
					}
					switch len(found) {
					case 0:
						absent = append(absent, valueLabel(v))
					case 1:
						p.ops = append(p.ops, map[string]any{"remove": f.writeForm(found[0])})
					default:
						p.problems = append(p.problems, fmt.Sprintf("%s: %s matches several values (%s); name one exactly", f.label(), quoted(v), quotedList(found)))
					}
					continue
				}
				write, match, err := f.normalize(v)
				switch {
				case err != nil:
					p.problems = append(p.problems, err.Error())
				case match != nil && len(matches(have, match, equal)) > 0:
					present = append(present, valueLabel(match))
				default:
					have = append(have, match)
					p.ops = append(p.ops, map[string]any{"add": write})
				}
			}
		default:
			p.problems = append(p.problems, fmt.Sprintf("%s: %q is not an operation; use add, remove or set", f.label(), op))
		}
	}
	if len(present) > 0 {
		p.notes = append(p.notes, "already there: "+strings.Join(present, ", "))
	}
	if len(absent) > 0 {
		p.notes = append(p.notes, "not there: "+strings.Join(absent, ", "))
	}
	if p.hasSet && len(p.ops) > 0 {
		p.problems = append(p.problems, f.label()+" was both set and added to or removed from")
	}
	return p
}

func (f editField) normalize(v any) (write, match any, err error) {
	s, isStr := v.(string)
	switch {
	case f.team():
		id := teamIDOf(v)
		if id == "" || strings.ContainsFunc(id, unicode.IsSpace) {
			return nil, nil, fmt.Errorf("%s takes a team id from resolve_jira_team, not %s", f.label(), quoted(v))
		}
		return id, id, nil
	case len(f.allowed) > 0:
		return f.pick(v)
	case f.richText():
		if isStr {
			return textToADF(s), nil, nil
		}
		return v, nil, nil
	case f.labels():
		if !isStr || s == "" || len(s) > maxLabelLength || strings.ContainsFunc(s, unicode.IsSpace) {
			return nil, nil, fmt.Errorf("%s: %s is not a valid label; Jira labels are 1 to %d characters without spaces", f.label(), quoted(v), maxLabelLength)
		}
		return s, s, nil
	case f.itemType() == "user":
		id := s
		if m, ok := v.(map[string]any); ok {
			id, _ = m["accountId"].(string)
		}
		if id == "" || strings.ContainsFunc(id, unicode.IsSpace) {
			return nil, nil, fmt.Errorf("%s takes a Jira accountId from resolve_jira_user, not %s", f.label(), quoted(v))
		}
		return map[string]any{"accountId": id}, id, nil
	case !isStr:
		return v, v, nil
	case f.itemType() == "number":
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			return json.Number(s), json.Number(s), nil
		}
	}
	write = f.fromString(s)
	return write, write, nil
}

func (f editField) pick(v any) (write, match any, err error) {
	opt, err := pickOption(f.allowed, v, f.label())
	if err != nil {
		return nil, nil, err
	}
	m, _ := v.(map[string]any)
	if m["child"] == nil {
		return f.writeForm(opt), opt, nil
	}
	parent, _ := opt.(map[string]any)
	children, _ := parent["children"].([]any)
	kid, err := pickOption(children, m["child"], f.label()+" option "+quoted(opt))
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"value": valueLabel(opt), "child": f.writeForm(kid)}, nil, nil
}

func pickOption(cands []any, v any, of string) (any, error) {
	found := matches(cands, v, equal)
	if len(found) == 0 {
		found = matches(cands, v, strings.EqualFold)
	}
	switch {
	case len(found) == 0:
		return nil, fmt.Errorf("%s has no option %s%s", of, quoted(v), optionHint(cands, v))
	case len(found) > 1:
		return nil, fmt.Errorf("%s: %s matches several options (%s); give one by id", of, quoted(v), quotedList(found))
	}
	if m, ok := found[0].(map[string]any); ok && m["disabled"] == true {
		return nil, fmt.Errorf("%s option %s is disabled", of, quoted(found[0]))
	}
	return found[0], nil
}

func optionHint(cands []any, v any) string {
	want := squash(valueLabel(v))
	var names, near []any
	for _, c := range cands {
		if m, ok := c.(map[string]any); ok && m["disabled"] == true {
			continue
		}
		names = append(names, c)
		if have := squash(valueLabel(c)); min(len(want), len(have)) >= 3 && (strings.Contains(have, want) || strings.Contains(want, have)) {
			near = append(near, c)
		}
	}
	switch {
	case len(near) > 0 && len(near) <= 5:
		return "; did you mean " + quotedList(near) + "?"
	case len(names) == 0:
		return ""
	case len(names) > maxListedOptions:
		return fmt.Sprintf("; allowed values include %s and %d more", quotedList(names[:maxListedOptions]), len(names)-maxListedOptions)
	}
	return "; allowed values: " + quotedList(names)
}

func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

func (f editField) fromString(s string) any {
	switch f.itemType() {
	case "option", "option-with-child":
		return map[string]any{"value": s}
	case "priority", "component", "version", "group", "securitylevel", "resolution", "issuetype":
		return map[string]any{"name": s}
	case "user":
		return map[string]any{"accountId": s}
	}
	if f.id == "parent" {
		return map[string]any{"key": s}
	}
	return s
}

func (f editField) writeForm(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range []string{"accountId", "value", "name", "key", "id"} {
			if s, ok := t[k].(string); ok && s != "" {
				return map[string]any{k: s}
			}
		}
	case string:
		return f.fromString(t)
	}
	return v
}

func (f editField) text(v any, returned bool) string {
	if !returned {
		return "(Jira did not return this field)"
	}
	if id := teamIDOf(v); f.team() && id != "" {
		return withID(fieldText(v), id)
	}
	return editedText(v)
}

func editedText(v any) string {
	switch t := v.(type) {
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := editedText(e); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return jsonText(parts)
	case map[string]any:
		if id, _ := t["accountId"].(string); id != "" {
			name, _ := t["displayName"].(string)
			return withID(name, id)
		}
	}
	return fieldText(v)
}

func withID(name, id string) string {
	if name == "" || name == id {
		return id
	}
	return name + " (" + id + ")"
}

func matches(cands []any, v any, same func(a, b string) bool) []any {
	want := identities(v)
	var out []any
	for _, c := range cands {
		if slices.ContainsFunc(identities(c), func(have string) bool {
			return slices.ContainsFunc(want, func(w string) bool { return same(w, have) })
		}) {
			out = append(out, c)
		}
	}
	return out
}

func equal(a, b string) bool { return a == b }

func identities(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case json.Number, float64, bool:
		return []string{fmt.Sprint(t)}
	case map[string]any:
		var out []string
		for _, k := range []string{"accountId", "key", "id", "value", "name", "displayName"} {
			switch s := t[k].(type) {
			case string:
				if s != "" {
					out = append(out, s)
				}
			case json.Number, float64:
				out = append(out, fmt.Sprint(s))
			}
		}
		return out
	}
	return nil
}

func valueLabel(v any) string {
	if m, ok := v.(map[string]any); ok {
		for _, k := range []string{"value", "name", "displayName", "key", "accountId", "id"} {
			if s, ok := m[k].(string); ok && s != "" {
				return s
			}
		}
	}
	if s, ok := v.(string); ok {
		return s
	}
	return jsonText(v)
}

func quoted(v any) string { return strconv.Quote(valueLabel(v)) }

func quotedList(vs []any) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = quoted(v)
	}
	return strings.Join(out, ", ")
}

func teamIDOf(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case map[string]any:
		for _, k := range []string{"id", "teamId"} {
			if s, ok := t[k].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func listOf(v any) []any {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		return t
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	}
	return []any{v}
}

func isList(v any) bool {
	switch v.(type) {
	case []any, []string:
		return true
	}
	return false
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	}
	return false
}
