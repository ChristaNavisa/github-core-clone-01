// GraphQL v4: a small recursive-descent parser plus an executor for the exact
// documents the suite sends (see the kit's graphql-client.ts). No external
// GraphQL library — only the 18 documents, introspection of the four types the
// suite checks, batch requests, and global ids that match the REST node_id.

import {
  FIXED_ISO,
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  globalId,
  parseGlobalId,
  type Forge,
  type Issue,
  type Pull,
  type Repo,
} from './store.ts';
import { errorResult, jsonResult, type HandlerResult } from './web.ts';

export interface GraphqlRequest {
  method: string;
  segments: string[];
  query: URLSearchParams;
  headers: Record<string, string | undefined>;
  json: unknown | null;
}

// ---------------------------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------------------------

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: Array<{ name: string; value: ValueNode }> };

interface FieldNode {
  alias: string | null;
  name: string;
  args: Array<{ name: string; value: ValueNode }>;
  selection: SelectionNode | null;
}

interface FragmentSpreadNode {
  kind: 'spread';
  name: string;
  selection: null;
}

interface InlineFragmentNode {
  kind: 'inline';
  typeCondition: string | null;
  selection: SelectionNode;
}

type SelectionItem = FieldNode | FragmentSpreadNode | InlineFragmentNode;
type SelectionNode = SelectionItem[];

interface OperationNode {
  kind: 'operation';
  operation: 'query' | 'mutation';
  name: string | null;
  variables: Array<{ name: string; defaultValue: ValueNode | null }>;
  selection: SelectionNode;
}

interface FragmentNode {
  kind: 'fragment';
  name: string;
  typeCondition: string;
  selection: SelectionNode;
}

type DefinitionNode = OperationNode | FragmentNode;

const NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*/;

class Parser {
  private pos = 0;
  private readonly src: string;

  constructor(src: string) {
    this.src = src;
  }

  parseDocument(): DefinitionNode[] {
    const defs: DefinitionNode[] = [];
    this.skipIgnored();
    while (this.pos < this.src.length) {
      defs.push(this.parseDefinition());
      this.skipIgnored();
    }
    return defs;
  }

  private peek(): string {
    return this.src[this.pos] ?? '';
  }

