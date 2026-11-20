// rest.go — REST v3: repos, branches, commits, contents, labels, issues,
// pulls and merges. Every reply is JSON with a non-empty X-GitHub-Request-Id
// (set by app.go) and X-OAuth-Scopes; errors are JSON {message}. Auth is
// enforced by app.go before this module runs.

package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// JSON shapes (key order matches the TypeScript oracle exactly)
// ---------------------------------------------------------------------------

func repoJSON(ctx *Ctx, repo *Repo) *JVal {
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
		"html_url", JStrVal("https://github.com/"+repo.Owner+"/"+repo.Name),
		"url", JStrVal("/api/v3/repos/"+repo.Owner+"/"+repo.Name),
		"fork", JBoolVal(false),
		"has_issues", JBoolVal(true),
		"has_projects", JBoolVal(true),
		"has_wiki", JBoolVal(true),
	)
}

func labelJSON(label *Label) *JVal {
	return Obj(
		"id", JIntVal(int64(label.ID)),
		"name", JStrVal(label.Name),
		"color", JStrVal(label.Color),
		"description", JStrVal(label.Description),
		"default", JBoolVal(false),
	)
}

func issueJSON(ctx *Ctx, repo *Repo, issue *IssueItem) *JVal {
	labels := make([]*JVal, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, labelJSON(l))
	}
	return Obj(
		"id", JIntVal(int64(issue.ID)),
		"number", JIntVal(int64(issue.Number)),
		"title", JStrVal(issue.Title),
		"body", JStrVal(issue.Body),
		"state", JStrVal(issue.State),
		"labels", JArrVal(labels...),
		"user", Obj("login", JStrVal(issue.UserLogin), "id", JIntVal(int64(ctx.Store.ViewerID))),
		"created_at", JStrVal(ctx.Store.TrialNowIso),
		"updated_at", JStrVal(ctx.Store.TrialNowIso),
		"html_url", JStrVal(fmt.Sprintf("https://github.com/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number)),
		"url", JStrVal(fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number)),
		"comments_url", JStrVal(fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d/comments", repo.Owner, repo.Name, issue.Number)),
	)
}

func pullJSON(ctx *Ctx, repo *Repo, pr *PullItem) *JVal {
	mergedAt := JNullVal()
	if pr.MergedAt != "" {
		mergedAt = JStrVal(pr.MergedAt)
	}
	mergeSha := JNullVal()
	if pr.MergeCommitSha != "" {
		mergeSha = JStrVal(pr.MergeCommitSha)
	}
	return Obj(
		"id", JIntVal(int64(pr.ID)),
		"number", JIntVal(int64(pr.Number)),
		"title", JStrVal(pr.Title),
		"body", JStrVal(pr.Body),
		"state", JStrVal(pr.State),
		"merged", JBoolVal(pr.Merged),
		"merged_at", mergedAt,
		"merge_commit_sha", mergeSha,
		"head", Obj("ref", JStrVal(pr.HeadRef), "sha", JStrVal(pr.HeadSha), "label", JStrVal(repo.Owner+":"+pr.HeadRef)),
		"base", Obj("ref", JStrVal(pr.BaseRef), "sha", JStrVal(pr.BaseSha), "label", JStrVal(repo.Owner+":"+pr.BaseRef)),
		"user", Obj("login", JStrVal(ctx.Store.ViewerLogin), "id", JIntVal(int64(ctx.Store.ViewerID))),
		"created_at", JStrVal(ctx.Store.TrialNowIso),
		"updated_at", JStrVal(ctx.Store.TrialNowIso),
		"html_url", JStrVal(fmt.Sprintf("https://github.com/%s/%s/pull/%d", repo.Owner, repo.Name, pr.Number)),
		"url", JStrVal(fmt.Sprintf("/api/v3/repos/%s/%s/pulls/%d", repo.Owner, repo.Name, pr.Number)),
		"mergeable", JBoolVal(!pr.Merged),
	)
}

func commentJSON(c *CommentItem) *JVal {
	return Obj(
		"id", JIntVal(int64(c.ID)),
		"body", JStrVal(c.Body),
		"user", Obj("login", JStrVal(c.UserLogin)),
		"created_at", JStrVal(c.CreatedAt),
		"updated_at", JStrVal(c.CreatedAt),
	)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func requireRepo(ctx *Ctx, owner, name string) *Repo {
	repo := ctx.Store.GetRepo(owner, name)
	if repo == nil {
		panic(apiErr(404, "Not Found"))
	}
	return repo
}

func parseIntParam(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// encodeQueryValue encodes one query value the way URLSearchParams does
// (application/x-www-form-urlencoded: space -> +, the rest percent-encoded
// except [A-Za-z0-9*-._]).
func encodeQueryValue(s string) string {
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			buf.WriteByte('+')
		case c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '*':
			buf.WriteByte(c)
		default:
			fmt.Fprintf(&buf, "%%%02X", c)
		}
	}
	return buf.String()
}

// paginate slices a list and returns the page plus a Link header value (or "").
func paginate(ctx *Ctx, total int, pageItems func(start, end int) []*JVal) ([]*JVal, string) {
	perPage := parseIntParam(ctx.Query.Get("per_page"), 30)
	page := parseIntParam(ctx.Query.Get("page"), 1)
	totalPages := (total + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	start := (page - 1) * perPage
	end := start + perPage
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	items := pageItems(start, end)
	host := ctx.Host
	if host == "" {
		host = "127.0.0.1"
	}
	origin := "http://" + host
	basePath := ctx.Path
	var extra []QPair
	for _, p := range ctx.Query.Entries() {
		if p.K != "page" && p.K != "per_page" {
			extra = append(extra, p)
		}
	}
	buildURL := func(p int) string {
		var parts []string
		for _, e := range extra {
			parts = append(parts, encodeQueryValue(e.K)+"="+encodeQueryValue(e.V))
		}
		parts = append(parts, "page="+strconv.Itoa(p), "per_page="+strconv.Itoa(perPage))
		return origin + basePath + "?" + strings.Join(parts, "&")
	}
	var links []string
	if page < totalPages {
		links = append(links, "<"+buildURL(page+1)+`>; rel="next"`)
		links = append(links, "<"+buildURL(totalPages)+`>; rel="last"`)
	}
	if page > 1 {
		links = append(links, "<"+buildURL(page-1)+`>; rel="prev"`)
		links = append(links, "<"+buildURL(1)+`>; rel="first"`)
	}
	link := ""
	if len(links) > 0 {
		link = strings.Join(links, ", ")
	}
	return items, link
}

// paginateVals is paginate for pre-built JVal slices.
func paginateVals(ctx *Ctx, items []*JVal) ([]*JVal, string) {
	return paginate(ctx, len(items), func(start, end int) []*JVal {
		return items[start:end]
	})
}

func branchTip(repo *Repo, branch string) string {
	return repo.Refs["refs/heads/"+branch]
}

func resolveCommitTip(repo *Repo, ref string) string {
	if ref == "" {
		return branchTip(repo, repo.DefaultBranch)
	}
	if isObjectId(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return ref
		}
		return ""
	}
	return branchTip(repo, ref)
}

func commitAuthorParts(identity string) (name, email string) {
	// "Name <email>" — same shape as the oracle's /^(.*?) <([^>]*)>/.
	if i := strings.Index(identity, " <"); i >= 0 {
		rest := identity[i+2:]
		if j := strings.Index(rest, ">"); j >= 0 {
			return identity[:i], rest[:j]
		}
	}
	return identity, ""
}

func bodyString(body *JVal, key string) string {
	if v := body.Get(key); v != nil {
		if s, ok := v.Str(); ok {
			return s
		}
	}
	return ""
}

// bodyStringOr returns the field's string value when it is a string (even
// an empty one), or def when the field is missing or not a string. This
// matches `typeof body.x === 'string' ? body.x : def` in the oracle.
func bodyStringOr(body *JVal, key, def string) string {
	if v := body.Get(key); v != nil {
		if s, ok := v.Str(); ok {
			return s
		}
	}
	return def
}

func bodyBool(body *JVal, key string) bool {
	if v := body.Get(key); v != nil {
		if b, ok := v.Bool(); ok {
			return b
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

// handleRest routes /api/v3/... (ctx.Segments is after /api/v3).
// It returns true when it handled the request (always, or panics ApiError).
func handleRest(ctx *Ctx, w http.ResponseWriter) {
	s := ctx.Segments
	method := ctx.Method

	if len(s) == 0 {
		if method == "GET" {
			sendJSON(w, 200, JObjVal())
			return
		}
		panic(apiErr(404, "Not Found"))
	}
	if len(s) == 1 && s[0] == "meta" {
		if method == "GET" {
			sendJSON(w, 200, Obj("installed_version", JStrVal("1.0.0")))
			return
		}
		panic(apiErr(404, "Not Found"))
	}
	if len(s) == 1 && s[0] == "user" {
		if method == "GET" {
			sendJSON(w, 200, Obj(
				"login", JStrVal(ctx.Store.ViewerLogin),
				"id", JIntVal(int64(ctx.Store.ViewerID)),
				"name", JStrVal(ctx.Store.ViewerName),
				"email", JStrVal(ctx.Store.ViewerEmail),
				"type", JStrVal("User"),
			))
			return
		}
		panic(apiErr(404, "Not Found"))
	}
	if len(s) == 2 && s[0] == "user" && s[1] == "repos" {
		if method == "POST" {
			createRepo(ctx, w)
			return
		}
		if method == "GET" {
			listUserRepos(ctx, w)
			return
		}
		panic(apiErr(404, "Not Found"))
	}
	if len(s) >= 3 && s[0] == "repos" {
		handleRepoRoute(ctx, w)
		return
	}
	panic(apiErr(404, "Not Found"))
}

func createRepo(ctx *Ctx, w http.ResponseWriter) {
	body, err := parseJSONBody(ctx)
	if err != nil {
		panic(err)
	}
	name := strings.TrimSpace(bodyString(body, "name"))
	if name == "" {
		panic(apiErr(422, "name is required"))
	}
	if ctx.Store.GetRepo(ctx.Store.ViewerLogin, name) != nil {
		panic(apiErr(422, "name already exists on this account"))
	}
	repo := ctx.Store.CreateRepo(name, bodyString(body, "description"), bodyBool(body, "private"))
	sendJSON(w, 201, repoJSON(ctx, repo))
}

func listUserRepos(ctx *Ctx, w http.ResponseWriter) {
	repos := ctx.Store.ListViewerRepos()
	items := make([]*JVal, 0, len(repos))
	for _, r := range repos {
		items = append(items, repoJSON(ctx, r))
	}
	page, link := paginateVals(ctx, items)
	extra := map[string]string{}
	if link != "" {
		extra["Link"] = link
	}
	sendJSON(w, 200, JArrVal(page...), extra)
}

func handleRepoRoute(ctx *Ctx, w http.ResponseWriter) {
	s := ctx.Segments
	method := ctx.Method
	owner := s[1]
	name := s[2]
	repo := requireRepo(ctx, owner, name)
	rest := s[3:]

	if len(rest) == 0 {
		if method == "GET" {
			sendJSON(w, 200, repoJSON(ctx, repo))
			return
		}
		if method == "PATCH" {
			body, err := parseJSONBody(ctx)
			if err != nil {
				panic(err)
			}
			if v := body.Get("description"); v != nil {
				if str, ok := v.Str(); ok {
					repo.Description = str
				}
			}
			if v := body.Get("private"); v != nil {
				if b, ok := v.Bool(); ok {
					repo.IsPrivate = b
				}
			}
			sendJSON(w, 200, repoJSON(ctx, repo))
			return
		}
		if method == "DELETE" {
			ctx.Store.DeleteRepo(owner, name)
			sendEmpty(w, 204, nil)
			return
		}
		panic(apiErr(404, "Not Found"))
	}

	switch rest[0] {
	case "labels":
		handleLabels(ctx, w, repo, rest[1:])
	case "issues":
		handleIssues(ctx, w, repo, rest[1:])
	case "pulls":
		handlePulls(ctx, w, repo, rest[1:])
	case "branches":
		handleBranches(ctx, w, repo, rest[1:])
	case "commits":
		handleCommits(ctx, w, repo)
	case "contents":
		handleContents(ctx, w, repo, rest[1:])
	case "git":
		handleGitRefs(ctx, w, repo, rest[1:])
	default:
		panic(apiErr(404, "Not Found"))
	}
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

func handleLabels(ctx *Ctx, w http.ResponseWriter, repo *Repo, rest []string) {
	if len(rest) == 0 {
		if ctx.Method == "GET" {
			items := make([]*JVal, 0, len(repo.Labels))
			for _, l := range repo.Labels {
				items = append(items, labelJSON(l))
			}
			sendJSON(w, 200, JArrVal(items...))
			return
		}
		if ctx.Method == "POST" {
			body, err := parseJSONBody(ctx)
			if err != nil {
				panic(err)
			}
			name := strings.TrimSpace(bodyString(body, "name"))
			if name == "" {
				panic(apiErr(422, "name is required"))
			}
			for _, l := range repo.Labels {
				if l.Name == name {
					panic(apiErr(422, "label already exists"))
				}
			}
			label := &Label{
				ID:          ctx.Store.NextLabelID,
				Name:        name,
				Color:       "ededed",
				Description: "",
			}
			if c := bodyString(body, "color"); c != "" {
				label.Color = c
			}
			if d := bodyString(body, "description"); d != "" {
				label.Description = d
			}
			ctx.Store.NextLabelID++
			repo.Labels = append(repo.Labels, label)
			sendJSON(w, 201, labelJSON(label))
			return
		}
	}
	panic(apiErr(404, "Not Found"))
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func findIssue(repo *Repo, number int) *IssueItem {
	for _, i := range repo.Issues {
		if i.Number == number {
			return i
		}
	}
	return nil
}

func resolveLabelNames(repo *Repo, names *JVal) []*Label {
	if names == nil || !names.IsArray() {
		return nil
	}
	var out []*Label
	for _, entry := range names.A {
		var name string
		if s, ok := entry.Str(); ok {
			name = s
		} else if entry.IsObject() {
			if v := entry.Get("name"); v != nil {
				if s, ok := v.Str(); ok {
					name = s
				}
			}
		}
		if name == "" {
			continue
		}
		var label *Label
		for _, l := range repo.Labels {
			if l.Name == name {
				label = l
				break
			}
		}
		if label == nil {
			panic(apiErr(422, "Label does not exist: "+name))
		}
		out = append(out, label)
	}
	return out
}

func handleIssues(ctx *Ctx, w http.ResponseWriter, repo *Repo, rest []string) {
	method := ctx.Method
	if len(rest) == 0 {
		if method == "GET" {
			state := ctx.Query.Get("state")
			if state == "" {
				state = "open"
			}
			sortDir := ctx.Query.Get("sort")
			if sortDir == "" {
				sortDir = "created"
			}
			direction := ctx.Query.Get("direction")
			if direction == "" {
				direction = "desc"
			}
			items := make([]*IssueItem, 0, len(repo.Issues))
			for _, i := range repo.Issues {
				if state != "all" && i.State != state {
					continue
				}
				items = append(items, i)
			}
			if sortDir == "created" {
				// Sort by seq; insertion order is already seq order, so a
				// stable sort by seq in the requested direction suffices.
				if direction == "asc" {
					// already ascending
				} else {
					for l, r := 0, len(items)-1; l < r; l, r = l+1, r-1 {
						items[l], items[r] = items[r], items[l]
					}
				}
			}
			vals := make([]*JVal, 0, len(items))
			for _, i := range items {
				vals = append(vals, issueJSON(ctx, repo, i))
			}
			page, link := paginateVals(ctx, vals)
			extra := map[string]string{}
			if link != "" {
				extra["Link"] = link
			}
			sendJSON(w, 200, JArrVal(page...), extra)
			return
		}
		if method == "POST" {
			body, err := parseJSONBody(ctx)
			if err != nil {
				panic(err)
			}
			title := bodyString(body, "title")
			if title == "" {
				panic(apiErr(422, "title is required"))
			}
			labels := resolveLabelNames(repo, body.Get("labels"))
			issue := &IssueItem{
				ID:        ctx.Store.NextIssueID,
				Number:    ctx.Store.NextRepoNumber(repo),
				Title:     title,
				Body:      bodyString(body, "body"),
				State:     "open",
				Labels:    labels,
				UserLogin: ctx.Store.ViewerLogin,
				Seq:       ctx.Store.NextIssueID,
				Rev:       0,
			}
			issue.repo = repo
			ctx.Store.NextIssueID++
			repo.Issues = append(repo.Issues, issue)
			sendJSON(w, 201, issueJSON(ctx, repo, issue))
			return
		}
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		panic(apiErr(404, "Not Found"))
	}
	issue := findIssue(repo, number)
	if issue == nil {
		panic(apiErr(404, "Not Found"))
	}
	if len(rest) == 1 {
		if method == "GET" {
			// The read workload GETs one unchanged issue 2,000 times: serve
			// the serialized body cached at first build. The cache is
			// cleared on every mutation of this issue, and the body depends
			// on nothing else that can change, so a cached body is
			// byte-identical.
			if issue.JSONCache == "" {
				issue.JSONCache = string(issueJSON(ctx, repo, issue).Marshal())
			}
			sendJSONString(w, 200, issue.JSONCache)
			return
		}
		if method == "PATCH" {
			// Clear first: resolveLabelNames below can throw after earlier
			// fields were already mutated, and a stale cache must never
			// survive a throw.
			issue.JSONCache = ""
			issue.Rev++
			body, err := parseJSONBody(ctx)
			if err != nil {
				panic(err)
			}
			if v := body.Get("title"); v != nil {
				if s, ok := v.Str(); ok {
					issue.Title = s
				}
			}
			if v := body.Get("body"); v != nil {
				if s, ok := v.Str(); ok {
					issue.Body = s
				}
			}
			if v := body.Get("state"); v != nil {
				if s, ok := v.Str(); ok && (s == "open" || s == "closed") {
					issue.State = s
				}
			}
			if body.Get("labels") != nil {
				issue.Labels = resolveLabelNames(repo, body.Get("labels"))
			}
			sendJSON(w, 200, issueJSON(ctx, repo, issue))
			return
		}
	}
	if len(rest) == 2 && rest[1] == "comments" {
		if method == "GET" {
			items := make([]*JVal, 0, len(issue.Comments))
			for _, c := range issue.Comments {
				items = append(items, commentJSON(c))
			}
			sendJSON(w, 200, JArrVal(items...))
			return
		}
		if method == "POST" {
			body, err := parseJSONBody(ctx)
			if err != nil {
				panic(err)
			}
			text := bodyString(body, "body")
			if text == "" {
				panic(apiErr(422, "body is required"))
			}
			comment := &CommentItem{
				ID:        ctx.Store.NextCommentID,
				Body:      text,
				UserLogin: ctx.Store.ViewerLogin,
				CreatedAt: ctx.Store.TrialNowIso,
			}
			ctx.Store.NextCommentID++
			issue.Comments = append(issue.Comments, comment)
			// A comment does not change the issue JSON shape, but clear the
			// cache anyway: the cached body must never outlive any mutation.
			issue.JSONCache = ""
			issue.Rev++
			sendJSON(w, 201, commentJSON(comment))
			return
		}
	}
	panic(apiErr(404, "Not Found"))
}

// ---------------------------------------------------------------------------
// Pulls
// ---------------------------------------------------------------------------

func findPull(repo *Repo, number int) *PullItem {
	for _, p := range repo.Pulls {
		if p.Number == number {
			return p
		}
	}
	return nil
}

func repoLookup(repo *Repo) ObjectLookup {
	return func(oid string) *GitObject {
		if obj, ok := repo.Objects[oid]; ok {
			o := obj
			return &o
		}
		return nil
	}
}

func pullConflictNow(repo *Repo, pr *PullItem) bool {
	lookup := repoLookup(repo)
	baseTip := branchTip(repo, pr.BaseRef)
	if baseTip == "" {
		return true
	}
	baseObj := lookup(baseTip)
	headObj := lookup(pr.HeadSha)
	if baseObj == nil || headObj == nil {
		return true
	}
	baseCommit, err1 := parseCommit(baseObj.Data)
	headCommit, err2 := parseCommit(headObj.Data)
	if err1 != nil || err2 != nil {
		return true
	}
	mb := mergeBase(lookup, pr.HeadSha, baseTip)
	var mbTree string
	if mb != "" {
		if mbObj := lookup(mb); mbObj != nil {
			if c, err := parseCommit(mbObj.Data); err == nil {
				mbTree = c.Tree
			}
		}
	}
	return mergeConflicts(lookup, mbTree, headCommit.Tree, baseCommit.Tree)
}

func handlePulls(ctx *Ctx, w http.ResponseWriter, repo *Repo, rest []string) {
	method := ctx.Method
	if len(rest) == 0 {
		if method == "GET" {
			state := ctx.Query.Get("state")
			if state == "" {
				state = "open"
			}
			items := make([]*PullItem, 0, len(repo.Pulls))
			for _, p := range repo.Pulls {
				if state != "all" && p.State != state {
					continue
				}
				items = append(items, p)
			}
			// Sort by seq descending (newest first).
			for l, r := 0, len(items)-1; l < r; l, r = l+1, r-1 {
				items[l], items[r] = items[r], items[l]
			}
			vals := make([]*JVal, 0, len(items))
			for _, p := range items {
				vals = append(vals, pullJSON(ctx, repo, p))
			}
			page, link := paginateVals(ctx, vals)
			extra := map[string]string{}
			if link != "" {
				extra["Link"] = link
			}
			sendJSON(w, 200, JArrVal(page...), extra)
			return
		}
		if method == "POST" {
			body, err := parseJSONBody(ctx)
			if err != nil {
				panic(err)
			}
			title := bodyString(body, "title")
			head := bodyString(body, "head")
			base := bodyString(body, "base")
			if title == "" || head == "" || base == "" {
				panic(apiErr(422, "title, head and base are required"))
			}
			headTip := branchTip(repo, head)
			baseTip := branchTip(repo, base)
			if headTip == "" || baseTip == "" {
				panic(apiErr(422, "head or base branch does not exist"))
			}
			for _, p := range repo.Pulls {
				if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
					panic(apiErr(422, "a pull request already exists for this head and base"))
				}
			}
			lookup := repoLookup(repo)
			mb := mergeBase(lookup, headTip, baseTip)
			headObj := lookup(headTip)
			headCommit, _ := parseCommit(headObj.Data)
			var mbTree string
			if mb != "" {
				if mbObj := lookup(mb); mbObj != nil {
					if c, err := parseCommit(mbObj.Data); err == nil {
						mbTree = c.Tree
					}
				}
			}
			files := diffTrees(lookup, mbTree, headCommit.Tree)
			pr := &PullItem{
				ID:           ctx.Store.NextIssueID,
				Number:       ctx.Store.NextRepoNumber(repo),
				Title:        title,
				Body:         bodyString(body, "body"),
				State:        "open",
				HeadRef:      head,
				BaseRef:      base,
				HeadSha:      headTip,
				BaseSha:      baseTip,
				MergeBaseSha: mb,
				Files:        files,
				Seq:          ctx.Store.NextIssueID,
			}
			pr.repo = repo
			ctx.Store.NextIssueID++
			repo.Pulls = append(repo.Pulls, pr)
			sendJSON(w, 201, pullJSON(ctx, repo, pr))
			return
		}
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		panic(apiErr(404, "Not Found"))
	}
	pr := findPull(repo, number)
	if pr == nil {
		panic(apiErr(404, "Not Found"))
	}
	if len(rest) == 1 {
		if method == "GET" {
			sendJSON(w, 200, pullJSON(ctx, repo, pr))
			return
		}
	}
	if len(rest) == 2 && rest[1] == "files" && method == "GET" {
		items := make([]*JVal, 0, len(pr.Files))
		for _, f := range pr.Files {
			items = append(items, Obj(
				"filename", JStrVal(f.Filename),
				"status", JStrVal(f.Status),
				"additions", JIntVal(int64(f.Additions)),
				"deletions", JIntVal(int64(f.Deletions)),
				"changes", JIntVal(int64(f.Changes)),
			))
		}
		sendJSON(w, 200, JArrVal(items...))
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && method == "PUT" {
		if pr.Merged || pr.State != "open" {
			if pr.Merged {
				panic(apiErr(405, "Pull request already merged"))
			}
			panic(apiErr(405, "Pull request is closed"))
		}
		if pullConflictNow(repo, pr) {
			panic(apiErr(405, "Merge conflict"))
		}
		lookup := repoLookup(repo)
		baseTip := branchTip(repo, pr.BaseRef)
		baseObj := lookup(baseTip)
		headObj := lookup(pr.HeadSha)
		baseCommit, _ := parseCommit(baseObj.Data)
		headCommit, _ := parseCommit(headObj.Data)
		mb := mergeBase(lookup, pr.HeadSha, baseTip)
		var mbTree string
		if mb != "" {
			if mbObj := lookup(mb); mbObj != nil {
				if c, err := parseCommit(mbObj.Data); err == nil {
					mbTree = c.Tree
				}
			}
		}
		mergedTree, err := applyMerge(lookup, func(typ ObjType, data []byte) string {
			return ctx.Store.PutObject(repo, typ, data)
		}, mbTree, headCommit.Tree, baseCommit.Tree)
		if err != nil {
			panic(err)
		}
		mergeCommitSha := ctx.Store.MakeCommit(repo, BuildCommitFields{
			Tree:    mergedTree,
			Parents: []string{baseTip},
			Message: fmt.Sprintf("Merge pull request #%d from %s/%s\n\n%s", pr.Number, repo.Owner, pr.HeadRef, pr.Title),
		})
		repo.Refs["refs/heads/"+pr.BaseRef] = mergeCommitSha
		pr.State = "closed"
		pr.Merged = true
		pr.MergedAt = ctx.Store.TrialNowIso
		pr.MergeCommitSha = mergeCommitSha
		sendJSON(w, 200, Obj(
			"sha", JStrVal(mergeCommitSha),
			"merged", JBoolVal(true),
			"message", JStrVal("Pull request successfully merged"),
		))
		return
	}
	panic(apiErr(404, "Not Found"))
}

// ---------------------------------------------------------------------------
// Branches, commits
// ---------------------------------------------------------------------------

func handleBranches(ctx *Ctx, w http.ResponseWriter, repo *Repo, rest []string) {
	if len(rest) == 0 {
		if ctx.Method != "GET" {
			panic(apiErr(404, "Not Found"))
		}
		names := make([]string, 0, len(repo.Refs))
		for ref := range repo.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, ref)
			}
		}
		// sort.Strings on ref names (v2 sorts the entries).
		sort.Strings(names)
		items := make([]*JVal, 0, len(names))
		for _, ref := range names {
			items = append(items, Obj(
				"name", JStrVal(strings.TrimPrefix(ref, "refs/heads/")),
				"commit", Obj("sha", JStrVal(repo.Refs[ref])),
				"protected", JBoolVal(false),
			))
		}
		sendJSON(w, 200, JArrVal(items...))
		return
	}
	if ctx.Method != "GET" {
		panic(apiErr(404, "Not Found"))
	}
	branch := strings.Join(rest, "/")
	tip := branchTip(repo, branch)
	if tip == "" {
		panic(apiErr(404, "Not Found"))
	}
	sendJSON(w, 200, Obj(
		"name", JStrVal(branch),
		"commit", Obj("sha", JStrVal(tip)),
		"protected", JBoolVal(false),
	))
}

func handleCommits(ctx *Ctx, w http.ResponseWriter, repo *Repo) {
	if ctx.Method != "GET" {
		panic(apiErr(404, "Not Found"))
	}
	tip := resolveCommitTip(repo, ctx.Query.Get("sha"))
	if tip == "" {
		panic(apiErr(404, "Not Found"))
	}
	lookup := repoLookup(repo)
	commits := listCommits(lookup, tip)
	vals := make([]*JVal, 0, len(commits))
	for _, entry := range commits {
		authorName, authorEmail := commitAuthorParts(entry.Commit.Author)
		committerName, committerEmail := commitAuthorParts(entry.Commit.Committer)
		parents := make([]*JVal, 0, len(entry.Commit.Parents))
		for _, p := range entry.Commit.Parents {
			parents = append(parents, Obj("sha", JStrVal(p)))
		}
		vals = append(vals, Obj(
			"sha", JStrVal(entry.Sha),
			"commit", Obj(
				"message", JStrVal(entry.Commit.Message),
				"author", Obj("name", JStrVal(authorName), "email", JStrVal(authorEmail), "date", JStrVal(ctx.Store.TrialNowIso)),
				"committer", Obj("name", JStrVal(committerName), "email", JStrVal(committerEmail), "date", JStrVal(ctx.Store.TrialNowIso)),
				"tree", Obj("sha", JStrVal(entry.Commit.Tree)),
			),
			"parents", JArrVal(parents...),
			"tree", Obj("sha", JStrVal(entry.Commit.Tree)),
		))
	}
	page, link := paginateVals(ctx, vals)
	extra := map[string]string{}
	if link != "" {
		extra["Link"] = link
	}
	sendJSON(w, 200, JArrVal(page...), extra)
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

func decodeContentB64(s string) []byte {
	// Node's Buffer.from(str, 'base64') is lenient about whitespace; strip
	// it before decoding. Invalid characters after stripping still error,
	// which the caller treats as an empty buffer (matching Node's
	// stop-at-first-invalid behavior closely enough for well-formed input).
	clean := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		// Fall back to raw (unpadded) decoding; still invalid -> empty.
		data2, err2 := base64.RawStdEncoding.DecodeString(clean)
		if err2 != nil {
			return []byte{}
		}
		return data2
	}
	return data
}

func handleContents(ctx *Ctx, w http.ResponseWriter, repo *Repo, rest []string) {
	method := ctx.Method
	path := strings.Join(rest, "/")
	segments := make([]string, 0, len(rest))
	for _, seg := range rest {
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	lookup := repoLookup(repo)

	if method == "GET" {
		tip := resolveCommitTip(repo, ctx.Query.Get("ref"))
		if tip == "" {
			panic(apiErr(404, "Not Found"))
		}
		tipObj := lookup(tip)
		commit, err := parseCommit(tipObj.Data)
		if err != nil {
			panic(err)
		}
		if len(segments) == 0 {
			// Root directory listing.
			treeObj := lookup(commit.Tree)
			entries, err := parseTree(treeObj.Data)
			if err != nil {
				panic(err)
			}
			items := make([]*JVal, 0, len(entries))
			for _, e := range entries {
				size := 0
				typ := "file"
				if e.Mode == "40000" {
					typ = "dir"
				} else if obj := lookup(e.Oid); obj != nil {
					size = len(obj.Data)
				}
				items = append(items, Obj(
					"type", JStrVal(typ),
					"name", JStrVal(e.Name),
					"path", JStrVal(e.Name),
					"sha", JStrVal(e.Oid),
					"size", JIntVal(int64(size)),
				))
			}
			sendJSON(w, 200, JArrVal(items...))
			return
		}
		found := readPath(lookup, commit.Tree, segments)
		if found == nil {
			panic(apiErr(404, "Not Found"))
		}
		if found.Type == "blob" {
			data := lookup(found.Oid).Data
			sendJSON(w, 200, Obj(
				"type", JStrVal("file"),
				"encoding", JStrVal("base64"),
				"size", JIntVal(int64(len(data))),
				"name", JStrVal(segments[len(segments)-1]),
				"path", JStrVal(path),
				"sha", JStrVal(found.Oid),
				"content", JStrVal(base64.StdEncoding.EncodeToString(data)),
			))
			return
		}
		treeObj := lookup(found.Oid)
		entries, err := parseTree(treeObj.Data)
		if err != nil {
			panic(err)
		}
		items := make([]*JVal, 0, len(entries))
		for _, e := range entries {
			size := 0
			typ := "file"
			if e.Mode == "40000" {
				typ = "dir"
			} else if obj := lookup(e.Oid); obj != nil {
				size = len(obj.Data)
			}
			items = append(items, Obj(
				"type", JStrVal(typ),
				"name", JStrVal(e.Name),
				"path", JStrVal(path+"/"+e.Name),
				"sha", JStrVal(e.Oid),
				"size", JIntVal(int64(size)),
			))
		}
		sendJSON(w, 200, JArrVal(items...))
		return
	}

	if method == "PUT" {
		if len(segments) == 0 {
			panic(apiErr(422, "path is required"))
		}
		body, err := parseJSONBody(ctx)
		if err != nil {
			panic(err)
		}
		contentB64 := bodyString(body, "content")
		message := bodyStringOr(body, "message", "Update "+path)
		branch := bodyString(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		tip := branchTip(repo, branch)
		if tip == "" {
			// New branch from the default tip (or unborn when empty).
			if defaultTip := branchTip(repo, repo.DefaultBranch); defaultTip != "" {
				repo.Refs["refs/heads/"+branch] = defaultTip
				tip = defaultTip
			}
		}
		data := decodeContentB64(contentB64)
		blobOid := ctx.Store.PutObject(repo, ObjBlob, data)
		if tip != "" {
			tipObj := lookup(tip)
			commit, err := parseCommit(tipObj.Data)
			if err != nil {
				panic(err)
			}
			isUpdate := false
			if existing := readPath(lookup, commit.Tree, segments); existing != nil && existing.Type == "blob" {
				isUpdate = true
				if expectedSha := bodyString(body, "sha"); expectedSha != "" && expectedSha != existing.Oid {
					panic(apiErr(409, "sha does not match the current file"))
				}
			}
			newTree, err := setPath(lookup, func(typ ObjType, d []byte) string {
				return ctx.Store.PutObject(repo, typ, d)
			}, commit.Tree, segments, blobOid)
			if err != nil {
				panic(err)
			}
			newCommit := ctx.Store.MakeCommit(repo, BuildCommitFields{Tree: newTree, Parents: []string{tip}, Message: message})
			repo.Refs["refs/heads/"+branch] = newCommit
			status := 201
			if isUpdate {
				status = 200
			}
			sendJSON(w, status, Obj(
				"content", Obj(
					"name", JStrVal(segments[len(segments)-1]),
					"path", JStrVal(path),
					"sha", JStrVal(blobOid),
					"size", JIntVal(int64(len(data))),
					"encoding", JStrVal("base64"),
					"type", JStrVal("file"),
				),
				"commit", Obj("sha", JStrVal(newCommit), "message", JStrVal(message)),
			))
			return
		}
		// Unborn repo: first commit on the default branch.
		emptyTree := ctx.Store.PutObject(repo, ObjTree, []byte{})
		newTree, err := setPath(lookup, func(typ ObjType, d []byte) string {
			return ctx.Store.PutObject(repo, typ, d)
		}, emptyTree, segments, blobOid)
		if err != nil {
			panic(err)
		}
		newCommit := ctx.Store.MakeCommit(repo, BuildCommitFields{Tree: newTree, Parents: nil, Message: message})
		repo.Refs["refs/heads/"+branch] = newCommit
		sendJSON(w, 201, Obj(
			"content", Obj(
				"name", JStrVal(segments[len(segments)-1]),
				"path", JStrVal(path),
				"sha", JStrVal(blobOid),
				"size", JIntVal(int64(len(data))),
				"encoding", JStrVal("base64"),
				"type", JStrVal("file"),
			),
			"commit", Obj("sha", JStrVal(newCommit), "message", JStrVal(message)),
		))
		return
	}

	if method == "DELETE" {
		if len(segments) == 0 {
			panic(apiErr(422, "path is required"))
		}
		body, err := parseJSONBody(ctx)
		if err != nil {
			panic(err)
		}
		message := bodyStringOr(body, "message", "Delete "+path)
		branch := bodyString(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		tip := branchTip(repo, branch)
		if tip == "" {
			panic(apiErr(404, "Not Found"))
		}
		tipObj := lookup(tip)
		commit, err := parseCommit(tipObj.Data)
		if err != nil {
			panic(err)
		}
		existing := readPath(lookup, commit.Tree, segments)
		if existing == nil || existing.Type != "blob" {
			panic(apiErr(404, "Not Found"))
		}
		newTree, err := setPath(lookup, func(typ ObjType, d []byte) string {
			return ctx.Store.PutObject(repo, typ, d)
		}, commit.Tree, segments, "")
		if err != nil {
			panic(err)
		}
		newCommit := ctx.Store.MakeCommit(repo, BuildCommitFields{Tree: newTree, Parents: []string{tip}, Message: message})
		repo.Refs["refs/heads/"+branch] = newCommit
		sendJSON(w, 200, Obj(
			"content", JNullVal(),
			"commit", Obj("sha", JStrVal(newCommit), "message", JStrVal(message)),
		))
		return
	}

	panic(apiErr(404, "Not Found"))
}

// ---------------------------------------------------------------------------
// git/refs
// ---------------------------------------------------------------------------

func handleGitRefs(ctx *Ctx, w http.ResponseWriter, repo *Repo, rest []string) {
	// POST /repos/{o}/{r}/git/refs — rest is ['refs'] here.
	if len(rest) == 1 && rest[0] == "refs" && ctx.Method == "POST" {
		body, err := parseJSONBody(ctx)
		if err != nil {
			panic(err)
		}
		ref := bodyString(body, "ref")
		sha := bodyString(body, "sha")
		if !strings.HasPrefix(ref, "refs/") || !isObjectId(sha) {
			panic(apiErr(422, "ref and sha are required"))
		}
		obj, ok := repo.Objects[sha]
		if !ok || obj.Type != ObjCommit {
			panic(apiErr(422, "sha does not name a commit"))
		}
		if _, exists := repo.Refs[ref]; exists {
			panic(apiErr(422, "Reference already exists"))
		}
		repo.Refs[ref] = sha
		sendJSON(w, 201, Obj(
			"ref", JStrVal(ref),
			"object", Obj("sha", JStrVal(sha), "type", JStrVal("commit")),
			"sha", JStrVal(sha),
			"commit", Obj("sha", JStrVal(sha)),
		))
		return
	}
	if len(rest) >= 3 && rest[0] == "refs" && rest[1] == "heads" && ctx.Method == "GET" {
		branch := strings.Join(rest[2:], "/")
		tip := branchTip(repo, branch)
		if tip == "" {
			panic(apiErr(404, "Not Found"))
		}
		sendJSON(w, 200, Obj(
			"ref", JStrVal("refs/heads/"+branch),
			"object", Obj("sha", JStrVal(tip), "type", JStrVal("commit")),
			"sha", JStrVal(tip),
			"commit", Obj("sha", JStrVal(tip)),
		))
		return
	}
	panic(apiErr(404, "Not Found"))
}
