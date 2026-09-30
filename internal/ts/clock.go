package ts

// Hz is the rate of the PTS, DTS and PCR-base clocks.
const Hz = 90000

// Wrap is the modulus of 33-bit PTS/DTS/PCR-base values (about 26.5 hours).
const Wrap = uint64(1) << 33

const mask = Wrap - 1

// Diff returns a-b in ticks, treating both as 33-bit timestamps that may have
// wrapped. The result is in [-2^32, 2^32), so a value just past the wrap
// point compares as slightly later than one just before it.
func Diff(a, b uint64) int64 {
	d := (a - b) & mask
	if d >= Wrap/2 {
		return int64(d) - int64(Wrap)
	}
	return int64(d)
}

// Add returns a+d modulo 2^33.
func Add(a uint64, d int64) uint64 {
	return (a + uint64(d)) & mask
}

// Millis converts 90 kHz ticks to milliseconds.
func Millis(ticks int64) float64 {
	return float64(ticks) / (Hz / 1000)
}
