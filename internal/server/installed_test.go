package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iconidentify/maclab/internal/api"
	"github.com/iconidentify/maclab/internal/installed"
)

const (
	tKrel = "7.1.12-sim-9.9"
	tVml  = "1111111111111111111111111111111111111111111111111111111111111111"
	tDtbA = "2222222222222222222222222222222222222222222222222222222222222222"
	tDtbB = "3333333333333333333333333333333333333333333333333333333333333333"
	tM1n1 = "4444444444444444444444444444444444444444444444444444444444444444"
	tUbt  = "5555555555555555555555555555555555555555555555555555555555555555"
	tBoot = "6666666666666666666666666666666666666666666666666666666666666666"
	tUKI  = "7777777777777777777777777777777777777777777777777777777777777777"
	tIrd  = "8888888888888888888888888888888888888888888888888888888888888888"
	tMtr  = "9999999999999999999999999999999999999999999999999999999999999999"
)

func (l *lab) post(path string, body any) (*http.Response, string) {
	l.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", l.url+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(out)
}

func pinsFor(dir string, rels ...string) string {
	var b strings.Builder
	b.WriteString("# test pins\n")
	for _, r := range rels {
		data, _ := os.ReadFile(filepath.Join(dir, r))
		b.WriteString(installed.SHA256(data) + "  " + r + "\n")
	}
	return b.String()
}