  private skipIgnored(): void {
    for (;;) {
      const c = this.peek();
      if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',') {
        this.pos += 1;
        continue;
      }
      if (c === '#') {
        while (this.pos < this.src.length && this.src[this.pos] !== '\n') this.pos += 1;
        continue;
      }
      return;
    }
  }

  private expect(ch: string): void {
    this.skipIgnored();
    if (this.peek() !== ch) throw new Error(`expected ${JSON.stringify(ch)} at ${this.pos}`);
    this.pos += 1;
  }

  private matchWord(word: string): boolean {
    this.skipIgnored();
    if (this.src.startsWith(word, this.pos)) {
      const after = this.src[this.pos + word.length] ?? '';
      if (after === '' || !/[A-Za-z0-9_]/.test(after)) {
        this.pos += word.length;
        return true;
      }
    }
    return false;
  }

  private readName(): string {
    this.skipIgnored();
    const m = NAME_RE.exec(this.src.slice(this.pos));
    if (m === null) throw new Error(`expected a name at ${this.pos}`);
    this.pos += m[0].length;
    return m[0];
  }

  private readString(): string {
    this.skipIgnored();
    if (this.peek() !== '"') throw new Error(`expected a string at ${this.pos}`);
    this.pos += 1;
    let out = '';
    for (;;) {
      const c = this.src[this.pos];
      if (c === undefined) throw new Error('unterminated string');
      if (c === '"') {
        this.pos += 1;
        return out;
      }
      if (c === '\\') {
        const esc = this.src[this.pos + 1];
        this.pos += 2;
        if (esc === 'n') out += '\n';
        else if (esc === 't') out += '\t';
        else if (esc === 'r') out += '\r';
        else if (esc === '"') out += '"';
        else if (esc === '\\') out += '\\';
        else if (esc === '/') out += '/';
        else if (esc === 'u') {
          out += String.fromCharCode(Number.parseInt(this.src.slice(this.pos, this.pos + 4), 16));
          this.pos += 4;
        } else out += esc ?? '';
        continue;
      }
      out += c;
      this.pos += 1;
    }
  }

  private parseDefinition(): DefinitionNode {
    this.skipIgnored();
    if (this.peek() === '{') {
      return { kind: 'operation', operation: 'query', name: null, variables: [], selection: this.parseSelection() };
    }
    let operation: 'query' | 'mutation' | null = null;
    if (this.matchWord('query')) operation = 'query';
    else if (this.matchWord('mutation')) operation = 'mutation';
    if (operation !== null) {
      let name: string | null = null;
      this.skipIgnored();
      if (/[A-Za-z_]/.test(this.peek())) name = this.readName();
      const variables = this.parseVariableDefinitions();
      return { kind: 'operation', operation, name, variables, selection: this.parseSelection() };
    }
    if (this.matchWord('fragment')) {
      const name = this.readName();
      if (!this.matchWord('on')) throw new Error('expected "on" in a fragment');
      const typeCondition = this.readName();
      return { kind: 'fragment', name, typeCondition, selection: this.parseSelection() };
    }
    throw new Error(`unexpected token at ${this.pos}`);
  }

  private parseVariableDefinitions(): Array<{ name: string; defaultValue: ValueNode | null }> {
    const out: Array<{ name: string; defaultValue: ValueNode | null }> = [];
    this.skipIgnored();
    if (this.peek() !== '(') return out;
    this.pos += 1;
    for (;;) {
      this.skipIgnored();
      if (this.peek() === ')') {
        this.pos += 1;
        return out;
      }
      if (this.peek() !== '$') throw new Error('expected a variable in variable definitions');
      this.pos += 1;
      const name = this.readName();
      this.expect(':');
      this.parseType();
      let defaultValue: ValueNode | null = null;
      this.skipIgnored();
      if (this.peek() === '=') {
        this.pos += 1;
        defaultValue = this.parseValue();
      }
      out.push({ name, defaultValue });
    }
  }

  private parseType(): void {
    this.skipIgnored();
    if (this.peek() === '[') {
      this.pos += 1;
      this.parseType();
      this.expect(']');
    } else {
      this.readName();
    }
    this.skipIgnored();
    if (this.peek() === '!') this.pos += 1;
  }

  private parseSelection(): SelectionNode {
    this.expect('{');
    const items: SelectionItem[] = [];
    for (;;) {
      this.skipIgnored();
      if (this.peek() === '}') {
        this.pos += 1;
        return items;
      }
      items.push(this.parseSelectionItem());
    }
  }

  private parseSelectionItem(): SelectionItem {
    this.skipIgnored();
    if (this.src.startsWith('...', this.pos)) {
      this.pos += 3;
      this.skipIgnored();
      if (this.matchWord('on')) {
        const typeCondition = this.readName();
        return { kind: 'inline', typeCondition, selection: this.parseSelection() };
      }
      const name = this.readName();
      return { kind: 'spread', name, selection: null };
    }
    let alias: string | null = null;
    let name = this.readName();
    this.skipIgnored();
    if (this.peek() === ':') {
      this.pos += 1;
      alias = name;
      name = this.readName();
    }
    const args = this.parseArguments();
    this.skipDirectives();
    let selection: SelectionNode | null = null;
    this.skipIgnored();
    if (this.peek() === '{') selection = this.parseSelection();
    return { alias, name, args, selection };
  }

  private skipDirectives(): void {
    for (;;) {
      this.skipIgnored();
      if (this.peek() !== '@') return;
      this.pos += 1;
      this.readName();
      this.parseArguments();
    }
  }

  private parseArguments(): Array<{ name: string; value: ValueNode }> {
    const args: Array<{ name: string; value: ValueNode }> = [];
    this.skipIgnored();
    if (this.peek() !== '(') return args;
    this.pos += 1;
    for (;;) {
      this.skipIgnored();
      if (this.peek() === ')') {
        this.pos += 1;
        return args;
      }
      const name = this.readName();
      this.expect(':');
      args.push({ name, value: this.parseValue() });
    }
  }

  private parseValue(): ValueNode {
    this.skipIgnored();
    const c = this.peek();
    if (c === '$') {
      this.pos += 1;
      return { kind: 'variable', name: this.readName() };
    }
    if (c === '"') return { kind: 'string', value: this.readString() };
    if (c === '[') {
      this.pos += 1;
      const values: ValueNode[] = [];
      for (;;) {
        this.skipIgnored();
        if (this.peek() === ']') {
          this.pos += 1;
          return { kind: 'list', values };
        }
        values.push(this.parseValue());
      }
    }
    if (c === '{') {
      this.pos += 1;
      const fields: Array<{ name: string; value: ValueNode }> = [];
      for (;;) {
        this.skipIgnored();
        if (this.peek() === '}') {
          this.pos += 1;
          return { kind: 'object', fields };
        }
        const name = this.readName();
        this.expect(':');
        fields.push({ name, value: this.parseValue() });
      }
    }
    if (c === '-' || /[0-9]/.test(c)) {
      const m = /^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/.exec(this.src.slice(this.pos));
      if (m === null) throw new Error(`bad number at ${this.pos}`);
      this.pos += m[0].length;
      return m[0].includes('.') || m[0].includes('e') || m[0].includes('E')
        ? { kind: 'float', value: Number.parseFloat(m[0]) }
        : { kind: 'int', value: Number.parseInt(m[0], 10) };
    }
    const name = this.readName();
    if (name === 'true') return { kind: 'boolean', value: true };
    if (name === 'false') return { kind: 'boolean', value: false };
    if (name === 'null') return { kind: 'null' };
    return { kind: 'enum', value: name };
  }
}

// ---------------------------------------------------------------------------------------------
// schema model (only what the suite introspects)
// ---------------------------------------------------------------------------------------------

interface TypeRef {
  kind: 'SCALAR' | 'OBJECT' | 'ENUM' | 'NON_NULL' | 'LIST' | 'INPUT_OBJECT';
  name?: string;
  ofType?: TypeRef;
}

const T = {
  str: { kind: 'SCALAR', name: 'String' } as TypeRef,
  int: { kind: 'SCALAR', name: 'Int' } as TypeRef,
  bool: { kind: 'SCALAR', name: 'Boolean' } as TypeRef,
  id: { kind: 'SCALAR', name: 'ID' } as TypeRef,
  obj: (name: string): TypeRef => ({ kind: 'OBJECT', name }),
  enum: (name: string): TypeRef => ({ kind: 'ENUM', name }),
  nonNull: (t: TypeRef): TypeRef => ({ kind: 'NON_NULL', ofType: t }),
  list: (t: TypeRef): TypeRef => ({ kind: 'LIST', ofType: t }),
};

interface FieldDef {
  name: string;
  type: TypeRef;
}

interface TypeDef {
  kind: 'OBJECT' | 'ENUM' | 'SCALAR' | 'INPUT_OBJECT';
  fields?: FieldDef[];
  enumValues?: string[];
}

