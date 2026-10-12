/**
 * What a validator does to a claim, as the instructions that do it: commit,
 * then reveal (docs/PLATFORM.md section 2, "Commit and reveal").
 *
 * Each reads what it needs from the chain — the claim's objective and colony,
 * the validator's record there — and returns instructions for a wallet to
 * sign. Whether the validator was selected, whether the window is open, and
 * whether the reveal opens the commitment are the program's to refuse.
 *
 * What is committed is computed here from the message and the nonce, by
 * `commitmentOf`, and `commit` takes the result rather than the message: the
 * caller has to have kept the message and nonce before it can ask for the
 * transaction, which is the order that does not lose a bond.
 */
import { PublicKey, SYSVAR_SLOT_HASHES_PUBKEY, type TransactionInstruction } from "@solana/web3.js";
import type { Program } from "@anchor-lang/core";
import { hexToBytes } from "@noble/hashes/utils";
import { commitmentOf, revealArgs, type ValidationMessage } from "./finding";

export interface ValidationActions {
  /** The commitment `validator` would make to `message` on `claim`, hex. */
  commitment(claim: PublicKey, validator: PublicKey, message: ValidationMessage, nonce: string): string;
  /** `commit_validation`, for a commitment already computed and kept. */
  commit(claim: PublicKey, validator: PublicKey, commitment: string): Promise<TransactionInstruction[]>;
  /** `reveal_validation`: open the commitment with the message and nonce it was made from. */
  reveal(claim: PublicKey, validator: PublicKey, message: ValidationMessage, nonce: string): Promise<TransactionInstruction[]>;
}

export function validationActions(program: Program): ValidationActions {
  const pda = (...seeds: Buffer[]) => PublicKey.findProgramAddressSync(seeds, program.programId)[0];

  /** The accounts both instructions share. */
  async function accounts(claim: PublicKey, validator: PublicKey) {
    const c = await (program.account as any).claim.fetch(claim);
    const o = await (program.account as any).objective.fetch(c.objective);
    return {
      validator, claim, objective: c.objective as PublicKey, colony: o.colony as PublicKey,
      record: pda(Buffer.from("validator"), o.colony.toBuffer(), validator.toBuffer()),
      validation: pda(Buffer.from("validation"), claim.toBuffer(), validator.toBuffer()),
    };
  }

  return {
    commitment: (claim, validator, message, nonce) => commitmentOf(claim.toBytes(), validator.toBytes(), message, nonce),

    async commit(claim, validator, commitment) {
      const a = await accounts(claim, validator);
      return [await program.methods.commitValidation(Array.from(hexToBytes(commitment)))
        .accountsPartial({ ...a, slotHashes: SYSVAR_SLOT_HASHES_PUBKEY }).instruction()];
    },

    async reveal(claim, validator, message, nonce) {
      const a = await accounts(claim, validator);
      return [await (program.methods.revealValidation as any)(...revealArgs(message, nonce))
        .accountsPartial(a).instruction()];
    },
  };
}
