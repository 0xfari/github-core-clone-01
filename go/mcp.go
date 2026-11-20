// mcp.go — Model Context Protocol over JSON-RPC 2.0 Streamable HTTP.
// Modern era via `server/discover` (supportedVersions incl. 2026-07-28);
// legacy handshake via `initialize` + `notifications/initialized`. The tool
// set is built once at package load so two `tools/list` calls are
// byte-identical. Tools act on the same store as REST and GraphQL.

package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const mcpServerName = "github-core-clone-forge"
const mcpServerVersion = "1.0.0"
const mcpModernRevision = "2026-07-28"
const mcpLegacyRevision = "2025-11-25"

var mcpRevisionRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// buildMCPTools builds the fixed tool list once, in a fixed order.
func buildMCPTools() *JVal {
	tool := func(name, desc string, props [][3]string, required ...string) *JVal {
		properties := &JVal{Kind: JObject}
		for _, p := range props {
			properties.O = append(properties.O, JPair{K: p[0], V: Obj("type", JStrVal(p[1]), "description", JStrVal(p[2]))})
		}
		reqItems := make([]*JVal, 0, len(required))
		for _, r := range required {
			reqItems = append(reqItems, JStrVal(r))
		}
		return Obj(
			"name", JStrVal(name),
			"description", JStrVal(desc),
			"inputSchema", Obj(
				"type", JStrVal("object"),
				"properties", properties,
				"required", JArrVal(reqItems...),
			),
		)
	}
	return JArrVal(
		tool("create_repo", "Create a new repository owned by the authenticated user.", [][3]string{
			{"name", "string", "Repository name."},
			{"description", "string", "A short description of the repository."},
			{"private", "boolean", "Whether the repository is private."},
		}, "name"),
		tool("get_repo", "Get a repository by owner and name.", [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
		}, "owner", "repo"),
		tool("create_issue", "Create an issue in a repository.", [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"title", "string", "Issue title."},
			{"body", "string", "Issue body."},
		}, "owner", "repo", "title"),
		tool("list_issues", "List issues in a repository.", [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"state", "string", "Issue state filter: open, closed, or all."},
		}, "owner", "repo"),
		tool("create_pull_request", "Create a pull request from a head branch into a base branch.", [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"title", "string", "Pull request title."},
			{"head", "string", "Head branch name."},
			{"base", "string", "Base branch name."},
			{"body", "string", "Pull request body."},
		}, "owner", "repo", "title", "head", "base"),
		tool("merge_pull_request", "Merge a pull request with a squash commit onto its base branch.", [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"pull_number", "integer", "Pull request number."},
		}, "owner", "repo", "pull_number"),
	)
}

var mcpTools = buildMCPTools()

func mcpServerInfo() *JVal {
	return Obj("name", JStrVal(mcpServerName), "version", JStrVal(mcpServerVersion))
}

func mcpJSONRPCResult(id *JVal, result *JVal) *JVal {
	return Obj("jsonrpc", JStrVal("2.0"), "id", id, "result", result)
}

func mcpJSONRPCError(id *JVal, code int, message string) *JVal {
	return Obj("jsonrpc", JStrVal("2.0"), "id", id, "error", Obj("code", JIntVal(int64(code)), "message", JStrVal(message)))
}

func mcpToolResult(obj *JVal) *JVal {
	return Obj(
		"content", JArrVal(Obj("type", JStrVal("text"), "text", JStrVal(string(obj.Marshal())))),
		"structuredContent", obj,
	)
}

func mcpToolError(message string) *JVal {
	return Obj(
		"content", JArrVal(Obj("type", JStrVal("text"), "text", JStrVal(message))),
		"isError", JBoolVal(true),
	)
}

