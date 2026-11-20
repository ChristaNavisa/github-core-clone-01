// GraphQL v4: a small recursive-descent parser plus an executor for the exact
// documents the suite sends. No external GraphQL library — only the 18
// documents, introspection of the four types the suite checks, batch requests,
// and global ids that match the REST node_id.

package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------------------------

type valueNode struct {
	kind   string // variable | string | int | float | boolean | null | enum | list | object
	str    string
	num    float64
	b      bool
	list   []*valueNode
	fields []namedValue
}

type namedValue struct {
	name  string
	value *valueNode
}

type fieldNode struct {
	alias     string // "" = none
	name      string
	args      []namedValue
	selection []selectionItem // nil = none
}

type selectionItem struct {
	kind          string // field | spread | inline
	field         *fieldNode
	name          string // fragment name for a spread
	typeCondition string // "" = none for an inline fragment
	selection     []selectionItem
}

type varDef struct {
	name         string
	defaultValue *valueNode // nil = none
}

type operationNode struct {
	operation string // query | mutation
	name      string
	variables []varDef
	selection []selectionItem
}

type fragmentNode struct {
	name          string
	typeCondition string
	selection     []selectionItem
}

type definition struct {
	isFragment bool
	op         *operationNode
	frag       *fragmentNode
}

type gqlParser struct {
	pos int
	src string
}

func (p *gqlParser) peek() byte {
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *gqlParser) skipIgnored() {
	for {
		c := p.peek()
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
			continue
		}
		if c == '#' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		return
	}
}

func (p *gqlParser) expect(ch byte) error {
	p.skipIgnored()
	if p.peek() != ch {
		q, _ := jsonMarshalString(string(ch))
		return fmt.Errorf("expected %s at %d", q, p.pos)
	}
	p.pos++
	return nil
}

func isNameChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}

func isNameStart(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}

func (p *gqlParser) matchWord(word string) bool {
	p.skipIgnored()
	if !strings.HasPrefix(p.src[p.pos:], word) {
		return false
	}
	after := byte(0)
	if p.pos+len(word) < len(p.src) {
		after = p.src[p.pos+len(word)]
	}
	if after == 0 || !isNameChar(after) {
		p.pos += len(word)
		return true
	}
	return false
}

func (p *gqlParser) readName() (string, error) {
	p.skipIgnored()
	start := p.pos
	if p.pos >= len(p.src) || !isNameStart(p.src[p.pos]) {
		return "", fmt.Errorf("expected a name at %d", p.pos)
	}
	for p.pos < len(p.src) && isNameChar(p.src[p.pos]) {
		p.pos++
	}
	return p.src[start:p.pos], nil
}

func (p *gqlParser) readString() (string, error) {
	p.skipIgnored()
	if p.peek() != '"' {
		return "", fmt.Errorf("expected a string at %d", p.pos)
	}
	p.pos++
	var out strings.Builder
	for {
		if p.pos >= len(p.src) {
			return "", fmt.Errorf("unterminated string")
		}
		c := p.src[p.pos]
		if c == '"' {
			p.pos++
			return out.String(), nil
		}
		if c == '\\' {
			esc := byte(0)
			if p.pos+1 < len(p.src) {
				esc = p.src[p.pos+1]
			}
			p.pos += 2
			switch esc {
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'r':
				out.WriteByte('\r')
			case '"':
				out.WriteByte('"')
			case '\\':
				out.WriteByte('\\')
			case '/':
				out.WriteByte('/')
			case 'u':
				hexDigits := ""
				if p.pos+4 <= len(p.src) {
					hexDigits = p.src[p.pos : p.pos+4]
				}
				n, _ := strconv.ParseInt(hexDigits, 16, 32)
				out.WriteRune(rune(n))
				p.pos += 4
			default:
				if esc != 0 {
					out.WriteByte(esc)
				}
			}
			continue
		}
		out.WriteByte(c)
		p.pos++
	}
}

func (p *gqlParser) parseDocument() ([]definition, error) {
	var defs []definition
	p.skipIgnored()
	for p.pos < len(p.src) {
		def, err := p.parseDefinition()
		if err != nil {
			return nil, err
		}
		defs = append(defs, def)
		p.skipIgnored()
	}
	return defs, nil
}

func (p *gqlParser) parseDefinition() (definition, error) {
	p.skipIgnored()
	if p.peek() == '{' {
		sel, err := p.parseSelection()
		if err != nil {
			return definition{}, err
		}
		return definition{op: &operationNode{operation: "query", selection: sel}}, nil
	}
	var operation string
	if p.matchWord("query") {
		operation = "query"
	} else if p.matchWord("mutation") {
		operation = "mutation"
	}
	if operation != "" {
		var name string
		p.skipIgnored()
		if p.pos < len(p.src) && isNameStart(p.src[p.pos]) {
			n, err := p.readName()
			if err != nil {
				return definition{}, err
			}
			name = n
		}
		variables, err := p.parseVariableDefinitions()
		if err != nil {
			return definition{}, err
		}
		sel, err := p.parseSelection()
		if err != nil {
			return definition{}, err
		}
		return definition{op: &operationNode{operation: operation, name: name, variables: variables, selection: sel}}, nil
	}
	if p.matchWord("fragment") {
		name, err := p.readName()
		if err != nil {
			return definition{}, err
		}
		if !p.matchWord("on") {
			return definition{}, fmt.Errorf("expected \"on\" in a fragment")
		}
		typeCondition, err := p.readName()
		if err != nil {
			return definition{}, err
		}
		sel, err := p.parseSelection()
		if err != nil {
			return definition{}, err
		}
		return definition{isFragment: true, frag: &fragmentNode{name: name, typeCondition: typeCondition, selection: sel}}, nil
	}
	return definition{}, fmt.Errorf("unexpected token at %d", p.pos)
}

func (p *gqlParser) parseVariableDefinitions() ([]varDef, error) {
	var out []varDef
	p.skipIgnored()
	if p.peek() != '(' {
		return out, nil
	}
	p.pos++
	for {
		p.skipIgnored()
		if p.peek() == ')' {
			p.pos++
			return out, nil
		}
		if p.peek() != '$' {
			return nil, fmt.Errorf("expected a variable in variable definitions")
		}
		p.pos++
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		if err := p.parseType(); err != nil {
			return nil, err
		}
		var defaultValue *valueNode
		p.skipIgnored()
		if p.peek() == '=' {
			p.pos++
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			defaultValue = v
		}
		out = append(out, varDef{name: name, defaultValue: defaultValue})
	}
}

