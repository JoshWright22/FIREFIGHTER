package sysinfo

import (
	"testing"
	"time"
)

func TestFindStormsGroupsIdenticalRecentProcesses(t *testing.T) {
	now := time.Now()
	var infos []procInfo
	add := func(n int, name, cmd string, age time.Duration, gbEach float64) {
		for i := 0; i < n; i++ {
			infos = append(infos, procInfo{pid: len(infos) + 100, name: name, created: now.Add(-age).UnixMilli(),
				cmdline: func() string { return cmd }, ramGB: func() float64 { return gbEach }})
		}
	}
	// The 11/23 bomb: thousands of one hook, fresh, together tens of GB.
	add(300, "sh.exe", `sh C:/Users/awsom/.git-hooks-global/pre-push origin https://github.com/x/y`, time.Minute, 0.009)
	// A long-lived pool of identical workers (a server's), not a storm.
	add(300, "worker.exe", "worker --pool", 3*time.Hour, 0.02)
	// Many copies, but each command line different (a build's compilers): not one group.
	for i := 0; i < 300; i++ {
		add(1, "cc1.exe", "cc1 file"+string(rune('a'+i%26))+string(rune('a'+i/26)), time.Minute, 0.05)
	}
	// Too few to matter.
	add(20, "python.exe", "python train.py", time.Minute, 1)
	skip := map[int]bool{}
	got := findStorms(infos, StormParams{MinCount: 200, MinGB: 2, Within: 10 * time.Minute}, skip, now)
	if len(got) != 1 || got[0].Name != "sh.exe" || len(got[0].PIDs) != 300 {
		t.Fatalf("storms: %+v", got)
	}
	// Under the RAM bar: no storm.
	if got := findStorms(infos, StormParams{MinCount: 200, MinGB: 50, Within: 10 * time.Minute}, skip, now); len(got) != 0 {
		t.Fatalf("a storm holding under MinGB: %+v", got)
	}
}
