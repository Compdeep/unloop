/**
 * Safety: how likely a claim in a colony is judged fairly by its validators
 * (docs/PLATFORM.md section 4, "Creating a colony: Safety"). Enough of them
 * answer, they reflect the whole queue, and no single party can swing it:
 *
 *   Safety = P_turnout × (1 − P_capture) × P_represent
 *
 * Each public validator is picked with probability π = min(1, k / N), which
 * is what `Claim::selects` does — a hash mod the public pool, below k — and
 * every trusted validator is on every claim.
 *
 *   turnout     at least m answer. Public answers are Binomial(N, π(1 − q)),
 *               trusted ones Binomial(T, 1 − q); the two summed exactly.
 *   capture     no party swings it alone. Of n expected answers, each public
 *               one is hostile with probability h and trusted ones are honest.
 *               A hostile party forces a bad claim through at ⌈a·n⌉ hostile
 *               answers, or blocks a good one at more than n − ⌈a·n⌉; the
 *               larger of the two tails.
 *   represent   a clear claim, true pass share p = a + d, lands on the same
 *               side of a in a sample of n as in the whole queue: the normal
 *               approximation with the finite-queue correction.
 *
 * And the other direction, `cheapest`: the settings that reach a target for
 * the least a claim pays to be judged, which is the validators it draws.
 *
 * No Node built-ins: the colony pages run this in the browser.
 */

/** What the calculation uses. Every one is on the page and editable. */
export interface Values {
  /** Trusted validators, picked for every claim. */
  trusted: number;
  /** The public queue: eligible validators who are not trusted. */
  queue: number;
  /** Public validators picked per claim: an objective's `validators_per_claim`. */
  perClaim: number;
  /** Votes needed to decide: the colony's `min_votes`. */
  minVotes: number;
  /** The share of picked validators expected not to answer. */
  failRate: number;
  /** The share of the public queue one party might hold. */
  hostile: number;
  /** How far above the accept threshold a clear claim's true pass share sits. */
  margin: number;
  /** The accept threshold, as a share: `accept_threshold_bps` / 10 000. */
  accept: number;
}

export interface Safety {
  turnout: number;
  capture: number;
  represent: number;
  safety: number;
  /** Answers expected per claim, and how many of them come from trusted validators. */
  answers: number;
  trustedAnswers: number;
}

/** What `create_colony` and `create_objective` refuse below, for a public colony. */
export const PUBLIC_MIN_VOTES = 5;
export const PUBLIC_DRAW_PER_VOTE = 3;
/** `validators_per_claim` is a u8. */
export const MAX_PER_CLAIM = 255;

/** ln of each Binomial(n, p) probability, 0..n. */
function logPmf(n: number, p: number): Float64Array {
  const out = new Float64Array(n + 1).fill(-Infinity);
  if (p <= 0) { out[0] = 0; return out; }
  if (p >= 1) { out[n] = 0; return out; }
  const lp = Math.log(p), lq = Math.log1p(-p);
  let logC = 0;
  for (let i = 0; i <= n; i++) {
    out[i] = logC + i * lp + (n - i) * lq;
    logC += Math.log((n - i) / (i + 1));
  }
  return out;
}
/** P(X ≤ x) for each x in 0..upto, X ~ Binomial(n, p). */
function cdf(n: number, p: number, upto: number): number[] {
  const l = logPmf(n, p), out: number[] = [];
  let sum = 0;
  for (let x = 0; x <= upto; x++) { if (x <= n) sum += Math.exp(l[x]); out.push(Math.min(1, sum)); }
  return out;
}
/** P(X ≥ x), X ~ Binomial(n, p). */
export function tail(n: number, p: number, x: number): number {
  if (x <= 0) return 1;
  if (x > n) return 0;
  const l = logPmf(n, p);
  let sum = 0;
  for (let i = x; i <= n; i++) sum += Math.exp(l[i]);
  return Math.min(1, sum);
}
/** The standard normal's distribution function (Abramowitz and Stegun 7.1.26, |ε| < 1.5e-7). */
function phi(z: number): number {
  const x = Math.abs(z) / Math.SQRT2, t = 1 / (1 + 0.3275911 * x);
  const erf = 1 - t * (0.254829592 + t * (-0.284496736 + t * (1.421413741 + t * (-1.453152027 + t * 1.061405429)))) * Math.exp(-x * x);
  return z >= 0 ? (1 + erf) / 2 : (1 - erf) / 2;
}