const SCHEMA: Record<string, TypeDef> = {
  Query: {
    kind: 'OBJECT',
    fields: [
      { name: 'viewer', type: T.obj('User') },
      { name: 'repository', type: T.obj('Repository') },
      { name: 'node', type: T.obj('Node') },
    ],
  },
  Mutation: {
    kind: 'OBJECT',
    fields: [
      { name: 'createIssue', type: T.obj('CreateIssuePayload') },
      { name: 'closeIssue', type: T.obj('CloseIssuePayload') },
      { name: 'createPullRequest', type: T.obj('CreatePullRequestPayload') },
      { name: 'mergePullRequest', type: T.obj('MergePullRequestPayload') },
      { name: 'addComment', type: T.obj('AddCommentPayload') },
    ],
  },
  User: {
    kind: 'OBJECT',
    fields: [
      { name: 'login', type: T.nonNull(T.str) },
      { name: 'id', type: T.nonNull(T.id) },
      { name: 'name', type: T.str },
      { name: 'email', type: T.str },
      { name: 'databaseId', type: T.int },
    ],
  },
  Repository: {
    kind: 'OBJECT',
    fields: [
      { name: 'id', type: T.nonNull(T.id) },
      { name: 'databaseId', type: T.int },
      { name: 'name', type: T.nonNull(T.str) },
      { name: 'nameWithOwner', type: T.nonNull(T.str) },
      { name: 'owner', type: T.obj('User') },
      { name: 'defaultBranchRef', type: T.obj('Ref') },
      { name: 'viewerPermission', type: T.enum('RepositoryPermission') },
      { name: 'hasIssuesEnabled', type: T.bool },
      { name: 'issue', type: T.obj('Issue') },
      { name: 'issues', type: T.obj('IssueConnection') },
      { name: 'pullRequests', type: T.obj('PullRequestConnection') },
      { name: 'pullRequest', type: T.obj('PullRequest') },
      { name: 'url', type: T.str },
    ],
  },
  Ref: { kind: 'OBJECT', fields: [{ name: 'name', type: T.nonNull(T.str) }] },
  Issue: {
    kind: 'OBJECT',
    fields: [
      { name: 'id', type: T.nonNull(T.id) },
      { name: 'databaseId', type: T.int },
      { name: 'number', type: T.nonNull(T.int) },
      { name: 'title', type: T.nonNull(T.str) },
      { name: 'body', type: T.str },
      { name: 'state', type: T.enum('IssueState') },
      { name: 'author', type: T.obj('User') },
      { name: 'labels', type: T.obj('LabelConnection') },
      { name: 'comments', type: T.obj('IssueCommentConnection') },
      { name: 'createdAt', type: T.str },
      { name: 'updatedAt', type: T.str },
      { name: 'closedAt', type: T.str },
      { name: 'url', type: T.str },
    ],
  },
  PullRequest: {
    kind: 'OBJECT',
    fields: [
      { name: 'id', type: T.nonNull(T.id) },
      { name: 'databaseId', type: T.int },
      { name: 'number', type: T.nonNull(T.int) },
      { name: 'title', type: T.nonNull(T.str) },
      { name: 'body', type: T.str },
      { name: 'state', type: T.enum('PullRequestState') },
      { name: 'headRefName', type: T.nonNull(T.str) },
      { name: 'baseRefName', type: T.nonNull(T.str) },
      { name: 'headRefOid', type: T.str },
      { name: 'baseRefOid', type: T.str },
      { name: 'mergeable', type: T.enum('MergeableState') },
      { name: 'merged', type: T.bool },
      { name: 'mergeCommit', type: T.obj('Commit') },
      { name: 'files', type: T.obj('PullRequestFileConnection') },
      { name: 'author', type: T.obj('User') },
      { name: 'createdAt', type: T.str },
      { name: 'updatedAt', type: T.str },
      { name: 'mergedAt', type: T.str },
      { name: 'url', type: T.str },
    ],
  },
  Commit: { kind: 'OBJECT', fields: [{ name: 'oid', type: T.nonNull(T.str) }] },
  IssueConnection: {
    kind: 'OBJECT',
    fields: [
      { name: 'nodes', type: T.list(T.obj('Issue')) },
      { name: 'edges', type: T.list(T.obj('IssueEdge')) },
      { name: 'pageInfo', type: T.nonNull(T.obj('PageInfo')) },
      { name: 'totalCount', type: T.nonNull(T.int) },
    ],
  },
  PullRequestConnection: {
    kind: 'OBJECT',
    fields: [
      { name: 'nodes', type: T.list(T.obj('PullRequest')) },
      { name: 'edges', type: T.list(T.obj('PullRequestEdge')) },
      { name: 'pageInfo', type: T.nonNull(T.obj('PageInfo')) },
      { name: 'totalCount', type: T.nonNull(T.int) },
    ],
  },
  IssueCommentConnection: {
    kind: 'OBJECT',
    fields: [
      { name: 'nodes', type: T.list(T.obj('IssueComment')) },
      { name: 'edges', type: T.list(T.obj('IssueCommentEdge')) },
      { name: 'pageInfo', type: T.nonNull(T.obj('PageInfo')) },
      { name: 'totalCount', type: T.nonNull(T.int) },
    ],
  },
  PullRequestFileConnection: {
    kind: 'OBJECT',
    fields: [
      { name: 'nodes', type: T.list(T.obj('PullRequestFile')) },
      { name: 'edges', type: T.list(T.obj('PullRequestFileEdge')) },
      { name: 'pageInfo', type: T.nonNull(T.obj('PageInfo')) },
      { name: 'totalCount', type: T.nonNull(T.int) },
    ],
  },
  LabelConnection: {
    kind: 'OBJECT',
    fields: [
      { name: 'nodes', type: T.list(T.obj('Label')) },
      { name: 'edges', type: T.list(T.obj('LabelEdge')) },
      { name: 'pageInfo', type: T.nonNull(T.obj('PageInfo')) },
      { name: 'totalCount', type: T.nonNull(T.int) },
    ],
  },
  IssueEdge: { kind: 'OBJECT', fields: [{ name: 'node', type: T.obj('Issue') }, { name: 'cursor', type: T.nonNull(T.str) }] },
  PullRequestEdge: { kind: 'OBJECT', fields: [{ name: 'node', type: T.obj('PullRequest') }, { name: 'cursor', type: T.nonNull(T.str) }] },
  IssueCommentEdge: { kind: 'OBJECT', fields: [{ name: 'node', type: T.obj('IssueComment') }, { name: 'cursor', type: T.nonNull(T.str) }] },
  PullRequestFileEdge: { kind: 'OBJECT', fields: [{ name: 'node', type: T.obj('PullRequestFile') }, { name: 'cursor', type: T.nonNull(T.str) }] },
  LabelEdge: { kind: 'OBJECT', fields: [{ name: 'node', type: T.obj('Label') }, { name: 'cursor', type: T.nonNull(T.str) }] },
  IssueComment: {
    kind: 'OBJECT',
    fields: [
      { name: 'id', type: T.nonNull(T.id) },
      { name: 'databaseId', type: T.int },
      { name: 'body', type: T.nonNull(T.str) },
      { name: 'author', type: T.obj('User') },
      { name: 'createdAt', type: T.str },
      { name: 'updatedAt', type: T.str },
    ],
  },
  PullRequestFile: {
    kind: 'OBJECT',
    fields: [
      { name: 'path', type: T.nonNull(T.str) },
      { name: 'additions', type: T.nonNull(T.int) },
      { name: 'deletions', type: T.nonNull(T.int) },
      { name: 'changes', type: T.nonNull(T.int) },
    ],
  },
  Label: {
    kind: 'OBJECT',
    fields: [
      { name: 'id', type: T.nonNull(T.id) },
      { name: 'name', type: T.nonNull(T.str) },
      { name: 'color', type: T.nonNull(T.str) },
      { name: 'description', type: T.str },
    ],
  },
  PageInfo: {
    kind: 'OBJECT',
    fields: [
      { name: 'hasNextPage', type: T.nonNull(T.bool) },
      { name: 'hasPreviousPage', type: T.nonNull(T.bool) },
      { name: 'startCursor', type: T.str },
      { name: 'endCursor', type: T.str },
    ],
  },
  CreateIssuePayload: { kind: 'OBJECT', fields: [{ name: 'issue', type: T.obj('Issue') }, { name: 'clientMutationId', type: T.str }] },
  CloseIssuePayload: { kind: 'OBJECT', fields: [{ name: 'issue', type: T.obj('Issue') }, { name: 'clientMutationId', type: T.str }] },
  CreatePullRequestPayload: { kind: 'OBJECT', fields: [{ name: 'pullRequest', type: T.obj('PullRequest') }, { name: 'clientMutationId', type: T.str }] },
  MergePullRequestPayload: { kind: 'OBJECT', fields: [{ name: 'pullRequest', type: T.obj('PullRequest') }, { name: 'clientMutationId', type: T.str }] },
  AddCommentPayload: { kind: 'OBJECT', fields: [{ name: 'commentEdge', type: T.obj('IssueCommentEdge') }, { name: 'subject', type: T.obj('Issue') }, { name: 'clientMutationId', type: T.str }] },
  Node: { kind: 'OBJECT', fields: [] },
  IssueState: { kind: 'ENUM', enumValues: ['OPEN', 'CLOSED'] },
  PullRequestState: { kind: 'ENUM', enumValues: ['OPEN', 'CLOSED', 'MERGED'] },
  MergeableState: { kind: 'ENUM', enumValues: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
  RepositoryPermission: { kind: 'ENUM', enumValues: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'] },
  SearchType: { kind: 'ENUM', enumValues: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT'] },
  String: { kind: 'SCALAR' },
  Int: { kind: 'SCALAR' },
  Boolean: { kind: 'SCALAR' },
  ID: { kind: 'SCALAR' },
};

// ---------------------------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------------------------

interface ExecContext {
  forge: Forge;
  variables: Record<string, unknown>;
  fragments: Map<string, FragmentNode>;
  errors: Array<{ message: string; path?: Array<string | number> }>;
}

type Value = unknown;

function resolveValue(node: ValueNode, ctx: ExecContext): Value {
  switch (node.kind) {
    case 'variable': {
      const v = ctx.variables[node.name];
      return v === undefined ? null : v;
    }
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
    case 'enum':
      return node.value;
    case 'null':
      return null;
    case 'list':
      return node.values.map((v) => resolveValue(v, ctx));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const f of node.fields) out[f.name] = resolveValue(f.value, ctx);
      return out;
    }
  }
}

function argsOf(field: FieldNode, ctx: ExecContext): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const a of field.args) out[a.name] = resolveValue(a.value, ctx);
  return out;
}

