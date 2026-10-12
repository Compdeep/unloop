/**
 * What a person can do to a claim, as the instructions that do it
 * (docs/PLATFORM.md section 2, "What a person can do"):
 *
 *   file      prepare the body and its hash, then record_claim with the
 *             author's own fee ceiling. The body is published after, never
 *             before: whoever reads it first could record it first.
 *   seed      select_validators, anybody's: fixes who judges the claim, and
 *             must be sent before the claim's block leaves the chain's
 *             history, about three and a half minutes after filing
 *   expire    expire_claim, anybody's, once nobody can seed it any more
 *   award     award_claim — no amount, the program computes it
 *   collect   collect, the agent draining its escrow for an objective
 *   fee       collect_validation_fee, a validator's share once resolved
 *   refund    refund_validation_fee, the claimant's, where nothing counted
 *   resolve   resolve_claim — the settlement authority's at any time,
 *             anybody's once the window (and for an undecided claim, the
 *             grace after it) has passed
 *   edge      mint_edge_reward, pulled by either end of a confirmed edge
 *   unbond    withdraw_bond, a validator's whole bond in a colony, which
 *             ends its eligibility there
 *
 * Each reads what it needs from the chain and returns instructions for a
 * wallet to sign; a token account the action pays into is opened first if it
 * is not there. Who may do what, and when, is the program's to refuse.
 */
import { PublicKey, SYSVAR_SLOT_HASHES_PUBKEY, type TransactionInstruction } from "@solana/web3.js";
import { BN, type Program } from "@anchor-lang/core";
import { ACCOUNT_SIZE, getAssociatedTokenAddressSync, getMint, unpackAccount } from "@solana/spl-token";
import { tokens } from "./tokens";
import { checkBody, claimHashOf, type ClaimBody, type Mode } from "./claim-body";
import { seedWindow, type SeedWindow } from "./claim";

/** A claim ready to file: the body with its hash in it, and the address it will have. */
export interface Prepared {
  body: ClaimBody & { objective: string; awardIndex: number; claimHash: string; roots: string[] };
  /** hex */
  hash: string;
  claim: PublicKey;
}

/** What filing costs in validation, read from the colony now. */
export interface Fee {
  /** The colony's fee per validator times the objective's validators per claim, in the protocol token's base units. */
  amount: bigint;
  /** The protocol token and its decimals; null where no token exists yet, which only a free colony can do without. */
  mint: PublicKey | null;
  decimals: number | null;
}

/**
 * What an author holds toward filing, read now: SOL for the claim account's
 * rent and the transaction, and the protocol token for the fee.
 */
/** Lamports per signature, Solana's base fee. */
const SIGNATURE_FEE = 5000n;

export interface Means {
  lamports: bigint;
  /**
   * The most SOL one filing takes: the claim account's rent, which is what
   * filing costs and is never returned, the signature, and where the
   * protocol token exists, the rent of the objective's fees account, which
   * its first fee-paying claim opens.
   */
  filing: bigint;
  /** The author's balance of the protocol token and its decimals; null where no token exists yet. */
  token: { mint: PublicKey; amount: bigint; decimals: number } | null;
}

/**
 * Where a claim's seed stands: fixed already, decided without one (the
 * settlement authority may resolve at any time), or its window as
 * `seedWindow` reads it from the chain now.
 */
export type Seeding = { state: "fixed" } | { state: "settled" } | SeedWindow;

export interface Settlement { admit: boolean; noveltyBps: number; scoreBps: number; rootsConfirmed: number }

