// MCP: JSON-RPC 2.0 Streamable HTTP on POST /mcp (plain application/json
// replies). Supports the modern 2026-07-28 discovery handshake and the legacy
// initialize handshake, a static tools/list, and six tools over the same Forge
// store the REST and GraphQL surfaces use.

import {
  FIXED_ISO,
  VIEWER_LOGIN,
  type Forge,
} from './store.ts';
import { emptyResult, jsonResult, type HandlerResult } from './web.ts';

export interface McpRequest {
  method: string;
  segments: string[];
  query: URLSearchParams;
  headers: Record<string, string | undefined>;
  json: unknown | null;
}

const SERVER_NAME = 'github-core-clone';
const SERVER_VERSION = '1.0.0';

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the viewer.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
      additionalProperties: false,
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
      additionalProperties: false,
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
      additionalProperties: false,
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state filter', enum: ['open', 'closed', 'all'] },
      },
      required: ['owner', 'repo'],
      additionalProperties: false,
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
        body: { type: 'string', description: 'Pull request body' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
      additionalProperties: false,
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with the squash method.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'integer', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
      additionalProperties: false,
    },
  },
];

function rpcResult(id: unknown, result: unknown): HandlerResult {
  return jsonResult(200, { jsonrpc: '2.0', id: id ?? null, result });
}

function rpcError(id: unknown, code: number, message: string): HandlerResult {
  return jsonResult(200, { jsonrpc: '2.0', id: id ?? null, error: { code, message } });
}

function toolText(structured: unknown): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: JSON.stringify(structured) }],
    structuredContent: structured,
    isError: false,
  };
}

function toolError(message: string): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: JSON.stringify({ error: message }) }],
    structuredContent: { error: message },
    isError: true,
  };
}

/** Owner/repo from canonical args, or from a `full_name: "owner/repo"` arg. */
function ownerRepoOf(args: Record<string, unknown>): { owner: string; repo: string } | null {
  if (typeof args.full_name === 'string') {
    const [owner, repo] = args.full_name.split('/');
    if (owner !== undefined && repo !== undefined && owner !== '' && repo !== '') return { owner, repo };
  }
  if (typeof args.owner === 'string' && typeof args.repo === 'string') {
    return { owner: args.owner, repo: args.repo };
  }
  return null;
}

export function handleMcp(forge: Forge, req: McpRequest): HandlerResult {
  if (req.method === 'DELETE') return emptyResult(204);
  if (req.method !== 'POST') return jsonResult(404, { message: 'Not Found' });

  const body = req.json as { jsonrpc?: unknown; id?: unknown; method?: unknown; params?: unknown } | null;
  if (body === null || typeof body !== 'object') {
    return jsonResult(400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error: body is not a JSON-RPC object' } });
  }
  const id = 'id' in body ? body.id : undefined;
  const method = body.method;
  // A notification has no id: acknowledge and answer nothing.
  if (id === undefined) return emptyResult(202);
  if (typeof method !== 'string') {
    return rpcError(id, -32600, 'Invalid Request: method is required');
  }
  const params = (body.params !== null && typeof body.params === 'object' ? body.params : {}) as Record<string, unknown>;

  if (method === 'server/discover') {
    const serverInfo = { name: SERVER_NAME, version: SERVER_VERSION };
    return rpcResult(id, {
      supportedVersions: ['2026-07-28', '2025-11-25', '2025-06-18'],
      capabilities: { tools: {} },
      serverInfo,
      _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo },
    });
  }
  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '2025-11-25';
    return rpcResult(id, {
      protocolVersion: requested,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    });
  }
  if (method === 'ping') return rpcResult(id, {});
  if (method === 'tools/list') {
    return rpcResult(id, { tools: TOOLS });
  }
  if (method === 'tools/call') {
    const name = params.name;
    const args = (params.arguments !== null && typeof params.arguments === 'object' ? params.arguments : {}) as Record<string, unknown>;
    if (typeof name !== 'string' || name === '') {
      return rpcError(id, -32602, 'Invalid params: tools/call requires a tool name');
    }
    const tool = TOOLS.find((t) => {
      const lower = name.toLowerCase();
      if (lower === t.name) return true;
      const suffix = lower.split(/[/.:]/).pop() ?? '';
      return suffix === t.name;
    });
    if (tool === undefined) {
      return rpcError(id, -32601, `Method not found: no tool named "${name}"`);
    }
    const required = (tool.inputSchema.required ?? []) as string[];
    for (const prop of required) {
      if (args[prop] === undefined || args[prop] === null || args[prop] === '') {
        return rpcError(id, -32602, `Invalid params: missing required argument "${prop}" for tool "${tool.name}"`);
      }
    }
    try {
      const result = callTool(forge, tool.name, args);
      return rpcResult(id, result);
    } catch (err) {
      return rpcResult(id, toolError((err as Error).message));
    }
  }
  return rpcError(id, -32601, `Method not found: ${method}`);
}