// installedLab is a simulated Mac with release 9.9 "installed": a frozen
// pre-install baseline (taken from a real sim baseline job), a frozen expected
// manifest (through the HTTP freeze endpoint) and a frozen installer entry.
func installedLab(t *testing.T, tamper func(phase string, o map[string]any)) (*lab, string, installed.Entry) {
	l := newLab(t, false)
	l.s.cfg.OMT = true
	l.waitDevice(func(d api.Device) bool { return d.Facts.AgentVersion != "" })
	l.baseline()
	jobs, _ := l.s.store.jobs("sim-mac", 5, false)
	bj := jobs[0].ID
	dir := l.s.frozenDir("9.9")
	pre := filepath.Join(dir, "preinstall", "sim-mac")
	os.MkdirAll(pre, 0o755)
	rep, _ := os.ReadFile(filepath.Join(l.s.jobDir(bj), omtFile))
	os.WriteFile(filepath.Join(pre, "omt-report.json"), rep, 0o444)
	st, _, _, _ := installed.ReportStatus(rep)
	sb, _ := json.Marshal(st)
	os.WriteFile(filepath.Join(pre, "omt-status.json"), sb, 0o444)
	dm, _ := os.ReadFile(l.s.baselinePath("sim-mac"))
	os.WriteFile(filepath.Join(pre, "dmesg-errors.txt"), dm, 0o444)
	os.WriteFile(filepath.Join(pre, "baseline-job.json"), []byte(`{"id":"`+bj+`"}`), 0o444)
	os.WriteFile(filepath.Join(dir, "PINS.preinstall"), []byte(pinsFor(dir, "preinstall/sim-mac/omt-report.json",
		"preinstall/sim-mac/omt-status.json", "preinstall/sim-mac/dmesg-errors.txt", "preinstall/sim-mac/baseline-job.json")), 0o444)

	payload := tVml + "  vmlinuz\n" + tDtbA + "  dtbs/t-a.dtb\n" + tDtbB + "  dtbs/t-b.dtb\n" + tMtr + "  modules.dep\n"
	m := installed.Manifest{Schema: installed.ManifestSchema, Release: "9.9",
		Artifacts: []installed.Artifact{{Name: "linux-aurora", File: "linux-aurora-9.9.pkg.tar.zst", SHA256: tVml, Version: "9.9-1", MtreeSHA256: tMtr}},
		DTBs:      []installed.DTB{{Name: "t-a.dtb", SHA256: tDtbA}, {Name: "t-b.dtb", SHA256: tDtbB}},
		Macs:      map[string]installed.MacExpect{"sim-mac": {Board: "apple,sim", BoardDTB: "t-a.dtb", UbootGzSHA256: tUbt, BootBinSHA256: tBoot}}}
	m.DTBsSHA256 = installed.DTBsDigest(m.DTBs)
	m.Kernel.Release, m.Kernel.VmlinuzSHA256 = tKrel, tVml
	m.Kernel.PayloadManifest, m.Kernel.PayloadManifestSHA256, m.Kernel.PayloadCount = "payload/linux-aurora.files.sha256", installed.SHA256([]byte(payload)), 4
	m.Kernel.RegeneratedOnMac = []string{"modules.dep"}
	m.M1n1.BinSHA256, m.M1n1.Version = tM1n1, "v1.6.1-omarchy.sim"
	mb, _ := json.MarshalIndent(m, "", "  ")
	resp, body := l.post("/api/installed/9.9/manifest", map[string]any{"manifest": l.artifact(string(mb)), "payload": l.artifact(payload), "approved_by": []string{"boss", "dave"}})
	if resp.StatusCode != 200 {
		t.Fatalf("freeze manifest: %s %s", resp.Status, body)
	}
	// written once
	if resp, _ := l.post("/api/installed/9.9/manifest", map[string]any{"manifest": l.artifact(string(mb)), "payload": l.artifact(payload)}); resp.StatusCode != 409 {
		t.Fatalf("second freeze: %s", resp.Status)
	}

	tokens := []string{"root=UUID=sim", "rw", "quiet", "splash"}
	lines := []string{"  //linux-aurora", "  protocol: efi", "  path: boot():/EFI/Linux/omarchy_linux-aurora.efi#abc", "  cmdline: root=UUID=sim rw quiet splash"}
	entry := installed.Entry{Found: true, Name: "Omarchy / linux-aurora", Protocol: "efi", Path: "boot():/EFI/Linux/omarchy_linux-aurora.efi#abc",
		Cmdline: strings.Join(tokens, " "), CmdlineTokens: tokens, EntryLines: lines, EntrySHA256: installed.EntryDigest(lines), UKISHA256: tUKI,
		UKISections: map[string]installed.Section{".linux": {SHA256: tVml}, ".initrd": {SHA256: tIrd}, ".cmdline": {SHA256: "cc"}, ".uname": {SHA256: "uu", Text: tKrel}}}
	eb, _ := json.MarshalIndent(installed.InstallerEntry{Schema: "maclab.installer-entry/1", Device: "sim-mac", Release: "9.9", Entry: entry}, "", "  ")
	os.MkdirAll(filepath.Join(dir, "installer-entry"), 0o755)
	os.WriteFile(filepath.Join(dir, "installer-entry", "sim-mac.json"), eb, 0o444)
	os.WriteFile(filepath.Join(dir, "PINS.installer-entry.sim-mac"), []byte(pinsFor(dir, "installer-entry/sim-mac.json")), 0o444)

	l.sim.PackagedKernels = []string{tKrel}
	l.sim.InstalledObs = func(phase, job, bootID, kernel, cmdline string) []byte {
		stage2 := "v1.6.1-omarchy.sim"
		if phase == installed.PhaseBefore {
			stage2 = "v1.6.1-omarchy.old"
		}
		o := map[string]any{"schema": "maclab.installed-observed/1", "phase": phase, "device": "sim-mac", "job": job,
			"running":  map[string]any{"boot_id": bootID, "uname_r": kernel, "proc_cmdline": cmdline},
			"packages": map[string]any{"linux-aurora": map[string]any{"version": "9.9-1", "mtree_sha256": tMtr}},
			"payload": map[string]any{"krel": tKrel, "root": "/usr/lib/modules/" + tKrel, "count": 4,
				"files": map[string]string{"vmlinuz": tVml, "dtbs/t-a.dtb": tDtbA, "dtbs/t-b.dtb": tDtbB, "modules.dep": "regenerated"}},
			"installer_entry": entry,
			"boot_bin": map[string]any{"path": "/boot/efi/m1n1/boot.bin", "sha256": tBoot, "parts": map[string]any{"m1n1_sha256": tM1n1,
				"dtb_shas": []string{tDtbA, tDtbB}, "dtb_count": 2, "board_dtb_sha256": tDtbA, "board_dtb_count": 1, "uboot_gz_sha256": tUbt, "trailer": ""}},
			"dt": map[string]any{"board": "apple,sim", "compatible": []string{"apple,sim"}, "live_stage2_version": stage2}}
		if tamper != nil {
			tamper(phase, o)
		}
		b, _ := json.MarshalIndent(o, "", " ")
		return b
	}
	v := l.s.dev("sim-mac")
	v.mu.Lock()
	v.d.Lease = &api.Lease{Holder: "tester", Expires: time.Now().Add(time.Hour)}
	v.mu.Unlock()
	return l, installed.SHA256(mb), entry
}

