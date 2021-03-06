package bridge

import (
	"encoding/hex"

	"github.com/telehash/gogotelehash/e3x"
	"github.com/telehash/gogotelehash/e3x/cipherset"
	"github.com/telehash/gogotelehash/internal/hashname"
	"github.com/telehash/gogotelehash/internal/lob"
	"github.com/telehash/gogotelehash/internal/util/bufpool"
	"github.com/telehash/gogotelehash/internal/util/logs"
)

var mainLog = logs.Module("peers")

func (mod *module) connect(ex *e3x.Exchange, inner *bufpool.Buffer) error {
	ch, err := ex.Open("connect", false)
	if err != nil {
		return err
	}

	defer ch.Kill()

	err = ch.WritePacket(lob.New(inner.RawBytes()))
	if err != nil {
		return err
	}

	return nil
}

func (mod *module) handle_connect(ch *e3x.Channel) {
	defer ch.Kill()

	var (
		from        hashname.H
		localIdent  *e3x.Identity
		remoteIdent *e3x.Identity
		handshake   cipherset.Handshake
		innerData   = bufpool.New()
		err         error
	)

	localIdent, err = mod.e.LocalIdentity()
	if err != nil {
		return
	}

	pkt, err := ch.ReadPacket()
	if err != nil {
		return
	}

	pkt.Body(innerData.SetLen(pkt.BodyLen()).RawBytes()[:0])

	inner, err := lob.Decode(innerData)
	if err != nil {
		return
	}

	innerHdr := inner.Header()
	if innerHdr.IsBinary() && len(innerHdr.Bytes) == 1 {
		// handshake
		var (
			csid = innerHdr.Bytes[0]
			key  = localIdent.Keys()[csid]
		)
		if key == nil {
			return
		}

		handshake, err = cipherset.DecryptHandshake(csid, key, inner.Body(nil))
		if err != nil {
			return
		}

		from, err = hashname.FromIntermediates(handshake.Parts())
		if err != nil {
			return
		}

		remoteIdent, err = e3x.NewIdentity(cipherset.Keys{
			handshake.CSID(): handshake.PublicKey(),
		}, handshake.Parts(), nil)
		if err != nil {
			return
		}

	} else {
		// key packet

		var parts = make(cipherset.Parts)
		var csid uint8
		for key, value := range inner.Header().Extra {
			if len(key) != 2 {
				continue
			}

			keyData, err := hex.DecodeString(key)
			if err != nil {
				continue
			}

			partCSID := keyData[0]
			switch v := value.(type) {
			case bool:
				csid = partCSID
			case string:
				parts[partCSID] = v
			}
		}

		hn, err := hashname.FromKeyAndIntermediates(csid, inner.Body(nil), parts)
		if err != nil {
			return
		}

		from = hn

		pubKey, err := cipherset.DecodeKeyBytes(csid, inner.Body(nil), nil)
		if err != nil {
			return
		}

		remoteIdent, err = e3x.NewIdentity(cipherset.Keys{csid: pubKey}, parts, nil)
		if err != nil {
			return
		}
	}

	if from == "" {
		return
	}

	if mod.config.AllowConnect != nil && !mod.config.AllowConnect(from, ch.RemoteHashname()) {
		return
	}

	x, err := mod.e.CreateExchange(remoteIdent)
	if err != nil {
		return
	}

	// when the BODY contains a handshake
	if handshake != nil {
		routerExchange := ch.Exchange()
		routerAddr := &peerAddr{
			router: routerExchange.RemoteHashname(),
		}

		conn := newConnection(x.RemoteHashname(), routerAddr, routerExchange, func() {
			mod.unregisterConnection(routerExchange, x.LocalToken())
		})

		pipe, added := x.AddPipeConnection(conn, nil)
		if added {
			mod.registerConnection(routerExchange, x.LocalToken(), conn)
		}

		resp, ok := x.ApplyHandshake(handshake, pipe)
		if !ok {
			return
		}

		if resp != nil {
			err = mod.peerVia(ch.Exchange(), from, resp)
			if err != nil {
				return
			}
		}
	}

	// when the BODY contains a key packet
	if handshake == nil {
		pkt, err := x.GenerateHandshake()
		if err != nil {
			return
		}

		err = mod.peerVia(ch.Exchange(), from, pkt)
		if err != nil {
			return
		}
	}

	// Notify on-exchange callbacks
	mod.getIntroduction(from).resolve(x, nil)
}
