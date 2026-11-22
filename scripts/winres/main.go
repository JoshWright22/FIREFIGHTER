// Command winres regenerates the Windows resources (version info, icon, manifest) of firefighter.exe
// and firefighterw.exe (the manifest asks for no elevation): a program with a version resource that names its maker reads less like
// malware to antivirus models than an anonymous one (security audit 2026-11-22).
//
//	go run ./scripts/winres 0.3.3
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/winres <version, e.g. 0.3.3>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "winres:", err)
		os.Exit(1)
	}
}

func run(version string) error {
	ico := filepath.Join(os.TempDir(), "firefighter-winres.ico")
	if err := writeICO("assets/icon-512.png", ico, 256); err != nil {
		return err
	}
	defer os.Remove(ico)
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	num := func(i int) int { n, _ := strconv.Atoi(parts[i]); return n }
	for _, p := range []struct{ dir, name, what string }{
		{"cmd/firefighter", "firefighter.exe", "firefighter: lets AI coding agents share one PC's GPU, CPU and RAM while the human comes first"},
		{"cmd/firefighterw", "firefighterw.exe", "firefighter logon launcher: starts the firefighter guard without a window"},
	} {
		v := map[string]any{
			"FixedFileInfo": map[string]any{
				"FileVersion":    map[string]int{"Major": num(0), "Minor": num(1), "Patch": num(2), "Build": num(3)},
				"ProductVersion": map[string]int{"Major": num(0), "Minor": num(1), "Patch": num(2), "Build": num(3)},
				"FileFlagsMask":  "3f", "FileFlags": "00", "FileOS": "040004", "FileType": "01", "FileSubType": "00",
			},
			"StringFileInfo": map[string]string{
				"CompanyName": "Josh Wright", "FileDescription": p.what, "FileVersion": strings.Join(parts, "."),
				"InternalName": strings.TrimSuffix(p.name, ".exe"), "LegalCopyright": "Copyright (c) 2026 Josh Wright. MIT License.",
				"OriginalFilename": p.name, "ProductName": "firefighter", "ProductVersion": strings.Join(parts, "."),
				"Comments": "https://github.com/JoshWright22/FIREFIGHTER",
			},
			"VarFileInfo":  map[string]any{"Translation": map[string]string{"LangID": "0409", "CharsetID": "04B0"}},
			"IconPath":     ico,
			"ManifestPath": "scripts/winres/firefighter.manifest", // asInvoker (never elevation), DPI aware, Windows 7-11
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		js := filepath.Join(os.TempDir(), "firefighter-versioninfo.json")
		if err := os.WriteFile(js, b, 0o644); err != nil {
			return err
		}
		for _, arch := range []string{"amd64", "arm64"} {
			args := []string{"run", "github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.4.1", "-o", filepath.Join(p.dir, "rsrc_windows_"+arch+".syso")}
			if arch == "arm64" {
				args = append(args, "-arm", "-64")
			} else {
				args = append(args, "-64")
			}
			args = append(args, js)
			if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("goversioninfo %s %s: %v: %s", p.name, arch, err, out)
			}
		}
		os.Remove(js)
	}
	return nil
}

// writeICO scales a square PNG down to size (box filter) and wraps it as a one-image .ico (PNG
// inside, which Windows reads since Vista).
func writeICO(src, dst string, size int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return err
	}
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, size, size))
	sx, sy := float64(b.Dx())/float64(size), float64(b.Dy())/float64(size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, bl, a, n uint64
			for yy := int(float64(y) * sy); yy < int(float64(y+1)*sy); yy++ {
				for xx := int(float64(x) * sx); xx < int(float64(x+1)*sx); xx++ {
					cr, cg, cb, ca := img.At(b.Min.X+xx, b.Min.Y+yy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			if n == 0 {
				continue
			}
			i := out.PixOffset(x, y)
			if a == 0 {
				continue
			}
			// premultiplied sums -> straight alpha
			out.Pix[i+0] = uint8(min(255, r*255/a))
			out.Pix[i+1] = uint8(min(255, g*255/a))
			out.Pix[i+2] = uint8(min(255, bl*255/a))
			out.Pix[i+3] = uint8(a / n >> 8)
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, out); err != nil {
		return err
	}
	var ico bytes.Buffer
	w := func(v any) { _ = binary.Write(&ico, binary.LittleEndian, v) }
	w(uint16(0))
	w(uint16(1))
	w(uint16(1))
	dim := uint8(size)
	if size >= 256 {
		dim = 0
	}
	ico.Write([]byte{dim, dim, 0, 0})
	w(uint16(1))
	w(uint16(32))
	w(uint32(pngBuf.Len()))
	w(uint32(6 + 16))
	ico.Write(pngBuf.Bytes())
	return os.WriteFile(dst, ico.Bytes(), 0o644)
}
