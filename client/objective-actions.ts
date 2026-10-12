/**
 * What a person can do to an objective, as the instructions that do it:
 * create, fund, withdraw, close, and repoint its document (docs/PLATFORM.md
 * sections 1 and 5).
 *
 * One implementation, so the page and anything else that acts for a person
 * cannot differ about which accounts an action needs. Each action takes the
 * objective and who signs, reads the rest from the chain — the colony, the
 * mint, which token program owns the mint — and returns instructions for a
 * wallet to sign. Nothing here holds a key or sends anything.
 *
 * Whether the action is allowed is the program's question: a withdrawal over
 * what the account calls withdrawable, or a close by somebody who is not the
 * leader, is built here and refused there.
 *
 * `create` is the one with a sequence, and it is here so that it exists once:
 * the terms are derived from the document rather than entered beside it, so
 * the hashes signed are the document's and every field the account copies
 * agrees with it by construction (`checkSpec` is the test of that).
 */
import { PublicKey, type TransactionInstruction } from "@solana/web3.js";
import { BN, type Program } from "@anchor-lang/core";
import { getAssociatedTokenAddressSync, createAssociatedTokenAccountIdempotentInstruction } from "@solana/spl-token";
import { specHashes, seconds, type PublishBody } from "./spec";
import { setProblems, type Samples } from "./samples";

/** An objective as its leader drafts it: the document, and what the chain holds beside it. */
export interface Draft {
  /**
   * The `objective.publish` body, `id` included. Its payout, validatorsPerClaim
   * and two deadlines become the account's, and its hashes the commitments.
   */
  body: PublishBody;
  /** What each award pays, in body.awards' order. The text of each is in the body; the money never is. */
  awards: { mode: "winnerTakeAll" | "graded"; allocation: bigint }[];
  /** Unix seconds. The account holds it and the document does not. */
  commitDeadline: number;
  /** Put in the vault at creation, from the leader's own account. */
  pool: bigint;
  /** Where the body can be fetched; empty for bus-only. */
  uri: string;
}

/** `MAX_URI_LEN`: an objective's URI, in bytes. */
export const MAX_URI_BYTES = 200;

export interface ObjectiveActions {
  /** The address the colony's next objective will have, which the document names before it exists. */
  next(colony: PublicKey): Promise<PublicKey>;
  /**
   * `create_objective` for `draft`, at `objective` — which must still be the
   * colony's next, or the program refuses it, so a document naming one address
   * cannot commit at another.
   */
  create(colony: PublicKey, leader: PublicKey, objective: PublicKey, draft: Draft): Promise<TransactionInstruction[]>;
  /** `top_up_objective`: anybody may add to a pool, from their own token account for its mint. */
  fund(objective: PublicKey, payer: PublicKey, amount: bigint): Promise<TransactionInstruction[]>;
  /** `withdraw_unencumbered`, to the leader's own token account, opened first if it is not there. */
  withdraw(objective: PublicKey, leader: PublicKey, amount: bigint): Promise<TransactionInstruction[]>;
  /** `close_objective`: no more claims. Pending ones still settle. */
  close(objective: PublicKey, leader: PublicKey, cancelled: boolean): Promise<TransactionInstruction[]>;
  /**
   * `set_objective_uri`: where its document can be fetched, empty for
   * nowhere. Nothing the objective committed to moves; a reader checks
   * whatever this names against the hashes.
   */
  setUri(objective: PublicKey, leader: PublicKey, uri: string): Promise<TransactionInstruction[]>;
}

