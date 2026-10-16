package main

// CD-ROM Mode 1 sector framing: sync, header, EDC and the P/Q ECC parity, so a
// sector can be regenerated from its 2048 bytes of user data and compared with
// what a raw read returned. Used by --native, where the Dreamcast drive returns
// whole 2352-byte sectors without applying error correction itself.

const sectorUser = 2048

var (
	edcTable [256]uint32
	eccF     [256]byte
	eccB     [256]byte
)

func init() {
	for i := 0; i < 256; i++ {
		e := uint32(i)
		for k := 0; k < 8; k++ {
			if e&1 != 0 {
				e = e>>1 ^ 0xD8018001
			} else {
				e >>= 1
			}
		}
		edcTable[i] = e
		j := i << 1
		if i&0x80 != 0 {
			j ^= 0x11D
		}
		eccF[i] = byte(j)
		eccB[i^(j&0xFF)] = byte(i)
	}
}

func sectorEDC(b []byte) uint32 {
	var e uint32
	for _, x := range b {
		e = edcTable[byte(e)^x] ^ e>>8
	}
	return e
}

func eccBlock(src []byte, majorCount, minorCount, majorMult, minorInc int, dest []byte) {
	size := majorCount * minorCount
	for major := 0; major < majorCount; major++ {
		index := (major>>1)*majorMult + major&1
		var a, b byte
		for minor := 0; minor < minorCount; minor++ {
			t := src[index]
			index += minorInc
			if index >= size {
				index -= size
			}
			a ^= t
			b ^= t
			a = eccF[a]
		}
		a = eccB[eccF[a]^b]
		dest[major] = a
		dest[major+majorCount] = a ^ b
	}
}

func bcd(x int) byte { return byte(x/10<<4 | x%10) }

// Mode1Sector builds the canonical raw sector for absolute frame address fad
// around 2048 bytes of user data.
func Mode1Sector(user []byte, fad int) []byte {
	s := make([]byte, sectorRaw)
	copy(s, syncPattern)
	s[12], s[13], s[14], s[15] = bcd(fad/4500), bcd(fad/75%60), bcd(fad%75), 1
	copy(s[16:], user[:sectorUser])
	e := sectorEDC(s[:2064])
	s[2064], s[2065], s[2066], s[2067] = byte(e), byte(e>>8), byte(e>>16), byte(e>>24)
	eccBlock(s[12:], 86, 24, 2, 86, s[2076:])
	eccBlock(s[12:], 52, 43, 86, 88, s[2248:])
	return s
}

// SectorState classifies a raw sector read from a Mode 1 data track.
type SectorState int

const (
	SectorClean      SectorState = iota // identical to its regeneration
	SectorRepairable                    // user data passes EDC; framing or parity bytes differ
	SectorBad                           // user data fails EDC
)

// CheckMode1 compares raw with the sector regenerated from its own user data.
// For a repairable sector the returned slice is the corrected sector.
func CheckMode1(raw []byte, fad int) (SectorState, []byte) {
	regen := Mode1Sector(raw[16:16+sectorUser], fad)
	if string(regen) == string(raw) {
		return SectorClean, raw
	}
	stored := uint32(raw[2064]) | uint32(raw[2065])<<8 | uint32(raw[2066])<<16 | uint32(raw[2067])<<24
	if sectorEDC(regen[:2064]) == stored {
		return SectorRepairable, regen
	}
	return SectorBad, raw
}

// scrambleTable is the ECMA-130 Annex B sequence XORed over bytes 12..2351.
var scrambleTable = func() [sectorRaw - 12]byte {
	var t [sectorRaw - 12]byte
	reg := uint(1)
	for i := range t {
		var b byte
		for k := 0; k < 8; k++ {
			b |= byte(reg&1) << k
			fb := (reg ^ reg>>1) & 1
			reg = reg>>1 | fb<<14
		}
		t[i] = b
	}
	return t
}()

func unbcd(x byte) int { return int(x>>4)*10 + int(x&15) }

// ScrambledHeaderFAD returns the frame address in the header of a still
// scrambled sector whose sync starts at b[0], or -1 if b does not hold one.
func ScrambledHeaderFAD(b []byte) int {
	if len(b) < 16 || string(b[:12]) != string(syncPattern) {
		return -1
	}
	m, s, f := b[12]^scrambleTable[0], b[13]^scrambleTable[1], b[14]^scrambleTable[2]
	if m&15 > 9 || s&15 > 9 || f&15 > 9 {
		return -1
	}
	return (unbcd(m)*60+unbcd(s))*75 + unbcd(f)
}