function typeNameOf(value: Value): string | null {
  if (value !== null && typeof value === 'object') {
    const t = (value as Record<string, unknown>).__typename;
    if (typeof t === 'string') return t;
  }
  return null;
}

function cursorFor(kind: string, key: string): string {
  return Buffer.from(`${kind}:${key}`, 'utf8').toString('base64');
}

function cursorKey(cursor: string | null | undefined): string | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  try {
    const decoded = Buffer.from(cursor, 'base64').toString('utf8');
    const idx = decoded.indexOf(':');
    return idx < 0 ? null : decoded.slice(idx + 1);
  } catch {
    return null;
  }
}

/** A Relay-style connection over an already-ordered list of node values. */
function connection<T>(kind: string, keyOf: (item: T) => string, items: readonly T[], args: Record<string, unknown>): Record<string, unknown> {
  const after = cursorKey(typeof args.after === 'string' ? args.after : null);
  let start = 0;
  if (after !== null) {
    const idx = items.findIndex((item) => keyOf(item) === after);
    start = idx < 0 ? items.length : idx + 1;
  }
  const first = typeof args.first === 'number' ? Math.max(0, args.first) : items.length;
  const page = items.slice(start, start + first);
  const hasNextPage = start + page.length < items.length;
  return {
    __typename: 'Connection',
    nodes: page,
    edges: page.map((item) => ({ __typename: 'Edge', node: item, cursor: cursorFor(kind, keyOf(item)) })),
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage,
      hasPreviousPage: start > 0,
      startCursor: page.length > 0 ? cursorFor(kind, keyOf(page[0] as T)) : null,
      endCursor: page.length > 0 ? cursorFor(kind, keyOf(page[page.length - 1] as T)) : null,
    },
    totalCount: items.length,
  };
}

