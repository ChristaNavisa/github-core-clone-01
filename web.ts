// Shared HTTP plumbing for the four surfaces: one response shape, JSON replies
// and the git pkt-line codec.

export interface HandlerResult {
  status: number;
  contentType: string;
  body: Buffer;
  headers?: Record<string, string>;
}

export const JSON_TYPE = 'application/json; charset=utf-8';

export function jsonResult(status: number, value: unknown, headers: Record<string, string> = {}): HandlerResult {
  const text = `${JSON.stringify(value)}\n`;
  return { status, contentType: JSON_TYPE, body: Buffer.from(text, 'utf8'), headers };
}

export function emptyResult(status: number, headers: Record<string, string> = {}): HandlerResult {
  return { status, contentType: JSON_TYPE, body: Buffer.alloc(0), headers };
}

export function errorResult(status: number, message: string): HandlerResult {
  return jsonResult(status, { message });
}

/** A monotonic, deterministic request id (never a clock or Math.random). */
let requestCounter = 0;
export function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter.toString(16).padStart(8, '0')}`;
}

// ---------------------------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------------------------

export function pktLine(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const head = Buffer.allocUnsafe(4);
  head.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([head, data]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

export type PktKind = 'data' | 'flush' | 'delim' | 'response-end';

export interface PktPacket {
  kind: PktKind;
  payload: Buffer;
  /** Byte offset just past this packet in the source buffer. */
  end: number;
}

/**
 * Decode pkt-lines from the start of `buf`. Stops at the first flush/delim (the
 * caller decides what follows) or at the end of the buffer. Throws on malformed
 * framing. `stopAtControl` returns as soon as a flush/delim/response-end packet
 * is seen, so the caller can treat the rest as raw bytes (a packfile).
 */
export function decodePktLines(buf: Buffer, stopAtControl = false): PktPacket[] {
  const packets: PktPacket[] = [];
  let off = 0;
  while (off + 4 <= buf.length) {
    const head = buf.subarray(off, off + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(head)) throw new Error(`invalid pkt-line length ${JSON.stringify(head)}`);
    const len = Number.parseInt(head, 16);
    if (len === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0), end: off + 4 });
      if (stopAtControl) return packets;
      off += 4;
      continue;
    }
    if (len === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0), end: off + 4 });
      if (stopAtControl) return packets;
      off += 4;
      continue;
    }
    if (len === 2) {
      packets.push({ kind: 'response-end', payload: Buffer.alloc(0), end: off + 4 });
      if (stopAtControl) return packets;
      off += 4;
      continue;
    }
    if (len === 3 || len > 65520) throw new Error(`invalid pkt-line length ${len}`);
    if (off + len > buf.length) throw new Error('truncated pkt-line');
    packets.push({ kind: 'data', payload: buf.subarray(off + 4, off + len), end: off + len });
    off += len;
  }
  if (off !== buf.length) throw new Error('trailing bytes after pkt-lines');
  return packets;
}

/** The text of the data packets, control packets dropped. */
export function pktLinesText(packets: readonly PktPacket[]): string[] {
  const lines: string[] = [];
  for (const p of packets) if (p.kind === 'data') lines.push(p.payload.toString('utf8'));
  return lines;
}

/** One sideband data packet: band byte + chunk, framed as a pkt-line. */
export function sidebandPacket(band: 1 | 2 | 3, chunk: Buffer): Buffer {
  return pktLine(Buffer.concat([Buffer.from([band]), chunk]));
}

/** Split `data` into sideband band-1 packets of at most 65515 payload bytes. */
export function sidebandChunks(data: Buffer): Buffer[] {
  const out: Buffer[] = [];
  for (let off = 0; off < data.length; off += 65515) {
    out.push(sidebandPacket(1, data.subarray(off, Math.min(data.length, off + 65515))));
  }
  if (data.length === 0) out.push(sidebandPacket(1, Buffer.alloc(0)));
  return out;
}
