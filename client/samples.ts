/**
 * An objective's worked samples, which a would-be validator answers to let
 * itself into a public colony (`submit_entry_proof`; docs/PLATFORM.md section
 * 5, "The door"). One implementation of each rule, for the leader drafting
 * the set and the validator proving an answer, mirroring the program's
 * `Objective::entry_proof_for` and `verifies_entry_proof`:
 *
 *   answer = sha256(utf8(result))              the result the sample's input asks for
 *   leaf   = sha256(index_le32 ‖ answer)
 *   pair   = sha256(lower ‖ higher)            sorted, so a branch needs no directions
 *   index  = u64_le(sha256(address ‖ objective)[0..8]) mod count
 *
 * The tree pairs each level left to right and carries an odd last node up
 * unchanged. A document publishes the inputs, the root and the leaves, and
 * never a result or an answer: with the leaves anybody can build the branch
 * for their own sample, and check an answer before sending it.
 *
 * No Node built-ins: the objective form and the colony page run this.
 */
import { sha256 } from "@noble/hashes/sha256";
import { bytesToHex, hexToBytes, utf8ToBytes } from "@noble/hashes/utils";

/** What the published document carries. */
export interface Samples {
  root: string;
  items: { index: number; input: string; artifacts?: string[] }[];
  leaves?: string[];
}

/** The 32 bytes a sample is answered with: the hash of the result as written. */
export const answerOf = (result: string) => sha256(utf8ToBytes(result));

export function leafOf(index: number, answer: Uint8Array): Uint8Array {
  const n = new Uint8Array(4);
  new DataView(n.buffer).setUint32(0, index, true);
  return sha256(new Uint8Array([...n, ...answer]));
}

function pair(a: Uint8Array, b: Uint8Array): Uint8Array {
  const lower = bytesToHex(a) <= bytesToHex(b);
  return sha256(new Uint8Array(lower ? [...a, ...b] : [...b, ...a]));
}

/** Every level of the tree, leaves first, root last. */
function levels(leaves: Uint8Array[]): Uint8Array[][] {
  const out = [leaves];
  while (out[out.length - 1].length > 1) {
    const l = out[out.length - 1], next: Uint8Array[] = [];
    for (let i = 0; i < l.length; i += 2) next.push(i + 1 < l.length ? pair(l[i], l[i + 1]) : l[i]);
    out.push(next);
  }
  return out;
}

/** The root over these leaves; zero for none, which is an objective with no door. */
export function rootOf(leaves: Uint8Array[]): Uint8Array {
  return leaves.length ? levels(leaves).at(-1)![0] : new Uint8Array(32);
}

/** The siblings from leaf `index` up to the root: what `submit_entry_proof` takes as its branch. */
export function branchOf(leaves: Uint8Array[], index: number): Uint8Array[] {
  const out: Uint8Array[] = [];
  let i = index;
  for (const l of levels(leaves).slice(0, -1)) {
    const sib = i % 2 === 0 ? i + 1 : i - 1;
    if (sib < l.length) out.push(l[sib]);
    i = Math.floor(i / 2);
  }
  return out;
}

/** Which sample `address` must answer on `objective` (both 32 bytes), out of `count`. */
export function sampleFor(address: Uint8Array, objective: Uint8Array, count: number): number {
  if (count <= 1) return 0;
  const h = sha256(new Uint8Array([...address, ...objective]));
  return Number(new DataView(h.buffer, h.byteOffset).getBigUint64(0, true) % BigInt(count));
}

/**
 * The leader's side: a set drafted from each sample's input and its result,
 * as the document publishes it. The results stay with the leader.
 */
export function sampleSet(drafts: { input: string; result: string; artifacts?: string[] }[]): Samples {
  if (!drafts.length) throw new Error("a sample set needs at least one sample");
  drafts.forEach((d, i) => {
    if (!d.input.trim()) throw new Error(`sample ${i} has no input`);
    if (!d.result) throw new Error(`sample ${i} has no result to answer it with`);
  });
  const leaves = drafts.map((d, i) => leafOf(i, answerOf(d.result)));
  return {
    root: bytesToHex(rootOf(leaves)),
    items: drafts.map((d, i) => ({ index: i, input: d.input, ...(d.artifacts?.length ? { artifacts: d.artifacts } : {}) })),
    leaves: leaves.map(bytesToHex),
  };
}

/**
 * What is wrong with a published set, one line each, empty when nothing is:
 * items not numbered in order, leaves missing or not one per item, or leaves
 * that do not rebuild the root. A set without leaves can still be the one
 * committed, and nobody can prove an answer to it.
 *
 * Because pairs are hashed sorted, two sibling leaves published swapped still
 * rebuild the root. Nothing short of the answers can tell; the cost is that a
 * validator holding one of those two samples is told its right answer does
 * not match, and the program would refuse the proof too, so nothing false
 * gets in.
 */
export function setProblems(s: Samples): string[] {
  const out: string[] = [];
  s.items.forEach((it, i) => { if (it.index !== i) out.push(`samples.items[${i}] is numbered ${it.index}`); });
  if (!s.leaves) return [...out, "samples.leaves is missing, so no answer to these samples can be proved"];
  if (s.leaves.length !== s.items.length) out.push(`samples.leaves has ${s.leaves.length} leaves for ${s.items.length} samples`);
  else if (bytesToHex(rootOf(s.leaves.map(l => hexToBytes(l)))) !== s.root) out.push("samples.leaves do not rebuild samples.root");
  return out;
}

/**
 * The validator's side: the proof for `result` to the sample this address
 * gets, and whether the answer matches the published leaf — so a wrong one is
 * found here rather than by a transaction the program refuses.
 */
export function proofFor(s: Samples, address: Uint8Array, objective: Uint8Array, result: string) {
  const problems = setProblems(s);
  if (problems.length) throw new Error(problems.join("; "));
  const leaves = s.leaves!.map(l => hexToBytes(l));
  const index = sampleFor(address, objective, s.items.length);
  const answer = answerOf(result);
  return { index, answer, branch: branchOf(leaves, index), matches: bytesToHex(leafOf(index, answer)) === s.leaves![index] };
}
