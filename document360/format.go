package document360

import (
	"fmt"
	"sort"
	"strings"

	"github.com/justmike1/arbetern/internal/text"
)

const (
	// maxArticleChars caps the article body returned to the model.
	maxArticleChars = 12000
	// maxTreeNodes caps how many categories a tree render lists.
	maxTreeNodes = 300
)

// FormatWorkspaces renders the workspace list with the IDs other tools take.
func FormatWorkspaces(project string, list []Workspace) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Document360 workspaces — %s (%d)*\n", project, len(list))
	if len(list) == 0 {
		sb.WriteString("_No workspaces are visible to this API key._\n")
		return sb.String()
	}
	for _, w := range list {
		marker := ""
		if w.IsDefault {
			marker = " _(default)_"
		}
		fmt.Fprintf(&sb, "• *%s*%s — id `%s`", text.Truncate(w.Name, 80), marker, w.ID)
		if w.Slug != "" {
			fmt.Fprintf(&sb, " · slug `%s`", w.Slug)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("_Omit workspace_id in other tools to use the default workspace._\n")
	return sb.String()
}

// FormatSearch renders one page of search hits.
func FormatSearch(r *SearchResult) string {
	if r == nil {
		return "No results."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Document360 search — %q in %s*", text.Truncate(r.Query, 120), r.Workspace.Name)
	if r.LangCode != "" {
		fmt.Fprintf(&sb, " · lang %s", r.LangCode)
	}
	sb.WriteString("\n")
	if len(r.Hits) == 0 {
		sb.WriteString("_No published articles matched. Try fewer or different keywords, or browse with document360_list_categories._\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "%s\n", pageLine(r.Pagination, len(r.Hits)))
	for i, h := range r.Hits {
		fmt.Fprintf(&sb, "%d. *%s* — article_id `%s`", (r.Pagination.Page-1)*r.Pagination.PageSize+i+1, text.Truncate(h.Title, 120), h.ArticleID)
		if h.CategoryID != "" {
			fmt.Fprintf(&sb, " · category `%s`", h.CategoryID)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("_Use document360_get_article with an article_id to read the content._\n")
	return sb.String()
}

// FormatCategories renders the category tree, indented by depth.
func FormatCategories(ws Workspace, tree []Category) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Document360 categories — %s*\n", ws.Name)
	if len(tree) == 0 {
		sb.WriteString("_No categories are visible to this API key._\n")
		return sb.String()
	}
	count := 0
	var walk func(nodes []Category, depth int)
	walk = func(nodes []Category, depth int) {
		sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Order < nodes[j].Order })
		for _, n := range nodes {
			if count >= maxTreeNodes {
				return
			}
			count++
			fmt.Fprintf(&sb, "%s• %s — `%s`\n", strings.Repeat("    ", depth), text.Truncate(n.Name, 100), n.ID)
			walk(n.ChildCategories, depth+1)
		}
	}
	walk(tree, 0)
	if count >= maxTreeNodes {
		fmt.Fprintf(&sb, "_…tree truncated at %d categories._\n", maxTreeNodes)
	}
	sb.WriteString("_Pass a category id to document360_list_articles to list its articles._\n")
	return sb.String()
}

// FormatArticles renders one page of article summaries.
func FormatArticles(l *ArticleList) string {
	if l == nil {
		return "No articles."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Document360 articles — %s*", l.Workspace.Name)
	if l.CategoryID != "" {
		fmt.Fprintf(&sb, " · category `%s`", l.CategoryID)
	}
	sb.WriteString("\n")
	if len(l.Articles) == 0 {
		if l.CategoryID != "" {
			fmt.Fprintf(&sb, "_No articles in that category across the %d page(s) scanned._\n", l.PagesScanned)
		} else {
			sb.WriteString("_No articles on this page._\n")
		}
		return sb.String()
	}
	if l.CategoryID == "" {
		fmt.Fprintf(&sb, "%s\n", pageLine(l.Pagination, len(l.Articles)))
	} else {
		fmt.Fprintf(&sb, "_%d match(es) across %d page(s) scanned", len(l.Articles), l.PagesScanned)
		if l.Pagination.HasMore {
			sb.WriteString("; more pages exist beyond the scan limit")
		}
		sb.WriteString("._\n")
	}
	for _, a := range l.Articles {
		flags := a.Status
		if a.Hidden {
			flags += ", hidden"
		}
		fmt.Fprintf(&sb, "• *%s* — article_id `%s` _(%s · v%d · updated %s)_\n",
			text.Truncate(a.Title, 120), a.ID, flags, a.PublicVersion, shortDate(a.ModifiedAt))
	}
	return sb.String()
}

// FormatArticle renders an article's metadata and body.
func FormatArticle(a *Article) string {
	if a == nil {
		return "No article."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%s*\n", text.Truncate(strings.TrimSpace(a.Title), 200))
	meta := []string{fmt.Sprintf("article_id `%s`", a.ID)}
	if a.Status != "" {
		meta = append(meta, a.Status)
	}
	if a.PublicVersion > 0 {
		meta = append(meta, fmt.Sprintf("v%d", a.PublicVersion))
	}
	if a.LangCode != "" {
		meta = append(meta, "lang "+a.LangCode)
	}
	if a.ModifiedAt != "" {
		meta = append(meta, "updated "+shortDate(a.ModifiedAt))
	}
	if a.Hidden {
		meta = append(meta, "hidden from readers")
	}
	fmt.Fprintf(&sb, "_%s_\n", strings.Join(meta, " · "))
	if a.URL != "" {
		fmt.Fprintf(&sb, "<%s|Open in Document360>\n", a.URL)
	}
	if desc := strings.TrimSpace(a.Description); desc != "" {
		fmt.Fprintf(&sb, "\n_%s_\n", text.Truncate(desc, 400))
	}
	if len(a.AvailableLanguages) > 0 {
		codes := make([]string, 0, len(a.AvailableLanguages))
		for _, l := range a.AvailableLanguages {
			if l.LangCode != "" {
				codes = append(codes, l.LangCode)
			}
		}
		if len(codes) > 0 {
			fmt.Fprintf(&sb, "Also available in: %s\n", strings.Join(codes, ", "))
		}
	}
	body := a.PlainContent()
	if body == "" {
		sb.WriteString("\n_The article has no readable body (it may be a folder page or unpublished)._\n")
		return sb.String()
	}
	sb.WriteString("\n")
	if len(body) > maxArticleChars {
		sb.WriteString(text.TruncatePlain(body, maxArticleChars))
		fmt.Fprintf(&sb, "\n\n_…truncated: %d of %d characters shown. Open the article link for the rest._\n", maxArticleChars, len(body))
	} else {
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	return sb.String()
}

func pageLine(p Pagination, shown int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "_Page %d · %d shown", maxInt(p.Page, 1), shown)
	if p.TotalCount != nil {
		fmt.Fprintf(&sb, " of %d", *p.TotalCount)
	}
	if p.HasMore {
		sb.WriteString(" · more pages available (pass page+1)")
	}
	sb.WriteString("._")
	return sb.String()
}

func shortDate(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	if ts == "" {
		return "unknown"
	}
	return ts
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