func (p *gqlParser) parseType() error {
	p.skipIgnored()
	if p.peek() == '[' {
		p.pos++
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expect(']'); err != nil {
			return err
		}
	} else {
		if _, err := p.readName(); err != nil {
			return err
		}
	}
	p.skipIgnored()
	if p.peek() == '!' {
		p.pos++
	}
	return nil
}

func (p *gqlParser) parseSelection() ([]selectionItem, error) {
	if err := p.expect('{'); err != nil {
		return nil, err
	}
	var items []selectionItem
	for {
		p.skipIgnored()
		if p.peek() == '}' {
			p.pos++
			return items, nil
		}
		item, err := p.parseSelectionItem()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
}

func (p *gqlParser) parseSelectionItem() (selectionItem, error) {
	p.skipIgnored()
	if strings.HasPrefix(p.src[p.pos:], "...") {
		p.pos += 3
		p.skipIgnored()
		if p.matchWord("on") {
			typeCondition, err := p.readName()
			if err != nil {
				return selectionItem{}, err
			}
			sel, err := p.parseSelection()
			if err != nil {
				return selectionItem{}, err
			}
			return selectionItem{kind: "inline", typeCondition: typeCondition, selection: sel}, nil
		}
		name, err := p.readName()
		if err != nil {
			return selectionItem{}, err
		}
		return selectionItem{kind: "spread", name: name}, nil
	}
	alias := ""
	name, err := p.readName()
	if err != nil {
		return selectionItem{}, err
	}
	p.skipIgnored()
	if p.peek() == ':' {
		p.pos++
		alias = name
		name, err = p.readName()
		if err != nil {
			return selectionItem{}, err
		}
	}
	args, err := p.parseArguments()
	if err != nil {
		return selectionItem{}, err
	}
	if err := p.skipDirectives(); err != nil {
		return selectionItem{}, err
	}
	var selection []selectionItem
	p.skipIgnored()
	if p.peek() == '{' {
		sel, err := p.parseSelection()
		if err != nil {
			return selectionItem{}, err
		}
		selection = sel
	}
	return selectionItem{kind: "field", field: &fieldNode{alias: alias, name: name, args: args, selection: selection}}, nil
}

func (p *gqlParser) skipDirectives() error {
	for {
		p.skipIgnored()
		if p.peek() != '@' {
			return nil
		}
		p.pos++
		if _, err := p.readName(); err != nil {
			return err
		}
		if _, err := p.parseArguments(); err != nil {
			return err
		}
	}
}

func (p *gqlParser) parseArguments() ([]namedValue, error) {
	var args []namedValue
	p.skipIgnored()
	if p.peek() != '(' {
		return args, nil
	}
	p.pos++
	for {
		p.skipIgnored()
		if p.peek() == ')' {
			p.pos++
			return args, nil
		}
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args = append(args, namedValue{name: name, value: value})
	}
}

func (p *gqlParser) parseValue() (*valueNode, error) {
	p.skipIgnored()
	c := p.peek()
	if c == '$' {
		p.pos++
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: "variable", str: name}, nil
	}
	if c == '"' {
		s, err := p.readString()
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: "string", str: s}, nil
	}
	if c == '[' {
		p.pos++
		var values []*valueNode
		for {
			p.skipIgnored()
			if p.peek() == ']' {
				p.pos++
				return &valueNode{kind: "list", list: values}, nil
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
	}
	if c == '{' {
		p.pos++
		var fields []namedValue
		for {
			p.skipIgnored()
			if p.peek() == '}' {
				p.pos++
				return &valueNode{kind: "object", fields: fields}, nil
			}
			name, err := p.readName()
			if err != nil {
				return nil, err
			}
			if err := p.expect(':'); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			fields = append(fields, namedValue{name: name, value: v})
		}
	}
	if c == '-' || c >= '0' && c <= '9' {
		rest := p.src[p.pos:]
		end := 0
		if end < len(rest) && rest[end] == '-' {
			end++
		}
		// integer part: 0 or [1-9][0-9]* — at least one digit is required.
		if end >= len(rest) || rest[end] < '0' || rest[end] > '9' {
			return nil, fmt.Errorf("bad number at %d", p.pos)
		}
		// integer part: 0 or [1-9][0-9]*
		if end < len(rest) && rest[end] == '0' {
			end++
		} else {
			for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
				end++
			}
		}
		isFloat := false
		if end < len(rest) && rest[end] == '.' {
			j := end + 1
			digits := 0
			for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
				j++
				digits++
			}
			if digits > 0 {
				isFloat = true
				end = j
			}
		}
		if end < len(rest) && (rest[end] == 'e' || rest[end] == 'E') {
			j := end + 1
			if j < len(rest) && (rest[j] == '+' || rest[j] == '-') {
				j++
			}
			digits := 0
			for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
				j++
				digits++
			}
			if digits > 0 {
				isFloat = true
				end = j
			}
		}
		text := rest[:end]
		p.pos += end
		num, _ := strconv.ParseFloat(text, 64)
		if isFloat {
			return &valueNode{kind: "float", num: num}, nil
		}
		return &valueNode{kind: "int", num: num}, nil
	}
	name, err := p.readName()
	if err != nil {
		return nil, err
	}
	switch name {
	case "true":
		return &valueNode{kind: "boolean", b: true}, nil
	case "false":
		return &valueNode{kind: "boolean", b: false}, nil
	case "null":
		return &valueNode{kind: "null"}, nil
	default:
		return &valueNode{kind: "enum", str: name}, nil
	}
}

// ---------------------------------------------------------------------------------------------
// schema model (only what the suite introspects)
// ---------------------------------------------------------------------------------------------

type typeRef struct {
	kind   string // SCALAR | OBJECT | ENUM | NON_NULL | LIST | INPUT_OBJECT
	name   string
	ofType *typeRef
}

func scalarRef(name string) *typeRef    { return &typeRef{kind: "SCALAR", name: name} }
func objRef(name string) *typeRef       { return &typeRef{kind: "OBJECT", name: name} }
func enumRef(name string) *typeRef      { return &typeRef{kind: "ENUM", name: name} }
func nonNullRef(t *typeRef) *typeRef    { return &typeRef{kind: "NON_NULL", ofType: t} }
func listRef(t *typeRef) *typeRef       { return &typeRef{kind: "LIST", ofType: t} }

var (
	strRef  = scalarRef("String")
	intRef  = scalarRef("Int")
	boolRef = scalarRef("Boolean")
	idRef   = scalarRef("ID")
)

type fieldDef struct {
	name string
	typ  *typeRef
}

type typeDef struct {
	kind       string // OBJECT | ENUM | SCALAR | INPUT_OBJECT
	fields     []fieldDef
	enumValues []string
}

var schemaOrder []string
var schemaDefs map[string]*typeDef