function userValue(): Record<string, unknown> {
  return {
    __typename: 'User',
    login: VIEWER_LOGIN,
    id: globalId('User', VIEWER_LOGIN),
    databaseId: VIEWER_ID,
    name: VIEWER_NAME,
    email: VIEWER_EMAIL,
  };
}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullState(pull: Pull): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function issueValue(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    __typename: 'Issue',
    id: globalId('Issue', VIEWER_LOGIN, repo.name, String(issue.number)),
    databaseId: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issueState(issue),
    author: userValue(),
    labels: (_args: Record<string, unknown>) =>
      connection('label', (l: { name: string }) => l.name, issue.labels.map((name) => {
        const found = repo.labels.get(name);
        return { __typename: 'Label', id: globalId('Label', name), name, color: found?.color ?? '000000', description: '' };
      }), _args),
    comments: (_args: Record<string, unknown>) =>
      connection('comment', (c: { id: number }) => String(c.id), issue.comments.map((c) => commentValue(repo, issue, c)), _args),
    createdAt: FIXED_ISO,
    updatedAt: FIXED_ISO,
    closedAt: issue.state === 'closed' ? FIXED_ISO : null,
    url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/issues/${issue.number}`,
  };
}

function commentValue(repo: Repo, issue: Issue, comment: { id: number; body: string }): Record<string, unknown> {
  return {
    __typename: 'IssueComment',
    id: globalId('IssueComment', VIEWER_LOGIN, repo.name, String(issue.number), String(comment.id)),
    databaseId: comment.id,
    body: comment.body,
    author: userValue(),
    createdAt: FIXED_ISO,
    updatedAt: FIXED_ISO,
  };
}

function pullValue(forge: Forge, repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    __typename: 'PullRequest',
    id: globalId('PullRequest', VIEWER_LOGIN, repo.name, String(pull.number)),
    databaseId: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pullState(pull),
    headRefName: pull.head,
    baseRefName: pull.base,
    headRefOid: pull.headSha,
    baseRefOid: forge.baseTip(repo, pull),
    mergeable: forge.pullMergeable(repo, pull) ? 'MERGEABLE' : 'CONFLICTING',
    merged: pull.merged,
    mergeCommit: pull.mergeCommitSha === null ? null : { __typename: 'Commit', oid: pull.mergeCommitSha },
    files: (_args: Record<string, unknown>) =>
      connection('file', (f: { path: string }) => f.path, forge.pullFiles(repo, pull).map((f) => ({ __typename: 'PullRequestFile', path: f.path, additions: f.additions, deletions: f.deletions, changes: f.additions + f.deletions })), _args),
    author: userValue(),
    createdAt: FIXED_ISO,
    updatedAt: FIXED_ISO,
    mergedAt: pull.merged ? FIXED_ISO : null,
    url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/pull/${pull.number}`,
  };
}

function repoValue(forge: Forge, repo: Repo): Record<string, unknown> {
  return {
    __typename: 'Repository',
    id: globalId('Repository', VIEWER_LOGIN, repo.name),
    databaseId: repo.id,
    name: repo.name,
    nameWithOwner: `${VIEWER_LOGIN}/${repo.name}`,
    owner: userValue(),
    defaultBranchRef: repo.refs.has(`refs/heads/${repo.defaultBranch}`) ? { __typename: 'Ref', name: repo.defaultBranch } : null,
    viewerPermission: 'ADMIN',
    hasIssuesEnabled: true,
    issue: (args: Record<string, unknown>) => {
      const number = typeof args.number === 'number' ? args.number : -1;
      const found = repo.issues.find((i) => i.number === number);
      return found === undefined ? null : issueValue(repo, found);
    },
    issues: (args: Record<string, unknown>) => {
      let items = [...repo.issues];
      const states = args.states;
      if (Array.isArray(states) && states.length > 0) {
        const want = new Set(states.map((s) => String(s)));
        items = items.filter((i) => want.has(issueState(i)));
      }
      const orderBy = args.orderBy as { field?: unknown; direction?: unknown } | undefined;
      const direction = orderBy?.direction === 'ASC' ? 'ASC' : 'DESC';
      items.sort((a, b) => (direction === 'ASC' ? a.number - b.number : b.number - a.number));
      return connection('issue', (i: Issue) => String(i.number), items.map((i) => issueValue(repo, i)), args);
    },
    pullRequests: (args: Record<string, unknown>) => {
      const items = [...repo.pulls].sort((a, b) => b.number - a.number);
      return connection('pull', (p: Pull) => String(p.number), items.map((p) => pullValue(forge, repo, p)), args);
    },
    pullRequest: (args: Record<string, unknown>) => {
      const number = typeof args.number === 'number' ? args.number : -1;
      const found = repo.pulls.find((p) => p.number === number);
      return found === undefined ? null : pullValue(forge, repo, found);
    },
    url: `https://github.com/${VIEWER_LOGIN}/${repo.name}`,
  };
}