func mcpRepoJSON(ctx *Ctx, repo *Repo) *JVal {
	return Obj(
		"id", JIntVal(int64(repo.ID)),
		"name", JStrVal(repo.Name),
		"full_name", JStrVal(repo.Owner+"/"+repo.Name),
		"owner", Obj("login", JStrVal(repo.Owner), "id", JIntVal(int64(ctx.Store.ViewerID))),
		"private", JBoolVal(repo.IsPrivate),
		"description", JStrVal(repo.Description),
		"default_branch", JStrVal(repo.DefaultBranch),
		"created_at", JStrVal(ctx.Store.TrialNowIso),
		"updated_at", JStrVal(ctx.Store.TrialNowIso),
	)
}

// mcpResolveRepoArgs resolves owner/repo from tool arguments, accepting
// the common synonyms. full_name wins first; owner/owner_login and
// repo/repo_name override; repository fills only empty slots.
func mcpResolveRepoArgs(args map[string]*JVal) (owner, repo string) {
	if v := args["full_name"]; v != nil {
		if s, ok := v.Str(); ok && strings.Contains(s, "/") {
			parts := strings.SplitN(s, "/", 2)
			owner, repo = parts[0], parts[1]
		}
	}
	if v := args["owner"]; v != nil {
		if s, ok := v.Str(); ok {
			owner = s
		}
	}
	if v := args["owner_login"]; v != nil {
		if s, ok := v.Str(); ok {
			owner = s
		}
	}
	if v := args["repo"]; v != nil {
		if s, ok := v.Str(); ok {
			repo = s
		}
	}
	if v := args["repo_name"]; v != nil {
		if s, ok := v.Str(); ok {
			repo = s
		}
	}
	if v := args["repository"]; v != nil {
		if s, ok := v.Str(); ok && strings.Contains(s, "/") {
			parts := strings.SplitN(s, "/", 2)
			if owner == "" {
				owner = parts[0]
			}
			if repo == "" {
				repo = parts[1]
			}
		}
	}
	return owner, repo
}

// mcpRequireArg returns the argument value, or nil when it is missing,
// null, or the empty string.
func mcpRequireArg(args map[string]*JVal, name string) *JVal {
	v := args[name]
	if v == nil {
		return nil
	}
	if v.Kind == JNull {
		return nil
	}
	if s, ok := v.Str(); ok && s == "" {
		return nil
	}
	return v
}

func mcpArgString(args map[string]*JVal, name string) string {
	if v := args[name]; v != nil {
		if s, ok := v.Str(); ok {
			return s
		}
	}
	return ""
}

