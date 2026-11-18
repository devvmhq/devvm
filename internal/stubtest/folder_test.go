package stubtest

import "testing"

// Renumbered moves an identity to the next device, its inode and birth
// time kept, and leaves one without a device as it is.
func TestRenumbered(t *testing.T) {
	for id, want := range map[string]string{
		"16777233:4242:1700000000.000000001": "16777234:4242:1700000000.000000001",
		"9:5":                                "10:5",
		"":                                   "",
		"x:5":                                "x:5",
		"16777233":                           "16777233",
	} {
		if got := Renumbered(id); got != want {
			t.Errorf("Renumbered(%q) = %q, want %q", id, got, want)
		}
	}
}
