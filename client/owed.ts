/**
 * What an address can be paid now (docs/PLATFORM.md section 3, "What is
 * owed"). Every payment in the protocol is pulled, so money earned sits in an
 * account until its earner asks; this finds it.
 *
 * The index says where to look and this says whether anything is there: each
 * candidate is read from its accounts and kept only if the instruction that
 * pays it would pay it now, by that instruction's own checks —
 *
 *   escrow   collect                  the escrow's balance, more than zero,
 *                                     less the protocol fee `Config` takes
 *   edge     mint_edge_reward         both claims admitted, the edge confirmed
 *                                     and unpaid, not self-cited, the colony
 *                                     mintable, the rate above zero, the cap
 *                                     not reached
 *   share    collect_validation_fee   the claim resolved; the validation
 *                                     evaluated, counted and unpaid; a share
 *                                     above zero
 *   refund   refund_validation_fee    the address's own claim, resolved,
 *                                     nothing validated, a fee still held
 *
 * — and a candidate that fails says which check, so a stale index is visible
 * rather than silently dropped. Tests hold each rule to its instruction.
 *
 * A bond is not owed. Taking it back ends eligibility, and `withdraw_bond`
 * refuses only while a commitment is sealed; so each bond comes back as a
 * state — free now, or held by sealed commitments — and never as a line to
 * collect with the rest.
 */
import { PublicKey } from "@solana/web3.js";
import type { Program } from "@anchor-lang/core";
import { unpackAccount } from "@solana/spl-token";
import type { ClaimActions } from "./claim-actions";

/** Where the index says to look. */
export interface Candidates {
  /** Objectives whose escrow for this address may hold something. */
  escrows: string[];
  /** Edges this address is either end of: the citing claim and the root's position in it. */
  edges: { claim: string; rootIndex: number }[];
  /** Claims this address validated. */
  shares: string[];
  /** Claims this address filed. */
  refunds: string[];
  /** Colonies this address has a validator record in. */
  bonds: string[];
}

/**
 * One payment the address can pull now. `amount` is what reaches the address:
 * for an escrow, the balance less the protocol fee, which `gross` and `fee`
 * show before anything is signed.
 */
export type Line =
  | { kind: "escrow"; objective: string; mint: string; amount: bigint; gross: bigint; fee: bigint; feeBps: number }
  | { kind: "edge"; claim: string; rootIndex: number; root: string; end: "root" | "citer"; objective: string; mint: string; amount: bigint }
  | { kind: "share"; claim: string; objective: string; mint: string; amount: bigint }
  | { kind: "refund"; claim: string; objective: string; mint: string; amount: bigint };

/** A candidate the account does not bear out, and why. */
export interface Refused { kind: Line["kind"]; at: string; reason: string }

export interface BondState {
  colony: string; mint: string; amount: bigint;
  /** Commitments still sealed: while any is, the bond cannot leave. */
  sealed: number;
  free: boolean;
}

export interface Owed { lines: Line[]; refused: Refused[]; bonds: BondState[] }

type U64 = { toString(): string };
const big = (n: U64) => BigInt(n.toString());
/** `Config::split`'s fee on `gross`: rounded down, so the recipient keeps the remainder. */
export const protocolFee = (gross: bigint, bps: number) => gross * BigInt(bps) / 10_000n;
const is = (e: object | null | undefined, variant: string) => e != null && variant in e;

