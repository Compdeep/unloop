/**
 * The token accounts every action file needs, in one place: which token
 * program owns a mint, an owner's associated account with the instruction
 * that opens it, and the protocol token — fees, bonds and edges are paid in
 * it, and it does not exist on a chain nobody has initialised it on.
 */
import { PublicKey } from "@solana/web3.js";
import type { Program } from "@anchor-lang/core";
import { getAssociatedTokenAddressSync, createAssociatedTokenAccountIdempotentInstruction } from "@solana/spl-token";

export function tokens(program: Program) {
  const connection = program.provider.connection;

  /** The token program that owns `mint`: Token or Token-2022. */
  async function tokenProgramOf(mint: PublicKey) {
    const info = await connection.getAccountInfo(mint);
    if (!info) throw new Error(`the mint ${mint.toBase58()} is not on this chain`);
    return info.owner;
  }
  /** `owner`'s account for `mint`, and the instruction that opens it if it is not there, paid by `payer`. */
  function ata(payer: PublicKey, owner: PublicKey, mint: PublicKey, tokenProgram: PublicKey) {
    const address = getAssociatedTokenAddressSync(mint, owner, true, tokenProgram);
    return { address, open: createAssociatedTokenAccountIdempotentInstruction(payer, address, owner, mint, tokenProgram) };
  }
  /** The protocol token, or null where it does not exist yet. */
  async function protocolToken() {
    const config = PublicKey.findProgramAddressSync([Buffer.from("token")], program.programId)[0];
    const t = await (program.account as any).tokenConfig.fetchNullable(config);
    if (!t) return null;
    const mint = t.mint as PublicKey;
    return { config, mint, tokenProgram: await tokenProgramOf(mint) };
  }
  return { tokenProgramOf, ata, protocolToken };
}
