package profiles

import "testing"

func members(n int) []GroupMember {
	out := make([]GroupMember, n)
	for i := 0; i < n; i++ {
		out[i] = GroupMember{Key: ObjectKey{Raw: "obj-" + itoa(int64(i))}}
	}
	return out
}

func TestSampleSizeAndDeterminism(t *testing.T) {
	ms := members(100)

	// n >= len returns all.
	if got := Sample(ms, 100); len(got) != 100 {
		t.Errorf("Sample(100,100) len = %d, want 100", len(got))
	}
	if got := Sample(ms, 0); len(got) != 100 {
		t.Errorf("Sample(.,0) len = %d, want 100 (no cap)", len(got))
	}

	a := Sample(ms, 30)
	b := Sample(ms, 30)
	if len(a) != 30 {
		t.Fatalf("len = %d, want 30", len(a))
	}
	for i := range a {
		if a[i].Key.Raw != b[i].Key.Raw {
			t.Fatalf("sampling not deterministic at %d: %q vs %q", i, a[i].Key.Raw, b[i].Key.Raw)
		}
	}

	// A larger sample should be a stable prefix-extension of the ranking, so the
	// smaller sample's members are all contained in the larger one.
	big := Sample(ms, 60)
	set := map[string]bool{}
	for _, m := range big {
		set[m.Key.Raw] = true
	}
	for _, m := range a {
		if !set[m.Key.Raw] {
			t.Errorf("member %q in Sample(30) missing from Sample(60)", m.Key.Raw)
		}
	}
}

func TestSampleDoesNotMutateInput(t *testing.T) {
	ms := members(10)
	first := ms[0].Key.Raw
	_ = Sample(ms, 5)
	if ms[0].Key.Raw != first {
		t.Errorf("input order mutated: %q != %q", ms[0].Key.Raw, first)
	}
}
