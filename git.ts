// git smart HTTP: info/refs advertisements (protocol v2 and v0), ls-refs,
// upload-pack fetch (v2 and v0) and receive-pack push, all over pkt-lines.
//
// The wire is exactly what the suite's git-client.ts speaks; see its header for
// the framing. No git binary is involved anywhere.

import {
  VIEWER_LOGIN,
  ZERO_OID,
  hashObject,
  isAncestor,
  isOid,
  parseCommit,
  parseTag,
  parseTree,
  readPack,
  writePack,
  type Forge,
  type GitObject,
  type Repo,
} from './store.ts';
import {
  PKT_DELIM,
  PKT_FLUSH,
  decodePktLines,
  errorResult,
  jsonResult,
  pktLine,
  pktLinesText,
  sidebandChunks,
  type HandlerResult,
} from './web.ts';

export interface GitRequest {
  method: string;
  segments: string[];
  query: URLSearchParams;
  headers: Record<string, string | undefined>;
  body: Buffer;
}

function sortedRefNames(repo: Repo): string[] {
  return [...repo.refs.keys()].sort();
}

function tagPeel(repo: Repo, oid: string): string | null {
  const obj = repo.objects.get(oid);
  if (obj === undefined || obj.type !== 'tag') return null;
  return parseTag(obj.data).object;
}

function headTip(repo: Repo): string {
  return repo.refs.get('refs/heads/main') ?? repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? ZERO_OID;
}

const UPLOAD_CAPS_V0 = 'symref=HEAD:refs/heads/main side-band-64k thin-pack ofs-delta shallow multi_ack';
const RECEIVE_CAPS_V0 = 'symref=HEAD:refs/heads/main report-status side-band-64k delete-refs ofs-delta';

/** One pkt-line as a string: 4-hex length prefix + payload. Byte-exact with pktLine for ASCII payloads; byteLength keeps it exact for any ref name. */
function pktLineStr(payload: string): string {
  return (Buffer.byteLength(payload, 'utf8') + 4).toString(16).padStart(4, '0') + payload;
}

/**
 * A v0 ref advertisement: preamble, then one line per ref (caps on the first).
 * Built as one string and encoded once (instead of a Buffer.concat per line).
 * Refs change on every push, but clones ask for the same advertisement dozens
 * of times with identical refs; cache on the exact ref state.
 */
function advertisementV0(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps = service === 'git-upload-pack' ? UPLOAD_CAPS_V0 : RECEIVE_CAPS_V0;
  const refState = [...repo.refs.entries()].map(([n, o]) => `${n}=${o}`).join(',');
  const key = `${service}|${refState}`;
  const cached = repo.adV0Cache.get(key);
  if (cached !== undefined) return cached;
  let text = pktLineStr(`# service=${service}\n`) + '0000';
  const names = sortedRefNames(repo);
  if (names.length === 0) {
    text += pktLineStr(`${ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    text += pktLineStr(`${headTip(repo)} HEAD\0${caps}\n`);
    for (const name of names) {
      const oid = repo.refs.get(name) as string;
      text += pktLineStr(`${oid} ${name}\n`);
      const peeled = tagPeel(repo, oid);
      if (peeled !== null) text += pktLineStr(`${peeled} ${name}^{}\n`);
    }
  }
  text += '0000';
  const body = Buffer.from(text, 'utf8');
  if (repo.adV0Cache.size >= 8) repo.adV0Cache.clear();
  repo.adV0Cache.set(key, body);
  return body;
}

/** A v2 capability advertisement: preamble, then `version 2` and the capabilities. */
function advertisementV2(service: 'git-upload-pack'): Buffer {
  return Buffer.concat([
    pktLine(`# service=${service}\n`),
    PKT_FLUSH,
    pktLine('version 2\n'),
    pktLine('ls-refs=unborn\n'),
    pktLine('fetch=shallow\n'),
    pktLine('object-format=sha1\n'),
    PKT_FLUSH,
  ]);
}

/** The v2 capability advertisement never changes; build it once. */
const ADVERTISEMENT_V2_UPLOAD_PACK = advertisementV2('git-upload-pack');