// ---------------------------------------------------------------------------------------------
// introspection
// ---------------------------------------------------------------------------------------------

function typeRefValue(t: TypeRef): Record<string, unknown> {
  if (t.kind === 'NON_NULL' || t.kind === 'LIST') {
    return { __typename: '__Type', name: null, kind: t.kind, description: null, fields: null, enumValues: null, ofType: typeRefValue(t.ofType as TypeRef) };
  }
  const def = SCHEMA[t.name as string];
  return {
    __typename: '__Type',
    name: t.name ?? null,
    kind: t.kind,
    description: null,
    fields: (_args: Record<string, unknown>) => (def?.fields ?? []).map((f) => fieldValue(f)),
    enumValues: (_args: Record<string, unknown>) => (def?.enumValues ?? []).map((v) => ({ __typename: '__EnumValue', name: v, description: null })),
    inputFields: null,
    interfaces: [],
    possibleTypes: null,
    ofType: null,
  };
}

function fieldValue(f: FieldDef): Record<string, unknown> {
  return {
    __typename: '__Field',
    name: f.name,
    description: null,
    args: [],
    type: typeRefValue(f.type),
  };
}

function typeValue(name: string): Record<string, unknown> | null {
  const def = SCHEMA[name];
  if (def === undefined) return null;
  return {
    __typename: '__Type',
    name,
    kind: def.kind,
    description: null,
    fields: (_args: Record<string, unknown>) => (def.fields ?? []).map((f) => fieldValue(f)),
    enumValues: (_args: Record<string, unknown>) => (def.enumValues ?? []).map((v) => ({ __typename: '__EnumValue', name: v, description: null })),
    inputFields: null,
    interfaces: [],
    possibleTypes: null,
    ofType: null,
  };
}

function schemaValue(): Record<string, unknown> {
  return {
    __typename: '__Schema',
    queryType: typeValue('Query'),
    mutationType: typeValue('Mutation'),
    subscriptionType: null,
    types: Object.keys(SCHEMA).map((name) => typeValue(name)),
    directives: [],
  };
}

// ---------------------------------------------------------------------------------------------
// field resolution
// ---------------------------------------------------------------------------------------------

