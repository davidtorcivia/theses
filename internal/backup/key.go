package backup

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
	"strings"

	"filippo.io/age"
)

// identity derives the age key the archives are encrypted to from
// THESES_SECRET_KEY. It is deterministic, so a restore needs the environment
// variable the app already runs with and nothing else, and it is a separate
// derivation from the one that encrypts the settings secrets, so the archive
// key is not the settings key even though both come from the same env value.
func identity(secretKey []byte) (*age.X25519Identity, error) {
	scalar, err := hkdf.Key(sha256.New, secretKey, nil, "theses/backup", 32)
	if err != nil {
		return nil, fmt.Errorf("backup: derive the archive key: %w", err)
	}
	id, err := age.ParseX25519Identity(bech32("AGE-SECRET-KEY-", scalar))
	if err != nil {
		return nil, fmt.Errorf("backup: derive the archive key: %w", err)
	}
	return id, nil
}

// bech32 encodes a secret key the way age writes one, so that ParseX25519Identity
// takes it. age exports no way to build an identity from bytes and keeps its own
// encoder internal, so this is the encoding half of BIP173: five bit groups from
// the eight bit input, then the six character checksum over the human readable
// part and the data.
//
// ponytail: encode only, one caller, no error return because the only input is
// thirty-two bytes under a constant prefix. If age ever exports a constructor
// from a scalar, this goes.
func bech32(hrp string, data []byte) string {
	var values []byte
	acc, bits := uint32(0), 0
	for _, b := range data {
		acc, bits = acc<<8|uint32(b), bits+8
		for bits >= 5 {
			bits -= 5
			values = append(values, byte(acc>>bits)&31)
		}
	}
	if bits > 0 {
		values = append(values, byte(acc<<(5-bits))&31)
	}

	const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	var out strings.Builder
	out.WriteString(strings.ToLower(hrp))
	out.WriteString("1")
	for _, v := range append(values, checksum(strings.ToLower(hrp), values)...) {
		out.WriteByte(charset[v])
	}
	return strings.ToUpper(out.String())
}

func checksum(hrp string, values []byte) []byte {
	var input []byte
	for _, c := range []byte(hrp) {
		input = append(input, c>>5)
	}
	input = append(input, 0)
	for _, c := range []byte(hrp) {
		input = append(input, c&31)
	}
	input = append(input, values...)
	input = append(input, 0, 0, 0, 0, 0, 0)

	mod := polymod(input) ^ 1
	out := make([]byte, 6)
	for i := range out {
		out[i] = byte(mod>>(5*(5-i))) & 31
	}
	return out
}

func polymod(values []byte) uint32 {
	generator := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := range generator {
			if top>>i&1 == 1 {
				chk ^= generator[i]
			}
		}
	}
	return chk
}
