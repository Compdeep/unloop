/**
 * What anybody may send about a claim's validators now, and nobody is paid
 * to (docs/PLATFORM.md section 5, "Upkeep"):
 *
 *   absence  record_absence   drawn or trusted, in the pool, not the claimant,
 *                             no commitment where one would be, the commit
 *                             window closed, and the claim filed after the
 *                             last one it committed to — so a run of misses
 *                             is in claim order
 *   slash    slash_bond       a commitment still sealed once the validation
 *                             window has closed, and a bond to burn
 *
 * The index says which validators to look at; each is read from its accounts
 * and kept only where the instruction's own checks pass, and one that is not
 * says which check, the way `owed` does. `now` is the chain's time.
 */
import { PublicKey } from "@solana/web3.js";
import type { Program } from "@anchor-lang/core";
import { unpackAccount, getMint } from "@solana/spl-token";
import { selects } from "./claim";

export type Duty =
  | { kind: "absence"; validator: string; absences: number; suspends: boolean }
  | { kind: "slash"; validator: string; bond: bigint; mint: string; decimals: number };

export interface Upkeep { duties: Duty[]; refused: { validator: string; reason: string }[] }

/** Selections in a row a validator may miss before it is suspended (`MAX_ABSENCES`). */
export const MAX_ABSENCES = 3;

const big = (n: { toString(): string }) => BigInt(n.toString());
const ZERO = "0".repeat(64);

export async function upkeep(program: Program, claim: PublicKey, candidates: PublicKey[], now: bigint): Promise<Upkeep> {
  const pda = (...seeds: Buffer[]) => PublicKey.findProgramAddressSync(seeds, program.programId)[0];
  const accounts = program.account as any;
  const c = await accounts.claim.fetch(claim);
  const o = await accounts.objective.fetch(c.objective);
  const colony: PublicKey = o.colony;
  const unique = [...new Map(candidates.map(k => [k.toBase58(), k])).values()];
  const records = await accounts.validator.fetchMultiple(unique.map(v => pda(Buffer.from("validator"), colony.toBuffer(), v.toBuffer())));
  const validations = await accounts.validation.fetchMultiple(unique.map(v => pda(Buffer.from("validation"), claim.toBuffer(), v.toBuffer())));
  const bondAddresses = unique.map(v => pda(Buffer.from("bond"), colony.toBuffer(), v.toBuffer()));
  const bondInfos = await program.provider.connection.getMultipleAccountsInfo(bondAddresses);

  const duties: Duty[] = [], refused: Upkeep["refused"] = [];
  const slashes: { validator: string; bond: bigint; mint: PublicKey; tokenProgram: PublicKey }[] = [];
  unique.forEach((v, i) => {
    const validator = v.toBase58();
    const no = (reason: string) => { refused.push({ validator, reason }); };
    const r = records[i], x = validations[i];
    if (!r) return no("it has no record in this colony");

    if (x) {
      // A marker record_absence left has a zero commitment: nothing to slash.
      if (x.revealed) return no("it opened its commitment");
      if (Buffer.from(x.commitment).toString("hex") === ZERO) return no("its absence is already recorded");
      if (now < big(o.validationDeadline)) return no("its commitment can still be opened");
      const info = bondInfos[i];
      const held = info ? unpackAccount(bondAddresses[i], info, info.owner) : null;
      if (!held || held.amount === 0n) return no("it has no bond to burn");
      slashes.push({ validator, bond: held.amount, mint: held.mint, tokenProgram: info!.owner });
      return;
    }

    if (now < big(o.commitDeadline)) return no("it can still commit");
    if (!r.activated) return no("it is not in the pool");
    if (c.agent.equals(v)) return no("it filed this claim");
    if (!r.trusted && !selects(c.selectionSeed, Uint8Array.from(c.hash), v.toBytes(), o.validatorsPerClaim, c.selectionPool))
      return no("this claim did not draw it");
    if (!(big(c.claimSlot) > big(r.committedSlot))) return no("it committed to a later claim, so this miss is not in its run");
    const absences = r.absences + 1;
    duties.push({ kind: "absence", validator, absences, suspends: absences >= MAX_ABSENCES });
  });
  // A bond is in the protocol token: its decimals, read once, so the burn is shown in whole tokens.
  const decimals = new Map<string, number>();
  for (const x of slashes) {
    const m = x.mint.toBase58();
    if (!decimals.has(m)) decimals.set(m, (await getMint(program.provider.connection, x.mint, "confirmed", x.tokenProgram)).decimals);
    duties.push({ kind: "slash", validator: x.validator, bond: x.bond, mint: m, decimals: decimals.get(m)! });
  }
  return { duties, refused };
}
