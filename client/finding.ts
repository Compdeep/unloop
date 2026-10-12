/**
 * A validator's finding as the chain takes it: the commitment that seals it,
 * the hash of its detail, and the arguments that open it
 * (docs/PROTOCOL.md, rules 5.1 and 5.2).
 *
 *   finding     = sha256(canonical({ items, predicates, rationale }))
 *   commitment  = sha256(claim ‖ validator ‖ reason ‖ claimSupport ‖ alignment
 *                        ‖ novelty_le16 ‖ score_le16 ‖ reproduction ‖ finding ‖ nonce)
 *
 * The first is the `validation.publish` schema's: the detail travels off
 * chain under that hash, an absent field left out as the canonical encoding
 * always leaves it. The second is `Validation::commitment_of`, which the
 * program recomputes at the reveal, and the tests hold this to it.
 *
 * Written over the `validation.publish` message itself, in the protocol's own
 * words for the enums, so what a validator keeps between the two windows is
 * the message it will publish and the nonce, and nothing else. The program's
 * enums are the same lists in the same order.
 *
 * No Node built-ins and no Solana client: the page computes these in the
 * browser, before anything is signed.
 */
import { sha256 } from "@noble/hashes/sha256";
import { bytesToHex, hexToBytes, utf8ToBytes } from "@noble/hashes/utils";
import { canonical } from "./spec";

/** In discriminant order, the protocol's names (`validation.publish`). */
export const REASONS = ["none", "refused", "unavailable", "access_denied", "timed_out",
  "unsupported", "environment", "resource_limit", "internal"] as const;
export const FINDINGS = ["pass", "fail", "insufficient"] as const;
export const REPRODUCTIONS = ["not_attempted", "pass", "fail"] as const;

/** The same, as Anchor names the program's variants. */
const ANCHOR = {
  reason: ["none", "refused", "unavailable", "accessDenied", "timeout", "unsupported", "environment", "resourceLimit", "internal"],
  finding: ["pass", "fail", "insufficient"],
  reproduction: ["notAttempted", "pass", "fail"],
};

/** The parts of a `validation.publish` message the chain sees, and the detail its `finding` hashes. */
export interface ValidationMessage {
  claimHash?: string;
  reason: typeof REASONS[number];
  claimSupport: typeof FINDINGS[number];
  objectiveAlignment: typeof FINDINGS[number];
  noveltyBps: number;
  scoreBps: number;
  reproduction: typeof REPRODUCTIONS[number];
  /** hex */
  finding: string;
  items?: unknown[];
  predicates?: unknown[];
  rationale?: string;
  [field: string]: unknown;
}

const ZERO = "0".repeat(64);
const index = <T extends readonly string[]>(list: T, v: string, what: string) => {
  const i = list.indexOf(v);
  if (i < 0) throw new Error(`${what} is ${JSON.stringify(v)}, not one of ${list.join(", ")}`);
  return i;
};

/** The hash the chain holds for a finding's detail, hex. */
export function findingHash(m: Pick<ValidationMessage, "items" | "predicates" | "rationale">): string {
  const detail: Record<string, unknown> = {};
  for (const k of ["items", "predicates", "rationale"] as const) if (m[k] !== undefined) detail[k] = m[k];
  return bytesToHex(sha256(utf8ToBytes(canonical(detail))));
}

/**
 * What would stop this message opening, in a sentence each: what the program
 * refuses at the reveal, and a `finding` that is not the hash of the detail
 * beside it. Empty when it is sound.
 */
export function problems(m: ValidationMessage): string[] {
  const out: string[] = [];
  const check = (f: () => unknown) => { try { f(); } catch (e) { out.push((e as Error).message); } };
  check(() => index(REASONS, m.reason, "reason"));
  check(() => index(FINDINGS, m.claimSupport, "claimSupport"));
  check(() => index(FINDINGS, m.objectiveAlignment, "objectiveAlignment"));
  check(() => index(REPRODUCTIONS, m.reproduction, "reproduction"));
  for (const k of ["noveltyBps", "scoreBps"] as const) {
    if (!Number.isInteger(m[k]) || m[k] < 0 || m[k] > 10_000) out.push(`${k} must be a whole number from 0 to 10000`);
  }
  if (!/^[0-9a-f]{64}$/.test(String(m.finding))) out.push("finding must be 64 hex characters");
  else if (m.reason === "none" && m.finding === ZERO) out.push("an evaluated finding cannot be the zero hash");
  // One that did not evaluate may commit to no detail at all, which is the zero hash.
  else if (m.items !== undefined && !(m.reason !== "none" && m.finding === ZERO) && m.finding !== findingHash(m)) {
    out.push("finding is not the hash of the items, predicates and rationale beside it");
  }
  if (m.reason !== "none" && (m.noveltyBps !== 0 || m.scoreBps !== 0)) out.push("a validation that did not evaluate carries no numbers");
  return out;
}

/** A fresh nonce, hex. Without it a commitment to two small numbers could be guessed by trying them all. */
export function nonce(): string {
  return bytesToHex(globalThis.crypto.getRandomValues(new Uint8Array(32)));
}

/** `Validation::commitment_of`, hex. `claim` and `validator` are the 32 bytes of each address. */
export function commitmentOf(claim: Uint8Array, validator: Uint8Array, m: ValidationMessage, nonceHex: string): string {
  const le16 = (n: number) => [n & 0xff, n >> 8];
  return bytesToHex(sha256(new Uint8Array([
    ...claim, ...validator,
    index(REASONS, m.reason, "reason"), index(FINDINGS, m.claimSupport, "claimSupport"),
    index(FINDINGS, m.objectiveAlignment, "objectiveAlignment"),
    ...le16(m.noveltyBps), ...le16(m.scoreBps), index(REPRODUCTIONS, m.reproduction, "reproduction"),
    ...hexToBytes(m.finding), ...hexToBytes(nonceHex),
  ])));
}

/** `reveal_validation`'s arguments, in order, as Anchor takes them. */
export function revealArgs(m: ValidationMessage, nonceHex: string) {
  const variant = (names: string[], i: number) => ({ [names[i]]: {} });
  return [
    variant(ANCHOR.reason, index(REASONS, m.reason, "reason")),
    variant(ANCHOR.finding, index(FINDINGS, m.claimSupport, "claimSupport")),
    variant(ANCHOR.finding, index(FINDINGS, m.objectiveAlignment, "objectiveAlignment")),
    m.noveltyBps, m.scoreBps,
    variant(ANCHOR.reproduction, index(REPRODUCTIONS, m.reproduction, "reproduction")),
    Array.from(hexToBytes(m.finding)), Array.from(hexToBytes(nonceHex)),
  ] as const;
}
