/**
 * What a colony's leader and its admission authority do to it, as the
 * instructions that do it: the leader creates it, and later changes its
 * policy and reprices its bond and its validation fee; the admission
 * authority chooses its trusted validators and, in a private colony, removes
 * one (docs/PLATFORM.md sections 4 and 5).
 *
 * `create` takes the settings the Safety calculator chose (client/safety.ts)
 * and the terms beside them; the program checks them — a policy that can
 * decide something, and for a public colony the floor and a bond — and a
 * refusal arrives in its words. `updatePolicy` is checked the same way, under
 * the same floor. Nothing here holds a key or sends anything.
 */
import { PublicKey, type TransactionInstruction } from "@solana/web3.js";
import { BN, type Program } from "@anchor-lang/core";

export type Admission = "public" | "keyed" | "private";

/** The colony's consensus policy: what an objective created here copies and is settled by. */
export interface Policy {
  minVotes: number;
  acceptThresholdBps: number;
  rejectThresholdBps: number;
}

export interface ColonyTerms {
  slug: string;
  /** The currency its objectives pay in. */
  paymentMint: string;
  admission: Admission;
  /** Who records a qualification. Not necessarily the leader. */
  authority: string;
  /** Protocol-token base units a validator puts at risk. A public colony must ask for one. */
  bond: bigint;
  /** Protocol-token base units one validator is paid for judging one claim. */
  validationFee: bigint;
  minVotes: number;
  acceptThresholdBps: number;
  rejectThresholdBps: number;
  /** Answers a new validator reveals before they count and are paid. */
  probationValidations: number;
}

/** `MAX_SLUG_LEN`: the slug is the colony's address seed, in bytes. */
export const MAX_SLUG_BYTES = 32;

export interface ColonyActions {
  /** The address a colony with this slug has: one per slug, ever. */
  colonyAddress(slug: string): PublicKey;
  createColony(leader: PublicKey, terms: ColonyTerms): Promise<TransactionInstruction[]>;
  /**
   * `update_colony`: the policy objectives created from now on copy. One
   * already created keeps the policy it copied, so no verdict changes rules.
   */
  updatePolicy(leader: PublicKey, colony: PublicKey, policy: Policy): Promise<TransactionInstruction[]>;
  /** `set_validation_fee`: what a claim filed from now on pays each validator. A claim already filed keeps what it paid. */
  setValidationFee(leader: PublicKey, colony: PublicKey, fee: bigint): Promise<TransactionInstruction[]>;
  /** `set_bond`: what validators who join from now on put up. */
  setBond(leader: PublicKey, colony: PublicKey, bond: bigint): Promise<TransactionInstruction[]>;
  /** `set_trusted`, the admission authority's: a validator picked for every claim, or not. */
  setTrusted(authority: PublicKey, colony: PublicKey, validator: PublicKey, trusted: boolean): Promise<TransactionInstruction[]>;
  /**
   * `revoke_qualification`, a private colony's admission authority's: the
   * validator out for good, its bond still its own to withdraw. Refused in a
   * keyed or public colony, where only a proven fault removes anybody.
   */
  revoke(authority: PublicKey, colony: PublicKey, validator: PublicKey): Promise<TransactionInstruction[]>;
}

export function colonyActions(program: Program): ColonyActions {
  const pda = (...seeds: Buffer[]) => PublicKey.findProgramAddressSync(seeds, program.programId)[0];
  const address = (slug: string) => pda(Buffer.from("colony"), Buffer.from(slug));
  const record = (colony: PublicKey, validator: PublicKey) => pda(Buffer.from("validator"), colony.toBuffer(), validator.toBuffer());
  const key = (s: string, what: string) => {
    try { return new PublicKey(s); } catch { throw new Error(`${what} is not an address: ${JSON.stringify(s)}`); }
  };

  return {
    colonyAddress: address,

    async createColony(leader, t) {
      const bytes = Buffer.byteLength(t.slug);
      if (bytes === 0) throw new Error("a colony needs a slug");
      if (bytes > MAX_SLUG_BYTES) throw new Error(`a slug is at most ${MAX_SLUG_BYTES} bytes; this one is ${bytes}`);
      const colony = address(t.slug);
      if (await program.provider.connection.getAccountInfo(colony)) throw new Error(`a colony called "${t.slug}" already exists`);
      return [await program.methods.createColony(t.slug,
        { minVotes: t.minVotes, acceptThresholdBps: t.acceptThresholdBps, rejectThresholdBps: t.rejectThresholdBps },
        { admission: { [t.admission]: {} }, authority: key(t.authority, "the admission authority"),
          bond: new BN(t.bond.toString()), validationFee: new BN(t.validationFee.toString()),
          entryProofRequired: false, probationValidations: t.probationValidations })
        .accountsPartial({ leader, colony, paymentMint: key(t.paymentMint, "the payment mint") }).instruction()];
    },

    async updatePolicy(leader, colony, p) {
      return [await program.methods.updateColony(
        { minVotes: p.minVotes, acceptThresholdBps: p.acceptThresholdBps, rejectThresholdBps: p.rejectThresholdBps })
        .accountsPartial({ leader, colony }).instruction()];
    },

    async setValidationFee(leader, colony, fee) {
      return [await program.methods.setValidationFee(new BN(fee.toString())).accountsPartial({ leader, colony }).instruction()];
    },

    async setBond(leader, colony, bond) {
      return [await program.methods.setBond(new BN(bond.toString())).accountsPartial({ leader, colony }).instruction()];
    },

    async setTrusted(authority, colony, validator, trusted) {
      return [await program.methods.setTrusted(trusted).accountsPartial({ authority, colony, record: record(colony, validator) }).instruction()];
    },

    async revoke(authority, colony, validator) {
      return [await program.methods.revokeQualification().accountsPartial({ authority, colony, record: record(colony, validator) }).instruction()];
    },
  };
}
