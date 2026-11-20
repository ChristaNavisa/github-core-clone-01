// MCP: JSON-RPC 2.0 Streamable HTTP on POST /mcp (plain application/json
// replies). Supports the modern 2026-07-28 discovery handshake and the legacy
// initialize handshake, a static tools/list, and six tools over the same Forge
// store the REST and GraphQL surfaces use.

package main

import (
	"strconv"
	"strings"
)

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"

func mcpTools() []any {
	tool := func(name, description string, properties map[string]any, required []string) map[string]any {
		req := make([]any, 0, len(required))
		for _, r := range required {
			req = append(req, r)
		}
		return map[string]any{
			"name": name, "description": description,
			"inputSchema": map[string]any{
				"type": "object", "properties": properties, "required": req, "additionalProperties": false,
			},
		}
	}
	strProp := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []any{
		tool("create_repo", "Create a repository owned by the viewer.", map[string]any{
			"name":        strProp("Repository name"),
			"description": strProp("Repository description"),
			"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private"},
		}, []string{"name"}),
		tool("get_repo", "Get a repository by owner and name.", map[string]any{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
		}, []string{"owner", "repo"}),
		tool("create_issue", "Create an issue in a repository.", map[string]any{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
			"title": strProp("Issue title"),
			"body":  strProp("Issue body"),
		}, []string{"owner", "repo", "title"}),
		tool("list_issues", "List issues in a repository.", map[string]any{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
			"state": map[string]any{"type": "string", "description": "Issue state filter", "enum": []any{"open", "closed", "all"}},
		}, []string{"owner", "repo"}),
		tool("create_pull_request", "Create a pull request from a head branch into a base branch.", map[string]any{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
			"title": strProp("Pull request title"),
			"head":  strProp("Head branch name"),
			"base":  strProp("Base branch name"),
			"body":  strProp("Pull request body"),
		}, []string{"owner", "repo", "title", "head", "base"}),
		tool("merge_pull_request", "Merge a pull request with the squash method.", map[string]any{
			"owner":       strProp("Repository owner login"),
			"repo":        strProp("Repository name"),
			"pull_number": map[string]any{"type": "integer", "description": "Pull request number"},
		}, []string{"owner", "repo", "pull_number"}),
	}
}

var mcpToolNames = []string{"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request"}

func rpcResult(id any, result any) *HandlerResult {
	return jsonResult(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func rpcError(id any, code int, message string) *HandlerResult {
	return jsonResult(200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

// marshalJSONNoNL is marshalJSON without the trailing newline: tool text
// carries JSON.stringify output, which has no trailing newline.
func marshalJSONNoNL(value any) []byte {
	b := marshalJSON(value)
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return b[:len(b)-1]
	}
	return b
}

func toolText(structured any) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(marshalJSONNoNL(structured))}},
		"structuredContent": structured,
		"isError":           false,
	}
}

func toolError(message string) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(marshalJSONNoNL(map[string]any{"error": message}))}},
		"structuredContent": map[string]any{"error": message},
		"isError":           true,
	}
}

// ownerRepoOf returns owner/repo from canonical args, or from a
// `full_name: "owner/repo"` arg.
func ownerRepoOf(args map[string]any) (owner, repo string, ok bool) {
	if fullName, isStr := args["full_name"].(string); isStr {
		parts := strings.Split(fullName, "/")
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return parts[0], parts[1], true
		}
	}
	ownerStr, ownerOK := args["owner"].(string)
	repoStr, repoOK := args["repo"].(string)
	if ownerOK && repoOK {
		return ownerStr, repoStr, true
	}
	return "", "", false
}

type mcpRequest struct {
	method string
	json   any
}

