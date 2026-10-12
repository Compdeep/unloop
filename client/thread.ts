/**
 * A claim's thread, read: what has been said, what is still open, and whether
 * the author is expected to answer (protocol/README.md, the thread layer;
 * docs/PLATFORM.md section 7). The server reads every claim's thread through
 * this, and the page builds the next message with it.
 *
 *   cannot_verify   a validator says where it stopped, filed against the
 *                   latest clarification it has seen (`answering`)
 *   clarification   the author answers objections (`addresses`), changing
 *                   nothing the hash covers
 *   amendment       the author adds evidence, as submission 1, 2, …
 *
 * Objections count toward the ratio when they are filed against the latest
 * clarification — an answer starts a new round, or one bad round condemns a
 * claim for good — and come from a validator with a commitment on the claim,
 * once each, leaving out `out_of_reach`, which is a validator without the
 * means rather than a claim without them. The denominator is the validators
 * with a commitment: before the reveal nobody has voted, and they are who will.
 *
 * Nobody assigns `seq`, so two messages may share one. They are told apart by
 * their message ids, and ordered by when the node heard them.
 *
 * No Node built-ins: the claim page runs this.
 */

export type ThreadType = "cannot_verify" | "clarification" | "amendment";
export const THREAD_TYPES: readonly ThreadType[] = ["cannot_verify", "clarification", "amendment"];

/** A colony's limits on its threads (policy.schema.json, `threadPolicy`). */
export interface ThreadPolicy { objectionThreshold: number; maxClarifications: number; maxBodyBytes: number }

/** The schema's defaults, for a colony that has announced no policy of its own. */
export const DEFAULT_POLICY: ThreadPolicy = { objectionThreshold: 0.1, maxClarifications: 5, maxBodyBytes: 262_144 };

/** One message of a thread, as the archive keeps it. */
export interface Said {
  id: string;
  type: ThreadType;
  author: string;
  body: Record<string, any>;
  receivedAt: string | Date;
}

export interface ThreadState {
  /** The seq of the latest clarification; 0 before any. What a new objection answers. */
  round: number;
  /** Validators objecting in this round, once each, out_of_reach left out. */
  objecting: string[];
  /** Validators with a commitment on the claim. */
  voters: number;
  /** objecting / voters; null while nobody has committed. */
  ratio: number | null;
  /** The policy's threshold, and whether the ratio is above it: the author is expected to answer. */
  threshold: number;
  expected: boolean;
  /** Objection seqs no clarification has addressed yet, out_of_reach included: it may still deserve an answer. */
  open: number[];
  clarificationsLeft: number;
  /** The numbers the next message carries. */
  next: { seq: number; answering: number; submission: number };
}

const time = (s: Said) => new Date(s.receivedAt).getTime();

/** The thread in the order the node heard it, seq breaking ties. */
export function ordered(thread: Said[]): Said[] {
  return [...thread].sort((a, b) => time(a) - time(b) || (a.body.seq ?? 0) - (b.body.seq ?? 0) || (a.id < b.id ? -1 : 1));
}

/** Where a thread stands, for the validators with a commitment on its claim. */
export function threadState(thread: Said[], voters: string[], policy: ThreadPolicy = DEFAULT_POLICY): ThreadState {
  const of = (t: ThreadType) => thread.filter(m => m.type === t);
  const clarifications = of("clarification"), objections = of("cannot_verify"), amendments = of("amendment");
  const round = Math.max(0, ...clarifications.map(m => m.body.seq));
  const committed = new Set(voters);

  const objecting = [...new Set(objections
    .filter(m => m.body.answering >= round && m.body.blocker !== "out_of_reach" && committed.has(m.author))
    .map(m => m.author))];
  const ratio = committed.size ? objecting.length / committed.size : null;
  const addressed = new Set(clarifications.flatMap(m => m.body.addresses ?? []));
  const sequenced = [...objections, ...clarifications].map(m => m.body.seq);

  return {
    round, objecting, voters: committed.size, ratio, threshold: policy.objectionThreshold,
    expected: ratio !== null && ratio > policy.objectionThreshold,
    open: [...new Set(objections.map(m => m.body.seq).filter(s => !addressed.has(s)))].sort((a, b) => a - b),
    clarificationsLeft: Math.max(0, policy.maxClarifications - clarifications.length),
    next: {
      seq: sequenced.length ? Math.max(...sequenced) + 1 : 0,
      answering: round,
      submission: Math.max(0, ...amendments.map(m => m.body.submission)) + 1,
    },
  };
}

/** Evidence an amendment adds, with ids that cannot collide with the claim's (`D1`) or another amendment's. */
export function amendmentEvidence(submission: number, items: { kind: "deterministic" | "procedural" | "semantic"; statement: string; provenance: string }[]) {
  const set = { deterministic: [] as any[], procedural: [] as any[], semantic: [] as any[] };
  for (const kind of ["deterministic", "procedural", "semantic"] as const) {
    set[kind] = items.filter(e => e.kind === kind).map((e, i) => ({
      id: `A${submission}${kind[0].toUpperCase()}${i + 1}`, kind, statement: e.statement, provenance: e.provenance,
    }));
  }
  return set;
}