export function safety(v: Values): Safety {
  const T = Math.max(0, Math.floor(v.trusted)), N = Math.max(0, Math.floor(v.queue));
  const k = Math.max(0, Math.floor(v.perClaim)), m = Math.max(1, Math.floor(v.minVotes));
  const answer = 1 - v.failRate;
  const pi = N === 0 ? 0 : Math.min(1, k / N);

  // Turnout: P(public + trusted ≥ m) = 1 − Σ_{x<m} P(public = x) · P(trusted ≤ m − 1 − x).
  const pub = logPmf(N, pi * answer), tr = cdf(T, answer, m - 1);
  let short = 0;
  for (let x = 0; x < m && x <= N; x++) short += Math.exp(pub[x]) * tr[m - 1 - x];
  const turnout = Math.max(0, Math.min(1, 1 - short));

  // The answers a claim gets, as whole votes: what is expected, and never
  // fewer than decide it — fewer is a turnout failure, counted above.
  const trustedAnswers = Math.round(T * answer);
  const publicAnswers = Math.max(Math.round(N * pi * answer), Math.min(N, m - trustedAnswers));
  const n = trustedAnswers + publicAnswers;

  // Capture: hostile answers among the public ones.
  let capture = 0;
  if (n > 0) {
    const needed = Math.ceil(v.accept * n);
    capture = Math.max(tail(publicAnswers, v.hostile, needed), tail(publicAnswers, v.hostile, n - needed + 1));
  }

  // Representativeness: a sample of n from the queue it was drawn from.
  const whole = N + T;
  const p = Math.min(v.accept + v.margin, 1 - 1e-9);
  let represent = 1;
  if (n === 0) represent = 0;
  else if (n < whole && v.margin > 0) {
    const sd = Math.sqrt((p * (1 - p)) / n * ((whole - n) / (whole - 1)));
    represent = phi(v.margin / sd);
  }

  return { turnout, capture, represent, safety: turnout * (1 - capture) * represent, answers: n, trustedAnswers };
}

/** The least a public colony asks, or nothing for one that admits by key or by choice. */
export function floor(publicAdmission: boolean, minVotes: number) {
  return publicAdmission ? { minVotes: PUBLIC_MIN_VOTES, perClaim: minVotes * PUBLIC_DRAW_PER_VOTE } : { minVotes: 1, perClaim: 0 };
}

/**
 * The cheapest votes-needed and picked-per-claim that reach `target`, the rest
 * of `v` held: fewest validators a claim draws (T + k, which is what its fee
 * pays for), then the most votes that still reach it — or, with
 * `fixed.minVotes`, an existing colony's votes and the least draw for them. Votes cost nothing and
 * only turnout counts them here, so the safest of equal cost would always be
 * the fewest — and a verdict one answer can decide is not what anybody means
 * by safe. Within the floor a public colony must
 * meet. Null when no setting reaches it with this queue — then the ways out
 * are fewer votes, a lower target, or trusted validators.
 */
export function cheapest(target: number, v: Values, publicAdmission: boolean,
                         fixed: { minVotes?: number } = {}): { minVotes: number; perClaim: number; safety: Safety } | null {
  const N = Math.max(0, Math.floor(v.queue));
  const most = Math.min(MAX_PER_CLAIM, N);
  let best: { minVotes: number; perClaim: number; safety: Safety } | null = null;
  // An existing colony's votes are its own: only the draw is left to choose.
  const mFrom = fixed.minVotes ?? floor(publicAdmission, 0).minVotes;
  const mTo = fixed.minVotes ?? Math.min(Math.floor(v.trusted) + most, MAX_PER_CLAIM);   // more than can ever answer is never reached
  for (let m = mFrom; m <= mTo; m++) {
    for (let k = floor(publicAdmission, m).perClaim; k <= MAX_PER_CLAIM; k++) {
      // Past the whole queue, picking more changes nothing.
      if (k > most && k > floor(publicAdmission, m).perClaim) break;
      if (best && k > best.perClaim) break;
      const s = safety({ ...v, minVotes: m, perClaim: k });
      if (s.safety < target) continue;
      if (!best || k < best.perClaim || m > best.minVotes) best = { minVotes: m, perClaim: k, safety: s };
      break;   // the least k for this m; a larger one costs more
    }
  }
  return best;
}

/**
 * The bond the page suggests: the largest bounty spread over the votes that
 * decide it, so a party taking a verdict alone puts up what it would win —
 * in protocol tokens, at the price the leader states. Null without a price.
 */
export function suggestedBond(largestBounty: number, minVotes: number, tokenPrice: number): number | null {
  if (!(tokenPrice > 0) || !(largestBounty > 0) || !(minVotes > 0)) return null;
  return largestBounty / minVotes / tokenPrice;
}
