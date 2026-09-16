package document360

import "encoding/json"

// envelope is the v3 response wrapper shared by every endpoint.
type envelope struct {
	Data       json.RawMessage `json:"data"`
	Pagination *Pagination     `json:"pagination"`
	Success    bool            `json:"success"`
	RequestID  string          `json:"request_id"`
	Errors     []apiErrorItem  `json:"errors"`
}

type apiErrorItem struct {
	Code        string `json:"code"`
	ErrorCode   string `json:"error_code"`
	Message     string `json:"message"`
	Description string `json:"description"`
}

// problem is the RFC 7807 body the API returns on a non-2xx status.
type problem struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Title   string         `json:"title"`
	Detail  string         `json:"detail"`
	TraceID string         `json:"trace_id"`
	Errors  []apiErrorItem `json:"errors"`
}

// Pagination describes one page of a list response.
type Pagination struct {
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
	TotalCount *int   `json:"total_count"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

// Project is a Document360 project (one knowledge base portal).
type Project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	SubDomainName string `json:"sub_domain_name"`
	Status        int    `json:"status"`
	CreatedAt     string `json:"created_at"`
	ModifiedAt    string `json:"modified_at"`
}

// Workspace is one version/workspace of a project's knowledge base.
type Workspace struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	Order         int    `json:"order"`
	IsDefault     bool   `json:"is_default"`
	WorkspaceType string `json:"workspace_type"`
	CreatedAt     string `json:"created_at"`
	ModifiedAt    string `json:"modified_at"`
}

// Category is a node in a workspace's category tree.
type Category struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Order            int        `json:"order"`
	ParentCategoryID string     `json:"parent_category_id"`
	ChildCategories  []Category `json:"child_categories"`
}

// ArticleSummary is one row of a workspace article listing.
type ArticleSummary struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	CategoryID    string `json:"category_id"`
	Hidden        bool   `json:"hidden"`
	Order         int    `json:"order"`
	Status        string `json:"status"`
	LatestVersion int    `json:"latest_version"`
	PublicVersion int    `json:"public_version"`
	ModifiedAt    string `json:"modified_at"`
}

// SearchHit is one keyword-search match.
type SearchHit struct {
	ArticleID  string `json:"article_id"`
	Title      string `json:"title"`
	CategoryID string `json:"category_id"`
	Slug       string `json:"slug"`
	Version    int    `json:"version"`
}

// SearchResult is one page of search hits with the workspace it ran in.
type SearchResult struct {
	Query      string
	Workspace  Workspace
	LangCode   string
	Hits       []SearchHit
	Pagination Pagination
}

// ArticleList is one page of article summaries, optionally narrowed to a
// category on the client side.
type ArticleList struct {
	Workspace    Workspace
	CategoryID   string
	Articles     []ArticleSummary
	Pagination   Pagination
	PagesScanned int
}

// Author is an article contributor.
type Author struct {
	ID        string `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
}

// ArticleLanguage is one translation an article is available in.
type ArticleLanguage struct {
	LangCode          string `json:"lang_code"`
	URL               string `json:"url"`
	TranslationStatus string `json:"translation_status"`
}

// Article is the full detail of one article in one language.
type Article struct {
	ID                 string            `json:"id"`
	Title              string            `json:"title"`
	Content            string            `json:"content"`
	HTMLContent        string            `json:"html_content"`
	CategoryID         string            `json:"category_id"`
	WorkspaceID        string            `json:"workspace_id"`
	VersionNumber      int               `json:"version_number"`
	PublicVersion      int               `json:"public_version"`
	LatestVersion      int               `json:"latest_version"`
	Hidden             bool              `json:"hidden"`
	Status             string            `json:"status"`
	Slug               string            `json:"slug"`
	Description        string            `json:"description"`
	ContentType        string            `json:"content_type"`
	URL                string            `json:"url"`
	PreviewURL         string            `json:"preview_url"`
	LangCode           string            `json:"lang_code"`
	CreatedAt          string            `json:"created_at"`
	ModifiedAt         string            `json:"modified_at"`
	Authors            []Author          `json:"authors"`
	AvailableLanguages []ArticleLanguage `json:"available_languages"`
	CustomFieldsData   map[string]any    `json:"custom_fields_data"`
}
