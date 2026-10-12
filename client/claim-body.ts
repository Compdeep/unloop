/**
 * A claim's body against its account: the claim hash recomputed the way the
 * objective's mode says it is taken, and the body's copies of what the
 * account holds compared with it.
 *
 * The rule (docs/PROTOCOL.md, rule 3.1):
 *
 *   deterministic             claim_hash = sha256(utf8(body.assertion))
 *   procedural, semantic      claim_hash = sha256(canonical(body without claimHash))
 *
 * Under `deterministic` the assertion is the canonical result: the objective's
 * claim format applied to it gives it back unchanged. That half is the
 * claimant's to have done and a validator's to confirm by running the format;
 * this file hashes what the body says, and does not run anybody's format.
 *
 * The mode comes from the objective's verified specification. Without one,
 * which rule applies is unknown, and a body cannot be checked at all — which
 * is a different failure from a body that does not hash to its account, and
 * is reported as one.
 */
import { sha256 } from "@noble/hashes/sha256";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils";
import { canonical } from "./spec";

export type Mode = "deterministic" | "procedural" | "semantic";

/** The parts of a `claim.publish` body this reads. The rest is hashed whole under the two judged modes. */
export interface ClaimBody {
  objective?: unknown; awardIndex?: unknown; claimHash?: unknown;
  assertion?: unknown; roots?: unknown;
  [field: string]: unknown;
}

/** What the account (and the index's copy of its roots) holds that the body repeats. */
export interface ClaimCommitments {
  objective: string;
  /** hex */
  hash: string;
  awardIndex: number;
  /** Root claim addresses, in the order declared. */
  roots: string[];
}

export interface BodyCheck {
  /** The text that was hashed: the assertion, or the canonical encoding. Null when the mode is unknown. */
  canonical: string | null;
  /** hex, recomputed. Null when the mode is unknown. */
  hash: string | null;
  verified: boolean;
  /**
   * What failed, when something did: "mode", no verified specification to say
   * which rule applies; "hash", the body is not what was committed; "fields",
   * it hashes right but repeats the account wrongly (under `deterministic`
   * only the assertion is hashed, so its other fields can drift).
   */
  failed: "mode" | "hash" | "fields" | null;
  /** One line per disagreement, empty when verified. */
  differences: string[];
}

/** The text a claim hash is taken over, under `mode`. */
export function hashedText(body: ClaimBody, mode: Mode): string {
  if (mode === "deterministic") {
    if (typeof body.assertion !== "string") throw new Error("a deterministic claim's assertion is its canonical result, and must be text");
    return body.assertion;
  }
  const { claimHash: _self, ...rest } = body;
  return canonical(rest);
}

/** The claim hash, hex, of `body` under `mode`. */
export function claimHashOf(body: ClaimBody, mode: Mode): string {
  return bytesToHex(sha256(utf8ToBytes(hashedText(body, mode))));
}

/** The body against the account. `mode` is the objective's, from its verified specification, or null without one. */
export function checkBody(body: ClaimBody, account: ClaimCommitments, mode: Mode | null): BodyCheck {
  if (mode === null) {
    return { canonical: null, hash: null, verified: false, failed: "mode",
      differences: ["the objective's specification is not verified, so which rule hashes its claims is unknown"] };
  }
  let text: string;
  try { text = hashedText(body, mode); }
  catch (e) { return { canonical: null, hash: null, verified: false, failed: "hash", differences: [(e as Error).message] }; }
  const hash = bytesToHex(sha256(utf8ToBytes(text)));

  const differences: string[] = [];
  const differ = (field: string, said: unknown, chain: unknown) => {
    if (String(said) !== String(chain)) differences.push(`${field}: the body says ${said}, the account ${chain}`);
  };
  differ("claim hash", hash, account.hash);
  if (body.claimHash !== undefined) differ("claimHash as the body states it", body.claimHash, account.hash);
  differ("objective", body.objective, account.objective);
  differ("awardIndex", body.awardIndex, account.awardIndex);
  differ("roots", JSON.stringify(body.roots ?? []), JSON.stringify(account.roots));

  const verified = differences.length === 0;
  return { canonical: text, hash, verified, failed: hash !== account.hash ? "hash" : verified ? null : "fields", differences };
}