func TestInstalledJob(t *testing.T) {
	l, msha, entry := installedLab(t, nil)
	kg := l.s.dev("sim-mac").snapshot().KnownGood
	refBefore, _ := os.ReadFile(l.s.omtBaselinePath("sim-mac"))

	// refusals: a kernel, cmdline overrides, the wrong holder, an unknown manifest
	for _, spec := range []api.JobSpec{
		{InstalledManifest: msha, Holder: "tester", Cmdline: "foo=1"},
		{InstalledManifest: msha, Holder: "tester", CmdlineBase: "default"},
		{InstalledManifest: msha, Holder: "tester", VerboseBoot: true},
		{InstalledManifest: msha, Holder: "someone-else"},
		{InstalledManifest: strings.Repeat("0", 64), Holder: "tester"},
	} {
		spec.Device = "sim-mac"
		if _, code, err := l.s.Submit(spec); err == nil {
			t.Errorf("accepted %+v", spec)
		} else if code == 200 {
			t.Errorf("code 200 with error %v", err)
		}
	}

	j := l.run(api.JobSpec{InstalledManifest: msha, Holder: "tester", Tests: []api.TestSpec{{Name: "display-check", Builtin: "x"}}})
	l.expect(j, api.OutcomePass)
	ir := j.Result.Installed
	if ir == nil || ir.Verdicts["before"] != "MATCH" || ir.Verdicts["booted"] != "MATCH" || ir.Verdicts["after"] != "MATCH" || ir.OMT != "NO_REGRESSION" {
		t.Fatalf("installed result %+v", ir)
	}
	if ir.Dmesg != "REVIEW_REQUIRED" { // the sim logs one new warning on any kernel but its known-good
		t.Fatalf("dmesg %s", ir.Dmesg)
	}
	var names []string
	for _, tr := range j.Result.Tests {
		names = append(names, tr.Name)
	}
	if names[0] != "identity-booted" || names[len(names)-1] != "identity-after" || slices.Contains(names, "identity-before") || ir.Before == nil {
		t.Fatalf("tests %v (before %v)", names, ir.Before)
	}
	want := strings.Join(entry.CmdlineTokens, " ") + " maclab.job=" + j.ID + " panic=10"
	if j.Result.BootKernel != tKrel || j.Result.BootCmdline != want {
		t.Fatalf("booted %s with %q, want %q", j.Result.BootKernel, j.Result.BootCmdline, want)
	}
	for _, f := range []string{"identity-before.json", "identity-booted.json", "identity-after.json", "omt-compare.json", "dmesg-compare.json", "dmesg-errors.txt", "job.json"} {
		if _, err := os.Stat(filepath.Join(l.s.jobDir(j.ID), "installed", f)); err != nil {
			t.Errorf("evidence %s: %v", f, err)
		}
		if _, err := os.Stat(filepath.Join(l.s.frozenDir("9.9"), "installed", "sim-mac", j.ID, f)); err != nil {
			t.Errorf("frozen copy %s: %v", f, err)
		}
	}
	// nothing promoted
	if got := l.s.dev("sim-mac").snapshot().KnownGood; got != kg {
		t.Fatalf("known-good moved: %s -> %s", kg, got)
	}
	if refAfter, _ := os.ReadFile(l.s.omtBaselinePath("sim-mac")); !bytes.Equal(refBefore, refAfter) {
		t.Fatal("the live OMT reference changed")
	}
	if dir := os.Getenv("MACLAB_INSTALLED_SAMPLE"); dir != "" {
		os.CopyFS(dir, os.DirFS(filepath.Join(l.s.jobDir(j.ID), "installed")))
	}

	// promotion is explicit, and only of a passed installed job
	if resp, _ := l.post("/api/installed/9.9/promote", map[string]any{"device": "sim-mac", "job": j.ID}); resp.StatusCode != 400 {
		t.Fatalf("promote without accepted_by: %s", resp.Status)
	}
	resp, body := l.post("/api/installed/9.9/promote", map[string]any{"device": "sim-mac", "job": j.ID, "accepted_by": "dave"})
	if resp.StatusCode != 200 || l.s.dev("sim-mac").snapshot().KnownGood != tKrel {
		t.Fatalf("promote: %s %s", resp.Status, body)
	}
	if b, _ := os.ReadFile(filepath.Join(l.s.frozenDir("9.9"), "promotions.log")); !strings.Contains(string(b), j.ID+"\t"+tKrel+"\taccepted-by=dave") {
		t.Fatalf("promotions.log: %q", b)
	}
}

func TestInstalledJobCatchesInheritedCmdline(t *testing.T) {
	// the Mac boots, but with a bring-up argument the installer's entry doesn't have
	l, msha, _ := installedLab(t, func(phase string, o map[string]any) {
		if phase != installed.PhaseBefore {
			r := o["running"].(map[string]any)
			r["proc_cmdline"] = r["proc_cmdline"].(string) + " asahi.m3_backend=runtime"
		}
	})
	j := l.run(api.JobSpec{InstalledManifest: msha, Holder: "tester"})
	l.expect(j, api.OutcomeTestsFailed)
	if v := j.Result.Installed.Verdicts; v["booted"] != "MISMATCH" || v["after"] != "MISMATCH" {
		t.Fatalf("verdicts %v", v)
	}
}

func TestInstalledJobStopsBeforeBootOnWrongState(t *testing.T) {
	// the installed boot.bin is not the expected one: fail before rebooting
	l, msha, _ := installedLab(t, func(phase string, o map[string]any) {
		o["boot_bin"].(map[string]any)["sha256"] = tVml
	})
	boot0 := l.s.dev("sim-mac").snapshot().BootID
	j := l.run(api.JobSpec{InstalledManifest: msha, Holder: "tester"})
	l.expect(j, api.OutcomeTestsFailed)
	if !strings.Contains(j.Summary, "before boot") || j.Result.Booted || l.s.dev("sim-mac").snapshot().BootID != boot0 {
		t.Fatalf("summary %q booted %v", j.Summary, j.Result.Booted)
	}
}