func init() {
	add := func(name string, def *typeDef) {
		schemaOrder = append(schemaOrder, name)
		schemaDefs[name] = def
	}
	schemaDefs = map[string]*typeDef{}
	add("Query", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"viewer", objRef("User")}, {"repository", objRef("Repository")}, {"node", objRef("Node")},
	}})
	add("Mutation", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"createIssue", objRef("CreateIssuePayload")}, {"closeIssue", objRef("CloseIssuePayload")},
		{"createPullRequest", objRef("CreatePullRequestPayload")}, {"mergePullRequest", objRef("MergePullRequestPayload")},
		{"addComment", objRef("AddCommentPayload")},
	}})
	add("User", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"login", nonNullRef(strRef)}, {"id", nonNullRef(idRef)}, {"name", strRef}, {"email", strRef}, {"databaseId", intRef},
	}})
	add("Repository", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"id", nonNullRef(idRef)}, {"databaseId", intRef}, {"name", nonNullRef(strRef)},
		{"nameWithOwner", nonNullRef(strRef)}, {"owner", objRef("User")}, {"defaultBranchRef", objRef("Ref")},
		{"viewerPermission", enumRef("RepositoryPermission")}, {"hasIssuesEnabled", boolRef},
		{"issue", objRef("Issue")}, {"issues", objRef("IssueConnection")},
		{"pullRequests", objRef("PullRequestConnection")}, {"pullRequest", objRef("PullRequest")},
		{"url", strRef},
	}})
	add("Ref", &typeDef{kind: "OBJECT", fields: []fieldDef{{"name", nonNullRef(strRef)}}})
	add("Issue", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"id", nonNullRef(idRef)}, {"databaseId", intRef}, {"number", nonNullRef(intRef)},
		{"title", nonNullRef(strRef)}, {"body", strRef}, {"state", enumRef("IssueState")},
		{"author", objRef("User")}, {"labels", objRef("LabelConnection")},
		{"comments", objRef("IssueCommentConnection")}, {"createdAt", strRef}, {"updatedAt", strRef},
		{"closedAt", strRef}, {"url", strRef},
	}})
	add("PullRequest", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"id", nonNullRef(idRef)}, {"databaseId", intRef}, {"number", nonNullRef(intRef)},
		{"title", nonNullRef(strRef)}, {"body", strRef}, {"state", enumRef("PullRequestState")},
		{"headRefName", nonNullRef(strRef)}, {"baseRefName", nonNullRef(strRef)},
		{"headRefOid", strRef}, {"baseRefOid", strRef}, {"mergeable", enumRef("MergeableState")},
		{"merged", boolRef}, {"mergeCommit", objRef("Commit")}, {"files", objRef("PullRequestFileConnection")},
		{"author", objRef("User")}, {"createdAt", strRef}, {"updatedAt", strRef}, {"mergedAt", strRef}, {"url", strRef},
	}})
	add("Commit", &typeDef{kind: "OBJECT", fields: []fieldDef{{"oid", nonNullRef(strRef)}}})
	add("IssueConnection", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"nodes", listRef(objRef("Issue"))}, {"edges", listRef(objRef("IssueEdge"))},
		{"pageInfo", nonNullRef(objRef("PageInfo"))}, {"totalCount", nonNullRef(intRef)},
	}})
	add("PullRequestConnection", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"nodes", listRef(objRef("PullRequest"))}, {"edges", listRef(objRef("PullRequestEdge"))},
		{"pageInfo", nonNullRef(objRef("PageInfo"))}, {"totalCount", nonNullRef(intRef)},
	}})
	add("IssueCommentConnection", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"nodes", listRef(objRef("IssueComment"))}, {"edges", listRef(objRef("IssueCommentEdge"))},
		{"pageInfo", nonNullRef(objRef("PageInfo"))}, {"totalCount", nonNullRef(intRef)},
	}})
	add("PullRequestFileConnection", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"nodes", listRef(objRef("PullRequestFile"))}, {"edges", listRef(objRef("PullRequestFileEdge"))},
		{"pageInfo", nonNullRef(objRef("PageInfo"))}, {"totalCount", nonNullRef(intRef)},
	}})
	add("LabelConnection", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"nodes", listRef(objRef("Label"))}, {"edges", listRef(objRef("LabelEdge"))},
		{"pageInfo", nonNullRef(objRef("PageInfo"))}, {"totalCount", nonNullRef(intRef)},
	}})
	add("IssueEdge", &typeDef{kind: "OBJECT", fields: []fieldDef{{"node", objRef("Issue")}, {"cursor", nonNullRef(strRef)}}})
	add("PullRequestEdge", &typeDef{kind: "OBJECT", fields: []fieldDef{{"node", objRef("PullRequest")}, {"cursor", nonNullRef(strRef)}}})
	add("IssueCommentEdge", &typeDef{kind: "OBJECT", fields: []fieldDef{{"node", objRef("IssueComment")}, {"cursor", nonNullRef(strRef)}}})
	add("PullRequestFileEdge", &typeDef{kind: "OBJECT", fields: []fieldDef{{"node", objRef("PullRequestFile")}, {"cursor", nonNullRef(strRef)}}})
	add("LabelEdge", &typeDef{kind: "OBJECT", fields: []fieldDef{{"node", objRef("Label")}, {"cursor", nonNullRef(strRef)}}})
	add("IssueComment", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"id", nonNullRef(idRef)}, {"databaseId", intRef}, {"body", nonNullRef(strRef)},
		{"author", objRef("User")}, {"createdAt", strRef}, {"updatedAt", strRef},
	}})
	add("PullRequestFile", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"path", nonNullRef(strRef)}, {"additions", nonNullRef(intRef)},
		{"deletions", nonNullRef(intRef)}, {"changes", nonNullRef(intRef)},
	}})
	add("Label", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"id", nonNullRef(idRef)}, {"name", nonNullRef(strRef)}, {"color", nonNullRef(strRef)}, {"description", strRef},
	}})
	add("PageInfo", &typeDef{kind: "OBJECT", fields: []fieldDef{
		{"hasNextPage", nonNullRef(boolRef)}, {"hasPreviousPage", nonNullRef(boolRef)},
		{"startCursor", strRef}, {"endCursor", strRef},
	}})
	add("CreateIssuePayload", &typeDef{kind: "OBJECT", fields: []fieldDef{{"issue", objRef("Issue")}, {"clientMutationId", strRef}}})
	add("CloseIssuePayload", &typeDef{kind: "OBJECT", fields: []fieldDef{{"issue", objRef("Issue")}, {"clientMutationId", strRef}}})
	add("CreatePullRequestPayload", &typeDef{kind: "OBJECT", fields: []fieldDef{{"pullRequest", objRef("PullRequest")}, {"clientMutationId", strRef}}})
	add("MergePullRequestPayload", &typeDef{kind: "OBJECT", fields: []fieldDef{{"pullRequest", objRef("PullRequest")}, {"clientMutationId", strRef}}})
	add("AddCommentPayload", &typeDef{kind: "OBJECT", fields: []fieldDef{{"commentEdge", objRef("IssueCommentEdge")}, {"subject", objRef("Issue")}, {"clientMutationId", strRef}}})
	add("Node", &typeDef{kind: "OBJECT"})
	add("IssueState", &typeDef{kind: "ENUM", enumValues: []string{"OPEN", "CLOSED"}})
	add("PullRequestState", &typeDef{kind: "ENUM", enumValues: []string{"OPEN", "CLOSED", "MERGED"}})
	add("MergeableState", &typeDef{kind: "ENUM", enumValues: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}})
	add("RepositoryPermission", &typeDef{kind: "ENUM", enumValues: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}})
	add("SearchType", &typeDef{kind: "ENUM", enumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"}})
	add("String", &typeDef{kind: "SCALAR"})
	add("Int", &typeDef{kind: "SCALAR"})
	add("Boolean", &typeDef{kind: "SCALAR"})
	add("ID", &typeDef{kind: "SCALAR"})
}

// ---------------------------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------------------------

type gqlError struct {
	message string
	path    []any
}

// gqlLazy is a field whose value is computed on demand (connections and
// lookups). It is never serialized directly: a lazy field selected without
// a sub-selection is dropped from the JSON, exactly like JSON.stringify
// drops a function value.
type gqlLazy func(args map[string]any) (any, error)

type execContext struct {
	forge     *Forge
	variables map[string]any
	fragments map[string]*fragmentNode
	errors    []gqlError
}

func resolveValue(node *valueNode, ctx *execContext) any {
	switch node.kind {
	case "variable":
		if v, ok := ctx.variables[node.str]; ok {
			return v
		}
		return nil
	case "string", "enum":
		return node.str
	case "int", "float":
		return node.num
	case "boolean":
		return node.b
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(node.list))
		for _, v := range node.list {
			out = append(out, resolveValue(v, ctx))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, f := range node.fields {
			out[f.name] = resolveValue(f.value, ctx)
		}
		return out
	}
	return nil
}

func argsOf(field *fieldNode, ctx *execContext) map[string]any {
	out := map[string]any{}
	for _, a := range field.args {
		out[a.name] = resolveValue(a.value, ctx)
	}
	return out
}

func typeNameOf(value any) string {
	if m, ok := value.(map[string]any); ok {
		if t, ok := m["__typename"].(string); ok {
			return t
		}
	}
	return ""
}

func toNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func cursorFor(kind, key string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + key))
}

