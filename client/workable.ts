/**
 * What an objective pays, and whether it can still pay — read from its
 * account, which is the only place that says.
 *
 * An objective arrives on the bus as text: its statement, its criteria, the
 * title of each award. What it pays does not travel with it, because a figure
 * in a message is a second source for money the chain already holds, and the
 * two could differ. So an agent deciding which prizes to go after, and the
 * platform deciding what to list, read the account and ask this.
 *
 * It mirrors the program's own rules — `Award::remaining`, the reserve behind
 * `Objective::withdrawable`, `Objective::covers_another` — and a test holds it
 * to them: whatever this calls payable, `record_claim` accepts, and whatever it
 * does not, `record_claim` refuses.
 */

/** A u64 as the account decoder hands it over: a BN, or anything with a decimal toString. */
type U64 = { toString(): string };
const big = (n: U64) => BigInt(n.toString());

/** The fields of a decoded `Objective` account this reads. */
export interface ObjectiveAccount {
  payout: { prize?: object; rate?: object };
  status: { open?: object; closed?: object; cancelled?: object };
  claimDeadline: U64;
  validationDeadline: U64;
  pool: U64;
  pendingClaims: U64;
  awardCount: number;
  awards: {
    mode: { winnerTakeAll?: object; graded?: object };
    allocation: U64;
    awarded: U64;
    pending: U64;
    awaiting: U64;
    closed: boolean;
    firstSlot: U64;
  }[];
}

export interface AwardOffer {
  index: number;
  mode: "winnerTakeAll" | "graded";
  /**
   * Prize: the most this prize can still pay — the whole allocation for a
   * winner-take-all prize, what is left of it for a graded one. Rate: the
   * rate one admitted discovery is paid. A graded award pays this times the
   * claim's settled score and novelty.
   */
  amount: bigint;
  /** Whether a claim filed against this award now could be paid. */
  payable: boolean;
}

export interface Workable {
  payout: "prize" | "rate";
  /** Open, and before its claim deadline. */
  open: boolean;
  awards: AwardOffer[];
  /** Worth working on: open, with at least one award that can still pay. */
  workable: boolean;
}

/**
 * Where an objective's money stands. The figures `Objective::balance`,
 * `encumbered` and `withdrawable` compute, and a test holds these to them:
 * the leader can withdraw exactly `withdrawable`, and not one unit more.
 */
export interface Funds {
  /** What the vault holds: everything put in, less everything awarded out. */
  balance: bigint;
  /** What the leader cannot take back: prizes not yet released, and a rate held for every claim not yet rejected or paid. */
  encumbered: bigint;
  /** What the leader can take back now. */
  withdrawable: bigint;
  /** What has left the vault for agents' escrows. */
  paid: bigint;
}

const floor0 = (n: bigint) => (n > 0n ? n : 0n);

/** What a prize could still pay: `Award::remaining`. */
export function remaining(a: ObjectiveAccount["awards"][number]): bigint {
  if (a.closed) return 0n;
  return a.mode.winnerTakeAll ? big(a.allocation) : floor0(big(a.allocation) - big(a.awarded));
}

/**
 * Why one award holds what it holds — `promised` and `pending_reserve`, award
 * by award, so a leader can see which part of the committed figure is which
 * and what would let it go. The holds add up to `Funds.encumbered`, which is
 * computed from them.
 *
 *   promised   a prize not yet won, locked until it is released
 *   owed       a prize admitted to a claim and not yet paid: never released
 *   claims     a rate, held for each claim under judgement or admitted and unpaid
 *   released   a prize the leader may take back: final, judged, nothing pending
 *   spent      nothing left of it to hold
 */
export interface Hold {
  index: number;
  held: bigint;
  why: "promised" | "owed" | "claims" | "released" | "spent";
  /** Claims on this award under judgement, and admitted but unpaid. */
  pending: bigint;
  awaiting: bigint;
}

/**
 * What would release the prizes, where they are not released: each condition
 * the program checks, and whether it is met. Null for a rate, which holds per
 * claim and has nothing to release.
 */
export interface Release {
  released: boolean;
  /** The objective is closed or cancelled: it takes no more claims. */
  final: boolean;
  /** The validation deadline has passed. */
  judged: boolean;
  /** Claims recorded and not yet resolved, across every award. */
  pendingClaims: bigint;
}

export interface Encumbrance { holds: Hold[]; release: Release | null }

/** `now` is unix seconds, as the program's clock reads it. */
export function encumbrance(o: ObjectiveAccount, now: bigint): Encumbrance {
  const active = o.awards.slice(0, o.awardCount);
  const counts = (a: (typeof active)[number]) => ({ pending: big(a.pending), awaiting: big(a.awaiting) });
  if (o.payout.prize) {
    // Released once nothing more can be claimed or judged — but never a prize
    // admitted to a claim and not yet paid, or the leader could take back
    // what it owes the winner.
    const release: Release = { final: !o.status.open, judged: now >= big(o.validationDeadline), pendingClaims: big(o.pendingClaims), released: false };
    release.released = release.final && release.judged && release.pendingClaims === 0n;
    const holds = active.map((a, index): Hold => {
      const left = remaining(a), c = counts(a);
      if (left === 0n) return { index, held: 0n, why: "spent", ...c };
      if (c.awaiting > 0n) return { index, held: left, why: "owed", ...c };
      if (release.released) return { index, held: 0n, why: "released", ...c };
      return { index, held: left, why: "promised", ...c };
    });
    return { holds, release };
  }
  // A rate per discovery, held for each claim filed and not yet rejected or paid.
  return {
    release: null,
    holds: active.map((a, index): Hold => {
      const c = counts(a), held = big(a.allocation) * (c.pending + c.awaiting);
      return { index, held, why: held > 0n ? "claims" : "spent", ...c };
    }),
  };
}

/** `now` is unix seconds, as the program's clock reads it. */
export function funds(o: ObjectiveAccount, now: bigint): Funds {
  const paid = o.awards.slice(0, o.awardCount).reduce((sum, a) => sum + big(a.awarded), 0n);
  const balance = floor0(big(o.pool) - paid);
  const encumbered = encumbrance(o, now).holds.reduce((sum, h) => sum + h.held, 0n);
  return { balance, encumbered, withdrawable: floor0(balance - encumbered), paid };
}

/** What `first_slot` holds until a prize that goes to the first has an admitted claim. */
const NONE = 2n ** 64n - 1n;

/** `now` is unix seconds, as the program's clock reads it. */
export function workable(o: ObjectiveAccount, now: bigint): Workable {
  const payout = o.payout.prize ? "prize" : "rate";
  const open = Boolean(o.status.open) && now < big(o.claimDeadline);
  const active = o.awards.slice(0, o.awardCount);

  // A rate award, and what bounds it is the money not already
  // reserved — which is what the leader could withdraw (`covers_another`).
  const unreserved = funds(o, now).withdrawable;

  const awards = active.map((a, index): AwardOffer => {
    const mode = a.mode.winnerTakeAll ? "winnerTakeAll" : "graded";
    const allocation = big(a.allocation);
    if (payout === "rate") {
      return { index, mode, amount: allocation, payable: open && unreserved >= allocation };
    }
    // A prize: whole until it is won, or what is left of it. One that goes to
    // the first takes no claims once it has an admitted one — a later claim
    // cannot be the first.
    const taken = mode === "winnerTakeAll" && big(a.firstSlot) !== NONE;
    return { index, mode, amount: remaining(a), payable: open && !a.closed && !taken };
  });

  return { payout, open, awards, workable: awards.some(a => a.payable) };
}
