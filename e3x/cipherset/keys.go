package cipherset

import (
	"crypto/sha256"
	"encoding/json"
	"sort"

	"github.com/telehash/gogotelehash/internal/hashname"
	"github.com/telehash/gogotelehash/internal/util/base32util"
)

type (
	Key         []byte
	Keys        map[CSID]Key
	PrivateKeys map[CSID]*PrivateKey
	Parts       map[CSID]string
)

func (key Key) String() string {
	return base32util.EncodeToString(key)
}

func (key Key) ToPart() string {
	part := sha256.Sum256(key)
	return base32util.EncodeToString(part[:])
}

func (key Key) MarshalText() (text []byte, err error) {
	return []byte(base32util.EncodeToString(key)), nil
}

func (keyPtr *Key) UnmarshalText(text []byte) error {
	k, err := base32util.DecodeString(string(text))
	if err != nil {
		return err
	}

	*keyPtr = k
	return nil
}

func (pkeys PrivateKeys) ToPublicKeys() Keys {
	var (
		keys = make(Keys, len(pkeys))
	)

	for id, key := range pkeys {
		keys[id] = Key(key.Public)
	}

	return keys
}

func (keys Keys) ToParts() Parts {
	var (
		parts = make(Parts, len(keys))
	)

	for id, key := range keys {
		parts[id] = key.ToPart()
	}

	return parts
}

func (parts Parts) ToHashname() hashname.H {
	if len(parts) == 0 {
		return ""
	}

	var (
		hash = sha256.New()
		ids  = make([]int, 0, len(parts))
		buf  [32]byte
	)

	for id := range parts {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)

	for _, id := range ids {
		partString := parts[CSID(id)]
		if len(partString) != 52 {
			return ""
		}

		part, err := base32util.DecodeString(partString)
		if err != nil {
			return ""
		}

		buf[0] = byte(id)
		hash.Write(buf[:1])
		hash.Sum(buf[:0])
		hash.Reset()

		hash.Write(buf[:32])
		hash.Write(part)
		hash.Sum(buf[:0])
		hash.Reset()

		hash.Write(buf[:32])
	}

	return hashname.H(base32util.EncodeToString(buf[:]))
}

func (parts Parts) MarshalJSON() ([]byte, error) {
	var m = make(map[string]string, len(parts))

	for csid, part := range parts {
		m[csid.String()] = part
	}

	return json.Marshal(m)
}

func (keys Keys) MarshalJSON() ([]byte, error) {
	var m = make(map[string]string, len(keys))

	for csid, key := range keys {
		m[csid.String()] = key.String()
	}

	return json.Marshal(m)
}

func (keys PrivateKeys) MarshalJSON() ([]byte, error) {
	var m = make(map[string]*PrivateKey, len(keys))

	for csid, key := range keys {
		m[csid.String()] = key
	}

	return json.Marshal(m)
}