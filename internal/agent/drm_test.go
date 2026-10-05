package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDRMHasMaster(t *testing.T) {
	old := debugDRI
	debugDRI = t.TempDir()
	defer func() { debugDRI = old }()
	write := func(minor, clients string) {
		os.MkdirAll(filepath.Join(debugDRI, minor), 0o755)
		os.WriteFile(filepath.Join(debugDRI, minor, "clients"), []byte(clients), 0o644)
	}
	head := "             command  tgid dev master a   uid      magic      name   id\n"
	// The desktop is up: logind and Hyprland hold the card as master.
	write("2", head+"      systemd-logind   633   2   y    y     0          0   <unset>    7\n"+
		"            Hyprland  1521   2   y    y  1000          0   <unset>    9\n")
	// Between the boot splash and the compositor nobody is master.
	write("1", head+"            Hyprland  1521 128   n    n  1000          0   <unset>    8\n")
	write("0", head)
	for _, c := range []struct {
		card       string
		has, known bool
	}{{"/dev/dri/card2", true, true}, {"/dev/dri/card1", false, true}, {"/dev/dri/card0", false, true}, {"/dev/dri/card9", false, false}} {
		if has, known := drmHasMaster(c.card); has != c.has || known != c.known {
			t.Errorf("%s: has=%v known=%v, want %v %v", c.card, has, known, c.has, c.known)
		}
	}
}
