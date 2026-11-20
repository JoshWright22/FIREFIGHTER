package sysinfo

import "testing"

// OBS recording on NVENC (2026-11-20): 32% on two VideoEncode engines and 2% of 3D. The encoder is
// separate hardware, so only the 3D share competes with a CUDA job.
func TestNVENCOnlyDoesNotCompete(t *testing.T) {
	var g ProcGPU
	g.AddEngine("pid_27832_luid_0x00000000_0x00012e0c_phys_0_eng_0_engtype_3D", 2.3)
	g.AddEngine("pid_27832_luid_0x00000000_0x00012e0c_phys_0_eng_5_engtype_VideoEncode", 15.2)
	g.AddEngine("pid_27832_luid_0x00000000_0x00012e0c_phys_0_eng_6_engtype_VideoEncode", 17.3)
	if g.Util < 34 || g.Util > 35 {
		t.Fatalf("Util %v: the full sum stays for display", g.Util)
	}
	if c := g.Compete(); c < 2.2 || c > 2.4 {
		t.Fatalf("Compete %v, want the 3D share only", c)
	}
	var enc ProcGPU
	enc.AddEngine("pid_1_luid_0x0_0x1_phys_0_eng_5_engtype_VideoEncode", 60)
	enc.AddEngine("pid_1_luid_0x0_0x1_phys_0_eng_1_engtype_VideoDecode", 30)
	enc.AddEngine("pid_1_luid_0x0_0x1_phys_0_eng_2_engtype_VideoProcessing", 10)
	if enc.Compete() != 0 || enc.UtilMedia != 100 {
		t.Fatalf("media-only process: compete %v, media %v", enc.Compete(), enc.UtilMedia)
	}
	var cuda ProcGPU
	cuda.AddEngine("pid_2_luid_0x0_0x1_phys_0_eng_3_engtype_Compute_0", 70)
	cuda.AddEngine("pid_2_luid_0x0_0x1_phys_0_eng_4_engtype_Copy", 10)
	if cuda.Compete() != 80 {
		t.Fatalf("compute + copy compete: %v", cuda.Compete())
	}
	if (ProcGPU{Util: 40}).Compete() != 40 {
		t.Fatal("readings without engines (Linux, scripted) count all of Util")
	}
}