/** The pack for zero objects; built once (used for empty fetch responses). */
const EMPTY_PACK = writePack([]);

/**
 * Pack bytes for a fetch plan, cached on the exact object list. Objects are
 * immutable, so a pack built for one object set is valid forever; the clone
 * workload fetches the same set dozens of times. Bounded FIFO.
 */
function cachedPack(repo: Repo, objects: Map<string, GitObject>): Buffer {
  let key = '';
  for (const oid of objects.keys()) key += `${oid},`;
  const hit = repo.packCache.get(key);
  if (hit !== undefined) return hit;
  const pack = writePack(objects.values());
  if (repo.packCache.size >= 32) repo.packCache.clear();
  repo.packCache.set(key, pack);
  return pack;
}

function findRepo(forge: Forge, owner: string, repoName: string): Repo | null {
  // The viewer owns every repo in this forge; any other owner is unknown.
  if (owner !== VIEWER_LOGIN) return null;
  return forge.getRepo(owner, repoName) ?? null;
}

// ---------------------------------------------------------------------------------------------
// fetch planning
// ---------------------------------------------------------------------------------------------

interface FetchPlan {
  objects: Map<string, GitObject>;
  shallow: string[];
  unshallow: string[];
}

/** The objects a client holding `haves` (with `shallow` boundaries) already has. */
function heldClosure(repo: Repo, haves: readonly string[], shallowSet: Set<string>): Set<string> {
  const seen = new Set<string>();
  const stack = [...haves];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj === undefined) continue;
    seen.add(oid);
    if (obj.type === 'commit') {
      const c = parseCommit(obj.data);
      stack.push(c.tree);
      if (!shallowSet.has(oid)) for (const p of c.parents) stack.push(p);
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    } else if (obj.type === 'tree') {
      for (const e of parseTree(obj.data)) if (e.mode !== '160000') stack.push(e.oid);
    }
  }
  return seen;
}

function addTreeClosure(repo: Repo, rootOid: string, out: Set<string>): void {
  const stack = [rootOid];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (out.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj === undefined) continue;
    out.add(oid);
    if (obj.type === 'tree') for (const e of parseTree(obj.data)) stack.push(e.oid);
  }
}

/**
 * The effective depth for one want. `deepen N` on its own is an absolute
 * total depth; with `deepen-relative` it is N more generations below the
 * client's current shallow boundary (the distance from the want to the
 * nearest boundary it sent, plus one for the boundary itself, plus N).
 */
function effectiveDepth(repo: Repo, want: string, shallowBoundaries: readonly string[], depth: number, relative: boolean): number {
  if (!relative) return depth;
  const targets = new Set(shallowBoundaries);
  if (targets.size === 0) return depth;
  let distance = 0;
  let frontier = [want];
  const seen = new Set<string>([want]);
  while (frontier.length > 0) {
    if (frontier.some((oid) => targets.has(oid))) return distance + 1 + depth;
    const next: string[] = [];
    for (const oid of frontier) {
      const obj = repo.objects.get(oid);
      if (obj === undefined || obj.type !== 'commit') continue;
      for (const p of parseCommit(obj.data).parents) {
        if (!seen.has(p)) {
          seen.add(p);
          next.push(p);
        }
      }
    }
    frontier = next;
    distance += 1;
  }
  return depth;
}
/**
 * The objects a fetch sends: the closure of `wants` (depth-limited when
 * `depth` is set), minus what the client already holds. Also the new shallow
 * boundary and the boundaries the fetch unshallows. Each want has its own
 * effective depth (see effectiveDepth) because a relative deepen depends on
 * how far that want is from the client's shallow boundary.
 */
