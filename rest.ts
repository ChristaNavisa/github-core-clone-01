// REST v3: repos, refs, branches, commits, contents, labels, issues, comments,
// pull requests. GitHub-shaped JSON; every reply carries X-GitHub-Request-Id
// (added by app.ts) and an error JSON body carries `message`.

import {
  FIXED_ISO,
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  globalId,
  isOid,
  listCommits,
  parseCommit,
  parseTag,
  readFileAt,
  listDirAt,
  type Forge,
  type Issue,
  type Pull,
  type Repo,
  type TreeEntry,
} from './store.ts';
import { emptyResult, errorResult, jsonResult, type HandlerResult } from './web.ts';

export interface RestRequest {
  method: string;
  segments: string[];
  query: URLSearchParams;
  headers: Record<string, string | undefined>;
  json: unknown | null;
  host: string;
}

function bodyOf<T>(req: RestRequest): T {
  return (req.json ?? {}) as T;
}

function viewerJson(): Record<string, unknown> {
  return {
    login: VIEWER_LOGIN,
    id: VIEWER_ID,
    node_id: globalId('User', VIEWER_LOGIN),
    avatar_url: `https://avatars.githubusercontent.com/u/${VIEWER_ID}`,
    gravatar_id: '',
    url: `/api/v3/users/${VIEWER_LOGIN}`,
    html_url: `https://github.com/${VIEWER_LOGIN}`,
    type: 'User',
    site_admin: false,
    name: VIEWER_NAME,
    company: null,
    blog: '',
    location: null,
    email: VIEWER_EMAIL,
    hireable: null,
    bio: null,
    public_repos: 0,
    public_gists: 0,
    followers: 0,
    following: 0,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
  };
}

function repoJson(forge: Forge, repo: Repo): Record<string, unknown> {
  const owner = VIEWER_LOGIN;
  return {
    id: repo.id,
    node_id: globalId('Repository', owner, repo.name),
    name: repo.name,
    full_name: `${owner}/${repo.name}`,
    owner: { login: owner, id: VIEWER_ID, node_id: globalId('User', owner), type: 'User', site_admin: false },
    private: repo.isPrivate,
    html_url: `https://github.com/${owner}/${repo.name}`,
    description: repo.description,
    fork: false,
    url: `/api/v3/repos/${owner}/${repo.name}`,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    pushed_at: FIXED_ISO,
    git_url: `git://github.com/${owner}/${repo.name}.git`,
    ssh_url: `git@github.com:${owner}/${repo.name}.git`,
    clone_url: `https://github.com/${owner}/${repo.name}.git`,
    homepage: null,
    size: 0,
    stargazers_count: 0,
    watchers_count: 0,
    language: null,
    has_issues: true,
    has_projects: false,
    has_downloads: true,
    has_wiki: false,
    has_pages: false,
    has_discussions: false,
    forks_count: 0,
    mirror_url: null,
    archived: false,
    disabled: false,
    open_issues_count: repo.issues.filter((i) => i.state === 'open').length,
    license: null,
    allow_forking: true,
    is_template: false,
    topics: [],
    visibility: repo.isPrivate ? 'private' : 'public',
    forks: 0,
    open_issues: repo.issues.filter((i) => i.state === 'open').length,
    watchers: 0,
    default_branch: repo.defaultBranch,
  };
}

function labelJson(label: { name: string; color: string }, id: number): Record<string, unknown> {
  return {
    id,
    node_id: globalId('Label', label.name),
    url: `/api/v3/labels/${encodeURIComponent(label.name)}`,
    name: label.name,
    color: label.color,
    default: false,
    description: '',
  };
}

/** A label's stable id: its 1-based position in the repo's label insertion order. */
function labelId(repo: Repo, name: string): number {
  return [...repo.labels.keys()].indexOf(name) + 1;
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: globalId('Issue', VIEWER_LOGIN, repo.name, String(issue.number)),
    url: `/api/v3/repos/${VIEWER_LOGIN}/${repo.name}/issues/${issue.number}`,
    repository_url: `/api/v3/repos/${VIEWER_LOGIN}/${repo.name}`,
    html_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/issues/${issue.number}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    locked: false,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: globalId('User', VIEWER_LOGIN), type: 'User', site_admin: false },
    labels: issue.labels.map((name) => {
      const found = repo.labels.get(name);
      return found === undefined ? { name, color: '000000', description: '' } : labelJson(found, labelId(repo, name));
    }),
    assignees: [],
    comments: issue.comments.length,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    closed_at: issue.state === 'closed' ? FIXED_ISO : null,
    author_association: 'OWNER',
    state_reason: null,
  };
}

