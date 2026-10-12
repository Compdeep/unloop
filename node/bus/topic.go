package bus

import (
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/Compdeep/unloop/node/envelope"
	"github.com/mr-tron/base58"
)

// Channel is one of the protocol's three kinds of topic
// (protocol/schemas/envelope.schema.json, `channel`).
type Channel string

const (
	// Directory is global: how a node finds a colony it is not in.
	Directory Channel = "directory"
	// Colony is per colony: what everybody inside it needs.
	Colony Channel = "colony"
	// Claims is per colony: the traffic of individual claims.
	Claims Channel = "claims"
)

// Topic is a channel, and for a per-colony channel, the colony's slug.
type Topic struct {
	Channel Channel `json:"channel"`
	Colony  string  `json:"colony,omitempty"`
}

// Name is the gossipsub topic (protocol/README.md, "On the wire"):
//
//	/unloop/<cluster>/directory
//	/unloop/<cluster>/colony/<slug>
//	/unloop/<cluster>/claims/<slug>
func (t Topic) Name(cluster string) string {
	if t.Channel == Directory {
		return "/unloop/" + cluster + "/directory"
	}
	return "/unloop/" + cluster + "/" + string(t.Channel) + "/" + t.Colony
}

func (t Topic) valid() error {
	switch t.Channel {
	case Directory:
		if t.Colony != "" {
			return fmt.Errorf("the directory belongs to no colony, not %q", t.Colony)
		}
	case Colony, Claims:
		if t.Colony == "" {
			return fmt.Errorf("a %s topic names its colony", t.Channel)
		}
	default:
		return fmt.Errorf("no channel %q", t.Channel)
	}
	return nil
}

// Cluster is the topic names' <cluster> for a Solana cluster: the first eight
// bytes of its genesis hash (base58, as getGenesisHash returns it), in hex.
// Not the program id, which is the same on every cluster it is deployed to.
func Cluster(genesisHash string) (string, error) {
	b, err := base58.Decode(genesisHash)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("%q is not a genesis hash", genesisHash)
	}
	return hex.EncodeToString(b[:8]), nil
}

// carried is which channels each message type travels on: the first six on
// colony and the rest on claims, the announcements also on the directory.
var carried = map[string][]Channel{
	"colony.announce":     {Colony, Directory},
	"validator.announce":  {Colony},
	"objective.publish":   {Colony, Directory},
	"objective.update":    {Colony, Directory},
	"claim.resolved":      {Colony},
	"claim.paid":          {Colony},
	"claim.notice":        {Claims},
	"claim.publish":       {Claims},
	"amendment":           {Claims},
	"validation.publish":  {Claims},
	"cannot_verify":       {Claims},
	"clarification":       {Claims},
	"validators.selected": {Claims},
}

// carries is whether a message belongs on this topic: its type on this
// channel, and its colony this topic's colony. The `channel` an envelope may
// name is advisory (envelope.schema.json) and not asked: it is signed, so an
// announcement named for its colony could not otherwise be carried on the
// directory too.
func (t Topic) carries(m *envelope.Message) error {
	if !slices.Contains(carried[m.Type], t.Channel) {
		return fmt.Errorf("%w: %s does not travel on %s", ErrChannel, m.Type, t.Channel)
	}
	if t.Channel != Directory && m.Colony != t.Colony {
		return fmt.Errorf("%w: a message about %s on %s's topic", ErrChannel, m.Colony, t.Colony)
	}
	return nil
}
