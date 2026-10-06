package server

// Installed-kernel jobs: an acceptance run of a release as its installer
// installed it. The expectation lives in a frozen, pinned store
// (frozen/<release>/), never in anything a later run can replace:
//
//	PINS.preinstall                 pins preinstall/<mac>/*  (written by hand, before any install)
//	expected/manifest.json          maclab.installed-manifest/2  } pinned by PINS.expected
//	expected/payload/*.sha256       payload file list            }
//	installer-entry/<mac>.json      maclab.installer-entry/1, pinned by PINS.installer-entry.<mac>
//	preinstall/<mac>/omt-report.json, omt-status.json, dmesg-errors.txt, ...
//	installed/<mac>/<job>/          append-only evidence copies
//	promotions.log                  append-only
//
// A job never promotes anything: KnownGood, the live OMT reference and the live
// kernel-error baseline stay as they are until `lab installed promote`.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iconidentify/maclab/internal/api"
	"github.com/iconidentify/maclab/internal/installed"
)

const installedMinAgent = "0.7.0"

var reRelease = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]*$`)

func (s *Server) frozenDir(release string) string {
	return filepath.Join(s.cfg.DataDir, "frozen", release)
}

// installedCtx is everything frozen that an installed job is judged against.
type installedCtx struct {
	release   string
	dir       string
	man       *installed.Manifest
	manSHA    string
	payload   map[string]string
	entry     *installed.InstallerEntry
	entrySHA  string
	frozenJob string
	omtReport string
	omtSHA    string
	omtStatus map[string]string
	omtTool   string
	omtKernel string
	dmesgPath string
	dmesgSHA  string
	dmesg     string
}

// pins reads a sha256sum-format pins file into path -> sha.
func readPins(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sha, p, ok := strings.Cut(line, " ")
		if ok && len(sha) == 64 {
			out[strings.TrimSpace(p)] = sha
		}
	}
	return out, sc.Err()
}

// pinned reads dir/rel and checks it against its pin.
func pinned(dir, rel string, pins map[string]string) ([]byte, string, error) {
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return nil, "", err
	}
	sha := installed.SHA256(b)
	if want, ok := pins[rel]; !ok {
		return nil, "", fmt.Errorf("%s is not pinned", rel)
	} else if want != sha {
		return nil, "", fmt.Errorf("%s is %s but its pin is %s", rel, sha, want)
	}
	return b, sha, nil
}

// findManifest locates the frozen release whose expected manifest has this sha256.
func (s *Server) findManifest(sha string) (string, error) {
	dirs, _ := filepath.Glob(filepath.Join(s.cfg.DataDir, "frozen", "*", "expected", "manifest.json"))
	for _, p := range dirs {
		if b, err := os.ReadFile(p); err == nil && installed.SHA256(b) == sha {
			return filepath.Base(filepath.Dir(filepath.Dir(p))), nil
		}
	}
	return "", fmt.Errorf("no frozen release has an expected manifest with sha256 %s", sha)
}

// loadInstalled loads and pin-checks the frozen store for one device.
func (s *Server) loadInstalled(manifestSHA, device string) (*installedCtx, error) {
	release, err := s.findManifest(manifestSHA)
	if err != nil {
		return nil, err
	}
	c := &installedCtx{release: release, dir: s.frozenDir(release)}
	exp, err := readPins(filepath.Join(c.dir, "PINS.expected"))
	if err != nil {
		return nil, fmt.Errorf("release %s: no PINS.expected (lab installed freeze-manifest): %w", release, err)
	}
	mb, msha, err := pinned(c.dir, "expected/manifest.json", exp)
	if err != nil {
		return nil, err
	}
	c.man, c.manSHA = &installed.Manifest{}, msha
	if err := json.Unmarshal(mb, c.man); err != nil {
		return nil, err
	}
	pb, _, err := pinned(c.dir, filepath.Join("expected", c.man.Kernel.PayloadManifest), exp)
	if err != nil {
		return nil, err
	}
	if err := c.man.Validate(pb); err != nil {
		return nil, fmt.Errorf("frozen manifest: %w", err)
	}
	if c.payload, err = installed.ParsePayload(pb); err != nil {
		return nil, err
	}
	if _, ok := c.man.Macs[device]; !ok {
		return nil, fmt.Errorf("the %s manifest has no entry for %s", release, device)
	}
	if ep, err := readPins(filepath.Join(c.dir, "PINS.installer-entry."+device)); err == nil {
		eb, esha, err := pinned(c.dir, "installer-entry/"+device+".json", ep)
		if err != nil {
			return nil, err
		}
		c.entry, c.entrySHA = &installed.InstallerEntry{}, esha
		if err := json.Unmarshal(eb, c.entry); err != nil {
			return nil, err
		}
	}
	pre, err := readPins(filepath.Join(c.dir, "PINS.preinstall"))
	if err != nil {
		return nil, fmt.Errorf("release %s: no PINS.preinstall: %w", release, err)
	}
	rep, rsha, err := pinned(c.dir, "preinstall/"+device+"/omt-report.json", pre)
	if err != nil {
		return nil, err
	}
	sb, _, err := pinned(c.dir, "preinstall/"+device+"/omt-status.json", pre)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(sb, &c.omtStatus); err != nil {
		return nil, err
	}
	_, c.omtTool, c.omtKernel, _ = installed.ReportStatus(rep)
	c.omtReport, c.omtSHA = filepath.Join(c.dir, "preinstall", device, "omt-report.json"), rsha
	db, dsha, err := pinned(c.dir, "preinstall/"+device+"/dmesg-errors.txt", pre)
	if err != nil {
		return nil, err
	}
	c.dmesg, c.dmesgSHA, c.dmesgPath = string(db), dsha, filepath.Join(c.dir, "preinstall", device, "dmesg-errors.txt")
	if jb, err := os.ReadFile(filepath.Join(c.dir, "preinstall", device, "baseline-job.json")); err == nil {
		var j struct {
			ID string `json:"id"`
		}
		json.Unmarshal(jb, &j)
		c.frozenJob = j.ID
	}
	return c, nil
}

// validateInstalled refuses everything an installed job can't take.
func (s *Server) validateInstalled(spec api.JobSpec, d api.Device) (int, error) {
	switch {
	case !reSHA.MatchString(spec.InstalledManifest):
		return 400, errors.New("installed_manifest must be the full sha256 of a frozen expected manifest")
	case spec.Kernel != "" || spec.Source != "" || spec.Config != "":
		return 400, errors.New("an installed job boots the installed release: drop --kernel/--config and the source URL")
	case spec.Cmdline != "" || spec.CmdlineBase != "" || len(spec.CmdlineStrip) > 0 || spec.VerboseBoot:
		return 400, errors.New("an installed job boots the installer's own cmdline: --cmdline, --cmdline-base, --cmdline-strip and --verbose-boot are refused")
	case spec.Baseline || spec.Crash != "" || spec.Publish || len(spec.OMTAllow) > 0:
		return 400, errors.New("an installed job is not a baseline, crash test or publication, and takes no --omt-allow (exceptions belong to the gate)")
	case d.Lease == nil || d.Lease.Holder != spec.Holder || time.Now().After(d.Lease.Expires):
		return 409, fmt.Errorf("%s must be leased by the job's holder (%q) for an installed job", d.Name, spec.Holder)
	case !versionAtLeast(d.Facts.AgentVersion, installedMinAgent):
		return 409, fmt.Errorf("%s runs lab-agent %s; installed jobs need %s or later", d.Name, orStr(d.Facts.AgentVersion, "(unknown)"), installedMinAgent)
	}
	// Every test's files are bound to it by name (result.tests[*].files and
	// test-<name>/), so a name that runs twice would make that ambiguous.
	seen, omt := map[string]bool{}, 0
	for _, t := range testsFor(spec) {
		switch {
		case strings.HasPrefix(t.Name, "identity-"):
			return 400, fmt.Errorf("test name %s is reserved for the installed identity checks", t.Name)
		case t.Name == "" || seen[t.Name]:
			return 400, fmt.Errorf("test %q appears more than once (or has no name); each test of an installed job runs exactly once", t.Name)
		}
		seen[t.Name] = true
		if t.Builtin == omtTest {
			omt++
		}
	}
	if omt != 1 {
		return 409, fmt.Errorf("an installed job is judged by omarchy-m-test against the frozen baseline, and %s can't run it (no GUI user, OMT off or an old agent)", d.Name)
	}
	c, err := s.loadInstalled(spec.InstalledManifest, d.Name)
	if err != nil {
		return 409, err
	}
	if c.entry == nil {
		return 409, fmt.Errorf("no frozen installer entry for %s yet: run `lab installed freeze-entry %s --release %s` after the install", d.Name, d.Name, c.release)
	}
	return 0, nil
}

// installedPreflight lets an installed job past the one preflight problem the
// install causes by design: the old known-good kernel package is gone.
func installedPreflight(problems []string) []string {
	var rest []string
	for _, p := range problems {
		if !(strings.HasPrefix(p, "known-good kernel ") && strings.HasSuffix(p, " is no longer installed")) {
			rest = append(rest, p)
		}
	}
	return rest
}

// installedRun is one installed job's frozen context and the evidence so far.
type installedRun struct {
	r       *jobRun
	c       *installedCtx
	serial  bool
	booted  *installed.Identity
	res     api.InstalledResult
	scripts map[string]string // phase -> script artifact sha
}

func (r *jobRun) newInstalled() (*installedRun, error) {
	c, err := r.s.loadInstalled(r.j.Spec.InstalledManifest, r.j.Spec.Device)
	if err != nil {
		return nil, err
	}
	if c.entry == nil {
		return nil, errors.New("no frozen installer entry")
	}
	ir := &installedRun{r: r, c: c, serial: r.v.snapshot().OOB != nil, scripts: map[string]string{},
		res: api.InstalledResult{Release: c.release, ManifestSHA256: c.manSHA, EntrySHA256: c.entrySHA, Verdicts: map[string]string{}}}
	var pkgs []string
	for _, a := range c.man.Artifacts {
		pkgs = append(pkgs, a.Name)
	}
	for _, ph := range []string{installed.PhaseBefore, installed.PhaseBooted, installed.PhaseAfter} {
		script := installed.Script(ph, r.j.Spec.Device, c.man.Kernel.Release, c.entry.Path, pkgs, true)
		sha, _, err := r.s.storeArtifact(strings.NewReader(script))
		if err != nil {
			return nil, err
		}
		ir.scripts[ph] = sha
	}
	return ir, nil
}

func (ir *installedRun) test(phase string) api.TestSpec {
	return api.TestSpec{Name: installed.TestName(phase), Script: ir.scripts[phase], TimeoutSec: 600}
}

func (ir *installedRun) stageArgs() *api.InstalledStage {
	e := ir.c.entry
	return &api.InstalledStage{Path: e.Path, UKISHA256: e.UKISHA256, Release: ir.c.man.Kernel.Release,
		Cmdline: installed.Cmdline(e.CmdlineTokens, ir.r.j.ID, ir.serial)}
}

func (ir *installedRun) evDir() string { return filepath.Join(ir.r.s.jobDir(ir.r.j.ID), "installed") }

func (ir *installedRun) write(name string, v any) {
	os.MkdirAll(ir.evDir(), 0o755)
	b, _ := json.MarshalIndent(v, "", "  ")
	os.WriteFile(filepath.Join(ir.evDir(), name), append(b, '\n'), 0o644)
	rel := "installed/" + name
	for _, e := range ir.res.Evidence {
		if e == rel {
			return
		}
	}
	ir.res.Evidence = append(ir.res.Evidence, rel)
}

// identity judges one phase's observation. It reports a failure reason, or "".
func (ir *installedRun) identity(phase string, tr *api.TestResult) string {
	raw, err := os.ReadFile(filepath.Join(ir.r.s.jobDir(ir.r.j.ID), "test-"+installed.TestName(phase), "observed.json"))
	if err != nil {
		ir.res.Verdicts[phase] = "MISSING"
		return fmt.Sprintf("identity-%s wrote no observation", phase)
	}
	var booted *installed.Identity
	if phase == installed.PhaseAfter {
		booted = ir.booted
	}
	id, err := installed.Compare(phase, raw, ir.c.man, ir.c.manSHA, ir.c.payload, ir.c.entry, ir.c.entrySHA, ir.serial, booted)
	if err != nil {
		ir.res.Verdicts[phase] = "MISSING"
		return err.Error()
	}
	if id.Job != ir.r.j.ID {
		id.Verdict = "MISMATCH"
		id.Mismatches = append(id.Mismatches, fmt.Sprintf("observation names job %q, not %s", id.Job, ir.r.j.ID))
	}
	if phase == installed.PhaseBooted {
		ir.booted = id
	}
	ir.res.Verdicts[phase] = id.Verdict
	ir.write("identity-"+phase+".json", id)
	ir.r.ev("identity-%s: %s%s", phase, id.Verdict, firstOf(id.Mismatches))
	if id.Verdict != "MATCH" {
		if tr != nil {
			tr.Passed = false
			tr.Error = "identity " + id.Verdict + ": " + strings.Join(id.Mismatches, "; ")
		}
		return "identity-" + phase + " " + id.Verdict + firstOf(id.Mismatches)
	}
	return ""
}

func firstOf(l []string) string {
	if len(l) == 0 {
		return ""
	}
	if len(l) == 1 {
		return ": " + l[0]
	}
	return fmt.Sprintf(": %s (and %d more)", l[0], len(l)-1)
}

// omt compares the run's omarchy-m-test report with the frozen one.
func (ir *installedRun) omt(tr *api.TestResult, res *api.OMTResult, report []byte) {
	cur, tool, kernel, err := installed.ReportStatus(report)
	if err != nil {
		tr.Passed, tr.Error = false, "omarchy-m-test report: "+err.Error()
		return
	}
	c := installed.CompareOMT(ir.r.j.ID, ir.r.j.Spec.Device,
		installed.OMTSide{Job: ir.c.frozenJob, Report: ir.c.omtReport, ReportSHA256: ir.c.omtSHA, Tool: ir.c.omtTool, Kernel: ir.c.omtKernel}, ir.c.omtStatus,
		installed.OMTSide{Job: ir.r.j.ID, Report: filepath.Join(ir.r.s.jobDir(ir.r.j.ID), omtFile), ReportSHA256: installed.SHA256(report), Tool: tool, Kernel: kernel}, cur)
	ir.write("omt-compare.json", c)
	ir.res.OMT = c.Verdict
	res.ComparedTo = "the frozen pre-install run " + ir.c.frozenJob + " (" + ir.c.release + ")"
	res.Regressions = nil
	for _, t := range c.Regressions {
		res.Regressions = append(res.Regressions, api.OMTCheck{ID: t.ID, Outcome: t.Frozen + " -> " + t.Current})
	}
	if c.Verdict != "NO_REGRESSION" {
		ids := make([]string, len(c.Regressions))
		for i, t := range c.Regressions {
			ids[i] = t.ID + " (" + t.Frozen + " -> " + t.Current + ")"
		}
		tr.Passed = false
		tr.Error = "regressed against the frozen pre-install baseline: " + strings.Join(ids, ", ")
	}
}

// dmesg compares the run's kernel error lines with the frozen ones.
func (ir *installedRun) dmesg(got []byte) *installed.DmesgCompare {
	os.MkdirAll(ir.evDir(), 0o755)
	os.WriteFile(filepath.Join(ir.evDir(), "dmesg-errors.txt"), got, 0o644)
	ir.res.Evidence = append(ir.res.Evidence, "installed/dmesg-errors.txt")
	d := installed.CompareDmesg(ir.r.j.ID, ir.r.j.Spec.Device,
		installed.FileRef{File: ir.c.dmesgPath, SHA256: ir.c.dmesgSHA}, ir.c.dmesg,
		installed.FileRef{File: filepath.Join(ir.evDir(), "dmesg-errors.txt"), SHA256: installed.SHA256(got)}, string(got))
	ir.write("dmesg-compare.json", d)
	ir.res.Dmesg = d.Disposition
	return d
}

// finish writes job.json and the frozen store's append-only copy.
func (ir *installedRun) finish() {
	j, err := ir.r.s.store.job(ir.r.j.ID)
	if err != nil {
		return
	}
	j.Result.Installed = &ir.res
	ir.write("job.json", j)
	dst := filepath.Join(ir.c.dir, "installed", ir.r.j.Spec.Device, ir.r.j.ID)
	if _, err := os.Stat(dst); err == nil {
		ir.r.ev("installed: %s already exists; the frozen copy is append-only, not overwritten", dst)
		return
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		ir.r.ev("installed: frozen copy: %v", err)
		return
	}
	ents, _ := os.ReadDir(ir.evDir())
	for _, e := range ents {
		if b, err := os.ReadFile(filepath.Join(ir.evDir(), e.Name())); err == nil {
			os.WriteFile(filepath.Join(dst, e.Name()), b, 0o444)
		}
	}
	ir.res.Frozen = dst
	ir.r.s.updateJob(ir.r.j, func(j *api.Job) { j.Result.Installed = &ir.res })
}

// --- freezing and promotion (admin) ---

// writeOnce writes a file that must not exist yet, read-only.
func writeOnce(path string, b []byte) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; frozen files are written once", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o444)
}

type freezeManifestReq struct {
	Manifest   string   `json:"manifest"` // artifact sha256s of the uploaded files
	Payload    string   `json:"payload"`
	ApprovedBy []string `json:"approved_by"`
}

func (s *Server) hFreezeManifest(w http.ResponseWriter, r *http.Request) {
	release := r.PathValue("release")
	var req freezeManifestReq
	if !reRelease.MatchString(release) || json.NewDecoder(r.Body).Decode(&req) != nil || !reSHA.MatchString(req.Manifest) || !reSHA.MatchString(req.Payload) {
		httpErr(w, 400, "need a release name and the manifest and payload artifact sha256s")
		return
	}
	mb, err1 := os.ReadFile(s.artifactPath(req.Manifest))
	pb, err2 := os.ReadFile(s.artifactPath(req.Payload))
	if err1 != nil || err2 != nil {
		httpErr(w, 400, "upload the manifest and payload first")
		return
	}
	var m installed.Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		httpErr(w, 400, "manifest: %v", err)
		return
	}
	if m.Release != release {
		httpErr(w, 400, "manifest is for release %q, not %q", m.Release, release)
		return
	}
	if err := m.Validate(pb); err != nil {
		httpErr(w, 400, "manifest: %v", err)
		return
	}
	dir := s.frozenDir(release)
	if _, err := os.Stat(filepath.Join(dir, "PINS.expected")); err == nil {
		httpErr(w, 409, "release %s already has a frozen expected manifest", release)
		return
	}
	rel := filepath.Join("expected", m.Kernel.PayloadManifest)
	if err := writeOnce(filepath.Join(dir, "expected", "manifest.json"), mb); err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	if err := writeOnce(filepath.Join(dir, rel), pb); err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	pins := fmt.Sprintf("# PINS.expected %s by maclab (approved by %s): do not rewrite\n%s  expected/manifest.json\n%s  %s\n",
		time.Now().Format(time.RFC3339), strings.Join(req.ApprovedBy, ", "), installed.SHA256(mb), installed.SHA256(pb), rel)
	if err := writeOnce(filepath.Join(dir, "PINS.expected"), []byte(pins)); err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	writeJSON(w, map[string]any{"release": release, "manifest_sha256": installed.SHA256(mb), "pins": pins, "pins_sha256": installed.SHA256([]byte(pins))})
}

// hFreezeEntry observes the installed Mac and freezes its installer boot entry.
func (s *Server) hFreezeEntry(w http.ResponseWriter, r *http.Request) {
	release, device := r.PathValue("release"), r.PathValue("device")
	v := s.dev(device)
	if v == nil || !reRelease.MatchString(release) {
		httpErr(w, 404, "no device %q or bad release", device)
		return
	}
	dir := s.frozenDir(release)
	exp, err := readPins(filepath.Join(dir, "PINS.expected"))
	if err != nil {
		httpErr(w, 409, "release %s has no frozen expected manifest", release)
		return
	}
	mb, msha, err := pinned(dir, "expected/manifest.json", exp)
	if err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	var m installed.Manifest
	json.Unmarshal(mb, &m)
	if _, err := os.Stat(filepath.Join(dir, "PINS.installer-entry."+device)); err == nil {
		httpErr(w, 409, "%s's installer entry for %s is already frozen", device, release)
		return
	}
	script := installed.Script("freeze", device, m.Kernel.Release, "", nil, false)
	cmd := "export MACLAB_OUT=$(mktemp -d); MACLAB_JOB= bash -s >&2 <<'MACLAB_FREEZE_EOF'\n" + script + "MACLAB_FREEZE_EOF\ncat \"$MACLAB_OUT/observed.json\"; rm -rf \"$MACLAB_OUT\""
	var res api.ExecResult
	if err := v.call(r.Context(), api.CmdExec, "", api.ExecArgs{Command: cmd, TimeoutSec: 300}, 6*time.Minute, &res); err != nil {
		httpErr(w, 502, "%v", err)
		return
	}
	var o installed.Observed
	if err := json.Unmarshal([]byte(res.Stdout), &o); err != nil {
		httpErr(w, 502, "collector output: %v (stderr: %s)", err, lastLine(res.Stderr))
		return
	}
	e, err := installed.FreezeEntry(&o, &m)
	if err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	eb, _ := json.MarshalIndent(e, "", "  ")
	eb = append(eb, '\n')
	if err := writeOnce(filepath.Join(dir, "installer-entry", device+".json"), eb); err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	pins := fmt.Sprintf("# PINS.installer-entry.%s %s by maclab, manifest %s: do not rewrite\n%s  installer-entry/%s.json\n",
		device, time.Now().Format(time.RFC3339), msha, installed.SHA256(eb), device)
	if err := writeOnce(filepath.Join(dir, "PINS.installer-entry."+device), []byte(pins)); err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	writeJSON(w, map[string]any{"device": device, "release": release, "entry": e, "entry_sha256": installed.SHA256(eb), "pins": pins})
}

type promoteReq struct {
	Device     string `json:"device"`
	Job        string `json:"job"`
	AcceptedBy string `json:"accepted_by"`
}

// hPromote makes an accepted installed run the device's reference and known-good.
func (s *Server) hPromote(w http.ResponseWriter, r *http.Request) {
	release := r.PathValue("release")
	var req promoteReq
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.AcceptedBy == "" || req.Job == "" {
		httpErr(w, 400, "need device, job and accepted_by")
		return
	}
	j, err := s.store.job(req.Job)
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	ir := j.Result.Installed
	switch {
	case j.Spec.Device != req.Device:
		httpErr(w, 400, "job %s ran on %s, not %s", j.ID, j.Spec.Device, req.Device)
		return
	case ir == nil || ir.Release != release:
		httpErr(w, 400, "job %s is not an installed job of release %s", j.ID, release)
		return
	case j.Outcome != api.OutcomePass || ir.Verdicts[installed.PhaseBooted] != "MATCH" || ir.Verdicts[installed.PhaseAfter] != "MATCH":
		httpErr(w, 409, "job %s did not pass with matching identity (outcome %s, verdicts %v)", j.ID, j.Outcome, ir.Verdicts)
		return
	}
	v := s.dev(req.Device)
	report, err := os.ReadFile(filepath.Join(s.jobDir(j.ID), omtFile))
	if err != nil {
		httpErr(w, 409, "job %s has no omarchy-m-test report", j.ID)
		return
	}
	status, tool, kernel, _ := installed.ReportStatus(report)
	ob, _ := json.MarshalIndent(omtBaseline{Job: j.ID, Time: time.Now(), Kernel: kernel, Tool: tool, Status: status}, "", "  ")
	dm, _ := os.ReadFile(filepath.Join(s.jobDir(j.ID), "installed", "dmesg-errors.txt"))
	os.WriteFile(s.omtBaselinePath(req.Device), ob, 0o644)
	os.WriteFile(s.baselinePath(req.Device), dm, 0o644)
	v.mu.Lock()
	v.d.KnownGood = j.Result.BootKernel
	v.saveLocked()
	v.mu.Unlock()
	line := fmt.Sprintf("%s\t%s\t%s\t%s\taccepted-by=%s\n", time.Now().Format(time.RFC3339), req.Device, j.ID, j.Result.BootKernel, req.AcceptedBy)
	f, err := os.OpenFile(filepath.Join(s.frozenDir(release), "promotions.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		f.WriteString(line)
		f.Close()
	}
	writeJSON(w, map[string]any{"promoted": req.Device, "job": j.ID, "known_good": j.Result.BootKernel, "log": strings.TrimSpace(line)})
}

// hInstalledStatus lists a release's frozen files and their pins.
func (s *Server) hInstalledStatus(w http.ResponseWriter, r *http.Request) {
	dir := s.frozenDir(r.PathValue("release"))
	var files []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	writeJSON(w, map[string]any{"dir": dir, "files": files})
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return l[len(l)-1]
}

var _ = context.Background
var _ = io.EOF