func handleMcp(f *Forge, req mcpRequest) *HandlerResult {
	if req.method == "DELETE" {
		return emptyResult(204)
	}
	if req.method != "POST" {
		return jsonResult(404, map[string]any{"message": "Not Found"})
	}
	body, isObj := req.json.(map[string]any)
	if req.json == nil || !isObj {
		// An array body is an object in JS, so it passes the shape check but
		// has no "id": it is a notification and is answered with 202.
		if _, isArr := req.json.([]any); isArr {
			return emptyResult(202)
		}
		return jsonResult(400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error: body is not a JSON-RPC object"}})
	}
	id, hasID := body["id"]
	if !hasID {
		return emptyResult(202)
	}
	method, methodOK := body["method"].(string)
	if !methodOK {
		return rpcError(id, -32600, "Invalid Request: method is required")
	}
	params := map[string]any{}
	if p, ok := body["params"].(map[string]any); ok {
		params = p
	}

	if method == "server/discover" {
		serverInfo := map[string]any{"name": mcpServerName, "version": mcpServerVersion}
		return rpcResult(id, map[string]any{
			"supportedVersions": []any{"2026-07-28", "2025-11-25", "2025-06-18"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        serverInfo,
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": mcpServerName, "version": mcpServerVersion}},
		})
	}
	if method == "initialize" {
		requested := "2025-11-25"
		if s, ok := params["protocolVersion"].(string); ok {
			requested = s
		}
		return rpcResult(id, map[string]any{
			"protocolVersion": requested,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		})
	}
	if method == "ping" {
		return rpcResult(id, map[string]any{})
	}
	if method == "tools/list" {
		return rpcResult(id, map[string]any{"tools": mcpTools()})
	}
	if method == "tools/call" {
		name, _ := params["name"].(string)
		args := map[string]any{}
		if a, ok := params["arguments"].(map[string]any); ok {
			args = a
		}
		if name == "" {
			return rpcError(id, -32602, "Invalid params: tools/call requires a tool name")
		}
		toolName := ""
		lower := strings.ToLower(name)
		for _, t := range mcpToolNames {
			if lower == t {
				toolName = t
				break
			}
			parts := strings.FieldsFunc(lower, func(c rune) bool { return c == '/' || c == '.' || c == ':' })
			if len(parts) > 0 && parts[len(parts)-1] == t {
				toolName = t
				break
			}
		}
		if toolName == "" {
			return rpcError(id, -32601, "Method not found: no tool named \""+name+"\"")
		}
		required := mcpRequiredArgs(toolName)
		for _, prop := range required {
			v, exists := args[prop]
			if !exists || v == nil || v == "" {
				return rpcError(id, -32602, "Invalid params: missing required argument \""+prop+"\" for tool \""+toolName+"\"")
			}
		}
		result, err := callTool(f, toolName, args)
		if err != nil {
			return rpcResult(id, toolError(err.Error()))
		}
		return rpcResult(id, result)
	}
	return rpcError(id, -32601, "Method not found: "+method)
}

func mcpRequiredArgs(tool string) []string {
	switch tool {
	case "create_repo":
		return []string{"name"}
	case "get_repo":
		return []string{"owner", "repo"}
	case "create_issue":
		return []string{"owner", "repo", "title"}
	case "list_issues":
		return []string{"owner", "repo"}
	case "create_pull_request":
		return []string{"owner", "repo", "title", "head", "base"}
	case "merge_pull_request":
		return []string{"owner", "repo", "pull_number"}
	}
	return nil
}

func callTool(f *Forge, name string, args map[string]any) (map[string]any, error) {
	switch name {
	case "create_repo":
		repoName := strings.TrimSpace(jsString(args["name"]))
		if repoName == "" {
			return nil, &mcpError{"create_repo: name is required"}
		}
		if f.findRepo(viewerLogin, repoName) != nil {
			return nil, &mcpError{"create_repo: repository \"" + repoName + "\" already exists"}
		}
		description := ""
		if s, ok := args["description"].(string); ok {
			description = s
		}
		private := false
		if b, ok := args["private"].(bool); ok {
			private = b
		}
		repo := f.createRepo(repoName, description, private)
		return toolText(map[string]any{
			"id": repo.ID, "name": repo.Name, "full_name": viewerLogin + "/" + repo.Name,
			"owner": map[string]any{"login": viewerLogin}, "private": repo.Private,
			"description": repo.Description, "default_branch": repo.DefaultBranch,
			"created_at": fixedISO, "updated_at": fixedISO,
		}), nil
	case "get_repo":
		owner, repoName, ok := ownerRepoOf(args)
		if !ok {
			return nil, &mcpError{"get_repo: owner and repo are required"}
		}
		repo := f.findRepo(owner, repoName)
		if repo == nil {
			return nil, &mcpError{"get_repo: repository \"" + owner + "/" + repoName + "\" not found"}
		}
		return toolText(map[string]any{
			"id": repo.ID, "name": repo.Name, "full_name": owner + "/" + repoName,
			"owner": map[string]any{"login": owner}, "private": repo.Private,
			"description": repo.Description, "default_branch": repo.DefaultBranch,
			"created_at": fixedISO, "updated_at": fixedISO,
		}), nil
	case "create_issue":
		owner, repoName, ok := ownerRepoOf(args)
		if !ok {
			return nil, &mcpError{"create_issue: owner and repo are required"}
		}
		repo := f.findRepo(owner, repoName)
		if repo == nil {
			return nil, &mcpError{"create_issue: repository \"" + owner + "/" + repoName + "\" not found"}
		}
		body := ""
		if s, ok := args["body"].(string); ok {
			body = s
		}
		issue := f.createIssue(repo, jsString(args["title"]), body, []string{})
		labels := make([]any, 0, len(issue.Labels))
		for _, l := range issue.Labels {
			labels = append(labels, l)
		}
		return toolText(map[string]any{
			"id": issue.ID, "number": issue.Number, "title": issue.Title, "body": issue.Body,
			"state": issue.State, "labels": labels, "repository": owner + "/" + repoName,
			"created_at": fixedISO, "updated_at": fixedISO,
		}), nil
	case "list_issues":
		owner, repoName, ok := ownerRepoOf(args)
		if !ok {
			return nil, &mcpError{"list_issues: owner and repo are required"}
		}
		repo := f.findRepo(owner, repoName)
		if repo == nil {
			return nil, &mcpError{"list_issues: repository \"" + owner + "/" + repoName + "\" not found"}
		}
		state := "open"
		if s, ok := args["state"].(string); ok {
			state = s
		}
		var items []*Issue
		for _, i := range repo.Issues {
			items = append(items, i)
		}
		sortIssuesByNumber(items)
		out := make([]any, 0)
		for _, i := range items {
			if state != "all" && i.State != state {
				continue
			}
			labels := make([]any, 0, len(i.Labels))
			for _, l := range i.Labels {
				labels = append(labels, l)
			}
			out = append(out, map[string]any{
				"id": i.ID, "number": i.Number, "title": i.Title, "body": i.Body,
				"state": i.State, "labels": labels, "created_at": fixedISO, "updated_at": fixedISO,
			})
		}
		return toolText(out), nil
	case "create_pull_request":
		owner, repoName, ok := ownerRepoOf(args)
		if !ok {
			return nil, &mcpError{"create_pull_request: owner and repo are required"}
		}
		repo := f.findRepo(owner, repoName)
		if repo == nil {
			return nil, &mcpError{"create_pull_request: repository \"" + owner + "/" + repoName + "\" not found"}
		}
		headName := jsString(args["head"])
		baseName := jsString(args["base"])
		_, headExists := repo.Refs["refs/heads/"+headName]
		_, baseExists := repo.Refs["refs/heads/"+baseName]
		if !headExists || !baseExists {
			return nil, &mcpError{"create_pull_request: head or base branch does not exist"}
		}
		for _, p := range repo.Pulls {
			if p.State == "open" && p.HeadBranch == headName && p.BaseBranch == baseName {
				return nil, &mcpError{"create_pull_request: a pull request already exists for these branches"}
			}
		}
		body := ""
		if s, ok := args["body"].(string); ok {
			body = s
		}
		pull := f.createPull(repo, jsString(args["title"]), body, headName, baseName)
		return toolText(map[string]any{
			"id": pull.ID, "number": pull.Number, "title": pull.Title, "body": pull.Body,
			"state": pull.State,
			"head": map[string]any{"ref": pull.HeadBranch, "sha": pull.HeadSha},
			"base": map[string]any{"ref": pull.BaseBranch, "sha": f.baseTip(repo, pull)},
			"merged": pull.Merged, "created_at": fixedISO, "updated_at": fixedISO,
		}), nil
	case "merge_pull_request":
		owner, repoName, ok := ownerRepoOf(args)
		if !ok {
			return nil, &mcpError{"merge_pull_request: owner and repo are required"}
		}
		repo := f.findRepo(owner, repoName)
		if repo == nil {
			return nil, &mcpError{"merge_pull_request: repository \"" + owner + "/" + repoName + "\" not found"}
		}
		var number int
		numberOK := false
		if n, ok := toNumber(args["pull_number"]); ok {
			number = int(n)
			numberOK = true
		} else if n, ok := jsParseInt(jsString(args["pull_number"])); ok {
			number = n
			numberOK = true
		}
		numberStr := strconv.Itoa(number)
		if !numberOK {
			numberStr = "NaN"
		}
		pull, found := repo.Pulls[number]
		if !found {
			return nil, &mcpError{"merge_pull_request: pull request #" + numberStr + " not found"}
		}
		if pull.Merged {
			return nil, &mcpError{"merge_pull_request: pull request is already merged"}
		}
		if pull.State != "open" {
			return nil, &mcpError{"merge_pull_request: pull request is not open"}
		}
		merged := f.mergePull(repo, pull)
		if merged == "" {
			return nil, &mcpError{"merge_pull_request: merge conflict"}
		}
		return toolText(map[string]any{"sha": merged, "merged": true, "number": pull.Number, "state": "MERGED", "merge_commit_sha": merged}), nil
	}
	return nil, &mcpError{"unknown tool \"" + name + "\""}
}

type mcpError struct{ msg string }

func (e *mcpError) Error() string { return e.msg }