export function objectiveActions(program: Program): ObjectiveActions {
  const vault = (objective: PublicKey) =>
    PublicKey.findProgramAddressSync([Buffer.from("vault"), objective.toBuffer()], program.programId)[0];

  /** The objective's colony and mint, and the token program the mint belongs to — Token or Token-2022. */
  async function read(objective: PublicKey) {
    const o = await (program.account as any).objective.fetch(objective);
    const mint: PublicKey = o.paymentMint;
    const info = await program.provider.connection.getAccountInfo(mint);
    if (!info) throw new Error(`the objective's mint ${mint.toBase58()} is not on this chain`);
    return { colony: o.colony as PublicKey, mint, tokenProgram: info.owner };
  }

  const hashBytes = (hex: string) => Array.from(Buffer.from(hex, "hex"));
  const u64 = (n: bigint) => new BN(n.toString());

  return {
    async next(colony) {
      const c = await (program.account as any).colony.fetch(colony);
      const index = Buffer.alloc(8);
      index.writeBigUInt64LE(BigInt(c.objectiveCount.toString()));
      return PublicKey.findProgramAddressSync([Buffer.from("objective"), colony.toBuffer(), index], program.programId)[0];
    },

    async create(colony, leader, objective, draft) {
      const { body } = draft;
      if (body.id !== objective.toBase58()) throw new Error(`the document names ${body.id}, not ${objective.toBase58()}`);
      // A set is committed by its root and count, and a set nobody could
      // prove an answer to is a door drawn on a wall: its leaves must be
      // published and rebuild the root (client/samples.ts, `sampleSet`).
      if (body.samples) {
        const problems = setProblems(body.samples as Samples);
        if (problems.length) throw new Error(`the document's samples: ${problems.join("; ")}`);
      }
      if (body.payout !== "prize" && body.payout !== "rate") throw new Error("the document's payout must be prize or rate");
      if (typeof body.validatorsPerClaim !== "number") throw new Error("the document must say how many validators a claim selects");
      if (!body.claimDeadline || !body.validationDeadline) throw new Error("the document must carry both deadlines");
      if (body.awards?.length !== draft.awards.length) throw new Error("every award needs its text in the document and its amount here");
      const declared = (body as { criteriaHash?: string }).criteriaHash;
      const { criteriaHash, claimFormHash } = specHashes(body);
      if (declared !== undefined && declared !== criteriaHash) throw new Error("the document's criteriaHash is not the hash of its criteria");

      const c = await (program.account as any).colony.fetch(colony);
      const mint: PublicKey = c.paymentMint;
      const info = await program.provider.connection.getAccountInfo(mint);
      if (!info) throw new Error(`the colony's mint ${mint.toBase58()} is not on this chain`);
      const tokenProgram = info.owner;

      const terms = {
        payout: { [body.payout]: {} },
        criteriaHash: hashBytes(criteriaHash),
        claimFormHash: hashBytes(claimFormHash),
        entryProofRoot: body.samples ? hashBytes(body.samples.root!) : new Array(32).fill(0),
        entryProofCount: body.samples ? (body.samples.items as unknown[]).length : 0,
        uri: draft.uri,
        awards: draft.awards.map(a => ({ mode: { [a.mode]: {} }, allocation: u64(a.allocation) })),
        validatorsPerClaim: body.validatorsPerClaim,
        claimDeadline: new BN(seconds(body.claimDeadline)),
        commitDeadline: new BN(draft.commitDeadline),
        validationDeadline: new BN(seconds(body.validationDeadline)),
      };
      return [await program.methods.createObjective(terms, u64(draft.pool)).accountsPartial({
        leader, colony, objective, vault: vault(objective), paymentMint: mint,
        fundingAccount: getAssociatedTokenAddressSync(mint, leader, true, tokenProgram), tokenProgram,
      }).instruction()];
    },

    async fund(objective, payer, amount) {
      const { mint, tokenProgram } = await read(objective);
      return [await program.methods.topUpObjective(new BN(amount.toString())).accountsPartial({
        payer, objective, vault: vault(objective), paymentMint: mint,
        fundingAccount: getAssociatedTokenAddressSync(mint, payer, true, tokenProgram), tokenProgram,
      }).instruction()];
    },

    async withdraw(objective, leader, amount) {
      const { colony, mint, tokenProgram } = await read(objective);
      const destination = getAssociatedTokenAddressSync(mint, leader, true, tokenProgram);
      return [
        createAssociatedTokenAccountIdempotentInstruction(leader, destination, leader, mint, tokenProgram),
        await program.methods.withdrawUnencumbered(new BN(amount.toString())).accountsPartial({
          leader, colony, objective, vault: vault(objective), paymentMint: mint, destination, tokenProgram,
        }).instruction(),
      ];
    },

    async close(objective, leader, cancelled) {
      const { colony } = await read(objective);
      return [await program.methods.closeObjective(cancelled).accountsPartial({ leader, colony, objective }).instruction()];
    },

    async setUri(objective, leader, uri) {
      const bytes = Buffer.byteLength(uri);
      if (bytes > MAX_URI_BYTES) throw new Error(`a URI is at most ${MAX_URI_BYTES} bytes; this one is ${bytes}`);
      const { colony } = await read(objective);
      return [await program.methods.setObjectiveUri(uri).accountsPartial({ leader, colony, objective }).instruction()];
    },
  };
}
