package server

// omarchy-m-test (github.com/maralcbr/omarchy-m-testing) checks every hardware
// feature of a Mac under Omarchy and writes a signed report. The lab runs it
// in the desktop session after a boot, compares the report with the same
// Mac's report on its known-good kernel, and can publish known-good reports
// to omarchy-m-testing.org.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

// omtPackagingChecks look for the running kernel among the installed packages
// and in /boot. A lab boot runs a kernel that is neither, so they fail for
// every lab kernel and say nothing about it.
var omtPackagingChecks = map[string]bool{"boot.files": true, "boot.kernel-package": true}

const (
	omtTest     = "omarchy-m-test"
	omtFile     = "test-" + omtTest + "/omt-report.json"
	omtMinAgent = "0.3.0" // first agent with the builtin test
)

// omtReport is the part of a report the lab reads; the file itself is kept whole.
type omtReport struct {
	Tool struct {
		Version string `json:"version"`
	} `json:"tool"`
	Catalogue int `json:"catalogue_version"`
	Machine   struct {
		Kernel string `json:"kernel"`
	} `json:"machine"`
	Checks []struct {
		ID             string   `json:"id"`
		Status         string   `json:"status"`
		Evidence       []string `json:"evidence"`
		Classification struct {
			Outcome string `json:"outcome"`
		} `json:"classification"`
	} `json:"checks"`
}

// omtBaseline is the known-good report a Mac's other runs are compared with.
type omtBaseline struct {
	Job    string            `json:"job"`
	Time   time.Time         `json:"time"`
	Kernel string            `json:"kernel"`
	Tool   string            `json:"tool"`
	Status map[string]string `json:"status"` // check id -> pass, fail or skip
}

func (s *Server) omtBaselinePath(device string) string {
	return filepath.Join(s.cfg.DataDir, "baselines", device+".omt.json")
}

func (s *Server) omtBaseline(device string) *omtBaseline {
	b, err := os.ReadFile(s.omtBaselinePath(device))
	if err != nil {
		return nil
	}
	var base omtBaseline
	if json.Unmarshal(b, &base) != nil {
		return nil
	}
	return &base
}

// omtWanted says whether a job on this Mac runs omarchy-m-test without being asked:
// it needs a desktop user and an agent that carries the test.
func (s *Server) omtWanted(d api.Device, spec api.JobSpec) bool {
	return s.cfg.OMT && spec.Crash == "" && d.Facts.GUIUser != "" && versionAtLeast(d.Facts.AgentVersion, omtMinAgent)
}

func omtSpec() api.TestSpec {
	return api.TestSpec{Name: omtTest, Builtin: omtTest, GUI: true, TimeoutSec: 900}
}

// omtResult reads the report a finished omarchy-m-test left in the job,
// compares it with the Mac's known-good report, and records it as the new
// baseline when this run was on the known-good kernel.
func (r *jobRun) omtResult(tr *api.TestResult) {
	j := r.j
	path := filepath.Join(r.s.jobDir(j.ID), omtFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if tr.Passed {
			tr.Passed, tr.Error = false, "omarchy-m-test wrote no report"
		}
		return
	}
	var rep omtReport
	if err := json.Unmarshal(data, &rep); err != nil {
		tr.Passed, tr.Error = false, "omarchy-m-test report is not JSON: "+err.Error()
		return
	}
	res := &api.OMTResult{Report: omtFile, Tool: rep.Tool.Version, Catalogue: rep.Catalogue, Kernel: rep.Machine.Kernel,
		KnownGood: j.Spec.Kernel == "" && j.Spec.Source == ""}
	status := map[string]string{}
	byID := map[string]api.OMTCheck{}
	for _, c := range rep.Checks {
		status[c.ID] = c.Status
		ev := ""
		if len(c.Evidence) > 0 {
			ev = c.Evidence[0]
		}
		chk := api.OMTCheck{ID: c.ID, Outcome: c.Classification.Outcome, Evidence: ev}
		byID[c.ID] = chk
		switch c.Status {
		case "pass":
			res.Pass++
		case "fail":
			res.Fail++
			res.Fails = append(res.Fails, chk)
		default:
			res.Skip++
		}
	}
	base := r.s.omtBaseline(j.Spec.Device)
	switch {
	case res.KnownGood:
		res.ComparedTo = "this is the known-good run: later runs are compared with it"
		if base != nil {
			res.Regressions, res.Fixed = omtDiff(base.Status, status, byID)
			res.ComparedTo = "the previous known-good run " + base.Job
		}
	case base == nil:
		res.ComparedTo = "no known-good run of omarchy-m-test on this Mac yet (lab baseline makes one)"
	default:
		res.Regressions, res.Fixed = omtDiff(base.Status, status, byID)
		kept := res.Regressions[:0]
		for _, c := range res.Regressions {
			if omtPackagingChecks[c.ID] {
				res.LabBoot = append(res.LabBoot, c)
			} else {
				kept = append(kept, c)
			}
		}
		res.Regressions = kept
		res.ComparedTo = "known-good run " + base.Job + " on " + base.Kernel
		if base.Tool != res.Tool {
			res.ComparedTo += fmt.Sprintf(" (omarchy-m-test %s then, %s now)", base.Tool, res.Tool)
		}
	}
	r.s.updateJob(j, func(j *api.Job) { j.Result.OMT = res })
	r.ev("omarchy-m-test %s: %d pass, %d fail, %d skipped; %d regressed, %d fixed vs %s",
		res.Tool, res.Pass, res.Fail, res.Skip, len(res.Regressions), len(res.Fixed), res.ComparedTo)
	if res.KnownGood {
		// A known-good kernel can't regress against itself in a way that blames
		// the kernel; it just becomes the reference.
		b, _ := json.MarshalIndent(omtBaseline{Job: j.ID, Time: time.Now(), Kernel: res.Kernel, Tool: res.Tool, Status: status}, "", "  ")
		os.WriteFile(r.s.omtBaselinePath(j.Spec.Device), b, 0o644)
		return
	}
	if len(res.Regressions) > 0 {
		ids := make([]string, len(res.Regressions))
		for i, c := range res.Regressions {
			ids[i] = c.ID
		}
		tr.Passed = false
		tr.Error = fmt.Sprintf("%d checks pass on the known-good kernel and fail on this one: %s", len(ids), strings.Join(ids, ", "))
	}
}

