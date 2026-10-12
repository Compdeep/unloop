/**
 * Where an address stands as a validator in a colony, and the instructions
 * that move it (docs/PLATFORM.md section 5):
 *
 *   status    read from the colony and the address's record now: which state
 *             it is in, and what that state is waiting on
 *   qualify   the admission authority's attestation; it signs and pays for
 *             the record, and the address signs nothing
 *   prove     submit_entry_proof, the address's own way in, in a public
 *             colony, with the answer to the sample the program chose for it;
 *             `proveResult` builds it from the result itself and the
 *             published set (client/samples.ts), refusing a wrong result
 *             before anything is signed
 *   bond      post_bond, the colony's bond, which puts it in the pool
 *   rejoin    back after a suspension for missed selections, for nothing
 *   absence   record_absence, anybody's, and
 *   slash     slash_bond, anybody's: the upkeep `upkeep.ts` says is due
 *
 * Leaving is `withdrawBond` in claim-actions.ts, which what is owed reaches.
 * Who may do what is the program's to refuse; `status` says in advance which
 * of these would be refused, so a page offers only the ones that would not.
 */
import { PublicKey, type TransactionInstruction } from "@solana/web3.js";
import type { Program } from "@anchor-lang/core";
import { tokens } from "./tokens";
import { proofFor, sampleFor, type Samples } from "./samples";

import { MAX_ABSENCES } from "./upkeep";
export { MAX_ABSENCES };

export type Admission = "public" | "keyed" | "private";

/**
 *   none       no record: `doors` says how this colony lets one in
 *   qualified  in, but not in the pool until it posts `bond`
 *   probation  in the pool; its answers count and are paid after `probationLeft` more
 *   pool       in the pool, its answers counted
 *   suspended  out for missed selections; `rejoin` brings it back free
 *   revoked    out for good: its bond slashed (`slashed`), or removed by a private colony
 *   withdrawn  out for good: it took its bond back
 *
 * An address out for good can never come back to this colony: its record
 * cannot be created twice.
 */
export type State = "none" | "qualified" | "probation" | "pool" | "suspended" | "revoked" | "withdrawn";

export interface Status {
  state: State;
  colony: {
    admission: Admission;
    authority: PublicKey;
    /** What a validator joining now puts up, in protocol-token base units. */
    bond: bigint;
    entryProofRequired: boolean;
    probationValidations: number;
  };
  /** How an address with no record may get in: its own entry proof, the authority's attestation, or both. */
  doors: ("proof" | "attestation")[];
  record: null | {
    entry: "attested" | "proved";
    qualifiedAt: bigint;
    /** What it put up, which is what it gets back; zero once withdrawn or slashed. */
    bond: bigint;
    trusted: boolean;
    /** Answers it opened, which is what ends probation. */
    revealed: number;
    /** Selections missed in a row. */
    absences: number;
    /** Commitments not yet opened, any one of which keeps the bond in. */
    sealed: number;
    faults: number;
  };
  /** Answers before its votes count; zero once off probation. */
  probationLeft: number;
  /** Misses in a row before a suspension. */
  absencesLeft: number;
  /** Revoked by a slash rather than by a private colony's authority. */
  slashed: boolean;
}

export interface ValidatorActions {
  validatorAddress(colony: PublicKey, address: PublicKey): PublicKey;
  status(address: PublicKey, colony: PublicKey): Promise<Status>;
  qualify(authority: PublicKey, colony: PublicKey, address: PublicKey): Promise<TransactionInstruction[]>;
  prove(validator: PublicKey, objective: PublicKey, answer: Uint8Array, branch: Uint8Array[]): Promise<TransactionInstruction[]>;
  /** Which of `samples` the program gives `address` on `objective`. */
  sampleOf(address: PublicKey, objective: PublicKey, samples: Samples): { index: number; input: string; artifacts?: string[] };
  /** `prove` from the result as written, checked against the published leaf first. */
  proveResult(validator: PublicKey, objective: PublicKey, samples: Samples, result: string): Promise<TransactionInstruction[]>;
  bond(validator: PublicKey, colony: PublicKey): Promise<TransactionInstruction[]>;
  rejoin(validator: PublicKey, colony: PublicKey): Promise<TransactionInstruction[]>;
  recordAbsence(caller: PublicKey, claim: PublicKey, validator: PublicKey): Promise<TransactionInstruction[]>;
  slash(caller: PublicKey, claim: PublicKey, validator: PublicKey): Promise<TransactionInstruction[]>;
}