export async function owed(program: Program, address: PublicKey, c: Candidates): Promise<Owed> {
  const pda = (...seeds: (Buffer | Uint8Array)[]) => PublicKey.findProgramAddressSync(seeds, program.programId)[0];
  const seed = (s: string) => Buffer.from(s);
  const connection = program.provider.connection;
  const accounts = program.account as any;
  const me = address.toBase58();
  const lines: Line[] = [], refused: Refused[] = [], bonds: BondState[] = [];
  const no = (kind: Line["kind"], at: string, reason: string) => { refused.push({ kind, at, reason }); };

  /** Several accounts of one kind, keyed by address; null where there is none. */
  async function many(name: string, addresses: PublicKey[]): Promise<Map<string, any>> {
    const unique = [...new Map(addresses.map(a => [a.toBase58(), a])).values()];
    const out = new Map<string, any>();
    for (let i = 0; i < unique.length; i += 100) {
      const chunk = unique.slice(i, i + 100);
      (await accounts[name].fetchMultiple(chunk)).forEach((x: any, j: number) => out.set(chunk[j].toBase58(), x));
    }
    return out;
  }
  /** Token balances, keyed by address; null where the account does not exist. */
  async function balances(addresses: PublicKey[]): Promise<Map<string, bigint | null>> {
    const out = new Map<string, bigint | null>();
    for (let i = 0; i < addresses.length; i += 100) {
      const chunk = addresses.slice(i, i + 100);
      (await connection.getMultipleAccountsInfo(chunk)).forEach((info, j) =>
        out.set(chunk[j].toBase58(), info ? unpackAccount(chunk[j], info, info.owner).amount : null));
    }
    return out;
  }
  const key = (s: string) => new PublicKey(s);

  const token = await accounts.tokenConfig.fetchNullable(pda(seed("token")));
  const protocolMint = token ? (token.mint as PublicKey).toBase58() : null;

  // ── escrows ──
  const config = c.escrows.length ? await accounts.config.fetchNullable(pda(seed("config"))) : null;
  const feeBps = config ? config.treasuryBps + config.buybackBps : 0;
  const objectives = await many("objective", c.escrows.map(key));
  const escrowOf = (o: string) => pda(seed("escrow"), key(o).toBuffer(), address.toBuffer());
  const escrowed = await balances(c.escrows.map(escrowOf));
  for (const o of c.escrows) {
    const obj = objectives.get(o), amount = escrowed.get(escrowOf(o).toBase58());
    if (!obj) no("escrow", o, "there is no such objective");
    else if (!amount) no("escrow", o, "the escrow is empty");
    else if (!config) no("escrow", o, "the protocol is not configured on this chain");
    else {
      const fee = protocolFee(amount, feeBps);
      lines.push({ kind: "escrow", objective: o, mint: (obj.paymentMint as PublicKey).toBase58(), amount: amount - fee, gross: amount, fee, feeBps });
    }
  }

  // ── claims, for edges, shares and refunds ──
  const claims = await many("claim", [...c.edges.map(e => e.claim), ...c.shares, ...c.refunds].map(key));
  const claimOf = (a: string) => claims.get(a) ?? null;

  // ── edges ──
  const rootAddress = (cl: any, i: number) => new PublicKey(Uint8Array.from(cl.roots[i])).toBase58();
  const edgeRoots = c.edges.flatMap(e => {
    const cl = claimOf(e.claim);
    return cl && e.rootIndex < cl.rootCount ? [key(rootAddress(cl, e.rootIndex))] : [];
  });
  const roots = await many("claim", edgeRoots);
  const colonies = await many("objective", c.edges.map(e => claimOf(e.claim)?.objective).filter(Boolean));
  const mintable = await many("mintable", [...colonies.values()].filter(Boolean).map((o: any) => pda(seed("mintable"), o.colony.toBuffer())));
  let issued = token ? big(token.minted) : 0n;
  for (const e of c.edges) {
    const at = `${e.claim}#${e.rootIndex}`;
    const cl = claimOf(e.claim);
    if (!cl) { no("edge", at, "there is no such claim"); continue; }
    if (e.rootIndex >= cl.rootCount) { no("edge", at, `the claim declares no root ${e.rootIndex}`); continue; }
    const root = roots.get(rootAddress(cl, e.rootIndex));
    const end = cl.agent.toBase58() === me ? "citer" : root?.agent.toBase58() === me ? "root" : null;
    const bit = 1 << e.rootIndex;
    const o = colonies.get(cl.objective.toBase58());
    const m = o ? mintable.get(pda(seed("mintable"), o.colony.toBuffer()).toBase58()) : null;
    const amount = !token ? 0n : end === "root" ? big(token.edgeRate) : big(token.citerRate);
    const both = token ? big(token.edgeRate) + big(token.citerRate) : 0n;
    const reason =
      !token ? "the protocol token does not exist on this chain"
      : !root ? "the root claim is not on this chain"
      : !end ? "this address is neither end of the edge"
      : !is(cl.status, "admitted") || !is(root.status, "admitted") ? "both claims must be admitted"
      : !(cl.rootsConfirmed & bit) ? "the validators did not confirm this edge"
      : cl.rootsPaid & bit ? "this edge has been paid"
      : root.agent.equals(cl.agent) ? "a claim citing its own agent pays nothing"
      : !m?.enabled ? "this colony's work does not mint"
      : big(token.edgeRate) === 0n ? "the edge rate is zero"
      : issued + both > big(token.supplyCap) ? "the supply cap is reached"
      : amount === 0n ? "the citer's rate is zero"
      : null;
    if (reason) { no("edge", at, reason); continue; }
    issued += both;    // two edges pulled together draw on the same cap
    lines.push({ kind: "edge", claim: e.claim, rootIndex: e.rootIndex, root: rootAddress(cl, e.rootIndex), end: end!,
      objective: cl.objective.toBase58(), mint: protocolMint!, amount });
  }

  // ── shares ──
  const validationOf = (claim: string) => pda(seed("validation"), key(claim).toBuffer(), address.toBuffer());
  const validations = await many("validation", c.shares.map(validationOf));
  for (const a of c.shares) {
    const cl = claimOf(a), v = validations.get(validationOf(a).toBase58());
    const share = cl && big(cl.validations) > 0n ? big(cl.validationFee) / big(cl.validations) : 0n;
    const reason =
      !cl ? "there is no such claim"
      : !v ? "this address did not validate it"
      : is(cl.status, "pending") ? "the claim is not resolved"
      : !(v.revealed && is(v.reason, "none")) ? "the validation was not evaluated"
      : v.paid ? "the share has been collected"
      : !v.counted ? "the validation did not count, and pays nothing"
      : share === 0n ? "the claim's fee pays nothing per validation"
      : !protocolMint ? "the protocol token does not exist on this chain"
      : null;
    if (reason) no("share", a, reason);
    else lines.push({ kind: "share", claim: a, objective: cl.objective.toBase58(), mint: protocolMint!, amount: share });
  }

  // ── refunds ──
  for (const a of c.refunds) {
    const cl = claimOf(a);
    const reason =
      !cl ? "there is no such claim"
      : cl.agent.toBase58() !== me ? "this address did not file it"
      : is(cl.status, "pending") ? "the claim is not resolved"
      : cl.validations > 0 ? "a validation counted, so the fee went to the validators"
      : big(cl.validationFee) === 0n ? "no fee is held for it"
      : !protocolMint ? "the protocol token does not exist on this chain"
      : null;
    if (reason) no("refund", a, reason);
    else lines.push({ kind: "refund", claim: a, objective: cl.objective.toBase58(), mint: protocolMint!, amount: big(cl.validationFee) });
  }

  // ── bonds ──
  const recordOf = (colony: string) => pda(seed("validator"), key(colony).toBuffer(), address.toBuffer());
  const bondOf = (colony: string) => pda(seed("bond"), key(colony).toBuffer(), address.toBuffer());
  const records = await many("validator", c.bonds.map(recordOf));
  const held = await balances(c.bonds.map(bondOf));
  for (const colony of c.bonds) {
    const r = records.get(recordOf(colony).toBase58()), amount = held.get(bondOf(colony).toBase58()) ?? 0n;
    if (!r || r.withdrawn || amount === 0n || !protocolMint) continue;
    bonds.push({ colony, mint: protocolMint, amount, sealed: r.sealed, free: r.sealed === 0 });
  }

  return { lines, refused, bonds };
}

/** The action that pulls `line` for `address`: one per kind, and none of them takes an amount. */
export function pull(a: ClaimActions, address: PublicKey, line: Line) {
  const key = (s: string) => new PublicKey(s);
  switch (line.kind) {
    case "escrow": return a.collect(address, key(line.objective));
    case "edge": return a.edge(address, key(line.claim), line.rootIndex);
    case "share": return a.collectFee(address, key(line.claim));
    case "refund": return a.refundFee(address, key(line.claim));
  }
}
