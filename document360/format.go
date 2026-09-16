package document360

import (
	"fmt"
	"sort"
	"strings"

	"github.com/justmike1/arbetern/internal/text"
)

const (
	// maxArticleChars caps the body of a single article read on its own.
	maxArticleChars = 12000
	// minBatchArticleChars floors the per-article budget in a batch read, so
	// asking for five articles still returns something usable of each.
	minBatchArticleChars = 3000
	// maxInlineBodyChars caps a body inlined into search results, which is
	// there to answer or to rule an article out, not to be the full read.
	maxInlineBodyChars = 3000
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

// FormatSearch renders the merged hits of one search call, inlining bodies
// when the caller asked for them.
func FormatSearch(r *SearchResult) string {
	if r == nil {
		return "No results."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Document360 search — %s in %s*", quotedList(r.Queries), r.Workspace.Name)
	if r.LangCode != "" {
		fmt.Fprintf(&sb, " · lang %s", r.LangCode)
	}
	sb.WriteString("\n")
	if len(r.Failed) > 0 {
		fmt.Fprintf(&sb, "_These phrasings failed and returned nothing: %s._\n", quotedList(r.Failed))
	}
	if len(r.Hits) == 0 {
		sb.WriteString("_No published articles matched. Try fewer or different keywords, or browse with document360_list_categories._\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "_%d article(s)", len(r.Hits))
	if r.Duplicates > 0 {
		fmt.Fprintf(&sb, " · %d duplicate hit(s) merged", r.Duplicates)
	}
	if r.Pagination.HasMore {
		sb.WriteString(" · more pages available (pass page+1)")
	}
	sb.WriteString("._\n")

	for i, h := range r.Hits {
		fmt.Fprintf(&sb, "\n%d. *%s* — article_id `%s`", i+1, text.Truncate(h.Title, 120), h.ArticleID)
		if h.CategoryID != "" {
			fmt.Fprintf(&sb, " · category `%s`", h.CategoryID)
		}
		sb.WriteString("\n")
		if h.URL != "" {
			fmt.Fprintf(&sb, "   <%s|Open in Document360>\n", h.URL)
		}
		switch {
		case h.Body != "":
			body := text.Truncate(strings.TrimSpace(h.Body), maxInlineBodyChars)
			fmt.Fprintf(&sb, "%s\n", indentLines(body, "   "))
			if len(h.Body) > maxInlineBodyChars {
				fmt.Fprintf(&sb, "   _…excerpt only; document360_get_article on `%s` returns the full article._\n", h.ArticleID)
			}
		case h.BodyNote != "":
			fmt.Fprintf(&sb, "   _%s_\n", h.BodyNote)
		}
	}
	if r.ContentRead == 0 {
		sb.WriteString("\n_Titles only. Re-run with include_content=true, or call document360_get_article with up to 5 article_ids at once, to read the text._\n")
	} else if r.ContentRead < len(r.Hits) {
		fmt.Fprintf(&sb, "\n_Bodies shown for the top %d hit(s). Pass the remaining article_ids to document360_get_article in ONE call if you need them._\n", r.ContentRead)
	}
	return sb.String()
}

// FormatArticleBatch renders several articles read in one call, keeping each
// failure next to the ID that caused it. The per-article budget shrinks as
// the batch grows so a batch of five cannot flood the context.
func FormatArticleBatch(results []ArticleResult) string {
	if len(results) == 0 {
		return "No articles."
	}
	if len(results) == 1 {
		if results[0].Err != nil {
			return fmt.Sprintf("Error reading article `%s`: %v", results[0].ID, results[0].Err)
		}
		return FormatArticle(results[0].Article)
	}
	budget := maxArticleChars / len(results)
	if budget < minBatchArticleChars {
		budget = minBatchArticleChars
	}
	var sb strings.Builder
	ok := 0
	for _, r := range results {
		if r.Err == nil {
			ok++
		}
	}
	fmt.Fprintf(&sb, "*Document360 — %d of %d article(s) read*\n", ok, len(results))
	for _, r := range results {
		sb.WriteString("\n───\n")
		if r.Err != nil {
			fmt.Fprintf(&sb, "*`%s`* — could not be read: %v\n", r.ID, r.Err)
			continue
		}
		sb.WriteString(formatArticleBody(r.Article, budget))
	}
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
			fmt.Fprintf(&sb, "_No articles in that category, out of %d in the workspace. Check the category id with document360_list_categories._\n", l.Scanned)
		} else {
			sb.WriteString("_No articles on this page._\n")
		}
		return sb.String()
	}
	if l.CategoryID == "" {
		fmt.Fprintf(&sb, "%s\n", pageLine(l.Pagination, len(l.Articles)))
	} else {
		fmt.Fprintf(&sb, "_%d match(es) out of %d article(s) in the workspace", len(l.Articles), l.Scanned)
		if l.Cached {
			sb.WriteString(", served from cache")
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

// FormatArticle renders one article's metadata and body.
func FormatArticle(a *Article) string {
	if a == nil {
		return "No article."
	}
	return formatArticleBody(a, maxArticleChars)
}

// formatArticleBody renders an article with an explicit body budget, shared
// by the single and batch readers.
func formatArticleBody(a *Article, budget int) string {
	if a == nil {
		return "No article.\n"
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
	if len(body) > budget {
		sb.WriteString(text.TruncatePlain(body, budget))
		fmt.Fprintf(&sb, "\n\n_…truncated: %d of %d characters shown. Open the article link for the rest._\n", budget, len(body))
	} else {
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	return sb.String()
}

// quotedList renders a list of phrases for a header line.
func quotedList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, q := range items {
		quoted = append(quoted, fmt.Sprintf("%q", text.Truncate(q, 120)))
	}
	return strings.Join(quoted, ", ")
}

// indentLines prefixes every line of s, so an inlined article body reads as
// part of its hit rather than as new top-level text.
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
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