function planFetch(repo: Repo, wants: readonly string[], haves: readonly string[], shallowBoundaries: readonly string[], depth: number | null, relative = false): FetchPlan {
  const shallowSet = new Set(shallowBoundaries);
  const held = heldClosure(repo, haves, shallowSet);

  const included = new Set<string>();
  const boundary: string[] = [];
  const queue: Array<{ oid: string; dist: number; limit: number | null }> = wants
    .filter((w) => repo.objects.has(w))
    .map((w) => ({ oid: w, dist: 0, limit: depth === null ? null : effectiveDepth(repo, w, shallowBoundaries, depth, relative) }));
  // The largest limit under which each commit was already expanded: a commit
  // reached again under a larger limit is expanded again.
  const seenLimits = new Map<string, number | null>();
  while (queue.length > 0) {
    const { oid, dist, limit } = queue.shift() as { oid: string; dist: number; limit: number | null };
    const seen = seenLimits.get(oid);
    // Skip only when this commit was already expanded with an equal or larger
    // limit (null means unlimited, which covers everything).
    if (seen !== undefined && (seen === null || (limit !== null && limit <= seen))) continue;
    seenLimits.set(oid, limit);
    const obj = repo.objects.get(oid);
    if (obj === undefined) continue;
    if (obj.type === 'tag') {
      included.add(oid);
      const target = parseTag(obj.data).object;
      queue.push({ oid: target, dist, limit });
      continue;
    }
    if (obj.type !== 'commit') {
      included.add(oid);
      continue;
    }
    included.add(oid);
    const c = parseCommit(obj.data);
    addTreeClosure(repo, c.tree, included);
    if (limit === null || dist + 1 < limit) {
      for (const p of c.parents) queue.push({ oid: p, dist: dist + 1, limit });
    } else if (c.parents.length > 0) {
      boundary.push(oid);
    }
  }

  const objects = new Map<string, GitObject>();
  for (const oid of [...included].sort()) {
    if (held.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj !== undefined) objects.set(oid, obj);
  }

  const unshallow: string[] = [];
  for (const s of shallowBoundaries) {
    if (!included.has(s) && !held.has(s)) continue;
    const obj = repo.objects.get(s);
    if (obj === undefined || obj.type !== 'commit') continue;
    const parents = parseCommit(obj.data).parents;
    if (parents.every((p) => included.has(p) || held.has(p))) unshallow.push(s);
  }
  return { objects, shallow: depth === null ? [] : boundary, unshallow };
}

// ---------------------------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------------------------

function fetchResponseV2(plan: FetchPlan, haves: readonly string[], repo: Repo): Buffer {
  const parts: Buffer[] = [];
  // The acknowledgments section exists only when the client sent haves: with
  // no haves the response starts at the packfile section (real git and the
  // suite's parser both expect that).
  if (haves.length > 0) {
    parts.push(pktLine('acknowledgments\n'));
    let acked = false;
    for (const have of haves) {
      if (repo.objects.has(have)) {
        parts.push(pktLine(`ACK ${have}\n`));
        acked = true;
      }
    }
    if (!acked) parts.push(pktLine('NAK\n'));
    // The server ACKs every have it knows, so it has complete information
    // about what the client holds: answer `ready` so the client takes the
    // pack instead of expecting another negotiation round. Real git's client
    // does not send `done` in the fetch request, so `ready` cannot depend on
    // it; the suite's client sends `done` and also reads `ready`.
    parts.push(pktLine('ready\n'));
    parts.push(PKT_DELIM);
  }
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(PKT_DELIM);
  }
  parts.push(pktLine('packfile\n'));
  const pack = cachedPack(repo, plan.objects);
  for (const chunk of sidebandChunks(pack)) parts.push(chunk);
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

