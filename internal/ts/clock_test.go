package ts

import "testing"

func TestDiffHandles33BitWrap(t *testing.T) {
	const top = uint64(1) << 33
	tests := []struct {
		name string
		a, b uint64
		want int64
	}{
		{"forward", 100, 50, 50},
		{"backward", 50, 100, -50},
		{"forward across wrap", 10, top - 10, 20},
		{"backward across wrap", top - 10, 10, -20},
		{"one frame across wrap", 1502, top - 1501, 3003},
		{"inputs above 33 bits are masked", top + 7, 2, 5},
		{"half circle is negative", 0, 1 << 32, -(1 << 32)},
	}
	for _, tt := range tests {
		if got := Diff(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: Diff(%d, %d) = %d, want %d", tt.name, tt.a, tt.b, got, tt.want)
		}
	}
}

func TestAddWrapsModulo33Bits(t *testing.T) {
	const top = uint64(1) << 33
	tests := []struct {
		a    uint64
		d    int64
		want uint64
	}{
		{100, 50, 150},
		{top - 10, 30, 20},
		{5, -10, top - 5},
	}
	for _, tt := range tests {
		if got := Add(tt.a, tt.d); got != tt.want {
			t.Errorf("Add(%d, %d) = %d, want %d", tt.a, tt.d, got, tt.want)
		}
	}
}

func TestMillis(t *testing.T) {
	if got := Millis(90); got != 1 {
		t.Errorf("Millis(90) = %v, want 1", got)
	}
	if got := Millis(-4500); got != -50 {
		t.Errorf("Millis(-4500) = %v, want -50", got)
	}
}