export function validatorActions(program: Program): ValidatorActions {
  const pda = (...seeds: Buffer[]) => PublicKey.findProgramAddressSync(seeds, program.programId)[0];
  const record = (colony: PublicKey, address: PublicKey) => pda(Buffer.from("validator"), colony.toBuffer(), address.toBuffer());
  const validation = (claim: PublicKey, validator: PublicKey) => pda(Buffer.from("validation"), claim.toBuffer(), validator.toBuffer());
  const accounts = program.account as any;
  const { ata, protocolToken } = tokens(program);
  const variant = (e: object) => Object.keys(e)[0];

  async function prove(validator: PublicKey, objective: PublicKey, answer: Uint8Array, branch: Uint8Array[]) {
    if (answer.length !== 32) throw new Error(`an answer is 32 bytes; this one is ${answer.length}`);
    const o = await accounts.objective.fetch(objective);
    return [await program.methods.submitEntryProof(Array.from(answer), branch.map(b => Array.from(b))).accountsPartial({
      validator, colony: o.colony, objective, record: record(o.colony, validator),
    }).instruction()];
  }

  return {
    validatorAddress: record,

    async status(address, colony) {
      const [c, v] = await Promise.all([accounts.colony.fetch(colony), accounts.validator.fetchNullable(record(colony, address))]);
      const admission = variant(c.admission) as Admission;
      const terms = {
        admission, authority: c.admissionAuthority as PublicKey, bond: BigInt(c.bond.toString()),
        entryProofRequired: c.entryProofRequired as boolean, probationValidations: c.probationValidations as number,
      };
      const doors: Status["doors"] = [];
      if (admission === "public") doors.push("proof");
      if (!terms.entryProofRequired) doors.push("attestation");
      if (!v) return { state: "none", colony: terms, doors, record: null, probationLeft: terms.probationValidations, absencesLeft: MAX_ABSENCES, slashed: false };

      const r = {
        entry: variant(v.entry) as "attested" | "proved", qualifiedAt: BigInt(v.qualifiedAt.toString()),
        bond: BigInt(v.bond.toString()), trusted: v.trusted as boolean, revealed: v.selectedAndRevealed as number,
        absences: v.absences as number, sealed: v.sealed as number, faults: v.faults as number,
      };
      const probationLeft = Math.max(0, terms.probationValidations - r.revealed);
      const state: State = v.withdrawn ? "withdrawn" : v.revoked ? "revoked" : v.suspended ? "suspended"
        : !v.activated ? "qualified" : probationLeft > 0 ? "probation" : "pool";
      // A slash is the one thing that records a fault, and it removes the validator.
      return { state, colony: terms, doors, record: r, probationLeft, absencesLeft: Math.max(0, MAX_ABSENCES - r.absences), slashed: v.revoked && r.faults > 0 };
    },

    async qualify(authority, colony, address) {
      return [await program.methods.qualify(address).accountsPartial({
        authority, colony, validator: record(colony, address),
      }).instruction()];
    },

    prove,

    sampleOf(address, objective, samples) {
      return samples.items[sampleFor(address.toBytes(), objective.toBytes(), samples.items.length)];
    },

    async proveResult(validator, objective, samples, result) {
      const p = proofFor(samples, validator.toBytes(), objective.toBytes(), result);
      if (!p.matches) throw new Error(`that is not the answer to sample ${p.index + 1} of ${samples.items.length}: its hash is not the published leaf`);
      return prove(validator, objective, p.answer, p.branch);
    },

    async bond(validator, colony) {
      const token = await protocolToken();
      if (!token) throw new Error("the protocol token does not exist on this chain, so no bond can be posted");
      return [await program.methods.postBond().accountsPartial({
        validator, colony, record: record(colony, validator), tokenConfig: token.config, mint: token.mint,
        source: ata(validator, validator, token.mint, token.tokenProgram).address,
        bond: pda(Buffer.from("bond"), colony.toBuffer(), validator.toBuffer()), tokenProgram: token.tokenProgram,
      }).instruction()];
    },

    async rejoin(validator, colony) {
      return [await program.methods.rejoin().accountsPartial({ validator, colony, record: record(colony, validator) }).instruction()];
    },

    async recordAbsence(caller, claim, validator) {
      const objective = (await accounts.claim.fetch(claim)).objective as PublicKey;
      const colony = (await accounts.objective.fetch(objective)).colony as PublicKey;
      return [await program.methods.recordAbsence().accountsPartial({
        caller, colony, objective, claim, record: record(colony, validator), validation: validation(claim, validator),
      }).instruction()];
    },

    async slash(caller, claim, validator) {
      const objective = (await accounts.claim.fetch(claim)).objective as PublicKey;
      const colony = (await accounts.objective.fetch(objective)).colony as PublicKey;
      const token = await protocolToken();
      if (!token) throw new Error("the protocol token does not exist on this chain, so no bond was posted to burn");
      return [await program.methods.slashBond().accountsPartial({
        caller, colony, objective, claim, validation: validation(claim, validator), record: record(colony, validator),
        tokenConfig: token.config, mint: token.mint, bond: pda(Buffer.from("bond"), colony.toBuffer(), validator.toBuffer()),
        tokenProgram: token.tokenProgram,
      }).instruction()];
    },
  };
}