function fetchResponseV0(plan: FetchPlan, haves: readonly string[], repo: Repo): Buffer {
  const parts: Buffer[] = [];
  let acked = false;
  for (const have of haves) {
    if (repo.objects.has(have)) {
      parts.push(pktLine(`ACK ${have}\n`));
      acked = true;
    }
  }
  if (!acked) parts.push(pktLine('NAK\n'));
  for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
  const pack = cachedPack(repo, plan.objects);
  for (const chunk of sidebandChunks(pack)) parts.push(chunk);
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

function handleUploadPack(repo: Repo, ctx: GitRequest): HandlerResult {
  const isV2 = (ctx.headers['git-protocol'] ?? '') === 'version=2';
  let packets;
  try {
    packets = decodePktLines(ctx.body);
  } catch {
    return errorResult(400, 'malformed pkt-line stream');
  }
  const lines = pktLinesText(packets);
  if (isV2) {
    if ((lines[0] ?? '').replace(/\n$/, '') !== 'command=fetch') return errorResult(400, 'expected command=fetch');
    const wants: string[] = [];
    const haves: string[] = [];
    const shallow: string[] = [];
    let depth: number | null = null;
    let relative = false;
    for (const line of lines.slice(1)) {
      const text = line.replace(/\n$/, '');
      if (text === '' || text === 'thin-pack' || text === 'no-progress' || text === 'ofs-delta' || text === 'include-tag') continue;
      if (text.startsWith('agent=') || text.startsWith('object-format=')) continue;
      if (text.startsWith('want ')) wants.push(text.slice(5).trim());
      else if (text.startsWith('have ')) haves.push(text.slice(5).trim());
      else if (text.startsWith('shallow ')) shallow.push(text.slice(8).trim());
      else if (text.startsWith('deepen ')) depth = Number.parseInt(text.slice(7).trim(), 10);
      else if (text === 'deepen-relative') relative = true;
      else if (text === 'done') continue;
      else if (text.startsWith('filter ')) return errorResult(400, 'fetch filter is not supported');
    }
    if (wants.length === 0) {
      const parts: Buffer[] = [];
      if (haves.length > 0) {
        parts.push(pktLine('acknowledgments\n'), pktLine('NAK\n'), pktLine('ready\n'), PKT_DELIM);
      }
      parts.push(pktLine('packfile\n'), ...sidebandChunks(EMPTY_PACK), PKT_FLUSH);
      return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
    }
    const missing = wants.filter((w) => !repo.objects.has(w));
    if (missing.length > 0) {
      const parts: Buffer[] = [];
      if (haves.length > 0) {
        parts.push(pktLine('acknowledgments\n'), pktLine('NAK\n'), pktLine('ready\n'), PKT_DELIM);
      }
      parts.push(pktLine('packfile\n'));
      parts.push(pktLine(Buffer.concat([Buffer.from([3]), Buffer.from(`object ${missing[0]} not found\n`, 'utf8')])));
      parts.push(PKT_FLUSH);
      return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
    }
    const plan = planFetch(repo, wants, haves, shallow, depth, relative);
    return { status: 200, contentType: 'application/x-git-upload-pack-result', body: fetchResponseV2(plan, haves, repo) };
  }
  // v0: wants section, flush, haves section, flush.
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | null = null;
  let section = 0;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      section += 1;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const text = packet.payload.toString('utf8').replace(/\n$/, '');
    if (section === 0) {
      if (text.startsWith('want ')) {
        const rest = text.slice(5);
        const sp = rest.indexOf(' ');
        wants.push(sp < 0 ? rest : rest.slice(0, sp));
      } else if (text.startsWith('shallow ')) shallow.push(text.slice(8).trim());
      else if (text.startsWith('deepen ')) depth = Number.parseInt(text.slice(7).trim(), 10);
    } else if (text.startsWith('have ')) {
      haves.push(text.slice(5).trim());
    }
  }
  if (wants.length === 0) {
    return { status: 200, contentType: 'application/x-git-upload-pack-result', body: fetchResponseV0({ objects: new Map(), shallow: [], unshallow: [] }, haves, repo) };
  }
  const plan = planFetch(repo, wants, haves, shallow, depth);
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: fetchResponseV0(plan, haves, repo) };
}

