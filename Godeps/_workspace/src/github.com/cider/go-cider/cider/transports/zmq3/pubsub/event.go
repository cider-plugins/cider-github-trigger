// Copyright (c) 2013 The go-cider AUTHORS
//
// Use of this source code is governed by The MIT License
// that can be found in the LICENSE file.

package pubsub

import (
	"bytes"
	"encoding/binary"

	"github.com/cider/go-cider/cider/services/pubsub"
	"github.com/cider/go-cider/cider/utils/codecs"
)

type Event struct {
	kind      string
	seq       pubsub.EventSeqNum
	publisher string
	body      []byte
}

func newEvent(msg [][]byte) pubsub.Event {
	var seq pubsub.EventSeqNum
	// The message should be validated by the time it gets here. Panic on error.
	if err := binary.Read(bytes.NewReader(msg[4]), binary.BigEndian, &seq); err != nil {
		panic(err)
	}
	return &Event{
		kind:      string(msg[0]),
		seq:       seq,
		publisher: string(msg[1]),
		body:      msg[5],
	}
}

func (event *Event) Kind() string {
	return event.kind
}

func (event *Event) Seq() pubsub.EventSeqNum {
	return event.seq
}

func (event *Event) Publisher() string {
	return event.publisher
}

func (event *Event) Unmarshal(dst interface{}) error {
	return codecs.MessagePack.Decode(bytes.NewReader(event.body), dst)
}
