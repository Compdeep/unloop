/**
 * A protocol message as the browser makes one, for a wallet to sign
 * (docs/PLATFORM.md section 6; protocol/schemas/envelope.schema.json).
 *
 *   signed bytes  utf8(canonical(envelope without `sig`))
 *   sig           Ed25519 by `author`, base58 — what a wallet's
 *                 `signMessage` returns over those bytes
 *   id            sha256(utf8(canonical(envelope))), hex: the same message
 *                 whoever relays it
 *
 * `peer` is inside the signed bytes, so it is the node that will publish the
 * message, named before the wallet signs. Whether the message is well formed,
 * on the right channel, and from an address its account names is the node's
 * question (node/envelope, node/bus), asked once, of every message, its own
 * included. This builds and never judges.
 *
 * The node reads the same bytes in Go. protocol/vectors/envelopes.json holds
 * signed envelopes both sides must reproduce, byte for byte.
 *
 * No Node built-ins: the compose screens run this.
 */
import { sha256 } from "@noble/hashes/sha256";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils";
import { utils } from "@anchor-lang/core";
import { canonical } from "./spec";

/** The protocol version the schemas are written at (`common.schema.json#/$defs/version`). */
export const VERSION = "0.1.0";

export type MessageType =
  | "colony.announce" | "validator.announce" | "objective.publish" | "objective.update" | "claim.resolved" | "claim.paid"
  | "claim.notice" | "claim.publish" | "amendment" | "validation.publish" | "cannot_verify" | "clarification" | "validators.selected";

/**
 * The types that must name the transaction behind them (`chain`): those
 * money or priority depends on (envelope.schema.json, `requireChainRef`).
 */
export const CHAINED: readonly MessageType[] = ["objective.publish", "claim.publish", "claim.resolved", "claim.paid"];

export interface Unsigned {
  v: string;
  type: MessageType;
  /** The colony's slug. */
  colony: string;
  /** The Solana address that signs. */
  author: string;
  /** The libp2p peer that will publish it. */
  peer: string;
  /** RFC 3339; advisory, the slot orders. */
  ts: string;
  chain?: { tx: string; slot: number };
  body?: Record<string, unknown>;
  body_ref?: { hash: string; size: number; mime?: string };
}

export interface Envelope extends Unsigned { sig: string }

/** An envelope to sign. `ts` defaults to now, to the second. */
export function unsigned(m: Omit<Unsigned, "v" | "ts"> & { ts?: string }): Unsigned {
  if ((m.body === undefined) === (m.body_ref === undefined)) throw new Error("a message carries a body or a body_ref, and not both");
  return { v: VERSION, ...m, ts: m.ts ?? new Date(Math.floor(Date.now() / 1000) * 1000).toISOString().replace(".000Z", "Z") };
}

/** What the author signs. A `sig` already on it is not part of it. */
export function signingBytes(e: Unsigned): Uint8Array {
  const { sig: _, ...rest } = e as Envelope;
  return utf8ToBytes(canonical(rest));
}

/** The envelope with the wallet's 64-byte signature over `signingBytes`. */
export function signed(e: Unsigned, signature: Uint8Array): Envelope {
  if (signature.length !== 64) throw new Error(`an Ed25519 signature is 64 bytes; this one is ${signature.length}`);
  const { sig: _, ...rest } = e as Envelope;
  return { ...rest, sig: utils.bytes.bs58.encode(signature) };
}

/** The message's id, which every node computes the same way. */
export const messageId = (e: Envelope) => bytesToHex(sha256(utf8ToBytes(canonical(e))));