// omtDiff lists the checks that went from pass to fail, and from fail to pass.
// Skips on either side compare as nothing: a skip says nothing about the kernel.
func omtDiff(before, after map[string]string, byID map[string]api.OMTCheck) (regressed, fixed []api.OMTCheck) {
	ids := make([]string, 0, len(after))
	for id := range after {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		switch {
		case before[id] == "pass" && after[id] == "fail":
			regressed = append(regressed, byID[id])
		case before[id] == "fail" && after[id] == "pass":
			fixed = append(fixed, byID[id])
		}
	}
	return
}

// publishOMT uploads a job's report to omarchy-m-testing.org, the way
// `omarchy-m-test` itself does after its upload prompt. Only a run on the
// Mac's known-good kernel qualifies: the site names a report's build from the
// installed packages, and a lab kernel isn't one, so it would be filed under
// the wrong kernel.
func (s *Server) publishOMT(ctx context.Context, j *api.Job) (string, error) {
	res := j.Result.OMT
	switch {
	case s.cfg.OMTSite == "":
		return "", errors.New("publishing is off (labd --omt-site)")
	case res == nil:
		return "", errors.New("this job has no omarchy-m-test report")
	case res.Published != "":
		return res.Published, nil
	case !res.KnownGood:
		return "", fmt.Errorf("ran on lab kernel %s, which the installed packages don't describe; only known-good runs are published", res.Kernel)
	}
	if kg := s.dev(j.Spec.Device).snapshot().KnownGood; kg != "" && res.Kernel != kg {
		return "", fmt.Errorf("report names kernel %s but the known-good kernel is %s", res.Kernel, kg)
	}
	body, err := os.ReadFile(filepath.Join(s.jobDir(j.ID), res.Report))
	if err != nil {
		return "", err
	}
	site := strings.TrimRight(s.cfg.OMTSite, "/")
	req, _ := http.NewRequestWithContext(ctx, "POST", site+"/api/v1/reports", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "maclab (github.com/iconidentify/maclab)")
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var reply struct {
		ReportURL   string   `json:"report_url"`
		DeletionURL string   `json:"deletion_url"`
		Error       string   `json:"error"`
		Details     []string `json:"details"`
	}
	json.Unmarshal(out, &reply)
	if resp.StatusCode != http.StatusCreated {
		msg := reply.Error
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if len(reply.Details) > 0 {
			msg += " (" + strings.Join(reply.Details, "; ") + ")"
		}
		return "", fmt.Errorf("%s: %s", resp.Status, msg)
	}
	// The deletion link takes the report down again; it stays with the job, not in the API.
	os.WriteFile(filepath.Join(s.jobDir(j.ID), "omt-published.json"), out, 0o600)
	return reply.ReportURL, nil
}

// omtPublish publishes a job's report and records the outcome on the job.
func (s *Server) omtPublish(ctx context.Context, j *api.Job) (string, error) {
	url, err := s.publishOMT(ctx, j)
	s.updateJob(j, func(j *api.Job) {
		if j.Result.OMT == nil {
			return
		}
		if err != nil {
			j.Result.OMT.PublishNote = err.Error()
		} else {
			j.Result.OMT.Published, j.Result.OMT.PublishNote = url, ""
		}
	})
	return url, err
}

// omtScheduler re-runs omarchy-m-test on each Mac's known-good kernel once
// every OMTEvery, publishing the result if that's on: the reference that lab
// kernels are compared with stays fresh, and so does the site.
func (s *Server) omtScheduler() {
	if s.cfg.OMTEvery <= 0 {
		return
	}
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		for _, v := range s.allDevs() {
			d := v.snapshot()
			if d.State != api.StateReady || d.ActiveJob != "" || !s.omtWanted(d, api.JobSpec{}) {
				continue
			}
			if l := d.Lease; l != nil && time.Now().Before(l.Expires) {
				continue
			}
			if base := s.omtBaseline(d.Name); base != nil && time.Since(base.Time) < s.cfg.OMTEvery {
				continue
			}
			if open, _ := s.store.jobs(d.Name, 1, true); len(open) > 0 {
				continue
			}
			j, _, err := s.Submit(api.JobSpec{Device: d.Name, Holder: "maclab-omt", Publish: s.cfg.OMTPublish})
			if err != nil {
				s.log.Printf("%s: scheduled omarchy-m-test: %v", d.Name, err)
				continue
			}
			s.log.Printf("%s: scheduled omarchy-m-test on the known-good kernel: job %s", d.Name, j.ID)
		}
	}
}

// versionAtLeast compares dotted version numbers.
func versionAtLeast(have, want string) bool {
	h, w := strings.Split(have, "."), strings.Split(want, ".")
	for i := range w {
		var a, b int
		if i < len(h) {
			a, _ = strconv.Atoi(h[i])
		}
		b, _ = strconv.Atoi(w[i])
		if a != b {
			return a > b
		}
	}
	return true
}
