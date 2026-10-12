/**
 * An objective's specification against its account: the two hashes the
 * account commits to, recomputed from the `objective.publish` message, and the
 * message's other fields compared with the values the account stores.
 *
 * The rule (docs/PROTOCOL.md, rule 2.1):
 *
 *   criteria_hash   = sha256(canonical({ title, statement, validationMode,
 *                                         predicates, composition, awards,
 *                                         samples: samples.items }))
 *   claim_form_hash = sha256(canonical(claimForm))
 *
 * canonical is the protocol's one encoding, RFC 8785, with an absent field
 * left out rather than written as null. One implementation, so a leader, an
 * agent and the platform cannot disagree about whether a specification is
 * the one that was committed.
 */
import { sha256 as digest } from "@noble/hashes/sha256";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils";
import { setProblems, type Samples } from "./samples";

/**
 * RFC 8785: keys sorted by UTF-16 code unit, no whitespace, numbers and
 * strings as ECMAScript's JSON.stringify writes them — which is the
 * serialisation the RFC is defined by, so it is used directly.
 */
export function canonical(v: unknown): string {
  if (v === null || typeof v === "boolean" || typeof v === "string") return JSON.stringify(v);
  if (typeof v === "number") {
    if (!Number.isFinite(v)) throw new Error("RFC 8785 has no encoding for a non-finite number");
    return JSON.stringify(v);
  }
  if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
  if (typeof v === "object") {
    const entries = Object.entries(v as Record<string, unknown>).filter(([, x]) => x !== undefined);
    entries.sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    return `{${entries.map(([k, x]) => `${JSON.stringify(k)}:${canonical(x)}`).join(",")}}`;
  }
  throw new Error(`RFC 8785 has no encoding for ${typeof v}`);
}

// Not node:crypto: the page that drafts an objective computes these in the
// browser, and it must be this code that does it.
const sha256 = (s: string) => bytesToHex(digest(utf8ToBytes(s)));

/** The parts of an `objective.publish` body this reads. Anything else in it is ignored. */
export interface PublishBody {
  id?: string;
  title?: unknown; statement?: unknown; validationMode?: unknown;
  predicates?: unknown; composition?: unknown;
  awards?: { index: unknown; title: unknown; criteria: unknown }[];
  samples?: { root?: string; items?: unknown; leaves?: string[] };
  claimForm?: unknown;
  payout?: string;
  validatorsPerClaim?: number;
  consensus?: { minVotes?: number; acceptThresholdBps?: number; rejectThresholdBps?: number };
  claimDeadline?: string;
  validationDeadline?: string;
}

/** Both hashes, hex, recomputed from the message. */
export function specHashes(body: PublishBody): { criteriaHash: string; claimFormHash: string } {
  const criteria = {
    title: body.title,
    statement: body.statement,
    validationMode: body.validationMode,
    predicates: body.predicates,
    composition: body.composition,
    // Each award's definition only: what it pays is on chain.
    awards: body.awards?.map(a => ({ index: a.index, title: a.title, criteria: a.criteria })),
    samples: body.samples?.items,
  };
  return { criteriaHash: sha256(canonical(criteria)), claimFormHash: sha256(canonical(body.claimForm)) };
}

/** The body with its own criteriaHash written in, as a published one carries it. */
export function sealed(body: PublishBody): PublishBody & { criteriaHash: string } {
  return { ...body, criteriaHash: specHashes(body).criteriaHash };
}

/** What the account stores that this compares, in the shape the program's client reads it. */
export interface ObjectiveCommitments {
  address: string;
  criteriaHash: number[] | Uint8Array;
  claimFormHash: number[] | Uint8Array;
  entryProofRoot: number[] | Uint8Array;
  entryProofCount: number;
  payout: { prize?: object; rate?: object };
  validatorsPerClaim: number;
  consensus: { minVotes: number; acceptThresholdBps: number; rejectThresholdBps: number };
  claimDeadline: { toString(): string };
  validationDeadline: { toString(): string };
}

export interface SpecCheck {
  criteriaHash: string;
  claimFormHash: string;
  /** Both hashes equal the account's, and every copied field agrees with it. */
  verified: boolean;
  /** One line per disagreement, empty when verified. */
  differences: string[];
}

const hex = (b: number[] | Uint8Array) => bytesToHex(Uint8Array.from(b));
/** An RFC 3339 time as the unix seconds an account stores it as. */
export const seconds = (iso: string) => Math.floor(Date.parse(iso) / 1000);

/** The message against the account: the two hashes, then every field the account holds a copy of. */
export function checkSpec(body: PublishBody, account: ObjectiveCommitments): SpecCheck {
  const { criteriaHash, claimFormHash } = specHashes(body);
  const differences: string[] = [];
  const differ = (field: string, message: unknown, chain: unknown) => {
    if (String(message) !== String(chain)) differences.push(`${field}: the message says ${message}, the account ${chain}`);
  };

  differ("criteria hash", criteriaHash, hex(account.criteriaHash));
  differ("claim-format hash", claimFormHash, hex(account.claimFormHash));
  if (body.id !== undefined) differ("id", body.id, account.address);
  differ("payout", body.payout, account.payout.prize ? "prize" : "rate");
  if (body.validatorsPerClaim !== undefined) differ("validatorsPerClaim", body.validatorsPerClaim, account.validatorsPerClaim);
  // Absent here means the colony's default, which the account pinned; only a stated policy can disagree.
  if (body.consensus) {
    for (const k of ["minVotes", "acceptThresholdBps", "rejectThresholdBps"] as const) {
      if (body.consensus[k] !== undefined) differ(`consensus.${k}`, body.consensus[k], account.consensus[k]);
    }
  }
  if (body.claimDeadline) differ("claimDeadline", seconds(body.claimDeadline), account.claimDeadline.toString());
  if (body.validationDeadline) differ("validationDeadline", seconds(body.validationDeadline), account.validationDeadline.toString());
  const root = hex(account.entryProofRoot);
  differ("samples.root", body.samples?.root ?? "0".repeat(64), root);
  if (body.samples) {
    const items = Array.isArray(body.samples.items) ? body.samples.items : [];
    differ("samples count", items.length, account.entryProofCount);
    // Leaves are outside both hashes, so only a published list can be wrong; a
    // missing one leaves the document the committed one, with no door built.
    if (body.samples.leaves) {
      differences.push(...setProblems({ root: body.samples.root ?? "", items, leaves: body.samples.leaves } as Samples));
    }
  }

  return { criteriaHash, claimFormHash, verified: differences.length === 0, differences };
}
