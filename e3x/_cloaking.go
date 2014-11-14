package e3x

// This needs to be implmented at the transport layer.

import (
	"crypto/rand"
	"io"

	"github.com/telehash/gogotelehash/Godeps/_workspace/src/golang.org/x/crypto/salsa20"

	"github.com/telehash/gogotelehash/lob"
	"github.com/telehash/gogotelehash/util/bufpool"
)

func cloak(p []byte, key *[32]byte) ([]byte, error) {
	defer bufpool.PutBuffer(p)

	var (
		buf     = bufpool.GetBuffer()
		padding uint8
		err     error
	)

	// read nonce (8 bytes) and 1 extra random byte for padding
	for buf[0] == 0 || buf[1] == 0 || buf[1] == 1 {
		_, err = io.ReadFull(rand.Reader, buf[:9])
		if err != nil {
			bufpool.PutBuffer(buf)
			return nil, err
		}
	}
	padding = buf[8]

	// add body
	p[0] = padding
	if padding > 0 {
		olen := len(p)
		nlen := olen + int(padding)
		p = p[:nlen]
		_, err = io.ReadFull(rand.Reader, p[olen:])
		if err != nil {
			bufpool.PutBuffer(buf)
			return nil, err
		}
	}
	buf = buf[:8+len(p)]

	salsa20.XORKeyStream(buf[8:], p, buf[:8], key)

	return buf, nil
}

func decloak(p []byte, key *[32]byte) ([]byte, error) {
	if p[0] == 0 && (p[1] == 0 || p[1] == 1) {
		return p, nil
	}

	defer bufpool.PutBuffer(p)

	var (
		buf     = bufpool.GetBuffer()
		padding uint8
	)

	if len(p) < 8 {
		bufpool.PutBuffer(buf)
		return nil, lob.ErrInvalidPacket
	}

	buf = buf[:len(p)-8]
	salsa20.XORKeyStream(buf, p[8:], p[:8], key)

	if buf[0] > 0 {
		padding = buf[0]
		buf[0] = 0

		if int(padding) > len(buf) {
			bufpool.PutBuffer(buf)
			return nil, lob.ErrInvalidPacket
		}

		buf = buf[:len(buf)-int(padding)]
	}

	return buf, nil
}