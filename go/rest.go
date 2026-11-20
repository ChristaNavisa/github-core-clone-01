// REST v3: repos, refs, branches, commits, contents, labels, issues,
// comments, pull requests. GitHub-shaped JSON; every reply carries
// X-GitHub-Request-Id (added by app.go) and an error JSON body carries
// `message`.

package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// restRequest is everything a REST handler needs from the HTTP layer.
type restRequest struct {
	method   string
	segments []string
	query    map[string]string // first value per key, percent-decoded
	json     any               // parsed request body (nil when absent)
	jsonErr  bool              // body present but not valid JSON
	host     string
}

// body helpers: the reference reads fields off (json ?? {}); a non-object
// body yields undefined for every field.
func bodyMap(req restRequest) map[string]any {
	if m, ok := req.json.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func bodyString(req restRequest, key string) (string, bool) {
	v, ok := bodyMap(req)[key].(string)
	return v, ok
}

func bodyBool(req restRequest, key string) (bool, bool) {
	v, ok := bodyMap(req)[key].(bool)
	return v, ok
}

// jsParseInt mirrors JavaScript's parseInt(s, 10): leading whitespace and an
// optional sign, then decimal digits; no digits at all means NaN.
func jsParseInt(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n\v\f\r")
	if s == "" {
		return 0, false
	}
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		s = s[1:]
	}
	n := 0
	digits := 0
	for len(s) > 0 && s[0] >= '0' && s[0] <= '9' {
		n = n*10 + int(s[0]-'0')
		s = s[1:]
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// encodeURIComponent percent-encodes every byte except the unreserved set
// A-Z a-z 0-9 - _ . ! ~ * ' ( ), exactly like JavaScript's.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '!' || c == '~' || c == '*' || c == '\'' || c == '(' || c == ')' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// lenientBase64 mirrors Node's Buffer.from(s, 'base64'): characters outside
// the alphabet are ignored, missing padding is tolerated.
func lenientBase64(s string) ([]byte, error) {
	var filtered []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			filtered = append(filtered, c)
		}
	}
	for len(filtered)%4 != 0 {
		filtered = append(filtered, '=')
	}
	return base64.StdEncoding.DecodeString(string(filtered))
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------------------------
// JSON shapes
// ---------------------------------------------------------------------------------------------

func viewerJSON(f *Forge) map[string]any {
	return map[string]any{
		"login": viewerLogin, "id": viewerID, "node_id": f.globalID("User", viewerLogin),
		"avatar_url": fmt.Sprintf("https://avatars.githubusercontent.com/u/%d", viewerID),
		"gravatar_id": "", "url": "/api/v3/users/" + viewerLogin,
		"html_url": "https://github.com/" + viewerLogin, "type": "User", "site_admin": false,
		"name": viewerName, "company": nil, "blog": "", "location": nil, "email": viewerEmail,
		"hireable": nil, "bio": nil, "public_repos": 0, "public_gists": 0,
		"followers": 0, "following": 0, "created_at": fixedISO, "updated_at": fixedISO,
	}
}

func openIssueCount(repo *Repo) int {
	n := 0
	for _, i := range repo.Issues {
		if i.State == "open" {
			n++
		}
	}
	return n
}

func repoJSON(f *Forge, repo *Repo) map[string]any {
	owner := viewerLogin
	open := openIssueCount(repo)
	return map[string]any{
		"id": repo.ID, "node_id": f.globalID("Repository", owner, repo.Name), "name": repo.Name,
		"full_name": owner + "/" + repo.Name,
		"owner": map[string]any{"login": owner, "id": viewerID, "node_id": f.globalID("User", owner), "type": "User", "site_admin": false},
		"private": repo.Private, "html_url": "https://github.com/" + owner + "/" + repo.Name,
		"description": repo.Description, "fork": false,
		"url": "/api/v3/repos/" + owner + "/" + repo.Name,
		"created_at": fixedISO, "updated_at": fixedISO, "pushed_at": fixedISO,
		"git_url": "git://github.com/" + owner + "/" + repo.Name + ".git",
		"ssh_url": "git@github.com:" + owner + "/" + repo.Name + ".git",
		"clone_url": "https://github.com/" + owner + "/" + repo.Name + ".git",
		"homepage": nil, "size": 0, "stargazers_count": 0, "watchers_count": 0, "language": nil,
		"has_issues": true, "has_projects": false, "has_downloads": true, "has_wiki": false,
		"has_pages": false, "has_discussions": false, "forks_count": 0, "mirror_url": nil,
		"archived": false, "disabled": false, "open_issues_count": open, "license": nil,
		"allow_forking": true, "is_template": false, "topics": []any{},
		"visibility": map[bool]string{true: "private", false: "public"}[repo.Private],
		"forks": 0, "open_issues": open, "watchers": 0, "default_branch": repo.DefaultBranch,
	}
}

func repoJSONSimple(f *Forge, repo *Repo) map[string]any {
	return map[string]any{
		"id": repo.ID, "node_id": f.globalID("Repository", viewerLogin, repo.Name), "name": repo.Name,
		"full_name": viewerLogin + "/" + repo.Name, "private": repo.Private, "default_branch": repo.DefaultBranch,
	}
}

// labelID is a label's stable id: its 1-based position in the repo's label
// insertion order (0 when the label is not in the repo).
func labelID(repo *Repo, name string) int {
	for i, n := range repo.labelsOrder {
		if n == name {
			return i + 1
		}
	}
	return 0
}

func labelJSON(f *Forge, label *Label, id int) map[string]any {
	return map[string]any{
		"id": id, "node_id": f.globalID("Label", label.Name),
		"url": "/api/v3/labels/" + encodeURIComponent(label.Name),
		"name": label.Name, "color": label.Color, "default": false, "description": "",
	}
}

// Struct-shaped JSON for the issue reply: encoding/json marshals structs
// ~3x faster than maps (no key sorting, no per-key reflection). The write
// workload POSTs an issue and gets this shape back on every op, so the
// marshal cost is on the hot path. Parsed content is identical to the map
// version; only key order differs (the suite parses JSON).
type userJSONOut struct {
	Login     string `json:"login"`
	ID        int    `json:"id"`
	NodeID    string `json:"node_id"`
	Type      string `json:"type"`
	SiteAdmin bool   `json:"site_admin"`
}

type labelJSONOut struct {
	ID          int    `json:"id"`
	NodeID      string `json:"node_id"`
	URL         string `json:"url"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Default     bool   `json:"default"`
	Description string `json:"description"`
}

type issueJSONOut struct {
	ID                int         `json:"id"`
	NodeID            string      `json:"node_id"`
	URL               string      `json:"url"`
	RepositoryURL     string      `json:"repository_url"`
	HTMLURL           string      `json:"html_url"`
	Number            int         `json:"number"`
	Title             string      `json:"title"`
	Body              string      `json:"body"`
	State             string      `json:"state"`
	Locked            bool        `json:"locked"`
	User              userJSONOut `json:"user"`
	Labels            []any       `json:"labels"`
	Assignees         []any       `json:"assignees"`
	Comments          int         `json:"comments"`
	CreatedAt         string      `json:"created_at"`
	UpdatedAt         string      `json:"updated_at"`
	ClosedAt          *string     `json:"closed_at"`
	AuthorAssociation string      `json:"author_association"`
	StateReason       *string     `json:"state_reason"`
}

func labelJSONOutOf(f *Forge, label *Label, id int) labelJSONOut {
	return labelJSONOut{
		ID: id, NodeID: f.globalID("Label", label.Name),
		URL: "/api/v3/labels/" + encodeURIComponent(label.Name),
		Name: label.Name, Color: label.Color, Default: false, Description: "",
	}
}

func issueJSONOutOf(f *Forge, repo *Repo, issue *Issue) issueJSONOut {
	labels := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		if found, ok := repo.Labels[name]; ok {
			labels = append(labels, labelJSONOutOf(f, found, labelID(repo, name)))
		} else {
			labels = append(labels, map[string]any{"name": name, "color": "000000", "description": ""})
		}
	}
	var closedAt *string
	if issue.State == "closed" {
		s := fixedISO
		closedAt = &s
	}
	return issueJSONOut{
		ID: issue.ID, NodeID: f.globalID("Issue", viewerLogin, repo.Name, strconv.Itoa(issue.Number)),
		URL: fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", viewerLogin, repo.Name, issue.Number),
		RepositoryURL: fmt.Sprintf("/api/v3/repos/%s/%s", viewerLogin, repo.Name),
		HTMLURL: fmt.Sprintf("https://github.com/%s/%s/issues/%d", viewerLogin, repo.Name, issue.Number),
		Number: issue.Number, Title: issue.Title, Body: issue.Body, State: issue.State,
		Locked: false,
		User: userJSONOut{Login: viewerLogin, ID: viewerID, NodeID: f.globalID("User", viewerLogin), Type: "User", SiteAdmin: false},
		Labels: labels, Assignees: []any{}, Comments: len(issue.Comments),
		CreatedAt: fixedISO, UpdatedAt: fixedISO, ClosedAt: closedAt,
		AuthorAssociation: "OWNER", StateReason: nil,
	}
}

func issueJSON(f *Forge, repo *Repo, issue *Issue) map[string]any {
	labels := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		if found, ok := repo.Labels[name]; ok {
			labels = append(labels, labelJSON(f, found, labelID(repo, name)))
		} else {
			labels = append(labels, map[string]any{"name": name, "color": "000000", "description": ""})
		}
	}
	closedAt := any(nil)
	if issue.State == "closed" {
		closedAt = fixedISO
	}
	return map[string]any{
		"id": issue.ID, "node_id": f.globalID("Issue", viewerLogin, repo.Name, strconv.Itoa(issue.Number)),
		"url": fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", viewerLogin, repo.Name, issue.Number),
		"repository_url": fmt.Sprintf("/api/v3/repos/%s/%s", viewerLogin, repo.Name),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/issues/%d", viewerLogin, repo.Name, issue.Number),
		"number": issue.Number, "title": issue.Title, "body": issue.Body, "state": issue.State,
		"locked": false,
		"user": map[string]any{"login": viewerLogin, "id": viewerID, "node_id": f.globalID("User", viewerLogin), "type": "User", "site_admin": false},
		"labels": labels, "assignees": []any{}, "comments": len(issue.Comments),
		"created_at": fixedISO, "updated_at": fixedISO, "closed_at": closedAt,
		"author_association": "OWNER", "state_reason": nil,
	}
}

// issueResult is the GET-issue reply, cached per issue. The read workload
// fetches the same issue thousands of times; the serialized JSON only
// changes when the issue is mutated, and every mutation site invalidates
// the entry.
func issueResult(f *Forge, repo *Repo, issue *Issue) *HandlerResult {
	if cached, ok := repo.IssueJSONCache[issue.Number]; ok {
		return &HandlerResult{Status: 200, Headers: map[string]string{"content-type": "application/json; charset=utf-8"}, Body: cached}
	}
	body := marshalJSON(issueJSONOutOf(f, repo, issue))
	repo.IssueJSONCache[issue.Number] = body
	return &HandlerResult{Status: 200, Headers: map[string]string{"content-type": "application/json; charset=utf-8"}, Body: body}
}

func pullJSON(f *Forge, repo *Repo, pull *PullRequest) map[string]any {
	closedAt := any(nil)
	if pull.State != "open" {
		closedAt = fixedISO
	}
	return map[string]any{
		"id": pull.ID, "node_id": f.globalID("PullRequest", viewerLogin, repo.Name, strconv.Itoa(pull.Number)),
		"url": fmt.Sprintf("/api/v3/repos/%s/%s/pulls/%d", viewerLogin, repo.Name, pull.Number),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/pulls/%d", viewerLogin, repo.Name, pull.Number),
		"diff_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d.diff", viewerLogin, repo.Name, pull.Number),
		"patch_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d.patch", viewerLogin, repo.Name, pull.Number),
		"issue_url": fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", viewerLogin, repo.Name, pull.Number),
		"number": pull.Number, "state": pull.State, "locked": false, "title": pull.Title, "body": pull.Body,
		"user": map[string]any{"login": viewerLogin, "id": viewerID, "node_id": f.globalID("User", viewerLogin), "type": "User", "site_admin": false},
		"labels": []any{},
		"head": map[string]any{"label": viewerLogin + ":" + pull.HeadBranch, "ref": pull.HeadBranch, "sha": pull.HeadSha, "repo": repoJSONSimple(f, repo)},
		"base": map[string]any{"label": viewerLogin + ":" + pull.BaseBranch, "ref": pull.BaseBranch, "sha": f.baseTip(repo, pull), "repo": repoJSONSimple(f, repo)},
		"merged": pull.Merged, "mergeable": f.pullMergeable(repo, pull),
		"merge_commit_sha": strOrNil(pull.MergeCommitSha), "merged_at": strOrNil(pull.MergedAt),
		"created_at": fixedISO, "updated_at": fixedISO, "closed_at": closedAt,
		"author_association": "OWNER", "draft": false,
	}
}

func contentsEntryJSON(f *Forge, owner, repoName string, repo *Repo, parentPath string, entry TreeEntry) map[string]any {
	path := entry.Name
	if parentPath != "" {
		path = parentPath + "/" + entry.Name
	}
	isDir := entry.Mode == "40000"
	size := 0
	if !isDir {
		if blob := repo.Objects[entry.OID]; blob != nil {
			size = len(blob.Data)
		}
	}
	gitKind := "blobs"
	if isDir {
		gitKind = "trees"
	}
	downloadURL := any("https://raw.githubusercontent.com/" + owner + "/" + repoName + "/HEAD/" + path)
	if isDir {
		downloadURL = nil
	}
	return map[string]any{
		"type": map[bool]string{true: "dir", false: "file"}[isDir], "size": size, "name": entry.Name, "path": path,
		"sha": entry.OID, "url": "/api/v3/repos/" + owner + "/" + repoName + "/contents/" + path,
		"html_url": "https://github.com/" + owner + "/" + repoName + "/blob/HEAD/" + path,
		"git_url": "/api/v3/repos/" + owner + "/" + repoName + "/git/" + gitKind + "/" + entry.OID,
		"download_url": downloadURL,
	}
}

func commentJSON(f *Forge, repo *Repo, issue *Issue, c *IssueComment) map[string]any {
	return map[string]any{
		"id": c.ID, "node_id": f.globalID("IssueComment", viewerLogin, repo.Name, strconv.Itoa(issue.Number), strconv.Itoa(c.ID)),
		"url": fmt.Sprintf("/api/v3/repos/%s/%s/issues/comments/%d", viewerLogin, repo.Name, c.ID),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/issues/%d#issuecomment-%d", viewerLogin, repo.Name, issue.Number, c.ID),
		"body": c.Body,
		"user": map[string]any{"login": viewerLogin, "id": viewerID, "node_id": f.globalID("User", viewerLogin), "type": "User", "site_admin": false},
		"created_at": fixedISO, "updated_at": fixedISO,
	}
}

// resolveRefTip resolves a branch/tag name or an oid to a commit tip oid
// (following annotated tags up to 8 hops), or "".
func resolveRefTip(repo *Repo, ref string) string {
	var oid string
	if isOID(ref) {
		oid = ref
	} else if v, ok := repo.Refs["refs/heads/"+ref]; ok {
		oid = v
	} else if v, ok := repo.Refs["refs/tags/"+ref]; ok {
		oid = v
	} else if v, ok := repo.Refs[ref]; ok {
		oid = v
	} else {
		return ""
	}
	for i := 0; i < 8; i++ {
		obj := repo.Objects[oid]
		if obj == nil {
			return ""
		}
		if obj.Type == objTag {
			oid = parseTag(obj.Data).Object
			continue
		}
		if obj.Type == objCommit {
			return oid
		}
		return ""
	}
	return ""
}

// resolveRefTree is the root tree oid of a ref's tip commit, or "".
func resolveRefTree(repo *Repo, ref string) string {
	tip := resolveRefTip(repo, ref)
	if tip == "" {
		return ""
	}
	obj := repo.Objects[tip]
	if obj == nil || obj.Type != objCommit {
		return ""
	}
	return obj.parsedCommit().Tree
}

// paginate slices items per per_page/page and builds the next/last link
// targets ("" when absent).
func paginate[T any](items []T, req restRequest) (page []T, nextURL, lastURL string) {
	perPageRaw, perPageOK := jsParseInt(firstQuery(req, "per_page", "30"))
	perPage := 30
	if perPageOK && perPageRaw > 0 {
		perPage = perPageRaw
		if perPage > 100 {
			perPage = 100
		}
	}
	pageRaw, pageOK := jsParseInt(firstQuery(req, "page", "1"))
	pageNum := 1
	if pageOK && pageRaw > 0 {
		pageNum = pageRaw
	}
	start := (pageNum - 1) * perPage
	if start < 0 {
		start = 0
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	if start > len(items) {
		start = len(items)
		end = len(items)
	}
	page = items[start:end]
	totalPages := (len(items) + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	origin := "http://" + req.host
	var base strings.Builder
	base.WriteString(origin)
	base.WriteString("/api/v3")
	for _, s := range req.segments {
		base.WriteString("/")
		base.WriteString(encodeURIComponent(s))
	}
	link := func(p int) string {
		return fmt.Sprintf("%s?per_page=%d&page=%d", base.String(), perPage, p)
	}
	if pageNum < totalPages {
		nextURL = link(pageNum + 1)
	}
	if totalPages > 1 {
		lastURL = link(totalPages)
	}
	return page, nextURL, lastURL
}

func firstQuery(req restRequest, key, def string) string {
	if v, ok := req.query[key]; ok {
		return v
	}
	return def
}

func withLink(page any, nextURL, lastURL string) *HandlerResult {
	headers := map[string]string{}
	var links []string
	if nextURL != "" {
		links = append(links, "<"+nextURL+">; rel=\"next\"")
	}
	if lastURL != "" {
		links = append(links, "<"+lastURL+">; rel=\"last\"")
	}
	if len(links) > 0 {
		headers["Link"] = strings.Join(links, ", ")
	}
	return jsonResultWithHeaders(200, page, headers)
}

func jsonResultWithHeaders(status int, value any, headers map[string]string) *HandlerResult {
	if headers == nil {
		headers = map[string]string{}
	}
	headers["content-type"] = "application/json; charset=utf-8"
	return &HandlerResult{Status: status, Headers: headers, Body: marshalJSON(value)}
}

func parseQuery(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, value, _ := strings.Cut(pair, "=")
		k, err := url.QueryUnescape(strings.ReplaceAll(key, "+", " "))
		if err != nil {
			k = key
		}
		if _, exists := out[k]; exists {
			continue
		}
		v, err := url.QueryUnescape(strings.ReplaceAll(value, "+", " "))
		if err != nil {
			v = value
		}
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------------------------

func handleRest(f *Forge, req restRequest) *HandlerResult {
	s := req.segments
	notFound := func() *HandlerResult { return jsonResult(404, map[string]any{"message": "Not Found"}) }

	// GET /api/v3/
	if len(s) == 0 {
		if req.method != "GET" {
			return notFound()
		}
		return jsonResultWithHeaders(200, map[string]any{
			"current_user_url": "/api/v3/user",
			"current_user_authorizations_html_url": "https://github.com/settings/applications",
			"authorizations_url": "/api/v3/authorizations",
			"code_search_url": "/api/v3/search/code?q={query}{&page,per_page,sort,order}",
			"commit_search_url": "/api/v3/search/commits?q={query}{&page,per_page,sort,order}",
			"emails_url": "/api/v3/user/emails",
			"emojis_url": "/api/v3/emojis",
			"events_url": "/api/v3/events",
			"feeds_url": "/api/v3/feeds",
			"followers_url": "/api/v3/user/followers",
			"following_url": "/api/v3/user/following{/target}",
			"gists_url": "/api/v3/gists{/gist_id}",
			"hub_url": "https://api.github.com/hub",
			"issue_search_url": "/api/v3/search/issues?q={query}{&page,per_page,sort,order}",
			"issues_url": "/api/v3/issues",
			"keys_url": "/api/v3/user/keys",
			"label_search_url": "/api/v3/search/labels?q={query}{&repository_id,page,per_page}",
			"notifications_url": "/api/v3/notifications",
			"organization_url": "/api/v3/orgs/{org}",
			"organization_repositories_url": "/api/v3/orgs/{org}/repos{?type,page,per_page,sort}",
			"organization_teams_url": "/api/v3/orgs/{org}/teams",
			"public_gists_url": "/api/v3/gists/public",
			"rate_limit_url": "/api/v3/rate_limit",
			"repository_url": "/api/v3/repos/{owner}/{repo}",
			"repository_search_url": "/api/v3/search/repositories?q={query}{&page,per_page,sort,order}",
			"current_user_repositories_url": "/api/v3/user/repos{?type,page,per_page,sort}",
			"starred_url": "/api/v3/user/starred{/owner}{/repo}",
			"starred_gists_url": "/api/v3/gists/starred",
			"topic_search_url": "/api/v3/search/topics?q={query}{&page,per_page}",
			"user_url": "/api/v3/users/{user}",
			"user_organizations_url": "/api/v3/users/{user}/orgs",
			"user_repositories_url": "/api/v3/users/{user}/repos{?type,page,per_page,sort}",
			"user_search_url": "/api/v3/search/users?q={query}{&page,per_page,sort,order}",
		}, map[string]string{"X-OAuth-Scopes": "repo, read:org"})
	}

	if len(s) == 1 && s[0] == "meta" {
		if req.method != "GET" {
			return notFound()
		}
		return jsonResult(200, map[string]any{"verifiable_password_authentication": true, "installed_version": "1.0.0", "git_sha": "0000000000000000000000000000000000000000"})
	}

	if len(s) == 1 && s[0] == "user" {
		if req.method != "GET" {
			return notFound()
		}
		return jsonResult(200, viewerJSON(f))
	}

	// POST /user/repos and GET /user/repos
	if len(s) == 2 && s[0] == "user" && s[1] == "repos" {
		if req.method == "POST" {
			name, ok := bodyString(req, "name")
			if !ok || strings.TrimSpace(name) == "" {
				return errorResult(422, "Validation Failed: name is required")
			}
			name = strings.TrimSpace(name)
			if f.findRepo(viewerLogin, name) != nil {
				return errorResult(422, "Validation Failed: name already exists on this account")
			}
			description, _ := bodyString(req, "description")
			private, _ := bodyBool(req, "private")
			repo := f.createRepo(name, description, private)
			return jsonResult(201, repoJSON(f, repo))
		}
		if req.method == "GET" {
			repos := f.listViewerRepos()
			values := make([]any, 0, len(repos))
			for _, r := range repos {
				values = append(values, repoJSON(f, r))
			}
			page, nextURL, lastURL := paginate(values, req)
			return withLink(page, nextURL, lastURL)
		}
		return notFound()
	}

	if len(s) >= 2 && s[0] == "repos" {
		owner := s[1]
		if len(s) < 3 {
			return notFound()
		}
		name := s[2]
		repo := f.findRepo(owner, name)
		tail := s[3:]

		// GET/PATCH/DELETE /repos/{o}/{r}
		if len(tail) == 0 {
			if repo == nil {
				return notFound()
			}
			if req.method == "GET" {
				return jsonResult(200, repoJSON(f, repo))
			}
			if req.method == "PATCH" {
				if description, ok := bodyString(req, "description"); ok {
					repo.Description = description
				}
				if private, ok := bodyBool(req, "private"); ok {
					repo.Private = private
				}
				if newName, ok := bodyString(req, "name"); ok && strings.TrimSpace(newName) != "" && newName != repo.Name {
					trimmed := strings.TrimSpace(newName)
					if f.findRepo(owner, trimmed) != nil {
						return errorResult(422, "Validation Failed: name already exists on this account")
					}
					f.renameRepo(repo, trimmed)
				}
				return jsonResult(200, repoJSON(f, repo))
			}
			if req.method == "DELETE" {
				f.deleteRepo(owner, name)
				return emptyResult(204)
			}
			return notFound()
		}
		if repo == nil {
			return notFound()
		}

		// git refs: POST /repos/{o}/{r}/git/refs
		if len(tail) == 2 && tail[0] == "git" && tail[1] == "refs" && req.method == "POST" {
			ref, refOK := bodyString(req, "ref")
			sha, shaOK := bodyString(req, "sha")
			if !refOK || !shaOK || !strings.HasPrefix(ref, "refs/") || !isOID(sha) {
				return errorResult(422, "Validation Failed: ref and sha are required")
			}
			if _, exists := repo.Refs[ref]; exists {
				return errorResult(422, "Validation Failed: Reference already exists")
			}
			obj := repo.Objects[sha]
			if obj == nil {
				return errorResult(422, "Validation Failed: Invalid SHA")
			}
			repo.setRef(ref, sha)
			return jsonResult(201, map[string]any{
				"ref": ref, "node_id": f.globalID("Ref", owner, name, ref),
				"url": fmt.Sprintf("/api/v3/repos/%s/%s/git/refs/%s", owner, name, encodeURIComponent(ref)),
				"object": map[string]any{"sha": sha, "type": string(obj.Type), "url": fmt.Sprintf("/api/v3/repos/%s/%s/git/%ss/%s", owner, name, string(obj.Type), sha)},
			})
		}

		// GET /repos/{o}/{r}/git/refs/heads/{branch}
		if len(tail) == 4 && tail[0] == "git" && tail[1] == "refs" && tail[2] == "heads" && req.method == "GET" {
			branch, err := url.PathUnescape(tail[3])
			if err != nil {
				branch = tail[3]
			}
			ref := "refs/heads/" + branch
			sha, ok := repo.Refs[ref]
			if !ok {
				return notFound()
			}
			typ := "commit"
			if obj := repo.Objects[sha]; obj != nil {
				typ = string(obj.Type)
			}
			// The reference builds this url with tail[4], which is undefined:
			// the literal string ends with /undefined. Ported faithfully.
			return jsonResult(200, map[string]any{
				"ref": ref, "node_id": f.globalID("Ref", owner, name, ref),
				"url": fmt.Sprintf("/api/v3/repos/%s/%s/git/refs/heads/undefined", owner, name),
				"object": map[string]any{"sha": sha, "type": typ, "url": fmt.Sprintf("/api/v3/repos/%s/%s/git/%ss/%s", owner, name, typ, sha)},
			})
		}

		// branches
		if len(tail) == 1 && tail[0] == "branches" && req.method == "GET" {
			var branches []any
			for _, ref := range repo.refsOrder {
				if !strings.HasPrefix(ref, "refs/heads/") {
					continue
				}
				sha := repo.Refs[ref]
				branches = append(branches, map[string]any{
					"name": ref[len("refs/heads/"):],
					"commit": map[string]any{"sha": sha, "url": fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", owner, name, sha)},
					"protected": false,
				})
			}
			if branches == nil {
				branches = []any{}
			}
			return jsonResult(200, branches)
		}
		if len(tail) == 2 && tail[0] == "branches" && req.method == "GET" {
			branch, err := url.PathUnescape(tail[1])
			if err != nil {
				branch = tail[1]
			}
			ref := "refs/heads/" + branch
			sha, ok := repo.Refs[ref]
			if !ok {
				return notFound()
			}
			return jsonResult(200, map[string]any{
				"name": tail[1],
				"commit": map[string]any{"sha": sha, "url": fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", owner, name, sha)},
				"protected": false,
			})
		}

		// commits
		if len(tail) == 1 && tail[0] == "commits" && req.method == "GET" {
			refParam := firstQuery(req, "sha", repo.DefaultBranch)
			tip := resolveRefTip(repo, refParam)
			var oids []string
			if tip != "" {
				oids = listCommits(repo.Objects, tip, 100)
			}
			out := make([]any, 0, len(oids))
			for _, oid := range oids {
				c := parseCommitUncached(nil)
				if obj := repo.Objects[oid]; obj != nil {
					c = obj.parsedCommit()
				}
				parents := make([]any, 0, len(c.Parents))
				for _, p := range c.Parents {
					parents = append(parents, map[string]any{
						"sha": p, "url": fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", owner, name, p),
						"html_url": fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, name, p),
					})
				}
				out = append(out, map[string]any{
					"sha": oid, "node_id": f.globalID("Commit", owner, name, oid),
					"commit": map[string]any{
						"author": map[string]any{"name": viewerName, "email": viewerEmail, "date": fixedISO},
						"committer": map[string]any{"name": viewerName, "email": viewerEmail, "date": fixedISO},
						"message": strings.TrimSuffix(c.Message, "\n"),
						"tree": map[string]any{"sha": c.Tree},
						"url": fmt.Sprintf("/api/v3/repos/%s/%s/git/commits/%s", owner, name, oid),
						"comment_count": 0,
					},
					"url": fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", owner, name, oid),
					"html_url": fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, name, oid),
					"author": map[string]any{"login": viewerLogin, "id": viewerID, "node_id": f.globalID("User", viewerLogin), "type": "User", "site_admin": false},
					"committer": map[string]any{"login": viewerLogin, "id": viewerID, "node_id": f.globalID("User", viewerLogin), "type": "User", "site_admin": false},
					"parents": parents,
				})
			}
			return jsonResult(200, out)
		}

		// contents
		if len(tail) > 0 && tail[0] == "contents" {
			var pathParts []string
			for _, p := range tail[1:] {
				decoded, err := url.PathUnescape(p)
				if err != nil {
					decoded = p
				}
				pathParts = append(pathParts, decoded)
			}
			path := strings.Join(pathParts, "/")
			if req.method == "GET" {
				refParam := firstQuery(req, "ref", repo.DefaultBranch)
				tree := resolveRefTree(repo, refParam)
				if tree == "" {
					return notFound()
				}
				if path == "" {
					entries := listDirAt(repo.Objects, tree, "")
					if entries == nil {
						return notFound()
					}
					out := make([]any, 0, len(entries))
					for _, e := range entries {
						out = append(out, contentsEntryJSON(f, owner, name, repo, "", e))
					}
					return jsonResult(200, out)
				}
				if file := readFileAt(repo.Objects, tree, path); file != nil {
					base := path[strings.LastIndex(path, "/")+1:]
					return jsonResult(200, map[string]any{
						"type": "file", "encoding": "base64", "size": len(file.Data), "name": base, "path": path,
						"content": base64.StdEncoding.EncodeToString(file.Data), "sha": file.OID,
						"url": fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s", owner, name, path),
						"html_url": fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", owner, name, refParam, path),
						"git_url": fmt.Sprintf("/api/v3/repos/%s/%s/git/blobs/%s", owner, name, file.OID),
						"download_url": fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, name, refParam, path),
						"_links": map[string]any{
							"git": fmt.Sprintf("/api/v3/repos/%s/%s/git/blobs/%s", owner, name, file.OID),
							"self": fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s", owner, name, path),
							"html": fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", owner, name, refParam, path),
						},
					})
				}
				dirEntries := listDirAt(repo.Objects, tree, path)
				if dirEntries == nil || len(dirEntries) == 0 {
					return notFound()
				}
				out := make([]any, 0, len(dirEntries))
				for _, e := range dirEntries {
					out = append(out, contentsEntryJSON(f, owner, name, repo, path, e))
				}
				return jsonResult(200, out)
			}
			if req.method == "PUT" {
				contentStr, ok := bodyString(req, "content")
				if !ok {
					return errorResult(422, "Validation Failed: content is required")
				}
				content, err := lenientBase64(contentStr)
				if err != nil {
					return errorResult(422, "Validation Failed: content is not valid base64")
				}
				branch := repo.DefaultBranch
				if b, ok := bodyString(req, "branch"); ok && b != "" {
					branch = b
				}
				message := "Update " + path
				if m, ok := bodyString(req, "message"); ok {
					message = m
				}
				tree := resolveRefTree(repo, branch)
				var current *fileAt
				if tree != "" {
					current = readFileAt(repo.Objects, tree, path)
				}
				if sha, ok := bodyString(req, "sha"); ok && current != nil && sha != current.OID {
					return errorResult(409, "Conflict: sha does not match the current file")
				}
				existed := current != nil
				commitSha, blobSha := f.commitFile(repo, branch, path, string(content), message)
				base := path[strings.LastIndex(path, "/")+1:]
				status := 201
				if existed {
					status = 200
				}
				return jsonResult(status, map[string]any{
					"content": map[string]any{
						"type": "file", "encoding": "base64", "size": len(content), "name": base, "path": path,
						"content": base64.StdEncoding.EncodeToString(content), "sha": blobSha,
						"url": fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s", owner, name, path),
						"html_url": fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", owner, name, branch, path),
						"git_url": fmt.Sprintf("/api/v3/repos/%s/%s/git/blobs/%s", owner, name, blobSha),
						"download_url": fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, name, branch, path),
					},
					"commit": map[string]any{"sha": commitSha, "message": message},
				})
			}
			if req.method == "DELETE" {
				branch := repo.DefaultBranch
				if b, ok := bodyString(req, "branch"); ok && b != "" {
					branch = b
				}
				message := "Delete " + path
				if m, ok := bodyString(req, "message"); ok {
					message = m
				}
				tree := resolveRefTree(repo, branch)
				var current *fileAt
				if tree != "" {
					current = readFileAt(repo.Objects, tree, path)
				}
				if current == nil {
					return notFound()
				}
				if sha, ok := bodyString(req, "sha"); ok && sha != current.OID {
					return errorResult(409, "Conflict: sha does not match the current file")
				}
				deleted, ok := f.deleteFile(repo, branch, path, message)
				if !ok {
					return notFound()
				}
				return jsonResult(200, map[string]any{"commit": map[string]any{"sha": deleted, "message": message}})
			}
			return notFound()
		}

		// labels
		if len(tail) == 1 && tail[0] == "labels" {
			if req.method == "GET" {
				out := make([]any, 0, len(repo.labelsOrder))
				for _, n := range repo.labelsOrder {
					out = append(out, labelJSON(f, repo.Labels[n], labelID(repo, n)))
				}
				return jsonResult(200, out)
			}
			if req.method == "POST" {
				name, ok := bodyString(req, "name")
				if !ok || strings.TrimSpace(name) == "" {
					return errorResult(422, "Validation Failed: name is required")
				}
				name = strings.TrimSpace(name)
				if existing, ok := repo.Labels[name]; ok {
					return jsonResult(200, labelJSON(f, existing, labelID(repo, name)))
				}
				color := "000000"
				if c, ok := bodyString(req, "color"); ok {
					color = strings.TrimPrefix(c, "#")
				}
				label := f.createLabel(repo, name, color)
				return jsonResult(201, labelJSON(f, label, labelID(repo, label.Name)))
			}
			return notFound()
		}

		// issues
		if len(tail) == 1 && tail[0] == "issues" {
			if req.method == "GET" {
				state := firstQuery(req, "state", "open")
				sortParam := firstQuery(req, "sort", "created")
				direction := firstQuery(req, "direction", "desc")
				var items []*Issue
				for _, i := range repo.Issues {
					if state == "open" && i.State != "open" {
						continue
					}
					if state == "closed" && i.State != "closed" {
						continue
					}
					items = append(items, i)
				}
				sort.SliceStable(items, func(a, b int) bool { return items[a].Number < items[b].Number })
				if sortParam == "created" || sortParam == "updated" {
					if direction == "asc" {
						sort.SliceStable(items, func(a, b int) bool { return items[a].Number < items[b].Number })
					} else {
						sort.SliceStable(items, func(a, b int) bool { return items[a].Number > items[b].Number })
					}
				}
				values := make([]any, 0, len(items))
				for _, i := range items {
					values = append(values, issueJSON(f, repo, i))
				}
				page, nextURL, lastURL := paginate(values, req)
				return withLink(page, nextURL, lastURL)
			}
			if req.method == "POST" {
				title, ok := bodyString(req, "title")
				if !ok || strings.TrimSpace(title) == "" {
					return errorResult(422, "Validation Failed: title is required")
				}
				var labelNames []string
				if raw, exists := bodyMap(req)["labels"]; exists {
					if arr, isArr := raw.([]any); isArr {
						for _, l := range arr {
							if s, isStr := l.(string); isStr {
								labelNames = append(labelNames, s)
							} else if m, isMap := l.(map[string]any); isMap {
								if n, isStr := m["name"].(string); isStr {
									labelNames = append(labelNames, n)
								}
							}
						}
					}
				}
				for _, n := range labelNames {
					if _, exists := repo.Labels[n]; !exists {
						return errorResult(422, fmt.Sprintf("Validation Failed: label \"%s\" does not exist", n))
					}
				}
				body, _ := bodyString(req, "body")
				issue := f.createIssue(repo, strings.TrimSpace(title), body, labelNames)
				return jsonResult(201, issueJSONOutOf(f, repo, issue))
			}
			return notFound()
		}

		// issue by number and its comments
		if len(tail) >= 2 && tail[0] == "issues" {
			number, ok := jsParseInt(tail[1])
			var issue *Issue
			if ok {
				issue = repo.Issues[number]
			}
			if len(tail) == 2 {
				if issue == nil {
					return notFound()
				}
				if req.method == "GET" {
					return issueResult(f, repo, issue)
				}
				if req.method == "PATCH" {
					if title, ok := bodyString(req, "title"); ok {
						issue.Title = title
					}
					if body, ok := bodyString(req, "body"); ok {
						issue.Body = body
					}
					if state, ok := bodyString(req, "state"); ok && (state == "open" || state == "closed") {
						issue.State = state
					}
					delete(repo.IssueJSONCache, number)
					return jsonResult(200, issueJSONOutOf(f, repo, issue))
				}
				return notFound()
			}
			if len(tail) == 3 && tail[2] == "comments" {
				if issue == nil {
					return notFound()
				}
				if req.method == "GET" {
					out := make([]any, 0, len(issue.Comments))
					for i := range issue.Comments {
						out = append(out, commentJSON(f, repo, issue, &issue.Comments[i]))
					}
					return jsonResult(200, out)
				}
				if req.method == "POST" {
					body, ok := bodyString(req, "body")
					if !ok || strings.TrimSpace(body) == "" {
						return errorResult(422, "Validation Failed: body is required")
					}
					comment := f.addComment(repo, issue, body)
					return jsonResult(201, commentJSON(f, repo, issue, comment))
				}
				return notFound()
			}
			return notFound()
		}

		// pulls
		if len(tail) == 1 && tail[0] == "pulls" {
			if req.method == "GET" {
				state := firstQuery(req, "state", "open")
				var items []*PullRequest
				for _, p := range repo.Pulls {
					if state == "open" && p.State != "open" {
						continue
					}
					if state == "closed" && p.State != "closed" {
						continue
					}
					items = append(items, p)
				}
				sort.SliceStable(items, func(a, b int) bool { return items[a].Number > items[b].Number })
				values := make([]any, 0, len(items))
				for _, p := range items {
					values = append(values, pullJSON(f, repo, p))
				}
				page, nextURL, lastURL := paginate(values, req)
				return withLink(page, nextURL, lastURL)
			}
			if req.method == "POST" {
				title, titleOK := bodyString(req, "title")
				head, headOK := bodyString(req, "head")
				base, baseOK := bodyString(req, "base")
				if !titleOK || !headOK || !baseOK {
					return errorResult(422, "Validation Failed: title, head and base are required")
				}
				_, headExists := repo.Refs["refs/heads/"+head]
				_, baseExists := repo.Refs["refs/heads/"+base]
				if !headExists || !baseExists {
					return errorResult(422, "Validation Failed: head or base branch does not exist")
				}
				for _, p := range repo.Pulls {
					if p.State == "open" && p.HeadBranch == head && p.BaseBranch == base {
						return errorResult(422, "Validation Failed: A pull request already exists for these branches")
					}
				}
				body, _ := bodyString(req, "body")
				pull := f.createPull(repo, strings.TrimSpace(title), body, head, base)
				return jsonResult(201, pullJSON(f, repo, pull))
			}
			return notFound()
		}

		if len(tail) >= 2 && tail[0] == "pulls" {
			number, ok := jsParseInt(tail[1])
			var pull *PullRequest
			if ok {
				pull = repo.Pulls[number]
			}
			if len(tail) == 2 {
				if pull == nil {
					return notFound()
				}
				if req.method == "GET" {
					return jsonResult(200, pullJSON(f, repo, pull))
				}
				return notFound()
			}
			if len(tail) == 3 && tail[2] == "files" && req.method == "GET" {
				if pull == nil {
					return notFound()
				}
				files := f.pullFiles(repo, pull)
				out := make([]any, 0, len(files))
				for _, file := range files {
					sha := file.NewOID
					if sha == "" {
						sha = file.OldOID
					}
					out = append(out, map[string]any{
						"sha": sha, "filename": file.Path, "status": file.Status,
						"additions": file.Additions, "deletions": file.Deletions,
						"changes": file.Additions + file.Deletions,
						"blob_url": fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", viewerLogin, repo.Name, pull.HeadSha, file.Path),
						"raw_url": fmt.Sprintf("https://github.com/%s/%s/raw/%s/%s", viewerLogin, repo.Name, pull.HeadSha, file.Path),
						"contents_url": fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s?ref=%s", viewerLogin, repo.Name, file.Path, pull.HeadSha),
					})
				}
				return jsonResult(200, out)
			}
			if len(tail) == 3 && tail[2] == "merge" && req.method == "PUT" {
				if pull == nil {
					return notFound()
				}
				if pull.Merged {
					return errorResult(405, "Pull Request is already merged")
				}
				if pull.State != "open" {
					return errorResult(405, "Pull Request is not open")
				}
				if method, ok := bodyString(req, "merge_method"); ok && method != "squash" {
					return errorResult(405, "Only the squash merge method is supported")
				}
				merged := f.mergePull(repo, pull)
				if merged == "" {
					return errorResult(405, "Merge conflict: the pull request cannot be merged")
				}
				return jsonResult(200, map[string]any{"sha": merged, "merged": true, "message": fmt.Sprintf("Merge pull request #%d from %s", pull.Number, pull.HeadBranch)})
			}
			return notFound()
		}
	}

	return notFound()
}