function pullJson(forge: Forge, repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: globalId('PullRequest', VIEWER_LOGIN, repo.name, String(pull.number)),
    url: `/api/v3/repos/${VIEWER_LOGIN}/${repo.name}/pulls/${pull.number}`,
    html_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/pulls/${pull.number}`,
    diff_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/pull/${pull.number}.diff`,
    patch_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/pull/${pull.number}.patch`,
    issue_url: `/api/v3/repos/${VIEWER_LOGIN}/${repo.name}/issues/${pull.number}`,
    number: pull.number,
    state: pull.state,
    locked: false,
    title: pull.title,
    body: pull.body,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: globalId('User', VIEWER_LOGIN), type: 'User', site_admin: false },
    labels: [],
    head: { label: `${VIEWER_LOGIN}:${pull.head}`, ref: pull.head, sha: pull.headSha, repo: repoJsonSimple(repo) },
    base: { label: `${VIEWER_LOGIN}:${pull.base}`, ref: pull.base, sha: forge.baseTip(repo, pull), repo: repoJsonSimple(repo) },
    merged: pull.merged,
    mergeable: forge.pullMergeable(repo, pull),
    merge_commit_sha: pull.mergeCommitSha,
    merged_at: pull.mergedAt,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    closed_at: pull.state === 'open' ? null : FIXED_ISO,
    author_association: 'OWNER',
    draft: false,
  };
}

function repoJsonSimple(repo: Repo): Record<string, unknown> {
  return { id: repo.id, node_id: globalId('Repository', VIEWER_LOGIN, repo.name), name: repo.name, full_name: `${VIEWER_LOGIN}/${repo.name}`, private: repo.isPrivate, default_branch: repo.defaultBranch };
}

function ownerRepo(forge: Forge, owner: string, name: string): Repo | null {
  if (owner !== VIEWER_LOGIN) return null;
  return forge.getRepo(owner, name) ?? null;
}

/** Resolve a branch/tag name or an oid to a commit tip oid (following annotated tags), or null. */
function resolveRefTip(repo: Repo, ref: string): string | null {
  let oid: string | undefined;
  if (isOid(ref)) oid = ref;
  else oid = repo.refs.get(`refs/heads/${ref}`) ?? repo.refs.get(`refs/tags/${ref}`) ?? repo.refs.get(ref);
  if (oid === undefined) return null;
  for (let i = 0; i < 8; i += 1) {
    const obj = repo.objects.get(oid);
    if (obj === undefined) return null;
    if (obj.type === 'tag') {
      oid = parseTag(obj.data).object;
      continue;
    }
    return obj.type === 'commit' ? oid : null;
  }
  return null;
}

/** The root tree oid of a ref's tip commit, or null when the ref has no commit. */
function resolveRefTree(repo: Repo, ref: string): string | null {
  const tip = resolveRefTip(repo, ref);
  if (tip === null) return null;
  const obj = repo.objects.get(tip);
  if (obj === undefined || obj.type !== 'commit') return null;
  return parseCommit(obj.data).tree;
}

function paginate<T>(items: readonly T[], req: RestRequest): { page: T[]; next: string | null; last: string | null } {
  const perPageRaw = Number.parseInt(req.query.get('per_page') ?? '30', 10);
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(perPageRaw, 100) : 30;
  const pageRaw = Number.parseInt(req.query.get('page') ?? '1', 10);
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? pageRaw : 1;
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  const origin = `http://${req.host}`;
  const base = `${origin}/api/v3${req.segments.map((s) => `/${encodeURIComponent(s)}`).join('')}`;
  const link = (p: number): string => `${base}?per_page=${perPage}&page=${p}`;
  return {
    page: [...slice],
    next: page < totalPages ? link(page + 1) : null,
    last: totalPages > 1 ? link(totalPages) : null,
  };
}