func cursorKey(cursor string) string {
	if cursor == "" {
		return ""
	}
	decoded, err := lenientBase64Decode(cursor)
	if err != nil {
		return ""
	}
	idx := strings.Index(string(decoded), ":")
	if idx < 0 {
		return ""
	}
	return string(decoded)[idx+1:]
}

// connection is a Relay-style connection over an already-ordered list of node values.
func gqlConnection(kind string, keyOf func(item any) string, items []any, args map[string]any) map[string]any {
	after := ""
	if s, ok := args["after"].(string); ok {
		after = cursorKey(s)
	}
	start := 0
	if after != "" {
		idx := -1
		for i, item := range items {
			if keyOf(item) == after {
				idx = i
				break
			}
		}
		if idx < 0 {
			start = len(items)
		} else {
			start = idx + 1
		}
	}
	first := len(items)
	if n, ok := toNumber(args["first"]); ok {
		first = int(math.Max(0, n))
	}
	end := start + first
	if end > len(items) {
		end = len(items)
	}
	if start > len(items) {
		start = len(items)
		end = len(items)
	}
	page := items[start:end]
	edges := make([]any, 0, len(page))
	for _, item := range page {
		edges = append(edges, map[string]any{"__typename": "Edge", "node": item, "cursor": cursorFor(kind, keyOf(item))})
	}
	var startCursor, endCursor any
	if len(page) > 0 {
		startCursor = cursorFor(kind, keyOf(page[0]))
		endCursor = cursorFor(kind, keyOf(page[len(page)-1]))
	}
	return map[string]any{
		"__typename": "Connection",
		"nodes":      page,
		"edges":      edges,
		"pageInfo": map[string]any{
			"__typename": "PageInfo", "hasNextPage": start+len(page) < len(items),
			"hasPreviousPage": start > 0, "startCursor": startCursor, "endCursor": endCursor,
		},
		"totalCount": len(items),
	}
}

func gqlUserValue(f *Forge) map[string]any {
	return map[string]any{
		"__typename": "User", "login": viewerLogin, "id": f.globalID("User", viewerLogin),
		"databaseId": viewerID, "name": viewerName, "email": viewerEmail,
	}
}

func issueState(issue *Issue) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullState(pull *PullRequest) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func gqlIssueValue(f *Forge, repo *Repo, issue *Issue) map[string]any {
	closedAt := any(nil)
	if issue.State == "closed" {
		closedAt = fixedISO
	}
	return map[string]any{
		"__typename": "Issue",
		"id":         f.globalID("Issue", viewerLogin, repo.Name, strconv.Itoa(issue.Number)),
		"databaseId": issue.ID, "number": issue.Number, "title": issue.Title, "body": issue.Body,
		"state": issueState(issue), "author": gqlUserValue(f),
		"labels": gqlLazy(func(args map[string]any) (any, error) {
			items := make([]any, 0, len(issue.Labels))
			for _, name := range issue.Labels {
				color := "000000"
				if found, ok := repo.Labels[name]; ok {
					color = found.Color
				}
				items = append(items, map[string]any{"__typename": "Label", "id": f.globalID("Label", name), "name": name, "color": color, "description": ""})
			}
			return gqlConnection("label", func(item any) string { return item.(map[string]any)["name"].(string) }, items, args), nil
		}),
		"comments": gqlLazy(func(args map[string]any) (any, error) {
			items := make([]any, 0, len(issue.Comments))
			for i := range issue.Comments {
				items = append(items, gqlCommentValue(f, repo, issue, &issue.Comments[i]))
			}
			return gqlConnection("comment", func(item any) string {
				return strconv.Itoa(item.(map[string]any)["databaseId"].(int))
			}, items, args), nil
		}),
		"createdAt": fixedISO, "updatedAt": fixedISO, "closedAt": closedAt,
		"url": fmt.Sprintf("https://github.com/%s/%s/issues/%d", viewerLogin, repo.Name, issue.Number),
	}
}

