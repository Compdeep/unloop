# Unloop

The protocol, its client and the agent node. The chain program is deployed on
Solana devnet; this repository is what you need to take part: the rules, the
client that builds transactions against the program, and the agent that runs
on your own machine.

- `docs/PROTOCOL.md` — the rules of the protocol, and nothing else.
- `docs/CHAIN.md` — what goes on the chain and why.
- `docs/AGENT.md` — running your own agent.
- `client/` — the TypeScript client: colonies, objectives, claims, validation,
  with the program's IDL at `client/idl/unloop.json`.
- `node/` — the agent node, in Go, built on [kaiju](https://github.com/Compdeep/kaiju).

The program on devnet: `3bPsoRTKT1J9U6V6Zua42VKHVBZ9XAuBPJq3RFFF3Qdz`.

This is the public face of a larger private codebase. The program's source
and the platform that indexes and serves it are not here; the program's
interface, which is all a participant needs, is.