function withLink<T>(result: { page: T[]; next: string | null; last: string | null }): HandlerResult {
  const headers: Record<string, string> = {};
  const links: string[] = [];
  if (result.next !== null) links.push(`<${result.next}>; rel="next"`);
  if (result.last !== null) links.push(`<${result.last}>; rel="last"`);
  if (links.length > 0) headers.Link = links.join(', ');
  return jsonResult(200, result.page, headers);
}

export function handleRest(forge: Forge, req: RestRequest): HandlerResult {
  const s = req.segments;

  // GET /api/v3/
  if (s.length === 0) {
    if (req.method !== 'GET') return jsonResult(404, { message: 'Not Found' });
    return jsonResult(200, {
      current_user_url: '/api/v3/user',
      current_user_authorizations_html_url: 'https://github.com/settings/applications',
      authorizations_url: '/api/v3/authorizations',
      code_search_url: '/api/v3/search/code?q={query}{&page,per_page,sort,order}',
      commit_search_url: '/api/v3/search/commits?q={query}{&page,per_page,sort,order}',
      emails_url: '/api/v3/user/emails',
      emojis_url: '/api/v3/emojis',
      events_url: '/api/v3/events',
      feeds_url: '/api/v3/feeds',
      followers_url: '/api/v3/user/followers',
      following_url: '/api/v3/user/following{/target}',
      gists_url: '/api/v3/gists{/gist_id}',
      hub_url: 'https://api.github.com/hub',
      issue_search_url: '/api/v3/search/issues?q={query}{&page,per_page,sort,order}',
      issues_url: '/api/v3/issues',
      keys_url: '/api/v3/user/keys',
      label_search_url: '/api/v3/search/labels?q={query}{&repository_id,page,per_page}',
      notifications_url: '/api/v3/notifications',
      organization_url: '/api/v3/orgs/{org}',
      organization_repositories_url: '/api/v3/orgs/{org}/repos{?type,page,per_page,sort}',
      organization_teams_url: '/api/v3/orgs/{org}/teams',
      public_gists_url: '/api/v3/gists/public',
      rate_limit_url: '/api/v3/rate_limit',
      repository_url: '/api/v3/repos/{owner}/{repo}',
      repository_search_url: '/api/v3/search/repositories?q={query}{&page,per_page,sort,order}',
      current_user_repositories_url: '/api/v3/user/repos{?type,page,per_page,sort}',
      starred_url: '/api/v3/user/starred{/owner}{/repo}',
      starred_gists_url: '/api/v3/gists/starred',
      topic_search_url: '/api/v3/search/topics?q={query}{&page,per_page}',
      user_url: '/api/v3/users/{user}',
      user_organizations_url: '/api/v3/users/{user}/orgs',
      user_repositories_url: '/api/v3/users/{user}/repos{?type,page,per_page,sort}',
      user_search_url: '/api/v3/search/users?q={query}{&page,per_page,sort,order}',
    }, { 'X-OAuth-Scopes': 'repo, read:org' });
  }

  if (s.length === 1 && s[0] === 'meta') {
    if (req.method !== 'GET') return jsonResult(404, { message: 'Not Found' });
    return jsonResult(200, { verifiable_password_authentication: true, installed_version: '1.0.0', git_sha: '0000000000000000000000000000000000000000' });
  }

  if (s.length === 1 && s[0] === 'user') {
    if (req.method !== 'GET') return jsonResult(404, { message: 'Not Found' });
    return jsonResult(200, viewerJson());
  }

  // POST /user/repos and GET /user/repos
  if (s.length === 2 && s[0] === 'user' && s[1] === 'repos') {
    if (req.method === 'POST') {
      const body = bodyOf<{ name?: unknown; description?: unknown; private?: unknown }>(req);
      if (typeof body.name !== 'string' || body.name.trim() === '') {
        return errorResult(422, 'Validation Failed: name is required');
      }
      const name = body.name.trim();
      if (forge.getRepo(VIEWER_LOGIN, name) !== undefined) {
        return errorResult(422, `Validation Failed: name already exists on this account`);
      }
      const repo = forge.createRepo(VIEWER_LOGIN, name, typeof body.description === 'string' ? body.description : '', body.private === true);
      return jsonResult(201, repoJson(forge, repo));
    }
    if (req.method === 'GET') {
      return withLink(paginate(forge.listViewerRepos().map((r) => repoJson(forge, r)), req));
    }
    return jsonResult(404, { message: 'Not Found' });
  }

  if (s.length >= 2 && s[0] === 'repos') {
    const owner = s[1] as string;
    const name = s[2] as string;
    if (name === undefined) return jsonResult(404, { message: 'Not Found' });
    const repo = ownerRepo(forge, owner, name);
    const tail = s.slice(3);

    // GET/PATCH/DELETE /repos/{o}/{r}
    if (tail.length === 0) {
      if (repo === null) return jsonResult(404, { message: 'Not Found' });
      if (req.method === 'GET') return jsonResult(200, repoJson(forge, repo));
      if (req.method === 'PATCH') {
        const body = bodyOf<{ name?: unknown; description?: unknown; private?: unknown }>(req);
        if (typeof body.description === 'string') repo.description = body.description;
        if (typeof body.private === 'boolean') repo.isPrivate = body.private;
        if (typeof body.name === 'string' && body.name.trim() !== '' && body.name !== repo.name) {
          if (forge.getRepo(owner, body.name.trim()) !== undefined) return errorResult(422, 'Validation Failed: name already exists on this account');
          forge.renameRepo(repo, body.name.trim());
        }
        return jsonResult(200, repoJson(forge, repo));
      }
      if (req.method === 'DELETE') {
        forge.deleteRepo(owner, name);
        return emptyResult(204);
      }
      return jsonResult(404, { message: 'Not Found' });
    }
    if (repo === null) return jsonResult(404, { message: 'Not Found' });

    // git refs: POST /repos/{o}/{r}/git/refs
    if (tail.length === 2 && tail[0] === 'git' && tail[1] === 'refs' && req.method === 'POST') {
      const body = bodyOf<{ ref?: unknown; sha?: unknown }>(req);
      if (typeof body.ref !== 'string' || typeof body.sha !== 'string' || !body.ref.startsWith('refs/') || !isOid(body.sha)) {
        return errorResult(422, 'Validation Failed: ref and sha are required');
      }
      if (repo.refs.has(body.ref)) return errorResult(422, 'Validation Failed: Reference already exists');
      if (!repo.objects.has(body.sha)) return errorResult(422, 'Validation Failed: Invalid SHA');
      repo.refs.set(body.ref, body.sha);
      return jsonResult(201, { ref: body.ref, node_id: globalId('Ref', owner, name, body.ref), url: `/api/v3/repos/${owner}/${name}/git/refs/${encodeURIComponent(body.ref)}`, object: { sha: body.sha, type: repo.objects.get(body.sha)?.type ?? 'commit', url: `/api/v3/repos/${owner}/${name}/git/${repo.objects.get(body.sha)?.type ?? 'commit'}s/${body.sha}` } });
    }

    // GET /repos/{o}/{r}/git/refs/heads/{branch}
    if (tail.length === 4 && tail[0] === 'git' && tail[1] === 'refs' && tail[2] === 'heads' && req.method === 'GET') {
      const ref = `refs/heads/${decodeURIComponent(tail[3] as string)}`;
      const sha = repo.refs.get(ref);
      if (sha === undefined) return jsonResult(404, { message: 'Not Found' });
      const type = repo.objects.get(sha)?.type ?? 'commit';
      return jsonResult(200, { ref, node_id: globalId('Ref', owner, name, ref), url: `/api/v3/repos/${owner}/${name}/git/refs/heads/${tail[4]}`, object: { sha, type, url: `/api/v3/repos/${owner}/${name}/git/${type}s/${sha}` } });
    }

    // branches
    if (tail.length === 1 && tail[0] === 'branches' && req.method === 'GET') {
      const branches = [...repo.refs.entries()]
        .filter(([ref]) => ref.startsWith('refs/heads/'))
        .map(([ref, sha]) => ({ name: ref.slice('refs/heads/'.length), commit: { sha, url: `/api/v3/repos/${owner}/${name}/commits/${sha}` }, protected: false }));
      return jsonResult(200, branches);
    }
    if (tail.length === 2 && tail[0] === 'branches' && req.method === 'GET') {
      const ref = `refs/heads/${decodeURIComponent(tail[1] as string)}`;
      const sha = repo.refs.get(ref);
      if (sha === undefined) return jsonResult(404, { message: 'Not Found' });
      return jsonResult(200, { name: tail[1], commit: { sha, url: `/api/v3/repos/${owner}/${name}/commits/${sha}` }, protected: false });
    }

    // commits
    if (tail.length === 1 && tail[0] === 'commits' && req.method === 'GET') {
      const refParam = req.query.get('sha');
      const tip = resolveRefTip(repo, refParam ?? repo.defaultBranch);
      const oids = tip === null ? [] : listCommits(repo.objects, tip);
      const out = oids.map((oid) => {
        const c = parseCommit(repo.objects.get(oid)?.data ?? Buffer.alloc(0));
        return {
          sha: oid,
          node_id: globalId('Commit', owner, name, oid),
          commit: {
            author: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: FIXED_ISO },
            committer: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: FIXED_ISO },
            // Git stores the message with a trailing newline; the API reports
            // it without, as GitHub does.
            message: c.message.replace(/\n$/, ''),
            tree: { sha: c.tree },
            url: `/api/v3/repos/${owner}/${name}/git/commits/${oid}`,
            comment_count: 0,
          },
          url: `/api/v3/repos/${owner}/${name}/commits/${oid}`,
          html_url: `https://github.com/${owner}/${name}/commit/${oid}`,
          author: { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: globalId('User', VIEWER_LOGIN), type: 'User', site_admin: false },
          committer: { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: globalId('User', VIEWER_LOGIN), type: 'User', site_admin: false },
          parents: c.parents.map((p) => ({ sha: p, url: `/api/v3/repos/${owner}/${name}/commits/${p}`, html_url: `https://github.com/${owner}/${name}/commit/${p}` })),
        };
      });
      return jsonResult(200, out);
    }

    // contents
    if (tail[0] === 'contents') {
      const pathParts = tail.slice(1).map((p) => decodeURIComponent(p));
      const path = pathParts.join('/');
      if (req.method === 'GET') {
        const refParam = req.query.get('ref');
        const tree = resolveRefTree(repo, refParam ?? repo.defaultBranch);
        if (tree === null) return jsonResult(404, { message: 'Not Found' });
        if (path === '') {
          const entries = listDirAt(repo.objects, tree, '');
          if (entries === null) return jsonResult(404, { message: 'Not Found' });
          return jsonResult(200, entries.map((e) => contentsEntryJson(owner, name, repo, '', e)));
        }
        const file = readFileAt(repo.objects, tree, path);
        if (file !== null) {
          return jsonResult(200, {
            type: 'file',
            encoding: 'base64',
            size: file.data.length,
            name: path.split('/').pop() as string,
            path,
            content: file.data.toString('base64'),
            sha: file.oid,
            url: `/api/v3/repos/${owner}/${name}/contents/${path}`,
            html_url: `https://github.com/${owner}/${name}/blob/${refParam ?? repo.defaultBranch}/${path}`,
            git_url: `/api/v3/repos/${owner}/${name}/git/blobs/${file.oid}`,
            download_url: `https://raw.githubusercontent.com/${owner}/${name}/${refParam ?? repo.defaultBranch}/${path}`,
            _links: { git: `/api/v3/repos/${owner}/${name}/git/blobs/${file.oid}`, self: `/api/v3/repos/${owner}/${name}/contents/${path}`, html: `https://github.com/${owner}/${name}/blob/${refParam ?? repo.defaultBranch}/${path}` },
          });
        }
        const dirEntries = listDirAt(repo.objects, tree, path);
        if (dirEntries === null || dirEntries.length === 0) return jsonResult(404, { message: 'Not Found' });
        return jsonResult(200, dirEntries.map((e) => contentsEntryJson(owner, name, repo, path, e)));
      }
      if (req.method === 'PUT') {
        const body = bodyOf<{ message?: unknown; content?: unknown; branch?: unknown; sha?: unknown }>(req);
        if (typeof body.content !== 'string') return errorResult(422, 'Validation Failed: content is required');
        let content: Buffer;
        try {
          content = Buffer.from(body.content, 'base64');
        } catch {
          return errorResult(422, 'Validation Failed: content is not valid base64');
        }
        const branch = typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
        const message = typeof body.message === 'string' ? body.message : `Update ${path}`;
        const tree = resolveRefTree(repo, branch);
        const current = tree === null ? null : readFileAt(repo.objects, tree, path);
        if (typeof body.sha === 'string' && current !== null && body.sha !== current.oid) {
          return errorResult(409, 'Conflict: sha does not match the current file');
        }
        const existed = current !== null;
        const { commitSha, blobSha } = forge.commitFile(repo, branch, path, content, message);
        return jsonResult(existed ? 200 : 201, {
          content: {
            type: 'file',
            encoding: 'base64',
            size: content.length,
            name: path.split('/').pop() as string,
            path,
            content: content.toString('base64'),
            sha: blobSha,
            url: `/api/v3/repos/${owner}/${name}/contents/${path}`,
            html_url: `https://github.com/${owner}/${name}/blob/${branch}/${path}`,
            git_url: `/api/v3/repos/${owner}/${name}/git/blobs/${blobSha}`,
            download_url: `https://raw.githubusercontent.com/${owner}/${name}/${branch}/${path}`,
          },
          commit: { sha: commitSha, message },
        });
      }
      if (req.method === 'DELETE') {
        const body = bodyOf<{ message?: unknown; branch?: unknown; sha?: unknown }>(req);
        const branch = typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
        const message = typeof body.message === 'string' ? body.message : `Delete ${path}`;
        const tree = resolveRefTree(repo, branch);
        const current = tree === null ? null : readFileAt(repo.objects, tree, path);
        if (current === null) return jsonResult(404, { message: 'Not Found' });
        if (typeof body.sha === 'string' && body.sha !== current.oid) {
          return errorResult(409, 'Conflict: sha does not match the current file');
        }
        const deleted = forge.deleteFile(repo, branch, path, message);
        if (deleted === null) return jsonResult(404, { message: 'Not Found' });
        return jsonResult(200, { commit: { sha: deleted, message } });
      }
      return jsonResult(404, { message: 'Not Found' });
    }

    // labels
    if (tail.length === 1 && tail[0] === 'labels') {
      if (req.method === 'GET') return jsonResult(200, [...repo.labels.values()].map((l) => labelJson(l, labelId(repo, l.name))));
      if (req.method === 'POST') {
        const body = bodyOf<{ name?: unknown; color?: unknown; description?: unknown }>(req);
        if (typeof body.name !== 'string' || body.name.trim() === '') return errorResult(422, 'Validation Failed: name is required');
        const existing = repo.labels.get(body.name.trim());
        if (existing !== undefined) return jsonResult(200, labelJson(existing, labelId(repo, existing.name)));
        const label = forge.createLabel(repo, body.name.trim(), typeof body.color === 'string' ? body.color.replace(/^#/, '') : '000000');
        return jsonResult(201, labelJson(label, labelId(repo, label.name)));
      }
      return jsonResult(404, { message: 'Not Found' });
    }

    // issues
    if (tail.length === 1 && tail[0] === 'issues') {
      if (req.method === 'GET') {
        const state = req.query.get('state') ?? 'open';
        const sort = req.query.get('sort') ?? 'created';
        const direction = req.query.get('direction') ?? 'desc';
        let items = [...repo.issues];
        if (state === 'open') items = items.filter((i) => i.state === 'open');
        else if (state === 'closed') items = items.filter((i) => i.state === 'closed');
        if (sort === 'created') {
          items.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
        } else if (sort === 'updated') {
          items.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
        }
        return withLink(paginate(items.map((i) => issueJson(repo, i)), req));
      }
      if (req.method === 'POST') {
        const body = bodyOf<{ title?: unknown; body?: unknown; labels?: unknown }>(req);
        if (typeof body.title !== 'string' || body.title.trim() === '') return errorResult(422, 'Validation Failed: title is required');
        const labelNames: string[] = Array.isArray(body.labels)
          ? body.labels.map((l) => (typeof l === 'string' ? l : (l as { name?: unknown }).name)).filter((l): l is string => typeof l === 'string')
          : [];
        const unknown = labelNames.filter((n) => !repo.labels.has(n));
        if (unknown.length > 0) return errorResult(422, `Validation Failed: label "${unknown[0]}" does not exist`);
        const issue = forge.createIssue(repo, body.title.trim(), typeof body.body === 'string' ? body.body : '', labelNames);
        return jsonResult(201, issueJson(repo, issue));
      }
      return jsonResult(404, { message: 'Not Found' });
    }

    // issue by number and its comments
    if (tail.length >= 2 && tail[0] === 'issues') {
      const number = Number.parseInt(tail[1] as string, 10);
      const issue = repo.issues.find((i) => i.number === number);
      if (tail.length === 2) {
        if (issue === undefined) return jsonResult(404, { message: 'Not Found' });
        if (req.method === 'GET') return jsonResult(200, issueJson(repo, issue));
        if (req.method === 'PATCH') {
          const body = bodyOf<{ title?: unknown; body?: unknown; state?: unknown }>(req);
          if (typeof body.title === 'string') issue.title = body.title;
          if (typeof body.body === 'string') issue.body = body.body;
          if (body.state === 'open' || body.state === 'closed') issue.state = body.state;
          return jsonResult(200, issueJson(repo, issue));
        }
        return jsonResult(404, { message: 'Not Found' });
      }
      if (tail.length === 3 && tail[2] === 'comments') {
        if (issue === undefined) return jsonResult(404, { message: 'Not Found' });
        if (req.method === 'GET') {
          return jsonResult(200, issue.comments.map((c) => ({
            id: c.id,
            node_id: globalId('IssueComment', VIEWER_LOGIN, repo.name, String(issue.number), String(c.id)),
            url: `/api/v3/repos/${VIEWER_LOGIN}/${repo.name}/issues/comments/${c.id}`,
            html_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/issues/${issue.number}#issuecomment-${c.id}`,
            body: c.body,
            user: { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: globalId('User', VIEWER_LOGIN), type: 'User', site_admin: false },
            created_at: FIXED_ISO,
            updated_at: FIXED_ISO,
          })));
        }
        if (req.method === 'POST') {
          const body = bodyOf<{ body?: unknown }>(req);
          if (typeof body.body !== 'string' || body.body.trim() === '') return errorResult(422, 'Validation Failed: body is required');
          const comment = forge.addComment(repo, issue, body.body);
          return jsonResult(201, {
            id: comment.id,
            node_id: globalId('IssueComment', VIEWER_LOGIN, repo.name, String(issue.number), String(comment.id)),
            url: `/api/v3/repos/${VIEWER_LOGIN}/${repo.name}/issues/comments/${comment.id}`,
            html_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/issues/${issue.number}#issuecomment-${comment.id}`,
            body: comment.body,
            user: { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: globalId('User', VIEWER_LOGIN), type: 'User', site_admin: false },
            created_at: FIXED_ISO,
            updated_at: FIXED_ISO,
          });
        }
        return jsonResult(404, { message: 'Not Found' });
      }
      return jsonResult(404, { message: 'Not Found' });
    }

    // pulls
    if (tail.length === 1 && tail[0] === 'pulls') {
      if (req.method === 'GET') {
        const state = req.query.get('state') ?? 'open';
        let items = [...repo.pulls];
        if (state === 'open') items = items.filter((p) => p.state === 'open');
        else if (state === 'closed') items = items.filter((p) => p.state === 'closed');
        items.sort((a, b) => b.number - a.number);
        return withLink(paginate(items.map((p) => pullJson(forge, repo, p)), req));
      }
      if (req.method === 'POST') {
        const body = bodyOf<{ title?: unknown; body?: unknown; head?: unknown; base?: unknown }>(req);
        if (typeof body.title !== 'string' || typeof body.head !== 'string' || typeof body.base !== 'string') {
          return errorResult(422, 'Validation Failed: title, head and base are required');
        }
        const headSha = repo.refs.get(`refs/heads/${body.head}`);
        const baseSha = repo.refs.get(`refs/heads/${body.base}`);
        if (headSha === undefined || baseSha === undefined) {
          return errorResult(422, 'Validation Failed: head or base branch does not exist');
        }
        const duplicate = repo.pulls.some((p) => p.state === 'open' && p.head === body.head && p.base === body.base);
        if (duplicate) return errorResult(422, 'Validation Failed: A pull request already exists for these branches');
        const pull = forge.createPull(repo, body.title.trim(), typeof body.body === 'string' ? body.body : '', body.head, body.base, headSha, baseSha);
        return jsonResult(201, pullJson(forge, repo, pull));
      }
      return jsonResult(404, { message: 'Not Found' });
    }

    if (tail.length >= 2 && tail[0] === 'pulls') {
      const number = Number.parseInt(tail[1] as string, 10);
      const pull = repo.pulls.find((p) => p.number === number);
      if (tail.length === 2) {
        if (pull === undefined) return jsonResult(404, { message: 'Not Found' });
        if (req.method === 'GET') return jsonResult(200, pullJson(forge, repo, pull));
        return jsonResult(404, { message: 'Not Found' });
      }
      if (tail.length === 3 && tail[2] === 'files' && req.method === 'GET') {
        if (pull === undefined) return jsonResult(404, { message: 'Not Found' });
        return jsonResult(200, forge.pullFiles(repo, pull).map((f) => ({
          sha: f.newOid ?? f.oldOid,
          filename: f.path,
          status: f.status,
          additions: f.additions,
          deletions: f.deletions,
          changes: f.additions + f.deletions,
          blob_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/blob/${pull.headSha}/${f.path}`,
          raw_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/raw/${pull.headSha}/${f.path}`,
          contents_url: `/api/v3/repos/${VIEWER_LOGIN}/${repo.name}/contents/${f.path}?ref=${pull.headSha}`,
        })));
      }
      if (tail.length === 3 && tail[2] === 'merge' && req.method === 'PUT') {
        if (pull === undefined) return jsonResult(404, { message: 'Not Found' });
        if (pull.merged) return errorResult(405, 'Pull Request is already merged');
        if (pull.state !== 'open') return errorResult(405, 'Pull Request is not open');
        const body = bodyOf<{ merge_method?: unknown }>(req);
        if (typeof body.merge_method === 'string' && body.merge_method !== 'squash') {
          return errorResult(405, 'Only the squash merge method is supported');
        }
        const merged = forge.mergePull(repo, pull);
        if (merged === null) return errorResult(405, 'Merge conflict: the pull request cannot be merged');
        return jsonResult(200, { sha: merged, merged: true, message: `Merge pull request #${pull.number} from ${pull.head}` });
      }
      return jsonResult(404, { message: 'Not Found' });
    }
  }

  return jsonResult(404, { message: 'Not Found' });
}

function contentsEntryJson(owner: string, repoName: string, repo: Repo, parentPath: string, entry: TreeEntry): Record<string, unknown> {
  const path = parentPath === '' ? entry.name : `${parentPath}/${entry.name}`;
  const isDir = entry.mode === '40000';
  let size = 0;
  if (!isDir) {
    const blob = repo.objects.get(entry.oid);
    size = blob?.data.length ?? 0;
  }
  return {
    type: isDir ? 'dir' : 'file',
    size,
    name: entry.name,
    path,
    sha: entry.oid,
    url: `/api/v3/repos/${owner}/${repoName}/contents/${path}`,
    html_url: `https://github.com/${owner}/${repoName}/blob/HEAD/${path}`,
    git_url: `/api/v3/repos/${owner}/${repoName}/git/${isDir ? 'trees' : 'blobs'}/${entry.oid}`,
    download_url: isDir ? null : `https://raw.githubusercontent.com/${owner}/${repoName}/HEAD/${path}`,
  };
}
