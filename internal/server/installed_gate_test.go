package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iconidentify/maclab/internal/agent"
	"github.com/iconidentify/maclab/internal/api"
	"github.com/iconidentify/maclab/internal/installed"
)

// TestInstalledGateEndToEnd hands labd's own installed-mode output to Aurora's
// host acceptance gate (tests/attached/installed_gate.py): a simulated mbp13
// gets a frozen pre-install baseline from a real sim baseline job, an expected
// manifest and installer entry frozen through the HTTP endpoints, and then a
// driver job and an awake job. The gate reads the job directories and the
// frozen store exactly as it would on the lab host.
//
// What the Mac's own tests would write (hardware snapshots, the omarchy-m-test
// report, kernel error lines) comes from the gate's fixtures, relabelled with
// the simulated kernel. It proves the wiring, not 12.0.
//
// MACLAB_GATE=<path to installed_gate.py> runs it; MACLAB_GATE_SAMPLE=<dir>
// keeps the frozen store, both job directories and the gate's verdicts.
func TestInstalledGateEndToEnd(t *testing.T) {
	gate := os.Getenv("MACLAB_GATE")
	if gate == "" {
		t.Skip("MACLAB_GATE (path to installed_gate.py) is not set")
	}
	fix := filepath.Join(filepath.Dir(gate), "installed-fixtures")
	read := func(rel string) []byte {
		b, err := os.ReadFile(filepath.Join(fix, rel))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	const (
		release = "12.0" // the gate accepts only 12.0
		krel    = "7.1.12-2-12.0-sim-ARCH"
		fixKrel = "7.1.12-2-11.35-sep-ARCH" // the kernel the fixture OMT report names
		device  = "mbp13"
		stage2  = "v1.6.1-omarchy.sim12"
	)
	h := func(label string) string { return installed.SHA256([]byte("sim " + label)) }

	l := newLabNamed(t, false, device)
	l.s.cfg.OMT = true
	l.sim.OMTReport = func(kernel string) []byte {
		return bytes.ReplaceAll(read("mbp13/omt-report.json"), []byte(fixKrel), []byte(kernel))
	}
	dmesg := string(read("mbp13/dmesg-errors.txt"))
	l.sim.DmesgErrors = func(string) string { return dmesg }
	l.sim.TestFiles = func(test, kernel string) map[string][]byte {
		var tags, summary []string
		switch test {
		case "release-regress-r7-host":
			tags = []string{"boot", "resume"}
			summary = []string{"kernel " + kernel, "PASS attached-topology", "PASS suspend-cycle-devices", "PASS attached-topology-after-suspend"}
		case "attached-awake-r7-host":
			tags = []string{"manual"}
			summary = []string{"kernel " + kernel, "PASS attached-topology"}
		default:
			return nil
		}
		files := map[string][]byte{
			"frozen-release-regress-r7.sh": read("checker-r7.sh"),
			"summary.txt":                  []byte(strings.Join(append(summary, "overall: PASS"), "\n") + "\n"),
		}
		for _, tag := range tags {
			files["attached-"+tag+".profile"] = read("mbp13/profile")
			for _, f := range []string{"tb.txt", "drm.txt", "pci.txt", "usb.txt", "monitors.json"} {
				files["attached-"+tag+"/"+f] = read("mbp13/snapshot/" + f)
			}
			files["attached-"+tag+"/meta.txt"] = []byte("source live\nkernel " + kernel + "\ntime sim-fixture\n")
		}
		return files
	}
	l.waitDevice(func(d api.Device) bool { return d.Facts.AgentVersion != "" })
	l.baseline()

	// frozen pre-install baseline, from the sim's baseline job
	jobs, _ := l.s.store.jobs(device, 5, false)
	bj, _ := l.s.store.job(jobs[0].ID)
	dir := l.s.frozenDir(release)
	pre := filepath.Join(dir, "preinstall", device)
	os.MkdirAll(pre, 0o755)
	comp := map[string]any{"exe": "/usr/bin/chonkstep-wayland", "sha256": h("compositor")}
	rep, _ := os.ReadFile(filepath.Join(l.s.jobDir(bj.ID), omtFile))
	st, _, _, _ := installed.ReportStatus(rep)
	sb, _ := json.Marshal(st)
	jb, _ := json.MarshalIndent(bj, "", "  ")
	ib, _ := json.MarshalIndent(map[string]any{"schema": "maclab.preinstall-identity/1", "phase": "preinstall", "job": bj.ID,
		"device": device, "uname_r": agent.SimKnownGood, "compositor": comp}, "", "  ")
	dm, _ := os.ReadFile(l.s.baselinePath(device))
	for name, b := range map[string][]byte{"omt-report.json": rep, "omt-status.json": sb, "baseline-job.json": jb, "identity.json": ib, "dmesg-errors.txt": dm} {
		os.WriteFile(filepath.Join(pre, name), b, 0o444)
	}
	var rels []string
	for _, n := range []string{"baseline-job.json", "dmesg-errors.txt", "identity.json", "omt-report.json", "omt-status.json"} {
		rels = append(rels, "preinstall/"+device+"/"+n)
	}
	os.WriteFile(filepath.Join(dir, "PINS.preinstall"), []byte(pinsFor(dir, rels...)), 0o444)

	// expected manifest, frozen through the endpoint
	dtbs := []installed.DTB{{Name: "t8103-j293.dtb", SHA256: h("j293")}, {Name: "t8103-j313.dtb", SHA256: h("j313")}, {Name: "t6030-j516s.dtb", SHA256: h("j516s")}}
	payload := map[string]string{"vmlinuz": h("vmlinuz"), "kernel/drivers/sim/sim.ko.zst": h("module")}
	for _, d := range dtbs {
		payload["dtbs/"+d.Name] = d.SHA256
	}
	var pl strings.Builder
	for _, name := range []string{"vmlinuz", "dtbs/t8103-j293.dtb", "dtbs/t8103-j313.dtb", "dtbs/t6030-j516s.dtb", "kernel/drivers/sim/sim.ko.zst"} {
		fmt.Fprintf(&pl, "%s  %s\n", payload[name], name)
	}
	m := installed.Manifest{Schema: installed.ManifestSchema, Release: release, DTBs: dtbs, Macs: map[string]installed.MacExpect{}}
	for _, name := range []string{"linux-aurora", "linux-aurora-headers", "m1n1-aurora"} {
		m.Artifacts = append(m.Artifacts, installed.Artifact{Name: name, File: name + "-" + release + "-1-aarch64.pkg.tar.zst", SHA256: h(name), Version: release + "-1", MtreeSHA256: h(name + " mtree")})
	}
	m.DTBsSHA256 = installed.DTBsDigest(dtbs)
	m.Kernel.Release, m.Kernel.VmlinuzSHA256 = krel, payload["vmlinuz"]
	m.Kernel.PayloadManifest, m.Kernel.PayloadManifestSHA256, m.Kernel.PayloadCount = "payload/linux-aurora.files.sha256", installed.SHA256([]byte(pl.String())), len(payload)
	m.Kernel.RegeneratedOnMac = []string{"modules.dep", "modules.alias"}
	m.M1n1.BinSHA256, m.M1n1.Version = h("m1n1"), stage2
	for mac, b := range map[string][2]string{"mbp13": {"apple,j293", "t8103-j293.dtb"}, "m1air": {"apple,j313", "t8103-j313.dtb"}, "m3pro": {"apple,j516s", "t6030-j516s.dtb"}} {
		m.Macs[mac] = installed.MacExpect{Board: b[0], BoardDTB: b[1], UbootGzSHA256: h(mac + " u-boot"), BootBinSHA256: h(mac + " boot.bin")}
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	if resp, body := l.post("/api/installed/"+release+"/manifest", map[string]any{"manifest": l.artifact(string(mb)), "payload": l.artifact(pl.String()), "approved_by": []string{"boss", "dave"}}); resp.StatusCode != 200 {
		t.Fatalf("freeze manifest: %s %s", resp.Status, body)
	}

	// what the collector sees on the Mac once the simulated 12.0 is installed
	tokens := []string{"root=UUID=00000000-0000-4000-8000-000000000000", "rw", "rootflags=subvol=@", "quiet", "splash"}
	path := "boot():/EFI/Linux/omarchy_linux-aurora.efi#" + strings.Repeat("ab", 64)
	lines := []string{"  //linux-aurora", "    protocol: efi", "    path: " + path, "    cmdline: " + strings.Join(tokens, " ")}
	entry := installed.Entry{Found: true, Name: "Omarchy / linux-aurora", Protocol: "efi", Path: path,
		Cmdline: strings.Join(tokens, " "), CmdlineTokens: tokens, EntryLines: lines, EntrySHA256: installed.EntryDigest(lines),
		UKIFile: "/boot/efi/EFI/Linux/omarchy_linux-aurora.efi", UKISHA256: h("uki"),
		UKISections: map[string]installed.Section{".linux": {SHA256: payload["vmlinuz"]}, ".initrd": {SHA256: h("initramfs")},
			".cmdline": {SHA256: h("cmdline"), Text: strings.Join(tokens, " ")}, ".uname": {SHA256: h("uname"), Text: krel}}}
	l.sim.PackagedKernels = []string{krel}
	l.sim.InstalledObs = func(phase, job, bootID, kernel, cmdline string) []byte {
		s2 := stage2
		if phase == installed.PhaseBefore {
			s2 = "v1.6.1-omarchy.old"
		}
		pkgs := map[string]any{}
		for _, a := range m.Artifacts {
			pkgs[a.Name] = map[string]any{"version": a.Version, "mtree_sha256": a.MtreeSHA256}
		}
		files := map[string]string{"modules.dep": h("modules.dep as depmod wrote it")}
		for k, v := range payload {
			files[k] = v
		}
		mac := m.Macs[device]
		o := map[string]any{"schema": "maclab.installed-observed/1", "phase": phase, "device": device, "job": job,
			"time":            time.Now().Format("2006-01-02T15:04:05-0700"),
			"running":         map[string]any{"boot_id": bootID, "uname_r": kernel, "proc_cmdline": cmdline},
			"packages":        pkgs,
			"payload":         map[string]any{"krel": krel, "root": "/usr/lib/modules/" + krel, "count": len(files), "files": files},
			"installer_entry": entry,
			"boot_bin": map[string]any{"path": "/boot/efi/m1n1/boot.bin", "sha256": mac.BootBinSHA256, "parts": map[string]any{
				"m1n1_sha256": m.M1n1.BinSHA256, "m1n1_bytes": 1 << 20, "m1n1_versions": []string{stage2},
				"dtb_count": 3, "dtb_shas": []string{dtbs[0].SHA256, dtbs[1].SHA256, dtbs[2].SHA256},
				"board_dtb_index": 0, "board_dtb_count": 1, "board_dtb_sha256": dtbs[0].SHA256,
				"uboot_gz_sha256": mac.UbootGzSHA256, "trailer": mac.Trailer}},
			"dt":         map[string]any{"board": mac.Board, "compatible": []string{mac.Board, "apple,t8103", "apple,arm-platform"}, "live_stage2_version": s2},
			"compositor": comp}
		b, _ := json.MarshalIndent(o, "", " ")
		return b
	}
	l.sim.OnExec = func(cmd string) (string, bool) {
		if !strings.Contains(cmd, "MACLAB_FREEZE_EOF") {
			return "", false
		}
		d := l.s.dev(device).snapshot()
		return string(l.sim.InstalledObs("freeze", "", d.BootID, d.Kernel, "")), true
	}
	if resp, body := l.post("/api/installed/"+release+"/entry/"+device, map[string]any{}); resp.StatusCode != 200 {
		t.Fatalf("freeze entry: %s %s", resp.Status, body)
	}

	v := l.s.dev(device)
	v.mu.Lock()
	v.d.Lease = &api.Lease{Holder: "tester", Expires: time.Now().Add(time.Hour)}
	v.mu.Unlock()
	msha := installed.SHA256(mb)
	script := l.artifact("#!/bin/sh\n# sim: the simulated Mac writes the gate's fixture files for this test\n")
	hw := func(name string) api.JobSpec {
		return api.JobSpec{InstalledManifest: msha, Holder: "tester", Tests: []api.TestSpec{{Name: name, Script: script}}}
	}
	// a hardware test listed twice is refused: its files must bind to one run
	dup := hw("attached-awake-r7-host")
	dup.Device, dup.Tests = device, append(dup.Tests, dup.Tests[0])
	if _, code, err := l.s.Submit(dup); err == nil || code != 400 {
		t.Fatalf("duplicate test accepted: %d %v", code, err)
	}
	driver := l.run(hw("release-regress-r7-host"))
	l.expect(driver, api.OutcomePass)
	awake := l.run(hw("attached-awake-r7-host"))
	l.expect(awake, api.OutcomePass)

	pin := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return installed.SHA256(b)
	}
	runGate := func(regress, awakeJob string) (map[string]any, int) {
		cmd := exec.Command("python3", gate, "--frozen-root", dir,
			"--preinstall-pins-sha", pin("PINS.preinstall"), "--expected-pins-sha", pin("PINS.expected"),
			"--entry-pins-sha", pin("PINS.installer-entry."+device), "--profile", "mbp13-ts3plus",
			"--regress", l.s.jobDir(regress), "--awake", l.s.jobDir(awakeJob))
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		var res map[string]any
		if json.Unmarshal(out.Bytes(), &res) != nil {
			t.Fatalf("gate printed no verdict (exit %d): %s %s", code, out.String(), errb.String())
		}
		return res, code
	}
	res, code := runGate(driver.ID, awake.ID)
	if code != 0 || res["verdict"] != "PASS" {
		t.Fatalf("gate: exit %d, %v: %v", code, res["verdict"], res["reason"])
	}
	// the same evidence with the roles swapped must not pass
	swapped, scode := runGate(awake.ID, driver.ID)
	if scode != 1 || swapped["verdict"] != "FAIL" {
		t.Fatalf("swapped jobs: exit %d, %v", scode, swapped["verdict"])
	}

	if out := os.Getenv("MACLAB_GATE_SAMPLE"); out != "" {
		os.CopyFS(filepath.Join(out, "frozen", release), os.DirFS(dir))
		for _, j := range []*api.Job{driver, awake} {
			os.CopyFS(filepath.Join(out, "jobs", j.ID), os.DirFS(l.s.jobDir(j.ID)))
		}
		for name, v := range map[string]map[string]any{"gate-pass.json": res, "gate-swapped-fail.json": swapped} {
			b, _ := json.MarshalIndent(v, "", "  ")
			os.WriteFile(filepath.Join(out, name), append(b, '\n'), 0o644)
		}
	}
}
