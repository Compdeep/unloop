/**
 * Read an objective's entire discovery graph from the chain, in one request.
 *
 * There is no descent here and no level-by-level walk. Every claim carries the
 * roots it was built on, so a single filtered query returns every node AND
 * every edge at once; the graph is then assembled in memory. Depth costs
 * nothing, because depth is never traversed.
 */

import { Connection, PublicKey } from "@solana/web3.js";

/** Byte offsets in the Claim account. Fixed by the layout, so no IDL needed. */
const OFF = {
  objective: 8,
  agent: 40,
  hash: 72,
  awardIndex: 104,
  claimSlot: 105,
  status: 113,
  roots: 114,
  rootCount: 370,
} as const;

const MAX_ROOTS = 8;
const CLAIM_SIZE = 433;   // pinned by a test against the live account; see attacks.test.ts "layout"
const HEX = (b: Uint8Array) => Buffer.from(b).toString("hex");

export interface Claim {
  address: string;
  hash: string;
  agent: string;
  claimSlot: bigint;
  status: number;
  roots: string[];
}

export interface Graph {
  /** Keyed by claim address, not hash: two agents may hold claims with the same hash. */
  claims: Map<string, Claim>;
  /** address -> the claims it was built on. Carried by the claim itself. */
  rootsOf: Map<string, string[]>;
  /** address -> the claims built on it. The inverse, computed here rather than stored. */
  builtOn: Map<string, string[]>;
  independent: string[];
}

/**
 * `dataSlice` fetches only the bytes the graph needs — hash through root_count
 * — rather than whole accounts. For a thousand claims that is 299 KB instead
 * of 392 KB, over the same single request.
 */
const SLICE = { offset: OFF.hash, length: OFF.rootCount + 1 - OFF.hash };

export async function readGraph(
  connection: Connection,
  programId: PublicKey,
  objective: PublicKey,
): Promise<Graph> {
  const accounts = await connection.getProgramAccounts(programId, {
    filters: [
      { dataSize: CLAIM_SIZE },
      { memcmp: { offset: OFF.objective, bytes: objective.toBase58() } },
    ],
    dataSlice: SLICE,
  });

  const claims = new Map<string, Claim>();
  const rootsOf = new Map<string, string[]>();
  const builtOn = new Map<string, string[]>();

  for (const { pubkey, account } of accounts) {
    const d = account.data;
    const at = (o: number) => o - SLICE.offset;      // slice-relative

    const hash = HEX(d.subarray(at(OFF.hash), at(OFF.hash) + 32));
    const count = d[at(OFF.rootCount)];

    // A root is the address of the claim it names.
    const roots: string[] = [];
    for (let i = 0; i < Math.min(count, MAX_ROOTS); i++) {
      const o = at(OFF.roots) + i * 32;
      roots.push(new PublicKey(d.subarray(o, o + 32)).toBase58());
    }

    const address = pubkey.toBase58();
    claims.set(address, {
      address,
      hash,
      agent: "",                                      // outside the slice
      claimSlot: BigInt(d.readBigUInt64LE(at(OFF.claimSlot))),
      status: d[at(OFF.status)],
      roots,
    });
    rootsOf.set(address, roots);
  }

  // The inverse direction is derived, never stored. Storing it would mean a
  // child list inside a fixed-size account, which would cap how many claims
  // may build on a discovery — the better the result, the harder the cap.
  for (const [address, roots] of rootsOf) {
    for (const r of roots) {
      const list = builtOn.get(r) ?? [];
      list.push(address);
      builtOn.set(r, list);
    }
  }

  const independent = [...rootsOf].filter(([, r]) => r.length === 0).map(([h]) => h);
  return { claims, rootsOf, builtOn, independent };
}

/** Everything built on a claim, transitively. Pure memory, no further requests. */
export function descendants(graph: Graph, address: string): Set<string> {
  const seen = new Set<string>();
  const queue = [...(graph.builtOn.get(address) ?? [])];
  while (queue.length) {
    const h = queue.pop()!;
    if (seen.has(h)) continue;
    seen.add(h);
    queue.push(...(graph.builtOn.get(h) ?? []));
  }
  return seen;
}

/** Priority order. The claim slot is the protocol's only total ordering. */
export function byPriority(graph: Graph): Claim[] {
  return [...graph.claims.values()].sort((a, b) =>
    a.claimSlot < b.claimSlot ? -1 : a.claimSlot > b.claimSlot ? 1 : 0,
  );
}
