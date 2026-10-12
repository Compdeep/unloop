// Package protocol carries the message schemas into Go: the same files
// validate.py checks the examples against, embedded so a node needs nothing
// beside its binary. Which body goes with which type is the envelope's to say
// (schemas/envelope.schema.json); nothing here knows it.
package protocol

import "embed"

// Schemas holds every schema under schemas/, each addressable by its `$id`.
//
//go:embed schemas/*.schema.json
var Schemas embed.FS

// Envelope is the one entry point: validate a message against it and nothing else.
const Envelope = "https://unloop.network/schemas/0.1.0/envelope.schema.json"
