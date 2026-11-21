package main

// End-to-end container command parsing and labeling.

import (
	"strings"
	"testing"
)

func TestDockerizeLabelsContainers(t *testing.T) {
	argv, name := dockerize([]string{"docker", "run", "--rm", "img"}, "abc")
	if name != "firefighter-abc" || !strings.Contains(strings.Join(argv, " "), "--label firefighter.job=abc") {
		t.Fatalf("%v %s", argv, name)
	}
	_, name = dockerize([]string{"podman.exe", "run", "--name=mine", "img"}, "abc")
	if name != "mine" {
		t.Fatal(name)
	}
	if argv, name := dockerize([]string{"python", "x.py"}, "abc"); name != "" || len(argv) != 2 {
		t.Fatal("non-docker command changed")
	}
}

// The image is found past valued options docker has that firefighter doesn't list, and flags and
// short-flag clusters don't eat it; rewrites never touch the container's own arguments.
func TestDockerImageAt(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--ipc", "host", "--gpus", "all", "pytorch:latest", "python", "--name", "x"}, "pytorch:latest"},
		{[]string{"-it", "--rm", "--ulimit", "memlock=-1", "img"}, "img"},
		{[]string{"-d", "-e", "A=1", "--device=/dev/kfd", "rocm/pytorch", "--gpus", "all"}, "rocm/pytorch"},
		{[]string{"--group-add", "video", "-v", "/a:/b", "img:1"}, "img:1"},
		{[]string{"-dp", "8080:80", "--gpus", "all", "web"}, "web"},
		{[]string{"-itv", "/a:/b", "-p8080:80", "-q", "img2"}, "img2"},
	}
	for _, c := range cases {
		if got := dockerImage(c.args); got != c.want {
			t.Fatalf("%v: image %q, want %q", c.args, got, c.want)
		}
	}
	two := 2
	out := dockerGPU([]string{"docker", "run", "--ipc", "host", "--gpus", "all", "img", "--gpus", "all"}, &two, "nvidia")
	if out[5] != "device=2" || out[8] != "all" {
		t.Fatalf("narrow only docker's own --gpus: %v", out)
	}
	argv, name := dockerize([]string{"docker", "run", "--ipc", "host", "--name", "mine", "img", "--name", "inner"}, "abcd1234")
	if name != "mine" {
		t.Fatalf("the user's --name before the image is the container's name: %q %v", name, argv)
	}
}