export interface ClaimActions {
  fee(objective: PublicKey): Promise<Fee>;
  means(author: PublicKey): Promise<Means>;
  /**
   * The body as it will be filed: `claimHash` computed under the objective's
   * `mode` and written in, and checked the way a validator will check it, so
   * a body that would not verify against its own account is refused here.
   */
  prepare(agent: PublicKey, body: ClaimBody, mode: Mode): Prepared;
  /** `record_claim` for a prepared body, refused by the program if the fee has risen above `maxFee`. */
  file(agent: PublicKey, prepared: Prepared, maxFee: bigint): Promise<TransactionInstruction[]>;
  seeding(claim: PublicKey): Promise<Seeding>;
  /** `select_validators`: the seed and the pool, fixed once, by anybody. */
  seed(caller: PublicKey, claim: PublicKey): Promise<TransactionInstruction[]>;
  /** `expire_claim`: reject a claim whose seed can never be fixed. */
  expire(caller: PublicKey, claim: PublicKey): Promise<TransactionInstruction[]>;
  award(caller: PublicKey, claim: PublicKey): Promise<TransactionInstruction[]>;
  collect(agent: PublicKey, objective: PublicKey): Promise<TransactionInstruction[]>;
  collectFee(validator: PublicKey, claim: PublicKey): Promise<TransactionInstruction[]>;
  refundFee(agent: PublicKey, claim: PublicKey): Promise<TransactionInstruction[]>;
  resolve(caller: PublicKey, claim: PublicKey, s: Settlement): Promise<TransactionInstruction[]>;
  edge(caller: PublicKey, claim: PublicKey, rootIndex: number): Promise<TransactionInstruction[]>;
  withdrawBond(validator: PublicKey, colony: PublicKey): Promise<TransactionInstruction[]>;
}