function handleLsRefs(repo: Repo, ctx: GitRequest): HandlerResult {
  let packets;
  try {
    packets = decodePktLines(ctx.body);
  } catch {
    return errorResult(400, 'malformed pkt-line stream');
  }
  const lines = pktLinesText(packets);
  if ((lines[0] ?? '').replace(/\n$/, '') !== 'command=ls-refs') return errorResult(400, 'expected command=ls-refs');
  let peel = false;
  let symrefs = false;
  const prefixes: string[] = [];
  for (const line of lines.slice(1)) {
    const text = line.replace(/\n$/, '');
    if (text === 'peel') peel = true;
    else if (text === 'symrefs') symrefs = true;
    else if (text === 'unborn') continue;
    else if (text.startsWith('ref-prefix ')) prefixes.push(text.slice('ref-prefix '.length));
    else if (text.startsWith('agent=') || text.startsWith('object-format=')) continue;
  }
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  const parts: Buffer[] = [];
  const names = sortedRefNames(repo);
  if (symrefs && keep('HEAD')) {
    const tip = headTip(repo);
    if (tip === ZERO_OID) {
      // An unborn HEAD is announced with the `unborn` keyword, not the zero
      // oid: both the suite's ls-refs parser and real git treat that as "no
      // refs yet" and skip the fetch entirely.
      parts.push(pktLine(`unborn HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
    } else {
      parts.push(pktLine(`${tip} HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
    }
  }
  for (const name of names) {
    if (!keep(name)) continue;
    const oid = repo.refs.get(name) as string;
    let line = `${oid} ${name}`;
    if (peel) {
      const peeled = tagPeel(repo, oid);
      if (peeled !== null) line += ` peeled:${peeled}`;
    }
    parts.push(pktLine(`${line}\n`));
  }
  parts.push(PKT_FLUSH);
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

// ---------------------------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------------------------

/**
 * True when every object reachable from `tip` is present. Incremental: the
 * walk prunes at objects already proven complete (`repo.verified`), so a push
 * that extends a verified tip only inspects the newly arrived objects instead
 * of re-walking the whole history (O(n^2) over a push series). Objects are
 * append-only in a repo, so a verified closure stays complete forever; on
 * success every visited object becomes verified.
 */
function closureComplete(repo: Repo, tip: string): boolean {
  const stack = [tip];
  const visited = new Set<string>();
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (visited.has(oid) || repo.verified.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj === undefined) return false;
    visited.add(oid);
    if (obj.type === 'commit') {
      const c = parseCommit(obj.data);
      stack.push(c.tree);
      for (const p of c.parents) stack.push(p);
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    } else if (obj.type === 'tree') {
      for (const e of parseTree(obj.data)) if (e.mode !== '160000') stack.push(e.oid);
    }
  }
  for (const oid of visited) repo.verified.add(oid);
  return true;
}

function handleReceivePack(repo: Repo, ctx: GitRequest): HandlerResult {
  let packets;
  try {
    packets = decodePktLines(ctx.body, true);
  } catch {
    return errorResult(400, 'malformed pkt-line stream');
  }
  const flushPacket = packets[packets.length - 1];
  if (flushPacket === undefined || flushPacket.kind !== 'flush') {
    return errorResult(400, 'receive-pack command list has no flush');
  }
  const packBytes = ctx.body.subarray(flushPacket.end);
  const commands: Array<{ oldOid: string; newOid: string; ref: string }> = [];
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    let text = packet.payload.toString('utf8');
    const nul = text.indexOf('\0');
    if (nul >= 0) text = text.slice(0, nul);
    const line = text.replace(/\n$/, '');
    const [oldOid, newOid, ref] = line.split(' ');
    if (oldOid === undefined || newOid === undefined || ref === undefined || !isOid(oldOid) || !isOid(newOid) || !ref.startsWith('refs/')) {
      return errorResult(400, `bad receive-pack command ${JSON.stringify(line.slice(0, 60))}`);
    }
    commands.push({ oldOid, newOid, ref });
  }
  if (commands.length === 0) return errorResult(400, 'receive-pack sent no commands');

  const reportLines: string[] = [];
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (packBytes.length > 0) {
    try {
      const objects = readPack(packBytes);
      for (const [oid, obj] of objects) if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
    } catch (err) {
      unpackOk = false;
      unpackDetail = (err as Error).message;
    }
  }
  if (unpackOk) {
    for (const cmd of commands) {
      const current = repo.refs.get(cmd.ref);
      if (cmd.newOid === ZERO_OID) {
        if (current === undefined) {
          reportLines.push(`ng ${cmd.ref} remote ref does not exist`);
        } else if (cmd.oldOid !== ZERO_OID && cmd.oldOid !== current) {
          reportLines.push(`ng ${cmd.ref} non-fast-forward`);
        } else {
          repo.refs.delete(cmd.ref);
          reportLines.push(`ok ${cmd.ref}`);
        }
      } else if (cmd.oldOid === ZERO_OID) {
        if (current !== undefined) {
          reportLines.push(`ng ${cmd.ref} already exists`);
        } else if (!repo.objects.has(cmd.newOid)) {
          reportLines.push(`ng ${cmd.ref} missing object`);
        } else if (!closureComplete(repo, cmd.newOid)) {
          reportLines.push(`ng ${cmd.ref} missing objects`);
        } else {
          repo.refs.set(cmd.ref, cmd.newOid);
          reportLines.push(`ok ${cmd.ref}`);
        }
      } else if (current === undefined) {
        reportLines.push(`ng ${cmd.ref} remote ref does not exist`);
      } else if (cmd.oldOid !== current) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward`);
      } else if (cmd.newOid === current) {
        reportLines.push(`ok ${cmd.ref}`);
      } else if (!repo.objects.has(cmd.newOid)) {
        reportLines.push(`ng ${cmd.ref} missing object`);
      } else if (!closureComplete(repo, cmd.newOid)) {
        reportLines.push(`ng ${cmd.ref} missing objects`);
      } else if (!isAncestor(repo.objects, current, cmd.newOid)) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward`);
      } else {
        repo.refs.set(cmd.ref, cmd.newOid);
        reportLines.push(`ok ${cmd.ref}`);
      }
    }
  }
  const reportText = [`unpack ${unpackDetail}`, ...reportLines].map((l) => `${l}\n`).join('');
  // The report-status stream is itself pkt-lined inside band 1 and ends with a
  // flush inside the sideband data; the outer flush then ends the response.
  // Real git hangs up without the inner flush; the suite's parser ignores it.
  const reportStream = Buffer.concat([
    ...reportText.split('\n').filter((l) => l !== '').map((l) => pktLine(`${l}\n`)),
    PKT_FLUSH,
  ]);
  const body = Buffer.concat([...sidebandChunks(reportStream), PKT_FLUSH]);
  return { status: 200, contentType: 'application/x-git-receive-pack-result', body };
}

