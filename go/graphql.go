// graphql.go — a small hand-rolled GraphQL executor for the fixed document
// shapes the suite sends: repository/viewer/node/__type queries, issue and
// pull-request connections with cursor pagination, and the five mutations.
// No schema engine: a lexer + recursive-descent parser for the subset used,
// plus resolvers over the shared store.
//
// Unknown fields produce HTTP 200 with an `errors` entry naming the field
// (GitHub-style); an unknown repo resolves to `data.repository: null` with
// no errors.

package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tokPunct tokenKind = iota
	tokName
	tokString
	tokInt
	tokFloat
)

type gqlToken struct {
	kind  tokenKind
	value string
	num   float64
}

func gqlLex(input string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	for i < len(input) {
		ch := input[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
		case ch == '#':
			for i < len(input) && input[i] != '\n' {
				i++
			}
		case ch == '.' && strings.HasPrefix(input[i:], "..."):
			tokens = append(tokens, gqlToken{kind: tokPunct, value: "..."})
			i += 3
		case strings.ContainsRune("{}():$![]=@", rune(ch)):
			tokens = append(tokens, gqlToken{kind: tokPunct, value: string(ch)})
			i++
		case ch == '"':
			var sb strings.Builder
			i++
			for i < len(input) && input[i] != '"' {
				if input[i] == '\\' && i+1 < len(input) {
					esc := input[i+1]
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					default:
						sb.WriteByte(esc)
					}
					i += 2
				} else {
					sb.WriteByte(input[i])
					i++
				}
			}
			i++
			tokens = append(tokens, gqlToken{kind: tokString, value: sb.String()})
		case ch >= '0' && ch <= '9' || ch == '-':
			j := i
			if input[j] == '-' {
				j++
			}
			for j < len(input) && input[j] >= '0' && input[j] <= '9' {
				j++
			}
			if j < len(input) && input[j] == '.' {
				j++
				for j < len(input) && input[j] >= '0' && input[j] <= '9' {
					j++
				}
				f, _ := strconv.ParseFloat(input[i:j], 64)
				tokens = append(tokens, gqlToken{kind: tokFloat, value: input[i:j], num: f})
			} else {
				n, _ := strconv.ParseInt(input[i:j], 10, 64)
				tokens = append(tokens, gqlToken{kind: tokInt, value: input[i:j], num: float64(n)})
			}
			i = j
		case ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch == '_':
			j := i
			for j < len(input) && (input[j] >= 'A' && input[j] <= 'Z' || input[j] >= 'a' && input[j] <= 'z' || input[j] >= '0' && input[j] <= '9' || input[j] == '_') {
				j++
			}
			tokens = append(tokens, gqlToken{kind: tokName, value: input[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q in GraphQL document", string(ch))
		}
	}
	return tokens, nil
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

// gqlValue is a parsed argument value. Variables and enums are tagged so
// they can be resolved against the operation's variables at execution time.
type gqlValue struct {
	kind     string // "string" | "int" | "float" | "bool" | "null" | "var" | "enum" | "list" | "object"
	str      string
	num      float64
	boolean  bool
	list     []*gqlValue
	obj      map[string]*gqlValue
	objOrder []string
}

type gqlField struct {
	name       string
	alias      string
	args       map[string]*gqlValue
	selections []gqlSelection
}

type gqlInlineFragment struct {
	typeCondition string
	selections    []gqlSelection
}

type gqlSelection struct {
	isFragment bool
	field      *gqlField
	fragment   *gqlInlineFragment
}

type gqlOperation struct {
	kind       string // "query" | "mutation"
	name       string
	selections []gqlSelection
}

type gqlParser struct {
	pos    int
	tokens []gqlToken
}

func (p *gqlParser) peek() *gqlToken {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *gqlParser) next() (gqlToken, error) {
	if p.pos >= len(p.tokens) {
		return gqlToken{}, fmt.Errorf("unexpected end of GraphQL document")
	}
	t := p.tokens[p.pos]
	p.pos++
	return t, nil
}

func (p *gqlParser) expectPunct(value string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != tokPunct || t.value != value {
		return fmt.Errorf("expected %s in GraphQL document", value)
	}
	return nil
}

func (p *gqlParser) skipPunct(value string) bool {
	t := p.peek()
	if t != nil && t.kind == tokPunct && t.value == value {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) parseDocument() ([]*gqlOperation, error) {
	var operations []*gqlOperation
	for p.pos < len(p.tokens) {
		t := p.peek()
		if t.kind == tokPunct && t.value == "{" {
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			operations = append(operations, &gqlOperation{kind: "query", selections: sels})
			continue
		}
		if t.kind == tokName && (t.value == "query" || t.value == "mutation") {
			kind := t.value
			p.next()
			name := ""
			if nt := p.peek(); nt != nil && nt.kind == tokName {
				name = nt.value
				p.next()
			}
			// Variable definitions: ($x: Type, ...) — parsed, types discarded.
			if p.skipPunct("(") {
				for !p.skipPunct(")") {
					if _, err := p.next(); err != nil {
						return nil, err
					}
					if _, err := p.next(); err != nil {
						return nil, err
					}
					if err := p.expectPunct(":"); err != nil {
						return nil, err
					}
					if err := p.parseType(); err != nil {
						return nil, err
					}
					if p.skipPunct("=") {
						if _, err := p.parseValue(); err != nil {
							return nil, err
						}
					}
				}
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			operations = append(operations, &gqlOperation{kind: kind, name: name, selections: sels})
			continue
		}
		if t.kind == tokName && t.value == "fragment" {
			// Named fragment definitions are not used by the suite; skip them.
			p.next()
			p.next() // fragment name
			p.next() // 'on'
			p.next() // type name
			if _, err := p.parseSelectionSet(); err != nil {
				return nil, err
			}
			continue
		}
		return nil, fmt.Errorf("unexpected token %q in GraphQL document", t.value)
	}
	return operations, nil
}

func (p *gqlParser) parseType() error {
	t := p.peek()
	if t == nil {
		return nil
	}
	if t.kind == tokPunct && t.value == "[" {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		return p.expectPunct("]")
	}
	if t.kind == tokName {
		p.next()
	}
	p.skipPunct("!")
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var selections []gqlSelection
	for !p.skipPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, sel)
	}
	return selections, nil
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	t := p.peek()
	if t == nil {
		return gqlSelection{}, fmt.Errorf("unexpected end of GraphQL document")
	}
	if t.kind == tokPunct && t.value == "..." {
		p.next()
		next := p.peek()
		if next != nil && next.kind == tokName && next.value == "on" {
			p.next()
			typeToken, err := p.next()
			if err != nil {
				return gqlSelection{}, err
			}
			cond := ""
			if typeToken.kind == tokName {
				cond = typeToken.value
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return gqlSelection{}, err
			}
			return gqlSelection{isFragment: true, fragment: &gqlInlineFragment{typeCondition: cond, selections: sels}}, nil
		}
		if next != nil && next.kind == tokName {
			// Fragment spread (named) — not used by the suite; treat as empty.
			p.next()
			return gqlSelection{isFragment: true, fragment: &gqlInlineFragment{selections: nil}}, nil
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
		return gqlSelection{isFragment: true, fragment: &gqlInlineFragment{selections: sels}}, nil
	}
	if t.kind != tokName {
		return gqlSelection{}, fmt.Errorf("expected a field name in GraphQL document")
	}
	first, err := p.next()
	if err != nil {
		return gqlSelection{}, err
	}
	alias := ""
	name := first.value
	if p.skipPunct(":") {
		alias = first.value
		nt, err := p.next()
		if err != nil {
			return gqlSelection{}, err
		}
		name = nt.value
	}
	args := map[string]*gqlValue{}
	if p.skipPunct("(") {
		parsed, err := p.parseArguments()
		if err != nil {
			return gqlSelection{}, err
		}
		args = parsed
	}
	// Directives (@...) — skipped.
	for {
		peek := p.peek()
		if peek == nil || peek.kind != tokPunct || peek.value != "@" {
			break
		}
		p.next()
		p.next()
		if p.skipPunct("(") {
			if _, err := p.parseArguments(); err != nil {
				return gqlSelection{}, err
			}
		}
	}
	var selections []gqlSelection
	if peek := p.peek(); peek != nil && peek.kind == tokPunct && peek.value == "{" {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
		selections = sels
	}
	return gqlSelection{field: &gqlField{name: name, alias: alias, args: args, selections: selections}}, nil
}

func (p *gqlParser) parseArguments() (map[string]*gqlValue, error) {
	args := map[string]*gqlValue{}
	for !p.skipPunct(")") {
		nameToken, err := p.next()
		if err != nil {
			return nil, err
		}
		if nameToken.kind != tokName {
			return nil, fmt.Errorf("expected an argument name")
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[nameToken.value] = v
	}
	return args, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	switch {
	case t.kind == tokPunct && t.value == "$":
		nameToken, err := p.next()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "var", str: nameToken.value}, nil
	case t.kind == tokPunct && t.value == "[":
		var list []*gqlValue
		for !p.skipPunct("]") {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return &gqlValue{kind: "list", list: list}, nil
	case t.kind == tokPunct && t.value == "{":
		obj := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
		for !p.skipPunct("}") {
			nameToken, err := p.next()
			if err != nil {
				return nil, err
			}
			if nameToken.kind != tokName {
				return nil, fmt.Errorf("expected an object field name")
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			obj.obj[nameToken.value] = v
			obj.objOrder = append(obj.objOrder, nameToken.value)
		}
		return obj, nil
	case t.kind == tokString:
		return &gqlValue{kind: "string", str: t.value}, nil
	case t.kind == tokInt:
		return &gqlValue{kind: "int", num: t.num}, nil
	case t.kind == tokFloat:
		return &gqlValue{kind: "float", num: t.num}, nil
	case t.kind == tokName:
		switch t.value {
		case "true":
			return &gqlValue{kind: "bool", boolean: true}, nil
		case "false":
			return &gqlValue{kind: "bool", boolean: false}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", str: t.value}, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %q", t.value)
}

// resolveVariables substitutes $variables in a parsed argument value.
// Variables are JVals (from the request's "variables" object).
func resolveGQLValue(v *gqlValue, variables *JVal) *JVal {
	switch v.kind {
	case "var":
		if variables != nil {
			if got := variables.Get(v.str); got != nil {
				return got
			}
		}
		return JNullVal()
	case "enum":
		return JStrVal(v.str)
	case "string":
		return JStrVal(v.str)
	case "int":
		return JIntVal(int64(v.num))
	case "float":
		return JFloatVal(v.num)
	case "bool":
		return JBoolVal(v.boolean)
	case "null":
		return JNullVal()
	case "list":
		items := make([]*JVal, 0, len(v.list))
		for _, item := range v.list {
			items = append(items, resolveGQLValue(item, variables))
		}
		return JArrVal(items...)
	case "object":
		obj := &JVal{Kind: JObject}
		for _, k := range v.objOrder {
			obj.O = append(obj.O, JPair{K: k, V: resolveGQLValue(v.obj[k], variables)})
		}
		return obj
	}
	return JNullVal()
}

func resolveGQLArgs(args map[string]*gqlValue, variables *JVal) map[string]*JVal {
	out := map[string]*JVal{}
	for k, v := range args {
		out[k] = resolveGQLValue(v, variables)
	}
	return out
}

// ---------------------------------------------------------------------------
// Static introspection tables
// ---------------------------------------------------------------------------

var issueFields = []string{"id", "number", "title", "state", "body", "url", "createdAt"}
var pullRequestFields = []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body", "url"}
var repositoryFields = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "url"}
var searchTypeValues = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT", "MILESTONE"}

func typeFields(typeName string) []string {
	switch typeName {
	case "Issue":
		return issueFields
	case "PullRequest":
		return pullRequestFields
	case "Repository":
		return repositoryFields
	}
	return nil
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// Resolved is one field's resolved value plus the GraphQL type it carries.
type Resolved struct {
	Value any
	Type  string
}

// GQLIssue / GQLPull pair an item with its repo (the oracle tags the repo
// onto the item with a hidden property; here it is a plain wrapper).
type GQLIssue struct {
	Repo  *Repo
	Issue *IssueItem
}

type GQLPull struct {
	Repo *Repo
	Pull *PullItem
}

type gqlUser struct{ Login string }
type gqlRef struct{ Name string }

// gqlConn is a connection value (issues, pulls, or changed files).
type gqlConn struct {
	nodes      []any // *GQLIssue, *GQLPull, or *JVal ({path})
	pageInfo   *JVal
	totalCount int
}

type gqlEdge struct {
	node   any
	cursor string
}

type gqlTypeInfo struct {
	name       string
	kind       string // "OBJECT" | "ENUM"
	fields     []string
	enumValues []string
}

type execCtx struct {
	store  *Store
	errors []string
}

func (c *execCtx) pushError(msg string) { c.errors = append(c.errors, msg) }

func cursorFor(repo *Repo, number int) string {
	return b64enc("cursor:" + strconv.Itoa(repo.ID) + ":" + strconv.Itoa(number))
}

func decodeCursor(cursor string) (int, bool) {
	text, err := b64dec(cursor)
	if err != nil {
		return 0, false
	}
	var repoID, number int
	n, err := fmt.Sscanf(text, "cursor:%d:%d", &repoID, &number)
	if err != nil || n != 2 {
		return 0, false
	}
	return number, true
}

func pageInfoFor(nodes []any, repo *Repo, hasNextPage bool) *JVal {
	var startCursor, endCursor *JVal = JNullVal(), JNullVal()
	if len(nodes) > 0 {
		if iss, ok := nodes[0].(*GQLIssue); ok {
			startCursor = JStrVal(cursorFor(repo, iss.Issue.Number))
		} else if pr, ok := nodes[0].(*GQLPull); ok {
			startCursor = JStrVal(cursorFor(repo, pr.Pull.Number))
		}
		if iss, ok := nodes[len(nodes)-1].(*GQLIssue); ok {
			endCursor = JStrVal(cursorFor(repo, iss.Issue.Number))
		} else if pr, ok := nodes[len(nodes)-1].(*GQLPull); ok {
			endCursor = JStrVal(cursorFor(repo, pr.Pull.Number))
		}
	}
	return Obj(
		"hasNextPage", JBoolVal(hasNextPage),
		"hasPreviousPage", JBoolVal(false),
		"startCursor", startCursor,
		"endCursor", endCursor,
	)
}

func issueConnection(repo *Repo, args map[string]*JVal) Resolved {
	items := make([]*IssueItem, 0, len(repo.Issues))
	for _, i := range repo.Issues {
		items = append(items, i)
	}
	if states := args["states"]; states != nil && states.IsArray() && len(states.A) > 0 {
		wanted := map[string]bool{}
		for _, s := range states.A {
			if str, ok := s.Str(); ok {
				wanted[strings.ToUpper(str)] = true
			}
		}
		var filtered []*IssueItem
		for _, i := range items {
			if wanted[strings.ToUpper(i.State)] {
				filtered = append(filtered, i)
			}
		}
		items = filtered
	}
	field := "CREATED_AT"
	direction := "ASC"
	if ob := args["orderBy"]; ob != nil && ob.IsObject() {
		if f := ob.Get("field"); f != nil {
			if s, ok := f.Str(); ok {
				field = s
			}
		}
		if d := ob.Get("direction"); d != nil {
			if s, ok := d.Str(); ok {
				direction = s
			}
		}
	}
	if field == "CREATED_AT" {
		if direction == "DESC" {
			for l, r := 0, len(items)-1; l < r; l, r = l+1, r-1 {
				items[l], items[r] = items[r], items[l]
			}
		}
	}
	if after := args["after"]; after != nil {
		if s, ok := after.Str(); ok {
			if afterNumber, ok := decodeCursor(s); ok {
				idx := -1
				for i, it := range items {
					if it.Number == afterNumber {
						idx = i
						break
					}
				}
				if idx >= 0 {
					items = items[idx+1:]
				}
			}
		}
	}
	first := len(items)
	if f := args["first"]; f != nil {
		if n, ok := f.Int(); ok {
			first = int(n)
		}
	}
	if first > len(items) {
		first = len(items)
	}
	if first < 0 {
		first = 0
	}
	page := items[:first]
	hasNext := len(items) > first
	nodes := make([]any, 0, len(page))
	for _, it := range page {
		nodes = append(nodes, &GQLIssue{Repo: repo, Issue: it})
	}
	return Resolved{
		Value: &gqlConn{nodes: nodes, pageInfo: pageInfoFor(nodes, repo, hasNext), totalCount: len(items)},
		Type:  "IssueConnection",
	}
}

func pullRequestConnection(repo *Repo, args map[string]*JVal) Resolved {
	var items []*PullItem
	for _, p := range repo.Pulls {
		if p.State == "open" {
			items = append(items, p)
		}
	}
	first := len(items)
	if f := args["first"]; f != nil {
		if n, ok := f.Int(); ok {
			first = int(n)
		}
	}
	if first > len(items) {
		first = len(items)
	}
	if first < 0 {
		first = 0
	}
	page := items[:first]
	nodes := make([]any, 0, len(page))
	for _, pr := range page {
		nodes = append(nodes, &GQLPull{Repo: repo, Pull: pr})
	}
	return Resolved{
		Value: &gqlConn{nodes: nodes, pageInfo: pageInfoFor(nodes, repo, len(items) > first), totalCount: len(items)},
		Type:  "PullRequestConnection",
	}
}

func mergeableOf(store *Store, repo *Repo, pr *PullItem) string {
	if pr.Merged {
		return "UNKNOWN"
	}
	lookup := repoLookup(repo)
	baseTip := repo.Refs["refs/heads/"+pr.BaseRef]
	if baseTip == "" {
		return "UNKNOWN"
	}
	baseObj := lookup(baseTip)
	headObj := lookup(pr.HeadSha)
	if baseObj == nil || headObj == nil {
		return "UNKNOWN"
	}
	baseCommit, err1 := parseCommit(baseObj.Data)
	headCommit, err2 := parseCommit(headObj.Data)
	if err1 != nil || err2 != nil {
		return "UNKNOWN"
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
	if mergeConflicts(lookup, mbTree, headCommit.Tree, baseCommit.Tree) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

func resolveRepositoryField(ctx *execCtx, repo *Repo, name string, args map[string]*JVal) *Resolved {
	store := ctx.store
	switch name {
	case "id":
		return &Resolved{Value: store.RepoGlobalID(repo), Type: "ID"}
	case "databaseId":
		return &Resolved{Value: int64(repo.ID), Type: "Int"}
	case "name":
		return &Resolved{Value: repo.Name, Type: "String"}
	case "nameWithOwner":
		return &Resolved{Value: repo.Owner + "/" + repo.Name, Type: "String"}
	case "description":
		return &Resolved{Value: repo.Description, Type: "String"}
	case "url":
		return &Resolved{Value: "https://github.com/" + repo.Owner + "/" + repo.Name, Type: "String"}
	case "owner":
		return &Resolved{Value: gqlUser{Login: repo.Owner}, Type: "User"}
	case "defaultBranchRef":
		tip := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if tip == "" {
			return &Resolved{Value: nil, Type: "Ref"}
		}
		return &Resolved{Value: gqlRef{Name: repo.DefaultBranch}, Type: "Ref"}
	case "viewerPermission":
		return &Resolved{Value: "ADMIN", Type: "String"}
	case "hasIssuesEnabled":
		return &Resolved{Value: true, Type: "Boolean"}
	case "issue":
		number := 0
		if n := args["number"]; n != nil {
			if v, ok := n.Int(); ok {
				number = int(v)
			}
		}
		var found *IssueItem
		for _, i := range repo.Issues {
			if i.Number == number {
				found = i
				break
			}
		}
		if found == nil {
			return &Resolved{Value: nil, Type: "Issue"}
		}
		return &Resolved{Value: &GQLIssue{Repo: repo, Issue: found}, Type: "Issue"}
	case "issues":
		r := issueConnection(repo, args)
		return &r
	case "pullRequests":
		r := pullRequestConnection(repo, args)
		return &r
	case "pullRequest":
		number := 0
		if n := args["number"]; n != nil {
			if v, ok := n.Int(); ok {
				number = int(v)
			}
		}
		var found *PullItem
		for _, p := range repo.Pulls {
			if p.Number == number {
				found = p
				break
			}
		}
		if found == nil {
			return &Resolved{Value: nil, Type: "PullRequest"}
		}
		return &Resolved{Value: &GQLPull{Repo: repo, Pull: found}, Type: "PullRequest"}
	}
	return nil
}

func resolveIssueField(ctx *execCtx, repo *Repo, issue *IssueItem, name string) *Resolved {
	switch name {
	case "id":
		return &Resolved{Value: ctx.store.IssueGlobalID(repo, issue.Number), Type: "ID"}
	case "number":
		return &Resolved{Value: int64(issue.Number), Type: "Int"}
	case "title":
		return &Resolved{Value: issue.Title, Type: "String"}
	case "state":
		return &Resolved{Value: strings.ToUpper(issue.State), Type: "String"}
	case "body":
		return &Resolved{Value: issue.Body, Type: "String"}
	case "url":
		return &Resolved{Value: fmt.Sprintf("https://github.com/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number), Type: "String"}
	case "createdAt":
		return &Resolved{Value: ctx.store.TrialNowIso, Type: "String"}
	}
	return nil
}

func resolvePullField(ctx *execCtx, repo *Repo, pr *PullItem, name string, args map[string]*JVal) *Resolved {
	switch name {
	case "id":
		return &Resolved{Value: ctx.store.PullGlobalID(repo, pr.Number), Type: "ID"}
	case "number":
		return &Resolved{Value: int64(pr.Number), Type: "Int"}
	case "title":
		return &Resolved{Value: pr.Title, Type: "String"}
	case "body":
		return &Resolved{Value: pr.Body, Type: "String"}
	case "state":
		if pr.Merged {
			return &Resolved{Value: "MERGED", Type: "String"}
		}
		return &Resolved{Value: strings.ToUpper(pr.State), Type: "String"}
	case "merged":
		return &Resolved{Value: pr.Merged, Type: "Boolean"}
	case "headRefName":
		return &Resolved{Value: pr.HeadRef, Type: "String"}
	case "baseRefName":
		return &Resolved{Value: pr.BaseRef, Type: "String"}
	case "mergeable":
		return &Resolved{Value: mergeableOf(ctx.store, repo, pr), Type: "String"}
	case "url":
		return &Resolved{Value: fmt.Sprintf("https://github.com/%s/%s/pull/%d", repo.Owner, repo.Name, pr.Number), Type: "String"}
	case "files":
		first := len(pr.Files)
		if f := args["first"]; f != nil {
			if n, ok := f.Int(); ok {
				first = int(n)
			}
		}
		if first > len(pr.Files) {
			first = len(pr.Files)
		}
		if first < 0 {
			first = 0
		}
		nodes := make([]any, 0, first)
		for _, f := range pr.Files[:first] {
			nodes = append(nodes, Obj("path", JStrVal(f.Filename)))
		}
		return &Resolved{
			Value: &gqlConn{nodes: nodes, pageInfo: nil, totalCount: 0},
			Type:  "PullRequestChangedFileConnection",
		}
	}
	return nil
}

// resolveField resolves one field on a parent value; nil means "unknown field".
func resolveField(ctx *execCtx, parent Resolved, name string, args map[string]*JVal) *Resolved {
	store := ctx.store
	if parent.Value == nil {
		return nil
	}
	switch parent.Type {
	case "Query":
		switch name {
		case "viewer":
			return &Resolved{Value: gqlUser{Login: store.ViewerLogin}, Type: "User"}
		case "repository":
			owner, _ := argString(args["owner"])
			rname, _ := argString(args["name"])
			repo := store.GetRepo(owner, rname)
			if repo == nil {
				return &Resolved{Value: nil, Type: "Repository"}
			}
			return &Resolved{Value: repo, Type: "Repository"}
		case "node":
			id, _ := argString(args["id"])
			kind, parts, ok := store.DecodeGlobalID(id)
			if !ok {
				return &Resolved{Value: nil, Type: "Node"}
			}
			switch kind {
			case "R":
				dbID, _ := strconv.Atoi(parts[0])
				repo := store.FindRepoByDbID(dbID)
				if repo == nil {
					return &Resolved{Value: nil, Type: "Repository"}
				}
				return &Resolved{Value: repo, Type: "Repository"}
			case "I":
				dbID, _ := strconv.Atoi(parts[0])
				num, _ := strconv.Atoi(parts[1])
				repo := store.FindRepoByDbID(dbID)
				var issue *IssueItem
				if repo != nil {
					for _, i := range repo.Issues {
						if i.Number == num {
							issue = i
							break
						}
					}
				}
				if issue == nil {
					return &Resolved{Value: nil, Type: "Issue"}
				}
				return &Resolved{Value: &GQLIssue{Repo: repo, Issue: issue}, Type: "Issue"}
			case "PR":
				dbID, _ := strconv.Atoi(parts[0])
				num, _ := strconv.Atoi(parts[1])
				repo := store.FindRepoByDbID(dbID)
				var pr *PullItem
				if repo != nil {
					for _, p := range repo.Pulls {
						if p.Number == num {
							pr = p
							break
						}
					}
				}
				if pr == nil {
					return &Resolved{Value: nil, Type: "PullRequest"}
				}
				return &Resolved{Value: &GQLPull{Repo: repo, Pull: pr}, Type: "PullRequest"}
			}
			return &Resolved{Value: nil, Type: "Node"}
		case "__type":
			typeName, _ := argString(args["name"])
			if typeName == "SearchType" {
				return &Resolved{Value: &gqlTypeInfo{name: typeName, kind: "ENUM", enumValues: searchTypeValues}, Type: "__Type"}
			}
			fields := typeFields(typeName)
			if fields == nil {
				return &Resolved{Value: nil, Type: "__Type"}
			}
			return &Resolved{Value: &gqlTypeInfo{name: typeName, kind: "OBJECT", fields: fields}, Type: "__Type"}
		case "__schema":
			return &Resolved{Value: nil, Type: "__Schema"}
		}
		return nil
	case "Mutation":
		return nil // mutations are handled at the operation level
	case "Repository":
		if repo, ok := parent.Value.(*Repo); ok {
			return resolveRepositoryField(ctx, repo, name, args)
		}
		return nil
	case "Issue":
		if g, ok := parent.Value.(*GQLIssue); ok {
			return resolveIssueField(ctx, g.Repo, g.Issue, name)
		}
		return nil
	case "PullRequest":
		if g, ok := parent.Value.(*GQLPull); ok {
			return resolvePullField(ctx, g.Repo, g.Pull, name, args)
		}
		return nil
	case "User":
		if u, ok := parent.Value.(gqlUser); ok && name == "login" {
			return &Resolved{Value: u.Login, Type: "String"}
		}
		return nil
	case "Ref":
		if r, ok := parent.Value.(gqlRef); ok && name == "name" {
			return &Resolved{Value: r.Name, Type: "String"}
		}
		return nil
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection":
		conn, ok := parent.Value.(*gqlConn)
		if !ok {
			return nil
		}
		switch name {
		case "nodes":
			elementType := "Issue"
			if parent.Type == "PullRequestConnection" {
				elementType = "PullRequest"
			} else if parent.Type == "PullRequestChangedFileConnection" {
				elementType = "PullRequestChangedFile"
			}
			return &Resolved{Value: conn.nodes, Type: "Array:" + elementType}
		case "pageInfo":
			if conn.pageInfo == nil {
				return &Resolved{Value: nil, Type: "PageInfo"}
			}
			return &Resolved{Value: conn.pageInfo, Type: "PageInfo"}
		case "totalCount":
			return &Resolved{Value: int64(conn.totalCount), Type: "Int"}
		case "edges":
			edges := make([]any, 0, len(conn.nodes))
			for _, n := range conn.nodes {
				edges = append(edges, &gqlEdge{node: n, cursor: ""})
			}
			return &Resolved{Value: edges, Type: "Array:Edge"}
		}
		return nil
	case "PageInfo":
		if jv, ok := parent.Value.(*JVal); ok {
			if got := jv.Get(name); got != nil {
				typ := "String"
				if got.Kind == JBool {
					typ = "Boolean"
				}
				return &Resolved{Value: jvScalar(got), Type: typ}
			}
		}
		return nil
	case "PullRequestChangedFile":
		if jv, ok := parent.Value.(*JVal); ok && name == "path" {
			if got := jv.Get("path"); got != nil {
				if s, ok := got.Str(); ok {
					return &Resolved{Value: s, Type: "String"}
				}
			}
		}
		return nil
	case "IssueCommentEdge":
		if e, ok := parent.Value.(*gqlEdge); ok && name == "node" {
			return &Resolved{Value: e.node, Type: "IssueComment"}
		}
		return nil
	case "IssueComment":
		if jv, ok := parent.Value.(*JVal); ok {
			if name == "id" || name == "body" {
				if got := jv.Get(name); got != nil {
					if s, ok := got.Str(); ok {
						return &Resolved{Value: s, Type: "String"}
					}
				}
			}
		}
		return nil
	case "__Type":
		if info, ok := parent.Value.(*gqlTypeInfo); ok {
			switch name {
			case "name", "kind":
				if name == "name" {
					return &Resolved{Value: info.name, Type: "String"}
				}
				return &Resolved{Value: info.kind, Type: "String"}
			case "fields":
				nodes := make([]any, 0, len(info.fields))
				for _, f := range info.fields {
					nodes = append(nodes, Obj("name", JStrVal(f)))
				}
				return &Resolved{Value: nodes, Type: "Array:__Field"}
			case "enumValues":
				nodes := make([]any, 0, len(info.enumValues))
				for _, v := range info.enumValues {
					nodes = append(nodes, Obj("name", JStrVal(v)))
				}
				return &Resolved{Value: nodes, Type: "Array:__EnumValue"}
			}
		}
		return nil
	case "__Field":
		if jv, ok := parent.Value.(*JVal); ok && name == "name" {
			if got := jv.Get("name"); got != nil {
				if s, ok := got.Str(); ok {
					return &Resolved{Value: s, Type: "String"}
				}
			}
		}
		return nil
	case "__EnumValue":
		if jv, ok := parent.Value.(*JVal); ok && name == "name" {
			if got := jv.Get("name"); got != nil {
				if s, ok := got.Str(); ok {
					return &Resolved{Value: s, Type: "String"}
				}
			}
		}
		return nil
	}
	return nil
}

// jvScalar converts a JVal scalar to a Go scalar for Resolved values.
func jvScalar(v *JVal) any {
	if v == nil {
		return nil
	}
	switch v.Kind {
	case JString:
		return v.S
	case JBool:
		return v.B
	case JInt:
		return v.I
	case JFloat:
		return v.F
	default:
		return nil
	}
}

// argString coerces a JVal to a string the way JavaScript's String() does
// for the scalar kinds that can appear in GraphQL arguments.
func argString(v *JVal) (string, bool) {
	if v == nil {
		return "", false
	}
	switch v.Kind {
	case JString:
		return v.S, true
	case JInt:
		return strconv.FormatInt(v.I, 10), true
	case JFloat:
		return jsFloat(v.F), true
	case JBool:
		if v.B {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

// executeSelections executes a selection set against a parent value.
func executeSelections(ctx *execCtx, parent Resolved, selections []gqlSelection, variables *JVal) *JVal {
	out := &JVal{Kind: JObject}
	for _, sel := range selections {
		if sel.isFragment {
			if sel.fragment.typeCondition != "" && sel.fragment.typeCondition != parent.Type && parent.Type != "Node" {
				continue
			}
			frag := executeSelections(ctx, parent, sel.fragment.selections, variables)
			out.O = append(out.O, frag.O...)
			continue
		}
		f := sel.field
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		if f.name == "__typename" {
			out.O = append(out.O, JPair{K: key, V: JStrVal(parent.Type)})
			continue
		}
		args := resolveGQLArgs(f.args, variables)
		resolved := resolveField(ctx, parent, f.name, args)
		if resolved == nil {
			ctx.pushError(fmt.Sprintf("Cannot query field %q on type %q.", f.name, parent.Type))
			continue
		}
		out.O = append(out.O, JPair{K: key, V: materialize(ctx, *resolved, f.selections, variables)})
	}
	return out
}

func materialize(ctx *execCtx, resolved Resolved, selections []gqlSelection, variables *JVal) *JVal {
	if resolved.Value == nil {
		return JNullVal()
	}
	if len(selections) == 0 {
		switch v := resolved.Value.(type) {
		case string:
			return JStrVal(v)
		case int64:
			return JIntVal(v)
		case bool:
			return JBoolVal(v)
		case *JVal:
			return v
		default:
			return JNullVal()
		}
	}
	if strings.HasPrefix(resolved.Type, "Array:") {
		elementType := resolved.Type[len("Array:"):]
		items, _ := resolved.Value.([]any)
		arr := &JVal{Kind: JArray}
		for _, item := range items {
			if elementType == "Edge" {
				edge, _ := item.(*gqlEdge)
				// Faithful to the oracle: an edge's selections are executed
				// against its node as an Issue.
				arr.A = append(arr.A, executeSelections(ctx, Resolved{Value: edge.node, Type: "Issue"}, selections, variables))
				continue
			}
			arr.A = append(arr.A, executeSelections(ctx, Resolved{Value: item, Type: elementType}, selections, variables))
		}
		return arr
	}
	switch resolved.Value.(type) {
	case *JVal:
		return executeSelections(ctx, resolved, selections, variables)
	case *Repo, *GQLIssue, *GQLPull, gqlUser, gqlRef, *gqlConn, *gqlTypeInfo:
		return executeSelections(ctx, resolved, selections, variables)
	default:
		return JNullVal()
	}
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

// A mutation payload: the fields the mutation returns, plus its type.
type gqlPayload struct {
	value map[string]*Resolved
	typ   string
}

func repoFromGlobalID(store *Store, id string) *Repo {
	kind, parts, ok := store.DecodeGlobalID(id)
	if !ok || kind != "R" {
		return nil
	}
	dbID, _ := strconv.Atoi(parts[0])
	return store.FindRepoByDbID(dbID)
}

func executeMutation(ctx *execCtx, op *gqlOperation, variables *JVal) *JVal {
	data := &JVal{Kind: JObject}
	for _, sel := range op.selections {
		if sel.isFragment {
			continue
		}
		f := sel.field
		args := resolveGQLArgs(f.args, variables)
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		payload := executeMutationField(ctx, f.name, args)
		if payload == nil {
			ctx.pushError(fmt.Sprintf("Unknown mutation field %q.", f.name))
			continue
		}
		data.O = append(data.O, JPair{K: key, V: executePayloadSelections(ctx, payload, f.selections, variables)})
	}
	return data
}

func executePayloadSelections(ctx *execCtx, payload *gqlPayload, selections []gqlSelection, variables *JVal) *JVal {
	out := &JVal{Kind: JObject}
	for _, sel := range selections {
		if sel.isFragment {
			continue
		}
		f := sel.field
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		if f.name == "__typename" {
			out.O = append(out.O, JPair{K: key, V: JStrVal(payload.typ)})
			continue
		}
		wrapped, present := payload.value[f.name]
		if wrapped == nil || !present {
			if present {
				out.O = append(out.O, JPair{K: key, V: JNullVal()})
			} else {
				ctx.pushError(fmt.Sprintf("Cannot query field %q on type %q.", f.name, payload.typ))
			}
			continue
		}
		if f.name == "commentEdge" {
			// commentEdge's selections are executed against the edge.
			out.O = append(out.O, JPair{K: key, V: executeSelections(ctx, *wrapped, f.selections, variables)})
		} else {
			out.O = append(out.O, JPair{K: key, V: materialize(ctx, *wrapped, f.selections, variables)})
		}
	}
	return out
}

// executeMutationField runs one mutation and returns its payload.
func executeMutationField(ctx *execCtx, name string, args map[string]*JVal) *gqlPayload {
	store := ctx.store
	input := args["input"]
	if input == nil || !input.IsObject() {
		input = &JVal{Kind: JObject}
	}
	inputStr := func(key string) string {
		if v := input.Get(key); v != nil {
			s, _ := argString(v)
			return s
		}
		return ""
	}
	switch name {
	case "createIssue":
		repo := repoFromGlobalID(store, inputStr("repositoryId"))
		if repo == nil {
			ctx.pushError("Could not resolve to a Repository with the id provided.")
			return &gqlPayload{typ: "CreateIssuePayload", value: map[string]*Resolved{"issue": nil}}
		}
		issue := store.CreateIssue(repo, inputStr("title"), inputStr("body"))
		return &gqlPayload{typ: "CreateIssuePayload", value: map[string]*Resolved{
			"issue": {Value: &GQLIssue{Repo: repo, Issue: issue}, Type: "Issue"},
		}}
	case "closeIssue":
		kind, parts, ok := store.DecodeGlobalID(inputStr("issueId"))
		if !ok || kind != "I" {
			ctx.pushError("Could not resolve to an Issue with the id provided.")
			return &gqlPayload{typ: "CloseIssuePayload", value: map[string]*Resolved{"issue": nil}}
		}
		dbID, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		repo := store.FindRepoByDbID(dbID)
		var issue *IssueItem
		if repo != nil {
			for _, i := range repo.Issues {
				if i.Number == num {
					issue = i
					break
				}
			}
		}
		if issue == nil {
			ctx.pushError("Could not resolve to an Issue with the id provided.")
			return &gqlPayload{typ: "CloseIssuePayload", value: map[string]*Resolved{"issue": nil}}
		}
		issue.State = "closed"
		issue.JSONCache = ""
		issue.Rev++
		return &gqlPayload{typ: "CloseIssuePayload", value: map[string]*Resolved{
			"issue": {Value: &GQLIssue{Repo: repo, Issue: issue}, Type: "Issue"},
		}}
	case "createPullRequest":
		repo := repoFromGlobalID(store, inputStr("repositoryId"))
		if repo == nil {
			ctx.pushError("Could not resolve to a Repository with the id provided.")
			return &gqlPayload{typ: "CreatePullRequestPayload", value: map[string]*Resolved{"pullRequest": nil}}
		}
		pr := store.CreatePullInternal(repo, inputStr("title"), inputStr("headRefName"), inputStr("baseRefName"))
		if pr == nil {
			ctx.pushError("Could not create pull request for the given head and base.")
			return &gqlPayload{typ: "CreatePullRequestPayload", value: map[string]*Resolved{"pullRequest": nil}}
		}
		return &gqlPayload{typ: "CreatePullRequestPayload", value: map[string]*Resolved{
			"pullRequest": {Value: &GQLPull{Repo: repo, Pull: pr}, Type: "PullRequest"},
		}}
	case "mergePullRequest":
		kind, parts, ok := store.DecodeGlobalID(inputStr("pullRequestId"))
		if !ok || kind != "PR" {
			ctx.pushError("Could not resolve to a PullRequest with the id provided.")
			return &gqlPayload{typ: "MergePullRequestPayload", value: map[string]*Resolved{"pullRequest": nil}}
		}
		dbID, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		repo := store.FindRepoByDbID(dbID)
		var pr *PullItem
		if repo != nil {
			for _, p := range repo.Pulls {
				if p.Number == num {
					pr = p
					break
				}
			}
		}
		if pr == nil {
			ctx.pushError("Could not resolve to a PullRequest with the id provided.")
			return &gqlPayload{typ: "MergePullRequestPayload", value: map[string]*Resolved{"pullRequest": nil}}
		}
		if pr.Merged || pr.State != "open" {
			ctx.pushError("Pull request is already merged or closed.")
			return &gqlPayload{typ: "MergePullRequestPayload", value: map[string]*Resolved{"pullRequest": nil}}
		}
		if store.MergePullInternal(repo, pr) == "" {
			ctx.pushError("Merge conflict.")
			return &gqlPayload{typ: "MergePullRequestPayload", value: map[string]*Resolved{"pullRequest": nil}}
		}
		return &gqlPayload{typ: "MergePullRequestPayload", value: map[string]*Resolved{
			"pullRequest": {Value: &GQLPull{Repo: repo, Pull: pr}, Type: "PullRequest"},
		}}
	case "addComment":
		kind, parts, ok := store.DecodeGlobalID(inputStr("subjectId"))
		if !ok || kind != "I" {
			ctx.pushError("Could not resolve to an Issue with the id provided.")
			return &gqlPayload{typ: "AddCommentPayload", value: map[string]*Resolved{"commentEdge": nil}}
		}
		dbID, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		repo := store.FindRepoByDbID(dbID)
		var issue *IssueItem
		if repo != nil {
			for _, i := range repo.Issues {
				if i.Number == num {
					issue = i
					break
				}
			}
		}
		if issue == nil {
			ctx.pushError("Could not resolve to an Issue with the id provided.")
			return &gqlPayload{typ: "AddCommentPayload", value: map[string]*Resolved{"commentEdge": nil}}
		}
		body := inputStr("body")
		comment := &CommentItem{ID: store.NextCommentID, Body: body, UserLogin: store.ViewerLogin, CreatedAt: store.TrialNowIso}
		store.NextCommentID++
		issue.Comments = append(issue.Comments, comment)
		issue.JSONCache = ""
		issue.Rev++
		node := Obj("id", JStrVal(store.CommentGlobalID(repo, issue.Number, comment.ID)), "body", JStrVal(body))
		return &gqlPayload{typ: "AddCommentPayload", value: map[string]*Resolved{
			"commentEdge": {Value: &gqlEdge{node: node}, Type: "IssueCommentEdge"},
		}}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Endpoint handler
// ---------------------------------------------------------------------------

func handleGraphQL(ctx *Ctx, w http.ResponseWriter) {
	parsed, err := parseJSONBody(ctx)
	if err != nil {
		panic(err)
	}
	isArray := parsed != nil && parsed.IsArray()
	var requests []*JVal
	if isArray {
		requests = parsed.A
	} else {
		requests = []*JVal{parsed}
	}
	exec := &execCtx{store: ctx.Store}
	results := make([]*JVal, 0, len(requests))
	for _, req := range requests {
		data := executeGraphQLRequest(exec, req)
		result := &JVal{Kind: JObject}
		result.O = append(result.O, JPair{K: "data", V: data})
		if len(exec.errors) > 0 {
			errItems := make([]*JVal, 0, len(exec.errors))
			for _, msg := range exec.errors {
				errItems = append(errItems, Obj("message", JStrVal(msg)))
			}
			result.O = append(result.O, JPair{K: "errors", V: JArrVal(errItems...)})
			exec.errors = nil
		}
		results = append(results, result)
	}
	if isArray {
		sendJSON(w, 200, JArrVal(results...), nil)
	} else {
		sendJSON(w, 200, results[0], nil)
	}
}

func executeGraphQLRequest(exec *execCtx, req *JVal) *JVal {
	queryVal := req.Get("query")
	query := ""
	if queryVal != nil {
		query, _ = queryVal.Str()
	}
	var variables *JVal
	if v := req.Get("variables"); v != nil && v.IsObject() {
		variables = v
	} else {
		variables = &JVal{Kind: JObject}
	}
	tokens, err := gqlLex(query)
	if err != nil {
		exec.pushError(err.Error())
		return &JVal{Kind: JObject}
	}
	ops, err := (&gqlParser{tokens: tokens}).parseDocument()
	if err != nil {
		exec.pushError(err.Error())
		return &JVal{Kind: JObject}
	}
	if len(ops) == 0 {
		exec.pushError("No operation in GraphQL document.")
		return &JVal{Kind: JObject}
	}
	op := ops[0]
	if op.kind == "mutation" {
		return executeMutation(exec, op, variables)
	}
	return executeSelections(exec, Resolved{Value: struct{}{}, Type: "Query"}, op.selections, variables)
}