func mcpCallTool(ctx *Ctx, name string, args map[string]*JVal) *JVal {
	store := ctx.Store
	switch name {
	case "create_repo":
		repoName := mcpRequireArg(args, "name")
		nameStr, ok := "", false
		if repoName != nil {
			nameStr, ok = repoName.Str()
		}
		if !ok {
			return mcpToolError("Missing required argument: name")
		}
		if store.GetRepo(store.ViewerLogin, nameStr) != nil {
			return mcpToolError("Repository " + store.ViewerLogin + "/" + nameStr + " already exists")
		}
		repo := store.CreateRepo(nameStr, mcpArgString(args, "description"), args["private"] != nil && args["private"].Kind == JBool && args["private"].B)
		return mcpToolResult(mcpRepoJSON(ctx, repo))
	case "get_repo":
		owner, repoName := mcpResolveRepoArgs(args)
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required argument: owner and repo")
		}
		found := store.GetRepo(owner, repoName)
		if found == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found")
		}
		return mcpToolResult(mcpRepoJSON(ctx, found))
	case "create_issue":
		owner, repoName := mcpResolveRepoArgs(args)
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required argument: owner and repo")
		}
		title := mcpRequireArg(args, "title")
		titleStr, ok := "", false
		if title != nil {
			titleStr, ok = title.Str()
		}
		if !ok {
			return mcpToolError("Missing required argument: title")
		}
		found := store.GetRepo(owner, repoName)
		if found == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found")
		}
		issue := store.CreateIssue(found, titleStr, mcpArgString(args, "body"))
		return mcpToolResult(Obj(
			"id", JIntVal(int64(issue.ID)),
			"number", JIntVal(int64(issue.Number)),
			"title", JStrVal(issue.Title),
			"body", JStrVal(issue.Body),
			"state", JStrVal(issue.State),
			"labels", JArrVal(),
		))
	case "list_issues":
		owner, repoName := mcpResolveRepoArgs(args)
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required argument: owner and repo")
		}
		found := store.GetRepo(owner, repoName)
		if found == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found")
		}
		state := mcpArgString(args, "state")
		if state == "" {
			state = "open"
		}
		items := make([]*JVal, 0)
		for _, i := range found.Issues {
			if state != "all" && i.State != state {
				continue
			}
			labelItems := make([]*JVal, 0, len(i.Labels))
			for _, l := range i.Labels {
				labelItems = append(labelItems, Obj("name", JStrVal(l.Name)))
			}
			items = append(items, Obj(
				"id", JIntVal(int64(i.ID)),
				"number", JIntVal(int64(i.Number)),
				"title", JStrVal(i.Title),
				"body", JStrVal(i.Body),
				"state", JStrVal(i.State),
				"labels", JArrVal(labelItems...),
			))
		}
		return mcpToolResult(JArrVal(items...))
	case "create_pull_request":
		owner, repoName := mcpResolveRepoArgs(args)
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required argument: owner and repo")
		}
		title := mcpRequireArg(args, "title")
		head := mcpRequireArg(args, "head")
		base := mcpRequireArg(args, "base")
		titleStr, titleOK := "", false
		if title != nil {
			titleStr, titleOK = title.Str()
		}
		if !titleOK {
			return mcpToolError("Missing required argument: title")
		}
		headStr, headOK := "", false
		if head != nil {
			headStr, headOK = head.Str()
		}
		if !headOK {
			return mcpToolError("Missing required argument: head")
		}
		baseStr, baseOK := "", false
		if base != nil {
			baseStr, baseOK = base.Str()
		}
		if !baseOK {
			return mcpToolError("Missing required argument: base")
		}
		found := store.GetRepo(owner, repoName)
		if found == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found")
		}
		pr := store.CreatePullInternal(found, titleStr, headStr, baseStr)
		if pr == nil {
			return mcpToolError("Could not create pull request for the given head and base")
		}
		return mcpToolResult(Obj(
			"id", JIntVal(int64(pr.ID)),
			"number", JIntVal(int64(pr.Number)),
			"title", JStrVal(pr.Title),
			"body", JStrVal(pr.Body),
			"state", JStrVal(pr.State),
			"head", Obj("ref", JStrVal(pr.HeadRef), "sha", JStrVal(pr.HeadSha)),
			"base", Obj("ref", JStrVal(pr.BaseRef), "sha", JStrVal(pr.BaseSha)),
		))
	case "merge_pull_request":
		owner, repoName := mcpResolveRepoArgs(args)
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required argument: owner and repo")
		}
		rawNumber := mcpRequireArg(args, "pull_number")
		number, ok := mcpJSNumber(rawNumber)
		if !ok {
			return mcpToolError("Missing required argument: pull_number")
		}
		found := store.GetRepo(owner, repoName)
		if found == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found")
		}
		var pr *PullItem
		for _, p := range found.Pulls {
			if p.Number == number {
				pr = p
				break
			}
		}
		if pr == nil {
			return mcpToolError("Pull request #" + strconv.Itoa(number) + " not found")
		}
		if pr.Merged || pr.State != "open" {
			return mcpToolError("Pull request is already merged or closed")
		}
		sha := store.MergePullInternal(found, pr)
		if sha == "" {
			return mcpToolError("Merge conflict")
		}
		return mcpToolResult(Obj(
			"merged", JBoolVal(true),
			"sha", JStrVal(sha),
			"number", JIntVal(int64(pr.Number)),
			"state", JStrVal("closed"),
		))
	default:
		return mcpToolError("Unknown tool: " + name)
	}
}

