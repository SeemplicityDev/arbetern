package projects

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/justmike1/arbetern/github"
)

const maxListedPlaceholders = 5

var (
	angleSpanRe    = regexp.MustCompile(`<[^<>\n]+>`)
	htmlTagRe      = regexp.MustCompile(`^</?([a-zA-Z][a-zA-Z0-9]*)(?:\s[^<>]*)?/?>$`)
	autolinkRe     = regexp.MustCompile(`^<[a-zA-Z][a-zA-Z0-9+.-]*:[^\s<>]*>$`)
	prTemplateDirs = []string{".github", "", "docs"}
	htmlElements   = map[string]bool{
		"a": true, "abbr": true, "b": true, "blockquote": true, "br": true, "caption": true, "cite": true, "code": true,
		"dd": true, "del": true, "details": true, "div": true, "dl": true, "dt": true, "em": true, "figcaption": true,
		"figure": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "hr": true, "i": true,
		"img": true, "ins": true, "kbd": true, "li": true, "mark": true, "ol": true, "p": true, "picture": true,
		"pre": true, "q": true, "s": true, "samp": true, "small": true, "source": true, "span": true, "strike": true,
		"strong": true, "sub": true, "summary": true, "sup": true, "table": true, "tbody": true, "td": true,
		"tfoot": true, "th": true, "thead": true, "time": true, "tr": true, "tt": true, "u": true, "ul": true,
		"var": true, "video": true, "wbr": true,
	}
)

func (r *Registry) fetchPRTemplate(ctx context.Context, key string) (string, error) {
	if r.gh == nil {
		return "", errors.New("GitHub is not configured")
	}
	owner, name, _ := strings.Cut(key, "/")
	for _, dir := range prTemplateDirs {
		entries, err := r.gh.GetDirectoryContents(ctx, owner, name, dir, "")
		switch {
		case github.IsNotFound(err):
			continue
		case err != nil:
			return "", err
		}
		for _, e := range entries {
			if !strings.HasSuffix(e, "/") && isPRTemplate(path.Base(e)) {
				content, _, err := r.gh.GetFileContent(ctx, owner, name, e, "")
				return content, err
			}
		}
	}
	return "", nil
}

func isPRTemplate(file string) bool {
	stem, ext, _ := strings.Cut(strings.ToLower(file), ".")
	return stem == "pull_request_template" && (ext == "" || ext == "md" || ext == "txt")
}

func (g *gateway) placeholderProblem(ctx context.Context, p *Project, repo Repo, field, text string) string {
	if p.Conventions.Description != descriptionSession || repo.Name == "" {
		return ""
	}
	tmpl, err := g.r.prTemplates.Get(ctx, strings.ToLower(repo.Owner+"/"+repo.Name))
	if err != nil {
		log.Printf("[projects] %s: reading the pull request template of %s/%s: %v", p.ID, repo.Owner, repo.Name, err)
		return ""
	}
	left := leftPlaceholders(tmpl, text)
	if len(left) == 0 {
		return ""
	}
	shown := make([]string, 0, maxListedPlaceholders)
	for _, s := range left[:min(len(left), maxListedPlaceholders)] {
		shown = append(shown, codeSpan(oneLine(s, 80)))
	}
	list := strings.Join(shown, ", ")
	if more := len(left) - len(shown); more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return field + " still holds placeholders from the repository's pull request template: " + list +
		". Replace each with this change's own text, or with what the template allows where a part does not apply (such as \"none\" or \"N/A — reason\"), then call report_outcome again."
}

func leftPlaceholders(tmpl, text string) []string {
	text = htmlCommentRe.ReplaceAllString(text, "")
	var left []string
	for _, span := range angleSpanRe.FindAllString(htmlCommentRe.ReplaceAllString(tmpl, ""), -1) {
		if isPlaceholder(span) && strings.Contains(text, span) && !slices.Contains(left, span) {
			left = append(left, span)
		}
	}
	return left
}

func isPlaceholder(span string) bool {
	if m := htmlTagRe.FindStringSubmatch(span); m != nil && htmlElements[strings.ToLower(m[1])] {
		return false
	}
	return !autolinkRe.MatchString(span)
}
