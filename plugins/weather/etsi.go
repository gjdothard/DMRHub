// SPDX-License-Identifier: AGPL-3.0-or-later
// DMRHub - Run a DMR network server in a single binary
// Copyright (C) 2023-2026 Jacob McSwain
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.
//
// The source code is available at <https://github.com/USA-RedDragon/DMRHub>

package weather

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/USA-RedDragon/DMRHub/sdk/plugin"
	pluginv1 "github.com/USA-RedDragon/DMRHub/sdk/plugin/v1"
	"github.com/USA-RedDragon/dmrgo/v2/data"
	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/sms"
)

const (
	airReplyLifetime = 40 * time.Second
	maxAirReceivers  = 128
)

type airRoute struct {
	source, destination, repeater, slot uint32
}

type airLink struct {
	rx              *data.Receiver
	stream          uint32
	start, last, at time.Time
	body            string
	colorCode       enums.ColorCode
	compressed      bool
}

// etsiReplies correlates a validated hub text with the original DMR data
// bursts. The Text event alone does not distinguish full and compressed IP.
type etsiReplies struct {
	mu    sync.Mutex
	links map[airRoute]*airLink
	send  func(*pluginv1.Burst) error
	busy  chan struct{}
}

func (e *etsiReplies) observe(b *pluginv1.Burst) {
	if b.GetGroupCall() || b.GetRepeater() == 0 ||
		b.GetFrameType() != pluginv1.FrameType_FRAME_TYPE_DATA_SYNC ||
		len(b.GetPayload()) != len(layer2.RawBurst{}) {
		return
	}
	key := airRoute{b.GetSource(), b.GetDestination(), b.GetRepeater(), b.GetSlot()}
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	link := e.links[key]
	if link == nil || now.Sub(link.last) > airReplyLifetime {
		if len(e.links) >= maxAirReceivers {
			for route, old := range e.links {
				if now.Sub(old.last) > airReplyLifetime {
					delete(e.links, route)
				}
			}
		}
		if len(e.links) >= maxAirReceivers {
			return
		}
		link = &airLink{}
		e.links[key] = link
	}
	if link.rx == nil || link.stream != b.GetStreamId() {
		rx, err := data.NewReceiver(&data.ReceiverConfig{Address: key.destination, Lost: -1})
		if err != nil {
			return
		}
		link.rx, link.stream, link.start = rx, b.GetStreamId(), now
	}
	link.last = now
	raw := layer2.RawBurst(b.GetPayload())
	d := plugin.DataOf(b)
	at := now.Sub(link.start)
	for _, event := range link.rx.Burst(at, &raw, d.Hint(at), nil) {
		if event.Kind != data.EventDelivered && event.Kind != data.EventReassembled {
			continue
		}
		link.compressed = false
		m := event.Message
		sap, ok := m.Header.SAP()
		if !ok || sap != enums.SAPUDPIPHeaderCompression || m.BadCRC {
			continue
		}
		body, text, err := sms.Decode(nil, &m.Header, m.Payload)
		if err == nil && text.Format == sms.FormatIP && !text.Group &&
			text.Source == key.source && text.Destination == key.destination {
			link.at, link.body, link.colorCode = now, string(body), raw.SlotType().ColorCode
			link.compressed = true
		}
	}
}

func (e *etsiReplies) match(t *pluginv1.Text) (enums.ColorCode, bool) {
	if t.GetGroup() || t.GetFormat() != sms.FormatIP.String() ||
		(t.GetSlot() != 1 && t.GetSlot() != 2) {
		return 0, false
	}
	key := airRoute{t.GetSource(), t.GetDestination(), t.GetRepeater(), t.GetSlot()}
	e.mu.Lock()
	defer e.mu.Unlock()
	link := e.links[key]
	if link == nil || !link.compressed || time.Since(link.at) > airReplyLifetime || link.body != t.GetBody() {
		return 0, false
	}
	if t.GetAt() != 0 && link.at.Sub(time.UnixMilli(t.GetAt())).Abs() > 5*time.Second {
		return 0, false
	}
	return link.colorCode, true
}

func (e *etsiReplies) reply(ctx context.Context, source uint32, t *pluginv1.Text, body string, cc enums.ColorCode) (string, error) {
	var header sms.Header
	payload, err := sms.AppendPacket(nil, &sms.Text{
		Format: sms.FormatIP, Source: source, Destination: t.GetSource(), Body: []byte(body),
	}, &header)
	if err != nil {
		return "", fmt.Errorf("build full ETSI IP reply: %w: %w", plugin.ErrInvalid, err)
	}
	m := data.Message{Header: header.Data, Rate: enums.DataTypeRate12, Payload: payload}
	bursts, err := etsiBursts(m, source, t.GetSource(), t.GetSlot(), cc)
	if err != nil {
		return "", err
	}
	select {
	case e.busy <- struct{}{}:
		defer func() { <-e.busy }()
	case <-ctx.Done():
		return "", fmt.Errorf("wait to send ETSI IP reply: %w", ctx.Err())
	}
	if err := plugin.Play(ctx, e.send, bursts); err != nil {
		return "", fmt.Errorf("send full ETSI IP reply: %w", err)
	}
	return sms.FormatIP.String(), nil
}

// etsiBursts lets dmrgo encode and sequence an unconfirmed data message.
// The caller plays the resulting bursts at the pace of the timeslot.
func etsiBursts(m data.Message, source, destination, slot uint32, cc enums.ColorCode) ([]*pluginv1.Burst, error) {
	ts := enums.Slot1
	if slot == 2 {
		ts = enums.Slot2
	}
	tx, err := data.NewSender(&data.SenderConfig{
		ColorCode: cc, Source: layer2.SyncSourceBS, Access: enums.AccessImpolite, Slot: ts, Seed: rand.Uint64(),
	})
	if err != nil {
		return nil, fmt.Errorf("create ETSI IP data sender: %w", err)
	}
	if err := tx.Send(0, &m); err != nil {
		return nil, fmt.Errorf("encode ETSI IP reply: %w: %w", plugin.ErrInvalid, err)
	}
	tx.Channel(0, false)
	stream := rand.Uint32() | 1
	var bursts []*pluginv1.Burst
	var outputs []data.Output
	for at := time.Duration(0); ; {
		outputs = tx.Tick(at, outputs[:0])
		for _, out := range outputs {
			switch out.Kind {
			case data.OutputBurst:
				bursts = append(bursts, &pluginv1.Burst{
					Source: source, Destination: destination, Slot: slot, StreamId: stream,
					FrameType: pluginv1.FrameType_FRAME_TYPE_DATA_SYNC, DataTypeOrVoiceSequence: uint32(out.Burst.SlotType().DataType),
					Sequence: uint32(len(bursts) & 0xff), Payload: append([]byte(nil), out.Burst[:]...),
				})
			case data.OutputDone:
				if out.Result != data.ResultSent || len(bursts) == 0 {
					return nil, fmt.Errorf("ETSI IP encoder stopped with result %v", out.Result)
				}
				return bursts, nil
			}
		}
		next, ok := tx.Deadline()
		if !ok || next <= at {
			return nil, fmt.Errorf("ETSI IP encoder has no next burst")
		}
		at = next
	}
}
