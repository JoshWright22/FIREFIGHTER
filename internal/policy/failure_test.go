package policy

import "testing"

func TestClassifyExit(t *testing.T) {
	for _, c := range []struct {
		exit       int
		tail, kind string
	}{
		{1, "RuntimeError: CUDA out of memory. Tried to allocate 2.00 GiB", "vram"},
		{1, "torch.OutOfMemoryError: CUDA out of memory", "vram"},
		{137, "", "ram"},
		{1, "Traceback...\nMemoryError", "ram"},
		{1, "OSError: [Errno 28] No space left on device", "disk"},
		{1, "ModuleNotFoundError: No module named 'torch'", "missing"},
		{127, "", "missing"},
		{1, "'python3' is not recognized as an internal or external command", "missing"},
		{1, "urllib.error.URLError: <urlopen error [Errno -3] Temporary failure in name resolution>", "network"},
		{1, "docker: Cannot connect to the Docker daemon at unix:///var/run/docker.sock", "docker"},
		{139, "", "crash"},
		{-1073741819, "", "crash"},
		{2, "usage: train.py [-h]", "unknown"},
	} {
		f := ClassifyExit(c.exit, c.tail)
		if f == nil || f.Kind != c.kind {
			t.Errorf("exit %d %q: got %+v, want %s", c.exit, c.tail, f, c.kind)
		}
	}
	if ClassifyExit(0, "CUDA out of memory") != nil {
		t.Fatal("a successful exit has no failure")
	}
	// the output wins over the code: a SIGKILL'd job whose log shows a CUDA OOM is a VRAM failure
	if f := ClassifyExit(137, "CUDA out of memory"); f.Kind != "vram" {
		t.Fatalf("output should win: %+v", f)
	}
}
