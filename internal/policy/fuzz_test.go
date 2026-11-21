package policy

import "testing"

// Job output is anyone's text: classifying it never panics, and a failure always gets a known kind.
func FuzzClassifyExit(f *testing.F) {
	f.Add(1, "CUDA out of memory")
	f.Add(137, "")
	f.Add(-1073741819, "Segmentation fault")
	f.Add(0, "anything")
	f.Fuzz(func(t *testing.T, exit int, tail string) {
		fl := ClassifyExit(exit, tail)
		if exit == 0 {
			if fl != nil {
				t.Fatal("a success has no failure")
			}
			return
		}
		switch fl.Kind {
		case "vram", "ram", "disk", "missing", "network", "docker", "crash", "unknown":
		default:
			t.Fatalf("unknown kind %q", fl.Kind)
		}
		if fl.Retry != "same" && fl.Retry != "change" && fl.Retry != "no" {
			t.Fatalf("unknown retry %q", fl.Retry)
		}
	})
}
