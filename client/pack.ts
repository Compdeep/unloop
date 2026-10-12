/**
 * Several actions in as few transactions as fit. Each action's instructions
 * stay together and in order in one transaction, so every transaction stands
 * on its own: the account an action opens is opened in the transaction that
 * pays into it. An instruction already in the transaction — the same
 * idempotent open of the same account, say — is not added twice.
 *
 * A transaction is at most 1232 bytes, its one signature included; an action
 * too large for one on its own is refused rather than split.
 */
import { PublicKey, Transaction, type TransactionInstruction } from "@solana/web3.js";

export const PACKET = 1232;

const same = (i: TransactionInstruction) =>
  [i.programId.toBase58(), ...i.keys.map(k => `${k.pubkey.toBase58()}${+k.isSigner}${+k.isWritable}`), Buffer.from(i.data).toString("hex")].join(":");

/** The bytes a transaction of `instructions` paid by `payer` takes, signed. */
export function size(instructions: TransactionInstruction[], payer: PublicKey): number {
  const tx = new Transaction({ feePayer: payer, recentBlockhash: PublicKey.default.toBase58() }).add(...instructions);
  return tx.compileMessage().serialize().length + 1 + 64;
}

/** `actions` packed into transactions, as each transaction's instructions. */
export function pack(actions: TransactionInstruction[][], payer: PublicKey): TransactionInstruction[][] {
  const out: TransactionInstruction[][] = [];
  const withAction = (into: TransactionInstruction[], action: TransactionInstruction[]) => {
    const seen = new Set(into.map(same));
    return [...into, ...action.filter(i => !seen.has(same(i)))];
  };
  for (const action of actions) {
    if (size(withAction([], action), payer) > PACKET) throw new Error("one of these actions is too large for a transaction on its own");
    const last = out.at(-1);
    const joined = last && withAction(last, action);
    if (joined && size(joined, payer) <= PACKET) out[out.length - 1] = joined;
    else out.push(withAction([], action));
  }
  return out;
}