// mcpJSNumber converts a JSON value to an integer the way JavaScript's
// Number() does, returning ok=false when the result is not an integer.
func mcpJSNumber(v *JVal) (int, bool) {
	if v == nil {
		return 0, false
	}
	switch v.Kind {
	case JInt:
		return int(v.I), true
	case JFloat:
		if v.F == float64(int64(v.F)) {
			return int(v.F), true
		}
		return 0, false
	case JString:
		f, err := strconv.ParseFloat(strings.TrimSpace(v.S), 64)
		if err != nil || f != float64(int64(f)) {
			return 0, false
		}
		return int(f), true
	case JBool:
		if v.B {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func handleMcp(ctx *Ctx, w http.ResponseWriter) {
	if ctx.Method != "POST" {
		panic(apiErr(405, "Method Not Allowed"))
	}
	parsed, err := parseJSONBody(ctx)
	if err != nil {
		// The MCP layer answers a malformed body with a JSON-RPC Parse
		// error, not the shared "Problems parsing JSON" message.
		sendJSON(w, 400, mcpJSONRPCError(JNullVal(), -32700, "Parse error"), nil)
		return
	}
	if parsed == nil || !parsed.IsObject() || parsed.IsArray() {
		sendJSON(w, 400, mcpJSONRPCError(JNullVal(), -32600, "Invalid Request"), nil)
		return
	}
	// Notifications carry no id and get an empty 202.
	idVal := parsed.Get("id")
	if idVal == nil || idVal.Kind == JNull {
		sendEmpty(w, 202, nil)
		return
	}
	methodVal := parsed.Get("method")
	method := ""
	if methodVal != nil {
		method, _ = methodVal.Str()
	}
	params := parsed.Get("params")
	paramsObj := map[string]*JVal{}
	if params != nil && params.IsObject() {
		for _, p := range params.O {
			paramsObj[p.K] = p.V
		}
	}
	switch method {
	case "server/discover":
		info := mcpServerInfo()
		sendJSON(w, 200, mcpJSONRPCResult(idVal, Obj(
			"supportedVersions", JArrVal(JStrVal(mcpModernRevision), JStrVal(mcpLegacyRevision)),
			"capabilities", Obj("tools", &JVal{Kind: JObject}),
			"serverInfo", info,
			"_meta", Obj("io.modelcontextprotocol/serverInfo", info),
		)), nil)
	case "initialize":
		requested := mcpArgString(paramsObj, "protocolVersion")
		negotiated := mcpModernRevision
		if requested != "" && mcpRevisionRe.MatchString(requested) {
			negotiated = requested
		}
		sendJSON(w, 200, mcpJSONRPCResult(idVal, Obj(
			"protocolVersion", JStrVal(negotiated),
			"capabilities", Obj("tools", &JVal{Kind: JObject}),
			"serverInfo", mcpServerInfo(),
		)), nil)
	case "ping":
		sendJSON(w, 200, mcpJSONRPCResult(idVal, &JVal{Kind: JObject}), nil)
	case "resources/list":
		sendJSON(w, 200, mcpJSONRPCResult(idVal, Obj("resources", JArrVal())), nil)
	case "prompts/list":
		sendJSON(w, 200, mcpJSONRPCResult(idVal, Obj("prompts", JArrVal())), nil)
	case "tools/list":
		sendJSON(w, 200, mcpJSONRPCResult(idVal, Obj("tools", mcpTools)), nil)
	case "tools/call":
		toolName := mcpArgString(paramsObj, "name")
		known := false
		for _, t := range mcpTools.A {
			if n := t.Get("name"); n != nil {
				if s, ok := n.Str(); ok && s == toolName {
					known = true
					break
				}
			}
		}
		if !known {
			sendJSON(w, 200, mcpJSONRPCError(idVal, -32602, "Unknown tool: "+toolName), nil)
			return
		}
		argsObj := map[string]*JVal{}
		if argsVal := paramsObj["arguments"]; argsVal != nil && argsVal.IsObject() {
			for _, p := range argsVal.O {
				argsObj[p.K] = p.V
			}
		}
		result := mcpCallTool(ctx, toolName, argsObj)
		sendJSON(w, 200, mcpJSONRPCResult(idVal, result), nil)
	default:
		sendJSON(w, 200, mcpJSONRPCError(idVal, -32601, "Method not found: "+method), nil)
	}
}
