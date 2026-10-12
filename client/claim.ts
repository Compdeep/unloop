/**
 * What a claim's account decides, computed the way the program computes it,
 * for anybody who wants to show it without being trusted to: who was selected
 * to validate it, whether its selection can still be fixed, and what it would
 * be awarded. Each mirrors one function in the program — `Claim::selects`,
 * `seed_in`, `Award::due` — and tests hold it to the
 * program's own result, so a page that shows these is auditing, not deciding.
 *
 * No Node built-ins: the claim page runs this in the browser too.
 */
import { sha256 } from "@noble/hashes/sha256";
import { remaining, type ObjectiveAccount } from "./workable";

/** A u64 as the account decoder hands it over: a BN, a bigint, or anything with a decimal toString. */
type U64 = { toString(): string };
const big = (n: U64) => BigInt(n.toString());
const BPS = 10_000n;

/**
 * Whether `validator` is among those selected for a claim (`Claim::selects`):
 * sha256(seed_le ‖ claim hash ‖ validator) read as a u64, mod the pool, below
 * the number selected. Everybody is selected from a pool no larger than that.
 * Nobody is before the seed is fixed.
 */
export function selects(seed: U64, claimHash: Uint8Array, validator: Uint8Array,
                        validatorsPerClaim: number, pool: number): boolean {
  const s = big(seed);
  if (s === 0n || pool === 0) return false;
  if (pool <= validatorsPerClaim) return true;
  const le = new Uint8Array(8);
  new DataView(le.buffer).setBigUint64(0, s, true);
  const h = sha256(new Uint8Array([...le, ...claimHash, ...validator]));
  return new DataView(h.buffer, h.byteOffset).getBigUint64(0, true) % BigInt(pool) < BigInt(validatorsPerClaim);
}

/** The seed is the hash of this many blocks after the claim's own (`SELECTION_BLOCKS`). */
export const SELECTION_BLOCKS = 10;
/** How many blocks the SlotHashes sysvar holds, newest first (its `MAX_ENTRIES`). */
export const SLOT_HASHES_HELD = 512;

/**
 * Where a claim is in the one window its seed can be fixed in
 * (`seed_in` and `oldest_block_held`), from the SlotHashes sysvar's bytes:
 *
 *   waiting  the tenth block after the claim's own is not produced yet
 *   open     `select_validators` would fix the seed now
 *   closed   the history no longer reaches the claim's block, so nobody can
 *            ever seed it, and `expire_claim` rejects it
 *
 * `after` is the blocks produced since the claim's, and `left` how many more
 * before the window closes: the claim's block leaves the history once
 * `SLOT_HASHES_HELD` blocks are newer than it.
 */
export interface SeedWindow { state: "waiting" | "open" | "closed"; after: number; left: number }

export function seedWindow(slotHashes: Uint8Array, claimSlot: U64): SeedWindow {
  const view = new DataView(slotHashes.buffer, slotHashes.byteOffset, slotHashes.byteLength);
  const held = Number(view.getBigUint64(0, true));
  const slot = (i: number) => view.getBigUint64(8 + i * 40, true);
  const filed = big(claimSlot);
  let after = 0;
  while (after < held && slot(after) > filed) after++;
  const left = Math.max(0, SLOT_HASHES_HELD - after);
  if (held > 0 && slot(held - 1) > filed) return { state: "closed", after, left: 0 };
  // The seed block's first eight bytes are the seed, and zero is no seed.
  const ready = after >= SELECTION_BLOCKS && view.getBigUint64(8 + (after - SELECTION_BLOCKS) * 40 + 8, true) !== 0n;
  return { state: ready ? "open" : "waiting", after, left };
}

/**
 * What an admitted claim on this award is paid (`Award::due`): the whole
 * allocation for winner-take-all; for graded, allocation × score × novelty,
 * capped under a prize by what is left of it. Before resolution score and
 * novelty are zero, and so is a graded award.
 */
export function due(award: ObjectiveAccount["awards"][number], payout: "prize" | "rate",
                    scoreBps: number, noveltyBps: number): bigint {
  const allocation = big(award.allocation);
  if (award.mode.winnerTakeAll) return allocation;
  const share = allocation * BigInt(scoreBps) * BigInt(noveltyBps) / (BPS * BPS);
  if (payout === "rate") return share;
  const left = remaining(award);
  return share < left ? share : left;
}