function resolveRootField(forge: Forge, field: FieldNode, ctx: ExecContext, isMutation: boolean): Value {
  const args = argsOf(field, ctx);
  if (!isMutation) {
    if (field.name === 'viewer') return userValue();
    if (field.name === 'repository') {
      const owner = args.owner;
      const name = args.name;
      if (typeof owner !== 'string' || typeof name !== 'string') return null;
      if (owner !== VIEWER_LOGIN) return null;
      const repo = forge.getRepo(owner, name);
      return repo === undefined ? null : repoValue(forge, repo);
    }
    if (field.name === 'node') {
      const id = args.id;
      if (typeof id !== 'string') return null;
      const parsed = parseGlobalId(id);
      if (parsed === null) return null;
      if (parsed.kind === 'Repository') {
        const repo = forge.getRepo(parsed.parts[0] as string, parsed.parts[1] as string);
        return repo === undefined ? null : repoValue(forge, repo);
      }
      if (parsed.kind === 'Issue') {
        const repo = forge.getRepo(parsed.parts[0] as string, parsed.parts[1] as string);
        const issue = repo?.issues.find((i) => i.number === Number.parseInt(parsed.parts[2] as string, 10));
        return repo !== undefined && issue !== undefined ? issueValue(repo, issue) : null;
      }
      if (parsed.kind === 'PullRequest') {
        const repo = forge.getRepo(parsed.parts[0] as string, parsed.parts[1] as string);
        const pull = repo?.pulls.find((p) => p.number === Number.parseInt(parsed.parts[2] as string, 10));
        return repo !== undefined && pull !== undefined ? pullValue(forge, repo, pull) : null;
      }
      return null;
    }
    if (field.name === '__type') {
      return typeof args.name === 'string' ? typeValue(args.name) : null;
    }
    if (field.name === '__schema') return schemaValue();
  } else {
    const input = (args.input ?? {}) as Record<string, unknown>;
    if (field.name === 'createIssue') {
      const parsed = typeof input.repositoryId === 'string' ? parseGlobalId(input.repositoryId) : null;
      if (parsed === null || parsed.kind !== 'Repository') throw new Error('createIssue: repositoryId is not a repository');
      const repo = forge.getRepo(parsed.parts[0] as string, parsed.parts[1] as string);
      if (repo === undefined) throw new Error('createIssue: repository not found');
      const issue = forge.createIssue(repo, String(input.title ?? ''), typeof input.body === 'string' ? input.body : '', []);
      return { __typename: 'CreateIssuePayload', issue: issueValue(repo, issue), clientMutationId: typeof input.clientMutationId === 'string' ? input.clientMutationId : null };
    }
    if (field.name === 'closeIssue') {
      const parsed = typeof input.issueId === 'string' ? parseGlobalId(input.issueId) : null;
      if (parsed === null || parsed.kind !== 'Issue') throw new Error('closeIssue: issueId is not an issue');
      const repo = forge.getRepo(parsed.parts[0] as string, parsed.parts[1] as string);
      const issue = repo?.issues.find((i) => i.number === Number.parseInt(parsed.parts[2] as string, 10));
      if (repo === undefined || issue === undefined) throw new Error('closeIssue: issue not found');
      issue.state = 'closed';
      repo.issueJsonCache.delete(issue.number);
      return { __typename: 'CloseIssuePayload', issue: issueValue(repo, issue), clientMutationId: typeof input.clientMutationId === 'string' ? input.clientMutationId : null };
    }
    if (field.name === 'createPullRequest') {
      const parsed = typeof input.repositoryId === 'string' ? parseGlobalId(input.repositoryId) : null;
      if (parsed === null || parsed.kind !== 'Repository') throw new Error('createPullRequest: repositoryId is not a repository');
      const repo = forge.getRepo(parsed.parts[0] as string, parsed.parts[1] as string);
      if (repo === undefined) throw new Error('createPullRequest: repository not found');
      const headName = String(input.headRefName ?? '');
      const baseName = String(input.baseRefName ?? '');
      const headSha = repo.refs.get(`refs/heads/${headName}`);
      const baseSha = repo.refs.get(`refs/heads/${baseName}`);
      if (headSha === undefined || baseSha === undefined) throw new Error('createPullRequest: head or base branch does not exist');
      const duplicate = repo.pulls.some((p) => p.state === 'open' && p.head === headName && p.base === baseName);
      if (duplicate) throw new Error('createPullRequest: a pull request already exists for these branches');
      const pull = forge.createPull(repo, String(input.title ?? ''), typeof input.body === 'string' ? input.body : '', headName, baseName, headSha, baseSha);
      return { __typename: 'CreatePullRequestPayload', pullRequest: pullValue(forge, repo, pull), clientMutationId: typeof input.clientMutationId === 'string' ? input.clientMutationId : null };
    }
    if (field.name === 'mergePullRequest') {
      const parsed = typeof input.pullRequestId === 'string' ? parseGlobalId(input.pullRequestId) : null;
      if (parsed === null || parsed.kind !== 'PullRequest') throw new Error('mergePullRequest: pullRequestId is not a pull request');
      const repo = forge.getRepo(parsed.parts[0] as string, parsed.parts[1] as string);
      const pull = repo?.pulls.find((p) => p.number === Number.parseInt(parsed.parts[2] as string, 10));
      if (repo === undefined || pull === undefined) throw new Error('mergePullRequest: pull request not found');
      if (pull.merged) throw new Error('mergePullRequest: pull request is already merged');
      if (pull.state !== 'open') throw new Error('mergePullRequest: pull request is not open');
      const merged = forge.mergePull(repo, pull);
      if (merged === null) throw new Error('mergePullRequest: merge conflict');
      return { __typename: 'MergePullRequestPayload', pullRequest: pullValue(forge, repo, pull), clientMutationId: typeof input.clientMutationId === 'string' ? input.clientMutationId : null };
    }
    if (field.name === 'addComment') {
      const parsed = typeof input.subjectId === 'string' ? parseGlobalId(input.subjectId) : null;
      if (parsed === null) throw new Error('addComment: subjectId is not a known object');
      const repo = forge.getRepo(parsed.parts[0] as string, parsed.parts[1] as string);
      if (repo === undefined) throw new Error('addComment: repository not found');
      if (parsed.kind === 'Issue') {
        const issue = repo.issues.find((i) => i.number === Number.parseInt(parsed.parts[2] as string, 10));
        if (issue === undefined) throw new Error('addComment: issue not found');
        const comment = forge.addComment(repo, issue, String(input.body ?? ''));
        const node = commentValue(repo, issue, comment);
        return { __typename: 'AddCommentPayload', commentEdge: { __typename: 'IssueCommentEdge', node, cursor: cursorFor('comment', String(comment.id)) }, subject: issueValue(repo, issue), clientMutationId: typeof input.clientMutationId === 'string' ? input.clientMutationId : null };
      }
      throw new Error('addComment: subject is not an issue');
    }
  }
  throw new Error(`Cannot query field "${field.name}" on type "${isMutation ? 'Mutation' : 'Query'}".`);
}

/**
 * Execute a selection set against `value`. `typeName` is the GraphQL type of
 * `value` (used for unknown-field errors and for `... on Type` fragments).
 */