function callTool(forge: Forge, name: string, args: Record<string, unknown>): Record<string, unknown> {
  if (name === 'create_repo') {
    const repoName = String(args.name ?? '').trim();
    if (repoName === '') throw new Error('create_repo: name is required');
    if (forge.getRepo(VIEWER_LOGIN, repoName) !== undefined) throw new Error(`create_repo: repository "${repoName}" already exists`);
    const repo = forge.createRepo(VIEWER_LOGIN, repoName, typeof args.description === 'string' ? args.description : '', args.private === true);
    const out = {
      id: repo.id,
      name: repo.name,
      full_name: `${VIEWER_LOGIN}/${repo.name}`,
      owner: { login: VIEWER_LOGIN },
      private: repo.isPrivate,
      description: repo.description,
      default_branch: repo.defaultBranch,
      created_at: FIXED_ISO,
      updated_at: FIXED_ISO,
    };
    return toolText(out);
  }
  if (name === 'get_repo') {
    const or = ownerRepoOf(args);
    if (or === null) throw new Error('get_repo: owner and repo are required');
    const repo = forge.getRepo(or.owner, or.repo);
    if (repo === undefined) throw new Error(`get_repo: repository "${or.owner}/${or.repo}" not found`);
    return toolText({
      id: repo.id,
      name: repo.name,
      full_name: `${or.owner}/${repo.name}`,
      owner: { login: or.owner },
      private: repo.isPrivate,
      description: repo.description,
      default_branch: repo.defaultBranch,
      created_at: FIXED_ISO,
      updated_at: FIXED_ISO,
    });
  }
  if (name === 'create_issue') {
    const or = ownerRepoOf(args);
    if (or === null) throw new Error('create_issue: owner and repo are required');
    const repo = forge.getRepo(or.owner, or.repo);
    if (repo === undefined) throw new Error(`create_issue: repository "${or.owner}/${or.repo}" not found`);
    const issue = forge.createIssue(repo, String(args.title ?? ''), typeof args.body === 'string' ? args.body : '', []);
    return toolText({
      id: issue.id,
      number: issue.number,
      title: issue.title,
      body: issue.body,
      state: issue.state,
      labels: issue.labels,
      repository: `${or.owner}/${or.repo}`,
      created_at: FIXED_ISO,
      updated_at: FIXED_ISO,
    });
  }
  if (name === 'list_issues') {
    const or = ownerRepoOf(args);
    if (or === null) throw new Error('list_issues: owner and repo are required');
    const repo = forge.getRepo(or.owner, or.repo);
    if (repo === undefined) throw new Error(`list_issues: repository "${or.owner}/${or.repo}" not found`);
    const state = typeof args.state === 'string' ? args.state : 'open';
    const items = repo.issues.filter((i) => (state === 'all' ? true : i.state === state));
    return toolText(items.map((i) => ({
      id: i.id,
      number: i.number,
      title: i.title,
      body: i.body,
      state: i.state,
      labels: i.labels,
      created_at: FIXED_ISO,
      updated_at: FIXED_ISO,
    })));
  }
  if (name === 'create_pull_request') {
    const or = ownerRepoOf(args);
    if (or === null) throw new Error('create_pull_request: owner and repo are required');
    const repo = forge.getRepo(or.owner, or.repo);
    if (repo === undefined) throw new Error(`create_pull_request: repository "${or.owner}/${or.repo}" not found`);
    const headName = String(args.head ?? '');
    const baseName = String(args.base ?? '');
    const headSha = repo.refs.get(`refs/heads/${headName}`);
    const baseSha = repo.refs.get(`refs/heads/${baseName}`);
    if (headSha === undefined || baseSha === undefined) throw new Error('create_pull_request: head or base branch does not exist');
    const duplicate = repo.pulls.some((p) => p.state === 'open' && p.head === headName && p.base === baseName);
    if (duplicate) throw new Error('create_pull_request: a pull request already exists for these branches');
    const pull = forge.createPull(repo, String(args.title ?? ''), typeof args.body === 'string' ? args.body : '', headName, baseName, headSha, baseSha);
    return toolText({
      id: pull.id,
      number: pull.number,
      title: pull.title,
      body: pull.body,
      state: pull.state,
      head: { ref: pull.head, sha: pull.headSha },
      base: { ref: pull.base, sha: forge.baseTip(repo, pull) },
      merged: pull.merged,
      created_at: FIXED_ISO,
      updated_at: FIXED_ISO,
    });
  }
  if (name === 'merge_pull_request') {
    const or = ownerRepoOf(args);
    if (or === null) throw new Error('merge_pull_request: owner and repo are required');
    const repo = forge.getRepo(or.owner, or.repo);
    if (repo === undefined) throw new Error(`merge_pull_request: repository "${or.owner}/${or.repo}" not found`);
    const number = typeof args.pull_number === 'number' ? args.pull_number : Number.parseInt(String(args.pull_number ?? ''), 10);
    const pull = repo.pulls.find((p) => p.number === number);
    if (pull === undefined) throw new Error(`merge_pull_request: pull request #${number} not found`);
    if (pull.merged) throw new Error('merge_pull_request: pull request is already merged');
    if (pull.state !== 'open') throw new Error('merge_pull_request: pull request is not open');
    const merged = forge.mergePull(repo, pull);
    if (merged === null) throw new Error('merge_pull_request: merge conflict');
    return toolText({ sha: merged, merged: true, number: pull.number, state: 'MERGED', merge_commit_sha: merged });
  }
  throw new Error(`unknown tool "${name}"`);
}