func gqlCommentValue(f *Forge, repo *Repo, issue *Issue, comment *IssueComment) map[string]any {
	return map[string]any{
		"__typename": "IssueComment",
		"id":         f.globalID("IssueComment", viewerLogin, repo.Name, strconv.Itoa(issue.Number), strconv.Itoa(comment.ID)),
		"databaseId": comment.ID, "body": comment.Body, "author": gqlUserValue(f),
		"createdAt": fixedISO, "updatedAt": fixedISO,
	}
}

func gqlPullValue(f *Forge, repo *Repo, pull *PullRequest) map[string]any {
	mergedAt := any(nil)
	if pull.Merged {
		mergedAt = fixedISO
	}
	var mergeCommit any
	if pull.MergeCommitSha != "" {
		mergeCommit = map[string]any{"__typename": "Commit", "oid": pull.MergeCommitSha}
	}
	mergeable := "CONFLICTING"
	if f.pullMergeable(repo, pull) {
		mergeable = "MERGEABLE"
	}
	return map[string]any{
		"__typename": "PullRequest",
		"id":         f.globalID("PullRequest", viewerLogin, repo.Name, strconv.Itoa(pull.Number)),
		"databaseId": pull.ID, "number": pull.Number, "title": pull.Title, "body": pull.Body,
		"state": pullState(pull), "headRefName": pull.HeadBranch, "baseRefName": pull.BaseBranch,
		"headRefOid": pull.HeadSha, "baseRefOid": f.baseTip(repo, pull),
		"mergeable": mergeable, "merged": pull.Merged, "mergeCommit": mergeCommit,
		"files": gqlLazy(func(args map[string]any) (any, error) {
			files := f.pullFiles(repo, pull)
			items := make([]any, 0, len(files))
			for _, file := range files {
				items = append(items, map[string]any{"__typename": "PullRequestFile", "path": file.Path, "additions": file.Additions, "deletions": file.Deletions, "changes": file.Additions + file.Deletions})
			}
			return gqlConnection("file", func(item any) string { return item.(map[string]any)["path"].(string) }, items, args), nil
		}),
		"author": gqlUserValue(f), "createdAt": fixedISO, "updatedAt": fixedISO, "mergedAt": mergedAt,
		"url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", viewerLogin, repo.Name, pull.Number),
	}
}

func gqlRepoValue(f *Forge, repo *Repo) map[string]any {
	var defaultBranchRef any
	if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		defaultBranchRef = map[string]any{"__typename": "Ref", "name": repo.DefaultBranch}
	}
	return map[string]any{
		"__typename": "Repository",
		"id":         f.globalID("Repository", viewerLogin, repo.Name),
		"databaseId": repo.ID, "name": repo.Name, "nameWithOwner": viewerLogin + "/" + repo.Name,
		"owner": gqlUserValue(f), "defaultBranchRef": defaultBranchRef,
		"viewerPermission": "ADMIN", "hasIssuesEnabled": true,
		"issue": gqlLazy(func(args map[string]any) (any, error) {
			number := -1
			if n, ok := toNumber(args["number"]); ok {
				number = int(n)
			}
			if issue, ok := repo.Issues[number]; ok {
				return gqlIssueValue(f, repo, issue), nil
			}
			return nil, nil
		}),
		"issues": gqlLazy(func(args map[string]any) (any, error) {
			var items []*Issue
			for _, i := range repo.Issues {
				items = append(items, i)
			}
			sortIssuesByNumber(items)
			if raw, ok := args["states"].([]any); ok && len(raw) > 0 {
				want := map[string]bool{}
				for _, s := range raw {
					if str, ok := s.(string); ok {
						want[str] = true
					} else {
						want[fmt.Sprintf("%v", s)] = true
					}
				}
				var filtered []*Issue
				for _, i := range items {
					if want[issueState(i)] {
						filtered = append(filtered, i)
					}
				}
				items = filtered
			}
			direction := "DESC"
			if orderBy, ok := args["orderBy"].(map[string]any); ok {
				if d, ok := orderBy["direction"].(string); ok && d == "ASC" {
					direction = "ASC"
				}
			}
			if direction == "ASC" {
				sortIssuesByNumber(items)
			} else {
				sortIssuesByNumberDesc(items)
			}
			values := make([]any, 0, len(items))
			for _, i := range items {
				values = append(values, gqlIssueValue(f, repo, i))
			}
			return gqlConnection("issue", func(item any) string {
				return strconv.Itoa(item.(map[string]any)["number"].(int))
			}, values, args), nil
		}),
		"pullRequests": gqlLazy(func(args map[string]any) (any, error) {
			var items []*PullRequest
			for _, p := range repo.Pulls {
				items = append(items, p)
			}
			sortPullsByNumberDesc(items)
			values := make([]any, 0, len(items))
			for _, p := range items {
				values = append(values, gqlPullValue(f, repo, p))
			}
			return gqlConnection("pull", func(item any) string {
				return strconv.Itoa(item.(map[string]any)["number"].(int))
			}, values, args), nil
		}),
		"pullRequest": gqlLazy(func(args map[string]any) (any, error) {
			number := -1
			if n, ok := toNumber(args["number"]); ok {
				number = int(n)
			}
			if pull, ok := repo.Pulls[number]; ok {
				return gqlPullValue(f, repo, pull), nil
			}
			return nil, nil
		}),
		"url": fmt.Sprintf("https://github.com/%s/%s", viewerLogin, repo.Name),
	}
}

func sortSlice[T any](items []T, less func(a, b T) bool) {
	sort.SliceStable(items, func(i, j int) bool { return less(items[i], items[j]) })
}

func sortIssuesByNumber(items []*Issue) {
	sortSlice(items, func(a, b *Issue) bool { return a.Number < b.Number })
}

func sortIssuesByNumberDesc(items []*Issue) {
	sortSlice(items, func(a, b *Issue) bool { return a.Number > b.Number })
}

func sortPullsByNumberDesc(items []*PullRequest) {
	sortSlice(items, func(a, b *PullRequest) bool { return a.Number > b.Number })
}

// ---------------------------------------------------------------------------------------------
// introspection
// ---------------------------------------------------------------------------------------------