function executeSelection(value: Value, typeName: string | null, selection: SelectionNode, ctx: ExecContext, path: Array<string | number>): Value {
  if (value === null || value === undefined) return null;
  if (typeof value !== 'object') return value;
  const out: Record<string, unknown> = {};
  const object = value as Record<string, unknown>;
  for (const item of selection) {
    if (item.kind === 'spread') {
      const fragment = ctx.fragments.get(item.name);
      if (fragment === undefined) {
        ctx.errors.push({ message: `Unknown fragment "${item.name}".`, path: [...path] });
        continue;
      }
      const merged = executeSelection(value, typeName, fragment.selection, ctx, path);
      if (merged !== null && typeof merged === 'object') Object.assign(out, merged);
      continue;
    }
    if (item.kind === 'inline') {
      if (item.typeCondition !== null && typeName !== null && item.typeCondition !== typeName) continue;
      const merged = executeSelection(value, typeName, item.selection, ctx, path);
      if (merged !== null && typeof merged === 'object') Object.assign(out, merged);
      continue;
    }
    const field = item;
    const responseKey = field.alias ?? field.name;
    const fieldPath = [...path, responseKey];
    if (field.name === '__typename') {
      out[responseKey] = typeName;
      continue;
    }
    const def = typeName !== null ? SCHEMA[typeName] : undefined;
    const known = def?.fields?.some((f) => f.name === field.name) ?? false;
    if (typeName !== null && def !== undefined && !known) {
      ctx.errors.push({ message: `Cannot query field "${field.name}" on type "${typeName}".`, path: fieldPath });
      out[responseKey] = null;
      continue;
    }
    let raw = object[field.name];
    if (typeof raw === 'function') {
      try {
        raw = (raw as (args: Record<string, unknown>) => Value)(argsOf(field, ctx));
      } catch (err) {
        ctx.errors.push({ message: (err as Error).message, path: fieldPath });
        out[responseKey] = null;
        continue;
      }
    }
    if (field.selection === null) {
      out[responseKey] = raw ?? null;
      continue;
    }
    if (raw === null || raw === undefined) {
      out[responseKey] = null;
      continue;
    }
    if (Array.isArray(raw)) {
      const childType = childTypeName(typeName, field.name);
      out[responseKey] = raw.map((element, i) => executeSelection(element, childType, field.selection as SelectionNode, ctx, [...fieldPath, i]));
      continue;
    }
    if (typeof raw === 'object') {
      const childType = typeNameOf(raw) ?? childTypeName(typeName, field.name);
      out[responseKey] = executeSelection(raw, childType, field.selection as SelectionNode, ctx, fieldPath);
      continue;
    }
    out[responseKey] = raw;
  }
  return out;
}

/** The GraphQL type of a field's value, from the schema model. */
function childTypeName(parentType: string | null, fieldName: string): string | null {
  if (parentType === null) return null;
  const field = SCHEMA[parentType]?.fields?.find((f) => f.name === fieldName);
  if (field === undefined) return null;
  let t = field.type;
  while (t.kind === 'NON_NULL' || t.kind === 'LIST') t = t.ofType as TypeRef;
  return t.name ?? null;
}

function executeOperation(forge: Forge, op: OperationNode, fragments: Map<string, FragmentNode>, variables: Record<string, unknown>): { data: Record<string, unknown> | null; errors?: Array<{ message: string; path?: Array<string | number> }> } {
  const ctx: ExecContext = { forge, variables, fragments, errors: [] };
  const isMutation = op.operation === 'mutation';
  const data: Record<string, unknown> = {};
  for (const item of op.selection) {
    if (item.kind !== 'spread' && item.kind !== 'inline') {
      const field = item;
      const responseKey = field.alias ?? field.name;
      try {
        const raw = resolveRootField(forge, field, ctx, isMutation);
        if (field.selection === null) {
          data[responseKey] = raw ?? null;
        } else if (raw === null || raw === undefined) {
          data[responseKey] = null;
        } else {
          const typeName = typeNameOf(raw) ?? childTypeName(isMutation ? 'Mutation' : 'Query', field.name);
          data[responseKey] = executeSelection(raw, typeName, field.selection, ctx, [responseKey]);
        }
      } catch (err) {
        ctx.errors.push({ message: (err as Error).message, path: [responseKey] });
        data[responseKey] = null;
      }
      continue;
    }
    if (item.kind === 'inline') {
      const merged = executeSelection({}, isMutation ? 'Mutation' : 'Query', item.selection, ctx, []);
      if (merged !== null && typeof merged === 'object') Object.assign(data, merged);
    }
  }
  const result: { data: Record<string, unknown> | null; errors?: Array<{ message: string; path?: Array<string | number> }> } = { data };
  if (ctx.errors.length > 0) result.errors = ctx.errors;
  return result;
}

// ---------------------------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------------------------

export function handleGraphql(forge: Forge, req: GraphqlRequest): HandlerResult {
  if (req.method !== 'POST') return jsonResult(404, { message: 'Not Found' });
  const body = req.json;
  if (body === null || (typeof body !== 'object' && !Array.isArray(body))) {
    return jsonResult(400, { errors: [{ message: 'GraphQL request body must be an object or an array of objects' }] });
  }
  const requests = Array.isArray(body) ? body : [body];
  const results: unknown[] = [];
  for (const single of requests) {
    const reqObj = single as { query?: unknown; variables?: unknown; operationName?: unknown };
    if (typeof reqObj.query !== 'string') {
      results.push({ data: null, errors: [{ message: 'GraphQL query must be a string' }] });
      continue;
    }
    let defs: DefinitionNode[];
    try {
      defs = new Parser(reqObj.query).parseDocument();
    } catch (err) {
      results.push({ data: null, errors: [{ message: `GraphQL syntax error: ${(err as Error).message}` }] });
      continue;
    }
    const fragments = new Map<string, FragmentNode>();
    const operations: OperationNode[] = [];
    for (const def of defs) {
      if (def.kind === 'fragment') fragments.set(def.name, def);
      else operations.push(def);
    }
    const opName = typeof reqObj.operationName === 'string' ? reqObj.operationName : null;
    const op = opName === null ? operations[0] : operations.find((o) => o.name === opName);
    if (op === undefined) {
      results.push({ data: null, errors: [{ message: opName === null ? 'No operation provided' : `Unknown operation named "${opName}"` }] });
      continue;
    }
    const variables = (reqObj.variables !== null && typeof reqObj.variables === 'object' ? reqObj.variables : {}) as Record<string, unknown>;
    // Operation variable defaults for variables the caller did not supply.
    for (const v of op.variables) {
      if (variables[v.name] === undefined && v.defaultValue !== null) {
        variables[v.name] = resolveValue(v.defaultValue, { forge, variables, fragments, errors: [] });
      }
    }
    results.push(executeOperation(forge, op, fragments, variables));
  }
  return jsonResult(200, Array.isArray(body) ? results : results[0]);
}
