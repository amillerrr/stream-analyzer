package ts

// CRC-32/MPEG-2: polynomial 0x04C11DB7, init 0xFFFFFFFF, no xorout, no reflect.
// Used for PSI section integrity (ISO/IEC 13818-1 Annex B).

const mpegCRCPoly uint32 = 0x04C11DB7

var mpegCRCTable = func() [256]uint32 {
	var t [256]uint32
	for i := 0; i < 256; i++ {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = (c << 1) ^ mpegCRCPoly
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

// CRC32 computes the CRC-32 over data.
func CRC32(data []byte) uint32 {
	c := uint32(0xFFFFFFFF)
	for _, b := range data {
		c = (c << 8) ^ mpegCRCTable[byte(c>>24)^b]
	}
	return c
}

// verifyCRC32 returns true if the trailing 4 bytes of section are the valid
// MPEG-2 CRC-32 of the preceding bytes. Sections that don't include the
// trailing CRC (sections shorter than 4 bytes) are rejected.
func verifyCRC32(section []byte) bool {
	if len(section) < 4 {
		return false
	}
	// PSI section CRC convention: the CRC covers the entire section *including*
	// the CRC field itself. Computing CRC over the whole section should yield 0.
	return CRC32(section) == 0
}