func gqlTypeRefValue(t *typeRef) map[string]any {
	if t.kind == "NON_NULL" || t.kind == "LIST" {
		return map[string]any{
			"__typename": "__Type", "name": nil, "kind": t.kind, "description": nil,
			"fields": nil, "enumValues": nil, "ofType": gqlTypeRefValue(t.ofType),
		}
	}
	def := schemaDefs[t.name]
	var fields, enumValues any
	if def != nil {
		fields = gqlLazy(func(args map[string]any) (any, error) {
			out := make([]any, 0, len(def.fields))
			for _, fld := range def.fields {
				out = append(out, gqlFieldValue(fld))
			}
			return out, nil
		})
		enumValues = gqlLazy(func(args map[string]any) (any, error) {
			out := make([]any, 0, len(def.enumValues))
			for _, v := range def.enumValues {
				out = append(out, map[string]any{"__typename": "__EnumValue", "name": v, "description": nil})
			}
			return out, nil
		})
	} else {
		fields = gqlLazy(func(args map[string]any) (any, error) { return []any{}, nil })
		enumValues = gqlLazy(func(args map[string]any) (any, error) { return []any{}, nil })
	}
	return map[string]any{
		"__typename": "__Type", "name": t.name, "kind": t.kind, "description": nil,
		"fields": fields, "enumValues": enumValues, "inputFields": nil, "interfaces": []any{},
		"possibleTypes": nil, "ofType": nil,
	}
}

func gqlFieldValue(fld fieldDef) map[string]any {
	return map[string]any{
		"__typename": "__Field", "name": fld.name, "description": nil,
		"args": []any{}, "type": gqlTypeRefValue(fld.typ),
	}
}

func gqlTypeValue(name string) map[string]any {
	def, ok := schemaDefs[name]
	if !ok {
		return nil
	}
	return map[string]any{
		"__typename": "__Type", "name": name, "kind": def.kind, "description": nil,
		"fields": gqlLazy(func(args map[string]any) (any, error) {
			out := make([]any, 0, len(def.fields))
			for _, fld := range def.fields {
				out = append(out, gqlFieldValue(fld))
			}
			return out, nil
		}),
		"enumValues": gqlLazy(func(args map[string]any) (any, error) {
			out := make([]any, 0, len(def.enumValues))
			for _, v := range def.enumValues {
				out = append(out, map[string]any{"__typename": "__EnumValue", "name": v, "description": nil})
			}
			return out, nil
		}),
		"inputFields": nil, "interfaces": []any{}, "possibleTypes": nil, "ofType": nil,
	}
}

func gqlSchemaValue() map[string]any {
	types := make([]any, 0, len(schemaOrder))
	for _, name := range schemaOrder {
		types = append(types, gqlTypeValue(name))
	}
	return map[string]any{
		"__typename": "__Schema", "queryType": gqlTypeValue("Query"),
		"mutationType": gqlTypeValue("Mutation"), "subscriptionType": nil,
		"types": types, "directives": []any{},
	}
}

// ---------------------------------------------------------------------------------------------
// field resolution
// ---------------------------------------------------------------------------------------------

// jsString mirrors JavaScript's String() for the mutation inputs (which are
// almost always strings already).
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, jsString(e))
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return ""
	}
}

