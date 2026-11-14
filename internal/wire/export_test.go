package wire

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// DecodedCost is the decoder's charge for data.
func DecodedCost(k Kind, data []byte) int64 {
	_, cost, err := decode(k, data, DefaultMaxMessageBytes, DefaultMaxFragmentBytes)
	if err != nil {
		panic(err)
	}
	return int64(cost)
}

// SendAs sends m with an explicit class and ordering key.
func (l *Link) SendAs(ctx context.Context, m proto.Message, class Class, key string) error {
	return l.send(ctx, m, class, key)
}

// Classify is the scheduling table Send uses.
func Classify(m proto.Message) (Class, string, error) { return classify(m) }