// ---------------------------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------------------------

export function handleGit(forge: Forge, ctx: GitRequest): HandlerResult {
  const [owner, repoGit] = ctx.segments;
  if (owner === undefined || repoGit === undefined || !repoGit.endsWith('.git')) {
    return jsonResult(404, { message: 'Not Found' });
  }
  const repoName = repoGit.slice(0, -4);
  const rest = ctx.segments.slice(2);
  const service = ctx.query.get('service');
  const repo = findRepo(forge, owner, repoName);
  if (ctx.method === 'GET' && rest.length === 2 && rest[0] === 'info' && rest[1] === 'refs') {
    if (repo === null) return jsonResult(404, { message: 'Not Found' });
    if (service === 'git-upload-pack') {
      const isV2 = (ctx.headers['git-protocol'] ?? '') === 'version=2';
      return {
        status: 200,
        contentType: 'application/x-git-upload-pack-advertisement',
        body: isV2 ? ADVERTISEMENT_V2_UPLOAD_PACK : advertisementV0(repo, 'git-upload-pack'),
      };
    }
    if (service === 'git-receive-pack') {
      return { status: 200, contentType: 'application/x-git-receive-pack-advertisement', body: advertisementV0(repo, 'git-receive-pack') };
    }
    return jsonResult(404, { message: 'Not Found' });
  }
  if (repo === null) return jsonResult(404, { message: 'Not Found' });
  if (ctx.method === 'POST' && rest.length === 1 && rest[0] === 'git-upload-pack') {
    let firstLine = '';
    try {
      firstLine = pktLinesText(decodePktLines(ctx.body))[0] ?? '';
    } catch {
      return errorResult(400, 'malformed pkt-line stream');
    }
    if (firstLine.replace(/\n$/, '') === 'command=ls-refs') return handleLsRefs(repo, ctx);
    return handleUploadPack(repo, ctx);
  }
  if (ctx.method === 'POST' && rest.length === 1 && rest[0] === 'git-receive-pack') {
    return handleReceivePack(repo, ctx);
  }
  return jsonResult(404, { message: 'Not Found' });
}