func inputMap(args map[string]any) map[string]any {
	if m, ok := args["input"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func clientMutationID(input map[string]any) any {
	if s, ok := input["clientMutationId"].(string); ok {
		return s
	}
	return nil
}

func resolveRootField(f *Forge, field *fieldNode, ctx *execContext, isMutation bool) (any, error) {
	args := argsOf(field, ctx)
	if !isMutation {
		switch field.name {
		case "viewer":
			return gqlUserValue(f), nil
		case "repository":
			owner, ownerOK := args["owner"].(string)
			name, nameOK := args["name"].(string)
			if !ownerOK || !nameOK {
				return nil, nil
			}
			if owner != viewerLogin {
				return nil, nil
			}
			repo := f.findRepo(owner, name)
			if repo == nil {
				return nil, nil
			}
			return gqlRepoValue(f, repo), nil
		case "node":
			id, ok := args["id"].(string)
			if !ok {
				return nil, nil
			}
			kind, parts, ok := parseGlobalID(id)
			if !ok {
				return nil, nil
			}
			switch kind {
			case "Repository":
				if len(parts) < 2 {
					return nil, nil
				}
				repo := f.findRepo(parts[0], parts[1])
				if repo == nil {
					return nil, nil
				}
				return gqlRepoValue(f, repo), nil
			case "Issue":
				if len(parts) < 3 {
					return nil, nil
				}
				repo := f.findRepo(parts[0], parts[1])
				if repo == nil {
					return nil, nil
				}
				number, ok := jsParseInt(parts[2])
				if !ok {
					return nil, nil
				}
				issue, found := repo.Issues[number]
				if !found {
					return nil, nil
				}
				return gqlIssueValue(f, repo, issue), nil
			case "PullRequest":
				if len(parts) < 3 {
					return nil, nil
				}
				repo := f.findRepo(parts[0], parts[1])
				if repo == nil {
					return nil, nil
				}
				number, ok := jsParseInt(parts[2])
				if !ok {
					return nil, nil
				}
				pull, found := repo.Pulls[number]
				if !found {
					return nil, nil
				}
				return gqlPullValue(f, repo, pull), nil
			}
			return nil, nil
		case "__type":
			if name, ok := args["name"].(string); ok {
				return gqlTypeValue(name), nil
			}
			return nil, nil
		case "__schema":
			return gqlSchemaValue(), nil
		}
	} else {
		input := inputMap(args)
		switch field.name {
		case "createIssue":
			repositoryID, _ := input["repositoryId"].(string)
			kind, parts, ok := parseGlobalID(repositoryID)
			if !ok || kind != "Repository" {
				return nil, fmt.Errorf("createIssue: repositoryId is not a repository")
			}
			if len(parts) < 2 {
				return nil, fmt.Errorf("createIssue: repository not found")
			}
			repo := f.findRepo(parts[0], parts[1])
			if repo == nil {
				return nil, fmt.Errorf("createIssue: repository not found")
			}
			title := ""
			if v, exists := input["title"]; exists {
				title = jsString(v)
			}
			body := ""
			if s, ok := input["body"].(string); ok {
				body = s
			}
			issue := f.createIssue(repo, title, body, []string{})
			return map[string]any{"__typename": "CreateIssuePayload", "issue": gqlIssueValue(f, repo, issue), "clientMutationId": clientMutationID(input)}, nil
		case "closeIssue":
			issueID, _ := input["issueId"].(string)
			kind, parts, ok := parseGlobalID(issueID)
			if !ok || kind != "Issue" {
				return nil, fmt.Errorf("closeIssue: issueId is not an issue")
			}
			if len(parts) < 3 {
				return nil, fmt.Errorf("closeIssue: issue not found")
			}
			repo := f.findRepo(parts[0], parts[1])
			if repo == nil {
				return nil, fmt.Errorf("closeIssue: issue not found")
			}
			number, ok := jsParseInt(parts[2])
			if !ok {
				return nil, fmt.Errorf("closeIssue: issue not found")
			}
			issue, found := repo.Issues[number]
			if !found {
				return nil, fmt.Errorf("closeIssue: issue not found")
			}
			issue.State = "closed"
			delete(repo.IssueJSONCache, issue.Number)
			return map[string]any{"__typename": "CloseIssuePayload", "issue": gqlIssueValue(f, repo, issue), "clientMutationId": clientMutationID(input)}, nil
		case "createPullRequest":
			repositoryID, _ := input["repositoryId"].(string)
			kind, parts, ok := parseGlobalID(repositoryID)
			if !ok || kind != "Repository" {
				return nil, fmt.Errorf("createPullRequest: repositoryId is not a repository")
			}
			if len(parts) < 2 {
				return nil, fmt.Errorf("createPullRequest: repository not found")
			}
			repo := f.findRepo(parts[0], parts[1])
			if repo == nil {
				return nil, fmt.Errorf("createPullRequest: repository not found")
			}
			headName := jsString(input["headRefName"])
			baseName := jsString(input["baseRefName"])
			_, headExists := repo.Refs["refs/heads/"+headName]
			_, baseExists := repo.Refs["refs/heads/"+baseName]
			if !headExists || !baseExists {
				return nil, fmt.Errorf("createPullRequest: head or base branch does not exist")
			}
			for _, p := range repo.Pulls {
				if p.State == "open" && p.HeadBranch == headName && p.BaseBranch == baseName {
					return nil, fmt.Errorf("createPullRequest: a pull request already exists for these branches")
				}
			}
			title := jsString(input["title"])
			body := ""
			if s, ok := input["body"].(string); ok {
				body = s
			}
			pull := f.createPull(repo, title, body, headName, baseName)
			return map[string]any{"__typename": "CreatePullRequestPayload", "pullRequest": gqlPullValue(f, repo, pull), "clientMutationId": clientMutationID(input)}, nil
		case "mergePullRequest":
			pullRequestID, _ := input["pullRequestId"].(string)
			kind, parts, ok := parseGlobalID(pullRequestID)
			if !ok || kind != "PullRequest" {
				return nil, fmt.Errorf("mergePullRequest: pullRequestId is not a pull request")
			}
			if len(parts) < 3 {
				return nil, fmt.Errorf("mergePullRequest: pull request not found")
			}
			repo := f.findRepo(parts[0], parts[1])
			if repo == nil {
				return nil, fmt.Errorf("mergePullRequest: pull request not found")
			}
			number, ok := jsParseInt(parts[2])
			if !ok {
				return nil, fmt.Errorf("mergePullRequest: pull request not found")
			}
			pull, found := repo.Pulls[number]
			if !found {
				return nil, fmt.Errorf("mergePullRequest: pull request not found")
			}
			if pull.Merged {
				return nil, fmt.Errorf("mergePullRequest: pull request is already merged")
			}
			if pull.State != "open" {
				return nil, fmt.Errorf("mergePullRequest: pull request is not open")
			}
			if merged := f.mergePull(repo, pull); merged == "" {
				return nil, fmt.Errorf("mergePullRequest: merge conflict")
			}
			return map[string]any{"__typename": "MergePullRequestPayload", "pullRequest": gqlPullValue(f, repo, pull), "clientMutationId": clientMutationID(input)}, nil
		case "addComment":
			subjectID, _ := input["subjectId"].(string)
			kind, parts, ok := parseGlobalID(subjectID)
			if !ok {
				return nil, fmt.Errorf("addComment: subjectId is not a known object")
			}
			if len(parts) < 2 {
				return nil, fmt.Errorf("addComment: repository not found")
			}
			repo := f.findRepo(parts[0], parts[1])
			if repo == nil {
				return nil, fmt.Errorf("addComment: repository not found")
			}
			if kind == "Issue" {
				if len(parts) < 3 {
					return nil, fmt.Errorf("addComment: issue not found")
				}
				number, ok := jsParseInt(parts[2])
				if !ok {
					return nil, fmt.Errorf("addComment: issue not found")
				}
				issue, found := repo.Issues[number]
				if !found {
					return nil, fmt.Errorf("addComment: issue not found")
				}
				comment := f.addComment(repo, issue, jsString(input["body"]))
				node := gqlCommentValue(f, repo, issue, comment)
				return map[string]any{
					"__typename": "AddCommentPayload",
					"commentEdge": map[string]any{"__typename": "IssueCommentEdge", "node": node, "cursor": cursorFor("comment", strconv.Itoa(comment.ID))},
					"subject": gqlIssueValue(f, repo, issue), "clientMutationId": clientMutationID(input),
				}, nil
			}
			return nil, fmt.Errorf("addComment: subject is not an issue")
		}
	}
	rootType := "Query"
	if isMutation {
		rootType = "Mutation"
	}
	return nil, fmt.Errorf("Cannot query field \"%s\" on type \"%s\".", field.name, rootType)
}

// childTypeName is the GraphQL type of a field's value, from the schema model.
func childTypeName(parentType, fieldName string) string {
	if parentType == "" {
		return ""
	}
	def, ok := schemaDefs[parentType]
	if !ok {
		return ""
	}
	for _, fld := range def.fields {
		if fld.name == fieldName {
			t := fld.typ
			for t.kind == "NON_NULL" || t.kind == "LIST" {
				t = t.ofType
			}
			return t.name
		}
	}
	return ""
}

// executeSelection executes a selection set against value. typeName is the
// GraphQL type of value (used for unknown-field errors and ... on Type
// fragments); "" means unknown.
func executeSelection(value any, typeName string, selection []selectionItem, ctx *execContext, path []any) any {
	if value == nil {
		return nil
	}
	obj, isObj := value.(map[string]any)
	if !isObj {
		return value
	}
	out := map[string]any{}
	for _, item := range selection {
		if item.kind == "spread" {
			fragment, ok := ctx.fragments[item.name]
			if !ok {
				ctx.errors = append(ctx.errors, gqlError{message: fmt.Sprintf("Unknown fragment \"%s\".", item.name), path: append([]any(nil), path...)})
				continue
			}
			if merged := executeSelection(value, typeName, fragment.selection, ctx, path); merged != nil {
				if m, ok := merged.(map[string]any); ok {
					for k, v := range m {
						out[k] = v
					}
				}
			}
			continue
		}
		if item.kind == "inline" {
			if item.typeCondition != "" && typeName != "" && item.typeCondition != typeName {
				continue
			}
			if merged := executeSelection(value, typeName, item.selection, ctx, path); merged != nil {
				if m, ok := merged.(map[string]any); ok {
					for k, v := range m {
						out[k] = v
					}
				}
			}
			continue
		}
		field := item.field
		responseKey := field.name
		if field.alias != "" {
			responseKey = field.alias
		}
		fieldPath := append(append([]any(nil), path...), responseKey)
		if field.name == "__typename" {
			if typeName == "" {
				out[responseKey] = nil
			} else {
				out[responseKey] = typeName
			}
			continue
		}
		if typeName != "" {
			if def, ok := schemaDefs[typeName]; ok {
				known := false
				for _, fld := range def.fields {
					if fld.name == field.name {
						known = true
						break
					}
				}
				if !known {
					ctx.errors = append(ctx.errors, gqlError{message: fmt.Sprintf("Cannot query field \"%s\" on type \"%s\".", field.name, typeName), path: fieldPath})
					out[responseKey] = nil
					continue
				}
			}
		}
		raw, exists := obj[field.name]
		if !exists {
			raw = nil
		}
		if lazy, isLazy := raw.(gqlLazy); isLazy {
			result, err := lazy(argsOf(field, ctx))
			if err != nil {
				ctx.errors = append(ctx.errors, gqlError{message: err.Error(), path: fieldPath})
				out[responseKey] = nil
				continue
			}
			raw = result
		}
		if field.selection == nil {
			out[responseKey] = raw
			continue
		}
		if raw == nil {
			out[responseKey] = nil
			continue
		}
		if arr, isArr := raw.([]any); isArr {
			childType := childTypeName(typeName, field.name)
			mapped := make([]any, 0, len(arr))
			for i, element := range arr {
				mapped = append(mapped, executeSelection(element, childType, field.selection, ctx, append(append([]any(nil), fieldPath...), i)))
			}
			out[responseKey] = mapped
			continue
		}
		if _, isMap := raw.(map[string]any); isMap {
			childType := typeNameOf(raw)
			if childType == "" {
				childType = childTypeName(typeName, field.name)
			}
			out[responseKey] = executeSelection(raw, childType, field.selection, ctx, fieldPath)
			continue
		}
		out[responseKey] = raw
	}
	return out
}

func executeOperation(f *Forge, op *operationNode, fragments map[string]*fragmentNode, variables map[string]any) map[string]any {
	ctx := &execContext{forge: f, variables: variables, fragments: fragments}
	isMutation := op.operation == "mutation"
	rootType := "Query"
	if isMutation {
		rootType = "Mutation"
	}
	data := map[string]any{}
	for _, item := range op.selection {
		if item.kind == "field" {
			field := item.field
			responseKey := field.name
			if field.alias != "" {
				responseKey = field.alias
			}
			raw, err := resolveRootField(f, field, ctx, isMutation)
			if err != nil {
				ctx.errors = append(ctx.errors, gqlError{message: err.Error(), path: []any{responseKey}})
				data[responseKey] = nil
				continue
			}
			if field.selection == nil {
				data[responseKey] = raw
			} else if raw == nil {
				data[responseKey] = nil
			} else {
				typeName := typeNameOf(raw)
				if typeName == "" {
					typeName = childTypeName(rootType, field.name)
				}
				data[responseKey] = executeSelection(raw, typeName, field.selection, ctx, []any{responseKey})
			}
			continue
		}
		if item.kind == "inline" {
			if merged := executeSelection(map[string]any{}, rootType, item.selection, ctx, []any{}); merged != nil {
				if m, ok := merged.(map[string]any); ok {
					for k, v := range m {
						data[k] = v
					}
				}
			}
		}
		// A root-level fragment spread is silently skipped, as in the reference.
	}
	result := map[string]any{"data": data}
	if len(ctx.errors) > 0 {
		errs := make([]any, 0, len(ctx.errors))
		for _, e := range ctx.errors {
			entry := map[string]any{"message": e.message}
			if e.path != nil {
				entry["path"] = e.path
			}
			errs = append(errs, entry)
		}
		result["errors"] = errs
	}
	return result
}

// ---------------------------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------------------------

type graphqlRequest struct {
	method string
	json   any
}

func handleGraphql(f *Forge, req graphqlRequest) *HandlerResult {
	if req.method != "POST" {
		return jsonResult(404, map[string]any{"message": "Not Found"})
	}
	body := req.json
	if body == nil {
		return jsonResult(400, map[string]any{"errors": []any{map[string]any{"message": "GraphQL request body must be an object or an array of objects"}}})
	}
	switch body.(type) {
	case map[string]any, []any:
	default:
		return jsonResult(400, map[string]any{"errors": []any{map[string]any{"message": "GraphQL request body must be an object or an array of objects"}}})
	}
	isBatch := false
	var requests []any
	if arr, ok := body.([]any); ok {
		isBatch = true
		requests = arr
	} else {
		requests = []any{body}
	}
	results := make([]any, 0, len(requests))
	for _, single := range requests {
		var reqObj map[string]any
		if m, ok := single.(map[string]any); ok {
			reqObj = m
		}
		query, queryOK := reqObj["query"].(string)
		if !queryOK {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "GraphQL query must be a string"}}})
			continue
		}
		defs, err := (&gqlParser{src: query}).parseDocument()
		if err != nil {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "GraphQL syntax error: " + err.Error()}}})
			continue
		}
		fragments := map[string]*fragmentNode{}
		var operations []*operationNode
		for _, def := range defs {
			if def.isFragment {
				fragments[def.frag.name] = def.frag
			} else {
				operations = append(operations, def.op)
			}
		}
		opName := ""
		opNameGiven := false
		if s, ok := reqObj["operationName"].(string); ok {
			opName = s
			opNameGiven = true
		}
		var op *operationNode
		if !opNameGiven {
			if len(operations) > 0 {
				op = operations[0]
			}
		} else {
			for _, o := range operations {
				if o.name == opName {
					op = o
					break
				}
			}
		}
		if op == nil {
			msg := "No operation provided"
			if opNameGiven {
				msg = fmt.Sprintf("Unknown operation named \"%s\"", opName)
			}
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": msg}}})
			continue
		}
		variables := map[string]any{}
		if v, ok := reqObj["variables"].(map[string]any); ok {
			variables = v
		}
		// Operation variable defaults for variables the caller did not supply.
		for _, v := range op.variables {
			if _, exists := variables[v.name]; !exists && v.defaultValue != nil {
				defCtx := &execContext{forge: f, variables: variables, fragments: fragments}
				variables[v.name] = resolveValue(v.defaultValue, defCtx)
			}
		}
		results = append(results, executeOperation(f, op, fragments, variables))
	}
	if isBatch {
		return jsonResult(200, results)
	}
	return jsonResult(200, results[0])
}