export function claimActions(program: Program): ClaimActions {
  const pda = (...seeds: (Buffer | Uint8Array)[]) => PublicKey.findProgramAddressSync(seeds, program.programId)[0];
  const seed = (s: string) => Buffer.from(s);
  const connection = program.provider.connection;
  const account = (name: string, address: PublicKey) => (program.account as any)[name].fetch(address);

  const { tokenProgramOf, ata, protocolToken } = tokens(program);
  async function claimAndObjective(claim: PublicKey) {
    const c = await account("claim", claim);
    return { c, o: await account("objective", c.objective), objective: c.objective as PublicKey };
  }
  async function fee(objective: PublicKey): Promise<Fee> {
    const o = await account("objective", objective);
    const colony = await account("colony", o.colony);
    const amount = BigInt(colony.validationFee.toString()) * BigInt(o.validatorsPerClaim);
    const token = await protocolToken();
    if (!token) return { amount, mint: null, decimals: null };
    const { decimals } = await getMint(connection, token.mint, "confirmed", token.tokenProgram);
    return { amount, mint: token.mint, decimals };
  }
  const address = (s: string, what: string) => {
    try { return new PublicKey(s); } catch { throw new Error(`${what} is not an address: ${JSON.stringify(s)}`); }
  };

  return {
    fee,

    async means(author) {
      const info = (k: PublicKey) => connection.getAccountInfo(k, "confirmed");
      const lamports = BigInt((await info(author))?.lamports ?? 0);
      const rent = async (n: number) => BigInt(await connection.getMinimumBalanceForRentExemption(n));
      const claim = (await rent((program.account as any).claim.size)) + SIGNATURE_FEE;
      const token = await protocolToken();
      // Without the token no fee is paid, and no fees account is opened.
      if (!token) return { lamports, filing: claim, token: null };
      const filing = claim + (await rent(ACCOUNT_SIZE));
      const { decimals } = await getMint(connection, token.mint, "confirmed", token.tokenProgram);
      const address = ata(author, author, token.mint, token.tokenProgram).address;
      const held = await info(address);
      const amount = held ? unpackAccount(address, held, token.tokenProgram).amount : 0n;
      return { lamports, filing, token: { mint: token.mint, amount, decimals } };
    },

    prepare(agent, body, mode) {
      const objective = address(String(body.objective), "objective").toBase58();
      if (!Number.isInteger(body.awardIndex)) throw new Error("awardIndex must be a whole number");
      const roots = (body.roots ?? []) as unknown[];
      if (!Array.isArray(roots)) throw new Error("roots must be a list of claim addresses");
      const rootList = roots.map(r => address(String(r), "a root").toBase58());
      if (rootList.length > 8) throw new Error("a claim declares at most 8 roots");
      if (new Set(rootList).size !== rootList.length) throw new Error("a root is declared twice");

      const { claimHash: _old, ...rest } = body;
      const fields = { ...rest, objective, awardIndex: body.awardIndex as number, roots: rootList };
      const hash = claimHashOf(fields, mode);
      // Keys in the order a reader expects them, the rest after.
      const { objective: _o, awardIndex: _a, ...more } = fields;
      const sealed = { objective, awardIndex: fields.awardIndex, claimHash: hash, ...more } as Prepared["body"];
      const check = checkBody(sealed, { objective, hash, awardIndex: fields.awardIndex, roots: rootList }, mode);
      if (!check.verified) throw new Error(`this body would not verify against its own claim: ${check.differences.join("; ")}`);
      const claim = pda(seed("claim"), new PublicKey(objective).toBuffer(), Buffer.from(hash, "hex"), agent.toBuffer());
      return { body: sealed, hash, claim };
    },

    async file(agent, p, maxFee) {
      const objective = new PublicKey(p.body.objective);
      const o = await account("objective", objective);
      const { amount } = await fee(objective);
      // The fee accounts ride along only where the colony charges: a free
      // colony must not need the protocol token to exist.
      let feeAccounts: Record<string, PublicKey | null> = { tokenConfig: null, mint: null, source: null, fees: null, tokenProgram: null };
      if (amount > 0n) {
        const token = await protocolToken();
        if (!token) throw new Error("this colony charges a validation fee, and the protocol token does not exist on this chain");
        feeAccounts = { tokenConfig: token.config, mint: token.mint, fees: pda(seed("fees"), objective.toBuffer()),
          source: getAssociatedTokenAddressSync(token.mint, agent, true, token.tokenProgram), tokenProgram: token.tokenProgram };
      }
      return [await program.methods
        .recordClaim(Array.from(Buffer.from(p.hash, "hex")), p.body.awardIndex,
          p.body.roots.map(r => Array.from(new PublicKey(r).toBytes())), new BN(maxFee.toString()))
        .accountsPartial({ agent, objective, claim: p.claim, colony: o.colony, ...feeAccounts })
        .remainingAccounts(p.body.roots.map(r => ({ pubkey: new PublicKey(r), isWritable: false, isSigner: false })))
        .instruction()];
    },

    async seeding(claim) {
      const c = await account("claim", claim);
      if (BigInt(c.selectionSeed.toString()) !== 0n) return { state: "fixed" };
      if (!c.status.pending) return { state: "settled" };
      const history = await connection.getAccountInfo(SYSVAR_SLOT_HASHES_PUBKEY, "confirmed");
      if (!history) throw new Error("this chain serves no SlotHashes, so no claim's seed can be read");
      return seedWindow(history.data, c.claimSlot);
    },

    async seed(caller, claim) {
      const { o, objective } = await claimAndObjective(claim);
      return [await program.methods.selectValidators().accountsPartial({
        payer: caller, claim, objective, colony: o.colony, slotHashes: SYSVAR_SLOT_HASHES_PUBKEY,
      }).instruction()];
    },

    async expire(caller, claim) {
      const { objective } = await claimAndObjective(claim);
      return [await program.methods.expireClaim().accountsPartial({
        caller, objective, claim, slotHashes: SYSVAR_SLOT_HASHES_PUBKEY,
      }).instruction()];
    },

    async award(caller, claim) {
      const { c, o, objective } = await claimAndObjective(claim);
      return [await program.methods.awardClaim().accountsPartial({
        caller, objective, claim, escrow: pda(seed("escrow"), objective.toBuffer(), c.agent.toBuffer()),
        vault: pda(seed("vault"), objective.toBuffer()), paymentMint: o.paymentMint,
        tokenProgram: await tokenProgramOf(o.paymentMint),
      }).instruction()];
    },

    async collect(agent, objective) {
      const o = await account("objective", objective);
      const config = await account("config", pda(seed("config")));
      const tokenProgram = await tokenProgramOf(o.paymentMint);
      // Both destinations are opened if they are not there: the agent's own,
      // and the treasury's, which a first collection in a new mint finds missing.
      const mine = ata(agent, agent, o.paymentMint, tokenProgram);
      const treasury = ata(agent, config.treasury, o.paymentMint, tokenProgram);
      return [mine.open, treasury.open, await program.methods.collect().accountsPartial({
        agent, config: pda(seed("config")), objective, escrow: pda(seed("escrow"), objective.toBuffer(), agent.toBuffer()),
        paymentMint: o.paymentMint, destination: mine.address, treasuryAccount: treasury.address,
        buybackAccount: pda(seed("buyback"), o.paymentMint.toBuffer()), tokenProgram,
      }).instruction()];
    },

    async collectFee(validator, claim) {
      const { o, objective } = await claimAndObjective(claim);
      const token = await protocolToken();
      if (!token) throw new Error("the protocol token does not exist on this chain, so no fee was paid");
      const mine = ata(validator, validator, token.mint, token.tokenProgram);
      return [mine.open, await program.methods.collectValidationFee().accountsPartial({
        validator, objective, claim, validation: pda(seed("validation"), claim.toBuffer(), validator.toBuffer()),
        record: pda(seed("validator"), o.colony.toBuffer(), validator.toBuffer()),
        tokenConfig: token.config, mint: token.mint, fees: pda(seed("fees"), objective.toBuffer()),
        destination: mine.address, tokenProgram: token.tokenProgram,
      }).instruction()];
    },

    async refundFee(agent, claim) {
      const { objective } = await claimAndObjective(claim);
      const token = await protocolToken();
      if (!token) throw new Error("the protocol token does not exist on this chain, so no fee was paid");
      const mine = ata(agent, agent, token.mint, token.tokenProgram);
      return [mine.open, await program.methods.refundValidationFee().accountsPartial({
        agent, objective, claim, tokenConfig: token.config, mint: token.mint, fees: pda(seed("fees"), objective.toBuffer()),
        destination: mine.address, tokenProgram: token.tokenProgram,
      }).instruction()];
    },

    async resolve(caller, claim, s) {
      const { objective } = await claimAndObjective(claim);
      return [await program.methods.resolveClaim(s.admit, s.noveltyBps, s.scoreBps, s.rootsConfirmed)
        .accountsPartial({ authority: caller, objective, claim }).instruction()];
    },

    async edge(caller, claim, rootIndex) {
      const { c, o, objective } = await claimAndObjective(claim);
      if (!(rootIndex >= 0 && rootIndex < c.rootCount)) throw new Error(`this claim declares ${c.rootCount} roots; there is no root ${rootIndex}`);
      const root = new PublicKey(c.roots[rootIndex]);
      const r = await account("claim", root);
      const token = await protocolToken();
      if (!token) throw new Error("the protocol token does not exist on this chain, so nothing can be minted");
      const rootAccount = ata(caller, r.agent, token.mint, token.tokenProgram);
      const citerAccount = ata(caller, c.agent, token.mint, token.tokenProgram);
      return [rootAccount.open, citerAccount.open, await program.methods.mintEdgeReward(rootIndex).accountsPartial({
        caller, tokenConfig: token.config, mint: token.mint, objective, mintable: pda(seed("mintable"), o.colony.toBuffer()),
        claim, root, rootAccount: rootAccount.address, citerAccount: citerAccount.address, tokenProgram: token.tokenProgram,
      }).instruction()];
    },

    async withdrawBond(validator, colony) {
      const token = await protocolToken();
      if (!token) throw new Error("the protocol token does not exist on this chain, so no bond was posted");
      const mine = ata(validator, validator, token.mint, token.tokenProgram);
      return [mine.open, await program.methods.withdrawBond().accountsPartial({
        validator, colony, record: pda(seed("validator"), colony.toBuffer(), validator.toBuffer()),
        tokenConfig: token.config, mint: token.mint, bond: pda(seed("bond"), colony.toBuffer(), validator.toBuffer()),
        destination: mine.address, tokenProgram: token.tokenProgram,
      }).instruction()];
    },
  };
}
